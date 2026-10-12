package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAssignedTaskKeepsExecutorRole(t *testing.T) {
	for _, prompt := range []string{buildPrompt("/fixture", "不是当前 dispatch 会话直接实现"), buildResumePrompt("/fixture", "只派活不等待")} {
		if !strings.Contains(prompt, "执行者") || !strings.Contains(prompt, "亲自") || !strings.Contains(prompt, "dispatch 或 agentq run") {
			t.Fatal("assigned task can be mistaken for dispatch entry", prompt)
		}
	}
	for _, skill := range []string{"agent-task", "dispatch"} {
		b, e := installAssets.ReadFile("skills/" + skill + "/SKILL.md")
		if e != nil || !strings.Contains(string(b), "同一任务") || !strings.Contains(string(b), "执行合同") {
			t.Fatal("missing assigned-task role boundary", skill, e)
		}
	}
}

func TestRecoveryRejectsIncompleteReceipts(t *testing.T) {
	for _, command := range []string{"rollback", "restore"} {
		t.Run(command, func(t *testing.T) {
			i, o := installFixture(t)
			mustInstall(t, i, o)
			r := mustReceipt(t, i)
			file := "previous.json"
			if command == "rollback" {
				mustInstall(t, i, o)
				r = mustReceipt(t, i)
			} else {
				if e := i.remove(true); e != nil {
					t.Fatal(e)
				}
				file = "removed.json"
			}
			before := map[string]fileImage{}
			for p := range r.Owned {
				before[p], _ = image(p)
			}
			before[i.receiptPath()], _ = image(i.receiptPath())
			writeInstall(t, filepath.Join(i.share(), file), `{"version":1}`, 0600)
			var e error
			if command == "rollback" {
				e = i.rollback(true)
			} else {
				e = i.restore(true)
			}
			if e == nil {
				t.Fatal("incomplete recovery receipt activated")
			}
			for p, want := range before {
				got, e := image(p)
				if e != nil || !sameImage(got, want) {
					t.Fatal("invalid recovery changed active entries", p, e)
				}
			}
		})
	}
}

func TestRecoveryRejectsPartialGenerationEntries(t *testing.T) {
	i, o := installFixture(t)
	mustInstall(t, i, o)
	old := mustReceipt(t, i)
	before, _ := os.ReadFile(i.receiptPath())
	for _, path := range []string{filepath.Join(i.Home, ".local", "bin", "agentq"), filepath.Join(i.Home, ".claude", "skills", "dispatch")} {
		bad := mustReceipt(t, i)
		delete(bad.Owned, path)
		if e := saveInstallJSON(filepath.Join(i.share(), "previous.json"), bad); e != nil {
			t.Fatal(e)
		}
		if e := i.rollback(true); e == nil {
			t.Fatal("partial generation receipt accepted", path)
		}
		assertBytes(t, i.receiptPath(), string(before))
		if e := i.checkOwned(old); e != nil {
			t.Fatal(e)
		}
	}
}

func TestProbeUsesTargetEnvironmentPATH(t *testing.T) {
	i, o := installFixture(t)
	bad := t.TempDir()
	writeInstall(t, filepath.Join(bad, "claude"), "#!/bin/sh\nexit 9\n", 0755)
	t.Setenv("PATH", bad+":"+os.Getenv("PATH"))
	env := []string{"HOME=" + i.Home, "PATH=" + filepath.Dir(o.Claude) + ":/usr/bin:/bin"}
	r, e := probeEngineEnv(i.Home, "claude", env)
	if e != nil || r.Path != o.Claude {
		t.Fatalf("probe resolved shell PATH instead of target PATH: %+v %v", r, e)
	}
}

func TestServiceProbeRejectsShellOnlyIdentity(t *testing.T) {
	i, o := installFixture(t)
	cli := strings.Replace(probeClaude, `auth) echo '{"loggedIn":true,"authMethod":"claude.ai"}';;`, `auth) if [ "$CLAUDE_CODE_OAUTH_TOKEN" = fixture ]; then echo '{"loggedIn":true}'; else echo '{"loggedIn":false}'; fi;;`, 1)
	writeInstall(t, o.Claude, cli, 0755)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "fixture")
	i.Launchctl = func(...string) ([]byte, error) {
		t.Fatal("services reached launchctl before durable authentication check")
		return nil, nil
	}
	o.Services = true
	if e := i.setup(o); e == nil || !strings.Contains(e.Error(), "service environment") {
		t.Fatalf("shell-only identity accepted for background services: %v", e)
	}
	if mustReceipt(t, i).Version != 0 {
		t.Fatal("service install activated without durable identity")
	}
}

func TestDoctorRequiresInstallationReceipt(t *testing.T) {
	i, o := installFixture(t)
	t.Setenv("PATH", filepath.Dir(o.Claude)+":"+os.Getenv("PATH"))
	if e := i.doctor(); e == nil || !strings.Contains(e.Error(), "no managed installation") {
		t.Fatalf("absent installation reported healthy: %v", e)
	}
}

func TestDoctorChecksExternalRequiredSkills(t *testing.T) {
	for _, change := range []string{"missing", "modified"} {
		t.Run(change, func(t *testing.T) {
			i, o := installFixture(t)
			b, _ := installAssets.ReadFile("skills/dispatch/SKILL.md")
			p := filepath.Join(i.Home, ".claude", "skills", "dispatch", "SKILL.md")
			writeInstall(t, p, string(b), 0644)
			mustInstall(t, i, o)
			if e := i.doctor(); e != nil {
				t.Fatal(e)
			}
			if _, ok := mustReceipt(t, i).Owned[filepath.Dir(p)]; ok {
				t.Fatal("external skill adopted")
			}
			if change == "missing" {
				os.RemoveAll(filepath.Dir(p))
			} else {
				writeInstall(t, p, "user edit", 0644)
			}
			if e := i.doctor(); e == nil {
				t.Fatal("broken external required skill reported healthy")
			}
			// 依赖诊断不等于所有权，用户修改外部技能不妨碍只卸载受管入口。
			if e := i.remove(true); e != nil {
				t.Fatal(e)
			}
			if change == "modified" {
				assertBytes(t, p, "user edit")
			}
		})
	}
}

func TestManualPermissionModeAccepted(t *testing.T) {
	c := defaultConfig()
	c.PermissionMode = "manual"
	if e := c.validate(); e != nil {
		t.Fatal("official manual alias rejected", e)
	}
	if !strings.Contains(strings.Join(claudeArgs(c, "p", "p", "t", "", ""), " "), "--permission-mode manual") {
		t.Fatal("manual selection not passed to CLI")
	}
}

func TestSkillsRemovalPrintsWorkingRestoreEntry(t *testing.T) {
	i, o := installFixture(t)
	o.Scope = "skills"
	mustInstall(t, i, o)
	var out bytes.Buffer
	i.Out = &out
	if e := i.remove(true); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(out.String(), "sh scripts/bootstrap.sh restore --yes") {
		t.Fatalf("skills removal has no source restore entry: %s", out.String())
	}
	if e := i.restore(true); e != nil {
		t.Fatal(e)
	}
}
