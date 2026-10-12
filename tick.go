package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

type App struct {
	Home string
	Cfg  Config
	Now  func() time.Time
	Out  io.Writer
	// Launchctl 为空时执行 /bin/launchctl；测试里替换掉。
	Launchctl func(args ...string) ([]byte, error)
	// UsageText 为空时执行 claude -p /usage 读额度；测试里替换掉。
	UsageText func() (string, error)
	// DiskFree 为空时用 statfs 读磁盘剩余；AnalyzePatrol 为空时经任务外壳启动巡逻分析。测试里替换掉。
	DiskFree      func(path string) (free, total uint64, err error)
	AnalyzePatrol func(st *State, prompt string) (*Job, error)
	// MacNotify 为空时用 osascript 弹本机通知；StartChat 为空时经任务外壳启动 Telegram 对话任务。测试里替换掉。
	MacNotify   func(title, body string) error
	StartChat   func(st *State, id, prompt string) (*Job, error)
	TelegramAPI string
	// Compress 为空时用 ffmpeg（没有就用系统自带的 avconvert）压缩太大的录屏；测试里替换掉。
	Compress func(src, dir string) (string, error)
	// notifyHeld：本进程正拿着 notify.lock 时的推送状态（见 withNotify）。
	notifyHeld *NotifyState
	// filesOut：推送已发出、附件等放开 notify.lock 再发（见 queueNoticeFiles）。
	filesOut []string
	// 本次开机时间（秒），第一次用到时读，0 = 读不出。
	boot     int64
	bootRead bool
}

func (a *App) dataDir() string         { return filepath.Join(a.Home, ".agentq") }
func (a *App) configPath() string      { return filepath.Join(a.dataDir(), "config.json") }
func (a *App) statePath() string       { return filepath.Join(a.dataDir(), "state.json") }
func (a *App) lockPath() string        { return filepath.Join(a.dataDir(), "tick.lock") }
func (a *App) jobDir(id string) string { return filepath.Join(a.dataDir(), "jobs", id) }
func (a *App) projectsRoot() string    { return expandHome(a.Home, a.Cfg.ProjectsRoot) }
func (a *App) claudePath() string      { return expandHome(a.Home, a.Cfg.ClaudePath) }
func (a *App) nightlyPath(t time.Time) string {
	return filepath.Join(a.Home, ".claude", "nightly", t.Format("2006-01-02")+"-agentq.md")
}

func newApp(home string, out io.Writer) (*App, error) {
	a := &App{Home: home, Now: time.Now, Out: out}
	// 只给集成测试用：AGENTQ_TEST_NOW（RFC3339）把这个进程的「现在」拨到那一刻，之后照常走（测补跑窗口等按钟点判断的逻辑）
	if v := os.Getenv("AGENTQ_TEST_NOW"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			t0 := time.Now()
			a.Now = func() time.Time { return t.In(time.Local).Add(time.Since(t0)) }
		}
	}
	if err := os.MkdirAll(a.dataDir(), 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(a.dataDir(), 0o700); err != nil {
		return nil, err
	}
	cfg, err := loadConfig(a.configPath())
	if err != nil {
		return nil, err
	}
	a.Cfg = cfg
	return a, nil
}

