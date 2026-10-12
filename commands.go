package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// run/stop 改 state.json 前要拿和 tick 同一把锁；tick 收尾时可能等进程组退出，最多等这么久。
const cmdLockWait = 60 * time.Second

func (a *App) lockForCommand() (*fileLock, error) {
	lock, err := acquireLock(a.lockPath(), cmdLockWait)
	if err != nil {
		return nil, err
	}
	if lock == nil {
		return nil, fmt.Errorf("等了 %s 还拿不到 %s，可能有 tick 卡住了", cmdLockWait, a.lockPath())
	}
	return lock, nil
}

// cmdRun 立刻为项目启动一个任务：不看 paused、retry_after 和连环失败熔断（主人明确要做），仍遵守上限、用量冷却和登录检查；
// 订阅额度 ≥usageRefuseAt 时拒绝，≥usageStopAt 只提示。
func (a *App) cmdRun(project, keyword string) error {
	lock, err := a.lockForCommand()
	if err != nil {
		return err
	}
	defer lock.Close()
	st, err := loadState(a.statePath())
	if err != nil {
		return fmt.Errorf("状态文件读不出，当作未知，不启动：%w", err)
	}
	a.reap(st)
	a.enforceTimeouts(st)
	if err := saveState(a.statePath(), st); err != nil {
		return err
	}
	if now := a.Now(); now.Before(st.CooldownUntil) {
		return fmt.Errorf("用量冷却中，到 %s 前不启动", whenCN(st.CooldownUntil))
	}
	dir := filepath.Join(a.projectsRoot(), project)
	if !isGitMain(dir) {
		return fmt.Errorf("%s 不是 git 主目录（projects_root=%s）", dir, a.projectsRoot())
	}
	if ok, why := capacity(st, a.Cfg, project); !ok {
		return fmt.Errorf("已满，不启动：%s", why)
	}
	p := Project{Name: project, Dir: dir}
	items, err := p.items()
	if err != nil {
		return err
	}
	it, ok := a.pick(st, items, keyword, false)
	if !ok {
		if keyword != "" {
			return fmt.Errorf("%s 的「待做」里没有包含「%s」、可以做的条目（已标进行中、卡住或正在跑的不算）", p.backlogPath(), keyword)
		}
		return fmt.Errorf("%s 的「待做」里没有可以做的条目（已标进行中、卡住或正在跑的不算）", p.backlogPath())
	}
	if ok, why := a.checkLogin(); !ok {
		return errors.New(why)
	}
	u := a.usage(st)
	lvl, until, why := u.level(a.Now(), a.Cfg.Model)
	if lvl >= usageRefuse {
		if err := saveState(a.statePath(), st); err != nil {
			a.logf("写状态文件失败（额度读数没存下）：%v", err)
		}
		return errors.New(usageGateNote(lvl, until, why))
	}
	j, err := a.launch(st, p, it)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Out, "已启动任务 %s：%s「%s」pid %d\n", j.ID, p.Name, j.Title, j.PID)
	switch lvl {
	case usageStop:
		fmt.Fprintf(a.Out, "（%s）\n", usageGateNote(lvl, until, why))
	case usageUnknown:
		fmt.Fprintf(a.Out, "（额度未知，照常启动：%s）\n", oneLine(u.Err, 120))
	}
	if a.Cfg.Paused {
		fmt.Fprintln(a.Out, "（当前已暂停自动接活，run 不受影响；收尾仍由 tick 做）")
	}
	if now := a.Now(); now.Before(st.BreakerUntil) {
		fmt.Fprintf(a.Out, "（连环失败熔断中，到 %s 前 tick 不自动接活，run 不受影响）\n", whenCN(st.BreakerUntil))
	}
	fmt.Fprintf(a.Out, "看进度：agentq status；看结果：agentq logs %s\n", j.ID)
	return nil
}

