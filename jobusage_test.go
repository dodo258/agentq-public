package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 照真实 result.json（任务 sample-task）删减：顶层 usage 只算主会话，modelUsage 含子代理。
const resultWithUsage = `{"type":"result","is_error":false,"result":"已合并","session_id":"s",
"total_cost_usd":4.6071268000000005,
"usage":{"input_tokens":92,"cache_creation_input_tokens":157333,"cache_read_input_tokens":5681707,"output_tokens":54650},
"modelUsage":{"claude-opus-5-5":{"inputTokens":122,"outputTokens":69768,"cacheReadInputTokens":6388024,"cacheCreationInputTokens":292335,"costUSD":4.6071268000000005}}}`

func TestParseJobUsage(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want *JobUsage
	}{
		{"有 modelUsage 用它（含子代理）", resultWithUsage, &JobUsage{Input: 122, Output: 69768, CacheRead: 6388024, CacheWrite: 292335, CostUSD: 4.6071268000000005}},
		{"多个模型相加", `{"total_cost_usd":3,"modelUsage":{"a":{"inputTokens":1,"outputTokens":2,"cacheReadInputTokens":3,"cacheCreationInputTokens":4,"costUSD":1},"b":{"inputTokens":10,"outputTokens":20,"cacheReadInputTokens":30,"cacheCreationInputTokens":40,"costUSD":2}}}`,
			&JobUsage{Input: 11, Output: 22, CacheRead: 33, CacheWrite: 44, CostUSD: 3}},
		{"只有顶层 usage", `{"usage":{"input_tokens":5,"output_tokens":6,"cache_read_input_tokens":7,"cache_creation_input_tokens":8},"total_cost_usd":0.5}`,
			&JobUsage{Input: 5, Output: 6, CacheRead: 7, CacheWrite: 8, CostUSD: 0.5}},
		{"没有 total_cost_usd 用各模型之和", `{"modelUsage":{"a":{"inputTokens":1,"costUSD":0.25},"b":{"inputTokens":1,"costUSD":0.5}}}`,
			&JobUsage{Input: 2, CostUSD: 0.75}},
		{"缺 usage", `{"type":"result","is_error":false,"result":"做完了","session_id":"s"}`, nil},
		{"空文件", ``, nil},
		{"格式不对", `boom`, nil},
	}
	for _, c := range cases {
		got := parseJobUsage([]byte(c.raw))
		if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Errorf("%s：得到 %+v，应为 %+v", c.name, got, c.want)
		}
	}
}

func TestFmtTokens(t *testing.T) {
	for n, want := range map[int64]string{0: "0", 999: "999", 1234: "1.2K", 6388024: "6.39M", 1_234_567_890: "1.23B"} {
		if got := fmtTokens(n); got != want {
			t.Errorf("fmtTokens(%d) = %q，应为 %q", n, got, want)
		}
	}
}

