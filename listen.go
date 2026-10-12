package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Telegram 秒回：常驻进程 agentq listen（launchd 服务 io.agentq.agentq.listen，KeepAlive，挂了 launchd 拉起）
// 用 getUpdates 长轮询（timeout 50 秒）收主人的消息，收到马上处理：「状态」当场回摘要；其余先回一句「收到」
// （有排队的说前面还有几条），附件（图片、录屏、文件，20MB 内）下载下来，消息进 notify.json 的收件箱，
// 再试着启动对话任务（同一时间只有一个）。对话任务结束时任务外壳触发的 tick 回发回答，并接着启动排队的下一批。
// listen 拿着 listen.lock 期间 tick 不收消息；listen 没在跑（没装、挂了还没被拉起）时 tick 每 5 分钟短轮询兜底。
// 两边靠这把锁不会同时 getUpdates；offset 存在 notify.json 里，收到就确认，同一条不会处理两次。
const (
	listenPollWait  = 50               // getUpdates 长轮询挂多少秒
	listenIdleWait  = time.Minute      // 没配 Telegram、推送关着、配置读不出时隔多久再看
	listenRetryMax  = time.Minute      // 收消息出错时最多隔多久再试
	listenServeWait = 10 * time.Second // 启动对话任务前等 tick.lock 最多这么久；等不到就由正在跑的 tick 接着启动
	listenNoteEvery = time.Hour        // 同一个问题隔这么久才再记一次日志
)

func listenLabel() string                    { return launchdLabel + ".listen" }
func (a *App) listenLockPath() string        { return filepath.Join(a.dataDir(), "listen.lock") }
func (a *App) listenPidPath() string         { return filepath.Join(a.dataDir(), "listen.pid") }
func (a *App) inboxDir() string              { return filepath.Join(a.dataDir(), "inbox") }
func (a *App) chatFilesDir(id string) string { return filepath.Join(a.jobDir(id), "files") }

// listener：常驻循环里要跨轮记住的东西。
type listener struct {
	fails    int
	note     string
	noteAt   time.Time
	self     string
	selfInfo os.FileInfo
}

// say 记一行日志；同一句话 listenNoteEvery 内只记一次。
func (l *listener) say(a *App, msg string) {
	now := a.Now()
	if msg == l.note && now.Sub(l.noteAt) < listenNoteEvery {
		return
	}
	l.note, l.noteAt = msg, now
	a.logf("监听：%s", msg)
}

// cmdListen 是 agentq listen：一直跑，不返回（换了新版本时返回 nil，让 launchd 用新版本重启）。
func (a *App) cmdListen() error {
	lock, err := acquireLock(a.listenLockPath(), 0)
	if err != nil {
		return err
	}
	if lock == nil {
		a.logf("监听：已经有一个 agentq listen 在跑，等它退出")
		for lock == nil {
			if lock, err = acquireLock(a.listenLockPath(), time.Hour); err != nil {
				return err
			}
		}
	}
	defer lock.Close()
	if err := writeFileAtomic(a.listenPidPath(), []byte(strconv.Itoa(os.Getpid())+"\n")); err != nil {
		a.logf("监听：写 %s 失败（不影响收消息）：%v", a.listenPidPath(), err)
	}
	l := &listener{}
	if self, err := os.Executable(); err == nil {
		l.self = self
		l.selfInfo, _ = os.Stat(self)
	}
	a.logf("监听：启动（pid %d），长轮询收 Telegram 消息", os.Getpid())
	for {
		if l.selfInfo != nil {
			if fi, err := os.Stat(l.self); err == nil && !os.SameFile(fi, l.selfInfo) {
				a.logf("监听：%s 换了新版本，退出，由 launchd 用新版本重启", l.self)
				return nil
			}
		}
		if d := a.listenStep(l); d > 0 {
			time.Sleep(d)
		}
	}
}

