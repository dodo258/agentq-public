package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// 集成测试：编译出真的 agentq 二进制，用临时 HOME、临时 projects_root 和假 claude 脚本跑子进程。
// 子进程的 HOME 指向临时目录，绝不读写真实的 ~/.agentq、~/projects、~/.claude。

var agentqBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "agentq-bin-")
	if err != nil {
		panic(err)
	}
	agentqBin = filepath.Join(dir, "agentq")
	// 推送没配 Telegram 时会弹本机通知：测试里一律改成写这个文件，绝不弹真通知（子进程继承这个环境变量）
	os.Setenv("AGENTQ_MACNOTIFY_LOG", filepath.Join(dir, "macnotify.log"))
	out, err := exec.Command("go", "build", "-o", agentqBin, ".").CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "编译 agentq 失败：%v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// 假 claude：auth status 回登录状态；否则记录参数/目录/环境，按 prompt 里的关键字睡几秒、写 JSON。
// INTERRUPT：首次运行时像 agent 认领一样给 BACKLOG 那一条加进行中标记，然后停在「继续等通知」；
// 用 -r 续跑时把那一条勾掉并报已合并，prompt 里还有 AGAIN 就再中断一次。
// LIMITMID：首次运行时同样加进行中标记，然后撞用量上限报错；续跑时勾掉并报已合并。
// TRANSCRIPT：像真 claude 一样在 $HOME/.claude/projects 下写会话记录，子代理记录一直在长。
// TGDISPATCH：Telegram 对话任务，往 demo 的 BACKLOG「待做」写一条，回一条已派出的消息。
// USAGE：结果带 usage 和两个模型的 modelUsage（数字照真实 result.json 改的）；其余正常结果都没有用量字段。
// RESETS：撞上限，提示里的重置时间是 2 小时后（UTC 写法）。RATEOK：做了 40 个回合、结果里提到 rate limit 的正常完成。
// -p /usage：每次在 $HOME/fake-usage-calls 记一行（连同参数和目录）；$HOME/fake-usage.json 在就原样输出它，
// 否则输出真实 CLI 在读不到额度时的那句话（没有窗口，agentq 当未知）。
const fakeClaude = `#!/bin/sh
if [ "$1" = "-p" ] && [ "$2" = "/usage" ]; then
  echo "$* PWD=$PWD" >> "$HOME/fake-usage-calls"
  if [ -f "$HOME/fake-usage.json" ]; then cat "$HOME/fake-usage.json"
  else echo '{"type":"result","is_error":false,"result":"You are currently using your subscription to power your Claude Code usage"}'; fi
  exit 0
fi
if [ "$1" = "auth" ]; then
  # BREAKSTATE：在 tick 挑活之后、启动任务之前把 state.json 换成目录，让启动后写状态文件失败
  if [ -n "$AGENTQ_FAKE_BREAK_STATE" ]; then rm -f "$HOME/.agentq/state.json"; mkdir -p "$HOME/.agentq/state.json/x"; fi
  if [ -n "$AGENTQ_FAKE_LOGGED_OUT" ]; then echo '{"loggedIn": false}'; else echo '{"loggedIn": true}'; fi
  exit 0
fi
prompt=""; resume=""; session=""; prev=""
for a in "$@"; do
  case "$prev" in -p) prompt="$a" ;; -r) resume="$a" ;; --session-id) session="$a" ;; esac
  prev="$a"
done
{
  for a in "$@"; do printf 'ARG[%s]\n' "$a"; done
  printf 'PWD[%s]\n' "$PWD"
  printf 'PPID[%s]\n' "$PPID"
  env | sed 's/^/ENV[/; s/$/]/'
} > "$AGENTQ_FAKE_LOG/$$.txt"
case "$prompt" in *IGNORETERM*) trap '' TERM ;; esac
case "$prompt" in *ORPHAN*) sleep 30 & ;; esac
case "$prompt" in *TRANSCRIPT*)
  d="$HOME/.claude/projects/-fake/$session"; mkdir -p "$d/subagents"; echo '{}' > "$d.jsonl"
  ( while :; do echo '{}' >> "$d/subagents/agent-1.jsonl"; sleep 0.3; done ) & ;;
esac
s=$(printf '%s' "$prompt" | sed -n 's/.*SLEEP=\([0-9]*\).*/\1/p')
[ -z "$s" ] && s=$AGENTQ_FAKE_SLEEP # 夜班、复盘的 prompt 是固定的说明文件，睡多久靠环境变量给
[ -n "$s" ] && sleep "$s"
for f in $AGENTQ_FAKE_WRITE; do mkdir -p "$(dirname "$f")"; echo report > "$f"; done # 夜班、复盘要写的报告
case "$prompt" in
  *TGDISPATCH*) # Telegram 对话：像按 dispatch 派活一样往 demo 的 BACKLOG 写一条，回答是要发给主人的消息
    sed -i '' 's/^## 待做$/## 待做\
\
- [ ] 加个按月对比 —— 来自 Telegram/' demo/docs/BACKLOG.md
    printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"🟢 <b>已派出 · 今天</b>\n\n🟢 demo　加个按月对比\n<i>12:00 · agentq 对话</i>","session_id":"s-tg"}' ;;
  *INTERRUPT*)
    [ -z "$resume" ] && sed -i '' '/^- \[ \] .*INTERRUPT/ s/$/（进行中：agent\/t-int 10-10 03:00）/' docs/BACKLOG.md
    case "$resume:$prompt" in
      :*|*AGAIN*) printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"审查人还在复跑测试，继续等通知。","session_id":"s-int","origin":{"kind":"task-notification"},"result_index":1}' ;;
      *) sed -i '' -e '/^- \[ \] .*INTERRUPT/ s/（进行中.*$//' -e '/^- \[ \] .*INTERRUPT/ s/^- \[ \]/- [x]/' docs/BACKLOG.md
         printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"接着做完了。\n\n已合并，提交 def5678；主人看 README。","session_id":"s-int"}' ;;
    esac ;;
  *STUCKMARK*) sed -i '' '/^- \[ \] .*STUCKMARK/ s/$/（卡住：agent\/t-stuck 两次没过）（进行中：agent\/t-stuck 10-10 03:00）/' docs/BACKLOG.md
    printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"两次换思路都没过，标了卡住，没合并。","session_id":"s-st"}' ;;
  *DONEMOVE*) sed -i '' '/^- \[ \] .*DONEMOVE/d' docs/BACKLOG.md; printf '%s\n' '- [x] 移完 —— 合并提交 1a2b3c4；验收：看 README' >> docs/BACKLOG.md
    printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"做了很多。\n\n第二轮审查结论是可以合并。","session_id":"s-dm"}' ;;
  *OLDDONE*) sed -i '' '/^- \[ \] .*OLDDONE/ s/$/（卡住：agent\/t-old 两次没过）/' docs/BACKLOG.md; printf '%s\n' '- [x] 同名 —— 合并提交 9f8e7d6；以前做过的同名条目' >> docs/BACKLOG.md
    printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"两次换思路都没过，标了卡住，没合并。","session_id":"s-od"}' ;;
  *COMMITFIRST*) printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"已经合并，提交号 95e8a03（PR #5）。\n\n**做了什么**\n- 加了协议\n\n第二轮审查结论是可以合并。\n\n**怎么验收**\n看 PR #5。","session_id":"s-cf"}' ;;
  *USAGE*) printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"做完了。\n\n已合并，提交 ccc3333；主人看 README。","session_id":"s-us","total_cost_usd":4.6071268,"usage":{"input_tokens":92,"cache_creation_input_tokens":157333,"cache_read_input_tokens":5681707,"output_tokens":54650},"modelUsage":{"claude-opus-5-5":{"inputTokens":100,"outputTokens":60000,"cacheReadInputTokens":6000000,"cacheCreationInputTokens":250000,"costUSD":4.4},"claude-haiku-5-5":{"inputTokens":22,"outputTokens":9768,"cacheReadInputTokens":388024,"cacheCreationInputTokens":42335,"costUSD":0.2071268}}}' ;;
  *NOMERGE*) printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"做了一半。","session_id":"s-nm"}' ;;
  *LIMITMID*)
    if [ -z "$resume" ]; then
      sed -i '' '/^- \[ \] .*LIMITMID/ s/$/（进行中：agent\/t-lm 10-10 03:00）/' docs/BACKLOG.md
      printf '%s\n' '{"type":"result","subtype":"success","is_error":true,"num_turns":12,"result":"You'"'"'ve hit your session limit","session_id":"s-lm"}'
    else
      sed -i '' -e '/^- \[ \] .*LIMITMID/ s/（进行中.*$//' -e '/^- \[ \] .*LIMITMID/ s/^- \[ \]/- [x]/' docs/BACKLOG.md
      printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"num_turns":30,"result":"接着做完了。\n\n已合并，提交 aaa1111；主人看 README。","session_id":"s-lm"}'
    fi ;;
  *RESETS*) r=$(TZ=UTC date -v+2H '+%-I:%M%p')
    printf '{"type":"result","is_error":true,"num_turns":1,"result":"You'"'"'ve hit your session limit · resets %s (UTC)","session_id":"s-rs"}\n' "$r" ;;
  *RATEOK*) printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"num_turns":40,"result":"修好了 API rate limit 重试，撞到 usage limit 时会退避。\n\n已合并，提交 bbb2222；主人看 README。","session_id":"s-rate"}' ;;
  *HITOK*) printf '%s\n' '{"type":"result","is_error":false,"num_turns":1,"result":"You'"'"'ve hit your session limit","session_id":"s-hit"}' ;;
  *LIMIT*) printf '%s\n' '{"type":"result","is_error":true,"result":"Claude AI usage limit reached|1760000000","session_id":"s-limit"}' ;;
  *FAIL*) printf '%s\n' '{"type":"result","is_error":true,"result":"出错了：测试失败","session_id":"s-fail"}' ;;
  *EMPTY*) echo "boom: 进程崩了" >&2 ;;
  *) printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"做完了。\n\n已合并，提交 abc1234；主人看 README。","session_id":"s-ok"}' ;;
esac
`