func (a *App) logf(format string, args ...any) {
	fmt.Fprintf(a.Out, "%s %s\n", a.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
}

// errLogged 表示错误已经写进日志，main 只需要以非零状态退出。
var errLogged = errors.New("已记录")

// tick：①收尾 ②超时处理 ③补跑今天错过的夜班、复盘（见 routine.go）④挑活启动 ⑤离上次巡逻满一小时就巡逻一次（见 patrol.go）⑥推送和收 Telegram 消息（见 notify.go）。
// after 非空表示是任务外壳在该任务结束后触发的：拿不到锁时等一会儿（最多 cmdLockWait）而不是跳过，
// 否则正在跑的那次 tick 已经收尾过、没看到这个任务结束，空出的位置要等下一次定时 tick。
func (a *App) tick(after string) error {
	if after != "" && !a.Cfg.TickAfterJob {
		return nil
	}
	name, wait := "tick", time.Duration(0)
	if after != "" {
		name, wait = "tick（任务 "+after+" 结束后）", cmdLockWait
	}
	lock, err := acquireLock(a.lockPath(), wait)
	if err != nil {
		a.logf("%s：拿不到 %s：%v", name, a.lockPath(), err)
		return errLogged
	}
	if lock == nil {
		if after != "" {
			a.logf("%s：等了 %s 还拿不到 %s，本次跳过，等下次定时 tick", name, cmdLockWait, a.lockPath())
		} else {
			a.logf("%s：上一个 tick 还没结束，本次跳过", name)
		}
		return nil
	}
	defer lock.Close()

	st, err := loadState(a.statePath())
	recovered := false
	if errors.Is(err, errStateCorrupt) {
		st, err = a.recoverState(err)
		recovered = err == nil
	}
	if err != nil {
		a.logf("%s：状态文件读不出，当作未知，本次不收尾也不启动：%v", name, err)
		return errLogged
	}
	finished := a.reap(st)
	finished += a.enforceTimeouts(st)
	a.checkStalls(st)
	st.prune(a.Now())
	if err := saveState(a.statePath(), st); err != nil {
		a.logf("%s：写状态文件失败，本次不启动：%v", name, err)
		return errLogged
	}

	if !recovered {
		a.catchUpRoutines(st)
	}
	started, note := 0, ""
	now := a.Now()
	switch {
	case recovered:
		note = "状态文件损坏后按 meta.json 重建，本次只收尾不启动"
	case a.Cfg.Paused:
		note = "已暂停（paused=true），只收尾不启动"
	case now.Before(st.CooldownUntil):
		note = "用量冷却中，到 " + whenCN(st.CooldownUntil) + " 前不启动"
	case now.Before(st.BreakerUntil):
		note = "连环失败熔断中，到 " + whenCN(st.BreakerUntil) + " 前不启动"
	default:
		started, note = a.schedule(st)
		// 续跑会改被续跑任务的记录（ResumedBy、ResumePending），启动时只存了一次
		if err := saveState(a.statePath(), st); err != nil {
			a.logf("%s：写状态文件失败：%v", name, err)
		}
	}
	msg := fmt.Sprintf("%s：运行中 %d/%d，本次收尾 %d，启动 %d", name, len(st.tasks()), a.Cfg.MaxParallel, finished, started)
	if note != "" {
		msg += "；" + note
	}
	a.logf("%s", msg)
	a.patrolIfDue(st)
	a.notifyTick(st)
	return nil
}

// recoverState 在 state.json 内容损坏时调用：先把坏文件备份成 state.json.bad-<时间>，再按 jobs 目录下各任务的
// meta.json 重建任务列表（运行中的任务照常收尾和超时，进程在不在仍靠 ps 核对命令行里的 agentq 标记），
// 写日志和当天汇总报警。用量冷却只能按撞上限任务的结束时间 + limit_cooldown 估回来，熔断记录丢失。
// 重建的状态由调用方照常保存；保存失败时坏文件还在，下次 tick 再备份重建一次。备份不了就返回错误，当作未知。
func (a *App) recoverState(cause error) (*State, error) {
	raw, err := os.ReadFile(a.statePath())
	if err != nil {
		return nil, fmt.Errorf("%v；备份时读不出：%w", cause, err)
	}
	bak := a.statePath() + ".bad-" + a.Now().Format("20060102-150405")
	if err := writeFileAtomic(bak, raw); err != nil {
		return nil, fmt.Errorf("%v；备份到 %s 失败：%w", cause, bak, err)
	}
	st := &State{}
	metas, _ := filepath.Glob(filepath.Join(a.dataDir(), "jobs", "*", "meta.json"))
	var bad []string
	for _, m := range metas {
		b, err := os.ReadFile(m)
		var j Job
		if err == nil {
			err = json.Unmarshal(b, &j)
		}
		if err != nil || j.ID != filepath.Base(filepath.Dir(m)) || j.Status == "" {
			bad = append(bad, filepath.Base(filepath.Dir(m)))
			continue
		}
		st.Jobs = append(st.Jobs, &j)
		if j.LimitHit && j.EndedAt != nil {
			if until := j.EndedAt.Add(a.Cfg.LimitCooldown.Duration); until.After(st.CooldownUntil) {
				st.CooldownUntil = until
			}
		}
	}
	sort.SliceStable(st.Jobs, func(i, k int) bool { return st.Jobs[i].StartedAt.Before(st.Jobs[k].StartedAt) })
	msg := fmt.Sprintf("状态文件损坏：%v；已备份为 %s，按 %d 个任务的 meta.json 重建（运行中 %d 个，照常收尾和超时）",
		cause, bak, len(st.Jobs), len(st.running()))
	if len(bad) > 0 {
		msg += fmt.Sprintf("；%d 个任务的 meta.json 读不出，没算进去：%s", len(bad), strings.Join(bad, "、"))
	}
	msg += "；熔断记录丢了，用量冷却按撞上限任务的结束时间估；本次 tick 不启动新任务"
	a.logf("%s", msg)
	a.appendNightlyLine(a.Now(), msg)
	return st, nil
}

// reap 收尾进程已经不在的 running 任务，返回收尾个数。启动后机器重启过的任务不清进程组，记中断（见 finishRebooted）。
func (a *App) reap(st *State) int {
	n := 0
	for _, j := range st.running() {
		alive := procAlive(j.PID, nameMarker(j.Project))
		if a.bootChanged(j) {
			if alive {
				// 进程还在就不可能重启过，是校时让 kern.boottime 跟着变了：记下新的开机时间，照常看着
				a.logf("任务 %s 的开机时间记录（%d）和现在（%d）不一致，但进程还在，按校时处理", j.ID, j.BootTime, a.curBoot())
				j.BootTime = a.curBoot()
				a.writeMeta(j) // 状态文件坏了要按 meta.json 重建，别让重建后又被当成重启过
				continue
			}
			a.finishRebooted(st, j)
			n++
			continue
		}
		if alive {
			continue
		}
		a.finishFromResult(st, j)
		a.cleanupGroup(j)
		n++
	}
	return n
}

// bootSlack：开机时间相差不超过这么多不算重启（系统校时会让 kern.boottime 变几秒）。
const bootSlack = 60

// curBoot 返回本次开机时间（只读一次），读不出返回 0：不判断是否重启过，清进程组照样要核对标记。
func (a *App) curBoot() int64 {
	if !a.bootRead {
		a.bootRead = true
		t, err := bootTime()
		if err != nil {
			a.logf("读开机时间失败，本次不判断任务启动后是否重启过：%v", err)
		}
		a.boot = t
	}
	return a.boot
}

// bootChanged：任务记了启动时的开机时间，且和现在的不一致（机器在任务启动后重启过）。
func (a *App) bootChanged(j *Job) bool {
	cur := a.curBoot()
	if j.BootTime == 0 || cur == 0 {
		return false
	}
	d := cur - j.BootTime
	return d > bootSlack || d < -bootSlack
}

// finishRebooted 收尾启动后机器重启过的任务：记下的 pid 和进程组号可能已经属于别的程序，不清进程组。
// claude 在重启前已经正常退出、写了结果的，照常按结果收尾；否则记中断（不计连环失败），
// BACKLOG 里这一条仍标进行中、有会话 id、本身不是续跑的，用同一会话续跑一次。
func (a *App) finishRebooted(st *State, j *Job) {
	if res, err := readResult(filepath.Join(a.jobDir(j.ID), "result.json")); err == nil && (res.Result != "" || res.SessionID != "") {
		a.finishFromResult(st, j)
		return
	}
	j.Rebooted = true
	why := "机器重启过（开机时间和启动任务时不一致），claude 已不在；没清它的进程组（组号可能已被别的程序占用）"
	if j.Kind != "" { // 夜班、复盘没有 BACKLOG 条目，不续跑
		a.finish(st, j, statusInterrupted, why)
		return
	}
	items, err := Project{Dir: j.ProjectDir}.items()
	if err != nil {
		why += "；核验 BACKLOG 失败：" + err.Error()
	}
	if it, ok := inProgressItem(items, j.Fingerprint); ok {
		why += "；BACKLOG 里这一条仍标进行中" + branchNote(j.ProjectDir, it.Text)
		switch {
		case j.ResumeOf != "":
			why += "；这次已经是续跑，不再续跑，等主人处理"
		case j.SessionID == "":
			why += "；没有会话 id，不能续跑，等主人处理"
		default:
			j.ResumePending = true
			why += "；将自动用同一会话续跑一次"
		}
	} else if err == nil {
		why += "；条目已不在进行中，不续跑，等主人核对"
	}
	a.finish(st, j, statusInterrupted, why)
}

// cleanupGroup：claude 已退出但它拉起的子进程还在同一进程组里时，发 SIGTERM 清掉。
// 平时任务外壳在 claude 退出时已经清过；走到这里多半是外壳没了，离 claude 退出可能已经很久，
// 组号可能已被别的程序占用，所以组里要有带这个任务标记的进程才杀。
func (a *App) cleanupGroup(j *Job) {
	if j.PID <= 0 || !groupAlive(j.PID) {
		return
	}
	ok, err := groupMarked(j.PID, j.ID)
	switch {
	case err != nil:
		a.logf("任务 %s 的进程组 %d 还有进程，但核对不了是不是它留下的，不清理：%v", j.ID, j.PID, err)
	case !ok:
		a.logf("任务 %s 的进程组 %d 还有进程，但都没有这个任务的标记 %s=%s（可能组号已被别的程序占用，或只剩系统自带命令），不清理", j.ID, j.PID, jobEnv, j.ID)
	default:
		_ = syscall.Kill(-j.PID, syscall.SIGTERM)
		a.logf("任务 %s 的进程组 %d 还有残留进程，已发 SIGTERM", j.ID, j.PID)
	}
}

// checkStalls：运行中的任务会话记录超过 stall_after 没增长，就记日志、在当天汇总写一行「疑似卡死」，
// 同一次卡住只报一次（记录又长起来后清掉标记）。不杀：杀由 timeout 管，主人看到了可以自己 stop。
func (a *App) checkStalls(st *State) {
	for _, j := range st.running() {
		idle, ok := a.idleFor(j)
		if !ok {
			continue
		}
		stalled := idle >= a.Cfg.StallAfter.Duration
		switch {
		case stalled && !j.StallNoted:
			j.StallNoted = true
			a.writeMeta(j)
			msg := fmt.Sprintf("疑似卡死：任务 %s（%s「%s」）会话记录 %s 没增长；不自动杀，运行满 %s 才终止，要停就 agentq stop %s",
				j.ID, j.Project, truncRunes(j.Title, 40), formatIdle(idle), formatDuration(a.jobTimeout(j)), j.ID)
			a.logf("%s", msg)
			a.appendNightlyLine(a.Now(), msg)
		case !stalled && j.StallNoted:
			j.StallNoted = false
			a.writeMeta(j)
			a.logf("任务 %s 的会话记录又在增长了，解除疑似卡死", j.ID)
		}
	}
}

var safeSessionRe = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

// idleFor 返回任务的会话记录多久没增长：取 ~/.claude/projects/*/<会话id>.jsonl 和它的子代理记录
// （<会话id>/subagents/*.jsonl）里最新的修改时间，都没有或比启动时间早（续跑接着写旧会话）就从启动时算起。
// 没有会话 id（旧版本启动的任务）时返回 false。
func (a *App) idleFor(j *Job) (time.Duration, bool) {
	if !safeSessionRe.MatchString(j.SessionID) {
		return 0, false
	}
	base := filepath.Join(a.Home, ".claude", "projects", "*", j.SessionID)
	top, _ := filepath.Glob(base + ".jsonl")
	subs, _ := filepath.Glob(filepath.Join(base, "subagents", "*.jsonl"))
	last := j.StartedAt
	for _, f := range append(top, subs...) {
		if fi, err := os.Stat(f); err == nil && fi.ModTime().After(last) {
			last = fi.ModTime()
		}
	}
	return a.Now().Sub(last), true
}

func formatIdle(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	}
	return fmt.Sprintf("%d 分钟", int(d.Minutes()))
}

