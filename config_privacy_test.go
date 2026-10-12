package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

const (
	privacySecret       = "test-auth-token-9f7a4e2c"
	privacyNestedSecret = "test-header-secret-5b8d1c3a"
)

func privacyGit(t *testing.T, dir string, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode(), string(out)
	}
	t.Fatalf("git %v: %v", args, err)
	return -1, ""
}

func privacyGitOK(t *testing.T, dir string, args ...string) string {
	t.Helper()
	code, out := privacyGit(t, dir, args...)
	if code != 0 {
		t.Fatalf("git %v failed (%d): %s", args, code, out)
	}
	return strings.TrimSpace(out)
}

func privacyWrite(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func privacySettings(token string) string {
	return `{
  "env": {
    "ANTHROPIC_BASE_URL": "https://gateway.example.invalid",
    "ANTHROPIC_AUTH_TOKEN": "` + token + `"
  },
  "permissions": {
    "defaultMode": "bypassPermissions",
    "allow": ["Bash(go test ./...)"]
  },
  "hooks": {
    "SessionStart": [{
      "hooks": [{"type": "command", "command": "~/.claude/scripts/session-start.sh"}]
    }]
  },
  "extraKnownMarketplaces": {
    "example": {
      "source": {
        "source": "url",
        "url": "https://plugins.example.invalid/catalog.json",
        "headers": {"Authorization": "Bearer ` + privacyNestedSecret + `"},
        "vendorApiKey": "` + privacyNestedSecret + `",
        "accessKeyId": "` + privacyNestedSecret + `"
      }
    }
  },
  "enabledPlugins": {"example@example": true},
  "customCommand": "helper --token ` + token + `",
  "customUrl": "https://gateway.example.invalid/path?token=` + privacyNestedSecret + `"
}
`
}

func privacyRepo(t *testing.T) (home, dir string) {
	t.Helper()
	home = t.TempDir()
	dir = filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	privacyGitOK(t, dir, "init", "-q", "-b", "main")
	privacyWrite(t, filepath.Join(dir, "settings.json"), privacySettings(privacySecret), 0o644)
	privacyWrite(t, filepath.Join(dir, "settings.local.json"), `{"authToken":"`+strings.Join([]string{"local", "only", "secret"}, "-")+`"}`+"\n", 0o600)
	privacyWrite(t, filepath.Join(dir, ".claude.json"), `{"oauthToken":"`+strings.Join([]string{"oauth", "only", "secret"}, "-")+`"}`+"\n", 0o600)
	privacyWrite(t, filepath.Join(dir, "plugins", "auth.json"), `{"token":"`+strings.Join([]string{"plugin", "only", "secret"}, "-")+`"}`+"\n", 0o600)
	privacyGitOK(t, dir, "add", "settings.json", "settings.local.json", ".claude.json", "plugins/auth.json")
	privacyGitOK(t, dir, "commit", "-q", "-m", "initial")
	return home, dir
}

func privacyRun(t *testing.T, home, dir, command string, explicitDir bool) (int, string) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	script, err := filepath.Abs(filepath.Join("scripts", "config_privacy.py"))
	if err != nil {
		t.Fatal(err)
	}
	args := []string{script, command}
	if explicitDir {
		args = append(args, "--config-dir", dir)
	}
	cmd := exec.Command(python, args...)
	cmd.Env = []string{
		"HOME=" + home,
		"PATH=" + os.Getenv("PATH"),
		"LANG=C.UTF-8",
		"GIT_CONFIG_NOSYSTEM=1",
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode(), string(out)
	}
	t.Fatalf("config_privacy.py: %v", err)
	return -1, ""
}

func privacyRunOK(t *testing.T, home, dir, command string) string {
	t.Helper()
	code, out := privacyRun(t, home, dir, command, true)
	if code != 0 {
		t.Fatalf("%s failed (%d): %s", command, code, out)
	}
	return out
}

func privacyIndexPaths(t *testing.T, dir string) []string {
	t.Helper()
	raw := privacyGitOK(t, dir, "ls-files")
	if raw == "" {
		return nil
	}
	return strings.Split(raw, "\n")
}