type env struct {
	t      *testing.T
	home   string
	root   string
	fake   string
	fakeLg string
	extra  []string
}

func newEnv(t *testing.T, cfg map[string]any) *env {
	t.Helper()
	e := &env{t: t, home: t.TempDir(), root: t.TempDir(), fakeLg: t.TempDir()}
	e.fake = filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(e.fake, []byte(fakeClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	// 多数用例逐次手动 tick、断言每次的收尾/启动个数，所以默认关掉任务结束后的自动 tick、tick 里的巡逻和推送；测它们的用例显式打开。
	full := map[string]any{"projects_root": e.root, "claude_path": e.fake, "tick_after_job": false, "patrol_enabled": false, "notify_enabled": false}
	for k, v := range cfg {
		full[k] = v
	}
	// 每个 claude 的父进程是任务外壳，claude 结束后它还要执行一次 tick；等它们都退出再删临时目录，
	// 否则那次 tick 会在已删掉的临时 HOME 下重新建 .agentq。（这个 Cleanup 比 TempDir 的晚注册，先执行。）
	t.Cleanup(func() {
		logs, _ := filepath.Glob(filepath.Join(e.fakeLg, "*.txt"))
		for _, f := range logs {
			b, _ := os.ReadFile(f)
			if m := regexp.MustCompile(`PPID\[(\d+)\]`).FindSubmatch(b); m != nil {
				pid, _ := strconv.Atoi(string(m[1]))
				waitExit(t, pid, 70*time.Second)
			}
		}
	})
	b, _ := json.Marshal(full)
	os.MkdirAll(filepath.Join(e.home, ".agentq"), 0o755)
	if err := os.WriteFile(filepath.Join(e.home, ".agentq", "config.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return e
}

// run 执行 agentq；环境模仿 launchd：没有 LANG/LC_*，HOME 指向临时目录；故意带上 CLAUDECODE 验证会被去掉。
func (e *env) run(args ...string) (string, int) {
	e.t.Helper()
	cmd := exec.Command(agentqBin, args...)
	var envs []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if name == "HOME" || name == "LANG" || strings.HasPrefix(name, "LC_") {
			continue
		}
		envs = append(envs, kv)
	}
	envs = append(envs, "HOME="+e.home, "AGENTQ_FAKE_LOG="+e.fakeLg, "CLAUDECODE=1", "CLAUDE_CODE_SESSION_ID=parent")
	cmd.Env = append(envs, e.extra...)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		e.t.Fatal(err)
	}
	return string(out), code
}

func (e *env) mustRun(args ...string) string {
	e.t.Helper()
	out, code := e.run(args...)
	if code != 0 {
		e.t.Fatalf("agentq %v 退出码 %d：\n%s", args, code, out)
	}
	return out
}

func (e *env) state() *State {
	e.t.Helper()
	st, err := loadState(filepath.Join(e.home, ".agentq", "state.json"))
	if err != nil {
		e.t.Fatal(err)
	}
	return st
}

func (e *env) jobByTitle(title string) *Job {
	e.t.Helper()
	for _, j := range e.state().Jobs {
		if j.Title == title {
			return j
		}
	}
	e.t.Fatalf("没有标题为 %q 的任务", title)
	return nil
}

func (e *env) nightly() string {
	b, _ := os.ReadFile(filepath.Join(e.home, ".claude", "nightly", time.Now().Format("2006-01-02")+"-agentq.md"))
	return string(b)
}

// waitExit 等进程退出（孤儿进程由 launchd 回收）。
func waitExit(t *testing.T, pid int, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("进程 %d 在 %s 内没有退出", pid, d)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func realPath(t *testing.T, p string) string {
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func countRunning(st *State) int { return len(st.running()) }

func TestIntegrationLifecycle(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil) // 默认 max_parallel 2、per_project 1
	alpha := mkProject(t, e.root, "alpha", "- [ ] 甲一 —— SLEEP=1\n- [ ] 甲二 —— SLEEP=1\n")
	mkProject(t, e.root, "beta", "- [ ] 乙一 —— SLEEP=1\n")
	mkProject(t, e.root, "项目丙", "- [ ] 丙一 —— SLEEP=8\n")

	// tick 1：全局上限 2，按字母序启动 alpha、beta 各一个；每个项目每次最多 1 个
	out := e.mustRun("tick")
	if !strings.Contains(out, "启动 2") {
		t.Fatalf("tick 1 应启动 2 个：\n%s", out)
	}
	st := e.state()
	if countRunning(st) != 2 {
		t.Fatalf("应有 2 个运行中：%+v", st.Jobs)
	}
	j1 := e.jobByTitle("甲一")
	j2 := e.jobByTitle("乙一")
	if j1.Status != statusRunning || j1.Project != "alpha" || j1.ProjectDir != alpha {
		t.Errorf("甲一：%+v", j1)
	}
	if !strings.HasPrefix(j1.Home(e), e.home) {
		t.Fatalf("任务目录不在临时 HOME 下")
	}
	prompt, _ := os.ReadFile(filepath.Join(j1.Home(e), "prompt.txt"))
	if !strings.Contains(string(prompt), "把 "+alpha+" 的 docs/BACKLOG.md「待做」里的这一条做到合并：甲一 —— SLEEP=1。") {
		t.Errorf("prompt 不对：%s", prompt)
	}
	if out := e.mustRun("status"); !strings.Contains(out, "运行中（2/2）") || strings.Contains(out, "进程已结束") {
		t.Errorf("status 应显示 2 个运行中：\n%s", out)
	}

	waitExit(t, j1.PID, 10*time.Second)
	waitExit(t, j2.PID, 10*time.Second)
	fakeLog, _ := os.ReadFile(filepath.Join(e.fakeLg, fmt.Sprintf("%d.txt", j1.PID)))
	for _, want := range []string{"ARG[-p]", "ARG[--model]\nARG[opus]", "ARG[--effort]\nARG[high]",
		"ARG[--permission-mode]\nARG[bypassPermissions]", "ARG[--output-format]\nARG[json]",
		"ARG[--name]\nARG[agentq alpha 甲一]", "PWD[" + realPath(t, alpha) + "]"} {
		if !strings.Contains(string(fakeLog), want) {
			t.Errorf("假 claude 收到的参数缺 %q：\n%s", want, fakeLog)
		}
	}
	if strings.Contains(string(fakeLog), "ENV[CLAUDE") {
		t.Errorf("CLAUDE* 环境变量不应传给子进程：\n%s", fakeLog)
	}

	// tick 2：收尾 2 个；alpha 启动甲二；beta 的乙一在 retry_after 内不再拉起；项目丙启动丙一
	out = e.mustRun("tick")
	if !strings.Contains(out, "本次收尾 2，启动 2") {
		t.Fatalf("tick 2：\n%s", out)
	}
	if j := e.jobByTitle("甲一"); j.Status != statusDone || j.SessionID != "s-ok" || !strings.Contains(j.Summary, "已合并，提交 abc1234") {
		t.Errorf("甲一应 done 并带摘要：%+v", j)
	}
	n := e.nightly()
	if strings.Count(n, "跑完") != 2 || !strings.Contains(n, "alpha「甲一」跑完：已合并，提交 abc1234；主人看 README。") {
		t.Errorf("当天汇总不对：\n%s", n)
	}
	j3 := e.jobByTitle("甲二")
	jc := e.jobByTitle("丙一")
	// 中文项目名在没有 LANG 的环境下也要能认出进程还活着（ps 输出不能被转义）
	out = e.mustRun("tick")
	if !strings.Contains(out, "本次收尾 0") {
		t.Errorf("丙一还在睡，不应被收尾：\n%s", out)
	}
	waitExit(t, j3.PID, 10*time.Second)
	waitExit(t, jc.PID, 15*time.Second)
	out = e.mustRun("tick")
	if !strings.Contains(out, "本次收尾 2，启动 0") {
		t.Errorf("全部收尾后不应重复启动已做过的条目：\n%s", out)
	}
	if st := e.state(); len(st.Jobs) != 4 || countRunning(st) != 0 {
		t.Errorf("应有 4 个结束的任务：%+v", st.Jobs)
	}
	if out := e.mustRun("status"); !strings.Contains(out, "alpha：2 条（进行中 0，卡住 0）") || !strings.Contains(out, "最近结束") {
		t.Errorf("status：\n%s", out)
	}
	out = e.mustRun("logs", j1.ID)
	if !strings.Contains(out, "跑完") || !strings.Contains(out, "已合并，提交 abc1234") || !strings.Contains(out, "s-ok") {
		t.Errorf("logs：\n%s", out)
	}
}

// Home 返回任务目录（测试辅助）。
func (j *Job) Home(e *env) string { return filepath.Join(e.home, ".agentq", "jobs", j.ID) }

func TestIntegrationTimeout(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"timeout": "1s"})
	mkProject(t, e.root, "alpha", "- [ ] 睡很久 —— SLEEP=30\n")
	mkProject(t, e.root, "beta", "- [ ] 不理 TERM —— IGNORETERM SLEEP=30\n")
	e.mustRun("tick")
	ja, jb := e.jobByTitle("睡很久"), e.jobByTitle("不理 TERM")
	time.Sleep(1500 * time.Millisecond)
	start := time.Now()
	out := e.mustRun("tick")
	if time.Since(start) < killGrace {
		t.Errorf("不理 TERM 的进程应等满 %s 再 SIGKILL", killGrace)
	}
	if !strings.Contains(out, "本次收尾 2") || !strings.Contains(out, "SIGTERM 后 SIGKILL") {
		t.Errorf("tick 输出：\n%s", out)
	}
	for _, j := range []*Job{ja, jb} {
		if got := e.jobByTitle(j.Title); got.Status != statusTimeout {
			t.Errorf("%s 应为 timeout：%+v", j.Title, got)
		}
		if err := syscall.Kill(-j.PID, 0); err == nil {
			t.Errorf("%s 的进程组 %d 应已被杀掉", j.Title, j.PID)
		}
	}
	if n := e.nightly(); strings.Count(n, "超时") != 2 {
		t.Errorf("当天汇总应有 2 条超时：\n%s", n)
	}
	// 超时也算试过：retry_after 内不再拉起
	if out := e.mustRun("tick"); !strings.Contains(out, "启动 0") {
		t.Errorf("超时的条目不应马上再启动：\n%s", out)
	}
}

func TestIntegrationCooldownAndFailures(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"max_parallel": 4})
	mkProject(t, e.root, "alpha", "- [ ] 撞上限 —— LIMIT\n- [ ] 甲二 —— 正常\n")
	mkProject(t, e.root, "beta", "- [ ] 报错 —— FAIL\n")
	mkProject(t, e.root, "delta", "- [ ] 成功但撞上限 —— HITOK\n")
	mkProject(t, e.root, "gamma", "- [ ] 崩溃 —— EMPTY\n")
	e.mustRun("tick")
	for _, title := range []string{"撞上限", "报错", "崩溃", "成功但撞上限"} {
		waitExit(t, e.jobByTitle(title).PID, 10*time.Second)
	}
	out := e.mustRun("tick")
	if !strings.Contains(out, "本次收尾 4，启动 0") || !strings.Contains(out, "冷却") {
		t.Fatalf("撞上限后应收尾且不启动甲二：\n%s", out)
	}
	st := e.state()
	if d := time.Until(st.CooldownUntil); d < 55*time.Minute || d > 61*time.Minute {
		t.Errorf("冷却截止应约为 1 小时后：%v", st.CooldownUntil)
	}
	if j := e.jobByTitle("撞上限"); j.Status != statusFailed || !j.LimitHit {
		t.Errorf("撞上限：%+v", j)
	}
	// is_error 为假但结果是上限文案（CLI 现行措辞）：仍算跑完，但要冷却、不算试过
	if j := e.jobByTitle("成功但撞上限"); j.Status != statusDone || !j.LimitHit {
		t.Errorf("成功但撞上限：%+v", j)
	}
	if j := e.jobByTitle("报错"); j.Status != statusFailed || j.LimitHit || !strings.Contains(j.Summary, "出错了") {
		t.Errorf("报错：%+v", j)
	}
	if j := e.jobByTitle("崩溃"); j.Status != statusFailed || !strings.Contains(j.Summary, "boom") {
		t.Errorf("崩溃应 failed 且摘要带 stderr：%+v", j)
	}
	if out := e.mustRun("status"); !strings.Contains(out, "用量冷却中") {
		t.Errorf("status 应显示冷却：\n%s", out)
	}
	if out, code := e.run("run", "alpha"); code == 0 || !strings.Contains(out, "冷却") {
		t.Errorf("冷却中 run 应拒绝：%d\n%s", code, out)
	}
}

