package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseBacklog(t *testing.T) {
	text := `# 待办

## 待做

从上往下做。

<!-- - [ ] 注释里的模板 —— 不算 -->
<!--
- [ ] 多行注释里的 —— 不算
-->
- [ ] 第一条 —— 要什么；验收：……
- [ ] 第二条（进行中：agent/1010-x 2026-10-10 09:00）
- [ ] 第三条 —— 试过两次（卡住：agent/1010-y 换了两次思路测试仍红）
  - [ ] 缩进的子项不算
- [x] 勾掉的不算
- [ ] 第四条 <!-- 行尾注释 --> —— 照常算

### 小标题不结束待做节
- [ ] 第五条

## 等主人拍板
- [ ] 别的节不算

## 待做（第二个同名节也算）
- [ ] 第六条
## 已完成
- [ ] 不算
`
	items := parseBacklog(text)
	var got []string
	for _, it := range items {
		got = append(got, it.Title)
	}
	want := []string{"第一条", "第二条", "第三条", "第四条 <!-- 行尾注释 -->", "第五条", "第六条"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("标题\n got %q\nwant %q", got, want)
	}
	if !items[1].InProgress || items[1].Eligible() {
		t.Errorf("第二条应为进行中、不可做：%+v", items[1])
	}
	if !items[2].Stuck || items[2].Eligible() {
		t.Errorf("第三条应为卡住、不可做：%+v", items[2])
	}
	if !items[0].Eligible() || items[0].Text != "第一条 —— 要什么；验收：……" {
		t.Errorf("第一条：%+v", items[0])
	}
}

// 卡住只认括号标记和行尾标记；正文提到这个词的条目照常可挑。
func TestStuckMarker(t *testing.T) {
	cases := []struct {
		text  string
		stuck bool
	}{
		{"甲 —— 验收：标了卡住的条目要跳过，正文提到的照常可挑", false},
		{"甲 —— 进程会卡住；验收：卡住时 status 有提示", false},
		{"甲 —— 别把「卡住」当标记", false},
		{"甲 —— 任务不再卡住（进行中：agent/1010-x 10-10 03:00）", false},
		{"甲 —— 查任务卡住", false},
		{"甲 —— 2026 卡住率", false},
		{"甲 —— 验收：（卡住时 status 有提示）", false},
		{"修 [卡住检测](docs/x.md) 的误报", false},
		{"修 claude -p 卡住", false},
		{"甲 —— x **卡住**", false},
		{"甲 —— x（卡住：agent/1010-x 测试两次没过）", true},
		{"甲 —— x（卡住）（进行中：agent/1010-x 10-10 03:00）", true},
		{"甲 —— x (卡住: agent/1010-x)", true},
		{"甲 —— x【卡住】", true},
		{"甲 —— x [卡住] 等会再看", true},
		{"甲 —— x（卡住 agent/1010-x）", true},
		{"甲 —— x；卡住。", true},
		{"甲 —— x —— 卡住", true},
		{"甲 —— x；卡住（进行中：agent/t-stuck 10-10 03:00）", true},
		{"卡住", true},
	}
	for _, c := range cases {
		items := parseBacklog("## 待做\n- [ ] " + c.text + "\n")
		if len(items) != 1 {
			t.Fatalf("%q：解析出 %d 条", c.text, len(items))
		}
		if items[0].Stuck != c.stuck {
			t.Errorf("%q：Stuck=%v，应为 %v", c.text, items[0].Stuck, c.stuck)
		}
		if !items[0].InProgress && items[0].Eligible() == c.stuck {
			t.Errorf("%q：Eligible=%v", c.text, items[0].Eligible())
		}
	}
}

