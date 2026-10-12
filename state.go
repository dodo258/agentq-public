package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"
)

const (
	statusRunning = "running"
	statusDone    = "done"
	statusFailed  = "failed"
	statusTimeout = "timeout"
	statusKilled  = "killed"
	// statusInterrupted：claude 正常退出（is_error 为假），但这一条其实没做完（BACKLOG 仍标进行中，或结果里没写合并情况）。
	statusInterrupted = "interrupted"
)

// 结束的任务在 state.json 里保留多久（retry_after 和 status 只需要最近的）。
const keepEndedJobs = 30 * 24 * time.Hour

type Job struct {
	ID          string     `json:"id"`
	Project     string     `json:"project"`
	ProjectDir  string     `json:"project_dir"`
	Item        string     `json:"item"`
	Title       string     `json:"title"`
	Fingerprint string     `json:"fingerprint"`
	PID         int        `json:"pid"`
	StartedAt   time.Time  `json:"started_at"`
	Status      string     `json:"status"`
	EndedAt     *time.Time `json:"ended_at,omitempty"`
	Summary     string     `json:"summary,omitempty"`
	SessionID   string     `json:"session_id,omitempty"`
	// LimitHit 为真表示这次是撞了用量上限，不算一次「试过」，冷却过后可以再试。
	LimitHit bool     `json:"limit_hit,omitempty"`
	Args     []string `json:"args,omitempty"`
	// ResumePending：中断了、等下一次能启动时用同一会话续跑；ResumedBy 是续跑任务的 id。
	ResumePending bool   `json:"resume_pending,omitempty"`
	ResumedBy     string `json:"resumed_by,omitempty"`
	// ResumeOf 非空表示这个任务是续跑，值是被续跑的任务 id。续跑任务再中断就不再续跑。
	ResumeOf string `json:"resume_of,omitempty"`
	// BootTime 是启动任务时的开机时间（kern.boottime 的秒数，0 = 没记）。和现在不一致说明中途重启过：
	// 记下的 pid 和进程组号可能已经被别的程序占用，不能再拿来判断或清理。
	BootTime int64 `json:"boot_time,omitempty"`
	// Rebooted：任务因为机器重启而中断（不是 agent 的问题，不计连环失败）。
	Rebooted bool `json:"rebooted,omitempty"`
	// StallNoted：已经在汇总里报过「疑似卡死」，会话记录再长起来之前不重复报。
	StallNoted bool `json:"stall_noted,omitempty"`
	// Kind 非空表示是夜班（night）或复盘（review），不是 BACKLOG 里的待做：不占并发名额，不核验中断、不续跑。
	Kind string `json:"kind,omitempty"`
	// Usage 是结束时从 result.json 读到的用量（token、费用估算），结果文件里没有就为空。
	Usage *JobUsage `json:"usage,omitempty"`
}

type State struct {
	Jobs          []*Job    `json:"jobs"`
	CooldownUntil time.Time `json:"cooldown_until"`
	// 连环失败熔断：BreakerUntil 前不启动新任务；FailStreak 是连续秒退的任务数；
	// BreakerLevel 是熔断后还没有任务正常结束过的次数，大于 0 时再秒退一个就直接熔断，时长按它翻倍。
	BreakerUntil time.Time `json:"breaker_until"`
	FailStreak   int       `json:"fail_streak,omitempty"`
	BreakerLevel int       `json:"breaker_level,omitempty"`
	// Usage 是最近一次读到的订阅额度（claude -p /usage），缓存 usageCacheFor。
	Usage *UsageInfo `json:"usage,omitempty"`
}

// errStateCorrupt：state.json 读得出但内容坏了（不是没权限、读不了）。tick 遇到它会备份坏文件、按各任务的 meta.json 重建。
var errStateCorrupt = errors.New("状态文件内容损坏")

