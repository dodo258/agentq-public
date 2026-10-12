package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// recordOutcome 按刚结束的任务（运行了 ran）更新连环失败计数，需要熔断时设好 BreakerUntil，返回是否熔断、熔断多久。
// 秒退 = 没跑完（失败、中断、启动失败）且运行不到 breaker_quick。主人 stop 的、撞用量上限的、机器重启中断的不算，
// 也不清零（上限有自己的冷却）。其余结束（跑完、跑了一阵才失败、超时）说明不是系统性故障，计数和翻倍都清零；
// 跑完的还解除正在生效的熔断（比如主人修好后用 run 试跑成功了）。
func recordOutcome(st *State, cfg Config, j *Job, ran time.Duration, now time.Time) (bool, time.Duration) {
	if j.Status == statusKilled || j.LimitHit || j.Rebooted {
		return false, 0
	}
	// 夜班、复盘只计秒退：它们跑完不说明待做任务的问题好了，不清零、不解除熔断
	if j.Kind != "" && (j.Status == statusDone || ran >= cfg.BreakerQuick.Duration) {
		return false, 0
	}
	if j.Status == statusDone {
		st.BreakerUntil = time.Time{}
	}
	if j.Status == statusDone || ran >= cfg.BreakerQuick.Duration {
		st.FailStreak, st.BreakerLevel = 0, 0
		return false, 0
	}
	// 熔断中结束的（熔断前就启动的、或主人用 run 启动的）不再计，免得一次故障连续加码
	if now.Before(st.BreakerUntil) {
		return false, 0
	}
	st.FailStreak++
	if st.BreakerLevel == 0 && st.FailStreak < cfg.BreakerStreak {
		return false, 0
	}
	d := cfg.BreakerCooldown.Duration
	for i := 0; i < st.BreakerLevel && d < cfg.BreakerMax.Duration; i++ {
		d *= 2
	}
	if d > cfg.BreakerMax.Duration {
		d = cfg.BreakerMax.Duration
	}
	st.BreakerUntil = now.Add(d)
	st.BreakerLevel++
	st.FailStreak = 0
	return true, d
}

// noteOutcome 在任务结束时调用：更新熔断计数，熔断了就记日志并在当天汇总写一行报警，跑完解除熔断时也记一行。
func (a *App) noteOutcome(st *State, j *Job) {
	level, active := st.BreakerLevel, a.Now().Before(st.BreakerUntil)
	tripped, d := recordOutcome(st, a.Cfg, j, a.exitedAt(j).Sub(j.StartedAt), a.Now())
	if active && st.BreakerUntil.IsZero() {
		msg := fmt.Sprintf("熔断解除：任务 %s（%s「%s」）跑完，恢复自动接活", j.ID, j.Project, truncRunes(j.Title, 40))
		a.logf("%s", msg)
		a.appendNightlyLine(a.Now(), msg)
	}
	if !tripped {
		return
	}
	why := fmt.Sprintf("连续 %d 个任务都在 %s 内结束且没做完", a.Cfg.BreakerStreak, formatDuration(a.Cfg.BreakerQuick.Duration))
	if level > 0 {
		why = "熔断过后第一个任务又秒退，冷却时长翻倍（上限 " + formatDuration(a.Cfg.BreakerMax.Duration) + "）"
	}
	msg := fmt.Sprintf("熔断：%s，暂停启动新任务 %s，到 %s；最近一个是任务 %s（%s「%s」%s：%s），查原因看 agentq logs %s",
		why, formatDuration(d), whenCN(st.BreakerUntil), j.ID, j.Project, truncRunes(j.Title, 40),
		statusLabel(j.Status), oneLine(j.Summary, 120), j.ID)
	a.logf("%s", msg)
	a.appendNightlyLine(a.Now(), msg)
	a.notify(Notice{Cat: "alert", Key: "breaker", Text: "连环失败熔断：暂停接活到 " + whenCN(st.BreakerUntil),
		Lines: []string{why, fmt.Sprintf("最近一个：%s「%s」%s，看 agentq logs %s", j.Project, truncRunes(j.Title, 30), statusLabel(j.Status), j.ID)}})
}

