package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 2026-10-11 真机 claude 2.1.295 `claude -p /usage` 的输出（后半段贡献说明截短）。
const realUsageText = `You are currently using your subscription to power your Claude Code usage

Current session: 2% used · resets Oct 11 at 1:59pm (Asia/Shanghai)
Current week (all models): 40% used · resets Oct 13 at 8:59pm (Asia/Shanghai)
Current week (Fable): 0% used · resets Oct 13 at 9pm (Asia/Shanghai)

What's contributing to your limits usage?
Last 24h · 3750 requests · 47 sessions
  92% of your usage came from subagent-heavy sessions`

func TestParseUsage(t *testing.T) {
	sh, _ := time.LoadLocation("Asia/Shanghai")
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, sh)
	ws, err := parseUsage(realUsageText, now)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		label, scope string
		used         float64
		reset        time.Time
	}{
		{"session", "", 2, time.Date(2026, 10, 11, 13, 59, 0, 0, sh)},
		{"week (all models)", "", 40, time.Date(2026, 10, 13, 20, 59, 0, 0, sh)},
		{"week (Fable)", "Fable", 0, time.Date(2026, 10, 13, 21, 0, 0, 0, sh)},
	}
	if len(ws) != len(want) {
		t.Fatalf("窗口数 %d：%+v", len(ws), ws)
	}
	for i, w := range want {
		g := ws[i]
		if g.Label != w.label || g.Scope != w.scope || g.Used != w.used || g.ResetsAt == nil || !g.ResetsAt.Equal(w.reset) {
			t.Errorf("第 %d 个窗口 %+v（重置 %v），想要 %+v", i, g, g.ResetsAt, w)
		}
	}

	// 别的写法：带 ANSI 颜色、小数、逗号日期、没有重置时间、重复的行只取第一行
	odd := "\x1b[1mCurrent session: 97.5% used · resets Oct 11, 2:30pm (UTC)\x1b[0m\nCurrent week (Fable): 12% used\nCurrent session: 5% used\n"
	ws, err = parseUsage(odd, now)
	if err != nil || len(ws) != 2 {
		t.Fatalf("%v %+v", err, ws)
	}
	if ws[0].Used != 97.5 || ws[0].ResetsAt == nil || !ws[0].ResetsAt.Equal(time.Date(2026, 10, 11, 14, 30, 0, 0, time.UTC)) {
		t.Errorf("会话窗口：%+v %v", ws[0], ws[0].ResetsAt)
	}
	if ws[1].Scope != "Fable" || ws[1].Used != 12 || ws[1].ResetsAt != nil {
		t.Errorf("没写重置时间的窗口：%+v", ws[1])
	}

	// 读不懂：一个窗口都没有
	for _, s := range []string{"", "You are currently using your subscription to power your Claude Code usage",
		"Current session: lots used", "Usage: 40%", "Error: not logged in"} {
		if ws, err := parseUsage(s, now); err == nil {
			t.Errorf("%q 应当看不懂，得到 %+v", s, ws)
		}
	}
}

func usageAt(used float64, reset *time.Time, scope string) *UsageInfo {
	label := "session"
	if scope != "" {
		label = "week (" + scope + ")"
	}
	return &UsageInfo{Windows: []UsageWindow{{Label: label, Scope: scope, Used: used, ResetsAt: reset}}}
}

