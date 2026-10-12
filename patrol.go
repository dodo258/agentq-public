package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// 巡逻：tick 每小时调用一次（agentq patrol 立刻执行一次），纯脚本把下面几类检查走一圈，不调用 claude：
// launchd 服务退出码、自动备份推送 FAIL、当天备份报告失败、agentq 任务失败/超时/中断/疑似卡住、订阅额度、
// 08:00 后缺早报和夜班报告、各项目「等主人拍板」条目、磁盘剩余。每个问题带证据写进 ~/.agentq/patrol.json 和当天汇总。
// 按指纹记住报过的问题：问题还在就不重报，消失后再出现才重报。中断的任务写成待做放进对应项目 BACKLOG。
// 推给主人的只有 agentq 和项目自己的事（任务失败/卡住等）；基础设施类（infra）只写汇总，推送归看门狗
// （~/.claude/scripts/watchdog.py，不依赖 agentq，agentq 挂了也能报），免得同一件事推两遍。
// 有新异常时可选地启动一次 claude -p 只读分析原因（每天最多 patrolAnalyzeMax 次），结果在它结束时写进汇总。
const (
	patrolEvery          = time.Hour
	patrolWindow         = 24 * time.Hour // 自动推送 FAIL、结束的任务只看最近这么久
	patrolDiskMin        = 20 << 30       // 磁盘剩余低于这个就报
	patrolReportHour     = 8              // 这个钟点之后还没有当天早报、夜班报告就报
	patrolAnalyzeMax     = 3              // 每天最多启动几次 claude -p 分析
	patrolAnalyzeTimeout = 30 * time.Minute
	kindPatrol           = "patrol"
	patrolJobName        = "巡逻分析"
)

// launchdPrefix：只查和 agentq 自己的服务同一前缀的 launchd 服务（服务名去掉最后一段），系统和别的软件的不管。
var launchdPrefix = launchdLabel[:strings.LastIndex(launchdLabel, ".")+1]