type claudeResult struct {
	IsError   bool   `json:"is_error"`
	Result    string `json:"result"`
	SessionID string `json:"session_id"`
	// 最后一个回合是后台通知（task-notification）触发的时候才有；正常任务 result_index 为 0、没有 origin。
	Origin struct {
		Kind string `json:"kind"`
	} `json:"origin"`
	ResultIndex int `json:"result_index"`
	NumTurns    int `json:"num_turns"`
	// 正常结束时是 null；API 报错结束时才有值（具体类型没见过，只看有没有）。
	APIErrorStatus json.RawMessage `json:"api_error_status"`
}

// limitScanTurns：回合数不超过这个、且不是后台通知回合的结果，才可能是一开始就撞了上限。
const limitScanTurns = 3

// mayBeLimit：只有报错结束、API 报了错、或回合数很少时才扫用量上限字样。正常做了很多回合的任务，
// 结果里提到 rate limit（比如修的就是限流代码）不算撞上限。
func (r claudeResult) mayBeLimit() bool {
	api := strings.TrimSpace(string(r.APIErrorStatus))
	return r.IsError || (api != "" && api != "null") || (r.NumTurns <= limitScanTurns && r.ResultIndex == 0)
}

// finishFromResult 读 result.json 定结果：is_error 为假 → done，但核验出没做完 → interrupted；为真或读不出 → failed。
// 可能撞了上限（见 mayBeLimit）且结果文本里有用量上限字样 → 设冷却（能解析出重置时间就冷却到那时），这次不算「试过」；
// 撞上限时 BACKLOG 里这一条仍标进行中（做到一半撞上的）→ 记中断，冷却过后用同一会话续跑。
func (a *App) finishFromResult(st *State, j *Job) {
	dir := a.jobDir(j.ID)
	raw, rerr := os.ReadFile(filepath.Join(dir, "result.json"))
	var res claudeResult
	perr := json.Unmarshal(raw, &res)
	status, summary, scan := statusFailed, "", ""
	if rerr == nil && perr == nil {
		if res.SessionID != "" {
			j.SessionID = res.SessionID
		}
		summary = summaryParagraph(res.Result)
		if j.Kind == kindPatrol || j.Kind == kindChat { // 分析结论、对话回答整段都要，不按合并情况挑段落
			summary = oneLine(res.Result, 600)
		}
		if res.mayBeLimit() {
			scan = res.Result
		}
		if !res.IsError {
			status = statusDone
		}
	} else {
		tail := tailLines(filepath.Join(dir, "stderr.log"), 5)
		summary = "没有可读的结果（result.json 为空或格式不对）"
		if t := strings.TrimSpace(tail); t != "" {
			summary += "；stderr 末尾：" + oneLine(t, 200)
		}
		scan = string(raw) + "\n" + tail
	}
	if isUsageLimit(scan) {
		j.LimitHit = true
		until, how := a.Now().Add(a.Cfg.LimitCooldown.Duration), ""
		// 以 claude 退出（打出提示）的时刻为参照解析，收尾晚了、跨过午夜也不会推到下一天/下一周
		if t, ok := parseResetTime(scan, a.exitedAt(j)); ok && t.After(a.Now()) {
			until, how = t, "（按提示的重置时间）"
		}
		if until.After(st.CooldownUntil) {
			st.CooldownUntil = until
		}
		summary = "撞到用量上限，暂停启动到 " + whenCN(st.CooldownUntil) + how + "；" + summary
		a.logf("任务 %s 撞到用量上限，冷却到 %s%s", j.ID, st.CooldownUntil.Format("2006-01-02 15:04"), how)
		var items []Item
		var err error
		if j.Kind == "" { // 夜班、复盘没有 BACKLOG 条目，不续跑
			items, err = Project{Dir: j.ProjectDir}.items()
		}
		if err != nil {
			summary += "；核验 BACKLOG 失败：" + err.Error()
		}
		if it, ok := inProgressItem(items, j.Fingerprint); ok {
			status = statusInterrupted
			summary += "；BACKLOG 里这一条仍标进行中" + branchNote(j.ProjectDir, it.Text)
			switch {
			case j.SessionID == "":
				summary += "；没有会话 id，不能续跑，等主人处理"
			case resumeDepth(st, j) >= maxLimitResumes:
				summary += fmt.Sprintf("；已经连续续跑 %d 次，不再续跑，等主人处理", maxLimitResumes)
			default:
				j.ResumePending = true
				summary += "；冷却过后用同一会话续跑"
			}
		}
	}
	if summary == "" && status == statusFailed {
		summary = "claude 报错结束"
	}
	if _, routine := routines[j.Kind]; status == statusDone && !j.LimitHit && routine {
		if p := a.missingReport(j); p != "" {
			status = statusInterrupted
			summary = "正常退出但没写出报告 " + p + "，可能中途退出了（不续跑，等主人看 agentq logs " + j.ID + "）；结果最后：" + summary
		}
	}
	if status == statusDone && !j.LimitHit && j.Kind == "" {
		if bad, resumable, why := a.checkInterrupted(j, res); bad {
			status = statusInterrupted
			switch {
			case j.ResumeOf != "":
				why = "续跑后仍中断，等主人处理：" + why
			case !resumable:
				why += "；条目已不在进行中，不续跑，等主人核对"
			case j.SessionID == "":
				why += "；没有会话 id，不能续跑，等主人处理"
			default:
				j.ResumePending = true
				why += "；将自动用同一会话续跑一次"
			}
			if summary != "" {
				why += "；结果最后：" + summary
			}
			summary = why
		}
	}
	if status == statusDone && j.Kind == "" {
		summary = withDoneCommit(j, summary)
	}
	a.finish(st, j, status, summary)
}

