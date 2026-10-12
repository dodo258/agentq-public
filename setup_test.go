package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const probeClaude = `#!/bin/sh
case "$1" in
 --version) echo '2.1.296 (Claude Code)';;
 --help) echo '--print --output-format --session-id --effort';;
 auth) echo '{"loggedIn":true,"authMethod":"claude.ai"}';;
 *) echo '{"is_error":false,"result":"ready"}';;
esac
`

func installFixture(t *testing.T) (*installer, setupOptions) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "ignored-config"))
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	cli := filepath.Join(home, "engine with spaces", "claude")
	writeInstall(t, cli, probeClaude, 0755)
	i := &installer{Home: home, Out: io.Discard, Check: func(string) error { return nil }, Fetch: func(bool) (map[string]installFile, error) {
		m := map[string]installFile{"licenses/Waza-LICENSE": {[]byte("test license"), 0644}}
		for _, n := range []string{"check", "think", "hunt", "ui"} {
			m["skills/"+n+"/SKILL.md"] = installFile{[]byte("---\nname: " + n + "\ndescription: fixture\n---\n"), 0644}
		}
		return m, nil
	}}
	return i, setupOptions{Scope: "full", Claude: cli, Yes: true}
}
func writeInstall(t *testing.T, p, text string, mode os.FileMode) {
	t.Helper()
	if e := os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(p, []byte(text), mode); e != nil {
		t.Fatal(e)
	}
}
func mustReceipt(t *testing.T, i *installer) installReceipt {
	t.Helper()
	r, e := i.receipt()
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func mustInstall(t *testing.T, i *installer, o setupOptions) {
	t.Helper()
	if e := i.setup(o); e != nil {
		t.Fatal(e)
	}
}
func assertBytes(t *testing.T, p, want string) {
	t.Helper()
	b, e := os.ReadFile(p)
	if e != nil || string(b) != want {
		t.Fatalf("%s: %v got %q want %q", p, e, b, want)
	}
}
func TestSetupNoTelegramLifecycle(t *testing.T) {
	i, o := installFixture(t)
	mustInstall(t, i, o)
	first := mustReceipt(t, i)
	cfg, exists, e := readInstallConfig(i.Home)
	if e != nil || !exists || !cfg.Paused || cfg.NotifyEnabled || cfg.NightEnabled || cfg.ReviewEnabled || cfg.PatrolEnabled || cfg.PermissionMode != "default" {
		t.Fatalf("unsafe new config: %+v %v", cfg, e)
	}
	if _, e = os.Stat(filepath.Join(i.Home, ".agentq", "secrets", "telegram.env")); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("Telegram requested")
	}
	if len(first.Services) != 0 {
		t.Fatal("default installed services")
	}
	for _, p := range []string{"templates/BACKLOG.md", "support/scripts/with-lock.sh", "skills/no-ai-look/references/research-2026-10-11.md", "skills/check/SKILL.md", "licenses/Waza-LICENSE"} {
		if _, e = os.Stat(filepath.Join(first.Generation, p)); e != nil {
			t.Fatal(e)
		}
	}
	// 用户定制和状态不属于 generation，不回滚/卸载。
	configPath := filepath.Join(i.Home, ".agentq", "config.json")
	b, _ := os.ReadFile(configPath)
	var raw map[string]any
	json.Unmarshal(b, &raw)
	raw["paused"] = false
	raw["notify_enabled"] = false
	raw["user_extension"] = "keep"
	saveInstallJSON(configPath, raw)
	custom, _ := os.ReadFile(configPath)
	settings := `{"permissions":{"defaultMode":"default"},"hooks":{"Stop":[{"hooks":[{"type":"command","command":"user-hook"}]}]},"statusLine":{"type":"command","command":"user-status"}}`
	writeInstall(t, filepath.Join(i.Home, ".claude", "settings.json"), settings, 0600)
	writeInstall(t, filepath.Join(i.Home, ".agentq", "jobs", "old", "artifact"), "keep data", 0600)
	mustInstall(t, i, o)
	second := mustReceipt(t, i)
	if second.Generation == first.Generation {
		t.Fatal("update did not stage a generation")
	}
	assertBytes(t, configPath, string(custom))
	assertBytes(t, filepath.Join(i.Home, ".claude", "settings.json"), settings)
	if e = i.rollback(true); e != nil {
		t.Fatal(e)
	}
	if mustReceipt(t, i).Generation != first.Generation {
		t.Fatal("rollback did not restore entire generation")
	}
	assertBytes(t, configPath, string(custom))
	if e = i.remove(true); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Lstat(filepath.Join(i.Home, ".local", "bin", "agentq")); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("owned binary remains")
	}
	assertBytes(t, filepath.Join(i.Home, ".agentq", "jobs", "old", "artifact"), "keep data")
	assertBytes(t, configPath, string(custom))
	assertBytes(t, filepath.Join(i.Home, ".claude", "settings.json"), settings)
	if e = i.restore(true); e != nil {
		t.Fatal(e)
	}
	if mustReceipt(t, i).Generation != first.Generation {
		t.Fatal("restore mismatch")
	}
}
func TestSetupConflictsPreserveContent(t *testing.T) {
	for _, kind := range []string{"existing-skill", "vendor-edit", "link-edit", "invalid-settings", "invalid-config", "running", "null-job"} {
		t.Run(kind, func(t *testing.T) {
			i, o := installFixture(t)
			var target string
			switch kind {
			case "existing-skill":
				target = filepath.Join(i.Home, ".claude", "skills", "dispatch", "SKILL.md")
				writeInstall(t, target, "custom skill", 0644)
			case "invalid-settings":
				target = filepath.Join(i.Home, ".claude", "settings.json")
				writeInstall(t, target, "{ invalid", 0600)
			case "invalid-config":
				target = filepath.Join(i.Home, ".agentq", "config.json")
				writeInstall(t, target, "{ invalid", 0600)
			case "running":
				target = filepath.Join(i.Home, ".agentq", "state.json")
				writeInstall(t, target, `{"jobs":[{"status":"running"}]}`, 0600)
			case "null-job":
				target = filepath.Join(i.Home, ".agentq", "state.json")
				writeInstall(t, target, `{"jobs":[null]}`, 0600)
			default:
				mustInstall(t, i, o)
				r := mustReceipt(t, i)
				if kind == "vendor-edit" {
					target = filepath.Join(r.Generation, "skills", "dispatch", "SKILL.md")
					writeInstall(t, target, "local vendor edit", 0644)
				} else {
					target = filepath.Join(i.Home, ".claude", "skills", "dispatch")
					os.Remove(target)
					os.Symlink(filepath.Join(i.Home, "user-skill"), target)
				}
			}
			before, e := image(target)
			if e != nil {
				t.Fatal(e)
			}
			receiptBefore, _ := os.ReadFile(i.receiptPath())
			if e = i.setup(o); e == nil {
				t.Fatal("expected conflict")
			}
			after, e := image(target)
			if e != nil || !sameImage(before, after) {
				t.Fatal("conflicting content changed")
			}
			receiptAfter, _ := os.ReadFile(i.receiptPath())
			if !bytes.Equal(receiptBefore, receiptAfter) {
				t.Fatal("active install changed")
			}
		})
	}
}
func TestSetupFailureRollbackAndInterruptedRecovery(t *testing.T) {
	i, o := installFixture(t)
	mustInstall(t, i, o)
	old := mustReceipt(t, i)
	before, _ := os.ReadFile(i.receiptPath())
	i.AfterWrite = func(n int) error {
		if n == 3 {
			return errors.New("injected write failure")
		}
		return nil
	}
	if e := i.setup(o); e == nil || !strings.Contains(e.Error(), "rolled back") {
		t.Fatalf("want rolled back failure: %v", e)
	}
	after, _ := os.ReadFile(i.receiptPath())
	if !bytes.Equal(before, after) {
		t.Fatal("failed update changed receipt")
	}
	if e := i.checkOwned(old); e != nil {
		t.Fatal(e)
	}
	i.AfterWrite = nil
	i.Check = func(string) error { return errors.New("candidate does not start") }
	if e := i.setup(o); e == nil {
		t.Fatal("bad candidate activated")
	}
	after, _ = os.ReadFile(i.receiptPath())
	if !bytes.Equal(before, after) {
		t.Fatal("bad candidate changed receipt")
	}
	// 模拟崩溃停在一次已完成 rename 后，下一次安装必须拒绝，restore 能恢复。
	p := filepath.Join(i.Home, ".local", "bin", "agentq")
	v, _ := image(p)
	replacement := fileImage{Kind: "link", Link: "/missing-candidate"}
	j := installJournal{Changes: []installChange{{p, v, replacement}}, Applied: 1}
	saveInstallJSON(i.pendingPath(), j)
	putImage(p, replacement)
	if e := i.noPending(); e == nil {
		t.Fatal("ignored interrupted installation")
	}
	if e := i.restore(true); e != nil {
		t.Fatal(e)
	}
	now, _ := image(p)
	if !sameImage(now, v) {
		t.Fatal("interrupted restore failed")
	}
}
func TestSetupIdenticalExternalSkillNotOwned(t *testing.T) {
	i, o := installFixture(t)
	b, _ := installAssets.ReadFile("skills/dispatch/SKILL.md")
	p := filepath.Join(i.Home, ".claude", "skills", "dispatch", "SKILL.md")
	writeInstall(t, p, string(b), 0644)
	mustInstall(t, i, o)
	r := mustReceipt(t, i)
	if _, ok := r.Owned[filepath.Dir(p)]; ok {
		t.Fatal("adopted external skill")
	}
	if e := i.remove(true); e != nil {
		t.Fatal(e)
	}
	assertBytes(t, p, string(b))
}
func TestSetupOptionalServicesRollback(t *testing.T) {
	i, o := installFixture(t)
	loaded := map[string]bool{}
	fail := false
	i.Launchctl = func(args ...string) ([]byte, error) {
		switch args[0] {
		case "print":
			label := filepath.Base(args[1])
			if !loaded[label] {
				return nil, errors.New("not loaded")
			}
		case "bootout":
			delete(loaded, filepath.Base(args[1]))
		case "bootstrap":
			if fail {
				fail = false
				return nil, errors.New("injected bootstrap failure")
			}
			loaded[strings.TrimSuffix(filepath.Base(args[2]), ".plist")] = true
		}
		return nil, nil
	}
	o.Services = true
	o.Backup = true
	o.Watchdog = true
	mustInstall(t, i, o)
	r := mustReceipt(t, i)
	if len(loaded) != 5 || len(r.Services) != 5 {
		t.Fatalf("services: %v", loaded)
	}
	o.Services = false
	o.Backup = false
	o.Watchdog = false // 升级保留所选服务。
	mustInstall(t, i, o)
	if len(loaded) != 5 {
		t.Fatal("upgrade dropped service")
	}
	current := mustReceipt(t, i)
	fail = true
	if e := i.setup(o); e == nil {
		t.Fatal("expected bootstrap failure")
	}
	if len(loaded) != 5 || mustReceipt(t, i).Generation != current.Generation {
		t.Fatal("failed bootstrap lost old services")
	}
	if e := i.remove(true); e != nil {
		t.Fatal(e)
	}
	if len(loaded) != 0 {
		t.Fatal("service remains")
	}
	if e := i.restore(true); e != nil {
		t.Fatal(e)
	}
	if len(loaded) != 5 {
		t.Fatal("restore lost service")
	}
}
func TestProbeIndependentCLIAndAuthentication(t *testing.T) {
	i, o := installFixture(t)
	r, e := probeEngine(i.Home, o.Claude)
	if e != nil || !r.Headless || !r.Authenticated || r.Path != o.Claude {
		t.Fatalf("probe %+v %v", r, e)
	}
	private := filepath.Join(i.Home, "Desktop.app", "Contents", "MacOS", "claude")
	writeInstall(t, private, probeClaude, 0755)
	if _, e = probeEngine(i.Home, private); e == nil {
		t.Fatal("accepted private app CLI")
	}
	writeInstall(t, o.Claude, strings.Replace(probeClaude, `"loggedIn":true`, `"loggedIn":false`, 1), 0755)
	if _, e = probeEngine(i.Home, o.Claude); e == nil || !strings.Contains(e.Error(), "authentication") {
		t.Fatal("logged out accepted")
	}
}
func TestSetupSkillsOnlyAndPlan(t *testing.T) {
	i, o := installFixture(t)
	o.Scope = "skills"
	o.Plan = true
	mustInstall(t, i, o)
	if mustReceipt(t, i).Version != 0 {
		t.Fatal("plan activated")
	}
	o.Plan = false
	mustInstall(t, i, o)
	if _, e := os.Stat(filepath.Join(i.Home, ".local", "bin", "agentq")); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("skills scope installed background binary")
	}
	if e := i.remove(true); e != nil {
		t.Fatal(e)
	}
	if e := i.restore(true); e != nil {
		t.Fatal(e)
	}
}
func TestSetupPermissionSelection(t *testing.T) {
	c := defaultConfig()
	c.PermissionMode = "default"
	args := strings.Join(claudeArgs(c, "p", "p", "t", "", ""), " ")
	if strings.Contains(args, "bypassPermissions") || !strings.Contains(args, "--permission-mode default") {
		t.Fatal(args)
	}
	c.PermissionMode = "invalid"
	if c.validate() == nil {
		t.Fatal("invalid mode accepted")
	}
}
func TestGenericBacklogDecisionSection(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "sample")
	os.MkdirAll(filepath.Join(p, ".git"), 0700)
	writeInstall(t, filepath.Join(p, "docs", "BACKLOG.md"), "## 等用户决定\n- [ ] 明确授权 —— 推荐保留\n", 0644)
	ds, errs := scanDecisions(root)
	if len(errs) != 0 || len(ds) != 1 {
		t.Fatalf("%v %v", ds, errs)
	}
}
