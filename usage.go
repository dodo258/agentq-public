package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// 订阅额度：领活前用 `claude -p /usage`（Claude Code 自己回答的本地命令，不问模型、不花 token）读各窗口已用百分比和重置时间，
// 快用完就不领新活。做法学 magpie（yetone/magpie internal/provider/claude_usage_cli.go）。
const (
	usageStopAt   = 90               // 任一计入的窗口到这个百分比：tick 不再启动新任务（正在跑的不动）
	usageRefuseAt = 98               // 到这个百分比：agentq run 也拒绝
	usageCacheFor = 10 * time.Minute // 读一次管这么久
	// 到了重置时刻再等这么久才算恢复：本机时钟可能比服务端快一点，刚到点就启动会撞上限。
	usageResetSlack = time.Minute
	usageReadWait   = 60 * time.Second
)

// 额度档位。
const (
	usageUnknown = -1 // 读不出或看不懂：照常跑
	usageOK      = 0
	usageStop    = 1 // ≥usageStopAt：tick 不启动新任务
	usageRefuse  = 2 // ≥usageRefuseAt：run 也拒绝
)

type UsageWindow struct {
	// Label 是原文冒号前的部分去掉 "Current "：session、week、week (Fable)。
	Label string `json:"label"`
	// Scope 是周窗口括号里的模型（如 Fable）；「all models」和会话窗口为空。只限某个模型的窗口，
	// 任务用的不是这个模型就不计入（照样显示）。
	Scope    string     `json:"scope,omitempty"`
	Used     float64    `json:"used"`
	ResetsAt *time.Time `json:"resets_at,omitempty"`
}

type UsageInfo struct {
	CheckedAt time.Time     `json:"checked_at"`
	Windows   []UsageWindow `json:"windows,omitempty"`
	// Err 非空表示这次没读出来（命令失败、输出看不懂），档位当「未知」，照常跑。
	Err string `json:"err,omitempty"`
	// 上次写进当天汇总的日期和档位：每天第一次读到、或档位变了才写一行。
	NotedDay   string `json:"noted_day,omitempty"`
	NotedLevel int    `json:"noted_level,omitempty"`
}

// 「Current session: 2% used · resets Oct 11 at 1:59pm (Asia/Shanghai)」
// 「Current week (all models): 40% used · resets Oct 13 at 8:59pm (Asia/Shanghai)」「Current week (Fable): 0% used」
var usageLineRe = regexp.MustCompile(`^Current (session|week(?: \(([^)]+)\))?):\s*([0-9.]+)% used(?:\s*·\s*(resets .+))?$`)

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// parseUsage 从 /usage 的文字里取各窗口。一个窗口都没有就是看不懂，返回错误（调用方当未知）。
// 重置时间按 parseResetTime 的写法解析（它返回重置后 1 分钟，这里退回整点），换到本机时区显示，解析不出就不记。
func parseUsage(text string, now time.Time) ([]UsageWindow, error) {
	text = strings.TrimSpace(ansiRe.ReplaceAllString(text, ""))
	var out []UsageWindow
	seen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		m := usageLineRe.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil || seen[m[1]] {
			continue
		}
		used, err := strconv.ParseFloat(m[3], 64)
		if err != nil {
			continue
		}
		seen[m[1]] = true
		w := UsageWindow{Label: m[1], Used: used}
		if s := strings.TrimSpace(m[2]); s != "" && !strings.EqualFold(s, "all models") {
			w.Scope = s
		}
		if m[4] != "" {
			if t, ok := parseResetTime(m[4], now); ok {
				t = t.Add(-time.Minute).In(now.Location())
				w.ResetsAt = &t
			}
		}
		out = append(out, w)
	}
	if len(out) == 0 {
		first, _, _ := strings.Cut(text, "\n")
		if first == "" {
			return nil, errors.New("/usage 没有输出")
		}
		return nil, fmt.Errorf("看不懂 /usage 的输出（没有「Current session/week … %% used」行）：%s", oneLine(first, 120))
	}
	return out, nil
}

