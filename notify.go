package main

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// 主动找主人：要主人知道的事推到手机（配了 Telegram）或本机通知（没配）。见 README「推送和 Telegram」。
// 推什么：每天的早报摘要（复盘写完、且不早于 08:00）、新的「等主人拍板」条目、巡逻发现且没自动处理的问题、
// 成品送达（agentq notify send，夜班和 agent 调用）、额度 ≥90% 和连环失败熔断。
// 怎么推：要推的消息先记进 ~/.agentq/notify.json 的队列，tick（和 notify send）按规则发：同类 30 分钟内只发一条，
// 后来的攒着、到点合成一条；23:00–08:00 只攒着，08:00 后和早报一起发；每天最多 10 条，早报和「要你定」不受上限挡，
// 超出的普通消息不丢，跟着下一条发出的消息一起带上（或等早报、第二天）；
// 安全问题（备份连续失败等）不等合并窗口、不管夜间和上限，当时就发。
const (
	notifyMergeWindow = 30 * time.Minute
	notifyDailyMax    = 10
	notifyQuietFrom   = 23 // 23:00–08:00 只攒着
	notifyQuietTo     = 8
	notifyMaxLines    = 12 // 一条消息最多几行（不算空行），超了发摘要和全文路径
	notifyPendingMax  = 100
	notifyLockWait    = 30 * time.Second
	morningStable     = 2 * time.Minute // 早报文件这么久没改才算写完
	notifyErrEvery    = time.Hour       // 同一个推送错误隔这么久才再记一次日志
)

type notifyCat struct{ emoji, name string }

var notifyCats = map[string]notifyCat{
	"morning": {"☀️", "早报"},
	"decide":  {"🔴", "要你定"},
	"alert":   {"🔴", "要你知道"},
	"patrol":  {"🟠", "巡逻发现"},
	"done":    {"🟢", "完成"},
	"release": {"📦", "新版可装"},
	"status":  {"📊", "状态"},
}

// notifyOrder：队列里能用的类别，也是发送顺序（要主人做的、出问题的在前）。
var notifyOrder = []string{"alert", "decide", "patrol", "release", "done"}

type Notice struct {
	Cat string `json:"cat"`
	// Key 非空时，队列里同 Key 的旧消息被新的替换（同一件事只推最新的说法）。
	Key   string   `json:"key,omitempty"`
	Text  string   `json:"text"`            // 一句话，纯文本（发送时转义）
	Lines []string `json:"lines,omitempty"` // 补充，纯文本，每条一行
	// Security：安全问题（密钥或生产信息泄露、备份连续失败），当时就推。
	Security bool      `json:"security,omitempty"`
	At       time.Time `json:"at"`
	// Files：跟着这条消息发的截图、录屏、文件（记下时已检查过大小、压缩过），发完消息再逐个发。
	Files []string `json:"files,omitempty"`
	// MacShown：Telegram 发不出去时已经弹过本机通知（继续留在队列里等 Telegram 恢复）。
	MacShown bool `json:"mac_shown,omitempty"`
}

type NotifyState struct {
	Pending  []Notice             `json:"pending,omitempty"`
	LastSent map[string]time.Time `json:"last_sent,omitempty"`
	// Day/Count：今天（本地日期）推了几条，到 notifyDailyMax 就只攒着。
	Day        string `json:"day,omitempty"`
	Count      int    `json:"count,omitempty"`
	MorningDay string `json:"morning_day,omitempty"` // 最近一次推早报的日期
	// DecideSeen：见过的「等主人拍板」条目（指纹 → 第一次见到的日期）。第一次运行只记下、不推，免得一下子推一堆旧的。
	DecideInit bool              `json:"decide_init,omitempty"`
	DecideSeen map[string]string `json:"decide_seen,omitempty"`
	// UpdateOffset：下次 getUpdates 的 offset（比处理过的最大 update_id 大 1）。agentq listen 收消息，它不在跑时 tick 兜底。
	UpdateOffset int64 `json:"update_offset,omitempty"`
	// Inbox：收到、还没交给 claude 的主人消息（上一次对话还在跑时排队）。
	Inbox []InMsg `json:"inbox,omitempty"`
	// Replies：对话任务的回答当时没发出去的，下次 tick 重发（不受合并、夜间和上限限制），留 replyKeep。
	Replies     []PendingReply `json:"replies,omitempty"`
	LastError   string         `json:"last_error,omitempty"`
	LastErrorAt time.Time      `json:"last_error_at,omitempty"`
}

type PendingReply struct {
	Text    string    `json:"text"`
	ReplyTo int64     `json:"reply_to,omitempty"`
	At      time.Time `json:"at"`
	JobID   string    `json:"job_id,omitempty"`
}