func TestCostFooter(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	day := time.Date(2026, 10, 11, 12, 0, 0, 0, loc)
	at := func(h int) *time.Time { t := time.Date(2026, 10, 11, h, 0, 0, 0, loc); return &t }
	st := &State{Jobs: []*Job{
		{ID: "j1", Project: "a", Title: "便宜", EndedAt: at(1), Usage: &JobUsage{Input: 100, CacheRead: 800, CacheWrite: 100, CostUSD: 1}},
		{ID: "j2", Project: "b", Title: "最贵", EndedAt: at(2), Usage: &JobUsage{Input: 0, CacheRead: 1000, CostUSD: 9}},
		{ID: "j3", Project: "c", Title: "次贵", EndedAt: at(3), Usage: &JobUsage{CacheRead: 100, CacheWrite: 900, Output: 5, CostUSD: 5}},
		{ID: "j4", Project: "d", Title: "第三", EndedAt: at(4), Usage: &JobUsage{CacheRead: 1000, CostUSD: 2}},
		{ID: "j5", Project: "e", Title: "缺用量", EndedAt: at(5)},
		{ID: "j6", Project: "f", Title: "昨天的", EndedAt: ptime(day.Add(-24 * time.Hour)), Usage: &JobUsage{CacheRead: 1, CostUSD: 99}},
		{ID: "j7", Project: "g", Title: "还在跑", Status: statusRunning},
	}}
	a := &App{}
	f := a.costFooter(st, day)
	// 命中率 = (800+1000+100+1000) / (那四项 + 缓存写 100+900 + 输入 100) = 2900/4000
	for _, want := range []string{costBegin, "合计 4 个任务", "约 $17.00", "缓存命中率 72.5%",
		"1. b「最贵」token 1.0K，约 $9.00；j2", "2. c「次贵」token 1.0K，约 $5.00；j3", "3. d「第三」token 1.0K，约 $2.00；j4", costEnd} {
		if !strings.Contains(f, want) {
			t.Errorf("花费小结缺 %q：\n%s", want, f)
		}
	}
	for _, bad := range []string{"便宜", "昨天的", "缺用量", "4. "} {
		if strings.Contains(f, bad) {
			t.Errorf("花费小结不应有 %q：\n%s", bad, f)
		}
	}
	if f := a.costFooter(&State{Jobs: []*Job{{ID: "x", EndedAt: at(1)}}}, day); f != "" {
		t.Errorf("当天没有记了用量的任务时不写小结：%q", f)
	}
}

// 小结一直在汇总末尾：新行插在它前面，保护脚本用 >> 追加在它后面的行也挪到它前面，重写小结不丢行。
func TestNightlyCostFooterStaysLast(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 10, 11, 9, 30, 0, 0, time.Local)
	a := &App{Home: home, Now: func() time.Time { return now }, Out: &bytes.Buffer{}}
	end := now
	st := &State{Jobs: []*Job{{ID: "j1", Project: "a", Title: "甲", EndedAt: &end, Usage: &JobUsage{CacheRead: 9, Input: 1, CostUSD: 1}}}}
	path := a.nightlyPath(now)

	a.appendNightlyLine(now, "第一行")
	a.updateCostFooter(st, now)
	a.appendNightlyLine(now, "第二行")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("- 09:31 已自动回滚：保护脚本追加的\n")
	f.Close()
	st.Jobs = append(st.Jobs, &Job{ID: "j2", Project: "b", Title: "乙", EndedAt: &end, Usage: &JobUsage{CacheRead: 0, Input: 10, CostUSD: 3}})
	a.updateCostFooter(st, now)
	a.appendNightlyLine(now, "第三行")

	b, _ := os.ReadFile(path)
	s := string(b)
	if strings.Count(s, costBegin) != 1 || strings.Count(s, "# agentq 任务汇总") != 1 {
		t.Fatalf("应只有一个标题、一个小结：\n%s", s)
	}
	want := []string{"# agentq 任务汇总 2026-10-11", "- 09:30 第一行", "- 09:30 第二行", "已自动回滚：保护脚本追加的", "- 09:30 第三行", costBegin, "合计 2 个任务", "1. b「乙」", "2. a「甲」", costEnd}
	pos := 0
	for _, w := range want {
		i := strings.Index(s[pos:], w)
		if i < 0 {
			t.Fatalf("缺 %q 或顺序不对：\n%s", w, s)
		}
		pos += i + len(w)
	}
	if strings.TrimSpace(s[pos:]) != "" {
		t.Errorf("小结后面不应还有内容：\n%s", s)
	}
	if strings.Contains(s, "\n\n\n") {
		t.Errorf("不应有连续空行：\n%s", s)
	}
}

