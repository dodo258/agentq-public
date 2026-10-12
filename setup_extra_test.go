package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallerRejectsNullConfigurationAndState(t *testing.T) {
	for _, file := range []string{"config.json", "state.json"} {
		t.Run(file, func(t *testing.T) {
			i, o := installFixture(t)
			p := filepath.Join(i.Home, ".agentq", file)
			writeInstall(t, p, "null", 0600)
			if e := i.setup(o); e == nil {
				t.Fatal("null accepted")
			}
			assertBytes(t, p, "null")
		})
	}
}
func TestInstallerReportsCustomHostDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "custom"))
	var out bytes.Buffer
	if code := runInstaller("doctor", nil, &out, &out); code == 0 || !strings.Contains(out.String(), "custom CLAUDE_CONFIG_DIR") {
		t.Fatalf("%d %s", code, out.String())
	}
	if _, e := os.Stat(filepath.Join(home, ".local")); !os.IsNotExist(e) {
		t.Fatal("created install while host scope was ambiguous")
	}
}
func TestAuxiliaryServiceDefinitions(t *testing.T) {
	home := filepath.Join(t.TempDir(), "path & spaces")
	cfg := defaultConfig()
	for _, kind := range []string{"backup", "watchdog"} {
		s := auxiliaryService(home, cfg, kind, 1800)
		p := filepath.Join(t.TempDir(), kind+".plist")
		writeInstall(t, p, s.plist, 0600)
		if b, e := exec.Command("/usr/bin/plutil", "-lint", p).CombinedOutput(); e != nil {
			t.Fatalf("%s: %v %s", kind, e, b)
		}
		if kind == "watchdog" && !strings.Contains(s.plist, "<string>/bin/sh</string>") {
			t.Fatal("watchdog requires agentq to execute")
		}
	}
}
func TestGenerationGuardCallsPreviousExecutable(t *testing.T) {
	home := t.TempDir()
	i := &installer{Home: home}
	previous := filepath.Join(home, "previous generation")
	marker := filepath.Join(home, "rollback-called")
	writeInstall(t, filepath.Join(home, ".local", "bin", "agentq"), "#!/bin/sh\nexit 7\n", 0755)
	writeInstall(t, filepath.Join(previous, "bin", "agentq"), "#!/bin/sh\nprintf '%s\\n' \"$*\" > "+shQuote(marker)+"\n", 0755)
	guard := filepath.Join(home, ".agentq", "tick-guard.sh")
	writeInstall(t, guard, i.generationGuard(previous), 0700)
	for n := 0; n < 2; n++ {
		c := exec.Command("/bin/sh", guard)
		c.Env = installEnv(home)
		if e := c.Run(); e == nil {
			t.Fatal("guard hid tick failure")
		}
		if n == 0 {
			if _, e := os.Stat(marker); !os.IsNotExist(e) {
				t.Fatal("rollback too early")
			}
		}
	}
	assertBytes(t, marker, "rollback --yes\n")
}

func TestSetupCannotProbeDifferentEngineThanPreservedConfig(t *testing.T) {
	i, o := installFixture(t)
	mustInstall(t, i, o)
	before, _ := os.ReadFile(i.receiptPath())
	config, _ := os.ReadFile(filepath.Join(i.Home, ".agentq", "config.json"))
	o.Claude = filepath.Join(i.Home, "another engine", "claude")
	writeInstall(t, o.Claude, probeClaude, 0755)
	if e := i.setup(o); e == nil || !strings.Contains(e.Error(), "claude_path") {
		t.Fatalf("different runtime engine silently accepted: %v", e)
	}
	assertBytes(t, i.receiptPath(), string(before))
	assertBytes(t, filepath.Join(i.Home, ".agentq", "config.json"), string(config))
}

func TestProbeConfiguredCommandName(t *testing.T) {
	i, o := installFixture(t)
	t.Setenv("PATH", filepath.Dir(o.Claude)+":"+os.Getenv("PATH"))
	r, e := probeEngine(i.Home, "claude")
	if e != nil || r.Path != o.Claude {
		t.Fatalf("configured command name was not resolved through PATH: %+v %v", r, e)
	}
}

func TestInstallPlanAndOptionalToolFailure(t *testing.T) {
	i, o := installFixture(t)
	var out bytes.Buffer
	i.Out = &out
	o.Plan = true
	mustInstall(t, i, o)
	for _, text := range []string{"Support interpreter:", filepath.Join(i.Home, ".local", "bin", "agentq"), "absent -> link", "Before activation:", "Plan only: no activation"} {
		if !strings.Contains(out.String(), text) {
			t.Fatal("missing plan detail", text)
		}
	}
	writeInstall(t, filepath.Join(filepath.Dir(o.Claude), "gh"), "#!/bin/sh\nexit 9\n", 0755)
	t.Setenv("PATH", filepath.Dir(o.Claude)+":"+os.Getenv("PATH"))
	o.Plan, o.Backup = false, true
	if e := i.setup(o); e == nil || !strings.Contains(e.Error(), "gh version check failed") {
		t.Fatalf("broken selected optional tool not detected: %v", e)
	}
	if mustReceipt(t, i).Version != 0 {
		t.Fatal("activated before optional tool check")
	}
}

func TestRuleScanFollowsInstalledSkillLinks(t *testing.T) {
	home := t.TempDir()
	skill := filepath.Join(home, ".local", "share", "agentq", "generations", "fixture", "skills", "sample")
	writeInstall(t, filepath.Join(skill, "SKILL.md"), "fixture: 先问用户\n", 0644)
	link := filepath.Join(home, ".claude", "skills", "sample")
	if e := os.MkdirAll(filepath.Dir(link), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(skill, link); e != nil {
		t.Fatal(e)
	}
	c := exec.Command("/bin/bash", "support/scripts/ask-rule-scan.sh")
	c.Env = installEnv(home)
	b, e := c.CombinedOutput()
	if e != nil || !strings.Contains(string(b), "sample/SKILL.md:1:fixture: 先问用户") {
		t.Fatalf("installed skill omitted by rule scan: %v %s", e, b)
	}
}

func TestDoctorSkillsOnlyDoesNotRequireEngine(t *testing.T) {
	i, o := installFixture(t)
	o.Scope = "skills"
	mustInstall(t, i, o)
	// 独立 CLI 存在但未登录，不应把成功的 skills-only 安装判成故障。
	writeInstall(t, o.Claude, strings.Replace(probeClaude, `"loggedIn":true`, `"loggedIn":false`, 1), 0755)
	t.Setenv("PATH", filepath.Dir(o.Claude)+":"+os.Getenv("PATH"))
	if e := i.doctor(); e != nil {
		t.Fatalf("skills-only doctor requires authentication: %v", e)
	}
}
