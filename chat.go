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

// Telegram 双向：agentq listen 长轮询收主人发来的消息（见 listen.go，只认 telegram.env 里的 chat id）。
// 「状态」这类直接回 agentq 状态摘要（不花额度）；其余交给一次 claude -p（带主人的全局规则和技能）理解意图：
// 派活、拍板、问问题、小事当场办。它的最终回答原样回发给主人。同一时间只跑一个对话任务，期间来的消息排队，下一次一起交给它。
const (
	kindChat    = "chat"
	chatJobName = "Telegram对话" // 也是任务 id 的一部分，不能有空格（agentq logs <id> 要能直接复制）
	chatTimeout = 20 * time.Minute
	// chatFirstPollMaxAge：第一次收消息（还没有 offset）时，比这更早的消息不处理。
	chatFirstPollMaxAge = 10 * time.Minute
)

// isStatusAsk：只认这几个固定说法（整条消息就是它），别的话都交给 claude 理解。
func isStatusAsk(text string) bool {
	switch strings.ToLower(strings.TrimSpace(strings.TrimRight(text, "。.？?"))) {
	case "状态", "status", "/status", "/start":
		return true
	}
	return false
}

// serveInbox：有排队的消息、没有对话任务在跑时，启动一次 claude -p 处理它们（调用方拿着 tick.lock 和 notify.lock：
// tick 末尾，或 agentq listen 收到消息后）。不看暂停和熔断（主人亲口问的，同 agentq run），
// 用量冷却、额度 ≥98%、没登录时不启动，直接回主人原因。附件挪进这次对话任务目录的 files/，路径写进 prompt。
func (a *App) serveInbox(s *NotifyState, st *State, c *tgCreds) {
	if len(s.Inbox) == 0 {
		return
	}
	for _, j := range st.running() {
		if j.Kind == kindChat {
			return // 上一个还在跑，结束后的那次 tick 接着处理
		}
	}
	last := s.Inbox[len(s.Inbox)-1].ID
	refuse := func(why string) {
		msg := a.renderMessage("alert", a.Now(), []msgSection{{Emoji: "🔴", Head: "这条现在没法交给 claude", Lines: []string{why, "发「状态」看调度情况"}}}, "agentq 对话")
		if err := a.tgSend(c, msg, last); err != nil {
			a.notifyErr(s, "回复主人失败："+err.Error())
		}
		for _, m := range s.Inbox {
			for _, f := range m.Files {
				os.Remove(f)
			}
		}
		s.Inbox = nil
	}
	now := a.Now()
	if why := a.chatRefusal(st, now); why != "" {
		refuse(why)
		return
	}
	id, err := a.newJobID(chatJobName)
	if err != nil {
		refuse("建不了任务目录：" + err.Error())
		return
	}
	inbox := a.moveChatFiles(id, s.Inbox)
	if b, err := json.MarshalIndent(inbox, "", "  "); err == nil {
		_ = writeFileAtomic(filepath.Join(a.jobDir(id), "inbox.json"), append(b, '\n'))
	}
	start := a.StartChat
	if start == nil {
		start = a.startChatJob
	}
	j, err := start(st, id, chatPrompt(inbox, now))
	if err != nil {
		refuse("没能启动 claude：" + err.Error())
		return
	}
	if err := writeFileAtomic(filepath.Join(a.jobDir(j.ID), "reply-to"), []byte(strconv.FormatInt(last, 10)+"\n")); err != nil {
		a.logf("Telegram：对话任务 %s 记不下回复哪条消息（回答照发，只是不挂在那条下面）：%v", j.ID, err)
	}
	a.logf("Telegram：启动对话任务 %s 处理主人的 %d 条消息", j.ID, len(s.Inbox))
	s.Inbox = nil
}

// chatRefusal：现在不能启动对话任务的原因（用量冷却中、额度 ≥98%），能启动返回空。
func (a *App) chatRefusal(st *State, now time.Time) string {
	if now.Before(st.CooldownUntil) {
		return "用量冷却中，到 " + whenCN(st.CooldownUntil)
	}
	if lvl, until, why := st.Usage.level(now, a.Cfg.Model); lvl >= usageRefuse {
		return usageGateNote(lvl, until, why)
	}
	return ""
}

// moveChatFiles 把收件箱里的附件从 ~/.agentq/inbox/ 挪进对话任务目录的 files/，返回换好路径的消息。挪不动的保留原路径。
func (a *App) moveChatFiles(id string, inbox []InMsg) []InMsg {
	out := make([]InMsg, len(inbox))
	for i, m := range inbox {
		out[i] = m
		out[i].Files = nil
		for _, f := range m.Files {
			dest := filepath.Join(a.chatFilesDir(id), filepath.Base(f))
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err == nil {
				if err = os.Rename(f, dest); err == nil {
					f = dest
				}
			}
			out[i].Files = append(out[i].Files, f)
		}
	}
	return out
}