type PatrolIssue struct {
	Fingerprint string   `json:"fingerprint"`
	Check       string   `json:"check"` // launchd、autopush、backup、job、stall、usage、report、decide、disk
	Title       string   `json:"title"`
	Evidence    []string `json:"evidence"`
	// Action：已经怎么处理、主人要不要做什么。
	Action string `json:"action,omitempty"`
	// Anomaly：是不是异常（要不要启动分析）。「等主人拍板」的新条目、额度快用完只是要主人知道，不分析。
	Anomaly   bool      `json:"anomaly,omitempty"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	job       *Job      // 中断/失败/超时的待做任务：新发现时看要不要写接手的待做
	// quiet：tick 发现时已经在汇总里写过一行（疑似卡死、额度档位变化），巡逻不再重复写。
	quiet bool
	// handled：已经自动处理（写了接手的待做、或已有任务在接手），不推给主人。
	handled bool
	gone    string // 消失时：已恢复 / 已处理
}

// infra：基础设施类问题，看门狗也查得到。照常检查、写进 patrol.json 和当天汇总，但不推给主人（巡逻推送和早报的「出问题」都不带），
// 由看门狗推。看门狗查不到的照常推：launchd 服务没装上（它只看列表里有的服务的退出码和常驻服务的 pid）、夜班报告缺失（它只查早报）。
func (is PatrolIssue) infra() bool {
	switch is.Check {
	case "autopush", "backup", "disk":
		return true
	case "launchd":
		return !strings.HasSuffix(is.Fingerprint, ":missing")
	case "report":
		return strings.HasPrefix(is.Fingerprint, "report:"+kindReview+":")
	}
	return false
}

type PatrolState struct {
	LastRun time.Time `json:"last_run"`
	// Issues 是上次巡逻时还在的问题；这次还在的不重报，不在了的算已恢复。
	Issues        []PatrolIssue `json:"issues"`
	AnalysisDay   string        `json:"analysis_day,omitempty"`
	AnalysisCount int           `json:"analysis_count,omitempty"`
}

type patrolReport struct {
	At            time.Time
	launchd       map[string]bool // 这次 launchctl list 里有的服务（判断消失的 launchd 问题是不是真恢复）
	pushOK        map[string]bool // 之后同仓库又推成功的那几段推送失败的指纹（判断消失的 autopush 问题是不是真恢复）
	New, Old      []PatrolIssue
	Gone          []PatrolIssue
	Notes         []string // 检查本身出的问题（读不了 launchd、BACKLOG 等），不算发现
	Analysis      string   // 分析启动了没有、为什么
	AnalysisJobID string
}

func (a *App) patrolPath() string { return filepath.Join(a.dataDir(), "patrol.json") }

// loadPatrol：文件不存在是第一次巡逻；读不出返回错误（当作未知，不巡逻）；内容坏了就备份成 patrol.json.bad-<时间>、从空的开始
// （之前报过的问题会再报一次）。
func (a *App) loadPatrol() (*PatrolState, error) {
	b, err := os.ReadFile(a.patrolPath())
	if errors.Is(err, fs.ErrNotExist) {
		return &PatrolState{}, nil
	}
	if err != nil {
		return nil, err
	}
	var ps PatrolState
	if err := json.Unmarshal(b, &ps); err != nil {
		bak := a.patrolPath() + ".bad-" + a.Now().Format("20060102-150405")
		if werr := writeFileAtomic(bak, b); werr != nil {
			return nil, fmt.Errorf("%s 格式不对（%v），备份到 %s 也失败：%w", a.patrolPath(), err, bak, werr)
		}
		a.logf("巡逻：%s 格式不对（%v），已备份为 %s，从空的开始（报过的问题会再报一次）", a.patrolPath(), err, bak)
		return &PatrolState{}, nil
	}
	return &ps, nil
}

func (a *App) savePatrol(ps *PatrolState) error {
	b, err := json.MarshalIndent(ps, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(a.patrolPath(), append(b, '\n'))
}

// patrolIfDue：tick 调用。巡逻开着、离上次满 patrolEvery（或时钟往回拨过）才巡逻一次，结果只记日志和汇总，出错不影响 tick。
func (a *App) patrolIfDue(st *State) {
	if !a.Cfg.PatrolEnabled {
		return
	}
	defer func() { // 巡逻出了没想到的错也不能让 tick 非 0 退出（连续 2 次会触发自动回滚）
		if p := recover(); p != nil {
			a.logf("巡逻：出错，本次巡逻作废，不影响 tick：%v", p)
		}
	}()
	ps, err := a.loadPatrol()
	if err != nil {
		a.logf("巡逻：读不了 %s，本次不巡逻：%v", a.patrolPath(), err)
		return
	}
	if now := a.Now(); !ps.LastRun.IsZero() && !now.Before(ps.LastRun) && now.Sub(ps.LastRun) < patrolEvery {
		return
	}
	r, err := a.runPatrol(st, ps)
	if err != nil {
		a.logf("巡逻：%v", err)
		return
	}
	a.logf("%s", r.logLine())
}

// cmdPatrol：立刻巡逻一次（不看 patrol_enabled 和上次时间），打印全部现有问题。
func (a *App) cmdPatrol() error {
	lock, err := a.lockForCommand()
	if err != nil {
		return err
	}
	defer lock.Close()
	st, err := loadState(a.statePath())
	if err != nil {
		return fmt.Errorf("状态文件读不出，当作未知，不巡逻（下次 tick 会处理）：%w", err)
	}
	a.reap(st)
	a.enforceTimeouts(st)
	if err := saveState(a.statePath(), st); err != nil {
		return err
	}
	ps, err := a.loadPatrol()
	if err != nil {
		return fmt.Errorf("读不了 %s，不巡逻：%w", a.patrolPath(), err)
	}
	r, err := a.runPatrol(st, ps)
	if err != nil {
		return err
	}
	r.print(a.Out, a.patrolPath(), a.nightlyPath(r.At))
	return nil
}

// runPatrol 跑一圈检查，和上次的问题按指纹比对：新问题写接手待做、存 patrol.json、写当天汇总，有新异常时看要不要启动分析。
func (a *App) runPatrol(st *State, ps *PatrolState) (*patrolReport, error) {
	now := a.Now()
	r := &patrolReport{At: now}
	found, failed := a.patrolChecks(st, now, r)
	prev := map[string]PatrolIssue{}
	for _, is := range ps.Issues {
		prev[is.Fingerprint] = is
	}
	seen := map[string]bool{}
	var cur []PatrolIssue
	for _, f := range found {
		if seen[f.Fingerprint] {
			continue
		}
		seen[f.Fingerprint] = true
		f.LastSeen = now
		if old, ok := prev[f.Fingerprint]; ok {
			f.FirstSeen, f.Action = old.FirstSeen, old.Action
			r.Old = append(r.Old, f)
		} else {
			f.FirstSeen = now
			if f.job != nil {
				f.Action, f.handled = a.takeOverTodo(st, f.job)
			}
			r.New = append(r.New, f)
		}
		cur = append(cur, f)
	}
	for _, old := range ps.Issues {
		if seen[old.Fingerprint] {
			continue
		}
		// 这次检查本身没做成的，不知道问题还在不在：照旧记着，不算恢复
		if hasAnyPrefix(old.Fingerprint, failed) {
			cur = append(cur, old)
			continue
		}
		switch old.gone = a.goneHow(st, r, old, now); old.gone {
		case goneKeep:
			old.gone = ""
			cur = append(cur, old)
		case "":
		default:
			r.Gone = append(r.Gone, old)
		}
	}
	ps.Issues, ps.LastRun = cur, now
	if err := a.savePatrol(ps); err != nil {
		return nil, fmt.Errorf("写 %s 失败，本次发现不写汇总（下次巡逻会再报）：%w", a.patrolPath(), err)
	}
	a.notifyPatrol(r)
	a.patrolAnalyze(st, ps, r)
	if r.AnalysisJobID != "" {
		if err := a.savePatrol(ps); err != nil {
			a.logf("巡逻：写 %s 失败（今天的分析次数没存下）：%v", a.patrolPath(), err)
		}
	}
	for _, is := range r.New {
		if !is.quiet {
			a.appendNightlyLine(now, "巡逻发现："+is.nightlyText())
		}
	}
	var fixed, handled []string
	for _, is := range r.Gone {
		if is.gone == goneHandled {
			handled = append(handled, truncRunes(is.Title, 60))
		} else {
			fixed = append(fixed, truncRunes(is.Title, 60))
		}
	}
	if len(fixed) > 0 {
		a.appendNightlyLine(now, fmt.Sprintf("巡逻：已恢复 %d 个之前报过的问题：%s", len(fixed), strings.Join(fixed, "；")))
	}
	if len(handled) > 0 {
		a.appendNightlyLine(now, fmt.Sprintf("巡逻：%d 条等主人拍板的已处理（不在那一节了）：%s", len(handled), strings.Join(handled, "；")))
	}
	if r.AnalysisJobID != "" {
		a.appendNightlyLine(now, "巡逻："+r.Analysis)
	}
	return r, nil
}

const (
	goneFixed   = "已恢复"
	goneHandled = "已处理"
	goneKeep    = "keep" // 还说不准好没好：照旧记着
)

// goneHow：上次报过的问题这次没再出现，是真的好了（已恢复）、拍板条目被处理了（已处理）、还说不准（goneKeep，照旧记着），
// 还是只是过了时间窗口、换了一天、服务卸掉了（返回空：悄悄去掉，不写「已恢复」）。
func (a *App) goneHow(st *State, r *patrolReport, is PatrolIssue, now time.Time) string {
	switch is.Check {
	case "job":
		return ""
	case "autopush": // 之后同仓库推成功了才算恢复；过了 24 小时窗口的悄悄去掉
		if !r.pushOK[is.Fingerprint] {
			return ""
		}
	case "decide":
		if decs, fails := scanDecisions(a.projectsRoot()); len(fails) == 0 {
			for _, d := range decs {
				if d.Fingerprint == is.Fingerprint && d.Waiting {
					return "" // 条目还在，只是加上了等条件标记：悄悄去掉，不写「已处理」
				}
			}
		}
		return goneHandled
	case "stall":
		id := strings.TrimPrefix(is.Fingerprint, "stall:")
		for _, j := range st.running() {
			if j.ID == id {
				return goneFixed // 会话记录又在长了
			}
		}
		return "" // 任务结束了：失败、超时由任务那一类报
	case "backup":
		if !strings.HasSuffix(is.Fingerprint, ":"+now.Format("2006-01-02")) {
			return "" // 前一天的，换天了
		}
	case "report": // report:<kind>:<日期>：报告文件真的有了才算恢复；补跑还没写出来（或开关刚关）就照旧记着
		f := strings.Split(is.Fingerprint, ":")
		if len(f) != 3 || f[2] != now.Format("2006-01-02") {
			return ""
		}
		if _, err := os.Stat(filepath.Join(a.Home, ".claude", "nightly", f[2]+routines[f[1]].report+".md")); err != nil {
			return goneKeep
		}
	case "launchd": // launchd:<服务>[:missing|:down]：服务还在列表里（退出码回到 0、缺的装上了、又在跑了）才算恢复，从列表里卸掉了不算
		label := strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(is.Fingerprint, "launchd:"), ":missing"), ":down")
		if !r.launchd[label] {
			return ""
		}
	case "usage": // 一个窗口重置了，别的窗口仍 ≥90% 时不算恢复（那个窗口会作为新指纹报出来）
		if lvl, _, _ := st.Usage.level(now, a.Cfg.Model); lvl >= usageStop {
			return ""
		}
	}
	return goneFixed
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// patrolChecks 依次跑各项检查。检查本身没做成时，在 r.Notes 记原因、返回它的指纹前缀（那一类上次的问题照旧记着）。
func (a *App) patrolChecks(st *State, now time.Time, r *patrolReport) ([]PatrolIssue, []string) {
	var out []PatrolIssue
	var failed []string
	fail := func(prefix, note string) {
		failed = append(failed, prefix)
		r.Notes = append(r.Notes, note)
	}
	out = append(out, a.checkLaunchd(r, fail)...)
	out = append(out, a.checkAutopush(now, r, fail)...)
	out = append(out, a.checkBackupReport(now, fail)...)
	out = append(out, a.checkJobs(st, now)...)
	out = append(out, a.checkUsage(st, now)...)
	out = append(out, a.checkReports(st, now)...)
	out = append(out, a.checkDecisions(fail)...)
	out = append(out, a.checkDisk(fail)...)
	return out, failed
}

// checkLaunchd：launchctl list 里前缀是 launchdPrefix 的服务，上次退出码不是 0 的报出来，证据带它日志的末尾；
// agentq 装着时，开关打开的夜班、复盘定点服务不在列表里也报。
func (a *App) checkLaunchd(r *patrolReport, fail func(prefix, note string)) []PatrolIssue {
	out, err := a.launchctl("list")
	if err != nil {
		fail("launchd:", "读不了 launchd 服务列表（launchctl list）："+oneLine(err.Error()+" "+string(out), 200))
		return nil
	}
	var res []PatrolIssue
	loaded := map[string]bool{}
	r.launchd = loaded
	listenPID := ""
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 3 || !strings.HasPrefix(f[2], a.launchdPrefix()) {
			continue
		}
		pid, code, label := f[0], f[1], f[2]
		loaded[label] = true
		if label == a.listenLabel() {
			listenPID = pid
		}
		if code == "0" || code == "-" {
			continue
		}
		ev := "launchctl list：" + label + " 上次退出码 " + code
		if n, err := strconv.Atoi(code); err == nil && n < 0 {
			ev += fmt.Sprintf("（被信号 %d 终止）", -n)
		}
		if pid != "-" {
			ev += "，现在在跑（pid " + pid + "）"
		}
		evs := []string{ev}
		if t := a.launchdLogTail(label); t != "" {
			evs = append(evs, t)
		}
		res = append(res, PatrolIssue{
			Fingerprint: "launchd:" + label, Check: "launchd", Anomaly: true, // 不带退出码：从 1 变成 2 不算恢复再出问题
			Title:    "launchd 服务 " + label + " 上次退出码 " + code,
			Evidence: evs,
			Action:   "没自动处理：按日志查原因；修好后它下次运行退出码为 0，巡逻就记为已恢复",
		})
	}
	// Telegram 监听是常驻服务：配了 Telegram 时没装上、或装着却没在跑（KeepAlive 拉不起来），主人发消息就没人秒回
	if c, _ := a.telegramCreds(); c != nil && a.Cfg.NotifyEnabled && loaded[a.serviceLabel()] {
		label := a.listenLabel()
		switch {
		case !loaded[label]:
			res = append(res, PatrolIssue{
				Fingerprint: "launchd:" + label + ":missing", Check: "launchd", Anomaly: true,
				Title:    "Telegram 监听 " + label + " 没装上",
				Evidence: []string{"launchctl list 里有 " + a.serviceLabel() + "，没有 " + label + "，而 Telegram 配好了；现在靠 tick 每 5 分钟兜底收消息"},
				Action:   "没自动处理：在终端执行 agentq install 装上（可重复执行）",
			})
		case listenPID == "-":
			res = append(res, PatrolIssue{
				Fingerprint: "launchd:" + label + ":down", Check: "launchd", Anomaly: true,
				Title:    "Telegram 监听 " + label + " 没在跑",
				Evidence: []string{"launchctl list：" + label + " 没有 pid（KeepAlive 没把它拉起来）；现在靠 tick 每 5 分钟兜底收消息"},
				Action:   "没自动处理：看 ~/.agentq/launchd.log 里「监听：」开头的几行，再执行 agentq install 重新装上",
			})
		}
	}
	if loaded[a.serviceLabel()] {
		for _, kind := range []string{kindNight, kindReview} {
			r := routines[kind]
			if label := a.routineLabel(kind); r.enabled(a.Cfg) && !loaded[label] {
				res = append(res, PatrolIssue{
					Fingerprint: "launchd:" + label + ":missing", Check: "launchd", Anomaly: true,
					Title:    r.name + "的定点服务 " + label + " 没装上",
					Evidence: []string{"launchctl list 里有 " + a.serviceLabel() + "，没有 " + label + "，而 " + r.key + "=true"},
					Action:   "没自动处理：在终端执行 agentq install 重新装上（可重复执行）",
				})
			}
		}
	}
	return res
}

// launchdLogTail：从服务的 plist 取日志路径（先 StandardErrorPath 再 StandardOutPath），返回末尾 3 行；取不到返回空。
func (a *App) launchdLogTail(label string) string {
	plist := a.plistPathFor(label)
	for _, key := range []string{"StandardErrorPath", "StandardOutPath"} {
		out, err := exec.Command("/usr/bin/plutil", "-extract", key, "raw", "-o", "-", plist).Output()
		p := strings.TrimSpace(string(out))
		if err != nil || p == "" {
			continue
		}
		if t := strings.TrimSpace(tailLines(p, 3)); t != "" {
			return "日志 " + p + " 末尾：" + oneLine(t, 200)
		}
	}
	return ""
}

// autopush.log 的行：「2026-10-10 12:21:35 FAIL /…/agentq/.git: 报错」「2026-10-11 11:52:10 ok /…/agentq/.git」。
var autopushRe = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}) (FAIL|ok) (.+?)(?:: (.*))?$`)

