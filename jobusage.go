package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// JobUsage 是一个任务花了多少（从 claude 的 result.json 读）。和 usage.go 的订阅额度不是一回事。
type JobUsage struct {
	Input      int64   `json:"input_tokens"`
	Output     int64   `json:"output_tokens"`
	CacheRead  int64   `json:"cache_read_tokens"`
	CacheWrite int64   `json:"cache_write_tokens"`
	CostUSD    float64 `json:"cost_usd"`
}

func (u JobUsage) total() int64 { return u.Input + u.Output + u.CacheRead + u.CacheWrite }

// resultUsage 是 result.json 里和用量有关的字段。顶层 usage 只算主会话，不含子代理；
// modelUsage 按模型列出整个进程（含子代理）的用量，各模型 costUSD 之和等于 total_cost_usd。
type resultUsage struct {
	Usage *struct {
		Input      int64 `json:"input_tokens"`
		Output     int64 `json:"output_tokens"`
		CacheRead  int64 `json:"cache_read_input_tokens"`
		CacheWrite int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
	ModelUsage map[string]struct {
		Input      int64   `json:"inputTokens"`
		Output     int64   `json:"outputTokens"`
		CacheRead  int64   `json:"cacheReadInputTokens"`
		CacheWrite int64   `json:"cacheCreationInputTokens"`
		CostUSD    float64 `json:"costUSD"`
	} `json:"modelUsage"`
	TotalCostUSD *float64 `json:"total_cost_usd"`
}

// parseJobUsage 从 result.json 内容取用量：有 modelUsage 就按模型求和（含子代理），否则用顶层 usage；
// 费用取 total_cost_usd，没有就用各模型 costUSD 之和。三样都没有返回 nil（缺 usage 的结果文件）。
func parseJobUsage(raw []byte) *JobUsage {
	var r resultUsage
	if json.Unmarshal(raw, &r) != nil {
		return nil
	}
	var u JobUsage
	switch {
	case len(r.ModelUsage) > 0:
		for _, m := range r.ModelUsage {
			u.Input += m.Input
			u.Output += m.Output
			u.CacheRead += m.CacheRead
			u.CacheWrite += m.CacheWrite
			u.CostUSD += m.CostUSD
		}
	case r.Usage != nil:
		u = JobUsage{Input: r.Usage.Input, Output: r.Usage.Output, CacheRead: r.Usage.CacheRead, CacheWrite: r.Usage.CacheWrite}
	case r.TotalCostUSD == nil:
		return nil
	}
	if r.TotalCostUSD != nil {
		u.CostUSD = *r.TotalCostUSD
	}
	return &u
}

// readJobUsage 读任务的 result.json 记下用量；文件不在或没有用量字段就不记。
// 续跑（claude -r）时 modelUsage 和 total_cost_usd 是整个会话累计的（实测，见 DECISIONS），
// 要减掉上一个有结果的同会话任务的累计数，才是这次进程花的。
func (a *App) readJobUsage(st *State, j *Job) {
	j.Usage = a.rawJobUsage(j.ID)
	if j.Usage == nil {
		return
	}
	for p := j; p.ResumeOf != ""; {
		prev, err := st.find(p.ResumeOf)
		if err != nil {
			return
		}
		if u := a.rawJobUsage(prev.ID); u != nil {
			if d, ok := j.Usage.minus(*u); ok {
				j.Usage = &d
			}
			return
		}
		p = prev
	}
}

func (a *App) rawJobUsage(id string) *JobUsage {
	raw, err := os.ReadFile(filepath.Join(a.jobDir(id), "result.json"))
	if err != nil {
		return nil
	}
	return parseJobUsage(raw)
}

// minus 返回 u - p；有一项变小（不是累计的）就返回 false。
func (u JobUsage) minus(p JobUsage) (JobUsage, bool) {
	d := JobUsage{Input: u.Input - p.Input, Output: u.Output - p.Output, CacheRead: u.CacheRead - p.CacheRead,
		CacheWrite: u.CacheWrite - p.CacheWrite, CostUSD: u.CostUSD - p.CostUSD}
	ok := d.Input >= 0 && d.Output >= 0 && d.CacheRead >= 0 && d.CacheWrite >= 0 && d.CostUSD >= 0
	return d, ok
}

// fmtTokens：1234 → 1.2K，6388024 → 6.39M。
func fmtTokens(n int64) string {
	switch {
	case n >= 1e9:
		return fmt.Sprintf("%.2fB", float64(n)/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.2fM", float64(n)/1e6)
	case n >= 1e3:
		return fmt.Sprintf("%.1fK", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

// short：status 里用的「token 6.75M，约 $4.61」。
func (u JobUsage) short() string {
	return fmt.Sprintf("token %s，约 $%.2f", fmtTokens(u.total()), u.CostUSD)
}

// detail：汇总里用的「token 6.75M（输入 122、输出 69.8K、缓存读 6.39M、缓存写 292.3K），约 $4.61」。
func (u JobUsage) detail() string {
	return fmt.Sprintf("token %s（输入 %s、输出 %s、缓存读 %s、缓存写 %s），约 $%.2f", fmtTokens(u.total()),
		fmtTokens(u.Input), fmtTokens(u.Output), fmtTokens(u.CacheRead), fmtTokens(u.CacheWrite), u.CostUSD)
}

// 当天汇总末尾的小结（当天花费、一次满意率和返工率）夹在这两行之间，每有任务结束就重写；其余行插在它前面。
// 标记文字沿用最初的「当天花费」，改了会认不出已有汇总里的小结。
const (
	costBegin = "<!-- agentq 当天花费：开始（自动生成，每有任务结束重写） -->"
	costEnd   = "<!-- agentq 当天花费：结束 -->"
)

// costFooter 按 state 里 day 那天结束、记了用量的任务写花费小结；一个都没有返回空。
func (a *App) costFooter(st *State, day time.Time) string {
	y, m, d := day.Date()
	var jobs []*Job
	var sum JobUsage
	for _, j := range st.Jobs {
		if j.Usage == nil || j.EndedAt == nil {
			continue
		}
		if jy, jm, jd := j.EndedAt.In(day.Location()).Date(); jy != y || jm != m || jd != d {
			continue
		}
		jobs = append(jobs, j)
		sum.Input += j.Usage.Input
		sum.Output += j.Usage.Output
		sum.CacheRead += j.Usage.CacheRead
		sum.CacheWrite += j.Usage.CacheWrite
		sum.CostUSD += j.Usage.CostUSD
	}
	if len(jobs) == 0 {
		return ""
	}
	sort.SliceStable(jobs, func(i, k int) bool {
		if jobs[i].Usage.CostUSD != jobs[k].Usage.CostUSD {
			return jobs[i].Usage.CostUSD > jobs[k].Usage.CostUSD
		}
		return jobs[i].Usage.total() > jobs[k].Usage.total()
	})
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n## 当天花费\n\n", costBegin)
	fmt.Fprintf(&b, "- 合计 %d 个任务（没有用量记录的不算）：%s；费用按 API 标价估算\n", len(jobs), sum.detail())
	hit := "—"
	if den := sum.CacheRead + sum.CacheWrite + sum.Input; den > 0 {
		hit = fmt.Sprintf("%.1f%%", float64(sum.CacheRead)*100/float64(den))
	}
	fmt.Fprintf(&b, "- 缓存命中率 %s（缓存读 /（缓存读+缓存写+输入））\n", hit)
	b.WriteString("- 最费的 3 个任务：\n")
	for i, j := range jobs {
		if i == 3 {
			break
		}
		fmt.Fprintf(&b, "  %d. %s「%s」%s；%s\n", i+1, j.Project, truncRunes(j.Title, 40), j.Usage.short(), j.ID)
	}
	b.WriteString(costEnd + "\n")
	return b.String()
}

// splitCostFooter 把汇总拆成花费小结之外的内容（小结之后被追加的行接在后面）和小结本身。
func splitCostFooter(s string) (body, footer string) {
	// 取最后一个结束标记和它前面最近的开始标记：被改坏、多出来的开始标记留在正文里，不连带吞掉后面的行
	k := strings.LastIndex(s, costEnd)
	i := strings.LastIndex(s[:max(k, 0)], costBegin)
	if i < 0 || k < 0 {
		return s, ""
	}
	end := k + len(costEnd)
	after := strings.TrimPrefix(s[end:], "\n")
	return strings.TrimRight(s[:i], "\n") + "\n" + after, s[i:end] + "\n"
}

// rewriteNightly 重写 path：body 之后追加 line（可以为空），再接 footer。footer 为 nil 表示沿用文件里原有的小结。
func rewriteNightly(path, header, line string, footer *string) error {
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	body, old := splitCostFooter(string(raw))
	if body = strings.TrimLeft(body, "\n"); !strings.HasPrefix(body, "# ") {
		body = header + body
	}
	if footer == nil {
		footer = &old
	}
	out := body + line
	if *footer != "" {
		out += "\n" + *footer
	}
	return writeFileAtomic(path, []byte(out))
}

// updateCostFooter 重写 t 那天汇总末尾的小结：当天花费（那天还没有记了用量的任务就没有这一节）和最近 7 天的一次满意率、返工率。
func (a *App) updateCostFooter(st *State, t time.Time) {
	f := a.costFooter(st, t)
	if q := a.qualitySection(t); q != "" {
		if f == "" {
			f = costBegin + "\n\n" + q + costEnd + "\n"
		} else {
			f = strings.Replace(f, costEnd, "\n"+q+costEnd, 1)
		}
	}
	if f == "" {
		return
	}
	path := a.nightlyPath(t)
	err := os.MkdirAll(filepath.Dir(path), 0o755)
	if err == nil {
		err = rewriteNightly(path, nightlyHeader(t), "", &f)
	}
	if err != nil {
		a.logf("警告：写当天汇总 %s 的小结失败：%v", path, err)
	}
}