func TestIntegrationTickLock(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	mkProject(t, e.root, "alpha", "- [ ] 甲 —— x\n")
	lock, err := acquireLock(filepath.Join(e.home, ".agentq", "tick.lock"), 0)
	if err != nil || lock == nil {
		t.Fatalf("测试拿锁失败：%v", err)
	}
	out := e.mustRun("tick")
	if !strings.Contains(out, "上一个 tick 还没结束") {
		t.Errorf("拿不到锁应直接退出：\n%s", out)
	}
	if st := e.state(); len(st.Jobs) != 0 {
		t.Errorf("拿不到锁不应启动任务")
	}
	lock.Close()
	if out := e.mustRun("tick"); !strings.Contains(out, "启动 1") {
		t.Errorf("释放锁后应能启动：\n%s", out)
	}
	waitExit(t, e.state().Jobs[0].PID, 10*time.Second)
}

func TestIntegrationNotLoggedIn(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.extra = []string{"AGENTQ_FAKE_LOGGED_OUT=1"}
	mkProject(t, e.root, "alpha", "- [ ] 甲 —— x\n")
	out := e.mustRun("tick")
	if !strings.Contains(out, "未登录") || !strings.Contains(out, "启动 0") {
		t.Errorf("未登录时只记日志不启动：\n%s", out)
	}
	if out, code := e.run("run", "alpha"); code == 0 || !strings.Contains(out, "未登录") {
		t.Errorf("未登录时 run 应拒绝：\n%s", out)
	}
	if st := e.state(); len(st.Jobs) != 0 {
		t.Errorf("不应有任务：%+v", st.Jobs)
	}
}