// checkAutopush：~/.claude/state/autopush.log 最近 patrolWindow 里有 FAIL、之后同仓库还没推成功的仓库，报最近一段连续失败
// （中间没有 ok 的一串 FAIL，指纹带这一段第一次 FAIL 的时间：成功过之后再 FAIL 是新问题）。之后又推成功（ok）的一段算已恢复，
// 不报，指纹记进 r.pushOK。
func (a *App) checkAutopush(now time.Time, r *patrolReport, fail func(prefix, note string)) []PatrolIssue {
	path := filepath.Join(a.Home, ".claude", "state", "autopush.log")
	text, err := readTail(path, 256<<10)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		fail("autopush:", "读不了 "+path+"："+err.Error())
		return nil
	}
	type repoFails struct {
		n       int
		start   time.Time // 这一段连续失败的第一次 FAIL
		last    string
		lastAt  time.Time
		okAfter time.Time
	}
	repos := map[string]*repoFails{}
	var order []string
	r.pushOK = map[string]bool{}
	fp := func(repo string, start time.Time) string {
		return "autopush:" + repo + "@" + start.Format("2006-01-02T15:04:05")
	}
	for _, line := range strings.Split(text, "\n") {
		m := autopushRe.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		t, err := time.ParseInLocation("2006-01-02 15:04:05", m[1], now.Location())
		if err != nil {
			continue
		}
		rf := repos[m[3]]
		if m[2] == "FAIL" {
			if t.After(now.Add(time.Minute)) {
				continue
			}
			// 先不按 24 小时窗口筛：一段连续失败的起点要从它真正的第一次 FAIL 算，不随窗口往后移（不然指纹每小时一变、每小时重报）。
			// 一段失败长过读到的 256KB 尾巴（两千多行）时起点仍会移，可以接受。
			if rf == nil {
				rf = &repoFails{}
				repos[m[3]] = rf
				order = append(order, m[3])
			}
			if rf.n == 0 || !rf.okAfter.IsZero() { // 第一次，或成功推送过之后又失败：新的一段
				if rf.n > 0 {
					r.pushOK[fp(m[3], rf.start)] = true
				}
				rf.n, rf.start = 0, t
			}
			rf.n++
			rf.last, rf.lastAt, rf.okAfter = strings.TrimSpace(line), t, time.Time{}
		} else if rf != nil && t.After(rf.lastAt) {
			rf.okAfter = t
		}
	}
	var res []PatrolIssue
	for _, repo := range order {
		rf := repos[repo]
		if !rf.okAfter.IsZero() { // 之后推成功了：已恢复
			r.pushOK[fp(repo, rf.start)] = true
			continue
		}
		if now.Sub(rf.lastAt) > patrolWindow { // 最近一段的最后一次 FAIL 也在 24 小时前：不算新 FAIL
			continue
		}
		name := filepath.Base(repo)
		if name == ".git" {
			name = filepath.Base(filepath.Dir(repo))
		}
		res = append(res, PatrolIssue{
			Fingerprint: fp(repo, rf.start), Check: "autopush", Anomaly: true,
			Title:    "自动备份推送失败：" + name,
			Evidence: []string{fmt.Sprintf("%s 里 %s 起连续 %d 次 FAIL，之后还没推成功，最后一次：%s", path, whenCN(rf.start), rf.n, oneLine(rf.last, 200))},
			Action:   "没自动处理：每天 05:30 的定时备份会再推一次，仍失败会写在当天的 *-backup.md",
		})
	}
	return res
}

