package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBootstrapAcceptance(t *testing.T) {
	if os.Getenv("AGENTQ_BOOTSTRAP_ACCEPTANCE") != "1" {
		t.Skip("opt-in network acceptance for the formal bootstrap entry")
	}
	i, o := installFixture(t)
	source, e := os.Getwd()
	if e != nil {
		t.Fatal(e)
	}
	env := installEnv(i.Home)
	run := func(args ...string) []byte {
		t.Helper()
		c := exec.Command("/bin/sh", append([]string{filepath.Join(source, "scripts", "bootstrap.sh")}, args...)...)
		c.Env = env
		c.Dir = source
		b, e := c.CombinedOutput()
		if e != nil {
			t.Fatalf("bootstrap: %v\n%s", e, b)
		}
		return b
	}
	// 对正式入口本身验收，而非仅测安装引擎。
	run("--yes", "--claude", o.Claude, "--extras")
	r := mustReceipt(t, i)
	if !r.Extras {
		t.Fatal("extras not selected")
	}
	for _, name := range []string{"check", "think", "hunt", "ui", "read", "write", "learn", "health"} {
		if _, e = os.Stat(filepath.Join(i.Home, ".claude", "skills", name, "SKILL.md")); e != nil {
			t.Fatal(e)
		}
	}
	run("--yes")
	updated := mustReceipt(t, i)
	if !updated.Extras {
		t.Fatal("update lost extras")
	}
	run("rollback", "--yes")
	if mustReceipt(t, i).Generation != r.Generation {
		t.Fatal("entry rollback mismatch")
	}
	run("remove", "--yes")
	if _, e = os.Stat(i.receiptPath()); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("entry uninstall failed")
	}
	run("restore", "--yes")
	if e = i.checkOwned(mustReceipt(t, i)); e != nil {
		t.Fatal(e)
	}
	t.Log("formal bootstrap full/extras/repeat/rollback/remove/restore passed; no Telegram or launchd")
}
func TestSettingsShapeAndAncestorConflict(t *testing.T) {
	for _, payload := range []string{`{"env":{"TOKEN":123}}`, `{"hooks":"bad"}`, `{"permissions":{"allow":"all"}}`, `{"statusLine":true}`, `null`} {
		t.Run(payload, func(t *testing.T) {
			i, o := installFixture(t)
			p := filepath.Join(i.Home, ".claude", "settings.json")
			writeInstall(t, p, payload, 0600)
			if e := i.setup(o); e == nil {
				t.Fatal("invalid settings accepted")
			}
			assertBytes(t, p, payload)
		})
	}
	i, o := installFixture(t)
	outside := t.TempDir()
	os.MkdirAll(filepath.Join(i.Home, ".claude"), 0700)
	os.Symlink(outside, filepath.Join(i.Home, ".claude", "skills"))
	if e := i.setup(o); e == nil || !strings.Contains(e.Error(), "ancestor") {
		t.Fatalf("ancestor conflict: %v", e)
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) != 0 {
		t.Fatal("wrote through external ancestor")
	}
}
func TestInstallAndTickMutuallyExclusive(t *testing.T) {
	i, o := installFixture(t)
	os.MkdirAll(filepath.Join(i.Home, ".agentq"), 0700)
	lock, e := acquireLock(filepath.Join(i.Home, ".agentq", "tick.lock"), 0)
	if e != nil || lock == nil {
		t.Fatal(e)
	}
	if e = i.setup(o); e == nil {
		t.Fatal("setup replaced installation while tick held lock")
	}
	lock.Close()
	mustInstall(t, i, o)
}
func TestLegacyServiceCommandsProtectManagedInstall(t *testing.T) {
	i, o := installFixture(t)
	mustInstall(t, i, o)
	cfg, _, _ := readInstallConfig(i.Home)
	a := &App{Home: i.Home, Cfg: cfg, Out: io.Discard, Launchctl: func(...string) ([]byte, error) { t.Fatal("legacy command reached launchctl"); return nil, nil }}
	if e := a.cmdInstall(); e == nil {
		t.Fatal("legacy install took over managed guard")
	}
	if e := a.cmdUninstall(); e == nil {
		t.Fatal("legacy uninstall removed managed entries")
	}
}
func TestWatchdogDetectsBrokenBinary(t *testing.T) {
	home := t.TempDir()
	writeInstall(t, filepath.Join(home, ".local", "bin", "agentq"), "#!/bin/sh\nexit 9\n", 0755)
	c := exec.Command("/bin/sh", "support/scripts/watchdog.sh")
	c.Env = installEnv(home)
	if e := c.Run(); e == nil {
		t.Fatal("broken binary not reported")
	}
	b, e := os.ReadFile(filepath.Join(home, ".agentq", "watchdog-report.txt"))
	if e != nil || !strings.Contains(string(b), "ERROR") {
		t.Fatalf("watchdog %v %s", e, b)
	}
}
func TestBackupRequiresExplicitVerifiedPrivateTarget(t *testing.T) {
	home := t.TempDir()
	bin := t.TempDir()
	log := filepath.Join(home, "calls")
	// fake git/gh，不读取凭据、不向远端写。
	writeInstall(t, filepath.Join(bin, "git"), "#!/bin/sh\nif [ \"$1\" = remote ]; then echo https://example.invalid/private-repo; else echo push >> "+shQuote(log)+"; fi\n", 0755)
	writeInstall(t, filepath.Join(bin, "gh"), "#!/bin/sh\necho '{\"isPrivate\":false}'\n", 0755)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	if e := backupProjects(home, io.Discard); e != nil {
		t.Fatal(e)
	}
	saveInstallJSON(filepath.Join(home, ".agentq", "backup.json"), map[string]any{"targets": []backupTarget{{home, "backup", "https://example.invalid/private-repo"}}})
	if e := backupProjects(home, io.Discard); e == nil {
		t.Fatal("public target accepted")
	}
	if _, e := os.Stat(log); !os.IsNotExist(e) {
		t.Fatal("pushed to public target")
	}
	writeInstall(t, filepath.Join(bin, "gh"), "#!/bin/sh\necho '{\"isPrivate\":true}'\n", 0755)
	if e := backupProjects(home, io.Discard); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(log)
	if strings.Count(string(b), "push") != 2 {
		t.Fatal("private target not backed up")
	}
}
func TestBundledAssetClosureAndReviewBoundary(t *testing.T) {
	files, e := bundledFiles()
	if e != nil {
		t.Fatal(e)
	}
	for _, n := range []string{"agent-task", "dispatch", "nightly-review", "new-project", "project-status", "no-ai-look", "prior-art", "feedback-triage"} {
		b, ok := files["skills/"+n+"/SKILL.md"]
		if !ok || !strings.Contains(string(b.Data), "description:") {
			t.Fatal("missing standard description", n)
		}
	}
	for _, n := range []string{"AGENTS", "BACKLOG", "DECISIONS", "LESSONS", "PROGRESS"} {
		if _, ok := files["templates/"+n+".md"]; !ok {
			t.Fatal(n)
		}
	}
	var lock map[string]any
	b, _ := installAssets.ReadFile("dependencies/claude.lock.json")
	if json.Unmarshal(b, &lock) != nil {
		t.Fatal("bad lock")
	}
	text := string(files["skills/agent-task/SKILL.md"].Data)
	for _, want := range []string{"medium", "实现与自验", "不再调用 Agent/Workflow", "第二轮只复验", "check"} {
		if !strings.Contains(text, want) {
			t.Fatal("missing review boundary", want)
		}
	}
	if strings.Contains(nightPrompt, "运行 `agentq resume`，再运行") {
		t.Fatal("night enables global scheduling")
	}
}