func privacyLooseObjectCount(t *testing.T, dir string) string {
	t.Helper()
	for _, line := range strings.Split(privacyGitOK(t, dir, "count-objects", "-v"), "\n") {
		if strings.HasPrefix(line, "count: ") {
			return line
		}
	}
	t.Fatal("git count-objects did not report count")
	return ""
}

func privacyContains(paths []string, wanted string) bool {
	for _, path := range paths {
		if path == wanted {
			return true
		}
	}
	return false
}

func TestConfigPrivacyMigrationKeepsLiveSettingsAndSanitizesProjection(t *testing.T) {
	home, dir := privacyRepo(t)
	settingsPath := filepath.Join(dir, "settings.json")
	before, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeHead := privacyGitOK(t, dir, "rev-parse", "HEAD")

	out := privacyRunOK(t, home, dir, "migrate")
	if got := privacyGitOK(t, dir, "rev-parse", "HEAD"); got != beforeHead {
		t.Fatal("migration created a commit")
	}
	if strings.Contains(out, privacySecret) || strings.Contains(out, privacyNestedSecret) {
		t.Fatal("command output exposed a secret")
	}
	after, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("migration changed live settings.json")
	}
	info, err := os.Stat(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("settings.json mode = %o, want 600", info.Mode().Perm())
	}

	paths := privacyIndexPaths(t, dir)
	for _, private := range []string{"settings.json", "settings.local.json", ".claude.json", "plugins/auth.json"} {
		if privacyContains(paths, private) {
			t.Errorf("private path remains tracked: %s", private)
		}
	}
	if !privacyContains(paths, "config/settings.example.json") {
		t.Fatal("safe projection was not staged")
	}

	projectionRaw, err := os.ReadFile(filepath.Join(dir, "config", "settings.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(projectionRaw), privacySecret) || strings.Contains(string(projectionRaw), privacyNestedSecret) || strings.Contains(string(projectionRaw), "Authorization") {
		t.Fatal("projection contains a secret-bearing field or value")
	}
	var source, projection map[string]any
	if err := json.Unmarshal(before, &source); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(projectionRaw, &projection); err != nil {
		t.Fatal(err)
	}
	if _, ok := projection["env"]; ok {
		t.Fatal("projection must omit env as a unit")
	}
	if !reflect.DeepEqual(source["permissions"], projection["permissions"]) {
		t.Fatalf("permission policy changed: %#v", projection["permissions"])
	}
	if !reflect.DeepEqual(source["hooks"], projection["hooks"]) {
		t.Fatalf("hook scheduling policy changed: %#v", projection["hooks"])
	}
	if got := source["env"].(map[string]any)["ANTHROPIC_AUTH_TOKEN"]; got != privacySecret {
		t.Fatal("live authentication value was not preserved")
	}

	privacyGitOK(t, dir, "commit", "-q", "-m", "safe migration")
	for _, secret := range []string{privacySecret, privacyNestedSecret, strings.Join([]string{"oauth", "only", "secret"}, "-"), strings.Join([]string{"plugin", "only", "secret"}, "-")} {
		code, _ := privacyGit(t, dir, "grep", "-F", "-q", "--", secret, "HEAD")
		if code == 0 {
			t.Error("a secret value entered the migrated commit")
		} else if code != 1 {
			t.Fatalf("git grep returned %d", code)
		}
	}
}