// 等条件只认括号紧挨「等条件」再接冒号的标记；正文顺带提到这个词不算。标题里不带标记。
func TestWaitCondition(t *testing.T) {
	cases := []struct {
		text, cond string
		ok         bool
	}{
		{"v4 发布 —— 推荐发布（等条件：前面 7 条都合并）", "前面 7 条都合并", true},
		{"v4 发布（等条件：前面 7 条都合并） —— 推荐发布", "前面 7 条都合并", true},
		{"v4 发布 —— x (等条件: 主人验收完)", "主人验收完", true},
		{"v4 发布 —— x（ 等条件 ： a ）（进行中：agent/x 10-11 09:00）", "a", true},
		{"推送不催 —— 约定固定标记等条件：带这个标记的跳过", "", false},
		{"推送不催 —— 正文里提到「等条件」标记的用法", "", false},
		{"甲 —— 等条件满足后再推", "", false},
		{"甲 —— x（等条件）", "", false},
		{"甲 —— x（等条件满足后再说）", "", false},
	}
	for _, c := range cases {
		cond, ok := waitCondition(c.text)
		if ok != c.ok || cond != c.cond {
			t.Errorf("%q：得到 (%q, %v)，应为 (%q, %v)", c.text, cond, ok, c.cond, c.ok)
		}
	}
	if got := itemTitle("v4 发布（等条件：前面 7 条都合并） —— 推荐发布"); got != "v4 发布" {
		t.Errorf("标题应去掉等条件标记：%q", got)
	}
}

// 给人看的时间带程序算好的星期。
func TestWhenCN(t *testing.T) {
	cases := map[string]time.Time{
		"10-13 周二 21:00": time.Date(2026, 10, 13, 21, 0, 0, 0, time.Local),
		"10-11 周日 09:05": time.Date(2026, 10, 11, 9, 5, 0, 0, time.Local),
		"01-01 周五 00:00": time.Date(2027, 1, 1, 0, 0, 0, 0, time.Local),
	}
	for want, at := range cases {
		if got := whenCN(at); got != want {
			t.Errorf("%v：%q，应为 %q", at, got, want)
		}
	}
	reset := time.Date(2026, 10, 13, 21, 0, 0, 0, time.Local)
	u := &UsageInfo{Windows: []UsageWindow{{Label: "week", Used: 95, ResetsAt: &reset}}}
	if got := u.describe(reset.Add(-48*time.Hour), "opus"); !strings.Contains(got, "（10-13 周二 21:00 重置）") {
		t.Errorf("额度说明里的重置时间应带星期：%q", got)
	}
}

func TestParseBacklogEdgeCases(t *testing.T) {
	cases := map[string]string{
		"空文件":       "",
		"没有待做节":     "# 待办\n\n## 已完成\n- [ ] 不算\n",
		"待做节为空":     "## 待做\n\n## 已完成\n- [ ] x\n",
		"注释没闭合吞掉后面": "## 待做\n<!--\n- [ ] 不算\n",
	}
	for name, text := range cases {
		if items := parseBacklog(text); len(items) != 0 {
			t.Errorf("%s：应没有条目，得到 %+v", name, items)
		}
	}
	// CRLF 换行
	items := parseBacklog("## 待做\r\n- [ ] 甲 —— x\r\n")
	if len(items) != 1 || items[0].Title != "甲" {
		t.Errorf("CRLF：%+v", items)
	}
}

func TestFingerprintIgnoresInProgress(t *testing.T) {
	a := fingerprint("- [ ] 第一条 —— 验收：x")
	b := fingerprint("- [ ] 第一条 —— 验收：x（进行中：agent/1010-a 2026-10-10 10:00）")
	c := fingerprint("- [ ] 第一条 —— 验收：y")
	if a != b {
		t.Errorf("去掉进行中标记后指纹应相同：%s %s", a, b)
	}
	if a == c {
		t.Errorf("原文不同指纹应不同")
	}
	if len(a) != 40 {
		t.Errorf("应为 sha1 十六进制：%s", a)
	}
}

func ptime(t time.Time) *time.Time { return &t }