// loadState：文件不存在是正常的第一次运行，返回空状态；存在但读不出返回错误，调用方据此当作「未知」。
// 内容坏了的错误包着 errStateCorrupt。
func loadState(path string) (*State, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &State{}, nil
	}
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("%w：%s 格式不对：%w", errStateCorrupt, path, err)
	}
	for _, j := range st.Jobs {
		if j == nil {
			return nil, fmt.Errorf("%w：%s 里有空任务记录", errStateCorrupt, path)
		}
	}
	return &st, nil
}

func saveState(path string, st *State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(b, '\n'))
}

func (st *State) running() []*Job {
	var out []*Job
	for _, j := range st.Jobs {
		if j.Status == statusRunning {
			out = append(out, j)
		}
	}
	return out
}

func (st *State) find(id string) (*Job, error) {
	var hits []*Job
	for _, j := range st.Jobs {
		if j.ID == id {
			return j, nil
		}
		if len(id) >= 4 && len(j.ID) > len(id) && j.ID[:len(id)] == id {
			hits = append(hits, j)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return nil, fmt.Errorf("没有任务 %s（agentq status 看任务 id）", id)
	default:
		return nil, fmt.Errorf("任务 id 前缀 %s 对应多个任务，请写完整", id)
	}
}

// prune 去掉结束超过 keepEndedJobs 的记录。
func (st *State) prune(now time.Time) {
	kept := st.Jobs[:0]
	for _, j := range st.Jobs {
		if j.Status != statusRunning && j.EndedAt != nil && now.Sub(*j.EndedAt) > keepEndedJobs {
			continue
		}
		kept = append(kept, j)
	}
	st.Jobs = kept
}

// tasks 返回运行中的待做任务（不含夜班、复盘）。
func (st *State) tasks() []*Job {
	var out []*Job
	for _, j := range st.running() {
		if j.Kind == "" {
			out = append(out, j)
		}
	}
	return out
}

// capacity 判断全局和单项目上限，返回能否再为 project 启动一个任务及原因。夜班、复盘不占名额：
// 夜班一跑就是几个小时，就是在等这些名额上的任务做完。
func capacity(st *State, cfg Config, project string) (bool, string) {
	total, inProject := 0, 0
	for _, j := range st.tasks() {
		total++
		if j.Project == project {
			inProject++
		}
	}
	if total >= cfg.MaxParallel {
		return false, fmt.Sprintf("全局已有 %d 个任务在跑（上限 max_parallel=%d）", total, cfg.MaxParallel)
	}
	if inProject >= cfg.PerProject {
		return false, fmt.Sprintf("项目 %s 已有 %d 个任务在跑（上限 per_project=%d）", project, inProject, cfg.PerProject)
	}
	return true, ""
}

// pendingResumes 返回等着续跑的中断任务，按记录顺序。
func (st *State) pendingResumes() []*Job {
	var out []*Job
	for _, j := range st.Jobs {
		if j.Status == statusInterrupted && j.ResumePending {
			out = append(out, j)
		}
	}
	return out
}

// runningFor 返回指纹相同且还在跑的任务。
func (st *State) runningFor(fp string) *Job {
	for _, j := range st.running() {
		if j.Fingerprint == fp {
			return j
		}
	}
	return nil
}

// retryBlocked：同一条目最近一次结束（撞用量上限的不算）距今不足 retryAfter 时为真。
// 调用方只拿「待做」里没标进行中的条目来问，所以「那次之后条目仍在待做里未标进行中」已经成立。
func retryBlocked(st *State, fp string, now time.Time, retryAfter time.Duration) bool {
	var last *time.Time
	for _, j := range st.Jobs {
		if j.Fingerprint != fp || j.Status == statusRunning || j.EndedAt == nil || j.LimitHit {
			continue
		}
		if last == nil || j.EndedAt.After(*last) {
			last = j.EndedAt
		}
	}
	return last != nil && now.Sub(*last) < retryAfter
}