func TestConfigPrivacyRollbackRestoresPreMigrationState(t *testing.T) {
	home, dir := privacyRepo(t)
	projectionPath := filepath.Join(dir, "config", "settings.example.json")
	oldProjection := "{\n  \"old\": true\n}\n"
	privacyWrite(t, projectionPath, oldProjection, 0o644)
	privacyGitOK(t, dir, "add", "config/settings.example.json")
	privacyGitOK(t, dir, "commit", "-q", "-m", "old projection")
	beforeIndex := privacyGitOK(t, dir, "write-tree")
	settingsPath := filepath.Join(dir, "settings.json")
	beforeSettings, _ := os.ReadFile(settingsPath)

	privacyRunOK(t, home, dir, "migrate")
	privacyWrite(t, filepath.Join(dir, "scripts", "pending.sh"), "#!/bin/sh\nexit 0\n", 0o755)
	privacyRunOK(t, home, dir, "prepare")
	privacyRunOK(t, home, dir, "rollback")

	if got := privacyGitOK(t, dir, "write-tree"); got != beforeIndex {
		t.Fatal("rollback did not restore the original index")
	}
	if got, _ := os.ReadFile(projectionPath); string(got) != oldProjection {
		t.Fatal("rollback did not restore the previous projection")
	}
	if got, _ := os.ReadFile(settingsPath); string(got) != string(beforeSettings) {
		t.Fatal("rollback changed settings.json")
	}
	info, _ := os.Stat(settingsPath)
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o644 {
		t.Fatalf("rollback did not restore the original mode: %o", info.Mode().Perm())
	}
	for _, private := range []string{"settings.json", "settings.local.json", ".claude.json", "plugins/auth.json"} {
		if !privacyContains(privacyIndexPaths(t, dir), private) {
			t.Errorf("rollback did not restore tracked path %s", private)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "config-privacy-state")); !os.IsNotExist(err) {
		t.Fatal("rollback state should be removed")
	}
}

func TestConfigPrivacyMigrationFailureLeavesNoPartialState(t *testing.T) {
	home, dir := privacyRepo(t)
	settingsPath := filepath.Join(dir, "settings.json")
	before, _ := os.ReadFile(settingsPath)
	beforeTree := privacyGitOK(t, dir, "write-tree")
	privacyWrite(t, filepath.Join(dir, "config"), "not a directory\n", 0o644)

	code, out := privacyRun(t, home, dir, "migrate", true)
	if code == 0 {
		t.Fatalf("migration should fail: %s", out)
	}
	if strings.Contains(out, privacySecret) {
		t.Fatal("failure output exposed a secret")
	}
	if got := privacyGitOK(t, dir, "write-tree"); got != beforeTree {
		t.Fatal("failed migration changed the index")
	}
	if got, _ := os.ReadFile(settingsPath); string(got) != string(before) {
		t.Fatal("failed migration changed settings.json")
	}
	info, _ := os.Stat(settingsPath)
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o644 {
		t.Fatalf("failed migration changed settings mode: %o", info.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "config-privacy-state")); !os.IsNotExist(err) {
		t.Fatal("failed migration left rollback state")
	}
}

func TestConfigPrivacyIsIdempotentAndIgnoresSecretOnlyChanges(t *testing.T) {
	home, dir := privacyRepo(t)
	privacyRunOK(t, home, dir, "migrate")
	firstProjection, _ := os.ReadFile(filepath.Join(dir, "config", "settings.example.json"))
	firstTree := privacyGitOK(t, dir, "write-tree")
	privacyRunOK(t, home, dir, "migrate")
	if got := privacyGitOK(t, dir, "write-tree"); got != firstTree {
		t.Fatal("second migration changed the staged tree")
	}
	privacyGitOK(t, dir, "commit", "-q", "-m", "safe migration")

	rotated := "rotated-auth-token-2d8c6a4e"
	privacyWrite(t, filepath.Join(dir, "settings.json"), privacySettings(rotated), 0o600)
	privacyRunOK(t, home, dir, "prepare")
	if _, err := os.Stat(filepath.Join(dir, ".git", "config-privacy-state")); !os.IsNotExist(err) {
		t.Fatal("committed migration should discard stale rollback state")
	}
	secondProjection, _ := os.ReadFile(filepath.Join(dir, "config", "settings.example.json"))
	if string(secondProjection) != string(firstProjection) {
		t.Fatal("secret-only change altered the projection")
	}
	code, _ := privacyGit(t, dir, "diff", "--cached", "--quiet")
	if code != 0 {
		t.Fatal("secret-only change entered the next commit")
	}
}