// readTail 读文件最后 max 字节（从完整的一行开始）。
func readTail(path string, max int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	cut := false
	if fi, err := f.Stat(); err == nil && fi.Size() > max {
		if _, err := f.Seek(fi.Size()-max, io.SeekStart); err != nil {
			return "", err
		}
		cut = true
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	s := string(b)
	if cut {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
	}
	return s, nil
}

// 备份脚本（~/.claude/scripts/backup-push.sh）的固定写法：第一行「…，**N 个失败**」，失败的仓库一行「- 名字：**推送失败**：报错」。
var (
	backupFailRe     = regexp.MustCompile(`\*\*(\d+) 个失败\*\*`)
	backupFailLineRe = regexp.MustCompile(`^- .+：\*\*推送失败\*\*`)
)

// checkBackupReport：当天的 <日期>-backup.md 第一行写着「**N 个失败**」就报，证据带失败的那几行。
func (a *App) checkBackupReport(now time.Time, fail func(prefix, note string)) []PatrolIssue {
	day := now.Format("2006-01-02")
	path := filepath.Join(a.Home, ".claude", "nightly", day+"-backup.md")
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		fail("backup:", "读不了 "+path+"："+err.Error())
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	m := backupFailRe.FindStringSubmatch(lines[0])
	if m == nil || m[1] == "0" {
		return nil
	}
	evs := []string{path + " 第一行：" + oneLine(lines[0], 200)}
	for _, l := range lines[1:] {
		if backupFailLineRe.MatchString(strings.TrimSpace(l)) && len(evs) < 4 {
			evs = append(evs, oneLine(l, 200))
		}
	}
	return []PatrolIssue{{
		Fingerprint: "backup:" + day, Check: "backup", Anomaly: true,
		Title:    "今天的备份有 " + m[1] + " 个仓库推送失败",
		Evidence: evs,
		Action:   "没自动处理：明天 05:30 会再推一次；急的话手动执行 ~/.claude/scripts/backup-push.sh",
	}}
}

// checkJobs：最近 patrolWindow 里失败、超时、中断的任务（撞用量上限的、等着自动续跑或已经续跑的不算，巡逻分析和 Telegram 对话自己的不算），
// 和会话记录超过 stall_after 没增长的运行中任务。
func (a *App) checkJobs(st *State, now time.Time) []PatrolIssue {
	var res []PatrolIssue
	for _, j := range st.Jobs {
		if j.Kind == kindPatrol || j.Kind == kindChat { // 对话任务没做成时已经直接回了主人
			continue
		}
		name := j.Project + "「" + truncRunes(j.Title, 40) + "」"
		if j.Status == statusRunning {
			idle, ok := a.idleFor(j)
			if !ok || idle < a.Cfg.StallAfter.Duration {
				continue
			}
			res = append(res, PatrolIssue{
				Fingerprint: "stall:" + j.ID, Check: "stall", Anomaly: true, quiet: true,
				Title: "任务疑似卡住：" + name,
				Evidence: []string{fmt.Sprintf("任务 %s 已运行 %s，会话记录 %s 没增长（stall_after=%s），pid %d",
					j.ID, now.Sub(j.StartedAt).Round(time.Minute), formatIdle(idle), formatDuration(a.Cfg.StallAfter.Duration), j.PID)},
				Action: fmt.Sprintf("没自动处理：不会自动杀，运行满 %s 才终止；要停就 agentq stop %s", formatDuration(a.jobTimeout(j)), j.ID),
			})
			continue
		}
		if j.EndedAt == nil || now.Sub(*j.EndedAt) > patrolWindow {
			continue
		}
		switch j.Status {
		case statusFailed, statusTimeout, statusInterrupted:
		default:
			continue
		}
		if j.LimitHit || j.ResumePending || j.ResumedBy != "" {
			continue
		}
		is := PatrolIssue{
			Fingerprint: "job:" + j.ID, Check: "job", Anomaly: true,
			Title: "任务" + statusLabel(j.Status) + "：" + name,
			Evidence: []string{fmt.Sprintf("任务 %s，%s 启动，%s 结束：%s", j.ID, whenCN(j.StartedAt),
				whenCN(*j.EndedAt), oneLine(j.Summary, 200)), "详情：agentq logs " + j.ID},
			Action: "没自动处理：原因看 agentq logs " + j.ID,
		}
		if j.Kind == "" {
			is.job = j
		}
		res = append(res, is)
	}
	return res
}

// takeOverTodo：中断/失败/超时的待做任务，BACKLOG 里那一条还标着进行中、也没有别的任务在接手时，
// 在那个项目 BACKLOG「待做」末尾写一条接手的待做并提交，返回处理说明。接手的待做自己又没做完时不再写（免得一层套一层），
// 暂停时只报不写。第二个返回值：已经有人接手（写了待做、或已有任务在接手），不用推给主人。
func (a *App) takeOverTodo(st *State, j *Job) (string, bool) {
	if strings.HasPrefix(j.Title, takeOverPrefix) {
		return "这一条本身是巡逻写的接手待做，又没做完，不再写新的接手条目，等主人处理；原因看 agentq logs " + j.ID, false
	}
	if a.Cfg.Paused {
		return "已暂停（paused=true），只报不写接手的待做；原因看 agentq logs " + j.ID, false
	}
	p := Project{Name: j.Project, Dir: j.ProjectDir}
	items, err := p.items()
	if err != nil {
		return "没写接手的待做：读不了 BACKLOG：" + err.Error(), false
	}
	it, ok := inProgressItem(items, j.Fingerprint)
	if !ok {
		return "BACKLOG 里这一条已不在进行中，不用接手；原因看 agentq logs " + j.ID, false
	}
	for _, o := range st.Jobs {
		if o == j || o.Fingerprint != j.Fingerprint {
			continue
		}
		if o.Status == statusRunning || o.ResumePending || (o.EndedAt != nil && o.EndedAt.After(*j.EndedAt)) {
			return "之后已有任务 " + o.ID + " 在接手这一条，没写待做", true
		}
	}
	title := takeOverPrefix + truncRunes(j.Title, 30) + "」"
	// 不能出现「（进行中」和括号卡住标记，否则这一条自己会被当成进行中或卡住、永远不会被挑
	line := fmt.Sprintf("- [ ] %s —— 巡逻发现任务 %s 于 %s %s，原条目「%s」还标着进行中%s。看 git 状态、worktree 和分支现状，"+
		"按 agent-task 从实际进度接着把原条目做到合并，合并后把原条目和这一条一起移到「已完成」；做不下去就去掉原条目的进行中标记、"+
		"按 agent-task 换成卡住标记写明原因。验收：原条目不再标进行中（已完成或标了卡住）；任务记录 %s",
		title, j.ID, whenCN(*j.EndedAt), statusLabel(j.Status), j.Title, branchNote(j.ProjectDir, it.Text), a.jobDir(j.ID))
	if msg, err := a.commitTodo(p, j.ID, line, title); err != nil {
		return "没写接手的待做：" + err.Error(), false
	} else if msg != "" {
		return msg, true // 已经写过接手的待做
	}
	a.logf("巡逻：在 %s 的 BACKLOG 写了待做「%s」", p.Name, title)
	return "已在 " + p.Name + " 的 BACKLOG「待做」末尾写了「" + title + "」并提交，agentq 会接着做", true
}

const takeOverPrefix = "接手中断的「"

// mainLockWait：写接手待做时排队等 <项目>-main 锁最多多久（tick 拿着自己的锁，不能等太久）。测试里改小。
var mainLockWait = 2 * time.Minute

// commitTodo 拿着 <项目>-main 锁（和别的任务、夜班、复盘排队改主目录）：主目录在 main、BACKLOG 没有没提交的改动、
// 里面还没有提到这个任务时，在「待做」节末尾插入 line，只提交这个文件；提交失败把文件改回去。
// 没写、但不算出错时返回说明（如已经写过）。
func (a *App) commitTodo(p Project, jobID, line, title string) (string, error) {
	unlock, err := a.lockMain(p.Name+"-main", mainLockWait)
	if err != nil {
		return "", err
	}
	defer unlock()
	git := func(args ...string) (string, error) { return a.git(p.Dir, args...) }
	if br, err := git("symbolic-ref", "--short", "HEAD"); err != nil || br != "main" {
		return "", fmt.Errorf("主目录不在 main（%s）", oneLine(br, 60))
	}
	if out, err := git("status", "--porcelain", "--", "docs/BACKLOG.md"); err != nil || out != "" {
		return "", fmt.Errorf("BACKLOG 有没提交的改动或查不了 git 状态（%s）", oneLine(out, 100))
	}
	b, err := os.ReadFile(p.backlogPath())
	if err != nil {
		return "", err
	}
	old := string(b)
	if strings.Contains(old, jobID) {
		return "BACKLOG 里已经有提到任务 " + jobID + " 的条目，没再写", nil
	}
	text, ok := insertTodo(old, line)
	if !ok {
		return "", errors.New("BACKLOG 里没有「## 待做」一节")
	}
	if err := writeFileAtomic(p.backlogPath(), []byte(text)); err != nil {
		return "", err
	}
	if out, err := git("commit", "-q", "-m", "待办："+title+"（巡逻）", "--", "docs/BACKLOG.md"); err != nil {
		// 只在文件还是自己写的样子时改回去，免得盖掉别人刚写的
		if cur, rerr := os.ReadFile(p.backlogPath()); rerr == nil && string(cur) == text {
			_ = writeFileAtomic(p.backlogPath(), b)
		}
		return "", fmt.Errorf("提交失败，BACKLOG 已改回：%v %s", err, oneLine(out, 200))
	}
	return "", nil
}

// lockMain 按 ~/.claude/scripts/with-lock.sh 的约定占锁：目录 ~/.claude/state/locks/<名字>，里面 pid 文件写持有进程，
// 持有进程不在了算旧锁、清掉重来。最多等 wait，返回释放函数。
func (a *App) lockMain(name string, wait time.Duration) (func(), error) {
	dir := filepath.Join(a.Home, ".claude", "state", "locks", name)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		err := os.Mkdir(dir, 0o755)
		if err == nil {
			_ = os.WriteFile(filepath.Join(dir, "pid"), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644)
			return func() { os.RemoveAll(dir) }, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		b, _ := os.ReadFile(filepath.Join(dir, "pid"))
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 && errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			os.RemoveAll(dir)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("等了 %s 还拿不到锁 %s（持有进程 %s）", wait, dir, strings.TrimSpace(string(b)))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// git 在 dir 执行 git，HOME 用 a.Home（读主人的 git 配置和钩子；测试里是临时目录）。
func (a *App) git(dir string, args ...string) (string, error) {
	// tick 拿着锁：提交时的钩子（如查仓库可见性要联网）卡住也不能一直等
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gitEnv(a.Home)
	cmd.WaitDelay = 5 * time.Second // 杀掉 git 后它的子进程还占着输出管道时不再等
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func gitEnv(home string) []string {
	var out []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if name == "HOME" || name == "XDG_CONFIG_HOME" || strings.HasPrefix(name, "GIT_") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "HOME="+home)
}

// insertTodo 把 line 插到「## 待做」节最后一个条目之后；节里没有条目就插在节末最后一个非空行之后。
func insertTodo(text, line string) (string, bool) {
	lines := strings.Split(text, "\n")
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "## ") && strings.HasPrefix(strings.TrimSpace(l[3:]), "待做") {
			start = i
			break
		}
	}
	if start < 0 {
		return text, false
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "## ") {
			end = i
			break
		}
	}
	at := start
	for i := start + 1; i < end; i++ {
		if strings.HasPrefix(lines[i], "- [") {
			at = i
		}
	}
	if at == start {
		for i := end - 1; i > start; i-- {
			if strings.TrimSpace(lines[i]) != "" {
				at = i
				break
			}
		}
	}
	out := append([]string{}, lines[:at+1]...)
	out = append(out, line)
	out = append(out, lines[at+1:]...)
	return strings.Join(out, "\n"), true
}

