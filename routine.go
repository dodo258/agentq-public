package main

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// 夜班和复盘：launchd 每天定点叫醒 `agentq night|review --scheduled`，用 claude -p 执行随仓库提供的说明文件
// （编译进二进制，改了要 scripts/update.sh 重新安装才生效）。它们和待做任务一样记在 state.json、
// 落盘到 jobs/<id>/、进当天汇总、受超时管，但不占 max_parallel/per_project 的名额，也不做中断核验和续跑。

//go:embed prompts/night.md
var nightPrompt string

//go:embed prompts/review.md
var reviewPrompt string

const (
	kindNight  = "night"
	kindReview = "review"
)

type routine struct {
	kind   string // 命令名，也记在 Job.Kind
	name   string // 汇总、status 里显示的名字，也当 Job.Project
	key    string // config.json 里的开关
	file   string // 仓库里的说明文件
	prompt string
	hour   int // launchd 定点叫醒的时刻
	minute int
	report string // 说明要求写出的报告：~/.claude/nightly/<日期><report>.md
}

var routines = map[string]routine{
	kindNight:  {kind: kindNight, name: "夜班", key: "night_enabled", file: "prompts/night.md", prompt: nightPrompt, hour: 1, minute: 0, report: "-night"},
	kindReview: {kind: kindReview, name: "复盘", key: "review_enabled", file: "prompts/review.md", prompt: reviewPrompt, hour: 6, minute: 30, report: ""},
}

// missingReport：正常退出的夜班/复盘没写出说明要求的报告（启动那天或今天的都算）时返回报告路径，
// 多半是中途退出了（LESSONS：-p 在等通知时提前退出却记成跑完）；写了就返回空。
func (a *App) missingReport(j *Job) string {
	r := routines[j.Kind]
	path := func(t time.Time) string {
		return filepath.Join(a.Home, ".claude", "nightly", t.Format("2006-01-02")+r.report+".md")
	}
	for _, t := range []time.Time{j.StartedAt, a.Now()} {
		if _, err := os.Stat(path(t)); err == nil {
			return ""
		}
	}
	return path(a.Now())
}

func (r routine) enabled(cfg Config) bool {
	if r.kind == kindNight {
		return cfg.NightEnabled
	}
	return cfg.ReviewEnabled
}

func (r routine) job(dir, note string) *Job {
	return &Job{Kind: r.kind, Project: r.name, ProjectDir: dir, Item: r.file, Title: "agentq " + r.kind + note}
}

func (r routine) at() string { return fmt.Sprintf("%02d:%02d", r.hour, r.minute) }

// routinePromptPrefix：说明文件是照搬桌面端定时任务的，那边回合结束不会退出；claude -p 回合一结束进程就退出，
// 等夜班结束、等任务跑完都得在前台等（实测 -p 下前台 Bash 单独 sleep 很久会被拦，until 循环可以；Monitor 已禁用）。
const routinePromptPrefix = "你由 agentq 在无人值守模式下用 claude -p 启动：回合一结束进程就退出，后台命令、子代理通知都会被丢下。" +
	"一切等待都在前台做完：用前台 Bash 的 until 循环等条件（如 `until <条件>; do sleep 30; done`，单次 Bash 最长 10 分钟，到点再开一次），" +
	"不用后台命令、Monitor 或「等通知」。照下面的说明做完再结束，最后一段用一两句话写做了什么、结果写在哪。\n\n"

func routinePrompt(r routine) string { return routinePromptPrefix + r.prompt }

// jobTimeout：夜班、复盘用各自的超时（夜班要等到 05:00 才收工），巡逻分析、Telegram 对话各有固定超时，待做任务用 timeout。
func (a *App) jobTimeout(j *Job) time.Duration {
	switch j.Kind {
	case kindNight:
		return a.Cfg.NightTimeout.Duration
	case kindReview:
		return a.Cfg.ReviewTimeout.Duration
	case kindPatrol:
		return patrolAnalyzeTimeout
	case kindChat:
		return chatTimeout
	}
	return a.Cfg.Timeout.Duration
}