type InMsg struct {
	ID      int64     `json:"id"`
	At      time.Time `json:"at"`
	Text    string    `json:"text"`
	ReplyTo string    `json:"reply_to,omitempty"` // 主人回复的那条消息的原文
	// Files：主人发来的附件下载到本机的路径（收到时在 ~/.agentq/inbox/，交给 claude 时挪进那次对话任务目录的 files/）。
	Files []string `json:"files,omitempty"`
}

func (a *App) notifyPath() string     { return filepath.Join(a.dataDir(), "notify.json") }
func (a *App) notifyLockPath() string { return filepath.Join(a.dataDir(), "notify.lock") }

// loadNotify：不存在就从空的开始；内容坏了备份成 notify.json.bad-<时间> 再从空的开始（攒着的消息丢了，记日志）。
func (a *App) loadNotify() (*NotifyState, error) {
	s := &NotifyState{}
	b, err := os.ReadFile(a.notifyPath())
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, s); err != nil {
		bak := a.notifyPath() + ".bad-" + a.Now().Format("20060102-150405")
		if werr := writeFileAtomic(bak, b); werr != nil {
			return nil, fmt.Errorf("%s 格式不对（%v），备份到 %s 也失败：%w", a.notifyPath(), err, bak, werr)
		}
		a.logf("推送：%s 格式不对（%v），已备份为 %s，从空的开始（攒着的消息丢了）", a.notifyPath(), err, bak)
		return &NotifyState{}, nil
	}
	return s, nil
}

func (a *App) saveNotify(s *NotifyState) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(a.notifyPath(), append(b, '\n'))
}

// withNotify 拿着 notify.lock 读、改、存推送状态。tick、notify send、巡逻都走这里，互不覆盖。
func (a *App) withNotify(fn func(s *NotifyState) error) error {
	lock, err := acquireLock(a.notifyLockPath(), notifyLockWait)
	if err != nil {
		return err
	}
	if lock == nil {
		return fmt.Errorf("等了 %s 还拿不到 %s", notifyLockWait, a.notifyLockPath())
	}
	defer lock.Close()
	s, err := a.loadNotify()
	if err != nil {
		return err
	}
	// 拿着锁期间本进程里再要记消息（比如启动对话任务失败触发熔断），直接记进这份，不再去拿同一把锁
	a.notifyHeld = s
	ferr := fn(s)
	a.notifyHeld = nil
	if err := a.saveNotify(s); err != nil {
		return err
	}
	return ferr
}

// notify 把要推的消息记进队列（不在这里发：tick 末尾统一发，notify send 记完马上发）。出错只记日志，不影响调用方。
func (a *App) notify(list ...Notice) {
	if !a.Cfg.NotifyEnabled || len(list) == 0 {
		return
	}
	if a.notifyHeld != nil {
		a.notifyHeld.add(a.Now(), list...)
		return
	}
	if err := a.withNotify(func(s *NotifyState) error { s.add(a.Now(), list...); return nil }); err != nil {
		a.logf("推送：没记下 %d 条要推的消息：%v", len(list), err)
	}
}

func (s *NotifyState) add(now time.Time, list ...Notice) {
	for _, n := range list {
		if n.At.IsZero() {
			n.At = now
		}
		replaced := false
		if n.Key != "" {
			for i := range s.Pending {
				if s.Pending[i].Key == n.Key {
					s.Pending[i], replaced = n, true
					break
				}
			}
		}
		if !replaced {
			s.Pending = append(s.Pending, n)
		}
	}
	if len(s.Pending) > notifyPendingMax {
		s.Pending = s.Pending[len(s.Pending)-notifyPendingMax:]
	}
}

func isQuiet(t time.Time) bool { return t.Hour() >= notifyQuietFrom || t.Hour() < notifyQuietTo }

// notifyErr 记推送出的错：同样的错一小时只记一次日志，免得每 5 分钟刷一行。
func (a *App) notifyErr(s *NotifyState, msg string) {
	now := a.Now()
	if msg == s.LastError && now.Sub(s.LastErrorAt) < notifyErrEvery && !now.Before(s.LastErrorAt) {
		return
	}
	s.LastError, s.LastErrorAt = msg, now
	a.logf("推送：%s", msg)
}

// macNotify 弹一条本机通知。环境变量 AGENTQ_MACNOTIFY_LOG 非空时改成往那个文件追加一行（测试用，不弹真通知）。
func (a *App) macNotify(title, body string) error {
	if a.MacNotify != nil {
		return a.MacNotify(title, body)
	}
	if p := os.Getenv("AGENTQ_MACNOTIFY_LOG"); p != "" {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = fmt.Fprintf(f, "%s｜%s\n", title, strings.ReplaceAll(body, "\n", "⏎"))
		return err
	}
	// 用参数传标题和正文，不拼进脚本，免得引号、反斜杠出问题
	out, err := exec.Command("/usr/bin/osascript", "-e", "on run argv",
		"-e", "display notification (item 2 of argv) with title (item 1 of argv)", "-e", "end run", title, body).CombinedOutput()
	if err != nil {
		return fmt.Errorf("osascript：%v %s", err, oneLine(string(out), 200))
	}
	return nil
}