func TestIntegrationRunStopPauseLogs(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	mkProject(t, e.root, "alpha", "- [ ] 甲一 —— SLEEP=30\n- [ ] 甲二 关键字 —— SLEEP=30\n")
	e.mustRun("pause")
	b, _ := os.ReadFile(filepath.Join(e.home, ".agentq", "config.json"))
	if !strings.Contains(string(b), `"paused": true`) {
		t.Fatalf("pause 应写入配置：%s", b)
	}
	if out := e.mustRun("tick"); !strings.Contains(out, "已暂停") || !strings.Contains(out, "启动 0") {
		t.Errorf("暂停时 tick 不启动：\n%s", out)
	}
	out := e.mustRun("run", "alpha", "关键字")
	if !strings.Contains(out, "已启动任务") || !strings.Contains(out, "甲二 关键字") || !strings.Contains(out, "已暂停自动接活") {
		t.Fatalf("run 应按关键字启动甲二：\n%s", out)
	}
	j := e.jobByTitle("甲二 关键字")
	if out, code := e.run("run", "alpha"); code == 0 || !strings.Contains(out, "per_project") {
		t.Errorf("同项目已满应拒绝：\n%s", out)
	}
	if out, code := e.run("run", "nosuch"); code == 0 || !strings.Contains(out, "不是 git 主目录") {
		t.Errorf("不存在的项目应报错：\n%s", out)
	}
	if out := e.mustRun("status"); !strings.Contains(out, "已暂停") || !strings.Contains(out, "运行中（1/2）") {
		t.Errorf("status：\n%s", out)
	}
	if out := e.mustRun("logs", j.ID); !strings.Contains(out, "还在运行") {
		t.Errorf("logs 运行中：\n%s", out)
	}
	if out := e.mustRun("stop", j.ID); !strings.Contains(out, "已停止任务") {
		t.Errorf("stop：\n%s", out)
	}
	if err := syscall.Kill(-j.PID, 0); err == nil {
		t.Error("stop 后进程组应已不在")
	}
	if got := e.jobByTitle("甲二 关键字"); got.Status != statusKilled {
		t.Errorf("应为 killed：%+v", got)
	}
	if out := e.mustRun("logs", j.ID); !strings.Contains(out, "已停止") {
		t.Errorf("logs：\n%s", out)
	}
	if !strings.Contains(e.nightly(), "已停止") {
		t.Errorf("汇总应记录停止：\n%s", e.nightly())
	}
	if out, code := e.run("stop", j.ID); code == 0 || !strings.Contains(out, "不用停") {
		t.Errorf("重复 stop：\n%s", out)
	}
	e.mustRun("resume")
	b, _ = os.ReadFile(filepath.Join(e.home, ".agentq", "config.json"))
	if !strings.Contains(string(b), `"paused": false`) {
		t.Errorf("resume 应写入配置：%s", b)
	}
	// run 不看 retry_after：刚停掉的甲二仍能手动再启动
	out = e.mustRun("run", "alpha", "关键字")
	if !strings.Contains(out, "甲二 关键字") {
		t.Errorf("run 应忽略 retry_after：\n%s", out)
	}
	for _, jj := range e.state().running() {
		e.mustRun("stop", jj.ID)
	}
}

func TestIntegrationUsage(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	if out, code := e.run(); code != 2 || !strings.Contains(out, "用法") {
		t.Errorf("无参数：%d\n%s", code, out)
	}
	if out, code := e.run("bogus"); code != 2 || !strings.Contains(out, "不认识") {
		t.Errorf("未知命令：%d\n%s", code, out)
	}
	if _, code := e.run("logs"); code != 2 {
		t.Errorf("参数不够应退出码 2")
	}
	for _, args := range [][]string{{"tick", "x"}, {"tick", "--after"}, {"tick", "--after", ""}, {"tick", "x", "y"}} {
		if _, code := e.run(args...); code != 2 {
			t.Errorf("agentq %v 应退出码 2，得到 %d", args, code)
		}
	}
}

// 冷却到期后恢复启动；撞上限的那条不受 retry_after 限制，可以再试。
func TestIntegrationCooldownExpires(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"limit_cooldown": "2s", "max_parallel": 1})
	mkProject(t, e.root, "alpha", "- [ ] 撞上限 —— LIMIT\n")
	e.mustRun("tick")
	waitExit(t, e.jobByTitle("撞上限").PID, 10*time.Second)
	if out := e.mustRun("tick"); !strings.Contains(out, "启动 0") || !strings.Contains(out, "冷却") {
		t.Fatalf("应进入冷却：\n%s", out)
	}
	time.Sleep(2200 * time.Millisecond)
	out := e.mustRun("tick")
	if !strings.Contains(out, "启动 1") {
		t.Fatalf("冷却过后应再试撞上限的条目：\n%s", out)
	}
	st := e.state()
	if len(st.Jobs) != 2 || st.Jobs[1].Title != "撞上限" {
		t.Errorf("第二个任务应是同一条：%+v", st.Jobs)
	}
	waitExit(t, st.Jobs[1].PID, 10*time.Second)
}