func TestUsageLevel(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.Local)
	reset := now.Add(2 * time.Hour)
	cases := []struct {
		name  string
		u     *UsageInfo
		model string
		at    time.Time
		want  int
	}{
		{"没读过", nil, "opus", now, usageUnknown},
		{"读失败", &UsageInfo{Err: "看不懂"}, "opus", now, usageUnknown},
		{"89% 照常", usageAt(89, &reset, ""), "opus", now, usageOK},
		{"89.9% 照常", usageAt(89.9, &reset, ""), "opus", now, usageOK},
		{"90% 停止接活", usageAt(90, &reset, ""), "opus", now, usageStop},
		{"97% 停止接活", usageAt(97, &reset, ""), "opus", now, usageStop},
		{"98% 连 run 也拒绝", usageAt(98, &reset, ""), "opus", now, usageRefuse},
		{"100% 没写重置时间", usageAt(100, nil, ""), "opus", now, usageRefuse},
		{"重置时刻刚到还不算", usageAt(99, &reset, ""), "opus", reset.Add(30 * time.Second), usageRefuse},
		{"过了重置时刻自动恢复", usageAt(99, &reset, ""), "opus", reset.Add(time.Minute), usageOK},
		{"只限 Fable 的窗口不管 opus 任务", usageAt(99, &reset, "Fable"), "opus", now, usageOK},
		{"只限 Fable 的窗口管 Fable 任务", usageAt(99, &reset, "Fable"), "claude-fable-5-1", now, usageRefuse},
		{"Sonnet only 的写法", usageAt(95, &reset, "Sonnet only"), "sonnet", now, usageStop},
	}
	for _, c := range cases {
		if got, _, _ := c.u.level(c.at, c.model); got != c.want {
			t.Errorf("%s：档位 %d，想要 %d", c.name, got, c.want)
		}
	}

	// 挡住时给出最晚的恢复时刻；有窗口没写重置时间就给零值（等下次读）
	later := now.Add(48 * time.Hour)
	u := &UsageInfo{Windows: []UsageWindow{
		{Label: "session", Used: 95, ResetsAt: &reset},
		{Label: "week", Used: 91, ResetsAt: &later},
		{Label: "week (Fable)", Scope: "Fable", Used: 100},
	}}
	lvl, until, why := u.level(now, "opus")
	if lvl != usageStop || !until.Equal(later.Add(usageResetSlack)) || why != "会话 95%、本周 91%" {
		t.Errorf("%d %v %q", lvl, until, why)
	}
	u.Windows[1].ResetsAt = nil
	if _, until, _ := u.level(now, "opus"); !until.IsZero() {
		t.Errorf("有窗口没写重置时间时不该给恢复时刻：%v", until)
	}
	note := usageGateNote(usageRefuse, reset, "会话 99%")
	if !strings.Contains(note, "agentq run 也拒绝") || !strings.Contains(note, whenCN(reset)+" 重置后自动恢复") {
		t.Errorf("说明：%s", note)
	}
}

// 缓存 10 分钟；读完之后有窗口过了重置时刻就提前重读；每天第一次读到、档位变了才写当天汇总。
func TestUsageCacheAndNightly(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.Local)
	text, calls := "", 0
	var readErr error
	a := &App{Home: home, Cfg: defaultConfig(), Now: func() time.Time { return now }, Out: io.Discard,
		UsageText: func() (string, error) { calls++; return text, readErr }}
	st := &State{}
	nightly := func() string {
		b, _ := os.ReadFile(a.nightlyPath(now))
		return string(b)
	}

	text = "Current session: 50% used · resets 2pm\nCurrent week (all models): 95% used · resets 12:30pm\n"
	u := a.usage(st)
	if calls != 1 || st.Usage != u || u.Err != "" || len(u.Windows) != 2 {
		t.Fatalf("第一次应读：calls=%d %+v", calls, u)
	}
	if n := nightly(); strings.Count(n, "额度：") != 1 || !strings.Contains(n, "本周 95%（10-11 周日 12:30 重置）") ||
		!strings.Contains(n, "不启动新任务（正在跑的不动）") {
		t.Errorf("挡住时应写汇总：\n%s", n)
	}

	now = now.Add(9 * time.Minute)
	a.usage(st)
	if calls != 1 {
		t.Errorf("10 分钟内应用缓存：calls=%d", calls)
	}

	// 12:30 重置、12:31 起算恢复：缓存还没到 10 分钟也重读；新读数正常 → 汇总写「已恢复」
	now = time.Date(2026, 10, 11, 12, 31, 0, 0, time.Local)
	text = "Current session: 50% used · resets 2pm\nCurrent week (all models): 1% used · resets Oct 18 at 12:30pm\n"
	if !st.Usage.stale(now) {
		t.Fatal("过了重置时刻应算过期")
	}
	u = a.usage(st)
	if calls != 2 {
		t.Errorf("过了重置时刻应重读：calls=%d", calls)
	}
	if lvl, _, _ := u.level(now, "opus"); lvl != usageOK {
		t.Errorf("重读后应正常：%d", lvl)
	}
	if n := nightly(); strings.Count(n, "额度：") != 2 || !strings.Contains(n, "已恢复，照常接活") {
		t.Errorf("恢复时应写汇总：\n%s", n)
	}

	// 档位没变：只更新缓存，不写汇总
	now = now.Add(10 * time.Minute)
	a.usage(st)
	if calls != 3 || strings.Count(nightly(), "额度：") != 2 {
		t.Errorf("档位没变不该写汇总：calls=%d\n%s", calls, nightly())
	}

	// 读失败：当未知，记原因，写一行
	now = now.Add(10 * time.Minute)
	readErr = errors.New("执行 claude -p /usage 失败：exit status 1")
	u = a.usage(st)
	if lvl, _, _ := u.level(now, "opus"); lvl != usageUnknown || !strings.Contains(u.Err, "exit status 1") {
		t.Errorf("读失败应当未知：%+v", u)
	}
	if n := nightly(); strings.Count(n, "额度：") != 3 || !strings.Contains(n, "未知（执行 claude -p /usage 失败") {
		t.Errorf("变成未知时应写汇总：\n%s", n)
	}
	if !strings.Contains(u.describe(now, "opus"), "照常接活") {
		t.Errorf("describe：%s", u.describe(now, "opus"))
	}

	// 时钟往回拨也重读
	now = now.Add(-time.Hour)
	readErr = nil
	a.usage(st)
	if calls != 5 {
		t.Errorf("时钟往回拨应重读：calls=%d", calls)
	}
}