// checkUsage：state 里缓存的订阅额度（tick 领活前读的，巡逻不另读）到了 usageStopAt。
func (a *App) checkUsage(st *State, now time.Time) []PatrolIssue {
	u := st.Usage
	lvl, until, why := u.level(now, a.Cfg.Model)
	if lvl < usageStop {
		return nil
	}
	fp := "usage"
	for _, w := range u.Windows {
		if w.counts(a.Cfg.Model) && w.usedAt(now) >= usageStopAt {
			reset := "?"
			if w.ResetsAt != nil {
				reset = w.ResetsAt.Format("2006-01-02T15:04")
			}
			fp += ":" + w.Label + "@" + reset
		}
	}
	return []PatrolIssue{{
		Fingerprint: fp, Check: "usage", quiet: true,
		Title:    "订阅额度快用完：" + why,
		Evidence: []string{"额度：" + u.describe(now, a.Cfg.Model) + "（" + whenCN(u.CheckedAt) + " 读取）"},
		Action:   usageGateNote(lvl, until, why),
	}}
}

// checkReports：patrolReportHour 点以后，开关打开的复盘还没写当天早报（<日期>.md）、夜班还没写报告（<日期>-night.md）。
// 正在跑的不报。
func (a *App) checkReports(st *State, now time.Time) []PatrolIssue {
	if now.Hour() < patrolReportHour {
		return nil
	}
	day := now.Format("2006-01-02")
	var res []PatrolIssue
	for _, x := range []struct{ kind, what string }{{kindReview, "早报"}, {kindNight, "夜班报告"}} {
		r := routines[x.kind]
		if !r.enabled(a.Cfg) {
			continue
		}
		path := filepath.Join(a.Home, ".claude", "nightly", day+r.report+".md")
		if _, err := os.Stat(path); err == nil {
			continue
		}
		var last *Job
		running := false
		for _, j := range st.Jobs {
			if j.Kind != x.kind || j.StartedAt.Format("2006-01-02") != day {
				continue
			}
			if j.Status == statusRunning {
				running = true
			}
			last = j
		}
		if running {
			continue
		}
		evs := []string{fmt.Sprintf("%s 不存在（%s 查）", path, now.Format("15:04"))}
		action := fmt.Sprintf("没自动处理：要补就在终端执行 agentq %s", x.kind)
		if last != nil {
			evs = append(evs, fmt.Sprintf("今天的%s任务 %s %s：%s", r.name, last.ID, statusLabel(last.Status), oneLine(last.Summary, 150)))
			action = "没自动处理：原因看 agentq logs " + last.ID + "；要补就在终端执行 agentq " + x.kind
		} else {
			evs = append(evs, fmt.Sprintf("今天没有%s任务的记录（launchd 每天 %s 叫醒，看 %s）", r.name, r.at(), a.launchdLogPath()))
		}
		res = append(res, PatrolIssue{
			Fingerprint: "report:" + x.kind + ":" + day, Check: "report", Anomaly: true,
			Title:    fmt.Sprintf("%02d:00 后还没有当天的%s", patrolReportHour, x.what),
			Evidence: evs,
			Action:   action,
		})
	}
	return res
}