func TestRetryBlocked(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.Local)
	retry := 12 * time.Hour
	st := &State{Jobs: []*Job{
		{Fingerprint: "a", Status: statusDone, EndedAt: ptime(now.Add(-13 * time.Hour))},
		{Fingerprint: "b", Status: statusFailed, EndedAt: ptime(now.Add(-13 * time.Hour))},
		{Fingerprint: "b", Status: statusDone, EndedAt: ptime(now.Add(-1 * time.Hour))},
		{Fingerprint: "c", Status: statusFailed, EndedAt: ptime(now.Add(-1 * time.Hour)), LimitHit: true},
		{Fingerprint: "d", Status: statusRunning},
		{Fingerprint: "e", Status: statusTimeout, EndedAt: ptime(now.Add(-11 * time.Hour))},
	}}
	cases := map[string]bool{
		"a": false, // 上次结束已超过 12h
		"b": true,  // 最近一次只过了 1h
		"c": false, // 撞用量上限不算一次尝试
		"d": false, // 只有运行中的，不算结束
		"e": true,  // 超时也算试过
		"z": false, // 从没跑过
	}
	for fp, want := range cases {
		if got := retryBlocked(st, fp, now, retry); got != want {
			t.Errorf("指纹 %s：得到 %v，应为 %v", fp, got, want)
		}
	}
}

func TestCapacity(t *testing.T) {
	cfg := defaultConfig() // max_parallel 2, per_project 1
	st := &State{Jobs: []*Job{
		{Project: "a", Status: statusRunning},
		{Project: "b", Status: statusDone},
	}}
	if ok, _ := capacity(st, cfg, "a"); ok {
		t.Error("项目 a 已有 1 个在跑，不应再启动")
	}
	if ok, _ := capacity(st, cfg, "b"); !ok {
		t.Error("项目 b 没有在跑的，全局 1/2，应能启动")
	}
	st.Jobs = append(st.Jobs, &Job{Project: "c", Status: statusRunning})
	if ok, why := capacity(st, cfg, "b"); ok || !strings.Contains(why, "max_parallel") {
		t.Errorf("全局 2/2 应已满：%v %s", ok, why)
	}
	cfg.PerProject = 2
	cfg.MaxParallel = 5
	if ok, _ := capacity(st, cfg, "a"); !ok {
		t.Error("per_project=2 时项目 a 还能再启动一个")
	}
	// 夜班、复盘不占名额
	cfg = defaultConfig()
	st.Jobs = []*Job{{Project: "a", Status: statusRunning}, {Project: "夜班", Kind: kindNight, Status: statusRunning},
		{Project: "复盘", Kind: kindReview, Status: statusRunning}}
	if ok, why := capacity(st, cfg, "b"); !ok {
		t.Errorf("夜班、复盘不应占全局名额：%s", why)
	}
}

func TestIsUsageLimit(t *testing.T) {
	yes := []string{"Claude AI usage limit reached|1760000000", "API Error: Rate limit exceeded", "5-hour LIMIT REACHED",
		"You've hit your limit · resets 5pm", "You've hit your monthly spend limit"}
	no := []string{"", "合并了，提交 abc1234", "limits of the system", "hit your target, no limits"}
	for _, s := range yes {
		if !isUsageLimit(s) {
			t.Errorf("应识别为用量上限：%q", s)
		}
	}
	for _, s := range no {
		if isUsageLimit(s) {
			t.Errorf("不应识别为用量上限：%q", s)
		}
	}
}