// withDoneCommit：BACKLOG「已完成」里同标题的条目写了「合并提交 <号>」时，把它放在摘要开头
// （status 只显示摘要前 80 个字，前 80 个字里已经有这个号就不重复）。
// 「待做」里还有同标题的条目（比如标了卡住没合并）就不取，免得带出以前同名条目的旧号。
func withDoneCommit(j *Job, summary string) string {
	b, err := os.ReadFile(Project{Dir: j.ProjectDir}.backlogPath())
	if err != nil {
		return summary
	}
	for _, it := range parseBacklog(string(b)) {
		if it.Title == j.Title {
			return summary
		}
	}
	c := doneMergeCommit(string(b), j.Title)
	if c == "" || strings.Contains(oneLine(summary, 80), c) {
		return summary
	}
	return "合并提交 " + c + "；" + summary
}

// maxLimitResumes：同一条做到一半撞上限后最多接连续跑几次（每次之间都有冷却），防止一直报上限时无限续跑。
const maxLimitResumes = 3

// resumeDepth：j 是第几次续跑（顺着 ResumeOf 往回数，原任务为 0）。
func resumeDepth(st *State, j *Job) int {
	n := 0
	for j.ResumeOf != "" && n <= maxLimitResumes {
		prev, err := st.find(j.ResumeOf)
		if err != nil {
			break
		}
		j = prev
		n++
	}
	return n
}