// listenStep 收一轮：每轮重读配置（改了 notify_enabled、换了凭据不用重启），长轮询一次，处理收到的，
// 再看收件箱里有没有等着交给 claude 的。返回下一轮前要歇多久。
func (a *App) listenStep(l *listener) time.Duration {
	cfg, err := loadConfig(a.configPath())
	if err != nil {
		l.say(a, "配置读不出，先不收消息："+err.Error())
		return listenIdleWait
	}
	a.Cfg = cfg
	if !cfg.NotifyEnabled {
		l.say(a, "推送关着（notify_enabled=false），不收消息")
		return listenIdleWait
	}
	c, err := a.telegramCreds()
	if c == nil {
		if err != nil {
			l.say(a, "Telegram 凭据用不了，不收消息："+err.Error())
		} else {
			l.say(a, "没配 Telegram（agentq notify setup 配），不收消息")
		}
		return listenIdleWait
	}
	s, err := a.loadNotify()
	if err != nil {
		l.say(a, "读不了 "+a.notifyPath()+"，先不收消息："+err.Error())
		return listenIdleWait
	}
	ups, err := a.tgUpdates(c, s.UpdateOffset, listenPollWait)
	if err != nil {
		l.fails++
		d := min(time.Duration(1<<min(l.fails, 6))*time.Second, listenRetryMax)
		var te *tgError
		if errors.As(err, &te) && te.Code == 409 { // 设了 webhook，或者别处也在 getUpdates
			d = listenRetryMax
		}
		l.say(a, "收消息失败，过一会儿再试："+err.Error())
		return d
	}
	if l.fails > 0 {
		a.logf("监听：收消息恢复了")
		l.fails, l.note = 0, ""
	}
	if len(ups) > 0 {
		a.takeUpdatesSafe(c, s.UpdateOffset == 0, ups)
	}
	a.listenServe(c)
	return 0
}

// listenServe：收件箱里有消息时拿 tick.lock 试着启动对话任务（上一个还在跑就不启动，它结束时的 tick 会接上）。
func (a *App) listenServe(c *tgCreds) {
	if s, err := a.loadNotify(); err != nil || len(s.Inbox) == 0 {
		return
	}
	lock, err := acquireLock(a.lockPath(), listenServeWait)
	if err != nil || lock == nil {
		return // tick 正在跑：它末尾会接着处理收件箱
	}
	defer lock.Close()
	st, err := loadState(a.statePath())
	if err != nil {
		a.logf("监听：状态文件读不出，先不启动对话任务，等 tick 处理：%v", err)
		return
	}
	if err := a.withNotify(func(s *NotifyState) error { a.serveInbox(s, st, c); return nil }); err != nil {
		a.logf("监听：%v", err)
	}
}

// pollIfNoListener：tick 兜底收消息。listen 在跑（拿着 listen.lock）就不收，免得两处抢同一批更新；
// 不在跑时拿着这把锁短轮询一次，期间 listen 被拉起也会先等锁。
func (a *App) pollIfNoListener(c *tgCreds) {
	lock, err := acquireLock(a.listenLockPath(), 0)
	if err != nil || lock == nil {
		return
	}
	defer lock.Close()
	s, err := a.loadNotify()
	if err != nil {
		a.logf("推送：%v", err)
		return
	}
	ups, err := a.tgUpdates(c, s.UpdateOffset, 0)
	if err != nil {
		_ = a.withNotify(func(s *NotifyState) error {
			a.notifyErr(s, "收 Telegram 消息失败（下次 tick 再收）："+err.Error())
			return nil
		})
		return
	}
	if len(ups) > 0 {
		a.takeUpdatesSafe(c, s.UpdateOffset == 0, ups)
	}
}

// listenRunning：有 agentq listen 拿着 listen.lock 时返回它的 pid（读不到 pid 文件返回 0）和真。
func (a *App) listenRunning() (int, bool) {
	lock, err := acquireLock(a.listenLockPath(), 0)
	if err != nil {
		return 0, false
	}
	if lock != nil {
		lock.Close()
		return 0, false
	}
	b, _ := os.ReadFile(a.listenPidPath())
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid, true
}

// listenStatus：status 里「监听」一行；没配 Telegram 或推送关着时返回空（不显示）。
func (a *App) listenStatus() string {
	if !a.Cfg.NotifyEnabled {
		return ""
	}
	if c, _ := a.telegramCreds(); c == nil {
		return ""
	}
	if pid, ok := a.listenRunning(); ok {
		if pid > 0 {
			return fmt.Sprintf("在跑（pid %d），主人的 Telegram 消息随到随回", pid)
		}
		return "在跑，主人的 Telegram 消息随到随回"
	}
	return "没在跑，tick 每 5 分钟兜底收一次消息（agentq install 装上常驻的 " + a.listenLabel() + "）"
}

const (
	ackNow       = "收到，交给 claude 了，答完回你。"
	ackQueued    = "收到，前面还有 %d 条在答，答完就轮到这条。"
	ackUnknown   = "现在只看得懂文字、图片、视频和文件，换成一句话发我。"
	ackVoice     = "语音听不了，打字发我。"
	textOnlyFile = "（没写字，只发了附件）"
)