// deliver 发一条已排好版的消息：配了 Telegram 走 Telegram，没配（c 为空且没有凭据错误）走本机通知。返回用的通道名。
func (a *App) deliver(c *tgCreds, credErr error, msg string) (string, error) {
	if c != nil {
		return "Telegram", a.tgSend(c, msg, 0)
	}
	if credErr != nil {
		return "Telegram", credErr
	}
	title, body, _ := strings.Cut(htmlToPlain(msg), "\n")
	return "本机通知", a.macNotify(strings.TrimSpace(title), strings.TrimSpace(body))
}

// flushNotify 按规则把队列里到点的消息发出去（调用方拿着 notify.lock）。st 可以为空（读不出状态时不推早报）。
func (a *App) flushNotify(s *NotifyState, st *State, c *tgCreds, credErr error) {
	now := a.Now()
	if day := now.Format("2006-01-02"); s.Day != day {
		s.Day, s.Count = day, 0
	}
	if s.LastSent == nil {
		s.LastSent = map[string]time.Time{}
	}
	quiet := isQuiet(now)
	if !quiet && st != nil {
		a.pushMorning(s, st, c, credErr)
	}
	for _, cat := range notifyOrder {
		var group []Notice
		for _, n := range s.Pending {
			if n.Cat == cat {
				group = append(group, n)
			}
		}
		if len(group) == 0 {
			continue
		}
		last := s.LastSent[cat]
		// 到了每天上限（每类按当时的条数算，同一次里前面发的也算）：只有「要你定」和安全问题照发，其余攒着，搭这些消息一起发出去
		capped := s.Count >= notifyDailyMax
		due := !quiet && (!capped || bypassCap(cat)) && (now.Sub(last) >= notifyMergeWindow || now.Before(last))
		batch := group
		if !due {
			batch = nil
			for _, n := range group {
				if n.Security {
					batch = append(batch, n)
				}
			}
			if len(batch) == 0 {
				continue
			}
		}
		sortNotices(batch)
		var riders []Notice // 上限挡下的普通消息，搭这条一起发（夜里不搭：只有安全问题能在夜里发）
		if capped && !quiet {
			riders = cappedRiders(s.Pending, batch)
		}
		var secs []msgSection
		for _, n := range append(append([]Notice(nil), batch...), riders...) {
			secs = append(secs, msgSection{Emoji: notifyCats[n.Cat].emoji, Head: n.Text, Lines: append(append([]string(nil), n.Lines...), a.fileLines(c, n.Files)...)})
		}
		msg := a.renderMessage(cat, now, secs, "agentq")
		via, err := a.deliver(c, credErr, msg)
		if err != nil {
			a.notifyErr(s, fmt.Sprintf("%s 发送失败，%d 条留在队列里下次再试：%v", via, len(batch), err))
			if via == "Telegram" {
				a.macFallback(s, batch, msg)
			}
			continue
		}
		s.removePending(append(append([]Notice(nil), batch...), riders...))
		s.LastSent[cat] = now
		s.Count++
		s.LastError = ""
		if len(riders) > 0 {
			a.logf("推送：发出「%s」%d 条，带上上限挡下的 %d 条（%s）", notifyCats[cat].name, len(batch), len(riders), via)
		} else {
			a.logf("推送：发出「%s」%d 条（%s）", notifyCats[cat].name, len(batch), via)
		}
		a.queueNoticeFiles(c, batch)
		a.queueNoticeFiles(c, riders)
	}
}

// bypassCap：不受每天条数上限挡的类别（要主人做决定的）。安全问题按 Notice.Security 另算。
func bypassCap(cat string) bool { return cat == "decide" }

// cappedRiders：队列里被每天上限挡下的普通消息（不是「要你定」、不是安全问题、不在这一批里），按类别顺序排好。
func cappedRiders(pending, batch []Notice) []Notice {
	var out []Notice
	for _, n := range pending {
		if bypassCap(n.Cat) || n.Security {
			continue
		}
		in := false
		for _, m := range batch {
			if sameNotice(n, m) {
				in = true
				break
			}
		}
		if !in {
			out = append(out, n)
		}
	}
	sort.SliceStable(out, func(i, k int) bool { return catRank(out[i].Cat) < catRank(out[k].Cat) })
	return out
}

// macFallback：Telegram 发不出去时，没弹过本机通知的消息弹一次（主人在电脑前就能先看到），消息仍留着等 Telegram。
func (a *App) macFallback(s *NotifyState, batch []Notice, msg string) {
	fresh := false
	for _, n := range batch {
		if !n.MacShown {
			fresh = true
		}
	}
	if !fresh {
		return
	}
	title, body, _ := strings.Cut(htmlToPlain(msg), "\n")
	if err := a.macNotify(strings.TrimSpace(title)+"（Telegram 没发出去）", strings.TrimSpace(body)); err != nil {
		a.logf("推送：本机通知也没弹出来：%v", err)
		return
	}
	for i := range s.Pending {
		for _, n := range batch {
			if sameNotice(s.Pending[i], n) {
				s.Pending[i].MacShown = true
			}
		}
	}
}