func (a *App) cmdStatus() error {
	now := a.Now()
	w := a.Out
	fmt.Fprintf(w, "agentq 状态（%s）\n", now.Format("2006-01-02 15:04"))
	st, stErr := loadState(a.statePath())
	// 额度：缓存还新就用 state 里的；过期了现读一次（只显示，不写回 state，status 不拿锁）
	var u *UsageInfo
	if st != nil {
		u = st.Usage
	}
	if u.stale(now) {
		u = a.readUsage(now)
	}
	ulvl, uuntil, uwhy := u.level(now, a.Cfg.Model)
	fmt.Fprintf(w, "调度：%s\n", a.schedNote(st, stErr, now, ulvl, uuntil, uwhy))
	fmt.Fprintf(w, "额度：%s（%s 读取）\n", u.describe(now, a.Cfg.Model), whenCN(u.CheckedAt))
	fmt.Fprintf(w, "定点任务：%s；%s\n", a.routineStatus(kindNight), a.routineStatus(kindReview))
	fmt.Fprintf(w, "巡逻：%s\n", a.patrolStatus())
	fmt.Fprintf(w, "推送：%s\n", a.notifyStatus())
	if l := a.listenStatus(); l != "" {
		fmt.Fprintf(w, "监听：%s\n", l)
	}

	if st != nil {
		// 夜班等的就是「运行中」变成 0，所以它和复盘不算在里面，另列一段
		for _, j := range st.running() {
			if j.Kind == "" {
				continue
			}
			alive := ""
			if !procAlive(j.PID, nameMarker(j.Project)) {
				alive = "（进程已结束，等下次 tick 收尾）"
			}
			fmt.Fprintf(w, "定点任务在跑：%s，任务 %s，已跑 %s（超时 %s），pid %d%s\n", j.Project, j.ID,
				now.Sub(j.StartedAt).Round(time.Minute), formatDuration(a.jobTimeout(j)), j.PID, alive)
		}
		running := st.tasks()
		fmt.Fprintf(w, "\n运行中（%d/%d）：\n", len(running), a.Cfg.MaxParallel)
		if len(running) == 0 {
			fmt.Fprintln(w, "  无")
		}
		for _, j := range running {
			alive := ""
			if !procAlive(j.PID, nameMarker(j.Project)) {
				alive = "（进程已结束，等下次 tick 收尾）"
			} else if idle, ok := a.idleFor(j); ok && idle >= a.Cfg.StallAfter.Duration {
				alive = "，疑似卡死（会话记录 " + formatIdle(idle) + " 没增长，不会自动杀；要停就 agentq stop " + j.ID + "）"
			}
			resume := ""
			if j.ResumeOf != "" {
				resume = "续跑中（接中断的 " + j.ResumeOf + "），"
			}
			fmt.Fprintf(w, "  %s  %s「%s」%s已运行 %s，pid %d%s\n", j.ID, j.Project, truncRunes(j.Title, 30),
				resume, now.Sub(j.StartedAt).Round(time.Minute), j.PID, alive)
		}
	}

	fmt.Fprintf(w, "\n各项目「待做」（%s）：\n", a.projectsRoot())
	projects, err := scanProjects(a.projectsRoot())
	if err != nil {
		fmt.Fprintf(w, "  扫描失败：%v\n", err)
	}
	empty := 0
	for _, p := range projects {
		items, err := p.items()
		if err != nil {
			fmt.Fprintf(w, "  %s：BACKLOG 读不出：%v\n", p.Name, err)
			continue
		}
		if len(items) == 0 {
			empty++
			continue
		}
		inProg, stuck := 0, 0
		for _, it := range items {
			if it.InProgress {
				inProg++
			} else if it.Stuck {
				stuck++
			}
		}
		fmt.Fprintf(w, "  %s：%d 条（进行中 %d，卡住 %d）\n", p.Name, len(items), inProg, stuck)
	}
	if empty > 0 {
		fmt.Fprintf(w, "  其余 %d 个项目「待做」为空或没有 BACKLOG\n", empty)
	}

	if st != nil {
		var ended []*Job
		for _, j := range st.Jobs {
			if j.Status != statusRunning && j.EndedAt != nil {
				ended = append(ended, j)
			}
		}
		sortJobsByEnd(ended)
		if len(ended) > 10 {
			ended = ended[:10]
		}
		fmt.Fprintln(w, "\n最近结束：")
		if len(ended) == 0 {
			fmt.Fprintln(w, "  无")
		}
		for _, j := range ended {
			cost := ""
			if j.Usage != nil {
				cost = "（" + j.Usage.short() + "）"
			}
			fmt.Fprintf(w, "  %s  %s %s「%s」%s%s：%s%s\n", whenCN(*j.EndedAt), j.ID, j.Project,
				truncRunes(j.Title, 30), statusLabel(j.Status), resumeNote(j), oneLine(j.Summary, 80), cost)
		}
	}
	return nil
}

// schedNote：status「调度：」后面的那句（Telegram「状态」的回答也用）。
func (a *App) schedNote(st *State, stErr error, now time.Time, ulvl int, uuntil time.Time, uwhy string) string {
	switch {
	case stErr != nil:
		if errors.Is(stErr, errStateCorrupt) {
			return fmt.Sprintf("状态文件损坏，下次 tick 会备份它、按各任务的 meta.json 重建并收尾（那次不启动新任务）：%v", stErr)
		}
		return fmt.Sprintf("状态文件读不出，tick 不会启动新任务：%v", stErr)
	case a.Cfg.Paused:
		return "已暂停（paused=true），tick 只收尾不启动；agentq resume 恢复"
	case now.Before(st.CooldownUntil):
		return "用量冷却中，到 " + whenCN(st.CooldownUntil)
	case ulvl >= usageStop:
		return usageGateNote(ulvl, uuntil, uwhy)
	case now.Before(st.BreakerUntil):
		return fmt.Sprintf("连环失败熔断中，到 %s（原因见当天汇总 %s）", whenCN(st.BreakerUntil), a.nightlyPath(now))
	}
	return "正常"
}