// checkDecisions：各项目 BACKLOG「等主人拍板」里的条目（「- [ ]」开头），指纹按标题算，条目一直在就只报一次。
// 带等条件标记的跳过（前置条件没满足，现在不用主人回复）。推送由 tick 每 5 分钟看一遍（notify.go watchDecisions），巡逻只记汇总。
func (a *App) checkDecisions(fail func(prefix, note string)) []PatrolIssue {
	all, fails := scanDecisions(a.projectsRoot())
	decs := askable(all)
	for _, f := range fails {
		fail("decide:", f)
	}
	var res []PatrolIssue
	for _, d := range decs {
		res = append(res, PatrolIssue{
			Fingerprint: d.Fingerprint, Check: "decide",
			Title:    d.Project + " 等主人拍板：" + truncRunes(d.Title, 60),
			Evidence: []string{d.Path + "「等主人拍板」：" + oneLine(d.Text, 200)},
			Action:   "要主人回复：在会话里或 Telegram 上说选哪个（或直接改那一条），agent 会按决定接着做",
		})
	}
	return res
}

// checkDisk：HOME 所在磁盘剩余低于 patrolDiskMin。
func (a *App) checkDisk(fail func(prefix, note string)) []PatrolIssue {
	free, total, err := a.diskFree(a.Home)
	if err != nil {
		fail("disk", "读不了磁盘剩余空间："+err.Error())
		return nil
	}
	if free >= patrolDiskMin {
		return nil
	}
	gb := func(n uint64) string { return strconv.FormatFloat(float64(n)/(1<<30), 'f', 1, 64) }
	return []PatrolIssue{{
		Fingerprint: "disk", Check: "disk", Anomaly: true,
		Title:    "磁盘剩余不到 " + strconv.Itoa(patrolDiskMin>>30) + " GB",
		Evidence: []string{a.Home + " 所在磁盘剩余 " + gb(free) + " GB（共 " + gb(total) + " GB）"},
		Action:   "没自动处理：清理不用的大文件（Xcode 的 DerivedData、旧模拟器和归档常占很多）",
	}}
}