// mergeRe：结果里说到合并情况的字样（prompt 要求最后一段写「合并了没有」）。
var mergeRe = regexp.MustCompile(`(?i)合并|合入|没合|未合|merge`)

// checkInterrupted 核验 is_error 为假的结束是不是真的做完了，返回是否中断、能否续跑、写进摘要的原因。
// BACKLOG「待做」里同一条仍标进行中 → 中断，可以续跑（agent 认领后没走到合并后更新 BACKLOG 那一步）。
// 条目不在进行中但结果里没写合并情况 → 也按中断报，不续跑（不知道做到哪了，交给主人核对）。
func (a *App) checkInterrupted(j *Job, res claudeResult) (bool, bool, string) {
	var why []string
	inProgress := false
	items, err := Project{Dir: j.ProjectDir}.items()
	if err != nil {
		why = append(why, "核验 BACKLOG 失败："+err.Error())
	}
	if it, ok := inProgressItem(items, j.Fingerprint); ok {
		inProgress = true
		why = append(why, "BACKLOG 里这一条仍标进行中"+branchNote(j.ProjectDir, it.Text))
	}
	if !inProgress {
		if mergeRe.MatchString(res.Result) {
			return false, false, ""
		}
		why = append(why, "结果里没写合并情况")
	}
	if res.Origin.Kind == "task-notification" || res.ResultIndex > 0 {
		why = append(why, "最后一个回合由后台通知触发")
	}
	return true, inProgress, strings.Join(why, "；")
}

var branchRe = regexp.MustCompile(`（进行中[：:]\s*([^\s）]+)`)

// branchNote 从进行中标记里取分支名，用 git 看它有没有合进 main，返回「（分支 …）」说明；取不到就返回空。
func branchNote(dir, text string) string {
	m := branchRe.FindStringSubmatch(text)
	if m == nil || strings.HasPrefix(m[1], "-") {
		return ""
	}
	b := m[1]
	git := func(args ...string) (string, error) {
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
		return strings.TrimSpace(string(out)), err
	}
	if _, err := git("rev-parse", "--verify", "--quiet", "refs/heads/"+b); err != nil {
		return "（分支 " + b + " 不存在）"
	}
	n, err := git("rev-list", "--count", "main.."+b)
	switch {
	case err != nil:
		return "（分支 " + b + "）"
	case n == "0":
		return "（分支 " + b + " 没有 main 之外的提交）"
	}
	return "（分支 " + b + " 比 main 多 " + n + " 个提交，没合并）"
}

// enforceTimeouts 杀掉运行超过超时（待做任务 timeout，夜班、复盘各自的 night_timeout、review_timeout）的任务进程组。
func (a *App) enforceTimeouts(st *State) int {
	n := 0
	for _, j := range st.running() {
		limit := a.jobTimeout(j)
		if a.Now().Sub(j.StartedAt) < limit {
			continue
		}
		if !procAlive(j.PID, nameMarker(j.Project)) {
			a.finishFromResult(st, j)
			n++
			continue
		}
		hard := killGroup(j.PID, killGrace)
		how := "SIGTERM"
		if hard {
			how = "SIGTERM 后 SIGKILL"
		}
		a.logf("任务 %s 运行超过 %s，已对进程组 %d 发 %s", j.ID, formatDuration(limit), j.PID, how)
		a.finish(st, j, statusTimeout, "运行超过 "+formatDuration(limit)+"，已终止（"+how+"）")
		n++
	}
	return n
}

// 方案列的三种字样，加上 CLI 现行文案 You've hit your limit、You've hit your monthly spend limit。
var limitRe = regexp.MustCompile(`(?i)usage limit|rate limit|limit reached|hit your (\w+ ){0,3}limit`)

func isUsageLimit(s string) bool { return limitRe.MatchString(s) }