// routineStatus：夜班/复盘的定点时刻和开关。
func (a *App) routineStatus(kind string) string {
	r := routines[kind]
	if r.enabled(a.Cfg) {
		return fmt.Sprintf("%s %s 开（%s=true）", r.name, r.at(), r.key)
	}
	return fmt.Sprintf("%s %s 关（%s=false，launchd 叫醒也不执行）", r.name, r.at(), r.key)
}

// resumeNote：中断任务后面标上续跑情况。
func resumeNote(j *Job) string {
	switch {
	case j.ResumedBy != "":
		return "（已续跑：" + j.ResumedBy + "）"
	case j.ResumePending:
		return "（等待续跑）"
	case j.ResumeOf != "":
		return "（续跑自 " + j.ResumeOf + "）"
	}
	return ""
}

func (a *App) cmdLogs(id string) error {
	var j *Job
	if st, err := loadState(a.statePath()); err == nil {
		j, err = st.find(id)
		if err != nil {
			return err
		}
	} else {
		return fmt.Errorf("状态文件读不出：%w；任务文件直接看 %s", err, a.jobDir(id))
	}
	dir := a.jobDir(j.ID)
	w := a.Out
	fmt.Fprintf(w, "任务 %s：%s「%s」\n", j.ID, j.Project, j.Title)
	fmt.Fprintf(w, "状态：%s%s；启动 %s", jobLabel(j), resumeNote(j), j.StartedAt.Format("2006-01-02 15:04"))
	if j.EndedAt != nil {
		fmt.Fprintf(w, "；结束 %s", j.EndedAt.Format("2006-01-02 15:04"))
	}
	fmt.Fprintf(w, "；pid %d\n", j.PID)
	if j.SessionID != "" {
		fmt.Fprintf(w, "会话：%s（在 %s 里 claude -r %s 可接着看）\n", j.SessionID, j.ProjectDir, j.SessionID)
	}
	fmt.Fprintf(w, "目录：%s\n", dir)
	if j.Status == statusRunning {
		fmt.Fprintln(w, "\n还在运行，结果要等它结束。")
	} else {
		fmt.Fprintf(w, "\n摘要：%s\n", j.Summary)
		if res, err := readResult(filepath.Join(dir, "result.json")); err == nil && strings.TrimSpace(res.Result) != "" {
			fmt.Fprintf(w, "\n结果全文：\n%s\n", strings.TrimSpace(res.Result))
		}
	}
	tail := tailLines(filepath.Join(dir, "stderr.log"), 40)
	if strings.TrimSpace(tail) == "" {
		fmt.Fprintln(w, "\nstderr：空")
	} else {
		fmt.Fprintf(w, "\nstderr 末尾：\n%s\n", tail)
	}
	return nil
}

func (a *App) cmdStop(id string) error {
	lock, err := a.lockForCommand()
	if err != nil {
		return err
	}
	defer lock.Close()
	st, err := loadState(a.statePath())
	if err != nil {
		return fmt.Errorf("状态文件读不出：%w", err)
	}
	j, err := st.find(id)
	if err != nil {
		return err
	}
	if j.Status != statusRunning {
		return fmt.Errorf("任务 %s 已经%s，不用停", j.ID, statusLabel(j.Status))
	}
	if !procAlive(j.PID, nameMarker(j.Project)) {
		if a.bootChanged(j) {
			a.finishRebooted(st, j)
		} else {
			a.finishFromResult(st, j)
		}
		fmt.Fprintf(a.Out, "任务 %s 的进程已经结束，已按结果收尾：%s\n", j.ID, statusLabel(j.Status))
		return saveState(a.statePath(), st)
	}
	hard := killGroup(j.PID, killGrace)
	how := "SIGTERM"
	if hard {
		how = "SIGTERM 后 SIGKILL"
	}
	a.finish(st, j, statusKilled, "被 agentq stop 停止（"+how+"）")
	if err := saveState(a.statePath(), st); err != nil {
		return err
	}
	fmt.Fprintf(a.Out, "已停止任务 %s（%s）\n", j.ID, how)
	return nil
}

func (a *App) cmdSetPaused(paused bool) error {
	a.Cfg.Paused = paused
	if err := saveConfig(a.configPath(), a.Cfg); err != nil {
		return err
	}
	if paused {
		fmt.Fprintln(a.Out, "已暂停：tick 只收尾不启动新任务（agentq run 仍可手动启动）")
	} else {
		fmt.Fprintln(a.Out, "已恢复：下次 tick 起照常接活")
	}
	return nil
}

// sortJobsByEnd：结束得晚的在前。
func sortJobsByEnd(jobs []*Job) {
	sort.Slice(jobs, func(i, k int) bool { return jobs[i].EndedAt.After(*jobs[k].EndedAt) })
}

func readResult(path string) (claudeResult, error) {
	var res claudeResult
	b, err := os.ReadFile(path)
	if err != nil {
		return res, err
	}
	return res, json.Unmarshal(b, &res)
}