func (a *App) diskFree(path string) (free, total uint64, err error) {
	if a.DiskFree != nil {
		return a.DiskFree(path)
	}
	var s syscall.Statfs_t
	if err := syscall.Statfs(path, &s); err != nil {
		return 0, 0, err
	}
	return s.Bavail * uint64(s.Bsize), s.Blocks * uint64(s.Bsize), nil
}

// notifyPatrol：新发现、没自动处理的问题推给主人（合成一条「巡逻发现」）。「等主人拍板」由 tick 的 watchDecisions 推，
// 额度由读额度时推，基础设施类由看门狗推，这里不重复。
func (a *App) notifyPatrol(r *patrolReport) {
	var list []Notice
	for _, is := range r.New {
		if is.Check == "decide" || is.Check == "usage" || is.infra() || is.handled {
			continue
		}
		n := Notice{Cat: "patrol", Key: "patrol:" + is.Fingerprint, Text: is.Title}
		if len(is.Evidence) > 0 {
			n.Lines = append(n.Lines, oneLine(is.Evidence[0], 120))
		}
		if is.Action != "" {
			n.Lines = append(n.Lines, oneLine(is.Action, 120))
		}
		list = append(list, n)
	}
	a.notify(list...)
}

// patrolAnalyze：有新异常、patrol_analyze 开着、今天还没满 patrolAnalyzeMax 次、能接活（没暂停、不在冷却/熔断、额度没到 90%）、
// 上一次分析也没在跑时，启动一次 claude -p 只读分析新异常的原因。结果由那个任务结束时写进汇总。
func (a *App) patrolAnalyze(st *State, ps *PatrolState, r *patrolReport) {
	var issues []PatrolIssue
	for _, is := range r.New {
		if is.Anomaly {
			issues = append(issues, is)
		}
	}
	if len(issues) == 0 {
		return
	}
	now := a.Now()
	day := now.Format("2006-01-02")
	if ps.AnalysisDay != day {
		ps.AnalysisDay, ps.AnalysisCount = day, 0
	}
	no := func(why string) { r.Analysis = "没启动 claude -p 分析：" + why }
	lvl, _, _ := st.Usage.level(now, a.Cfg.Model)
	switch {
	case !a.Cfg.PatrolAnalyze:
		no("patrol_analyze=false")
		return
	case ps.AnalysisCount >= patrolAnalyzeMax:
		no(fmt.Sprintf("今天已经分析过 %d 次（每天最多 %d 次）", ps.AnalysisCount, patrolAnalyzeMax))
		return
	case a.Cfg.Paused:
		no("已暂停（paused=true）")
		return
	case now.Before(st.CooldownUntil):
		no("用量冷却中，到 " + whenCN(st.CooldownUntil))
		return
	case now.Before(st.BreakerUntil):
		no("连环失败熔断中，到 " + whenCN(st.BreakerUntil))
		return
	case lvl >= usageStop:
		no(fmt.Sprintf("订阅额度 ≥%d%%", usageStopAt))
		return
	}
	for _, j := range st.running() {
		if j.Kind == kindPatrol {
			no("上一次分析还在跑（任务 " + j.ID + "）")
			return
		}
	}
	launch := a.AnalyzePatrol
	if launch == nil {
		launch = a.startPatrolJob
	}
	j, err := launch(st, patrolPrompt(issues))
	if err != nil {
		no(err.Error())
		return
	}
	ps.AnalysisCount++
	r.AnalysisJobID = j.ID
	r.Analysis = fmt.Sprintf("已启动一次 claude -p 只读分析 %d 个新异常的原因（任务 %s，今天第 %d/%d 次），结果在它结束时写进当天汇总",
		len(issues), j.ID, ps.AnalysisCount, patrolAnalyzeMax)
}