// 写一份假 claude 的 /usage 输出（JSON 结果，result 是给定文字）。
func (e *env) setUsage(text string) {
	e.t.Helper()
	b, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": text, "local_command": "usage"})
	if err := os.WriteFile(filepath.Join(e.home, "fake-usage.json"), append(b, '\n'), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) usageCalls() []string {
	b, _ := os.ReadFile(filepath.Join(e.home, "fake-usage-calls"))
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// 让 state.json 里的额度读数过期，下次启动前重读。
func (e *env) expireUsage() {
	e.t.Helper()
	st := e.state()
	st.Usage.CheckedAt = st.Usage.CheckedAt.Add(-time.Hour)
	if err := saveState(filepath.Join(e.home, ".agentq", "state.json"), st); err != nil {
		e.t.Fatal(err)
	}
}

// weekReset：假 /usage 里周窗口的重置时刻（两天后的整分钟，UTC 写法）。
var weekReset = time.Now().Add(48 * time.Hour).Truncate(time.Minute)

func usageLine(pct string) string {
	reset := time.Now().Add(3 * time.Hour).UTC().Format("3:04pm")
	week := weekReset.UTC().Format("Jan 2 at 3:04pm")
	return "You are currently using your subscription to power your Claude Code usage\n\n" +
		"Current session: " + pct + "% used · resets " + reset + " (UTC)\n" +
		"Current week (all models): 10% used · resets " + week + " (UTC)\n" +
		"Current week (Fable): 100% used · resets " + week + " (UTC)\n"
}

// 89% 照常启动；涨到 90% 后 tick 不再启动新任务（正在跑的不动），run 仍可用；98% run 也拒绝。
func TestIntegrationUsageGate(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"max_parallel": 5, "per_project": 3})
	mkProject(t, e.root, "alpha", "- [ ] 甲一 —— SLEEP=20\n- [ ] 甲二 —— SLEEP=20\n- [ ] 甲三 —— SLEEP=20\n")
	mkProject(t, e.root, "beta", "- [ ] 乙一 —— SLEEP=20\n")

	e.setUsage(usageLine("89"))
	out := e.mustRun("tick")
	if !strings.Contains(out, "启动 2") {
		t.Fatalf("89%% 应照常启动（只限 Fable 的窗口 100%% 不管 opus 任务）：\n%s", out)
	}
	calls := e.usageCalls()
	if len(calls) != 1 || !strings.Contains(calls[0], "--no-session-persistence") || !strings.Contains(calls[0], "--setting-sources") ||
		!strings.Contains(calls[0], "PWD="+realPath(t, os.TempDir())) || !strings.Contains(calls[0], "agentq-usage-") {
		t.Errorf("一次 tick 只读一次额度，在临时目录里、不读设置、不存会话：%q", calls)
	}
	st := e.state()
	if st.Usage == nil || len(st.Usage.Windows) != 3 || st.Usage.Windows[0].Used != 89 {
		t.Errorf("额度应写进 state：%+v", st.Usage)
	}
	if n := e.nightly(); !strings.Contains(n, "额度：会话 89%") || !strings.Contains(n, "本周（Fable） 100%") || !strings.Contains(n, "不计入") {
		t.Errorf("当天第一次读到应写汇总：\n%s", n)
	}
	if out := e.mustRun("status"); !strings.Contains(out, "调度：正常") || !strings.Contains(out, "额度：会话 89%") ||
		!strings.Contains(out, "本周 10%（"+whenCN(weekReset)+" 重置）") {
		t.Errorf("status 应显示各窗口用量和重置时间：\n%s", out)
	}
	if len(e.usageCalls()) != 1 {
		t.Errorf("缓存没过期时 status 不重读：%q", e.usageCalls())
	}
	// 涨到 90%：不再启动新任务（甲二本来可以接），正在跑的不动；run 仍可用
	e.setUsage(usageLine("90"))
	e.expireUsage()
	out = e.mustRun("tick")
	if !strings.Contains(out, "启动 0") || !strings.Contains(out, "订阅额度快用完（会话 90%，≥90%）") || !strings.Contains(out, "重置后自动恢复") {
		t.Fatalf("90%% 不应启动：\n%s", out)
	}
	if n := countRunning(e.state()); n != 2 {
		t.Errorf("正在跑的不动：运行中 %d", n)
	}
	if n := e.nightly(); !strings.Contains(n, "额度：会话 90%") || !strings.Contains(n, "不启动新任务（正在跑的不动）") {
		t.Errorf("档位变了应写汇总：\n%s", n)
	}
	if out := e.mustRun("status"); !strings.Contains(out, "调度：订阅额度快用完") {
		t.Errorf("status 调度行应标出：\n%s", out)
	}
	out = e.mustRun("run", "alpha")
	if !strings.Contains(out, "已启动任务") || !strings.Contains(out, "agentq run 仍可用") {
		t.Errorf("90%% 时 run 仍可用并提示：\n%s", out)
	}

	// 98%：run 也拒绝
	e.setUsage(usageLine("98"))
	e.expireUsage()
	before := len(e.state().Jobs)
	out, code := e.run("run", "alpha")
	if code == 0 || !strings.Contains(out, "agentq run 也拒绝") {
		t.Errorf("98%% 时 run 应拒绝：%d\n%s", code, out)
	}
	if out := e.mustRun("tick"); !strings.Contains(out, "启动 0") || !strings.Contains(out, "≥98%") {
		t.Errorf("98%% 时 tick 不启动：\n%s", out)
	}
	if n := len(e.state().Jobs); n != before {
		t.Errorf("98%% 时不应有新任务：%d → %d", before, n)
	}
	for _, j := range e.state().running() {
		e.mustRun("stop", j.ID)
	}
}