func TestConfigPrivacyPrepareUsesBoundedAllowlist(t *testing.T) {
	home, dir := privacyRepo(t)
	privacyRunOK(t, home, dir, "migrate")
	privacyGitOK(t, dir, "commit", "-q", "-m", "safe migration")

	privacyWrite(t, filepath.Join(dir, "scripts", "new-tool.sh"), "#!/bin/sh\nexit 0\n", 0o755)
	privacyWrite(t, filepath.Join(dir, "scripts", "__pycache__", "generated.pyc"), "generated cache", 0o600)
	privacyWrite(t, filepath.Join(dir, "skills", "example", "SKILL.md"), "# Example\n", 0o644)
	privacyWrite(t, filepath.Join(dir, "runtime", "state.json"), `{"token":"`+strings.Join([]string{"runtime", "secret"}, "-")+`"}`+"\n", 0o600)
	privacyWrite(t, filepath.Join(dir, "settings.local.json"), `{"token":"`+strings.Join([]string{"local", "secret"}, "-")+`"}`+"\n", 0o600)
	privacyWrite(t, filepath.Join(dir, ".claude.json"), `{"token":"`+strings.Join([]string{"oauth", "secret"}, "-")+`"}`+"\n", 0o600)
	privacyWrite(t, filepath.Join(dir, "plugins", "auth.json"), `{"token":"`+strings.Join([]string{"plugin", "secret"}, "-")+`"}`+"\n", 0o600)

	privacyRunOK(t, home, dir, "prepare")
	paths := privacyIndexPaths(t, dir)
	for _, allowed := range []string{"scripts/new-tool.sh", "skills/example/SKILL.md", "config/settings.example.json"} {
		if !privacyContains(paths, allowed) {
			t.Errorf("allowlisted path was not staged: %s", allowed)
		}
	}
	for _, excluded := range []string{"runtime/state.json", "settings.local.json", ".claude.json", "plugins/auth.json", "scripts/__pycache__/generated.pyc"} {
		if privacyContains(paths, excluded) {
			t.Errorf("excluded path was staged: %s", excluded)
		}
	}
	privacyGitOK(t, dir, "commit", "-q", "-m", "allowlisted backup")

	privacyWrite(t, filepath.Join(dir, "scripts", "leak.txt"), privacyNestedSecret+"\n", 0o600)
	beforeTree := privacyGitOK(t, dir, "write-tree")
	beforeObjects := privacyLooseObjectCount(t, dir)
	code, out := privacyRun(t, home, dir, "prepare", true)
	if code == 0 {
		t.Fatalf("prepare should reject an allowlisted secret copy: %s", out)
	}
	if strings.Contains(out, privacyNestedSecret) {
		t.Fatal("prepare failure output exposed the secret")
	}
	if got := privacyGitOK(t, dir, "write-tree"); got != beforeTree {
		t.Fatal("failed prepare changed the index")
	}
	if privacyContains(privacyIndexPaths(t, dir), "scripts/leak.txt") {
		t.Fatal("failed prepare staged the secret-bearing file")
	}
	if afterObjects := privacyLooseObjectCount(t, dir); afterObjects != beforeObjects {
		t.Fatal("failed prepare wrote the secret-bearing blob to the object database")
	}
}

