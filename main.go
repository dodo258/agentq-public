// agentq：launchd 每 5 分钟叫醒一次，把各项目 BACKLOG「待做」交给独立的 claude -p 进程按 agent-task 做到合并。
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

const usage = `agentq：后台调度器，把各项目 docs/BACKLOG.md「待做」交给 claude -p 做到合并

用法：
  agentq version                  显示发行版本
  agentq tick                     一次调度：收尾、超时处理、挑活启动（launchd 每 5 分钟调用，每个任务结束时也会调用一次）
  agentq run <项目名> [条目关键字]  立刻为该项目启动一个任务（不看暂停，仍遵守上限；订阅额度 ≥98% 时拒绝）
  agentq status                   订阅额度、运行中的任务、各项目待做条数、最近结束的任务
  agentq quality                  最近 7 天的一次满意率、返工率和每条的依据（扫各项目 BACKLOG「已完成」和 git 历史）
  agentq patrol                   立刻巡逻一次：launchd 服务、备份推送、任务失败和卡住、额度、早报和夜班报告、等拍板条目、磁盘（tick 每小时自动巡逻一次）
  agentq logs <任务id>             结果摘要和 stderr 末尾
  agentq stop <任务id>             杀掉该任务的进程组
  agentq pause | resume           暂停 / 恢复自动接活
  agentq night                    立刻执行一次夜班（prompts/night.md）；launchd 每天 01:00 叫醒，night_enabled=true 才执行
  agentq review                   立刻执行一次复盘（prompts/review.md）；launchd 每天 06:30 叫醒，review_enabled=true 才执行
  agentq notify setup|test|send|file|status  推送：配 Telegram、发测试消息、记一条要推的消息、马上发一个文件、看推送情况（agentq notify 看详细用法）
  agentq listen                   常驻收主人的 Telegram 消息（长轮询，随到随回；launchd 的 KeepAlive 服务调用）
  agentq install | uninstall      装上 / 卸下 launchd 服务（每 5 分钟 tick 一次、夜班和复盘两个定点任务、常驻的 Telegram 监听）

安装与维护：
  agentq setup [--yes] [--scope full|skills] [--services] [--backup] [--watchdog] [--extras]
  agentq doctor                  只读安装、CLI 和配置检查
  agentq rollback --yes           回到上一完整安装代，不回滚运行数据
  agentq remove --yes             卸载受管入口和服务，保留数据及恢复材料
  agentq restore --yes            恢复中断事务或最近一次卸载

自然语言需求：在项目目录运行 claude，按 dispatch 派活；agentq 不是自然语言聊天命令。
配置 ~/.agentq/config.json，任务记录 ~/.agentq/jobs/<id>/，每日汇总 ~/.claude/nightly/<日期>-agentq.md
`

func main() {
	os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr))
}

func realMain(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	if cmd == "version" || cmd == "--version" || cmd == "-V" {
		if len(rest) != 0 {
			fmt.Fprintln(stderr, "agentq version: unexpected arguments")
			return 2
		}
		fmt.Fprintf(stdout, "agentq %s\n", releaseVersion())
		return 0
	}
	if cmd == "install" && len(rest) > 0 {
		return runInstaller("setup", rest, stdout, stderr)
	}
	if cmd == "uninstall" && len(rest) > 0 {
		return runInstaller("remove", rest, stdout, stderr)
	}

	if cmd == "setup" || cmd == "doctor" || cmd == "rollback" || cmd == "remove" || cmd == "restore" {
		return runInstaller(cmd, rest, stdout, stderr)
	}
	if cmd == "maintenance" {
		return runMaintenance(rest, stdout, stderr)
	}
	if cmd == "_job" { // 任务外壳，内部用，不列在用法里
		return runJobShell(rest, stderr)
	}
	if cmd == "help" || cmd == "-h" || cmd == "--help" {
		fmt.Fprint(stdout, usage)
		return 0
	}
	want := map[string][2]int{ // 参数个数下限、上限
		"tick": {0, 0}, "run": {1, 2}, "status": {0, 0}, "logs": {1, 1},
		"stop": {1, 1}, "pause": {0, 0}, "resume": {0, 0}, "install": {0, 0}, "uninstall": {0, 0},
		"night": {0, 0}, "review": {0, 0}, "quality": {0, 0}, "patrol": {0, 0}, "notify": {0, 30}, "listen": {0, 0},
	}
	n, ok := want[cmd]
	if !ok {
		fmt.Fprintf(stderr, "agentq：不认识的命令 %q\n\n%s", cmd, usage)
		return 2
	}
	after := "" // tick --after <任务id>：任务外壳在任务结束后调用
	if cmd == "tick" && len(rest) == 2 && rest[0] == "--after" && rest[1] != "" {
		after, rest = rest[1], nil
	}
	scheduled := false // night/review --scheduled：launchd 定点叫醒，开关没打开就只记一行日志
	if (cmd == "night" || cmd == "review") && len(rest) == 1 && rest[0] == "--scheduled" {
		scheduled, rest = true, nil
	}
	if len(rest) < n[0] || len(rest) > n[1] {
		fmt.Fprintf(stderr, "agentq %s：参数个数不对\n\n%s", cmd, usage)
		return 2
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(stderr, "agentq：找不到 HOME：", err)
		return 1
	}
	a, err := newApp(home, stdout)
	if err != nil {
		if cmd == "tick" || cmd == "listen" || scheduled {
			// launchd 日志里要带时间，方便对上是哪一次 tick
			(&App{Now: time.Now, Out: stdout}).logf("%s：%v", cmd, err)
			if cmd == "listen" {
				time.Sleep(listenIdleWait) // KeepAlive 会马上再拉起它：歇一会儿再退出，免得配置写坏时每 10 秒刷一行
			}
		} else {
			fmt.Fprintln(stderr, "agentq：", err)
		}
		return 1
	}
	switch cmd {
	case "tick":
		err = a.tick(after)
	case "run":
		kw := ""
		if len(rest) == 2 {
			kw = rest[1]
		}
		err = a.cmdRun(rest[0], kw)
	case "status":
		err = a.cmdStatus()
	case "quality":
		err = a.cmdQuality()
	case "patrol":
		err = a.cmdPatrol()
	case "notify":
		err = a.cmdNotify(rest)
	case "listen":
		err = a.cmdListen()
	case "logs":
		err = a.cmdLogs(rest[0])
	case "stop":
		err = a.cmdStop(rest[0])
	case "pause":
		err = a.cmdSetPaused(true)
	case "resume":
		err = a.cmdSetPaused(false)
	case "night", "review":
		err = a.cmdRoutine(cmd, scheduled)
	case "install":
		err = a.cmdInstall()
	case "uninstall":
		err = a.cmdUninstall()
	}
	if err != nil {
		if !errors.Is(err, errLogged) {
			fmt.Fprintln(stderr, "agentq：", err)
		}
		return 1
	}
	return 0
}
