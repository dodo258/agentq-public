package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Duration 在 config.json 里写成 "4h"、"30m" 这样的字符串。
type Duration struct{ time.Duration }

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(formatDuration(d.Duration))
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("时长要写成字符串，如 \"4h\"：%w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

func formatDuration(d time.Duration) string {
	switch {
	case d > 0 && d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d > 0 && d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	default:
		return d.String()
	}
}

type Config struct {
	LaunchdLabel   string   `json:"launchd_label"`
	ProjectsRoot   string   `json:"projects_root"`
	ClaudePath     string   `json:"claude_path"`
	Model          string   `json:"model"`
	Effort         string   `json:"effort"`
	PermissionMode string   `json:"permission_mode,omitempty"`
	MaxParallel    int      `json:"max_parallel"`
	PerProject     int      `json:"per_project"`
	Timeout        Duration `json:"timeout"`
	RetryAfter     Duration `json:"retry_after"`
	LimitCooldown  Duration `json:"limit_cooldown"`
	Paused         bool     `json:"paused"`
	// TickAfterJob：任务结束时马上 tick 一次，让排队的待做接上；关掉就只靠 launchd 每 5 分钟的定时 tick。
	TickAfterJob bool `json:"tick_after_job"`
	// 连环失败熔断：连续 BreakerStreak 个任务都在 BreakerQuick 内结束且没做完，就暂停启动 BreakerCooldown；
	// 熔断过后第一个任务又秒退，时长翻倍，最多 BreakerMax。
	BreakerStreak   int      `json:"breaker_streak"`
	BreakerQuick    Duration `json:"breaker_quick"`
	BreakerCooldown Duration `json:"breaker_cooldown"`
	BreakerMax      Duration `json:"breaker_max"`
	// StallAfter：运行中的任务会话记录这么久没增长，就在 status 和当天汇总里标「疑似卡死」（只提醒，不杀）。
	StallAfter Duration `json:"stall_after"`
	// 夜班、复盘：launchd 每天 01:00、06:30 叫醒 agentq night/review，开关为假时只记一行日志不执行。
	// 夜班要等到 05:00 才收工，所以超时单独配。
	NightEnabled  bool     `json:"night_enabled"`
	ReviewEnabled bool     `json:"review_enabled"`
	NightTimeout  Duration `json:"night_timeout"`
	ReviewTimeout Duration `json:"review_timeout"`
	// 巡逻：tick 每小时纯脚本检查一圈（见 patrol.go）；PatrolAnalyze 打开时，有新异常就启动一次 claude -p 分析原因（每天最多 3 次）。
	PatrolEnabled bool `json:"patrol_enabled"`
	PatrolAnalyze bool `json:"patrol_analyze"`
	// 推送：要主人知道的事推到 Telegram（没配就弹本机通知），tick 顺带收主人发来的 Telegram 消息（见 notify.go）。
	NotifyEnabled bool `json:"notify_enabled"`
}

func defaultConfig() Config {
	return Config{
		LaunchdLabel:   launchdLabel,
		ProjectsRoot:   "~/projects",
		ClaudePath:     "claude",
		Model:          "opus",
		Effort:         "high",
		PermissionMode: "bypassPermissions",
		MaxParallel:    2,
		PerProject:     1,
		Timeout:        Duration{4 * time.Hour},
		RetryAfter:     Duration{12 * time.Hour},
		LimitCooldown:  Duration{time.Hour},
		TickAfterJob:   true,

		BreakerStreak:   3,
		BreakerQuick:    Duration{3 * time.Minute},
		BreakerCooldown: Duration{30 * time.Minute},
		BreakerMax:      Duration{4 * time.Hour},
		StallAfter:      Duration{45 * time.Minute},

		NightTimeout:  Duration{6 * time.Hour},
		ReviewTimeout: Duration{4 * time.Hour},

		PatrolEnabled: true,
		PatrolAnalyze: true,

		NotifyEnabled: true,
	}
}

func (c Config) validate() error {
	var bad []string
	if !regexp.MustCompile(`^[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)+$`).MatchString(c.LaunchdLabel) {
		bad = append(bad, "launchd_label 要是点分隔的字母、数字、下划线或连字符")
	}
	if c.ProjectsRoot == "" {
		bad = append(bad, "projects_root 为空")
	}
	if c.ClaudePath == "" {
		bad = append(bad, "claude_path 为空")
	}
	if c.PermissionMode != "" && c.PermissionMode != "default" && c.PermissionMode != "manual" && c.PermissionMode != "acceptEdits" && c.PermissionMode != "auto" && c.PermissionMode != "dontAsk" && c.PermissionMode != "plan" && c.PermissionMode != "bypassPermissions" {
		bad = append(bad, "permission_mode 不受支持")
	}
	if c.MaxParallel < 1 {
		bad = append(bad, "max_parallel 至少为 1")
	}
	if c.PerProject < 1 {
		bad = append(bad, "per_project 至少为 1")
	}
	if c.Timeout.Duration <= 0 || c.RetryAfter.Duration <= 0 || c.LimitCooldown.Duration <= 0 || c.StallAfter.Duration <= 0 ||
		c.NightTimeout.Duration <= 0 || c.ReviewTimeout.Duration <= 0 {
		bad = append(bad, "timeout、retry_after、limit_cooldown、stall_after、night_timeout、review_timeout 都要大于 0")
	}
	if c.BreakerStreak < 1 {
		bad = append(bad, "breaker_streak 至少为 1")
	}
	if c.BreakerQuick.Duration <= 0 || c.BreakerCooldown.Duration <= 0 || c.BreakerMax.Duration < c.BreakerCooldown.Duration {
		bad = append(bad, "breaker_quick、breaker_cooldown 要大于 0，breaker_max 不能小于 breaker_cooldown")
	}
	if len(bad) > 0 {
		return errors.New(strings.Join(bad, "；"))
	}
	return nil
}

// loadConfig 读配置；文件不存在就用默认值并写出一份。缺的键沿用默认值。
func loadConfig(path string) (Config, error) {
	cfg := defaultConfig()
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, saveConfig(path, cfg)
	}
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("配置文件 %s 格式不对：%w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return cfg, fmt.Errorf("配置文件 %s：%w", path, err)
	}
	return cfg, nil
}

func saveConfig(path string, cfg Config) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(b, '\n'))
}

// expandHome 把开头的 ~ 换成 home。
func expandHome(home, p string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

// writeFileAtomic 先写同目录临时文件再 rename，读的一方不会看到写了一半的文件。
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