// startChatJob 经任务外壳启动对话任务（同巡逻分析：记在 state.json，不占并发名额）。id 是 serveInbox 建好的任务目录名。
func (a *App) startChatJob(st *State, id, prompt string) (*Job, error) {
	if ok, why := a.checkLogin(); !ok {
		return nil, errors.New(why)
	}
	dir := a.projectsRoot()
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		dir = a.Home
	}
	j := &Job{ID: id, Kind: kindChat, Project: chatJobName, ProjectDir: dir, Item: "agentq telegram", Title: "agentq telegram 对话"}
	return a.start(st, j, prompt, "")
}

const chatPromptHead = "你由 agentq 在无人值守模式下用 claude -p 启动，替主人处理他在 Telegram 上发来的消息（agentq 已核对是主人本人的 chat id）。" +
	"回合一结束进程就退出：一切等待都在前台做完，不用后台命令、Monitor 或「等通知」。\n\n" +
	"现在是 %s。回答里的日期和星期照抄 agentq 输出里的写法（如「10-13 周二 21:00」，星期是程序算好的）；" +
	"要写别的日期是星期几，用 `date -j -f %%F <YYYY-MM-DD> +%%u`（1 是周一，7 是周日）查，不自己推算。\n\n" +
	"按主人的全局规则和技能理解他要什么，用自然语言理解，不要求固定命令：\n" +
	"- 派活（如「某项目：加个按月对比」）：按 dispatch 技能写进那个项目的 docs/BACKLOG.md 并执行 agentq run，不等结果；回复已派出（任务 id）和做完去哪看。\n" +
	"- 拍板（回复了某条「要你定」，或说了某条选哪个）：按 dispatch 技能把决定写回那个项目「等主人拍板」里对应的条目、移回「待做」顶部（保留分支名）并派出。\n" +
	"- 问状态或问问题：查完简短回答（agentq status、agentq logs、各项目 BACKLOG 和 PROGRESS、~/.claude/nightly 里的汇总和早报）。\n" +
	"- 小事当场办：查个数据、改句配置这类合理又能撤回的直接做；付款、对外发布、删数据这类必须等主人的不做，说明原因。\n" +
	"判断不出是哪个项目时，列出 ~/projects 里的项目问一句。\n\n" +
	"附件：主人发来的截图、录屏、文件已经下载好，路径写在那条消息下面。图片用 Read 直接看；视频有 ffmpeg 就抽几帧看，" +
	"没有就用 `qlmanage -t -s 1600 -o <目录> <视频>` 取一帧看；派活时把附件路径原样写进 BACKLOG 条目，做的人才看得到。\n" +
	"要给主人看图片或录屏（界面截图、效果图、录屏）：执行 `agentq notify file <绝对路径> [一句说明]`，马上发出、挂在主人的消息下面" +
	"（图片按图片发，录屏按视频发，太大会先压缩，压不下就改发文件路径）；最终回答里提一句发了什么。\n\n" +
	"你的最终回答会原样作为一条 Telegram 消息（HTML parse_mode）发给主人，只写消息本身，不要别的话：\n" +
	"- 第一行 `<表情> <b>类别 · %s</b>`：派活、拍板用「🟢 已派出」，回答问题用「📊 状态」，出了问题或做不了用「🔴 要你知道」；\n" +
	"- 空一行，每段开头一个表情当标记，段内不再加；项目名后用全角空格「　」再写内容；要主人做的放最前；\n" +
	"- 最后一行 `<i>%s · agentq 对话</i>`；\n" +
	"- 不用感叹号、不客套，不超过 12 行；只用 <b> <i> <code> 标签，正文里的 < > & 写成 &lt; &gt; &amp;。\n\n" +
	"主人的消息（按时间先后）：\n"

func chatPrompt(inbox []InMsg, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, chatPromptHead, whenCN(now), dateCN(now), now.Format("15:04"))
	for i, m := range inbox {
		fmt.Fprintf(&b, "%d. [%s] %s\n", i+1, whenCN(m.At), m.Text)
		if m.ReplyTo != "" {
			fmt.Fprintf(&b, "   （这条是回复 agentq 之前发的这条消息：%s）\n", oneLine(m.ReplyTo, 600))
		}
		for _, f := range m.Files {
			fmt.Fprintf(&b, "   附件：%s\n", f)
		}
	}
	return b.String()
}