func TestSummaryParagraph(t *testing.T) {
	cases := map[string]string{
		"做了很多事。\n\n合并了：是\n提交 abc1234\n\n":        "合并了：是 提交 abc1234",
		"做完了。\n\n已合并，提交 abc1234。\n\n另外顺手记了一条待做。": "已合并，提交 abc1234。",
		"做了一半。\n\n继续等通知。":                        "继续等通知。",
		// 前面段落写了提交号、最后是顺带提到合并的审查结论：取带提交号的段落（任务 1010-145932-sing-box-reality-manager）
		"已经合并，提交号 `95e8a03`（PR #5）。\n\n**做了什么**\n- 加了 7 种协议\n\n第二轮审查结论是可以合并。\n\n**怎么验收**\n1. 看 PR #5":                   "已经合并，提交号 `95e8a03`（PR #5）。",
		"做完了。\n\n合并提交 bc930ae（1a65900 + bc930ae），只改 README。\n\n审查人说可以合并。":                                               "合并提交 bc930ae（1a65900 + bc930ae），只改 README。",
		"做完了。\n\n**合并提交**：`abc1234`\n\n审查结论是可以合并。":                                                                      "**合并提交**：`abc1234`",
		"Re-checked.\n\n**Merge result:** merged into main, merge commit `9366fa5`.\n\n**How to check:** open the app.": "**Merge result:** merged into main, merge commit `9366fa5`.",
		// 不是固定写法的号（大写、太短、后面还连着字母、没有「合并提交/提交号」）不算，照旧取最后一个说到合并的段落
		"提交号 3CCBA86E 跑了测试。\n\n提交号 abc12 太短。\n\n合并提交 abc1234x 不对。\n\n分支 abc1234 留着。\n\n没合并：测试没过。": "没合并：测试没过。",
	}
	for in, want := range cases {
		if got := summaryParagraph(in); got != want {
			t.Errorf("%q 得到 %q，应为 %q", in, got, want)
		}
	}
}

func TestDoneMergeCommit(t *testing.T) {
	text := "# 待办\n\n## 待做\n\n- [ ] 甲 —— 合并提交 1111111 写在待做里不算\n\n## 已完成\n" +
		"<!-- - [x] 标题 —— 合并提交 abc1234；验收：… -->\n" +
		"- [x] 乙 —— 2026-10-10 合并，提交 95e8a03（PR #5）\n" +
		"- [x] 丙 —— 合并提交 bc930ae（1a65900 + bc930ae）；验收：看 README\n" +
		"- [X] 丁 —— 合并提交：`d5229a0`；验收：…\n" +
		"- [x] 戊 —— 提到合并提交号要写成 abc 的格式\n" +
		"- [x] 丙 —— 合并提交 2222222；同名的旧条目\n" +
		"\n## 以后再说\n\n- [x] 己 —— 合并提交 3333333\n"
	cases := map[string]string{"甲": "", "乙": "", "丙": "bc930ae", "丁": "d5229a0", "戊": "", "己": "", "标题": "", "没有": ""}
	for title, want := range cases {
		if got := doneMergeCommit(text, title); got != want {
			t.Errorf("%s 得到 %q，应为 %q", title, got, want)
		}
	}
}

func TestBranchNote(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v：%v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	git("commit", "-q", "--allow-empty", "-m", "a")
	git("branch", "agent/empty")
	git("checkout", "-q", "-b", "agent/ahead")
	git("commit", "-q", "--allow-empty", "-m", "b")
	git("checkout", "-q", "main")
	cases := map[string]string{
		"甲（进行中：agent/ahead 2026-10-10 03:00）": "（分支 agent/ahead 比 main 多 1 个提交，没合并）",
		"甲（进行中：agent/empty 2026-10-10 03:00）": "（分支 agent/empty 没有 main 之外的提交）",
		"甲（进行中：agent/gone 2026-10-10 03:00）":  "（分支 agent/gone 不存在）",
		"甲（进行中：-x 2026-10-10）":                "",
		"甲（进行中）":                              "",
	}
	for text, want := range cases {
		if got := branchNote(dir, text); got != want {
			t.Errorf("%q 得到 %q，应为 %q", text, got, want)
		}
	}
}

func TestConfigDefaultsAndPartialFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Timeout.Duration != 4*time.Hour || cfg.MaxParallel != 2 || cfg.Model != "opus" {
		t.Errorf("默认值不对：%+v", cfg)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), `"timeout": "4h"`) || !strings.Contains(string(b), `"projects_root": "~/projects"`) {
		t.Errorf("应写出默认配置，得到：\n%s", b)
	}
	os.WriteFile(path, []byte(`{"max_parallel": 3, "timeout": "30m", "paused": true}`), 0o644)
	cfg, err = loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	// 旧配置文件没有 tick_after_job：沿用默认的开；没有夜班、复盘的键：默认关，超时 6h/4h
	if cfg.MaxParallel != 3 || cfg.Timeout.Duration != 30*time.Minute || !cfg.Paused || cfg.PerProject != 1 || !cfg.TickAfterJob {
		t.Errorf("部分配置应覆盖对应键、其余用默认：%+v", cfg)
	}
	if cfg.NightEnabled || cfg.ReviewEnabled || cfg.NightTimeout.Duration != 6*time.Hour || cfg.ReviewTimeout.Duration != 4*time.Hour {
		t.Errorf("夜班、复盘默认应关闭、超时 6h/4h：%+v", cfg)
	}
	os.WriteFile(path, []byte(`{"max_parallel": 0}`), 0o644)
	if _, err := loadConfig(path); err == nil {
		t.Error("max_parallel=0 应报错")
	}
	os.WriteFile(path, []byte(`{"timeout": 5}`), 0o644)
	if _, err := loadConfig(path); err == nil {
		t.Error("时长写成数字应报错")
	}
}

func TestExpandHome(t *testing.T) {
	if got := expandHome("/h", "~/projects"); got != "/h/projects" {
		t.Error(got)
	}
	if got := expandHome("/h", "/abs"); got != "/abs" {
		t.Error(got)
	}
}

func TestStatePruneAndFind(t *testing.T) {
	now := time.Now()
	st := &State{Jobs: []*Job{
		{ID: "0101-000000-old", Status: statusDone, EndedAt: ptime(now.Add(-31 * 24 * time.Hour))},
		{ID: "1010-120000-a", Status: statusDone, EndedAt: ptime(now.Add(-time.Hour))},
		{ID: "1010-120000-ab", Status: statusRunning},
	}}
	st.prune(now)
	if len(st.Jobs) != 2 {
		t.Fatalf("应只删掉 30 天前结束的：%d", len(st.Jobs))
	}
	if j, err := st.find("1010-120000-a"); err != nil || j.ID != "1010-120000-a" {
		t.Errorf("完整 id 应精确匹配：%v", err)
	}
	if _, err := st.find("1010-12"); err == nil {
		t.Error("有歧义的前缀应报错")
	}
	if j, err := st.find("1010-120000-ab"); err != nil || j.Status != statusRunning {
		t.Errorf("%v", err)
	}
}

// 状态文件内容坏了：tick 备份坏文件、按 meta.json 重建（读不出的 meta.json 跳过并报出来），
// 用量冷却按撞上限任务的结束时间估回来；那次不启动任务。
func TestTickRecoversCorruptState(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	mkProject(t, root, "alpha", "- [ ] 一条活 —— x\n")
	var out bytes.Buffer
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.Local)
	a := &App{Home: home, Now: func() time.Time { return now }, Out: &out}
	os.MkdirAll(a.dataDir(), 0o755)
	a.Cfg = defaultConfig()
	a.Cfg.ProjectsRoot = root
	a.Cfg.ClaudePath = "/nonexistent/claude"
	ended := now.Add(-20 * time.Minute)
	for _, j := range []*Job{
		{ID: "1010-114000-alpha", Project: "alpha", Status: statusFailed, StartedAt: ended.Add(-time.Minute), EndedAt: &ended, LimitHit: true},
		{ID: "1010-100000-alpha", Project: "alpha", Status: statusDone, StartedAt: now.Add(-2 * time.Hour), EndedAt: &ended},
	} {
		b, _ := json.Marshal(j)
		os.MkdirAll(a.jobDir(j.ID), 0o755)
		os.WriteFile(filepath.Join(a.jobDir(j.ID), "meta.json"), b, 0o644)
	}
	os.MkdirAll(a.jobDir("1010-110000-alpha"), 0o755)
	os.WriteFile(filepath.Join(a.jobDir("1010-110000-alpha"), "meta.json"), []byte("{"), 0o644)
	bad := []byte(`{"jobs": [ {"id": `)
	os.WriteFile(a.statePath(), bad, 0o644)

	if err := a.tick(""); err != nil {
		t.Fatalf("内容坏了应重建而不是报错：%v\n%s", err, out.String())
	}
	if b, _ := os.ReadFile(a.statePath() + ".bad-20261010-120000"); !bytes.Equal(b, bad) {
		t.Errorf("应把坏文件原样备份：%q", b)
	}
	st, err := loadState(a.statePath())
	if err != nil {
		t.Fatalf("重建后的状态应已保存：%v", err)
	}
	if len(st.Jobs) != 2 || st.Jobs[0].ID != "1010-100000-alpha" || st.Jobs[1].ID != "1010-114000-alpha" {
		t.Errorf("应按启动时间重建 2 个任务：%+v", st.Jobs)
	}
	if want := ended.Add(a.Cfg.LimitCooldown.Duration); !st.CooldownUntil.Equal(want) {
		t.Errorf("用量冷却应估到 %v，实际 %v", want, st.CooldownUntil)
	}
	n, _ := os.ReadFile(a.nightlyPath(now))
	if !strings.Contains(string(n), "状态文件损坏：") || !strings.Contains(string(n), "1 个任务的 meta.json 读不出，没算进去：1010-110000-alpha") {
		t.Errorf("当天汇总应报警：\n%s", n)
	}
	if entries, _ := os.ReadDir(filepath.Join(a.dataDir(), "jobs")); len(entries) != 3 {
		t.Errorf("不应创建任务：%v", entries)
	}
}