// 每个项目每次 tick 最多启动 1 个，即使 per_project 允许更多。
func TestIntegrationOnePerProjectPerTick(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"per_project": 2, "max_parallel": 3})
	mkProject(t, e.root, "alpha", "- [ ] 甲一 —— SLEEP=5\n- [ ] 甲二 —— SLEEP=5\n- [ ] 甲三 —— SLEEP=5\n")
	if out := e.mustRun("tick"); !strings.Contains(out, "启动 1") {
		t.Fatalf("第一次 tick 只应启动 1 个：\n%s", out)
	}
	if out := e.mustRun("tick"); !strings.Contains(out, "启动 1") {
		t.Fatalf("第二次 tick 应启动第 2 个（per_project=2）：\n%s", out)
	}
	if out := e.mustRun("tick"); !strings.Contains(out, "启动 0") {
		t.Fatalf("同项目已有 2 个在跑，不应再启动：\n%s", out)
	}
	for _, j := range e.state().running() {
		e.mustRun("stop", j.ID)
	}
}

// claude 退出后同进程组里还有残留进程：任务外壳等到 claude 退出就发 SIGTERM 清掉，记进 launchd.log。
func TestIntegrationShellCleansGroup(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	mkProject(t, e.root, "alpha", "- [ ] 留孤儿 —— ORPHAN\n")
	e.mustRun("tick")
	j := e.jobByTitle("留孤儿")
	waitExit(t, j.PID, 10*time.Second)
	if !waitGroupGone(j.PID, 5*time.Second) {
		t.Error("残留进程组应被外壳清掉")
		syscall.Kill(-j.PID, syscall.SIGKILL)
	}
	waitFor(t, 5*time.Second, "launchd.log 记下清理残留进程", func() bool {
		return strings.Contains(e.launchdLog(), "任务 "+j.ID+" 的 claude 已退出，进程组")
	})
}

// waitFor 每 50ms 检查一次 cond，d 内没成立就失败。
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s 内没等到：%s", d, what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (e *env) findJob(title string) *Job {
	for _, j := range e.state().Jobs {
		if j.Title == title {
			return j
		}
	}
	return nil
}

func (e *env) launchdLog() string {
	b, _ := os.ReadFile(filepath.Join(e.home, ".agentq", "launchd.log"))
	return string(b)
}

// 任务结束时外壳马上 tick 一次：A 结束后不用再手动 tick，B 几秒内就被启动；B 结束后也被自动收尾。
func TestIntegrationTickAfterJob(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"tick_after_job": true}) // per_project 1：甲二只能等甲一结束
	mkProject(t, e.root, "alpha", "- [ ] 甲一 —— SLEEP=1\n- [ ] 甲二 —— SLEEP=1\n")
	if out := e.mustRun("tick"); !strings.Contains(out, "启动 1") {
		t.Fatalf("第一次 tick 应只启动甲一：\n%s", out)
	}
	a := e.jobByTitle("甲一")
	waitExit(t, a.PID, 10*time.Second)
	aExited := time.Now()
	waitFor(t, 5*time.Second, "甲一结束后自动启动甲二", func() bool { return e.findJob("甲二") != nil })
	t.Logf("甲一进程退出到甲二出现在 state.json：%s", time.Since(aExited).Round(time.Millisecond))
	a = e.jobByTitle("甲一")
	b := e.jobByTitle("甲二")
	if a.Status != statusDone || a.EndedAt == nil {
		t.Fatalf("甲一应已被自动收尾为跑完：%+v", a)
	}
	if gap := b.StartedAt.Sub(*a.EndedAt); gap < 0 || gap > 3*time.Second {
		t.Errorf("甲二应在甲一收尾后马上启动，实际间隔 %s", gap)
	}
	if b.PID <= 0 || b.PID == a.PID {
		t.Errorf("甲二的 pid 不对：%+v", b)
	}
	// pid 是 claude 本身（假 claude 按自己的 pid 写参数记录），不是外壳
	if _, err := os.Stat(filepath.Join(e.fakeLg, fmt.Sprintf("%d.txt", b.PID))); err != nil {
		t.Errorf("记下的 pid 应是 claude 进程：%v", err)
	}
	waitExit(t, b.PID, 10*time.Second)
	// 收尾后的 state.json 先落盘，那次 tick 的汇总日志行最后才写，所以等日志行而不是等状态
	waitFor(t, 5*time.Second, "甲二结束后被自动收尾", func() bool {
		return e.jobByTitle("甲二").Status == statusDone && strings.Contains(e.launchdLog(), "tick（任务 "+b.ID+" 结束后）：")
	})
	log := e.launchdLog()
	for _, want := range []string{
		"tick（任务 " + a.ID + " 结束后）：运行中 1/2，本次收尾 1，启动 1",
		"启动任务 " + b.ID + "：alpha「甲二」",
		"tick（任务 " + b.ID + " 结束后）：运行中 0/2，本次收尾 1，启动 0",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("launchd.log 缺 %q：\n%s", want, log)
		}
	}
	if n := e.nightly(); strings.Count(n, "跑完") != 2 {
		t.Errorf("当天汇总应有 2 条跑完：\n%s", n)
	}
}

// 任务结束时别的 tick 正拿着锁：结束后的那次 tick 等锁而不是跳过，锁一放就接上下一条。
func TestIntegrationTickAfterJobWaitsForLock(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"tick_after_job": true})
	mkProject(t, e.root, "alpha", "- [ ] 甲一 —— SLEEP=1\n- [ ] 甲二 —— SLEEP=1\n")
	e.mustRun("tick")
	a := e.jobByTitle("甲一")
	lock, err := acquireLock(filepath.Join(e.home, ".agentq", "tick.lock"), 0)
	if err != nil || lock == nil {
		t.Fatalf("测试拿锁失败：%v", err)
	}
	waitExit(t, a.PID, 10*time.Second)
	time.Sleep(1500 * time.Millisecond)
	if e.findJob("甲二") != nil {
		t.Fatal("锁被占着时不应启动甲二")
	}
	lock.Close()
	waitFor(t, 5*time.Second, "放锁后启动甲二", func() bool { return e.findJob("甲二") != nil })
	if strings.Contains(e.launchdLog(), "本次跳过") {
		t.Errorf("任务结束后的 tick 应等锁而不是跳过：\n%s", e.launchdLog())
	}
	waitExit(t, e.jobByTitle("甲二").PID, 10*time.Second)
}

// tick_after_job=false：任务结束后不自动 tick，只靠定时 tick。
func TestIntegrationTickAfterJobOff(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	mkProject(t, e.root, "alpha", "- [ ] 甲一 —— x\n- [ ] 甲二 —— x\n")
	e.mustRun("tick")
	waitExit(t, e.jobByTitle("甲一").PID, 10*time.Second)
	time.Sleep(time.Second)
	if st := e.state(); len(st.Jobs) != 1 || st.Jobs[0].Status != statusRunning {
		t.Errorf("关掉后不应自动收尾或启动：%+v", st.Jobs)
	}
	if out := e.mustRun("tick"); !strings.Contains(out, "本次收尾 1，启动 1") {
		t.Errorf("定时 tick 照常收尾并启动：\n%s", out)
	}
	waitExit(t, e.jobByTitle("甲二").PID, 10*time.Second)
}