// takeUpdatesSafe：处理一批更新时出了意外（panic），记日志并把 offset 推过这批，
// 免得一条怪消息让 listen 反复崩溃重启、后面的消息全收不到。
func (a *App) takeUpdatesSafe(c *tgCreds, first bool, ups []tgUpdate) {
	defer func() {
		p := recover()
		if p == nil {
			return
		}
		last := ups[len(ups)-1].UpdateID
		for _, u := range ups {
			last = max(last, u.UpdateID)
		}
		a.logf("Telegram：处理 %d 条更新时出错，跳过这批（offset 推到 %d）：%v", len(ups), last+1, p)
		if err := a.withNotify(func(s *NotifyState) error { s.UpdateOffset = max(s.UpdateOffset, last+1); return nil }); err != nil {
			a.logf("Telegram：跳过这批时存 offset 失败：%v", err)
		}
	}()
	a.takeUpdates(c, first, ups)
}

// takeUpdates 处理一批更新：只认主人的 chat id；第一次收消息（还没有 offset）时跳过配机器人那会儿的旧消息；
// 「状态」当场回；附件先下载；其余进收件箱并回一句「收到」。offset 和收件箱一起存进 notify.json（拿着 notify.lock，
// 下载和发消息都在锁外，免得卡住 notify send）。没存下时 offset 不前进，下次会再收到这几条。
func (a *App) takeUpdates(c *tgCreds, first bool, ups []tgUpdate) {
	maxID := int64(-1)
	var queue []InMsg
	var group []string // 和 queue 一一对应：相册的几张图只回一次「收到」
	reply := func(text string, to int64, what string) {
		if err := a.tgSend(c, text, to); err != nil {
			a.logf("Telegram：回复主人（%s）失败：%v", what, err)
		}
	}
	for _, u := range ups {
		maxID = max(maxID, u.UpdateID)
		m := u.Message
		if m == nil {
			continue
		}
		if m.Chat.ID != c.ChatID {
			a.logf("Telegram：忽略 chat id %d 发来的消息（只认 telegram.env 里配的那一个）", m.Chat.ID)
			continue
		}
		text := strings.TrimSpace(m.Text)
		if text == "" {
			text = strings.TrimSpace(m.Caption)
		}
		if first && a.Now().Sub(time.Unix(m.Date, 0)) > chatFirstPollMaxAge {
			a.logf("Telegram：第一次收消息，跳过 %s 发的旧消息「%s」", whenCN(time.Unix(m.Date, 0)), oneLine(text, 20))
			continue
		}
		if isStatusAsk(text) && !hasAttachment(m) {
			digest := "📊 状态读不出：" + a.statePath()
			if st, err := loadState(a.statePath()); err == nil {
				digest = a.statusDigest(st)
			}
			if err := a.tgSend(c, digest, m.MessageID); err != nil {
				a.logf("Telegram：回复「状态」失败：%v", err)
			} else {
				a.logf("Telegram：回了主人的「%s」（状态摘要）", oneLine(text, 20))
			}
			continue
		}
		files, notes := a.fetchAttachments(c, m)
		for _, n := range notes {
			reply(esc(n), m.MessageID, "附件说明")
		}
		if text == "" && len(files) == 0 {
			if len(notes) == 0 {
				reply(ackUnknown, m.MessageID, "看不懂的消息")
			}
			continue
		}
		if text == "" {
			text = textOnlyFile
		}
		in := InMsg{ID: m.MessageID, At: time.Unix(m.Date, 0), Text: text, Files: files}
		if m.ReplyTo != nil {
			in.ReplyTo = strings.TrimSpace(m.ReplyTo.Text)
			if in.ReplyTo == "" {
				in.ReplyTo = strings.TrimSpace(m.ReplyTo.Caption)
			}
		}
		queue = append(queue, in)
		group = append(group, m.MediaGroupID)
	}
	if maxID < 0 {
		return
	}
	err := a.withNotify(func(s *NotifyState) error {
		s.UpdateOffset = max(s.UpdateOffset, maxID+1)
		s.Inbox = append(s.Inbox, queue...)
		return nil
	})
	if err != nil {
		a.logf("Telegram：收到的 %d 条消息没存下（下次再收一遍）：%v", len(ups), err)
		return
	}
	if st, err := loadState(a.statePath()); err == nil && a.chatRefusal(st, a.Now()) != "" {
		return // 马上就会回「这条现在没法交给 claude」和原因，不先说「收到」
	}
	// 排着的消息会和这条一起交给下一个对话任务，所以「前面」只算正在答的那几条
	ahead := a.answeringCount()
	for i, in := range queue {
		if i > 0 && group[i] != "" && group[i] == group[i-1] {
			continue
		}
		ack := ackNow
		if ahead > 0 {
			ack = fmt.Sprintf(ackQueued, ahead)
		}
		reply(ack, in.ID, "收到")
		a.logf("Telegram：收到主人的消息「%s」（附件 %d 个），排队交给 claude", oneLine(in.Text, 40), len(in.Files))
	}
}