// schedule 按项目名字母序挑活，每个项目每次最多启动 1 个。返回启动个数和备注。
func (a *App) schedule(st *State) (int, string) {
	if ok, why := capacity(st, a.Cfg, ""); !ok {
		return 0, why
	}
	projects, err := scanProjects(a.projectsRoot())
	if err != nil {
		return 0, "扫描项目目录失败：" + err.Error()
	}
	started := 0
	authChecked, usageBlocked := false, false
	var notes []string
	// ready：真要启动时才查登录和额度（额度读一次缓存 usageCacheFor）。额度 ≥usageStopAt 就不再启动；读不出当未知照常启动。
	ready := func() bool {
		if !authChecked {
			if ok, why := a.checkLogin(); !ok {
				notes = append(notes, why)
				return false
			}
			authChecked = true
			switch lvl, until, why := a.usage(st).level(a.Now(), a.Cfg.Model); {
			case lvl >= usageStop:
				usageBlocked = true
				notes = append(notes, usageGateNote(lvl, until, why))
			case lvl == usageUnknown:
				notes = append(notes, "额度未知，照常启动："+oneLine(st.Usage.Err, 120))
			}
		}
		return !usageBlocked
	}
	// 先续跑中断的任务（它们占着 BACKLOG 的进行中标记，不续跑就一直没人管），续跑过的项目这次不再挑新条目。
	resumed := map[string]bool{}
	// 这次 tick 里刚启动就失败的任务可能触发熔断，熔断了就不再启动
	tripped := false
	breaker := func() bool {
		if !tripped && a.Now().Before(st.BreakerUntil) {
			tripped = true
			notes = append(notes, "连环失败熔断，到 "+whenCN(st.BreakerUntil)+" 前不启动")
		}
		return tripped
	}
	for _, orig := range st.pendingResumes() {
		if ok, _ := capacity(st, a.Cfg, ""); !ok || breaker() {
			break
		}
		if ok, _ := capacity(st, a.Cfg, orig.Project); !ok || resumed[orig.Project] {
			continue
		}
		items, err := Project{Dir: orig.ProjectDir}.items()
		if err != nil {
			notes = append(notes, orig.Project+" 的 BACKLOG 读不出，暂不续跑："+err.Error())
			continue
		}
		if _, ok := inProgressItem(items, orig.Fingerprint); !ok || st.runningFor(orig.Fingerprint) != nil {
			orig.ResumePending = false
			a.logf("任务 %s 的条目已不在进行中或已有任务在做，不再续跑", orig.ID)
			continue
		}
		if !ready() {
			return started, strings.Join(notes, "；")
		}
		j, err := a.launchResume(st, orig)
		resumed[orig.Project] = true
		if err != nil {
			notes = append(notes, orig.Project+" 续跑失败："+err.Error())
			continue
		}
		a.logf("续跑任务 %s（接 %s，会话 %s）：%s「%s」pid %d", j.ID, orig.ID, orig.SessionID, j.Project, j.Title, j.PID)
		started++
	}
	for _, p := range projects {
		if ok, _ := capacity(st, a.Cfg, ""); !ok || breaker() {
			break
		}
		if ok, _ := capacity(st, a.Cfg, p.Name); !ok || resumed[p.Name] {
			continue
		}
		items, err := p.items()
		if err != nil {
			notes = append(notes, p.Name+" 的 BACKLOG 读不出："+err.Error())
			continue
		}
		it, ok := a.pick(st, items, "", true)
		if !ok {
			continue
		}
		if !ready() {
			break
		}
		j, err := a.launch(st, p, it)
		if err != nil {
			notes = append(notes, p.Name+" 启动失败："+err.Error())
			continue
		}
		a.logf("启动任务 %s：%s「%s」pid %d", j.ID, p.Name, j.Title, j.PID)
		started++
	}
	return started, strings.Join(notes, "；")
}

// inProgressItem 找「待做」里指纹为 fp、标着进行中的条目。同时标了卡住的不算：那是 agent 有意放下的，不是中断。
func inProgressItem(items []Item, fp string) (Item, bool) {
	for _, it := range items {
		if it.Fingerprint == fp && it.InProgress && !it.Stuck {
			return it, true
		}
	}
	return Item{}, false
}

// pick 取第一条可做的条目：没标进行中/卡住、没有同指纹任务在跑、（honorRetry 时）不在 retry_after 内、包含 keyword。
func (a *App) pick(st *State, items []Item, keyword string, honorRetry bool) (Item, bool) {
	for _, it := range items {
		if !it.Eligible() || st.runningFor(it.Fingerprint) != nil {
			continue
		}
		if keyword != "" && !strings.Contains(it.Text, keyword) {
			continue
		}
		if honorRetry && retryBlocked(st, it.Fingerprint, a.Now(), a.Cfg.RetryAfter.Duration) {
			continue
		}
		return it, true
	}
	return Item{}, false
}

func (a *App) checkLogin() (bool, string) {
	ok, err := claudeLoggedIn(a.claudePath())
	if err != nil {
		return false, "确认终端版 Claude 登录状态失败，不启动：" + err.Error()
	}
	if !ok {
		return false, "终端版 Claude 未登录（claude auth status 显示 loggedIn=false），不启动"
	}
	return true, ""
}

const promptTemplate = "你在无人值守模式下工作，主人不在场。你已经是调度器启动的执行者，亲自完成当前待办；不通过 dispatch 或 agentq run 把同一任务派回队列。条目中的入口会话不实现、不等待等描述不改变本进程的执行角色。先完整读 ~/.claude/skills/agent-task/SKILL.md 并严格照做（包括独立审查），把 %s 的 docs/BACKLOG.md「待做」里的这一条做到合并：%s。条目写了前置条件且不满足时，不认领，直接结束并说明。结束时最后一段写：合并了没有、合并提交号、没合的原因、主人怎么验收。"