// readUsageText 执行 `claude -p /usage`，返回结果文字。在空的临时目录里跑、不带工具、不读用户和项目的设置
// （免得项目 hook 拖慢或输出混进来）、不存会话记录。环境变量照任务的白名单：不会带上
// CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC（带上它 /usage 不联网，只给一小时内别处留下的旧读数）。
func readUsageText(claude string) (string, error) {
	tmp, err := os.MkdirTemp("", "agentq-usage-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	ctx, cancel := context.WithTimeout(context.Background(), usageReadWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, claude, "-p", "/usage", "--output-format", "json",
		"--tools", "", "--strict-mcp-config", "--setting-sources", "", "--no-session-persistence")
	cmd.Dir = tmp
	cmd.Env = childEnv()
	cmd.Stdin = strings.NewReader("")
	// 超时杀掉 claude 后，它留下的子进程可能还占着输出管道；最多再等这么久就不等了（tick 这时拿着锁）
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	var res claudeResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &res); err == nil && (res.Result != "" || res.IsError) {
		if res.IsError {
			return "", errors.New("claude -p /usage 报错：" + oneLine(res.Result, 200))
		}
		return res.Result, nil
	}
	msg := strings.TrimSpace(stderr.String())
	if msg == "" {
		msg = strings.TrimSpace(stdout.String())
	}
	if runErr != nil {
		if msg != "" {
			return "", fmt.Errorf("执行 claude -p /usage 失败：%v：%s", runErr, oneLine(msg, 200))
		}
		return "", fmt.Errorf("执行 claude -p /usage 失败：%w", runErr)
	}
	return "", errors.New("claude -p /usage 的输出不是 JSON 结果：" + oneLine(msg, 200))
}

// counts：这个窗口是否计入。只限某个模型的周窗口（如「week (Fable)」），只有任务用的模型（config 的 model）
// 名字里含这个模型名（括号里的第一个词）才算；会话窗口和「all models」总算。
func (w UsageWindow) counts(model string) bool {
	if w.Scope == "" {
		return true
	}
	f := strings.Fields(strings.ToLower(w.Scope))
	return len(f) > 0 && strings.Contains(strings.ToLower(model), f[0])
}

// used 返回 now 时的已用百分比：过了重置时刻（再加 usageResetSlack）就当已重置，算 0。
func (w UsageWindow) usedAt(now time.Time) float64 {
	if w.ResetsAt != nil && !now.Before(w.ResetsAt.Add(usageResetSlack)) {
		return 0
	}
	return w.Used
}

func (w UsageWindow) name() string {
	if w.Label == "session" {
		return "会话"
	}
	if w.Scope != "" {
		return "本周（" + w.Scope + "）"
	}
	return "本周"
}

// level 返回 now 时的档位；挡住启动时 until 是最晚恢复的时刻（零值 = 没写重置时间，要等下次读），
// why 列出挡住的窗口。
func (u *UsageInfo) level(now time.Time, model string) (lvl int, until time.Time, why string) {
	if u == nil || u.Err != "" || len(u.Windows) == 0 {
		return usageUnknown, time.Time{}, ""
	}
	var parts []string
	noReset := false
	for _, w := range u.Windows {
		p := w.usedAt(now)
		if !w.counts(model) || p < usageStopAt {
			continue
		}
		l := usageStop
		if p >= usageRefuseAt {
			l = usageRefuse
		}
		if l > lvl {
			lvl = l
		}
		parts = append(parts, fmt.Sprintf("%s %s%%", w.name(), fmtPct(p)))
		if w.ResetsAt == nil {
			noReset = true
		} else if r := w.ResetsAt.Add(usageResetSlack); r.After(until) {
			until = r
		}
	}
	if noReset {
		until = time.Time{}
	}
	return lvl, until, strings.Join(parts, "、")
}