func hasAttachment(m *tgMsg) bool {
	return len(m.Photo) > 0 || m.Video != nil || m.Document != nil || m.Animation != nil || m.VideoNote != nil || m.Voice != nil || m.Audio != nil
}

// fetchAttachments 把消息里的图片、录屏、文件下载到 ~/.agentq/inbox/<消息号>-<文件名>（启动对话任务时挪进任务目录）。
// 超过 20MB 的、下载失败的、语音，各返回一句回给主人的说明。
func (a *App) fetchAttachments(c *tgCreds, m *tgMsg) (files, notes []string) {
	type att struct {
		f          *tgFile
		what, name string
	}
	var list []att
	if len(m.Photo) > 0 {
		list = append(list, att{&m.Photo[len(m.Photo)-1], "图片", "photo"})
	}
	for _, x := range []att{{m.Video, "视频", "video"}, {m.Animation, "动图", "animation"}, {m.VideoNote, "视频", "video_note"},
		{m.Document, "文件", "file"}, {m.Audio, "音频", "audio"}} {
		if x.f != nil {
			if x.f.FileName != "" {
				x.name = x.f.FileName
			}
			list = append(list, x)
		}
	}
	if m.Voice != nil {
		notes = append(notes, ackVoice)
	}
	for _, x := range list {
		if x.f.FileSize > tgDownloadMax {
			notes = append(notes, fmt.Sprintf("这个%s有 %s，超过 Telegram 给机器人下载的上限 20 MB，没收到；压缩一下再发，或者放到电脑上告诉我路径。",
				x.what, humanSize(x.f.FileSize)))
			continue
		}
		dest := filepath.Join(a.inboxDir(), fmt.Sprintf("%d-%s", m.MessageID, safeFileName(x.name)))
		remote, err := a.tgDownload(c, x.f.FileID, dest)
		if err != nil {
			a.logf("Telegram：主人发的%s没下载下来：%v", x.what, err)
			notes = append(notes, fmt.Sprintf("这个%s没下载下来（%s），再发一次试试。", x.what, oneLine(err.Error(), 80)))
			continue
		}
		if filepath.Ext(dest) == "" && filepath.Ext(remote) != "" {
			if err := os.Rename(dest, dest+filepath.Ext(remote)); err == nil {
				dest += filepath.Ext(remote)
			}
		}
		files = append(files, dest)
	}
	return files, notes
}

// safeFileName：只取文件名本身，去掉路径分隔符和开头的点，最长 80 字。
func safeFileName(name string) string {
	name = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == 0 || r < ' ' {
			return '_'
		}
		return r
	}, filepath.Base(name))
	name = strings.TrimLeft(name, ".")
	if name == "" {
		name = "file"
	}
	if r := []rune(name); len(r) > 80 {
		ext := filepath.Ext(name)
		if len([]rune(ext)) > 16 { // 「v1.2 一整句话」这种，点后面不是扩展名
			ext = ""
		}
		name = string(r[:80-len([]rune(ext))]) + ext
	}
	return name
}

// answeringCount：正在跑的对话任务手上有几条主人的消息（读任务目录的 inbox.json；读不到算 1），没在跑返回 0。
func (a *App) answeringCount() int {
	st, err := loadState(a.statePath())
	if err != nil {
		return 0
	}
	for _, j := range st.running() {
		if j.Kind != kindChat {
			continue
		}
		var msgs []InMsg
		if b, err := os.ReadFile(filepath.Join(a.jobDir(j.ID), "inbox.json")); err == nil && json.Unmarshal(b, &msgs) == nil && len(msgs) > 0 {
			return len(msgs)
		}
		return 1
	}
	return 0
}