func buildPrompt(projectDir, item string) string {
	return fmt.Sprintf(promptTemplate, projectDir, item)
}

const resumePromptTemplate = "你仍是当前待办的执行者，亲自接着完成，不通过 dispatch 或 agentq run 重新派发同一任务。上次的进程在等后台任务或通知时退出了，后台的活（子代理、Monitor、后台命令）已经不在了，不要再等它们。先看 git 状态、worktree 和分支现状、%s 的 docs/BACKLOG.md 里这一条的标记，从实际进度接着按 ~/.claude/skills/agent-task/SKILL.md 把这一条做到合并：%s。一切等待都在前台等完。结束时最后一段写：合并了没有、合并提交号、没合的原因、主人怎么验收。"

func buildResumePrompt(projectDir, item string) string {
	return fmt.Sprintf(resumePromptTemplate, projectDir, item)
}

func (a *App) newJobID(project string) (string, error) {
	if err := os.MkdirAll(filepath.Join(a.dataDir(), "jobs"), 0o755); err != nil {
		return "", err
	}
	base := a.Now().Format("0102-150405") + "-" + project
	id := base
	for i := 2; ; i++ {
		err := os.Mkdir(a.jobDir(id), 0o755)
		if err == nil {
			return id, nil
		}
		if !os.IsExist(err) {
			return "", err
		}
		id = fmt.Sprintf("%s-%d", base, i)
	}
}

// launch 为条目新开一个任务。
func (a *App) launch(st *State, p Project, it Item) (*Job, error) {
	title := it.Title
	if title == "" {
		title = it.Text
	}
	j := &Job{Project: p.Name, ProjectDir: p.Dir, Item: it.Text, Title: title, Fingerprint: it.Fingerprint}
	return a.start(st, j, buildPrompt(p.Dir, it.Text), "")
}

// launchResume 用中断任务的会话（claude -r）续跑一次，记成一个新任务。
func (a *App) launchResume(st *State, orig *Job) (*Job, error) {
	j := &Job{
		Project: orig.Project, ProjectDir: orig.ProjectDir, Item: orig.Item, Title: orig.Title,
		Fingerprint: orig.Fingerprint, SessionID: orig.SessionID, ResumeOf: orig.ID,
		Summary: "上次中断（任务 " + orig.ID + "），已用同一会话 " + orig.SessionID + " 续跑",
	}
	orig.ResumePending = false
	j, err := a.start(st, j, buildResumePrompt(orig.ProjectDir, orig.Item), orig.SessionID)
	if j != nil {
		orig.ResumedBy = j.ID
		a.writeMeta(orig)
	}
	if err == nil {
		a.appendNightly(j) // 汇总里记一行「续跑中」，结束时再记一行结果
	}
	return j, err
}

// start 建任务目录、经任务外壳启动 claude（resume 非空时续跑那个会话），并立刻把任务写进 state.json。
// 外壳启动失败、或启动后写 state.json 失败（这时先杀掉刚启动的 claude）时任务记成 failed，返回这条记录和错误；
// 还没建出任务记录就失败时返回 nil。
func (a *App) start(st *State, j *Job, prompt, resume string) (*Job, error) {
	id := j.ID // 对话任务先建好任务目录（放主人发来的附件）再启动，id 已经有了
	if id == "" {
		var err error
		if id, err = a.newJobID(j.Project); err != nil {
			return nil, err
		}
	}
	dir := a.jobDir(id)
	session := ""
	if resume == "" {
		// 预先指定会话 id：tick 据此找会话记录判断卡死；中途被杀、重启时也能续跑（续跑沿用原会话，id 不变）
		var err error
		if session, err = newSessionID(); err != nil {
			return nil, err
		}
		j.SessionID = session
	}
	args := claudeArgs(a.Cfg, prompt, j.Project, j.Title, resume, session)
	if err := os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte(prompt+"\n"), 0o644); err != nil {
		return nil, err
	}
	j.ID, j.StartedAt, j.Status, j.BootTime = id, a.Now(), statusRunning, a.curBoot()
	j.Args = append([]string{a.claudePath()}, redactPrompt(args)...)
	self, err := os.Executable()
	var pid int
	if err == nil {
		pid, err = startJob(self, dir, j.ProjectDir, a.claudePath(), args, a.launchdLogPath())
	}
	if err != nil {
		j.Status = statusFailed
		now := a.Now()
		j.EndedAt = &now
		j.Summary = "启动失败：" + err.Error()
		st.Jobs = append(st.Jobs, j)
		a.writeMeta(j)
		a.appendNightly(j)
		a.noteOutcome(st, j)
		_ = saveState(a.statePath(), st)
		return j, err
	}
	j.PID = pid
	st.Jobs = append(st.Jobs, j)
	a.writeMeta(j)
	if err := saveState(a.statePath(), st); err != nil {
		// 状态里没有它，以后的 tick 就不会收尾、不会超时杀它，也不知道它占着位置：宁可马上杀掉
		how := "SIGTERM"
		if killGroup(pid, killGrace) {
			how = "SIGTERM 后 SIGKILL"
		}
		err = fmt.Errorf("启动后写状态文件失败，已杀掉刚启动的 claude（pid %d，%s）：%w", pid, how, err)
		a.logf("任务 %s：%v", id, err)
		a.finish(st, j, statusFailed, err.Error())
		return j, err
	}
	return j, nil
}