// fileLines：没配 Telegram（发本机通知）时，附件写成「附件 <路径>」几行；走 Telegram 时附件另发，不写。
func (a *App) fileLines(c *tgCreds, files []string) []string {
	if c != nil {
		return nil
	}
	var out []string
	for _, f := range files {
		out = append(out, "附件 "+a.tilde(f))
	}
	return out
}

// queueNoticeFiles：消息发到 Telegram 之后，把这批消息带的附件记下，等放开 notify.lock 再由 sendQueuedFiles 发
// （上传一个大录屏可能要几十秒，不能让 notify send 和收消息等着锁）。
func (a *App) queueNoticeFiles(c *tgCreds, batch []Notice) {
	if c == nil {
		return
	}
	for _, n := range batch {
		a.filesOut = append(a.filesOut, n.Files...)
	}
}

// sendQueuedFiles 逐个发出 queueNoticeFiles 记下的附件（调用方已放开 notify.lock）。发不出去只记日志，不重发（消息本身已经到了）。
func (a *App) sendQueuedFiles() {
	files := a.filesOut
	a.filesOut = nil
	if len(files) == 0 {
		return
	}
	c, err := a.telegramCreds()
	if c == nil {
		a.logf("推送：%d 个附件没发出去：Telegram 凭据用不了 %v", len(files), err)
		return
	}
	for _, f := range files {
		if err := a.tgSendFile(c, f, "", 0); err != nil {
			a.logf("推送：附件 %s 没发出去：%v", a.tilde(f), c.redact(err.Error()))
		}
	}
}

func sameNotice(x, y Notice) bool {
	return x.Cat == y.Cat && x.Key == y.Key && x.Text == y.Text && x.At.Equal(y.At)
}

func (s *NotifyState) removePending(sent []Notice) {
	kept := s.Pending[:0]
	for _, n := range s.Pending {
		drop := false
		for _, m := range sent {
			if sameNotice(n, m) {
				drop = true
				break
			}
		}
		if !drop {
			kept = append(kept, n)
		}
	}
	s.Pending = kept
}

// sortNotices：安全问题在前，其余按记下的先后。
func sortNotices(list []Notice) {
	sort.SliceStable(list, func(i, k int) bool { return list[i].Security && !list[k].Security })
}

type msgSection struct {
	Emoji string   // 段首的表情，段内不再加
	Head  string   // 段的第一行，纯文本
	Lines []string // 后面几行，纯文本
}

func dateCN(t time.Time) string { return fmt.Sprintf("%d月%d日", t.Month(), t.Day()) }

var weekdayCN = [...]string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}

// whenCN：给人看的时间一律写成「10-13 周二 21:00」，星期由程序算好（推送、汇总里的时间模型只照抄，不自己换算）。
func whenCN(t time.Time) string {
	return t.Format("01-02") + " " + weekdayCN[t.Weekday()] + " " + t.Format("15:04")
}

// renderMessage 排版：标题行「<表情> <b>类别 · 日期</b>」，空一行，各段（段间空一行），最后一行「<i>时间 · 来源</i>」。
// 不算空行超过 notifyMaxLines 时只留前面几行，末尾写「其余 N 行见 <全文路径>」，全文存到 ~/.agentq/notify/。
func (a *App) renderMessage(cat string, now time.Time, secs []msgSection, source string) string {
	ci := notifyCats[cat]
	header := ci.emoji + " <b>" + esc(ci.name) + " · " + dateCN(now) + "</b>"
	footer := "<i>" + now.Format("15:04") + " · " + esc(source) + "</i>"
	var lines []string // 空串是段间空行
	for i, s := range secs {
		if i > 0 {
			lines = append(lines, "")
		}
		head := s.Head
		if s.Emoji != "" {
			head = s.Emoji + " " + head
		}
		lines = append(lines, head)
		for _, l := range s.Lines {
			if strings.TrimSpace(l) != "" {
				lines = append(lines, l)
			}
		}
	}
	budget := notifyMaxLines - 2
	n := 0
	for _, l := range lines {
		if l != "" {
			n++
		}
	}
	var body []string
	if n <= budget {
		for _, l := range lines {
			body = append(body, esc(l))
		}
	} else {
		full := htmlToPlain(header) + "\n\n" + strings.Join(lines, "\n") + "\n" + htmlToPlain(footer) + "\n"
		path := a.saveFullText(now, cat, full)
		kept := 0
		for _, l := range lines {
			if l != "" {
				if kept == budget-1 {
					break
				}
				kept++
			}
			body = append(body, esc(l))
		}
		for len(body) > 0 && body[len(body)-1] == "" {
			body = body[:len(body)-1]
		}
		body = append(body, esc(fmt.Sprintf("其余 %d 行见 %s", n-kept, path)))
	}
	return header + "\n\n" + strings.Join(body, "\n") + "\n" + footer
}

