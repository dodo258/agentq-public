package main

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
)

const launchdLabel = "io.agentq.agentq"

// serviceLabel 保留本机已安装服务的标识，新安装使用通用默认值。
func (a *App) serviceLabel() string {
	if a.Cfg.LaunchdLabel != "" {
		return a.Cfg.LaunchdLabel
	}
	return launchdLabel
}
func (a *App) routineLabel(kind string) string { return a.serviceLabel() + "." + kind }
func (a *App) listenLabel() string             { return a.serviceLabel() + ".listen" }
func (a *App) launchdPrefix() string {
	label := a.serviceLabel()
	return label[:strings.LastIndex(label, ".")+1]
}
func (a *App) plistPath() string { return a.plistPathFor(a.serviceLabel()) }
func (a *App) plistPathFor(label string) string {
	return filepath.Join(a.Home, "Library", "LaunchAgents", label+".plist")
}

// routineLabel：夜班、复盘的 launchd 服务名，如 io.agentq.agentq.night。
func routineLabel(kind string) string { return launchdLabel + "." + kind }

// launchdService 是 install 装的一个 launchd 服务。
type launchdService struct {
	label string
	plist string // plist 内容
}

// services：每 5 分钟的 tick，夜班、复盘两个定点任务，常驻的 Telegram 监听。
func (a *App) services() []launchdService {
	return []launchdService{
		{a.serviceLabel(), a.plistContent()},
		{a.routineLabel(kindNight), a.routinePlistContent(routines[kindNight])},
		{a.routineLabel(kindReview), a.routinePlistContent(routines[kindReview])},
		{a.listenLabel(), a.listenPlistContent()},
	}
}
func (a *App) binPath() string        { return filepath.Join(a.Home, ".local", "bin", "agentq") }
func (a *App) launchdLogPath() string { return filepath.Join(a.dataDir(), "launchd.log") }
func (a *App) guardPath() string      { return filepath.Join(a.dataDir(), "tick-guard.sh") }
func (a *App) guardFailsPath() string { return filepath.Join(a.dataDir(), "guard-fails") }
func (a *App) launchdDomain() string  { return fmt.Sprintf("gui/%d", os.Getuid()) }

func (a *App) launchdPATH() string {
	return strings.Join([]string{
		filepath.Join(a.Home, ".local", "bin"), filepath.Join(a.Home, ".npm-global", "bin"),
		"/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin",
	}, ":")
}