// 中断的任务等着续跑时额度涨到 ≥90%：续跑也算新任务，不启动；中断的那条仍等着续跑。
func TestIntegrationUsageBlocksResume(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	mkProject(t, e.root, "alpha", "- [ ] 中断一 —— INTERRUPT SLEEP=1\n")
	e.setUsage(usageLine("50"))
	e.mustRun("tick")
	waitExit(t, e.jobByTitle("中断一").PID, 10*time.Second)
	e.setUsage(usageLine("92"))
	e.expireUsage()
	out := e.mustRun("tick")
	if !strings.Contains(out, "本次收尾 1，启动 0") || !strings.Contains(out, "订阅额度快用完（会话 92%") {
		t.Fatalf("额度 ≥90%% 时不续跑：\n%s", out)
	}
	st := e.state()
	if len(st.Jobs) != 1 || st.Jobs[0].Status != statusInterrupted || !st.Jobs[0].ResumePending {
		t.Errorf("中断的任务应仍等着续跑：%+v", st.Jobs[0])
	}
}

// 读数挡着、但读完之后过了重置时刻：缓存没到 10 分钟也重读，照常启动；没过重置时刻就用缓存、不启动。
func TestIntegrationUsageResetRecovers(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	mkProject(t, e.root, "alpha", "- [ ] 甲一 —— SLEEP=1\n")
	statePath := filepath.Join(e.home, ".agentq", "state.json")
	now := time.Now()
	future, past := now.Add(5*time.Minute), now.Add(-2*time.Minute)
	seed := func(reset time.Time) {
		st := &State{Usage: &UsageInfo{CheckedAt: now.Add(-5 * time.Minute), NotedLevel: usageRefuse, NotedDay: now.Format("2006-01-02"),
			Windows: []UsageWindow{{Label: "session", Used: 99, ResetsAt: &reset}}}}
		if err := saveState(statePath, st); err != nil {
			t.Fatal(err)
		}
	}
	e.setUsage(usageLine("3"))

	seed(future)
	if out := e.mustRun("tick"); !strings.Contains(out, "启动 0") || !strings.Contains(out, "会话 99%") {
		t.Fatalf("重置前用缓存、不启动：\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(e.home, "fake-usage-calls")); err == nil {
		t.Errorf("缓存没过期、没到重置时刻，不应重读：%q", e.usageCalls())
	}

	seed(past)
	out := e.mustRun("tick")
	if !strings.Contains(out, "启动 1") {
		t.Fatalf("过了重置时刻应自动恢复：\n%s", out)
	}
	if len(e.usageCalls()) != 1 {
		t.Errorf("过了重置时刻应重读一次：%q", e.usageCalls())
	}
	if n := e.nightly(); !strings.Contains(n, "额度：会话 3%") || !strings.Contains(n, "已恢复，照常接活") {
		t.Errorf("恢复应写汇总：\n%s", n)
	}
	waitExit(t, e.jobByTitle("甲一").PID, 10*time.Second)
}

// /usage 输出读不懂、报错、不是 JSON：当未知照常跑，status 标出。
func TestIntegrationUsageUnknown(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string]string{
		"读不懂":     `{"type":"result","is_error":false,"result":"Usage is temporarily unavailable."}`,
		"报错":      `{"type":"result","is_error":true,"result":"API Error: 500"}`,
		"不是 JSON": `Error: unexpected`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, nil)
			mkProject(t, e.root, "alpha", "- [ ] 甲一 —— SLEEP=1\n")
			if err := os.WriteFile(filepath.Join(e.home, "fake-usage.json"), []byte(raw+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			out := e.mustRun("tick")
			if !strings.Contains(out, "启动 1") || !strings.Contains(out, "额度未知，照常启动") {
				t.Fatalf("读不懂应照常启动：\n%s", out)
			}
			st := e.state()
			if st.Usage == nil || st.Usage.Err == "" {
				t.Errorf("state 应记下读失败：%+v", st.Usage)
			}
			if out := e.mustRun("status"); !strings.Contains(out, "调度：正常") || !strings.Contains(out, "额度：未知（") {
				t.Errorf("status 应标出未知：\n%s", out)
			}
			if n := e.nightly(); !strings.Contains(n, "额度：未知（") {
				t.Errorf("汇总应记下未知：\n%s", n)
			}
			waitExit(t, e.jobByTitle("甲一").PID, 10*time.Second)
		})
	}
}

// 夜班定点叫醒时额度 ≥90% 不启动、汇总记一行；手动执行到 98% 才拒绝。
func TestIntegrationUsageRoutine(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"night_enabled": true})
	e.setUsage(usageLine("95"))
	out, code := e.run("night", "--scheduled")
	if code == 0 || !strings.Contains(out, "夜班没有启动：订阅额度快用完") {
		t.Errorf("95%% 时定点夜班不应启动：%d\n%s", code, out)
	}
	if n := e.nightly(); !strings.Contains(n, "夜班没有启动：订阅额度快用完") {
		t.Errorf("汇总应记一行：\n%s", n)
	}
	e.setUsage(usageLine("98"))
	e.expireUsage()
	if out, code := e.run("night"); code == 0 || !strings.Contains(out, "agentq run 也拒绝") {
		t.Errorf("98%% 时手动夜班也拒绝：%d\n%s", code, out)
	}
	if st := e.state(); len(st.Jobs) != 0 {
		t.Errorf("不应有任务：%+v", st.Jobs)
	}
}