func TestConfigPrivacySupportsFakeHomeAndExplicitConfigDir(t *testing.T) {
	home := t.TempDir()
	defaultDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(defaultDir, 0o755); err != nil {
		t.Fatal(err)
	}
	privacyWrite(t, filepath.Join(defaultDir, "settings.json"), privacySettings(privacySecret), 0o600)
	code, out := privacyRun(t, home, defaultDir, "project", false)
	if code != 0 {
		t.Fatalf("fake HOME projection failed (%d): %s", code, out)
	}
	if _, err := os.Stat(filepath.Join(defaultDir, "config", "settings.example.json")); err != nil {
		t.Fatal(err)
	}

	explicitDir := filepath.Join(t.TempDir(), "it’s custom config")
	if err := os.MkdirAll(explicitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	privacyWrite(t, filepath.Join(explicitDir, "settings.json"), privacySettings(privacySecret), 0o600)
	code, out = privacyRun(t, home, explicitDir, "project", true)
	if code != 0 {
		t.Fatalf("explicit config dir projection failed (%d): %s", code, out)
	}
	if _, err := os.Stat(filepath.Join(explicitDir, "config", "settings.example.json")); err != nil {
		t.Fatal(err)
	}
}

func TestConfigPrivacyBackupCommand(t *testing.T) {
	home, dir := privacyRepo(t)
	privacyRunOK(t, home, dir, "migrate")
	for _, name := range []string{"config_privacy.py", "backup-config.sh"} {
		b, err := os.ReadFile(filepath.Join("scripts", name))
		if err != nil {
			t.Fatal(err)
		}
		privacyWrite(t, filepath.Join(dir, "scripts", name), string(b), 0o755)
	}
	privacyGitOK(t, dir, "config", "user.name", "backup-test")
	privacyGitOK(t, dir, "config", "user.email", "backup"+string(rune(64))+"invalid.example")
	run := func() {
		cmd := exec.Command("sh", filepath.Join(dir, "scripts", "backup-config.sh"), dir)
		cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1"}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("backup helper: %v\n%s", err, out)
		}
	}
	run()
	first := privacyGitOK(t, dir, "rev-parse", "HEAD")
	settingsPath := filepath.Join(dir, "settings.json")
	b, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	updated := strings.ReplaceAll(string(b), privacySecret, privacySecret+"-changed")
	privacyWrite(t, settingsPath, updated, 0o600)
	run()
	if got := privacyGitOK(t, dir, "rev-parse", "HEAD"); got != first {
		t.Fatal("secret-only change created a backup commit")
	}
	if code, _ := privacyGit(t, dir, "grep", "-F", "-q", "--", privacySecret, "HEAD"); code != 1 {
		t.Fatalf("backup tree contains secret (grep status %d)", code)
	}
	if privacyContains(privacyIndexPaths(t, dir), "settings.json") {
		t.Fatal("live settings entered backup index")
	}
}

func TestConfigPrivacyIgnoreNegationAndRollback(t *testing.T) {
	home, dir := privacyRepo(t)
	ignore := "/*\n!/.gitignore\n!/settings.json\n!/settings.local.json\n!/.claude.json\n!/plugins/\n!/scripts/\n"
	privacyWrite(t, filepath.Join(dir, ".gitignore"), ignore, 0o644)
	privacyGitOK(t, dir, "add", ".gitignore")
	privacyGitOK(t, dir, "commit", "-q", "-m", "legacy negations")
	privacyRunOK(t, home, dir, "migrate")
	for _, name := range []string{"settings.json", "settings.local.json", ".claude.json", "plugins"} {
		if code, _ := privacyGit(t, dir, "check-ignore", "--no-index", "-q", "--", name); code != 0 {
			t.Fatalf("private path not effectively ignored: %s", name)
		}
	}
	privacyRunOK(t, home, dir, "rollback")
	b, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil || string(b) != ignore {
		t.Fatal("rollback changed original ignore rules")
	}
	privacyRunOK(t, home, dir, "migrate")
	privacyGitOK(t, dir, "commit", "-q", "-m", "safe migration")
	privacyGitOK(t, dir, "add", "-A")
	for _, name := range []string{"settings.json", "settings.local.json", ".claude.json", "plugins/auth.json"} {
		if privacyContains(privacyIndexPaths(t, dir), name) {
			t.Fatalf("private path restaged: %s", name)
		}
	}
	// Removing the protection later must block backup, not silently continue.
	privacyWrite(t, filepath.Join(dir, ".gitignore"), ignore, 0o644)
	if code, _ := privacyRun(t, home, dir, "prepare", true); code == 0 {
		t.Fatal("backup accepted ineffective private ignore rules")
	}
}

func TestConfigPrivacyMissingPluginsDirectory(t *testing.T) {
	home, dir := privacyRepo(t)
	plugins := filepath.Join(dir, "plugins")
	if err := os.RemoveAll(plugins); err != nil {
		t.Fatal(err)
	}
	privacyRunOK(t, home, dir, "migrate")
	privacyGitOK(t, dir, "commit", "-q", "-m", "without plugins")
	privacyRunOK(t, home, dir, "prepare")
	if err := os.Mkdir(plugins, 0o700); err != nil {
		t.Fatal(err)
	}
	privacyRunOK(t, home, dir, "prepare")
	if err := os.Remove(plugins); err != nil {
		t.Fatal(err)
	}
	privacyRunOK(t, home, dir, "prepare")
	if privacyContains(privacyIndexPaths(t, dir), "settings.json") {
		t.Fatal("private settings restaged")
	}
}