// 与 plist 显式声明相同；不继承安装 shell 的 token 或代理。
func (a *App) launchdEnv() []string {
	return []string{"HOME=" + a.Home, "PATH=" + a.launchdPATH()}
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// shQuote 把 s 包成 sh 的单引号字符串。
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// guardContent 是 launchd 调用的保护脚本：执行 agentq tick，记连续非 0 退出的次数（scripts/update.sh 装新版本时清零），
// 连续 2 次就把 scripts/update.sh 留下的 agentq.prev 原子改名换回来（坏的留成 agentq.bad），在当天汇总写一行「已自动回滚」。
// agentq.prev 换回来后就没了，换回的版本再失败也不会来回换；没有 agentq.prev 时第 2 次失败写一行报警。
// 汇总的文件名、标题和行格式和 appendNightlyLine 一致。
func (a *App) guardContent() string {
	return `#!/bin/sh
# agentq launchd 保护脚本，由 agentq install 生成，改了会被下次 install 覆盖。
bin=` + shQuote(a.binPath()) + `
fails=` + shQuote(a.guardFailsPath()) + `
nightly_dir=` + shQuote(filepath.Join(a.Home, ".claude", "nightly")) + `

"$bin" tick
code=$?
if [ "$code" -eq 0 ]; then
	rm -f "$fails"
	exit 0
fi
n=$(cat "$fails" 2>/dev/null)
case "$n" in '' | *[!0-9]*) n=0 ;; esac
n=$((n + 1))
echo "$n" >"$fails"
echo "$(date '+%Y-%m-%d %H:%M:%S') 保护脚本：agentq tick 退出码 $code（连续第 $n 次非 0）"
[ "$n" -eq 2 ] || exit "$code"

summary() {
	day=$(date +%Y-%m-%d)
	f="$nightly_dir/$day-agentq.md"
	mkdir -p "$nightly_dir"
	[ -s "$f" ] || printf '# agentq 任务汇总 %s\n\n' "$day" >>"$f"
	printf -- '- %s %s\n' "$(date +%H:%M)" "$1" >>"$f"
}
prev_time=$(stat -f '%Sm' -t '%Y-%m-%d %H:%M' "$bin.prev" 2>/dev/null)
if [ -f "$bin.prev" ] && { cp -p "$bin" "$bin.bad" 2>/dev/null; mv -f "$bin.prev" "$bin"; }; then
	rm -f "$fails"
	echo "$(date '+%Y-%m-%d %H:%M:%S') 保护脚本：已自动回滚，$bin.prev（$prev_time 构建）换回 $bin，出错的版本留在 $bin.bad"
	summary "已自动回滚：agentq tick 连续 2 次非 0 退出（最后一次退出码 $code），已把 $bin.prev（$prev_time 构建）换回 $bin，出错的版本留在 $bin.bad；原因看 ~/.agentq/launchd.log"
else
	echo "$(date '+%Y-%m-%d %H:%M:%S') 保护脚本：没有可换回的 $bin.prev（或换回失败），没有回滚"
	summary "agentq tick 连续 2 次非 0 退出（最后一次退出码 $code），没有可换回的 $bin.prev，没有回滚，要人看 ~/.agentq/launchd.log"
fi
exit "$code"
`
}

// plist 按方案：StartInterval 300、RunAtLoad、PATH、日志到 ~/.agentq/launchd.log；
// ProgramArguments 是 /bin/sh 保护脚本（它再执行 agentq tick，连续失败时自动回滚），不直接调 agentq。
// AbandonProcessGroup：tick 退出时 launchd 不去杀同组进程（claude 本来就在自己的进程组里，这里再保险一层）。
func (a *App) plistContent() string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + a.serviceLabel() + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>/bin/sh</string>
		<string>` + xmlEscape(a.guardPath()) + `</string>
	</array>
	<key>StartInterval</key>
	<integer>300</integer>
	<key>RunAtLoad</key>
	<true/>
	<key>AbandonProcessGroup</key>
	<true/>
	<key>EnvironmentVariables</key>
	<dict>
		<key>HOME</key>
		<string>` + xmlEscape(a.Home) + `</string>
		<key>PATH</key>
		<string>` + xmlEscape(a.launchdPATH()) + `</string>
	</dict>
	<key>StandardOutPath</key>
	<string>` + xmlEscape(a.launchdLogPath()) + `</string>
	<key>StandardErrorPath</key>
	<string>` + xmlEscape(a.launchdLogPath()) + `</string>
</dict>
</plist>
`
}

// routinePlistContent：每天 r.hour:r.minute 执行 `agentq night|review --scheduled`（开关没打开时它只记一行日志）。
// 不经保护脚本（回滚由每 5 分钟的 tick 管），没有 RunAtLoad（装上时不能立刻跑一次夜班）；PATH、日志同 tick 的 plist。
// 机器在那个时刻睡着时，launchd 在醒来后补跑一次。
func (a *App) routinePlistContent(r routine) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + a.routineLabel(r.kind) + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + xmlEscape(a.binPath()) + `</string>
		<string>` + r.kind + `</string>
		<string>--scheduled</string>
	</array>
	<key>StartCalendarInterval</key>
	<dict>
		<key>Hour</key>
		<integer>` + fmt.Sprint(r.hour) + `</integer>
		<key>Minute</key>
		<integer>` + fmt.Sprint(r.minute) + `</integer>
	</dict>
	<key>AbandonProcessGroup</key>
	<true/>
	<key>EnvironmentVariables</key>
	<dict>
		<key>HOME</key>
		<string>` + xmlEscape(a.Home) + `</string>
		<key>PATH</key>
		<string>` + xmlEscape(a.launchdPATH()) + `</string>
	</dict>
	<key>StandardOutPath</key>
	<string>` + xmlEscape(a.launchdLogPath()) + `</string>
	<key>StandardErrorPath</key>
	<string>` + xmlEscape(a.launchdLogPath()) + `</string>
</dict>
</plist>
`
}

// listenPlistContent：常驻执行 `agentq listen`。KeepAlive：不管怎么退出（挂了、换了新版本主动退出）launchd 都拉起来，
// 两次启动至少隔 ThrottleInterval 10 秒；没配 Telegram 时它自己歇着不退出，不会被反复拉起。
// 不经保护脚本（回滚由每 5 分钟的 tick 管，换回旧版本后它也会发现文件变了自己重启）；PATH、日志同 tick 的 plist。
func (a *App) listenPlistContent() string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + a.listenLabel() + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + xmlEscape(a.binPath()) + `</string>
		<string>listen</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ThrottleInterval</key>
	<integer>10</integer>
	<key>AbandonProcessGroup</key>
	<true/>
	<key>EnvironmentVariables</key>
	<dict>
		<key>HOME</key>
		<string>` + xmlEscape(a.Home) + `</string>
		<key>PATH</key>
		<string>` + xmlEscape(a.launchdPATH()) + `</string>
	</dict>
	<key>StandardOutPath</key>
	<string>` + xmlEscape(a.launchdLogPath()) + `</string>
	<key>StandardErrorPath</key>
	<string>` + xmlEscape(a.launchdLogPath()) + `</string>
</dict>
</plist>
`
}

// runLaunchctl 默认执行 /bin/launchctl；测试里换成记录参数的假函数，绝不碰真实 launchd。
func runLaunchctl(args ...string) ([]byte, error) {
	return exec.Command("/bin/launchctl", args...).CombinedOutput()
}

func (a *App) launchctl(args ...string) ([]byte, error) {
	if a.Launchctl != nil {
		return a.Launchctl(args...)
	}
	return runLaunchctl(args...)
}

// checkRealLaunchd：没有替换 Launchctl 时会动真实 launchd，只允许 Home 是当前用户真实家目录（防测试误装）。
func (a *App) checkRealLaunchd() error {
	if a.Launchctl != nil {
		return nil
	}
	u, err := user.Current()
	if err != nil {
		return fmt.Errorf("查不到当前用户的家目录，不调用 launchctl：%w", err)
	}
	if filepath.Clean(u.HomeDir) != filepath.Clean(a.Home) {
		return fmt.Errorf("HOME=%s 不是当前用户的家目录 %s，不调用真实 launchctl", a.Home, u.HomeDir)
	}
	return nil
}

func (a *App) serviceTarget(label string) string { return a.launchdDomain() + "/" + label }

func (a *App) serviceLoaded(label string) bool {
	_, err := a.launchctl("print", a.serviceTarget(label))
	return err == nil
}

// cmdInstall 先写好保护脚本和所有 plist，再逐个服务：已装着就 bootout，再 bootstrap；可重复执行。
func (a *App) cmdInstall() error {
	if _, err := os.Stat(filepath.Join(a.Home, ".local", "share", "agentq", "receipt.json")); err == nil {
		return fmt.Errorf("managed installation: use agentq setup --services --yes; legacy install would overwrite its guard")
	}
	if err := a.checkRealLaunchd(); err != nil {
		return err
	}
	if fi, err := os.Stat(a.binPath()); err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
		return fmt.Errorf("没找到可执行的 %s，先在项目目录执行：scripts/update.sh", a.binPath())
	}
	// 先写保护脚本和 plist：写失败时旧服务还在跑，不会落到「旧的卸了、新的没装」
	if err := writeFileAtomic(a.guardPath(), []byte(a.guardContent())); err != nil {
		return err
	}
	svcs := a.services()
	for _, s := range svcs {
		if err := writeFileAtomic(a.plistPathFor(s.label), []byte(s.plist)); err != nil {
			return err
		}
	}
	for _, s := range svcs {
		if a.serviceLoaded(s.label) {
			if out, err := a.launchctl("bootout", a.serviceTarget(s.label)); err != nil {
				return fmt.Errorf("卸下旧的 %s 失败（新 plist 已写好，旧服务仍在）：%v %s", s.label, err, strings.TrimSpace(string(out)))
			}
		}
		if out, err := a.launchctl("bootstrap", a.launchdDomain(), a.plistPathFor(s.label)); err != nil {
			return fmt.Errorf("launchctl bootstrap %s 失败（它现在没装着，再执行一次 agentq install；刚卸下旧服务时偶尔会报 5: Input/output error）：%v %s",
				s.label, err, strings.TrimSpace(string(out)))
		}
	}
	fmt.Fprintf(a.Out, "已安装：%s\nlaunchd 每 5 分钟经保护脚本 %s 执行一次 %s tick（装上时先执行一次；连续 2 次失败自动换回 agentq.prev）\n日志：%s\n",
		a.plistPath(), a.guardPath(), a.binPath(), a.launchdLogPath())
	fmt.Fprintf(a.Out, "定点任务：%s、%s（%s；%s）\n", a.routineLabel(kindNight), a.routineLabel(kindReview),
		a.routineStatus(kindNight), a.routineStatus(kindReview))
	fmt.Fprintf(a.Out, "常驻服务：%s（launchd 保持运行，收主人的 Telegram 消息；没配 Telegram 时只歇着）\n", a.listenLabel())
	if a.Cfg.Paused {
		fmt.Fprintln(a.Out, "注意：当前已暂停（paused=true），tick 只收尾不启动；要开始自动接活执行 agentq resume")
	}
	return nil
}

// cmdUninstall bootout 并删掉所有 plist；~/.agentq 里的配置、状态和任务记录保留。
func (a *App) cmdUninstall() error {
	if _, err := os.Stat(filepath.Join(a.Home, ".local", "share", "agentq", "receipt.json")); err == nil {
		return fmt.Errorf("managed installation: use agentq remove --yes (keeps data/config and supports restore)")
	}
	if err := a.checkRealLaunchd(); err != nil {
		return err
	}
	for _, s := range a.services() {
		if a.serviceLoaded(s.label) {
			if out, err := a.launchctl("bootout", a.serviceTarget(s.label)); err != nil {
				return fmt.Errorf("launchctl bootout %s 失败：%v %s", s.label, err, strings.TrimSpace(string(out)))
			}
		}
		if err := os.Remove(a.plistPathFor(s.label)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	fmt.Fprintf(a.Out, "已卸载 %s、夜班和复盘两个定点任务、Telegram 监听（%s 里的配置、状态和任务记录保留；正在跑的任务不受影响，它们在自己的进程组里，launchd 卸下服务时不会一起清掉）\n"+
		"注意：还在跑的任务结束时仍会 tick 一次、接下一条待做；要彻底不接新活，再执行 agentq pause\n", a.serviceLabel(), a.dataDir())
	return nil
}