// claude 启动不了时，外壳把错误报回启动方，不留下外壳进程。
func TestStartJobReportsStartError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, err := startJob(agentqBin, dir, dir, filepath.Join(dir, "没有这个 claude"), nil, filepath.Join(dir, "launchd.log"))
	if err == nil || !strings.Contains(err.Error(), "没有这个 claude") {
		t.Errorf("应报回启动错误：%v", err)
	}
}

// claude 正常退出（is_error 为假）但 BACKLOG 那一条仍标进行中：记中断，下一次 tick 用同一会话续跑一次，续跑做完就算跑完。
func TestIntegrationInterruptResume(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	alpha := mkProject(t, e.root, "alpha", "- [ ] 中断一 —— INTERRUPT SLEEP=1\n")
	e.mustRun("tick")
	waitExit(t, e.jobByTitle("中断一").PID, 10*time.Second)
	out := e.mustRun("tick")
	if !strings.Contains(out, "本次收尾 1，启动 1") || !strings.Contains(out, "续跑任务") {
		t.Fatalf("应收尾中断的任务并续跑：\n%s", out)
	}
	st := e.state()
	if len(st.Jobs) != 2 {
		t.Fatalf("应有原任务和续跑任务：%+v", st.Jobs)
	}
	orig, res := st.Jobs[0], st.Jobs[1]
	if orig.Status != statusInterrupted || orig.SessionID != "s-int" || orig.ResumePending || orig.ResumedBy != res.ID {
		t.Errorf("原任务应记中断并指向续跑任务：%+v", orig)
	}
	for _, want := range []string{"仍标进行中", "agent/t-int", "后台通知", "续跑一次", "继续等通知"} {
		if !strings.Contains(orig.Summary, want) {
			t.Errorf("中断摘要缺 %q：%s", want, orig.Summary)
		}
	}
	if res.Status != statusRunning || res.ResumeOf != orig.ID || res.Fingerprint != orig.Fingerprint {
		t.Errorf("续跑任务：%+v", res)
	}
	if out := e.mustRun("status"); !strings.Contains(out, "续跑中（接中断的 "+orig.ID+"）") ||
		!strings.Contains(out, "「中断一」中断（已续跑："+res.ID+"）") {
		t.Errorf("status 应显示续跑中和中断：\n%s", out)
	}
	prompt, _ := os.ReadFile(filepath.Join(res.Home(e), "prompt.txt"))
	if !strings.Contains(string(prompt), "后台的活") || !strings.Contains(string(prompt), "中断一 —— INTERRUPT SLEEP=1") {
		t.Errorf("续跑 prompt 不对：%s", prompt)
	}
	waitExit(t, res.PID, 10*time.Second)
	fakeLog, _ := os.ReadFile(filepath.Join(e.fakeLg, fmt.Sprintf("%d.txt", res.PID)))
	for _, want := range []string{"ARG[-r]\nARG[s-int]\nARG[-p]", "ARG[--disallowed-tools]\nARG[Monitor]",
		"ARG[--name]\nARG[agentq alpha 中断一]", "PWD[" + realPath(t, alpha) + "]"} {
		if !strings.Contains(string(fakeLog), want) {
			t.Errorf("续跑的参数缺 %q：\n%s", want, fakeLog)
		}
	}
	if out := e.mustRun("tick"); !strings.Contains(out, "本次收尾 1，启动 0") {
		t.Fatalf("续跑做完后应收尾、不再启动：\n%s", out)
	}
	if j := e.state().Jobs[1]; j.Status != statusDone || !strings.Contains(j.Summary, "已合并，提交 def5678") {
		t.Errorf("续跑任务应跑完：%+v", j)
	}
	n := e.nightly()
	for _, want := range []string{"alpha「中断一」中断：BACKLOG 里这一条仍标进行中", "alpha「中断一」续跑中：上次中断（任务 " + orig.ID,
		"alpha「中断一」跑完：已合并，提交 def5678"} {
		if !strings.Contains(n, want) {
			t.Errorf("当天汇总缺 %q：\n%s", want, n)
		}
	}
}

// 续跑后又中断：记中断、写进汇总等主人，不再续跑第二次。
func TestIntegrationResumeInterruptedAgain(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	mkProject(t, e.root, "alpha", "- [ ] 再中断 —— INTERRUPT AGAIN\n")
	e.mustRun("tick")
	waitExit(t, e.jobByTitle("再中断").PID, 10*time.Second)
	if out := e.mustRun("tick"); !strings.Contains(out, "启动 1") {
		t.Fatalf("应续跑一次：\n%s", out)
	}
	res := e.state().Jobs[1]
	waitExit(t, res.PID, 10*time.Second)
	if out := e.mustRun("tick"); !strings.Contains(out, "本次收尾 1，启动 0") {
		t.Fatalf("续跑再中断后不应再启动：\n%s", out)
	}
	j := e.state().Jobs[1]
	if j.Status != statusInterrupted || j.ResumePending || j.ResumedBy != "" || !strings.Contains(j.Summary, "续跑后仍中断，等主人处理") {
		t.Errorf("续跑任务应记中断且不再续跑：%+v", j)
	}
	if out := e.mustRun("tick"); !strings.Contains(out, "启动 0") || len(e.state().Jobs) != 2 {
		t.Errorf("之后的 tick 也不应再续跑：\n%s", out)
	}
	if out := e.mustRun("status"); !strings.Contains(out, "「再中断」中断（续跑自 ") {
		t.Errorf("status 应显示续跑后的中断：\n%s", out)
	}
	if n := e.nightly(); !strings.Contains(n, "alpha「再中断」中断：续跑后仍中断，等主人处理：BACKLOG 里这一条仍标进行中") {
		t.Errorf("当天汇总应写明续跑后仍中断：\n%s", n)
	}
}

// 条目没标进行中，但结果里没写合并情况：按中断报，不续跑。
func TestIntegrationNoMergeParagraph(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	mkProject(t, e.root, "alpha", "- [ ] 没写合并 —— NOMERGE\n")
	e.mustRun("tick")
	waitExit(t, e.jobByTitle("没写合并").PID, 10*time.Second)
	if out := e.mustRun("tick"); !strings.Contains(out, "本次收尾 1，启动 0") {
		t.Fatalf("不应续跑：\n%s", out)
	}
	j := e.jobByTitle("没写合并")
	if j.Status != statusInterrupted || j.ResumePending || !strings.Contains(j.Summary, "结果里没写合并情况") || !strings.Contains(j.Summary, "不续跑") {
		t.Errorf("应记中断、不续跑：%+v", j)
	}
}