// saveFullText 把超长消息的全文存到 ~/.agentq/notify/<时间>-<类别>.txt，返回给人看的路径（~ 开头）；存不了返回说明。
func (a *App) saveFullText(now time.Time, cat, text string) string {
	path := filepath.Join(a.dataDir(), "notify", now.Format("20060102-150405")+"-"+cat+".txt")
	if err := writeFileAtomic(path, []byte(text)); err != nil {
		a.logf("推送：存全文 %s 失败：%v", path, err)
		return "（全文没存下：" + err.Error() + "）"
	}
	return a.tilde(path)
}

func (a *App) tilde(p string) string {
	if rel, err := filepath.Rel(a.Home, p); err == nil && !strings.HasPrefix(rel, "..") {
		return "~/" + rel
	}
	return p
}

// reportPushedPath：早报推到 Telegram 后写的标记，会话里的早报转告看到它就不再转告。
func (a *App) reportPushedPath(day string) string {
	return filepath.Join(a.Home, ".claude", "state", "report-pushed-"+day)
}

// morningReady：今天还没推过早报、早报文件写完了（存在、morningStable 内没再改、复盘任务没在跑）时返回早报路径。
func (a *App) morningReady(s *NotifyState, st *State, now time.Time) (string, bool) {
	day := now.Format("2006-01-02")
	if s.MorningDay == day {
		return "", false
	}
	if _, err := os.Stat(a.reportPushedPath(day)); err == nil {
		s.MorningDay = day // 别处已经推过（标记在），不再推
		return "", false
	}
	path := filepath.Join(a.Home, ".claude", "nightly", day+".md")
	fi, err := os.Stat(path)
	if err != nil || now.Sub(fi.ModTime()) < morningStable {
		return "", false
	}
	for _, j := range st.running() {
		if j.Kind == kindReview {
			return "", false
		}
	}
	return path, true
}

// pushMorning 早报摘要：要你定的、出问题的放最前，再写昨晚完成、任务成败和额度，最后是早报第一行；
// 队列里攒着的消息（夜里攒下的）一起带上。不受每天上限挡（但算一条）。推到 Telegram 成功才写 report-pushed 标记。
func (a *App) pushMorning(s *NotifyState, st *State, c *tgCreds, credErr error) {
	now := a.Now()
	path, ok := a.morningReady(s, st, now)
	if !ok {
		return
	}
	day := now.Format("2006-01-02")
	var secs []msgSection
	held := append([]Notice(nil), s.Pending...)
	sort.SliceStable(held, func(i, k int) bool { return catRank(held[i].Cat) < catRank(held[k].Cat) })
	for _, n := range held {
		if n.Cat == "decide" { // 下面「要你定」一段已经列了
			continue
		}
		secs = append(secs, msgSection{Emoji: notifyCats[n.Cat].emoji, Head: n.Text})
	}
	all, _ := scanDecisions(a.projectsRoot())
	if decs := askable(all); len(decs) > 0 {
		sec := msgSection{Emoji: "🔴", Head: fmt.Sprintf("要你定　%d 条", len(decs))}
		for i, d := range decs {
			if i == 3 {
				sec.Lines = append(sec.Lines, fmt.Sprintf("另有 %d 条", len(decs)-3))
				break
			}
			sec.Lines = append(sec.Lines, d.Project+"　"+truncRunes(d.Title, 40))
		}
		secs = append(secs, sec)
	}
	if ps, err := a.loadPatrol(); err == nil {
		var titles []string
		for _, is := range ps.Issues {
			if is.Check != "decide" && !is.infra() { // 基础设施类由看门狗推
				titles = append(titles, truncRunes(is.Title, 40))
			}
		}
		if len(titles) > 0 {
			sec := msgSection{Emoji: "🟠", Head: fmt.Sprintf("出问题　%d 个", len(titles))}
			if len(titles) > 2 {
				titles = append(titles[:2], fmt.Sprintf("另有 %d 个（agentq patrol 看证据）", len(titles)-2))
			}
			sec.Lines = titles
			secs = append(secs, sec)
		}
	}
	var done []string
	counts := map[string]int{}
	for _, j := range st.Jobs {
		if j.Kind != "" || j.EndedAt == nil || now.Sub(*j.EndedAt) > 24*time.Hour {
			continue
		}
		counts[j.Status]++
		if j.Status == statusDone {
			done = append(done, j.Project+"　"+truncRunes(j.Title, 30))
		}
	}
	if len(done) > 0 {
		sec := msgSection{Emoji: "🟢", Head: fmt.Sprintf("昨晚完成　%d 条", len(done))}
		if len(done) > 3 {
			done = append(done[:3], fmt.Sprintf("另有 %d 条", len(done)-3))
		}
		sec.Lines = done
		secs = append(secs, sec)
	}
	tasks := fmt.Sprintf("任务　跑完 %d，失败 %d，中断 %d，超时 %d（24 小时内）",
		counts[statusDone], counts[statusFailed], counts[statusInterrupted], counts[statusTimeout])
	secs = append(secs, msgSection{Emoji: "📊", Head: tasks, Lines: []string{"额度　" + st.Usage.describe(now, a.Cfg.Model)}})
	if first := reportFirstLine(path); first != "" {
		secs = append(secs, msgSection{Emoji: "📝", Head: first})
	}
	msg := a.renderMessage("morning", now, secs, "早报全文 "+a.tilde(path))
	via, err := a.deliver(c, credErr, msg)
	if err != nil {
		a.notifyErr(s, fmt.Sprintf("早报发送失败（%s），下次 tick 再试：%v", via, err))
		return
	}
	s.MorningDay = day
	s.LastSent["morning"] = now
	s.Count++
	s.Pending = nil
	a.queueNoticeFiles(c, held)
	a.logf("推送：发出早报摘要，带上攒着的 %d 条（%s）", len(held), via)
	if via != "Telegram" {
		return
	}
	if err := writeFileAtomic(a.reportPushedPath(day), []byte(now.Format(time.RFC3339)+"\n")); err != nil {
		a.logf("推送：早报已推，但写标记 %s 失败（会话里会再转告一次）：%v", a.reportPushedPath(day), err)
	}
}