// exitedAt 估计 claude 实际退出的时间：result.json 非空时取它的修改时间（--output-format json 只在退出时写一次）；
// 否则用收尾时间。收尾可能比退出晚好几分钟（等锁、只靠定时 tick 时），拿收尾时间算会把秒退算成跑了一阵。
// 空的 result.json 和 stderr.log 不能用：它们启动时就建好了，修改时间是启动时刻，超时或被杀的任务会被算成秒退。
func (a *App) exitedAt(j *Job) time.Time {
	t := a.Now()
	if j.EndedAt != nil {
		t = *j.EndedAt
	}
	fi, err := os.Stat(filepath.Join(a.jobDir(j.ID), "result.json"))
	if err == nil && fi.Size() > 0 && fi.ModTime().After(j.StartedAt) && fi.ModTime().Before(t) {
		return fi.ModTime()
	}
	return t
}

// 用量上限提示里的重置时间。官方文档（code.claude.com/docs/en/errors「Usage limits」）的写法：
// 「You've hit your session limit · resets 3:45pm」「… · resets Mon 12:00am」，有的带时区「resets 8pm (Africa/Cairo)」；
// 旧版 CLI 是「Claude AI usage limit reached|<unix 秒>」。
var (
	resetsRe = regexp.MustCompile(`(?i)\bresets\s+(?:(mon|tue|wed|thu|fri|sat|sun)[a-z]*\.?\s+|(jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*\.?\s+(\d{1,2}),?\s+(?:at\s+)?)?(\d{1,2})(?::(\d{2}))?\s*([ap]m)\b(?:\s*\(([^)]+)\))?`)
	epochRe  = regexp.MustCompile(`limit reached\|(\d{10})\b`)
)

// parseResetTime 从上限提示里解析重置时间，返回重置后 1 分钟（到点就能用）。
// 解析不出、或不在 (now, now+8 天] 之内（weekly 上限最长一周）就返回 false，调用方按 limit_cooldown 冷却。
// 提示里的时刻刚过去不到 1 小时，多半是任务结束到收尾之间跨过了重置点，也返回 false，免得多等一天或一周。
func parseResetTime(s string, now time.Time) (time.Time, bool) {
	var t time.Time
	if m := epochRe.FindStringSubmatch(s); m != nil {
		sec, _ := strconv.ParseInt(m[1], 10, 64)
		t = time.Unix(sec, 0)
	} else if m := resetsRe.FindStringSubmatch(s); m != nil {
		loc := now.Location()
		if m[7] != "" {
			l, err := time.LoadLocation(strings.TrimSpace(m[7]))
			if err != nil {
				return time.Time{}, false
			}
			loc = l
		}
		hour, _ := strconv.Atoi(m[4])
		minute := 0
		if m[5] != "" {
			minute, _ = strconv.Atoi(m[5])
		}
		if hour < 1 || hour > 12 || minute > 59 {
			return time.Time{}, false
		}
		hour %= 12
		if strings.EqualFold(m[6], "pm") {
			hour += 12
		}
		n := now.In(loc)
		switch {
		case m[2] != "": // 「resets Oct 14, 9am」
			mon := time.Month(strings.Index("janfebmaraprmayjunjulaugsepoctnovdec", strings.ToLower(m[2]))/3 + 1)
			day, _ := strconv.Atoi(m[3])
			t = time.Date(n.Year(), mon, day, hour, minute, 0, 0, loc)
			if justPassed(t, now) {
				return time.Time{}, false
			}
			if !t.After(now) {
				t = t.AddDate(1, 0, 0)
			}
		case m[1] != "": // 「resets Mon 12:00am」
			want := strings.Index("sunmontuewedthufrisat", strings.ToLower(m[1])) / 3
			t = time.Date(n.Year(), n.Month(), n.Day(), hour, minute, 0, 0, loc)
			t = t.AddDate(0, 0, (want-int(n.Weekday())+7)%7)
			if justPassed(t, now) {
				return time.Time{}, false
			}
			if !t.After(now) {
				t = t.AddDate(0, 0, 7)
			}
		default: // 「resets 3:45pm」：今天的这个点已过就是明天
			t = time.Date(n.Year(), n.Month(), n.Day(), hour, minute, 0, 0, loc)
			if justPassed(t, now) {
				return time.Time{}, false
			}
			if !t.After(now) {
				t = t.AddDate(0, 0, 1)
			}
		}
	} else {
		return time.Time{}, false
	}
	if !t.After(now) || t.After(now.Add(8*24*time.Hour)) {
		return time.Time{}, false
	}
	return t.Add(time.Minute), true
}

func justPassed(t, now time.Time) bool { return !t.After(now) && now.Sub(t) <= time.Hour }