// 摘要优先带合并提交号：条目移到「已完成」写了合并提交的，号放在摘要开头；结果前面段落写了提交号的，取那一段。
func TestIntegrationSummaryCommit(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"max_parallel": 3})
	mkProject(t, e.root, "alpha", "- [ ] 移完 —— DONEMOVE\n")
	mkProject(t, e.root, "beta", "- [ ] 号在前 —— COMMITFIRST\n")
	mkProject(t, e.root, "gamma", "- [ ] 同名 —— OLDDONE\n")
	e.mustRun("tick")
	waitExit(t, e.jobByTitle("移完").PID, 10*time.Second)
	waitExit(t, e.jobByTitle("号在前").PID, 10*time.Second)
	waitExit(t, e.jobByTitle("同名").PID, 10*time.Second)
	if out := e.mustRun("tick"); !strings.Contains(out, "本次收尾 3，启动 0") {
		t.Fatalf("应收尾三个任务：\n%s", out)
	}
	// 待做里同标题的条目标了卡住没合并：不带出「已完成」里以前同名条目的旧号
	if j := e.jobByTitle("同名"); j.Status != statusDone || j.Summary != "两次换思路都没过，标了卡住，没合并。" {
		t.Errorf("不应带出旧条目的合并提交：%+v", j)
	}
	if j := e.jobByTitle("移完"); j.Status != statusDone || j.Summary != "合并提交 1a2b3c4；第二轮审查结论是可以合并。" {
		t.Errorf("摘要应以已完成里的合并提交开头：%+v", j)
	}
	if j := e.jobByTitle("号在前"); j.Status != statusDone || j.Summary != "已经合并，提交号 95e8a03（PR #5）。" {
		t.Errorf("摘要应取写着提交号的段落：%+v", j)
	}
	out := e.mustRun("status")
	n := e.nightly()
	for _, want := range []string{"「移完」跑完：合并提交 1a2b3c4；", "「号在前」跑完：已经合并，提交号 95e8a03"} {
		if !strings.Contains(out, want) {
			t.Errorf("status 缺 %q：\n%s", want, out)
		}
		if !strings.Contains(n, want) {
			t.Errorf("当天汇总缺 %q：\n%s", want, n)
		}
	}
}

// agent 放下这一条时标了卡住、但没清进行中标记：不算中断（结果里写了没合并的原因）。
func TestIntegrationStuckNotInterrupted(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	mkProject(t, e.root, "alpha", "- [ ] 放下 —— STUCKMARK\n")
	e.mustRun("tick")
	waitExit(t, e.jobByTitle("放下").PID, 10*time.Second)
	if out := e.mustRun("tick"); !strings.Contains(out, "本次收尾 1，启动 0") {
		t.Fatalf("不应续跑：\n%s", out)
	}
	if j := e.jobByTitle("放下"); j.Status != statusDone || j.ResumePending {
		t.Errorf("标了卡住的应算跑完：%+v", j)
	}
}

// 正文提到「卡住」的条目照常被挑中；真正标了卡住的仍跳过（哪怕排在前面）。
func TestIntegrationStuckMarkerPick(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	mkProject(t, e.root, "alpha", "- [ ] 放下的 —— x（卡住：agent/1010-x 测试两次没过）\n- [ ] 修检测 —— 验收：正文提到卡住的照常可挑\n")
	if out := e.mustRun("tick"); !strings.Contains(out, "启动 1") {
		t.Fatalf("应只启动正文提到卡住的那条：\n%s", out)
	}
	j := e.jobByTitle("修检测")
	waitExit(t, j.PID, 10*time.Second)
	for _, j := range e.state().Jobs {
		if j.Title == "放下的" {
			t.Errorf("标了卡住的不该被挑：%+v", j)
		}
	}
}

// 连续 3 个任务秒退：熔断（默认 30 分钟），当天汇总写报警，status 显示熔断，tick 不再启动；
// 熔断到期后第一个任务又秒退：直接再熔断，时长翻倍。
func TestIntegrationBreaker(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"max_parallel": 3})
	for _, name := range []string{"alpha", "beta", "gamma"} {
		mkProject(t, e.root, name, "- [ ] "+name+" 秒退 —— FAIL\n")
	}
	mkProject(t, e.root, "zeta", "- [ ] 正常 —— x\n")
	if out := e.mustRun("tick"); !strings.Contains(out, "启动 3") {
		t.Fatalf("应启动 3 个：\n%s", out)
	}
	for _, j := range e.state().Jobs {
		waitExit(t, j.PID, 10*time.Second)
	}
	out := e.mustRun("tick")
	if !strings.Contains(out, "本次收尾 3，启动 0") || !strings.Contains(out, "连环失败熔断中") || !strings.Contains(out, "熔断：连续 3 个任务都在 3m 内结束且没做完") {
		t.Fatalf("3 个秒退后应熔断、不启动 zeta：\n%s", out)
	}
	st := e.state()
	if d := time.Until(st.BreakerUntil); d < 29*time.Minute || d > 30*time.Minute {
		t.Errorf("熔断应约 30 分钟：%v", st.BreakerUntil)
	}
	if st.BreakerLevel != 1 || st.FailStreak != 0 {
		t.Errorf("熔断计数：level %d streak %d", st.BreakerLevel, st.FailStreak)
	}
	if n := e.nightly(); !strings.Contains(n, "熔断：连续 3 个任务都在 3m 内结束且没做完，暂停启动新任务 30m") || !strings.Contains(n, "agentq logs ") {
		t.Errorf("当天汇总应写熔断报警：\n%s", n)
	}
	if out := e.mustRun("status"); !strings.Contains(out, "调度：连环失败熔断中") {
		t.Errorf("status 应显示熔断：\n%s", out)
	}
	if out := e.mustRun("tick"); !strings.Contains(out, "启动 0") || !strings.Contains(out, "熔断中") {
		t.Errorf("熔断中 tick 不启动：\n%s", out)
	}
	// run 是主人明确要做的，不受熔断限制
	out = e.mustRun("run", "zeta")
	if !strings.Contains(out, "已启动任务") || !strings.Contains(out, "连环失败熔断中") {
		t.Errorf("run 应不受熔断限制并提示：\n%s", out)
	}
	waitExit(t, e.jobByTitle("正常").PID, 10*time.Second)
	// 主人 run 试跑成功：收尾时解除熔断，汇总记一行
	e.mustRun("tick")
	if st := e.state(); !st.BreakerUntil.IsZero() || st.BreakerLevel != 0 {
		t.Errorf("跑完应解除熔断：%v level %d", st.BreakerUntil, st.BreakerLevel)
	}
	if n := e.nightly(); !strings.Contains(n, "熔断解除：任务 ") {
		t.Errorf("汇总应记熔断解除：\n%s", n)
	}
	if out := e.mustRun("status"); !strings.Contains(out, "调度：正常") {
		t.Errorf("解除后 status 应正常：\n%s", out)
	}
}

func TestIntegrationBreakerDoubles(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"max_parallel": 3, "breaker_cooldown": "2s"})
	for _, name := range []string{"alpha", "beta", "gamma"} {
		mkProject(t, e.root, name, "- [ ] "+name+" 一 —— FAIL\n- [ ] "+name+" 二 —— FAIL\n")
	}
	e.mustRun("tick")
	for _, j := range e.state().Jobs {
		waitExit(t, j.PID, 10*time.Second)
	}
	if out := e.mustRun("tick"); !strings.Contains(out, "暂停启动新任务 2s") {
		t.Fatalf("应熔断 2 秒：\n%s", out)
	}
	time.Sleep(2200 * time.Millisecond)
	if out := e.mustRun("tick"); !strings.Contains(out, "启动 3") {
		t.Fatalf("熔断到期后应恢复启动：\n%s", out)
	}
	for _, j := range e.state().running() {
		waitExit(t, j.PID, 10*time.Second)
	}
	out := e.mustRun("tick")
	if !strings.Contains(out, "熔断过后第一个任务又秒退，冷却时长翻倍") || !strings.Contains(out, "暂停启动新任务 4s") {
		t.Fatalf("熔断后第一个秒退应直接再熔断且翻倍：\n%s", out)
	}
	st := e.state()
	if st.BreakerLevel != 2 {
		t.Errorf("熔断过两次，level 应为 2：%d", st.BreakerLevel)
	}
	if d := time.Until(st.BreakerUntil); d < 2500*time.Millisecond || d > 4*time.Second {
		t.Errorf("第二次熔断应约 4 秒：%s", d)
	}
	if n := e.nightly(); strings.Count(n, "熔断：") != 2 {
		t.Errorf("汇总应有两行熔断（熔断中结束的另外两个不再加码）：\n%s", n)
	}
}