func catRank(cat string) int {
	for i, c := range notifyOrder {
		if c == cat {
			return i
		}
	}
	return len(notifyOrder)
}

// reportFirstLine：早报的第一行（一句话总结），去掉 Markdown 的标题井号和加粗。
func reportFirstLine(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(strings.ReplaceAll(strings.TrimLeft(l, "# "), "**", ""))
		if l != "" {
			return truncRunes(l, 120)
		}
	}
	return ""
}

type decision struct {
	Project, Title, Text, Fingerprint, Path string
	// Waiting/Cond：条目带「（等条件：…）」标记，前置条件还没满足，现在问主人也定不了。
	// 巡逻、推送、早报和「状态」的「要你定」都跳过它，只在早报详情列一行。
	Waiting bool
	Cond    string
}

// askable 去掉带等条件标记的条目，剩下现在就要主人定的。
func askable(decs []decision) []decision {
	var out []decision
	for _, d := range decs {
		if !d.Waiting {
			out = append(out, d)
		}
	}
	return out
}

// scanDecisions 列出各项目 BACKLOG「等主人拍板」里的条目（「- [ ]」开头），指纹按标题算（标题不含等条件标记）。
// 带等条件标记的也列出来（Waiting），用的地方按需用 askable 去掉。读不了的项目返回说明。
func scanDecisions(root string) ([]decision, []string) {
	projects, err := scanProjects(root)
	if err != nil {
		return nil, []string{"扫描项目目录失败：" + err.Error()}
	}
	var out []decision
	var fails []string
	for _, p := range projects {
		b, err := os.ReadFile(p.backlogPath())
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			fails = append(fails, p.Name+" 的 BACKLOG 读不了："+err.Error())
			continue
		}
		eachBacklogLine(string(b), func(section, line string) {
			if !(strings.HasPrefix(section, "等主人拍板") || strings.HasPrefix(section, "等用户决定")) || !strings.HasPrefix(line, "- [ ]") {
				return
			}
			text := strings.TrimSpace(strings.TrimPrefix(line, "- [ ]"))
			if text == "" {
				return
			}
			title := itemTitle(text)
			sum := sha1.Sum([]byte(title))
			cond, waiting := waitCondition(text)
			out = append(out, decision{Project: p.Name, Title: title, Text: text, Path: p.backlogPath(),
				Fingerprint: "decide:" + p.Name + ":" + hex.EncodeToString(sum[:6]), Waiting: waiting, Cond: cond})
		})
	}
	return out, fails
}

// watchDecisions：每次 tick 看一遍各项目「等主人拍板」，新出现的条目推一条「要你定」（主人回复那条消息就能拍板）。
// 带等条件标记的不推、也不记进 DecideSeen：标记去掉（条件满足）后它才算新出现，那时再推。
func (a *App) watchDecisions(s *NotifyState) {
	all, fails := scanDecisions(a.projectsRoot())
	decs := askable(all)
	if s.DecideSeen == nil {
		s.DecideSeen = map[string]string{}
	}
	now := a.Now()
	cur := map[string]bool{}
	for _, d := range decs {
		cur[d.Fingerprint] = true
		if _, ok := s.DecideSeen[d.Fingerprint]; ok {
			continue
		}
		s.DecideSeen[d.Fingerprint] = now.Format("2006-01-02")
		if !s.DecideInit {
			continue
		}
		var lines []string
		if _, rest, ok := strings.Cut(d.Text, "——"); ok && strings.TrimSpace(rest) != "" {
			lines = append(lines, oneLine(rest, 150))
		}
		lines = append(lines, "回复这条消息说选哪个，就按你的决定接着做")
		s.add(now, Notice{Cat: "decide", Key: d.Fingerprint, Text: d.Project + "　" + truncRunes(d.Title, 60), Lines: lines})
	}
	if len(fails) == 0 { // 有项目读不了时不知道它的条目还在不在，先都记着
		for fp := range s.DecideSeen {
			if !cur[fp] {
				delete(s.DecideSeen, fp)
			}
		}
	}
	s.DecideInit = true
}