// cmdRoutine 启动夜班或复盘。scheduled 表示是 launchd 定点叫醒的：开关没打开时只记一行日志就退出；
// 主人手动执行不看开关。启动前不看暂停（夜班不改暂停开关）和连环失败熔断（夜班、复盘不是待做；它们秒退照样计数），
// 仍遵守用量冷却和登录检查；订阅额度 ≥usageStopAt 时定点叫醒不启动，手动执行到 ≥usageRefuseAt 才拒绝（同 tick 和 run）。
// 同一种已经在跑就不重复启动。定点叫醒没能启动时在当天汇总记一行，早报能看到。
func (a *App) cmdRoutine(kind string, scheduled bool) error {
	r := routines[kind]
	if scheduled && !r.enabled(a.Cfg) {
		a.logf("%s：%s=false，launchd 定点叫醒但不执行（切换步骤见 README「夜班和复盘」）", r.name, r.key)
		return nil
	}
	fail := func(format string, args ...any) error {
		msg := fmt.Sprintf("%s没有启动：%s", r.name, fmt.Sprintf(format, args...))
		a.logf("%s", msg)
		if scheduled {
			a.appendNightlyLine(a.Now(), msg)
		}
		return errLogged
	}
	lock, err := a.lockForCommand()
	if err != nil {
		return fail("%v", err)
	}
	defer lock.Close()
	st, err := loadState(a.statePath())
	if err != nil {
		return fail("状态文件读不出，当作未知（下次 tick 会处理）：%v", err)
	}
	a.reap(st)
	a.enforceTimeouts(st)
	if err := saveState(a.statePath(), st); err != nil {
		return fail("写状态文件失败：%v", err)
	}
	for _, j := range st.running() {
		if j.Kind == kind {
			a.logf("%s已经在跑（任务 %s，%s 启动），不重复启动", r.name, j.ID, whenCN(j.StartedAt))
			return nil
		}
	}
	limit := usageRefuse
	if scheduled {
		limit = usageStop
	}
	if why := a.routineBlocked(st, limit); why != "" {
		return fail("%s", why)
	}
	j, err := a.start(st, r.job(a.projectsRoot(), ""), routinePrompt(r), "")
	if err != nil {
		if j != nil { // 已经记成失败的任务、写过汇总
			a.logf("%s没有启动：%v", r.name, err)
			return errLogged
		}
		return fail("%v", err)
	}
	a.logf("%s已启动：任务 %s，pid %d，超时 %s；看进度 agentq status，看结果 agentq logs %s",
		r.name, j.ID, j.PID, formatDuration(a.jobTimeout(j)), j.ID)
	if !scheduled && !r.enabled(a.Cfg) {
		fmt.Fprintf(a.Out, "（%s=false：launchd %s 定点叫醒时不执行，手动执行不受影响）\n", r.key, r.at())
	}
	return nil
}

// routineBlocked：夜班、复盘启动前的检查（用量冷却、projects_root、登录、订阅额度 ≥limit），不能启动时返回原因。
// 读到的额度存进 st.Usage，调用方负责保存状态。
func (a *App) routineBlocked(st *State, limit int) string {
	if now := a.Now(); now.Before(st.CooldownUntil) {
		return "用量冷却中，到 " + whenCN(st.CooldownUntil)
	}
	dir := a.projectsRoot()
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return fmt.Sprintf("projects_root %s 不是目录", dir)
	}
	if ok, why := a.checkLogin(); !ok {
		return why
	}
	if lvl, until, why := a.usage(st).level(a.Now(), a.Cfg.Model); lvl >= limit {
		if err := saveState(a.statePath(), st); err != nil {
			a.logf("写状态文件失败（额度读数没存下）：%v", err)
		}
		return usageGateNote(lvl, until, why)
	}
	return ""
}

// catchUpGrace：定点时刻过后这么久还没有当天的记录才算错过——launchd 定点叫醒的那次要先查登录、读额度，过一会儿才记进状态。
const catchUpGrace = 15 * time.Minute