// 成功结果里出现 rate limit（做了很多回合）不算撞上限；撞上限时能解析出重置时间就冷却到那时。
func TestIntegrationLimitDetection(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	mkProject(t, e.root, "alpha", "- [ ] 提到限流 —— RATEOK\n")
	e.mustRun("tick")
	waitExit(t, e.jobByTitle("提到限流").PID, 10*time.Second)
	if out := e.mustRun("tick"); strings.Contains(out, "冷却") {
		t.Errorf("成功结果提到 rate limit 不应冷却：\n%s", out)
	}
	if j, st := e.jobByTitle("提到限流"), e.state(); j.Status != statusDone || j.LimitHit || !st.CooldownUntil.IsZero() {
		t.Errorf("应跑完、不算撞上限：%+v，冷却到 %v", j, st.CooldownUntil)
	}

	mkProject(t, e.root, "beta", "- [ ] 有重置时间 —— RESETS\n")
	e.mustRun("tick")
	waitExit(t, e.jobByTitle("有重置时间").PID, 10*time.Second)
	out := e.mustRun("tick")
	if !strings.Contains(out, "按提示的重置时间") {
		t.Fatalf("应按提示的重置时间冷却：\n%s", out)
	}
	// 提示只精确到分钟，重置后再加 1 分钟
	if d := time.Until(e.state().CooldownUntil); d < 119*time.Minute || d > 121*time.Minute {
		t.Errorf("冷却应约 2 小时（按 resets 时间），实际 %s", d)
	}
}

// 做到一半撞上限（BACKLOG 已标进行中）：记中断，冷却过后用同一会话续跑。
func TestIntegrationLimitMidwayResumes(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"limit_cooldown": "2s"})
	mkProject(t, e.root, "alpha", "- [ ] 半路撞上限 —— LIMITMID\n")
	e.mustRun("tick")
	waitExit(t, e.jobByTitle("半路撞上限").PID, 10*time.Second)
	out := e.mustRun("tick")
	if !strings.Contains(out, "本次收尾 1，启动 0") || !strings.Contains(out, "冷却") {
		t.Fatalf("撞上限后应冷却、先不续跑：\n%s", out)
	}
	j := e.state().Jobs[0]
	if j.Status != statusInterrupted || !j.LimitHit || !j.ResumePending ||
		!strings.Contains(j.Summary, "仍标进行中（分支 agent/t-lm 不存在）") || !strings.Contains(j.Summary, "冷却过后用同一会话续跑") {
		t.Fatalf("应记中断、等冷却后续跑：%+v", j)
	}
	time.Sleep(2200 * time.Millisecond)
	if out := e.mustRun("tick"); !strings.Contains(out, "续跑任务") || !strings.Contains(out, "启动 1") {
		t.Fatalf("冷却过后应续跑：\n%s", out)
	}
	res := e.state().Jobs[1]
	if res.ResumeOf != j.ID {
		t.Errorf("续跑任务应指向原任务：%+v", res)
	}
	waitExit(t, res.PID, 10*time.Second)
	fakeLog, _ := os.ReadFile(filepath.Join(e.fakeLg, fmt.Sprintf("%d.txt", res.PID)))
	if !strings.Contains(string(fakeLog), "ARG[-r]\nARG[s-lm]") {
		t.Errorf("续跑应用同一会话：\n%s", fakeLog)
	}
	e.mustRun("tick")
	if r := e.state().Jobs[1]; r.Status != statusDone || !strings.Contains(r.Summary, "aaa1111") {
		t.Errorf("续跑应跑完：%+v", r)
	}
}

// 每个任务的用量：有 usage 的结果记进 meta、status、当天汇总，末尾小结列最费的任务和缓存命中率；缺 usage 的照常收尾、不显示用量。
func TestIntegrationJobUsage(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	mkProject(t, e.root, "alpha", "- [ ] 有用量 —— USAGE\n")
	mkProject(t, e.root, "beta", "- [ ] 没用量 —— 普通\n")
	e.mustRun("tick")
	waitExit(t, e.jobByTitle("有用量").PID, 10*time.Second)
	waitExit(t, e.jobByTitle("没用量").PID, 10*time.Second)
	if out := e.mustRun("tick"); !strings.Contains(out, "本次收尾 2，启动 0") {
		t.Fatalf("应收尾两个任务：\n%s", out)
	}
	want := JobUsage{Input: 122, Output: 69768, CacheRead: 6388024, CacheWrite: 292335, CostUSD: 4.6071268}
	ju := e.jobByTitle("有用量")
	if ju.Status != statusDone || ju.Usage == nil || *ju.Usage != want {
		t.Fatalf("state 里的用量不对：%+v %+v", ju, ju.Usage)
	}
	var meta Job
	b, _ := os.ReadFile(filepath.Join(ju.Home(e), "meta.json"))
	if err := json.Unmarshal(b, &meta); err != nil || meta.Usage == nil || *meta.Usage != want {
		t.Errorf("meta.json 里的用量不对：%v\n%s", err, b)
	}
	if !strings.Contains(string(b), `"cache_read_tokens": 6388024`) {
		t.Errorf("meta.json 字段名不对：\n%s", b)
	}
	jn := e.jobByTitle("没用量")
	if jn.Status != statusDone || jn.Usage != nil {
		t.Errorf("缺 usage 的结果应照常跑完、不记用量：%+v %+v", jn, jn.Usage)
	}

	out := e.mustRun("status")
	if !strings.Contains(out, "「有用量」跑完：已合并，提交 ccc3333；主人看 README。（token 6.75M，约 $4.61）") {
		t.Errorf("status 最近结束应带 token 量：\n%s", out)
	}
	if !strings.Contains(out, "「没用量」跑完：已合并，提交 abc1234；主人看 README。\n") {
		t.Errorf("缺 usage 的任务不显示用量：\n%s", out)
	}
	n := e.nightly()
	for _, w := range []string{
		"alpha「有用量」跑完：已合并，提交 ccc3333；主人看 README。；用量 token 6.75M（输入 122、输出 69.8K、缓存读 6.39M、缓存写 292.3K），约 $4.61；日志 ",
		"beta「没用量」跑完：已合并，提交 abc1234；主人看 README。；日志 ",
		"合计 1 个任务（没有用量记录的不算）", "缓存命中率 95.6%", "1. alpha「有用量」token 6.75M，约 $4.61；" + ju.ID,
	} {
		if !strings.Contains(n, w) {
			t.Errorf("当天汇总缺 %q：\n%s", w, n)
		}
	}
	if !strings.HasSuffix(n, costEnd+"\n") || strings.Index(n, costBegin) < strings.LastIndex(n, "；日志 ") {
		t.Errorf("花费小结应在当天汇总末尾：\n%s", n)
	}
}