// 状态文件读不了（不是内容坏了，比如没权限）：仍当作未知，不收尾不启动、不备份不覆盖，以非零状态退出。
func TestTickRefusesOnUnreadableState(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root 不受文件权限限制")
	}
	home := t.TempDir()
	root := t.TempDir()
	mkProject(t, root, "alpha", "- [ ] 一条活 —— x\n")
	var out bytes.Buffer
	a := &App{Home: home, Now: time.Now, Out: &out}
	os.MkdirAll(a.dataDir(), 0o755)
	a.Cfg = defaultConfig()
	a.Cfg.ProjectsRoot = root
	a.Cfg.ClaudePath = "/nonexistent/claude"
	os.WriteFile(a.statePath(), []byte(`{"jobs":[]}`), 0o644)
	os.Chmod(a.statePath(), 0o000)
	defer os.Chmod(a.statePath(), 0o644)
	if err := a.tick(""); err == nil {
		t.Error("状态文件读不了 tick 应返回错误")
	}
	if !strings.Contains(out.String(), "当作未知") {
		t.Errorf("应记日志说明：%s", out.String())
	}
	if baks, _ := filepath.Glob(a.statePath() + ".bad-*"); len(baks) != 0 {
		t.Errorf("读不了不是损坏，不应备份：%v", baks)
	}
	if entries, _ := os.ReadDir(filepath.Join(a.dataDir(), "jobs")); len(entries) != 0 {
		t.Errorf("不应创建任务：%v", entries)
	}
}

func TestBuildPromptAndArgs(t *testing.T) {
	cfg := defaultConfig()
	p := buildPrompt("/p/alpha", "条目 —— 验收")
	if !strings.Contains(p, "把 /p/alpha 的 docs/BACKLOG.md「待做」里的这一条做到合并：条目 —— 验收。") ||
		!strings.HasPrefix(p, "你在无人值守模式下工作") || !strings.Contains(p, "~/.claude/skills/agent-task/SKILL.md") {
		t.Errorf("prompt 不对：%s", p)
	}
	args := claudeArgs(cfg, p, "alpha", "一二三四五六七八九十一二三四五六七八九十多出来", "", "")
	joined := strings.Join(args, " ")
	if args[0] != "-p" {
		t.Errorf("新任务不应带 -r：%s", joined)
	}
	if r := claudeArgs(cfg, p, "alpha", "t", "", "sid-new"); r[0] != "--session-id" || r[1] != "sid-new" || r[2] != "-p" {
		t.Errorf("新任务应以 --session-id <会话> 开头：%v", r[:3])
	}
	if r := claudeArgs(cfg, p, "alpha", "t", "sid-1", "sid-new"); r[0] != "-r" || r[1] != "sid-1" || r[2] != "-p" {
		t.Errorf("续跑应以 -r <会话> 开头、不带 --session-id：%v", r[:3])
	}
	for _, want := range []string{"--model opus", "--effort high", "--permission-mode bypassPermissions", "--output-format json",
		"--disallowed-tools Monitor", "--name agentq alpha 一二三四五六七八九十一二三四五六七八九十"} {
		if !strings.Contains(joined, want) {
			t.Errorf("参数缺 %q：%s", want, joined)
		}
	}
	if strings.Contains(joined, "多出来") {
		t.Error("--name 里标题应截到 20 字")
	}
}

