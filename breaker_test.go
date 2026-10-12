package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRecordOutcome(t *testing.T) {
	cfg := defaultConfig()
	now := time.Date(2026, 10, 10, 3, 0, 0, 0, time.Local)
	st := &State{}
	rec := func(status string, ran time.Duration, limit bool) (bool, time.Duration) {
		return recordOutcome(st, cfg, &Job{ID: "x", Status: status, LimitHit: limit}, ran, now)
	}
	quick := func() (bool, time.Duration) { return rec(statusFailed, 30*time.Second, false) }

	// 前两个秒退只计数；中间夹一个被 stop 的、一个撞上限的，不清零也不计
	quick()
	rec(statusKilled, time.Second, false)
	rec(statusFailed, time.Second, true)
	if tripped, _ := quick(); tripped || st.FailStreak != 2 {
		t.Fatalf("两个秒退不应熔断：streak %d", st.FailStreak)
	}
	// 第三个（中断也算没做完）熔断 30 分钟
	if tripped, d := rec(statusInterrupted, 2*time.Minute, false); !tripped || d != 30*time.Minute {
		t.Fatalf("第三个秒退应熔断 30m：%v %s", tripped, d)
	}
	if !st.BreakerUntil.Equal(now.Add(30*time.Minute)) || st.BreakerLevel != 1 || st.FailStreak != 0 {
		t.Fatalf("熔断状态：%+v", st)
	}
	// 熔断中结束的秒退不计
	if tripped, _ := quick(); tripped || st.FailStreak != 0 {
		t.Fatalf("熔断中的秒退不应计数：%+v", st)
	}
	// 熔断到期后第一个秒退直接再熔断、时长翻倍，上限 4 小时
	for _, want := range []time.Duration{time.Hour, 2 * time.Hour, 4 * time.Hour, 4 * time.Hour} {
		now = st.BreakerUntil
		if tripped, d := quick(); !tripped || d != want {
			t.Fatalf("应熔断 %s：%v %s", want, tripped, d)
		}
	}
	// 熔断中有任务跑完（比如主人 run 试跑成功）：解除熔断，计数和翻倍都清零，之后又要连续 3 个才熔断
	rec(statusDone, 10*time.Second, false)
	if !st.BreakerUntil.IsZero() || st.BreakerLevel != 0 || st.FailStreak != 0 {
		t.Fatalf("跑完应解除熔断并清零：%+v", st)
	}
	if tripped, _ := quick(); tripped {
		t.Fatal("清零后第一个秒退不应熔断")
	}
	// 跑了超过 breaker_quick 才失败的也清零
	rec(statusFailed, 5*time.Minute, false)
	if st.FailStreak != 0 {
		t.Fatalf("跑了一阵才失败应清零：%+v", st)
	}
}

// 秒退按 claude 实际退出的时间算（结果文件的修改时间），不按收尾时间：只靠定时 tick 时收尾可能晚好几分钟。
func TestExitedAt(t *testing.T) {
	a := &App{Home: t.TempDir(), Now: time.Now}
	start := time.Now().Add(-10 * time.Minute)
	ended := time.Now()
	j := &Job{ID: "1010-000000-x", StartedAt: start, EndedAt: &ended}
	if got := a.exitedAt(j); !got.Equal(ended) {
		t.Errorf("没有结果文件时应用收尾时间：%v", got)
	}
	os.MkdirAll(a.jobDir(j.ID), 0o755)
	p := filepath.Join(a.jobDir(j.ID), "result.json")
	os.WriteFile(p, []byte("{}"), 0o644)
	exited := start.Add(20 * time.Second)
	os.Chtimes(p, exited, exited)
	if got := a.exitedAt(j); !got.Equal(exited) {
		t.Errorf("应取 result.json 的修改时间：%v，应为 %v", got, exited)
	}
	// 超时被杀的任务：result.json 是启动时建的空文件，修改时间在启动时刻，不能拿来算，否则会被当成秒退
	os.WriteFile(p, nil, 0o644)
	os.Chtimes(p, start.Add(50*time.Millisecond), start.Add(50*time.Millisecond))
	if got := a.exitedAt(j); !got.Equal(ended) {
		t.Errorf("空的 result.json 不应采用：%v", got)
	}
	st := &State{}
	j.Status = statusTimeout
	for i := 0; i < 3; i++ {
		if tripped, _ := recordOutcome(st, defaultConfig(), j, a.exitedAt(j).Sub(j.StartedAt), ended); tripped {
			t.Fatal("超时的任务不应算秒退")
		}
	}
}

