package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// 超时或 stop 时，SIGTERM 之后等这么久仍在就 SIGKILL。
const killGrace = 10 * time.Second

// childEnvKeep 是传给 claude（和任务外壳、claude auth status）的环境变量白名单，其余一律不传：
// 防止 ANTHROPIC_API_KEY 之类让任务改走 API 计费或别的账号，也防止调用方所在 Claude 会话注入的
// CLAUDECODE、CLAUDE_CODE_* 让子进程把自己当成那个会话的子会话。
// CLAUDE_CONFIG_DIR、CLAUDE_CODE_OAUTH_TOKEN 是主人可能有意设置的登录/配置变量；代理变量是在终端里 agentq run 时联网要用的。
var childEnvKeep = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "LANG": true,
	"TMPDIR": true, "SHELL": true, "SSH_AUTH_SOCK": true,
	"CLAUDE_CONFIG_DIR": true, "CLAUDE_CODE_OAUTH_TOKEN": true,
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true, "ALL_PROXY": true,
	"http_proxy": true, "https_proxy": true, "no_proxy": true, "all_proxy": true,
}

// childEnv 按白名单过滤当前环境。另外保留 LC_*（含 LC_ALL）和 agentq 自己的 AGENTQ_* 变量。
func childEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if childEnvKeep[name] || strings.HasPrefix(name, "LC_") || strings.HasPrefix(name, "AGENTQ_") {
			out = append(out, kv)
		}
	}
	return out
}

// nameMarker 出现在被启动进程的命令行里（--name 的开头），用来确认 pid 没被别的程序复用。
func nameMarker(project string) string { return "agentq " + project + " " }

// procAlive：kill(pid, 0) 判断进程在不在；再用 ps 排除僵尸进程和 pid 被复用的情况。
func procAlive(pid int, marker string) bool {
	if pid <= 0 {
		return false
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return false // ESRCH：不在了；EPERM：属于别的用户，不是我们启动的
	}
	cmd := exec.Command("/bin/ps", "-ww", "-o", "stat=", "-o", "command=", "-p", strconv.Itoa(pid))
	cmd.Env = append(os.Environ(), "LC_ALL=en_US.UTF-8") // launchd 下没有 LANG，ps 会把中文转义
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return false // ps 找不到这个进程
		}
		return true // ps 本身跑不起来，只能信 kill(pid, 0)
	}
	line := strings.TrimSpace(string(out))
	if line == "" || strings.HasPrefix(line, "Z") {
		return false
	}
	return strings.Contains(line, marker)
}

// killGroup 对进程组发 SIGTERM，grace 之后仍在就 SIGKILL。返回是否用了 SIGKILL。
func killGroup(pgid int, grace time.Duration) bool {
	if pgid <= 0 {
		return false
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	if waitGroupGone(pgid, grace) {
		return false
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	waitGroupGone(pgid, 3*time.Second)
	return true
}

func groupAlive(pgid int) bool { return syscall.Kill(-pgid, 0) == nil }

// jobEnv 是 claude 和它拉起的所有子进程都继承的环境变量，值是任务 id；收尾清残留进程组时用来认出是这个任务留下的。
const jobEnv = "AGENTQ_JOB"

// groupMarked：进程组 pgid 里有没有进程带着这个任务的标记（ps -E 显示的环境里有 AGENTQ_JOB=<id>）。
// 不认命令行里的 --name 标记：它只到项目一级，组号被同项目另一个任务的 claude 占用时会误杀。
// 系统自带的程序（/bin/zsh、/bin/sleep 等）ps 看不到环境变量，组里只剩它们时认不出，宁可不杀。
func groupMarked(pgid int, jobID string) (bool, error) {
	ps := func(args ...string) (string, error) {
		cmd := exec.Command("/bin/ps", args...)
		cmd.Env = append(os.Environ(), "LC_ALL=en_US.UTF-8") // launchd 下没有 LANG，ps 会把中文转义
		out, err := cmd.Output()
		return string(out), err
	}
	out, err := ps("-A", "-o", "pid=", "-o", "pgid=")
	if err != nil {
		return false, err
	}
	var pids []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == strconv.Itoa(pgid) {
			pids = append(pids, f[0])
		}
	}
	if len(pids) == 0 {
		return false, nil
	}
	out, err = ps("-E", "-ww", "-o", "command=", "-p", strings.Join(pids, ","))
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return false, nil // 列完之后都退出了
		}
		return false, err
	}
	envMark := jobEnv + "=" + jobID + " "
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line+" ", " "+envMark) {
			return true, nil
		}
	}
	return false, nil
}