func TestChildEnvWhitelist(t *testing.T) {
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "x")
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:1")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-x")
	t.Setenv("SOME_SECRET", "s")
	t.Setenv("AGENTQ_KEEP_ME", "1")
	t.Setenv("CLAUDE_CONFIG_DIR", "/cfg")
	t.Setenv("LC_CTYPE", "UTF-8")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:7890")
	env := "\n" + strings.Join(childEnv(), "\n")
	for _, bad := range []string{"CLAUDECODE=", "CLAUDE_CODE_SESSION_ID=", "ANTHROPIC_BASE_URL=", "ANTHROPIC_API_KEY=", "SOME_SECRET="} {
		if strings.Contains(env, "\n"+bad) {
			t.Errorf("应去掉 %s", bad)
		}
	}
	for _, want := range []string{"AGENTQ_KEEP_ME=1", "CLAUDE_CONFIG_DIR=/cfg", "LC_CTYPE=UTF-8", "HTTPS_PROXY=http://127.0.0.1:7890", "HOME=", "PATH="} {
		if !strings.Contains(env, "\n"+want) {
			t.Errorf("应保留 %s", want)
		}
	}
}

func mkProject(t *testing.T, root, name, todo string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(dir, "docs"), 0o755)
	backlog := "# 待办\n\n## 待做\n\n<!-- - [ ] 模板 -->\n" + todo + "\n## 已完成\n"
	if err := os.WriteFile(filepath.Join(dir, "docs", "BACKLOG.md"), []byte(backlog), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestScanProjects(t *testing.T) {
	root := t.TempDir()
	mkProject(t, root, "b", "")
	mkProject(t, root, "a", "")
	mkProject(t, root, ".hidden", "")
	os.MkdirAll(filepath.Join(root, "notgit"), 0o755)
	wt := filepath.Join(root, "worktree")
	os.MkdirAll(wt, 0o755)
	os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: /x\n"), 0o644)
	os.WriteFile(filepath.Join(root, "file.txt"), nil, 0o644)
	ps, err := scanProjects(root)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range ps {
		names = append(names, p.Name)
	}
	if strings.Join(names, ",") != "a,b" {
		t.Errorf("得到 %v", names)
	}
}

func TestProcAlive(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "5")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	if !procAlive(pid, "sleep 5") {
		t.Error("命令行含标记的活进程应判为活着")
	}
	if procAlive(pid, nameMarker("别的项目")) {
		t.Error("命令行不含标记（pid 被复用）应判为不在")
	}
	if procAlive(0, "x") || procAlive(-1, "x") {
		t.Error("非法 pid 应判为不在")
	}
	cmd.Process.Kill()
	// 不 Wait：子进程成为僵尸，kill(pid,0) 仍成功，但应判为不在
	deadline := time.Now().Add(3 * time.Second)
	for procAlive(pid, "sleep 5") && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if procAlive(pid, "sleep 5") {
		t.Error("僵尸进程应判为不在")
	}
	cmd.Wait()
}

func TestPrivateConfigAndRuntimePermissions(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".agentq")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	a, err := newApp(home, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dir)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("runtime directory permission: %v %v", fi, err)
	}
	if err := saveConfig(a.configPath(), a.Cfg); err != nil {
		t.Fatal(err)
	}
	fi, err = os.Stat(a.configPath())
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("config permission: %v %v", fi, err)
	}
}