func TestResumeDepth(t *testing.T) {
	st := &State{Jobs: []*Job{{ID: "a"}, {ID: "b", ResumeOf: "a"}, {ID: "c", ResumeOf: "b"}}}
	for i, want := range []int{0, 1, 2} {
		if got := resumeDepth(st, st.Jobs[i]); got != want {
			t.Errorf("%s 的续跑深度应为 %d：%d", st.Jobs[i].ID, want, got)
		}
	}
}

func TestParseResetTime(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 10, 10, 15, 30, 0, 0, loc)                                      // 周六
	at := func(d, h, m int) time.Time { return time.Date(2026, 10, d, h, m+1, 0, 0, loc) } // 重置后 1 分钟
	cases := []struct {
		in   string
		want time.Time
	}{
		{"You've hit your session limit · resets 3:45pm", at(10, 15, 45)},
		{"You've hit your limit · resets 5pm", at(10, 17, 0)},
		{"You've hit your Opus limit · resets 2pm", at(11, 14, 0)}, // 今天 2 点已过 1 个多小时 → 明天
		{"You've hit your weekly limit · resets Mon 12:00am", at(12, 0, 0)},
		{"You've hit your weekly limit · resets Sat 9am", at(17, 9, 0)}, // 今天就是周六且已过好几个小时 → 下周
		{"5-hour limit reached ∙ resets Oct 12, 9am", at(12, 9, 0)},
		{"You've hit your limit · resets 8pm (UTC)", time.Date(2026, 10, 11, 4, 1, 0, 0, loc)},
		{"Claude AI usage limit reached|" + "1791624600", time.Unix(1791624600, 0).Add(time.Minute)}, // 2026-10-10 17:30 CST
	}
	for _, c := range cases {
		got, ok := parseResetTime(c.in, now)
		if !ok || !got.Equal(c.want) {
			t.Errorf("%q：得到 %v %v，应为 %v", c.in, got, ok, c.want)
		}
	}
	for _, s := range []string{
		"You've hit your session limit",
		"Claude AI usage limit reached|1760000000", // 已经过去的时间
		"resets 13pm",
		"resets 5pm (Not/AZone)",
		"resets Oct 9, 9am", // 今年的已过 → 明年，超过 8 天不采用
		// 刚过去不到 1 小时：多半是收尾晚了、已经重置，不推到明天/下周
		"You've hit your session limit · resets 3pm",
		"You've hit your weekly limit · resets Sat 2:45pm",
		"resets Oct 10, 3:00pm",
	} {
		if got, ok := parseResetTime(s, now); ok {
			t.Errorf("%q 不应采用：%v", s, got)
		}
	}
}

func TestMayBeLimit(t *testing.T) {
	parse := func(s string) claudeResult {
		var r claudeResult
		if err := json.Unmarshal([]byte(s), &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	yes := []string{
		`{"is_error":true,"num_turns":50}`,
		`{"is_error":false,"num_turns":50,"api_error_status":429}`,
		`{"is_error":false,"num_turns":1,"result_index":0}`,
		`{"is_error":false}`,
	}
	no := []string{
		`{"is_error":false,"num_turns":40,"api_error_status":null}`,
		`{"is_error":false,"num_turns":1,"result_index":2}`, // 后台通知回合，不是一开始就撞上限
	}
	for _, s := range yes {
		if !parse(s).mayBeLimit() {
			t.Errorf("%s 应扫上限字样", s)
		}
	}
	for _, s := range no {
		if parse(s).mayBeLimit() {
			t.Errorf("%s 不应扫上限字样", s)
		}
	}
}

// 夜班、复盘：秒退照样计数，跑完或跑了一阵才结束不清零、不解除熔断（它们正常不说明待做任务好了）。
func TestRecordOutcomeRoutine(t *testing.T) {
	cfg := defaultConfig()
	now := time.Date(2026, 10, 11, 6, 45, 0, 0, time.Local)
	st := &State{FailStreak: 1, BreakerLevel: 1}
	if tripped, _ := recordOutcome(st, cfg, &Job{Kind: kindReview, Status: statusDone}, 2*time.Hour, now); tripped ||
		st.FailStreak != 1 || st.BreakerLevel != 1 {
		t.Fatalf("复盘跑完不应清零：%+v", st)
	}
	st.BreakerUntil = now.Add(time.Hour)
	recordOutcome(st, cfg, &Job{Kind: kindNight, Status: statusDone}, 4*time.Hour, now)
	if !st.BreakerUntil.Equal(now.Add(time.Hour)) {
		t.Fatalf("夜班跑完不应解除熔断：%+v", st)
	}
	st.BreakerUntil = time.Time{}
	if tripped, _ := recordOutcome(st, cfg, &Job{Kind: kindNight, Status: statusFailed}, 10*time.Second, now); !tripped {
		t.Fatalf("熔断过一次后夜班秒退应再熔断：%+v", st)
	}
}