// redactPrompt 在 meta.json 里把 prompt 换成「见 prompt.txt」，免得重复存一大段。
func redactPrompt(args []string) []string {
	out := append([]string(nil), args...)
	for i := 0; i+1 < len(out); i++ {
		if out[i] == "-p" {
			out[i+1] = "<见 prompt.txt>"
		}
	}
	return out
}

// finish 标记任务结束，更新 meta.json，追加当天汇总。
func (a *App) finish(st *State, j *Job, status, summary string) {
	now := a.Now()
	j.Status = status
	j.EndedAt = &now
	j.Summary = summary
	a.readJobUsage(st, j)
	a.writeMeta(j)
	a.appendNightly(j)
	a.updateCostFooter(st, now)
	a.logf("任务 %s 结束：%s %s", j.ID, statusLabel(status), oneLine(summary, 120))
	a.noteOutcome(st, j)
	if j.Kind == kindChat {
		a.replyChat(j)
	}
}

func (a *App) writeMeta(j *Job) {
	b, err := json.MarshalIndent(j, "", "  ")
	if err == nil {
		err = writeFileAtomic(filepath.Join(a.jobDir(j.ID), "meta.json"), append(b, '\n'))
	}
	if err != nil {
		a.logf("警告：写 %s 的 meta.json 失败：%v", j.ID, err)
	}
}

func statusLabel(s string) string {
	switch s {
	case statusRunning:
		return "运行中"
	case statusDone:
		return "跑完"
	case statusFailed:
		return "失败"
	case statusTimeout:
		return "超时"
	case statusKilled:
		return "已停止"
	case statusInterrupted:
		return "中断"
	}
	return s
}

// jobLabel 在 statusLabel 之外区分出正在续跑的任务。
func jobLabel(j *Job) string {
	if j.Status == statusRunning && j.ResumeOf != "" {
		return "续跑中"
	}
	return statusLabel(j.Status)
}

// appendNightly 给 ~/.claude/nightly/<日期>-agentq.md 追加这个任务的一行，每晚复盘会读。
func (a *App) appendNightly(j *Job) {
	t := a.Now()
	if j.EndedAt != nil {
		t = *j.EndedAt
	}
	cost := ""
	if j.Usage != nil {
		cost = "；用量 " + j.Usage.detail()
	}
	a.appendNightlyLine(t, fmt.Sprintf("%s「%s」%s：%s%s；日志 %s",
		j.Project, truncRunes(j.Title, 40), jobLabel(j), oneLine(j.Summary, 300), cost, a.jobDir(j.ID)))
}

// appendNightlyLine 给 t 那天的汇总追加一行「- 时:分 text」，文件是新的就先写标题。
func (a *App) appendNightlyLine(t time.Time, text string) {
	path := a.nightlyPath(t)
	line := fmt.Sprintf("- %s %s\n", t.Format("15:04"), text)
	// 末尾有花费小结时，新行插在小结前面（整个文件重写）
	if raw, err := os.ReadFile(path); err == nil && strings.Contains(string(raw), costBegin) {
		if err := rewriteNightly(path, nightlyHeader(t), line, nil); err != nil {
			a.logf("警告：写当天汇总 %s 失败：%v", path, err)
		}
		return
	}
	err := os.MkdirAll(filepath.Dir(path), 0o755)
	var f *os.File
	if err == nil {
		f, err = os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	}
	if err != nil {
		a.logf("警告：写当天汇总 %s 失败：%v", path, err)
		return
	}
	defer f.Close()
	var b strings.Builder
	if fi, err := f.Stat(); err == nil && fi.Size() == 0 {
		b.WriteString(nightlyHeader(t))
	}
	b.WriteString(line)
	if _, err := f.WriteString(b.String()); err != nil {
		a.logf("警告：写当天汇总 %s 失败：%v", path, err)
	}
}

func nightlyHeader(t time.Time) string {
	return fmt.Sprintf("# agentq 任务汇总 %s\n\n", t.Format("2006-01-02"))
}

// commitRe：「合并提交」「提交号」「merge commit」后面紧跟的 7–40 位小写十六进制提交号（中间可以有空白、冒号、反引号、加粗的星号）。
// 只认这个固定写法，正文顺带提到「合并」的段落（如「审查结论是可以合并」）不算。
var commitRe = regexp.MustCompile("(合并提交|提交号|(?i:merge commit))[\\s:：是为`*]*([0-9a-f]{7,40})(?:[^0-9A-Za-z_]|$)")

// summaryParagraph 依次取结果文本里最后一个写着合并提交号的段落、最后一个说到合并情况的段落
// （prompt 要求最后一段写，但 agent 不一定放在最后），都没有就取最后一段。压成一行。
func summaryParagraph(s string) string {
	paras := strings.Split(strings.TrimSpace(s), "\n\n")
	for _, re := range []*regexp.Regexp{commitRe, mergeRe} {
		for i := len(paras) - 1; i >= 0; i-- {
			if p := strings.TrimSpace(paras[i]); re.MatchString(p) {
				return oneLine(p, 300)
			}
		}
	}
	for i := len(paras) - 1; i >= 0; i-- {
		if p := strings.TrimSpace(paras[i]); p != "" {
			return oneLine(p, 300)
		}
	}
	return ""
}

func oneLine(s string, n int) string {
	return truncRunes(strings.Join(strings.Fields(s), " "), n)
}

// tailLines 读文件末尾最多 n 行（只读最后 64KB）。
func tailLines(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	const max = 64 << 10
	if fi, err := f.Stat(); err == nil && fi.Size() > max {
		_, _ = f.Seek(fi.Size()-max, io.SeekStart)
	}
	b, _ := io.ReadAll(f)
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