var bootSecRe = regexp.MustCompile(`sec = (\d+)`)

// bootTime 返回本次开机的时间（秒），取自 sysctl -n kern.boottime：「{ sec = 1791363855, usec = 257843 } Wed Oct  7 …」。
func bootTime() (int64, error) {
	out, err := exec.Command("/usr/sbin/sysctl", "-n", "kern.boottime").Output()
	if err != nil {
		return 0, err
	}
	return parseBootTime(string(out))
}

func parseBootTime(s string) (int64, error) {
	m := bootSecRe.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("kern.boottime 的输出看不懂：%q", strings.TrimSpace(s))
	}
	return strconv.ParseInt(m[1], 10, 64)
}

// newSessionID 生成一个随机 UUID（v4），启动时用 --session-id 预先指定会话，tick 才能找到会话记录看它有没有在长。
func newSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

func waitGroupGone(pgid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if !groupAlive(pgid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// claudeArgs 按方案拼启动参数；resume 非空时用 -r 续跑那个会话（会话 id 不变），否则用 --session-id 指定新会话的 id。
// 禁用 Monitor：-p 进程在回合结束时就退出，用 Monitor 等通知会中途退出（实测这个参数对子代理同样生效）。
func claudeArgs(cfg Config, prompt, project, title, resume, session string) []string {
	var args []string
	switch {
	case resume != "":
		args = append(args, "-r", resume)
	case session != "":
		args = append(args, "--session-id", session)
	}
	args = append(args, "-p", prompt, "--model", cfg.Model, "--effort", cfg.Effort)
	if cfg.PermissionMode != "" {
		args = append(args, "--permission-mode", cfg.PermissionMode)
	}
	return append(args,
		"--output-format", "json",
		"--disallowed-tools", "Monitor",
		"--name", nameMarker(project)+truncRunes(title, 20),
	)
}

// startClaude 在 dir 里启动 claude，新进程组，环境里带 AGENTQ_JOB=<jobID>，stdout/stderr 写进给定文件，不等它结束。
func startClaude(claude, dir, jobID string, args []string, stdoutPath, stderrPath string) (*exec.Cmd, error) {
	stdout, err := os.OpenFile(stdoutPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}
	defer stdout.Close()
	stderr, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}
	defer stderr.Close()
	cmd := exec.Command(claude, args...)
	cmd.Dir = dir
	cmd.Env = append(childEnv(), jobEnv+"="+jobID)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

// 外壳启动后多久内必须报回 claude 的 pid。
const shellReportWait = 30 * time.Second

// startJob 经任务外壳（agentq _job）启动 claude，返回 claude 的 pid。外壳在自己的进程组里等 claude 结束，
// 再换成 `agentq tick --after <任务id>`，让排队的待做马上接上；外壳和那次 tick 的输出追加到 logPath。
// claude 仍在它自己的进程组里，收尾、超时、stop 都照旧只看 claude 的 pid 和进程组。
func startJob(self, jobDir, projectDir, claude string, args []string, logPath string) (int, error) {
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, err
	}
	defer logf.Close()
	r, w, err := os.Pipe()
	if err != nil {
		return 0, err
	}
	defer r.Close()
	cmd := exec.Command(self, append([]string{"_job", jobDir, projectDir, claude}, args...)...)
	cmd.Dir = projectDir
	cmd.Env = childEnv()
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.ExtraFiles = []*os.File{w} // 外壳里是 fd 3，用来报 pid
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	err = cmd.Start()
	w.Close()
	if err != nil {
		return 0, err
	}
	_ = r.SetReadDeadline(time.Now().Add(shellReportWait))
	b, rerr := io.ReadAll(r)
	msg := strings.TrimSpace(string(b))
	if s, ok := strings.CutPrefix(msg, "pid "); ok {
		if pid, err := strconv.Atoi(s); err == nil && pid > 0 {
			// agentq 进程本身很快退出，外壳交给 launchd 回收；这里只是让 agentq 还活着时不留僵尸。
			go cmd.Wait()
			return pid, nil
		}
	}
	// 没拿到 pid 就别留下一个没人管的外壳。还没 Wait，外壳退出了也只是僵尸，组号不会被复用。
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Wait()
	if s, ok := strings.CutPrefix(msg, "err "); ok {
		return 0, errors.New(s)
	}
	if rerr != nil {
		return 0, fmt.Errorf("任务外壳没有报回 claude 的 pid：%v", rerr)
	}
	return 0, fmt.Errorf("任务外壳没有报回 claude 的 pid（收到 %q）", msg)
}

// runJobShell 是任务外壳本身：agentq _job <任务目录> <项目目录> <claude> [参数…]。
// 在项目目录启动 claude（新进程组，输出写进任务目录），经 fd 3 报回 pid；等 claude 结束后
// exec 成 `agentq tick --after <任务id>`。那次 tick 和定时 tick 共用 tick.lock，不会重叠。
func runJobShell(args []string, stderr io.Writer) int {
	if len(args) < 3 {
		fmt.Fprintln(stderr, "agentq _job：内部命令，由 tick/run 启动任务时调用")
		return 2
	}
	jobDir, projectDir, claude := args[0], args[1], args[2]
	syscall.CloseOnExec(3) // 别让 claude 继承报 pid 的管道，否则启动方要等 claude 结束才读到 EOF
	report := os.NewFile(3, "report")
	id := filepath.Base(jobDir)
	cmd, err := startClaude(claude, projectDir, id, args[3:], filepath.Join(jobDir, "result.json"), filepath.Join(jobDir, "stderr.log"))
	if err != nil {
		fmt.Fprintf(report, "err %v\n", err)
		report.Close()
		return 1
	}
	fmt.Fprintf(report, "pid %d\n", cmd.Process.Pid)
	report.Close()
	_ = cmd.Wait()
	// 外壳刚等到 claude 退出：组里还有进程时组号不会被复用，组里没进程了 kill 也只是 ESRCH，
	// 所以在这里清残留进程组不会误杀，不用像 tick 那样核对标记（tick 收尾时离退出可能已经很久）。
	pgid := cmd.Process.Pid
	if groupAlive(pgid) {
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		fmt.Fprintf(stderr, "%s 任务 %s 的 claude 已退出，进程组 %d 还有残留进程，已发 SIGTERM\n",
			time.Now().Format("2006-01-02 15:04:05"), id, pgid)
	}
	self, err := os.Executable()
	if err == nil {
		// exec 而不是在本进程里跑：期间重新 go build 过的话，用的是新版本
		err = syscall.Exec(self, []string{self, "tick", "--after", id}, os.Environ())
	}
	fmt.Fprintf(stderr, "%s 任务 %s 结束后执行 agentq tick 失败（等下次定时 tick）：%v\n",
		time.Now().Format("2006-01-02 15:04:05"), id, err)
	return 1
}

// claudeLoggedIn 执行 `claude auth status`，只有明确 loggedIn=true 才返回真。
func claudeLoggedIn(claude string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, claude, "auth", "status")
	cmd.Env = childEnv()
	out, err := cmd.Output()
	var st struct {
		LoggedIn *bool `json:"loggedIn"`
	}
	if jerr := json.Unmarshal(out, &st); jerr != nil || st.LoggedIn == nil {
		if err != nil {
			return false, fmt.Errorf("执行 %s auth status 失败：%w", claude, err)
		}
		return false, fmt.Errorf("%s auth status 的输出看不懂", claude)
	}
	return *st.LoggedIn, nil
}

// fileLock 用 flock 拿 ~/.agentq/tick.lock。
type fileLock struct{ f *os.File }

func (l *fileLock) Close() {
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	l.f.Close()
}

// acquireLock：wait 为 0 时拿不到立刻返回 (nil, nil)；否则最多等 wait。
func acquireLock(path string, wait time.Duration) (*fileLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &fileLock{f: f}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, err
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