// 续跑（claude -r）的 result.json 里 modelUsage、total_cost_usd 是整个会话累计的：减掉上一个有结果的同会话任务。
func TestReadJobUsageResume(t *testing.T) {
	a := &App{Home: t.TempDir()}
	write := func(id, raw string) {
		d := a.jobDir(id)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(d+"/result.json", []byte(raw), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	res := func(in, out, cr, cw int, cost float64) string {
		return fmt.Sprintf(`{"total_cost_usd":%g,"modelUsage":{"m":{"inputTokens":%d,"outputTokens":%d,"cacheReadInputTokens":%d,"cacheCreationInputTokens":%d}}}`, cost, in, out, cr, cw)
	}
	// 数字照实测：第一次 2/418/10873/24510 $0.00521993，-r 续跑后 4/422/46256/25149 $0.00570376
	write("orig", res(2, 418, 10873, 24510, 0.00521993))
	write("r1", `{"type":"result","is_error":true,"result":"崩了"}`) // 续跑一次没留下用量
	write("r2", res(4, 422, 46256, 25149, 0.00570376))
	write("odd", res(1, 1, 1, 1, 0.001)) // 比上一次还小：不是累计的，照原样记
	st := &State{Jobs: []*Job{{ID: "orig"}, {ID: "r1", ResumeOf: "orig"}, {ID: "r2", ResumeOf: "r1"}, {ID: "odd", ResumeOf: "orig"}, {ID: "gone", ResumeOf: "nosuch"}}}
	write("gone", res(5, 5, 5, 5, 0.5))

	j := st.Jobs[2]
	a.readJobUsage(st, j)
	if j.Usage == nil || j.Usage.Input != 2 || j.Usage.Output != 4 || j.Usage.CacheRead != 35383 || j.Usage.CacheWrite != 639 ||
		fmt.Sprintf("%.8f", j.Usage.CostUSD) != "0.00048383" {
		t.Errorf("续跑应只记这次进程的：%+v", j.Usage)
	}
	for id, want := range map[string]JobUsage{"orig": {2, 418, 10873, 24510, 0.00521993}, "odd": {1, 1, 1, 1, 0.001}, "gone": {5, 5, 5, 5, 0.5}} {
		j, _ := st.find(id)
		a.readJobUsage(st, j)
		if j.Usage == nil || *j.Usage != want {
			t.Errorf("%s：得到 %+v，应为 %+v", id, j.Usage, want)
		}
	}
	a.readJobUsage(st, st.Jobs[1])
	if j := st.Jobs[1]; j.Usage != nil {
		t.Errorf("没有用量的结果不记：%+v", j.Usage)
	}
}

func TestSplitCostFooterEdges(t *testing.T) {
	// 被改坏只剩开始标记：原样当正文
	s := "# agentq 任务汇总 2026-10-11\n\n- 01:00 甲\n" + costBegin + "\n- 02:00 乙\n"
	if body, f := splitCostFooter(s); body != s || f != "" {
		t.Errorf("只剩开始标记时不应丢内容：%q %q", body, f)
	}
	// 接着写两次新小结，坏掉的开始标记后面那行也一直在
	bad := filepath.Join(t.TempDir(), "bad.md")
	os.WriteFile(bad, []byte(s), 0o644)
	for _, f := range []string{costBegin + "\n一\n" + costEnd + "\n", costBegin + "\n二\n" + costEnd + "\n"} {
		if err := rewriteNightly(bad, "# 标题\n\n", "", &f); err != nil {
			t.Fatal(err)
		}
	}
	if b, _ := os.ReadFile(bad); !strings.Contains(string(b), "- 02:00 乙") || strings.Contains(string(b), "\n一\n") || !strings.HasSuffix(string(b), "二\n"+costEnd+"\n") {
		t.Errorf("重写两次后应保留「乙」、只剩新小结：%q", b)
	}
	// 文件开头就是小结、后面有保护脚本追加的行：补上标题
	path := filepath.Join(t.TempDir(), "n.md")
	os.WriteFile(path, []byte(costBegin+"\n旧\n"+costEnd+"\n- 03:00 已自动回滚\n"), 0o644)
	nf := "新小结\n"
	if err := rewriteNightly(path, "# 标题\n\n", "- 04:00 丙\n", &nf); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "# 标题\n\n- 03:00 已自动回滚\n- 04:00 丙\n\n新小结\n" {
		t.Errorf("得到 %q", b)
	}
}