// replyChat：对话任务结束时把它的最终回答回发给主人（作为对最后一条消息的回复）。没做成就回原因。
// 不进推送队列：主人刚问的，不受合并、夜间和每天上限的限制。
func (a *App) replyChat(j *Job) {
	c, err := a.telegramCreds()
	if c == nil {
		if err != nil {
			a.logf("Telegram：对话任务 %s 的回答发不出去：%v", j.ID, err)
		}
		return
	}
	text := ""
	if j.Status == statusDone {
		if res, err := readResult(filepath.Join(a.jobDir(j.ID), "result.json")); err == nil {
			text = strings.TrimSpace(res.Result)
		}
	}
	if text == "" {
		text = a.renderMessage("alert", a.Now(), []msgSection{{Emoji: "🔴", Head: "这条没答上来：" + statusLabel(j.Status),
			Lines: []string{oneLine(j.Summary, 200), "详情：agentq logs " + j.ID}}}, "agentq 对话")
	}
	if r := []rune(text); len(r) > tgMaxRunes {
		path := a.saveFullText(a.Now(), "reply", text)
		text = htmlToPlain(string(r[:tgMaxRunes-200])) + "\n…（太长了，全文 " + path + "）"
		text = esc(text)
	}
	replyTo := a.chatReplyTo(j)
	if err := a.tgSend(c, text, replyTo); err != nil {
		a.logf("Telegram：对话任务 %s 的回答发送失败，下次 tick 重发：%v", j.ID, c.redact(err.Error()))
		r := PendingReply{Text: text, ReplyTo: replyTo, At: a.Now(), JobID: j.ID}
		if a.notifyHeld != nil {
			a.notifyHeld.Replies = append(a.notifyHeld.Replies, r)
		} else if err := a.withNotify(func(s *NotifyState) error { s.Replies = append(s.Replies, r); return nil }); err != nil {
			a.logf("Telegram：对话任务 %s 的回答也没能存下来重发：%v", j.ID, err)
		}
		return
	}
	a.logf("Telegram：已把对话任务 %s 的回答发给主人", j.ID)
}

// replyKeep：没发出去的回答最多重发多久，过了就放弃（只记日志）。
const replyKeep = 24 * time.Hour

// retryReplies：重发之前没发出去的对话回答（调用方拿着 notify.lock）。
func (a *App) retryReplies(s *NotifyState, c *tgCreds) {
	kept := s.Replies[:0]
	for _, r := range s.Replies {
		if a.Now().Sub(r.At) > replyKeep {
			a.logf("Telegram：对话任务 %s 的回答 %s 起一直没发出去，放弃（原文 agentq logs %s）", r.JobID, formatDuration(replyKeep), r.JobID)
			continue
		}
		if err := a.tgSend(c, r.Text, r.ReplyTo); err != nil {
			a.notifyErr(s, "重发对话回答失败（下次 tick 再试）："+err.Error())
			kept = append(kept, r)
			continue
		}
		a.logf("Telegram：补发了对话任务 %s 的回答", r.JobID)
	}
	s.Replies = kept
}

// chatReplyTo：对话任务回复的是哪条消息（启动时写在任务目录的 reply-to 里），读不到就不挂回复。
func (a *App) chatReplyTo(j *Job) int64 {
	b, err := os.ReadFile(filepath.Join(a.jobDir(j.ID), "reply-to"))
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return n
}

// statusDigest：「状态」的回答，按推送格式压成十来行。
func (a *App) statusDigest(st *State) string {
	now := a.Now()
	var secs []msgSection
	all, _ := scanDecisions(a.projectsRoot())
	if decs := askable(all); len(decs) > 0 {
		sec := msgSection{Emoji: "🔴", Head: fmt.Sprintf("要你定　%d 条", len(decs))}
		for i, d := range decs {
			if i == 2 {
				break
			}
			sec.Lines = append(sec.Lines, d.Project+"　"+truncRunes(d.Title, 30))
		}
		secs = append(secs, sec)
	}
	ulvl, uuntil, uwhy := st.Usage.level(now, a.Cfg.Model)
	secs = append(secs, msgSection{Emoji: "⚙️", Head: "调度　" + a.schedNote(st, nil, now, ulvl, uuntil, uwhy),
		Lines: []string{"额度　" + st.Usage.describe(now, a.Cfg.Model)}})
	running := st.tasks()
	sec := msgSection{Emoji: "🔧", Head: fmt.Sprintf("在做　%d/%d", len(running), a.Cfg.MaxParallel)}
	for _, j := range running {
		sec.Lines = append(sec.Lines, fmt.Sprintf("%s　%s（%s）", j.Project, truncRunes(j.Title, 24), formatIdle(now.Sub(j.StartedAt))))
	}
	secs = append(secs, sec)
	if projects, err := scanProjects(a.projectsRoot()); err == nil {
		var todo []string
		for _, p := range projects {
			if items, err := p.items(); err == nil && len(items) > 0 {
				todo = append(todo, fmt.Sprintf("%s %d", p.Name, len(items)))
			}
		}
		if len(todo) > 0 {
			secs = append(secs, msgSection{Emoji: "📋", Head: "待做　" + strings.Join(todo, "，")})
		}
	}
	var ended []*Job
	for _, j := range st.Jobs {
		if j.Kind == "" && j.Status != statusRunning && j.EndedAt != nil {
			ended = append(ended, j)
		}
	}
	if len(ended) > 0 {
		sortJobsByEnd(ended)
		sec := msgSection{Emoji: "🟢", Head: "最近结束"}
		for i, j := range ended {
			if i == 2 {
				break
			}
			sec.Lines = append(sec.Lines, fmt.Sprintf("%s　%s %s", j.Project, truncRunes(j.Title, 24), statusLabel(j.Status)))
		}
		secs = append(secs, sec)
	}
	return a.renderMessage("status", now, secs, "agentq status")
}