// notifyTick：tick 末尾调用。看「等主人拍板」有没有新条目；agentq listen 没在跑时兜底收主人发来的 Telegram 消息；
// 收件箱里有消息、上一个对话任务结束了就启动下一个；把到点的消息发出去。
// 出任何错都只记日志，不能让 tick 非 0 退出（连续 2 次会触发自动回滚）。
func (a *App) notifyTick(st *State) {
	if !a.Cfg.NotifyEnabled {
		return
	}
	defer func() {
		if p := recover(); p != nil {
			a.logf("推送：出错，本次不推送，不影响 tick：%v", p)
		}
	}()
	if c, _ := a.telegramCreds(); c != nil {
		a.pollIfNoListener(c) // 自己拿 notify.lock，附件下载在锁外
	}
	err := a.withNotify(func(s *NotifyState) error {
		c, credErr := a.telegramCreds()
		if credErr != nil {
			a.notifyErr(s, "Telegram 凭据用不了（推送先弹本机通知）："+credErr.Error())
		}
		a.watchDecisions(s)
		if c != nil {
			a.retryReplies(s, c)
			a.serveInbox(s, st, c)
		}
		a.flushNotify(s, st, c, credErr)
		return nil
	})
	if err != nil {
		a.logf("推送：%v", err)
	}
	a.sendQueuedFiles()
}

const notifyUsage = `用法：
  agentq notify setup                    配 Telegram（执行 ~/.agentq/secrets/telegram-setup.sh；没有这个脚本就打印手动配置步骤）
  agentq notify test                     立刻发一条测试消息（配了 Telegram 发 Telegram，没配弹本机通知），不占每天的条数
  agentq notify send [--security] [--key=K] [--file=路径]… <类别> <一句话> [补充…]
                                         记一条要推给主人的消息并按规则推（类别：decide 要你定、alert 要你知道、
                                         patrol 巡逻发现、release 新版可装、done 完成）；夜班和 agent 用。
                                         --file 附上截图、效果图（按图片发）、录屏（按视频发，超过 50MB 先压缩），跟在消息后面发
  agentq notify file <路径> [说明]        马上把一个文件发给主人（Telegram；对话任务里回图片、录屏用，不进队列、不占条数）
  agentq notify status                   用哪个通道、今天推了几条、攒着哪些
`

func (a *App) cmdNotify(args []string) error {
	if len(args) == 0 {
		return errors.New("缺子命令\n" + notifyUsage)
	}
	switch args[0] {
	case "setup":
		return a.notifySetup()
	case "test":
		return a.notifyTest()
	case "send":
		return a.notifySend(args[1:])
	case "file":
		return a.notifyFile(args[1:])
	case "status":
		return a.notifyStatusCmd()
	}
	return fmt.Errorf("不认识的子命令 %q\n%s", args[0], notifyUsage)
}

func (a *App) notifySetup() error {
	script := filepath.Join(a.dataDir(), "secrets", "telegram-setup.sh")
	if _, err := os.Stat(script); err == nil {
		cmd := exec.Command("/bin/bash", script)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, a.Out, a.Out
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s 没跑成功：%w", script, err)
		}
		fmt.Fprintln(a.Out, "配好后执行 agentq notify test 收一条测试消息。")
		return nil
	}
	fmt.Fprintf(a.Out, `没有 %s，按下面手动配：
1. 在 Telegram 里找 @BotFather，发 /newbot 建一个机器人，记下它给的 token。
2. 给你的机器人发一句话，然后在浏览器打开 https://api.telegram.org/bot<token>/getUpdates，找到 "chat":{"id":…} 里的数字。
3. 写 %s（两行），再 chmod 600 它：
   TELEGRAM_BOT_TOKEN=<token>
   TELEGRAM_CHAT_ID=<数字>
4. 执行 agentq notify test，手机上收到测试消息就好了。之后 agentq 只给这个 chat id 发消息，也只认它发来的消息。
`, script, a.telegramEnvPath())
	return nil
}