// stale：没读过、超过 usageCacheFor、时钟往回拨过，或读完之后有窗口过了重置时刻（要重新读拿新数）。
func (u *UsageInfo) stale(now time.Time) bool {
	if u == nil || now.Before(u.CheckedAt) || now.Sub(u.CheckedAt) >= usageCacheFor {
		return true
	}
	for _, w := range u.Windows {
		if w.ResetsAt != nil && w.ResetsAt.After(u.CheckedAt) && !now.Before(w.ResetsAt.Add(usageResetSlack)) {
			return true
		}
	}
	return false
}

func fmtPct(p float64) string { return strconv.FormatFloat(p, 'f', -1, 64) }

// describe：各窗口用量和重置时间一行，status、当天汇总、日志共用。
func (u *UsageInfo) describe(now time.Time, model string) string {
	if u == nil {
		return "还没读过"
	}
	if u.Err != "" {
		return "未知（" + u.Err + "），照常接活"
	}
	var parts []string
	for _, w := range u.Windows {
		s := w.name() + " " + fmtPct(w.Used) + "%"
		switch {
		case w.ResetsAt == nil:
			s += "（重置时间没读出）"
		case !now.Before(w.ResetsAt.Add(usageResetSlack)):
			s += "（" + whenCN(*w.ResetsAt) + " 已重置）"
		default:
			s += "（" + whenCN(*w.ResetsAt) + " 重置）"
		}
		if !w.counts(model) {
			s += "（只限 " + w.Scope + "，任务用 " + model + "，不计入）"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "；")
}

// gateNote：挡住启动时的说明。
func usageGateNote(lvl int, until time.Time, why string) string {
	at := "，等下次读额度（" + formatDuration(usageCacheFor) + " 一次）"
	if !until.IsZero() {
		at = "，到 " + whenCN(until) + " 重置后自动恢复"
	}
	if lvl >= usageRefuse {
		return fmt.Sprintf("订阅额度快用完（%s，≥%d%%）：不启动新任务，agentq run 也拒绝%s", why, usageRefuseAt, at)
	}
	return fmt.Sprintf("订阅额度快用完（%s，≥%d%%）：不启动新任务（正在跑的不动），agentq run 仍可用%s", why, usageStopAt, at)
}

// readUsage 读一次额度（不看缓存）。测试可以换掉 App.UsageText。
func (a *App) readUsage(now time.Time) *UsageInfo {
	read := a.UsageText
	if read == nil {
		read = func() (string, error) { return readUsageText(a.claudePath()) }
	}
	u := &UsageInfo{CheckedAt: now}
	text, err := read()
	if err == nil {
		u.Windows, err = parseUsage(text, now)
	}
	if err != nil {
		u.Err = err.Error()
	}
	return u
}

// usage 返回当前额度：缓存过期才重读并存进 st（调用方负责保存 st）。每天第一次读到、或档位变了，
// 记日志并在当天汇总写一行。
func (a *App) usage(st *State) *UsageInfo {
	now := a.Now()
	if !st.Usage.stale(now) {
		return st.Usage
	}
	u := a.readUsage(now)
	prevLevel, prevDay := usageOK, ""
	if st.Usage != nil {
		prevLevel, prevDay = st.Usage.NotedLevel, st.Usage.NotedDay
	}
	u.NotedLevel, u.NotedDay = prevLevel, prevDay
	st.Usage = u
	lvl, until, why := u.level(now, a.Cfg.Model)
	day := now.Format("2006-01-02")
	if lvl == prevLevel && day == prevDay {
		return u
	}
	u.NotedLevel, u.NotedDay = lvl, day
	msg := "额度：" + u.describe(now, a.Cfg.Model)
	switch {
	case lvl >= usageStop:
		msg += "；" + usageGateNote(lvl, until, why)
	case lvl == usageOK && prevLevel >= usageStop && prevDay != "":
		msg += "；已恢复，照常接活"
	}
	a.logf("%s", msg)
	a.appendNightlyLine(now, msg)
	if lvl >= usageStop {
		a.notify(Notice{Cat: "alert", Key: "usage:" + day, Text: "订阅额度快用完：" + why, Lines: []string{usageGateNote(lvl, until, why)}})
	}
	return u
}