// startPatrolJob 经任务外壳启动分析任务（和夜班、复盘一样记在 state.json，不占并发名额）。
func (a *App) startPatrolJob(st *State, prompt string) (*Job, error) {
	if ok, why := a.checkLogin(); !ok {
		return nil, errors.New(why)
	}
	dir := a.projectsRoot()
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		dir = a.Home
	}
	j := &Job{Kind: kindPatrol, Project: patrolJobName, ProjectDir: dir, Item: "agentq patrol", Title: "agentq patrol 分析"}
	return a.start(st, j, prompt, "")
}

const patrolPromptHead = "你由 agentq 巡逻在无人值守模式下用 claude -p 启动，只做只读分析：不改任何文件、不提交、不推送、不启动或停止任何服务和任务" +
	"（回合一结束进程就退出，不用后台命令和「等通知」）。下面是这次巡逻新发现的异常和证据。逐条查原因（可以读日志、配置、" +
	"git 历史，跑只读命令），每条写一句原因和建议怎么修（能自动派活修的写派什么活，要主人做的写主人做什么）；查不出就写查不出和下一步该看哪里。" +
	"整个回答不超过 250 字，不用标题，不复述证据。\n\n"

func patrolPrompt(issues []PatrolIssue) string {
	var b strings.Builder
	b.WriteString(patrolPromptHead)
	for i, is := range issues {
		fmt.Fprintf(&b, "%d. %s\n", i+1, is.Title)
		for _, e := range is.Evidence {
			fmt.Fprintf(&b, "   证据：%s\n", e)
		}
	}
	return b.String()
}

func (is PatrolIssue) nightlyText() string {
	var ev []string
	for _, e := range is.Evidence {
		ev = append(ev, oneLine(e, 200))
	}
	s := is.Title + "；证据：" + strings.Join(ev, "；")
	if is.Action != "" {
		s += "；处理：" + is.Action
	}
	return s
}

func (r *patrolReport) logLine() string {
	if len(r.New)+len(r.Old) == 0 {
		s := "巡逻：没有发现问题"
		if len(r.Gone) > 0 {
			s += fmt.Sprintf("（已恢复或已处理 %d 个）", len(r.Gone))
		}
		return s + r.tail()
	}
	var t []string
	for _, is := range r.New {
		t = append(t, truncRunes(is.Title, 40))
	}
	s := fmt.Sprintf("巡逻：现有 %d 个问题（新 %d 个，已恢复或已处理 %d 个）", len(r.New)+len(r.Old), len(r.New), len(r.Gone))
	if len(t) > 0 {
		s += "；新的：" + strings.Join(t, "；")
	}
	return s + r.tail()
}

func (r *patrolReport) tail() string {
	s := ""
	if r.Analysis != "" {
		s += "；" + r.Analysis
	}
	if len(r.Notes) > 0 {
		s += "；检查没做成：" + strings.Join(r.Notes, "；")
	}
	return s
}

// print 是 agentq patrol 的输出：新问题、之前报过还在的、已恢复的，各带证据和处理。
func (r *patrolReport) print(w io.Writer, patrolPath, nightly string) {
	fmt.Fprintf(w, "巡逻 %s：现有 %d 个问题（新 %d 个，之前报过 %d 个），已恢复或已处理 %d 个\n",
		r.At.Format("2006-01-02 15:04"), len(r.New)+len(r.Old), len(r.New), len(r.Old), len(r.Gone))
	section := func(head string, list []PatrolIssue, since bool) {
		if len(list) == 0 {
			return
		}
		fmt.Fprintf(w, "\n%s\n", head)
		for _, is := range list {
			extra := ""
			if since {
				extra = "（" + whenCN(is.FirstSeen) + " 首次发现）"
			}
			fmt.Fprintf(w, "  - %s%s\n", is.Title, extra)
			for _, e := range is.Evidence {
				fmt.Fprintf(w, "      证据：%s\n", e)
			}
			if is.Action != "" {
				fmt.Fprintf(w, "      处理：%s\n", is.Action)
			}
		}
	}
	section("新问题（已写进当天汇总；疑似卡住、额度由 tick 写过，不重复写）：", r.New, false)
	section("之前报过、还没消失（不重复写汇总）：", r.Old, true)
	if len(r.Gone) > 0 {
		fmt.Fprintln(w, "\n已恢复或已处理：")
		for _, is := range r.Gone {
			fmt.Fprintf(w, "  - %s：%s（%s 首次发现）\n", is.gone, is.Title, whenCN(is.FirstSeen))
		}
	}
	if len(r.Notes) > 0 {
		fmt.Fprintln(w, "\n这些检查没做成（那一类上次的问题照旧记着）：")
		for _, n := range r.Notes {
			fmt.Fprintf(w, "  - %s\n", n)
		}
	}
	if r.Analysis != "" {
		fmt.Fprintf(w, "\n分析：%s\n", r.Analysis)
	}
	fmt.Fprintf(w, "\n记录：%s；当天汇总：%s\n", patrolPath, nightly)
}

// patrolStatus：status 里的一行，上次巡逻时间和现有问题数（读 patrol.json，不拿锁）。
func (a *App) patrolStatus() string {
	off := ""
	if !a.Cfg.PatrolEnabled {
		off = "tick 不自动巡逻（patrol_enabled=false），"
	}
	ps, err := a.loadPatrol()
	switch {
	case err != nil:
		return off + "读不了 " + a.patrolPath() + "：" + err.Error()
	case ps.LastRun.IsZero():
		return off + "还没巡逻过（agentq patrol 立刻查一遍）"
	case len(ps.Issues) == 0:
		return off + "上次 " + whenCN(ps.LastRun) + "，没有问题"
	}
	return fmt.Sprintf("%s上次 %s，现有 %d 个问题（agentq patrol 看证据）", off, whenCN(ps.LastRun), len(ps.Issues))
}