// catchUpBefore：错过的夜班只在这个钟点前补跑（再晚主人已经起床，夜班合并、上传的活不该挤到白天）；复盘当天任何时候都补。
var catchUpBefore = map[string]int{kindNight: 8, kindReview: 24}

// routineMissed：打开了的夜班或复盘今天的定点时刻已过、今天既没有这一种的任务记录（不论结果、手动还是补跑），
// 也没有它要写的报告（可能是别处跑的），且还在补跑窗口内时，返回 true 和今天的报告路径。
func (a *App) routineMissed(st *State, r routine, now time.Time) (bool, string) {
	if !r.enabled(a.Cfg) {
		return false, ""
	}
	y, m, d := now.Date()
	day := time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	at := time.Date(y, m, d, r.hour, r.minute, 0, 0, now.Location())
	if now.Before(at.Add(catchUpGrace)) || now.Hour() >= catchUpBefore[r.kind] {
		return false, ""
	}
	for _, j := range st.Jobs {
		if j.Kind == r.kind && !j.StartedAt.Before(day) {
			return false, ""
		}
	}
	report := filepath.Join(a.Home, ".claude", "nightly", day.Format("2006-01-02")+r.report+".md")
	if _, err := os.Stat(report); err == nil {
		return false, ""
	}
	return true, report
}

// catchUpRoutines：tick 补跑今天错过的夜班、复盘。launchd 的定点任务在关机、断电或还没登录时错过就不会再叫醒，
// 开机自动登录后由 tick 发现并补一次；补跑的任务今天有了记录，同一天不会再补。和定点叫醒一样不看暂停和熔断，
// 遵守用量冷却、登录和额度（≥usageStopAt 不启动），没能启动只记日志、下次 tick 再试。
// 复盘要汇总夜班报告，夜班还在跑时先不补复盘。
func (a *App) catchUpRoutines(st *State) {
	now := a.Now()
	for _, kind := range []string{kindNight, kindReview} {
		r := routines[kind]
		missed, report := a.routineMissed(st, r, now)
		if !missed || hasRunning(st, kind) { // 前一天开始、过了 0 点还在跑的不算今天的记录，也不能再起一个
			continue
		}
		if kind == kindReview && hasRunning(st, kindNight) {
			a.logf("%s：今天 %s 那次没跑，夜班还在跑，等它跑完再补", r.name, r.at())
			continue
		}
		if why := a.routineBlocked(st, usageStop); why != "" {
			a.logf("%s：今天 %s 那次没跑，开机后补跑没有启动：%s；下次 tick 再试", r.name, r.at(), why)
			continue
		}
		prompt := routinePrompt(r)
		if kind == kindNight {
			prompt = routinePromptPrefix + catchUpNightNote + r.prompt
		}
		j, err := a.start(st, r.job(a.projectsRoot(), "（开机后补跑）"), prompt, "")
		if err != nil {
			a.logf("%s：开机后补跑没有启动：%v", r.name, err) // 已记成失败的任务时，今天有了记录，不再补
			continue
		}
		msg := fmt.Sprintf("%s开机后补跑：今天 %s 那次没跑（没有当天的任务记录，也没有 %s），任务 %s",
			r.name, r.at(), filepath.Base(report), j.ID)
		a.logf("%s，pid %d", msg, j.PID)
		a.appendNightlyLine(now, msg)
	}
}

// catchUpNightNote：夜班说明里等任务最晚「到早上 05:00」，补跑时多半已经过了，不写明的话可能等到明天 05:00、一直占到超时。
const catchUpNightNote = "这次是开机后补跑（关机或断电错过了 01:00 那次）：下面说明里等待的截止时刻「早上 05:00」改成今天 08:00，到点就收工写报告。\n\n"

func hasRunning(st *State, kind string) bool {
	for _, j := range st.running() {
		if j.Kind == kind {
			return true
		}
	}
	return false
}