func (a *App) notifyTest() error {
	c, credErr := a.telegramCreds()
	now := a.Now()
	msg := a.renderMessage("status", now, []msgSection{
		{Emoji: "🟢", Head: "推送通了", Lines: []string{"以后要你定的事、巡逻发现的问题、每天的早报会推到这里", "回一句「状态」试试双向"}},
	}, "agentq notify test")
	via, err := a.deliver(c, credErr, msg)
	if err != nil {
		return fmt.Errorf("发送失败（%s）：%w", via, err)
	}
	fmt.Fprintf(a.Out, "测试消息已发出（%s）。\n", via)
	if c == nil {
		fmt.Fprintln(a.Out, "（没配 Telegram，只弹了本机通知；要推到手机执行 agentq notify setup）")
	}
	return nil
}

func (a *App) notifySend(args []string) error {
	var n Notice
	for len(args) > 0 && strings.HasPrefix(args[0], "--") {
		switch {
		case args[0] == "--security":
			n.Security = true
		case strings.HasPrefix(args[0], "--key="):
			n.Key = strings.TrimPrefix(args[0], "--key=")
		case strings.HasPrefix(args[0], "--file="):
			p := strings.TrimPrefix(args[0], "--file=")
			path, err := filepath.Abs(expandHome(a.Home, p))
			if err != nil || p == "" {
				return fmt.Errorf("--file 的路径不对：%q", p)
			}
			// 现在就检查、压缩（在调用方的进程里做，不让 tick 拿着锁去压录屏）；发不了的改成一行带路径的说明
			send, note, err := a.prepareMedia(path)
			if err != nil {
				return err
			}
			if note != "" {
				n.Lines = append(n.Lines, note)
			} else {
				n.Files = append(n.Files, send)
			}
		default:
			return fmt.Errorf("不认识的选项 %s\n%s", args[0], notifyUsage)
		}
		args = args[1:]
	}
	if len(args) < 2 || catRank(args[0]) == len(notifyOrder) || strings.TrimSpace(args[1]) == "" {
		return errors.New("要写类别和一句话\n" + notifyUsage)
	}
	n.Cat, n.Text, n.At = args[0], strings.TrimSpace(args[1]), a.Now()
	notes := n.Lines
	n.Lines = nil
	for _, l := range args[2:] {
		if l = strings.TrimSpace(l); l != "" {
			n.Lines = append(n.Lines, l)
		}
	}
	n.Lines = append(n.Lines, notes...)
	if !a.Cfg.NotifyEnabled {
		fmt.Fprintln(a.Out, "推送关着（notify_enabled=false），没记")
		return nil
	}
	st, _ := loadState(a.statePath()) // 读不出就不顺带推早报
	var pending int
	err := a.withNotify(func(s *NotifyState) error {
		s.add(a.Now(), n)
		c, credErr := a.telegramCreds()
		a.flushNotify(s, st, c, credErr)
		pending = len(s.Pending)
		for _, p := range s.Pending {
			if sameNotice(p, n) {
				fmt.Fprintln(a.Out, "已记下，按规则稍后推（同类 30 分钟内合并、夜里攒到 08:00、每天最多 10 条）")
				return nil
			}
		}
		fmt.Fprintln(a.Out, "已推出")
		return nil
	})
	a.sendQueuedFiles()
	if err == nil && pending > 0 {
		fmt.Fprintf(a.Out, "（队列里还攒着 %d 条）\n", pending)
	}
	return err
}

func (a *App) notifyStatusCmd() error {
	fmt.Fprintf(a.Out, "推送：%s\n", a.notifyStatus())
	s, err := a.loadNotify()
	if err != nil {
		return err
	}
	for _, n := range s.Pending {
		sec := ""
		if n.Security {
			sec = "（安全）"
		}
		fmt.Fprintf(a.Out, "  攒着：%s %s「%s」%s\n", whenCN(n.At), notifyCats[n.Cat].name, n.Text, sec)
	}
	if len(s.Inbox) > 0 {
		fmt.Fprintf(a.Out, "  主人发来、排队等回答的消息 %d 条\n", len(s.Inbox))
	}
	if s.LastError != "" {
		fmt.Fprintf(a.Out, "  最近的错误（%s）：%s\n", whenCN(s.LastErrorAt), s.LastError)
	}
	return nil
}

// notifyStatus：status 里的一行（读 notify.json，不拿锁）。
func (a *App) notifyStatus() string {
	if !a.Cfg.NotifyEnabled {
		return "关（notify_enabled=false）"
	}
	via := "本机通知（没配 Telegram，agentq notify setup 配）"
	if c, err := a.telegramCreds(); err != nil {
		via = "Telegram 凭据用不了：" + err.Error()
	} else if c != nil {
		via = "Telegram"
	}
	s, err := a.loadNotify()
	if err != nil {
		return via + "；读不了 " + a.notifyPath() + "：" + err.Error()
	}
	count := 0
	if s.Day == a.Now().Format("2006-01-02") {
		count = s.Count
	}
	return fmt.Sprintf("%s，今天已推 %d/%d 条，攒着 %d 条", via, count, notifyDailyMax, len(s.Pending))
}
