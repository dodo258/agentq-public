package main

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

//go:embed skills templates support dependencies/claude.lock.json
var installAssets embed.FS

type installFile struct {
	Data []byte
	Mode fs.FileMode
}
type fileImage struct {
	Kind string      `json:"kind"`
	Data []byte      `json:"data,omitempty"`
	Mode fs.FileMode `json:"mode,omitempty"`
	Link string      `json:"link,omitempty"`
}
type installReceipt struct {
	Extras     bool                 `json:"extras,omitempty"`
	Version    int                  `json:"version"`
	Generation string               `json:"generation"`
	Scope      string               `json:"scope"`
	Owned      map[string]fileImage `json:"owned"`
	Digests    map[string]string    `json:"digests"`
	External   map[string]string    `json:"external_skills,omitempty"`
	Services   []string             `json:"services,omitempty"`
}
type installChange struct {
	Path   string    `json:"path"`
	Before fileImage `json:"before"`
	After  fileImage `json:"after"`
}
type installJournal struct {
	Changes     []installChange `json:"changes"`
	Completed   bool            `json:"completed"`
	Applied     int             `json:"applied"`
	OldServices []string        `json:"old_services,omitempty"`
	NewServices []string        `json:"new_services,omitempty"`
}
type setupOptions struct {
	Scope, Claude                                 string
	Yes, Plan, Services, Backup, Watchdog, Extras bool
}
type installer struct {
	Home       string
	Out        io.Writer
	Fetch      func(bool) (map[string]installFile, error)
	Check      func(string) error
	Launchctl  func(...string) ([]byte, error)
	AfterWrite func(int) error // 事务故障注入，仅测试使用
}

func (i *installer) share() string       { return filepath.Join(i.Home, ".local", "share", "agentq") }
func (i *installer) receiptPath() string { return filepath.Join(i.share(), "receipt.json") }
func (i *installer) pendingPath() string { return filepath.Join(i.share(), "pending.json") }
func image(path string) (fileImage, error) {
	st, e := os.Lstat(path)
	if errors.Is(e, os.ErrNotExist) {
		return fileImage{Kind: "absent"}, nil
	}
	if e != nil {
		return fileImage{}, e
	}
	if st.Mode()&os.ModeSymlink != 0 {
		s, e := os.Readlink(path)
		return fileImage{Kind: "link", Link: s}, e
	}
	if !st.Mode().IsRegular() {
		return fileImage{}, fmt.Errorf("conflict: %s is not a regular file or managed link", path)
	}
	b, e := os.ReadFile(path)
	return fileImage{Kind: "file", Data: b, Mode: st.Mode().Perm()}, e
}
func sameImage(a, b fileImage) bool {
	return a.Kind == b.Kind && a.Link == b.Link && a.Mode == b.Mode && string(a.Data) == string(b.Data)
}
func putImage(path string, v fileImage) error {
	if v.Kind == "absent" {
		e := os.Remove(path)
		if errors.Is(e, os.ErrNotExist) {
			return nil
		}
		return e
	}
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".agentq-*")
	if e != nil {
		return e
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if v.Kind == "link" {
		f.Close()
		os.Remove(tmp)
		if e = os.Symlink(v.Link, tmp); e != nil {
			return e
		}
	} else if v.Kind == "file" {
		if _, e = f.Write(v.Data); e != nil {
			f.Close()
			return e
		}
		if e = f.Sync(); e != nil {
			f.Close()
			return e
		}
		if e = f.Close(); e != nil {
			return e
		}
		if e = os.Chmod(tmp, v.Mode); e != nil {
			return e
		}
	} else {
		f.Close()
		return fmt.Errorf("unknown image kind %q", v.Kind)
	}
	return os.Rename(tmp, path)
}
func saveInstallJSON(path string, v any) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	return writeFileAtomic(path, append(b, '\n'))
}
func (i *installer) receipt() (installReceipt, error) {
	var r installReceipt
	b, e := os.ReadFile(i.receiptPath())
	if errors.Is(e, os.ErrNotExist) {
		return r, nil
	}
	if e != nil {
		return r, e
	}
	e = json.Unmarshal(b, &r)
	if e == nil {
		e = i.validateReceipt(r)
	}
	return r, e
}
func (i *installer) validateReceipt(r installReceipt) error {
	parent := filepath.Join(i.share(), "generations")
	if r.Version != 1 || (r.Scope != "full" && r.Scope != "skills") || len(r.Owned) == 0 || len(r.Digests) == 0 || filepath.Dir(r.Generation) != parent {
		return errors.New("invalid install receipt")
	}
	if digest, e := hex.DecodeString(r.Digests[r.Generation]); e != nil || len(digest) != sha256.Size {
		return errors.New("invalid generation digest in install receipt")
	}
	current := r.Owned[filepath.Join(i.share(), "current")]
	if current.Kind != "link" || current.Link != r.Generation {
		return errors.New("invalid generation entry in install receipt")
	}
	required := map[string]string{}
	for _, name := range []string{"templates", "support", "licenses", "dependencies"} {
		required[filepath.Join(i.share(), name)] = filepath.Join(r.Generation, name)
	}
	if r.Scope == "full" {
		required[filepath.Join(i.Home, ".local", "bin", "agentq")] = filepath.Join(r.Generation, "bin", "agentq")
	}
	skills, e := os.ReadDir(filepath.Join(r.Generation, "skills"))
	if e != nil || len(skills) == 0 {
		return errors.New("invalid skill generation in install receipt")
	}
	for _, skill := range skills {
		p := filepath.Join(i.Home, ".claude", "skills", skill.Name())
		if digest, external := r.External[p]; external {
			if b, e := hex.DecodeString(digest); e != nil || len(b) != sha256.Size {
				return errors.New("invalid external skill digest in install receipt")
			}
		} else {
			required[p] = filepath.Join(r.Generation, "skills", skill.Name())
		}
	}
	for p, target := range required {
		v := r.Owned[p]
		if v.Kind != "link" || v.Link != target {
			return fmt.Errorf("incomplete install receipt: %s", p)
		}
	}
	for p, v := range r.Owned {
		if e := checkInstallParents(i.Home, p); e != nil {
			return e
		}
		if v.Kind != "file" && v.Kind != "link" {
			return errors.New("invalid owned entry in install receipt")
		}
	}
	return nil
}
func (i *installer) checkExternal(r installReceipt) error {
	for p, want := range r.External {
		got, e := treeDigest(p)
		if e != nil || got != want {
			return fmt.Errorf("external required skill missing or changed: %s; preserved, not owned by agentq", p)
		}
	}
	return nil
}
func treeDigest(path string) (string, error) {
	h := sha256.New()
	e := filepath.WalkDir(path, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		st, e := d.Info()
		if e != nil {
			return e
		}
		if !st.Mode().IsRegular() {
			return fmt.Errorf("unexpected link or special file: %s", p)
		}
		b, e := os.ReadFile(p)
		if e != nil {
			return e
		}
		rel, _ := filepath.Rel(path, p)
		fmt.Fprintf(h, "%s\x00%o\x00%x\n", filepath.ToSlash(rel), st.Mode().Perm(), sha256.Sum256(b))
		return nil
	})
	return hex.EncodeToString(h.Sum(nil)), e
}
func (i *installer) checkOwned(r installReceipt) error {
	for p, want := range r.Owned {
		got, e := image(p)
		if e != nil {
			return e
		}
		if !sameImage(got, want) {
			return fmt.Errorf("conflict: local modification at %s; preserved, no changes made", p)
		}
	}
	for p, want := range r.Digests {
		got, e := treeDigest(p)
		if e != nil {
			return e
		}
		if got != want {
			return fmt.Errorf("conflict: vendor generation modified at %s; preserved", p)
		}
	}
	return nil
}

type installLocks struct{ install, tick *fileLock }

func (l *installLocks) Close() { l.tick.Close(); l.install.Close() }
func (i *installer) lock() (*installLocks, error) {
	if e := checkInstallParents(i.Home, filepath.Join(i.share(), "install.lock")); e != nil {
		return nil, e
	}
	if e := checkInstallParents(i.Home, filepath.Join(i.Home, ".agentq", "tick.lock")); e != nil {
		return nil, e
	}
	if e := os.MkdirAll(i.share(), 0700); e != nil {
		return nil, e
	}
	if e := os.MkdirAll(filepath.Join(i.Home, ".agentq"), 0700); e != nil {
		return nil, e
	}
	one, e := acquireLock(filepath.Join(i.share(), "install.lock"), 0)
	if e != nil || one == nil {
		return nil, e
	}
	two, e := acquireLock(filepath.Join(i.Home, ".agentq", "tick.lock"), 0)
	if e != nil || two == nil {
		one.Close()
		return nil, e
	}
	return &installLocks{one, two}, nil
}
func (i *installer) ctl(args ...string) ([]byte, error) {
	if i.Launchctl != nil {
		return i.Launchctl(args...)
	}
	a := &App{Home: i.Home}
	if e := a.checkRealLaunchd(); e != nil {
		return nil, e
	}
	return runLaunchctl(args...)
}
func (i *installer) loaded(label string) bool {
	_, e := i.ctl("print", fmt.Sprintf("gui/%d/%s", os.Getuid(), label))
	return e == nil
}
func (i *installer) unload(labels []string) error {
	for _, s := range labels {
		if i.loaded(s) {
			out, e := i.ctl("bootout", fmt.Sprintf("gui/%d/%s", os.Getuid(), s))
			if e != nil {
				return fmt.Errorf("bootout %s: %w %s", s, e, out)
			}
		}
	}
	return nil
}
func (i *installer) load(labels []string) error {
	for _, s := range labels {
		out, e := i.ctl("bootstrap", fmt.Sprintf("gui/%d", os.Getuid()), filepath.Join(i.Home, "Library", "LaunchAgents", s+".plist"))
		if e != nil {
			return fmt.Errorf("bootstrap %s: %w %s", s, e, out)
		}
	}
	return nil
}
func (i *installer) recover(j *installJournal) error {
	if e := i.unload(j.NewServices); e != nil {
		return e
	}
	// 已写的条目只在仍等于事务前/后像时恢复，避免吞掉中断后的用户修改。
	for n := len(j.Changes) - 1; n >= 0; n-- {
		c := j.Changes[n]
		if e := checkInstallParents(i.Home, c.Path); e != nil {
			return e
		}
		v, e := image(c.Path)
		if e != nil {
			return e
		}
		if sameImage(v, c.Before) {
			continue
		}
		if !sameImage(v, c.After) {
			return fmt.Errorf("recovery conflict: %s; backup retained", c.Path)
		}
		if e = putImage(c.Path, c.Before); e != nil {
			return e
		}
	}
	if e := i.load(j.OldServices); e != nil {
		return e
	}
	return os.Remove(i.pendingPath())
}
func (i *installer) transaction(changes []installChange, oldServices, newServices []string) error {
	for _, c := range changes {
		if e := checkInstallParents(i.Home, c.Path); e != nil {
			return e
		}
	}
	j := installJournal{Changes: changes, OldServices: oldServices, NewServices: newServices}
	if e := saveInstallJSON(i.pendingPath(), j); e != nil {
		return e
	}
	backup := filepath.Join(i.share(), "backups", time.Now().UTC().Format("20060102T150405.000000000")+".json")
	if e := saveInstallJSON(backup, j); e != nil {
		return e
	}
	apply := func() error {
		if e := i.unload(oldServices); e != nil {
			return e
		}
		for n, c := range changes {
			now, e := image(c.Path)
			if e != nil {
				return e
			}
			if !sameImage(now, c.Before) {
				return fmt.Errorf("conflict during install: %s", c.Path)
			}
			if e = putImage(c.Path, c.After); e != nil {
				return e
			}
			j.Applied = n + 1
			if e = saveInstallJSON(i.pendingPath(), j); e != nil {
				return e
			}
			if i.AfterWrite != nil {
				if e = i.AfterWrite(n + 1); e != nil {
					return e
				}
			}
		}
		return i.load(newServices)
	}
	if e := apply(); e != nil {
		if r := i.recover(&j); r != nil {
			return fmt.Errorf("%w; rollback failed: %v; run agentq restore", e, r)
		}
		return fmt.Errorf("%w; installation rolled back", e)
	}
	j.Completed = true
	if e := saveInstallJSON(backup, j); e != nil {
		return e
	}
	if e := os.Remove(i.pendingPath()); e != nil {
		return e
	}
	fmt.Fprintf(i.Out, "Installed transaction backup: %s\n", backup)
	return nil
}
func (i *installer) noPending() error {
	if _, e := os.Stat(i.pendingPath()); e == nil {
		return errors.New("interrupted installation: run agentq restore before continuing")
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	return nil
}
func validateHostSettings(home string) error {
	for _, p := range []string{filepath.Join(home, ".claude", "settings.json"), filepath.Join(home, ".claude", "settings.local.json")} {
		b, e := os.ReadFile(p)
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			return e
		}
		var v map[string]any
		if json.Unmarshal(b, &v) != nil || v == nil || !validSettingsShape(v) {
			return fmt.Errorf("invalid Claude settings: %s (preserved)", p)
		}
	}
	return nil
}
func readInstallConfig(home string) (Config, bool, error) {
	c := defaultConfig()
	b, e := os.ReadFile(filepath.Join(home, ".agentq", "config.json"))
	if errors.Is(e, os.ErrNotExist) {
		return c, false, nil
	}
	if e != nil {
		return c, true, e
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(b, &object) != nil || object == nil {
		return c, true, errors.New("invalid agentq config: expected object (preserved)")
	}
	if e = json.Unmarshal(b, &c); e != nil {
		return c, true, fmt.Errorf("invalid agentq config (preserved): %w", e)
	}
	return c, true, c.validate()
}
func installerIdle(home string) error {
	b, e := os.ReadFile(filepath.Join(home, ".agentq", "state.json"))
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(b, &object) != nil || object == nil {
		return errors.New("state unknown: expected object")
	}
	var st State
	if e = json.Unmarshal(b, &st); e != nil {
		return fmt.Errorf("state unknown: %w", e)
	}
	for _, j := range st.Jobs {
		if j == nil {
			return errors.New("state unknown: null job")
		}
		if j.Status == "running" {
			return errors.New("jobs still running; wait for completion before replacing installation")
		}
	}
	return nil
}

type engineReport struct {
	OSVersion     string `json:"os_version"`
	System        string `json:"system"`
	Arch          string `json:"architecture"`
	Git           string `json:"git"`
	Path          string `json:"cli_path"`
	Version       string `json:"cli_version"`
	Headless      bool   `json:"headless"`
	Authenticated bool   `json:"authenticated"`
	AuthMethod    string `json:"auth_method"`
}

func probePath(home, name string, env []string) (string, error) {
	if strings.ContainsRune(name, os.PathSeparator) {
		if !filepath.IsAbs(name) {
			name = filepath.Join(home, name)
		}
		return exec.LookPath(name)
	}
	for _, value := range env {
		if path, ok := strings.CutPrefix(value, "PATH="); ok {
			for _, dir := range filepath.SplitList(path) {
				if filepath.IsAbs(dir) {
					if p, e := exec.LookPath(filepath.Join(dir, name)); e == nil {
						return p, nil
					}
				}
			}
		}
	}
	return "", exec.ErrNotFound
}
func probeCommand(home, path string, args ...string) ([]byte, error) {
	return probeCommandEnv(home, path, installEnv(home), args...)
}
func probeCommandEnv(home, path string, env []string, args ...string) ([]byte, error) {
	path, e := probePath(home, path, env)
	if e != nil {
		return nil, e
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, path, args...)
	c.Dir = home
	c.Env = env
	return c.Output()
}
func installEnv(home string) []string {
	var env []string
	for _, s := range childEnv() {
		k, _, _ := strings.Cut(s, "=")
		if k != "HOME" && k != "CLAUDE_CONFIG_DIR" {
			env = append(env, s)
		}
	}
	return append(env, "HOME="+home)
}
func probeEngine(home, explicit string) (engineReport, error) {
	return probeEngineEnv(home, explicit, installEnv(home))
}
func probeEngineEnv(home, explicit string, env []string) (engineReport, error) {
	r := engineReport{System: runtime.GOOS, Arch: runtime.GOARCH}
	probe := func(path string, args ...string) ([]byte, error) {
		return probeCommandEnv(home, path, env, args...)
	}
	if runtime.GOOS != "darwin" || (runtime.GOARCH != "arm64" && runtime.GOARCH != "amd64") {
		return r, errors.New("this installer supports macOS arm64/amd64 only")
	}
	version, e := probe("/usr/bin/sw_vers", "-productVersion")
	if e != nil {
		return r, errors.New("cannot detect macOS version")
	}
	var major int
	if _, e = fmt.Sscanf(string(version), "%d", &major); e != nil || major < 13 {
		return r, errors.New("Claude requires macOS 13 or newer")
	}
	r.OSVersion = strings.TrimSpace(string(version))
	out, e := probe("git", "--version")
	if e != nil {
		return r, errors.New("Git required; install Apple Command Line Tools or Git first")
	}
	r.Git = strings.TrimSpace(string(out))
	var candidates []string
	if explicit != "" {
		candidates = []string{expandHome(home, explicit)}
	} else {
		if p, e := probePath(home, "claude", env); e == nil {
			candidates = append(candidates, p)
		}
		candidates = append(candidates, filepath.Join(home, ".local", "bin", "claude"), "/opt/homebrew/bin/claude", "/usr/local/bin/claude")
	}
	for _, p := range candidates {
		p, e = probePath(home, p, env)
		if e != nil {
			continue
		}
		p, e = filepath.Abs(p)
		if e != nil {
			continue
		}
		resolved, e := filepath.EvalSymlinks(p)
		if e != nil {
			continue
		}
		if strings.Contains(resolved, ".app/Contents/") {
			continue
		}
		out, e = probe(p, "--version")
		if e != nil || !strings.Contains(string(out), "Claude Code") {
			continue
		}
		r.Path = p
		r.Version = strings.TrimSpace(string(out))
		break
	}
	if r.Path == "" {
		return r, errors.New("no independently callable Claude CLI; Desktop app/ZIP is not an engine. Official stable install (installer confirmation required): curl -fsSL https://claude.ai/install.sh | bash -s stable; then claude auth login")
	}
	out, e = probe(r.Path, "--help")
	if e != nil {
		return r, errors.New("Claude CLI help failed")
	}
	text := string(out)
	r.Headless = strings.Contains(text, "--print") && strings.Contains(text, "--output-format") && strings.Contains(text, "--session-id") && strings.Contains(text, "--effort")
	if !r.Headless {
		return r, errors.New("CLI lacks required print/JSON/session/effort capability; update via its official installation owner")
	}
	out, e = probe(r.Path, "auth", "status")
	var a struct {
		LoggedIn   bool   `json:"loggedIn"`
		AuthMethod string `json:"authMethod"`
	}
	if json.Unmarshal(out, &a) == nil {
		r.Authenticated = a.LoggedIn
		r.AuthMethod = a.AuthMethod
	}
	if e != nil || !r.Authenticated {
		return r, errors.New("CLI authentication unavailable in the background environment; run claude auth login, then retry. No credentials were requested or changed")
	}
	return r, nil
}

func bundledFiles() (map[string]installFile, error) {
	files := map[string]installFile{}
	e := fs.WalkDir(installAssets, ".", func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		b, e := installAssets.ReadFile(p)
		if e != nil {
			return e
		}
		mode := fs.FileMode(0644)
		if strings.HasSuffix(p, ".sh") {
			mode = 0755
		}
		files[p] = installFile{b, mode}
		return nil
	})
	return files, e
}

type dependencyLock struct {
	ThirdPartySources []struct {
		URL, Commit string
		Modules     []struct {
			Name, InstallPolicy string
			Files               []struct{ Path, BlobSha string }
		}
		RequiredArtifacts []struct{ SourcePath, BlobSha, Target string }
	}
}

func fetchDependencies(extras bool) (map[string]installFile, error) {
	b, _ := installAssets.ReadFile("dependencies/claude.lock.json")
	var lock dependencyLock
	if e := json.Unmarshal(b, &lock); e != nil {
		return nil, e
	}
	files := map[string]installFile{}
	for _, s := range lock.ThirdPartySources {
		dir, e := os.MkdirTemp("", "agentq-dependency-*")
		if e != nil {
			return nil, e
		}
		defer os.RemoveAll(dir)
		run := func(args ...string) ([]byte, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.hooksPath=/dev/null", "-c", "credential.helper=", "-C", dir}, args...)...)
			cmd.Env = append(installEnv(os.Getenv("HOME")), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
			return cmd.Output()
		}
		if _, e = run("init", "-q"); e != nil {
			return nil, e
		}
		if _, e = run("fetch", "--depth=1", s.URL, s.Commit); e != nil {
			return nil, fmt.Errorf("fetch locked third party: %w", e)
		}
		add := func(path, sha, target string) error {
			data, e := run("show", s.Commit+":"+path)
			if e != nil {
				return e
			}
			h := sha1.New()
			fmt.Fprintf(h, "blob %d%c", len(data), 0)
			h.Write(data)
			if hex.EncodeToString(h.Sum(nil)) != sha {
				return fmt.Errorf("locked blob mismatch: %s", path)
			}
			mode := fs.FileMode(0644)
			if strings.HasSuffix(path, ".sh") {
				mode = 0755
			}
			files[target] = installFile{data, mode}
			return nil
		}
		for _, m := range s.Modules {
			if m.InstallPolicy != "required" && !extras {
				continue
			}
			for _, f := range m.Files {
				if e = add(f.Path, f.BlobSha, f.Path); e != nil {
					return nil, e
				}
			}
		}
		for _, a := range s.RequiredArtifacts {
			if e = add(a.SourcePath, a.BlobSha, "licenses/"+filepath.Base(a.Target)); e != nil {
				return nil, e
			}
		}
	}
	return files, nil
}
func (i *installer) stage(o setupOptions) (string, []string, error) {
	files, e := bundledFiles()
	if e != nil {
		return "", nil, e
	}
	fetch := i.Fetch
	if fetch == nil {
		fetch = fetchDependencies
	}
	deps, e := fetch(o.Extras)
	if e != nil {
		return "", nil, e
	}
	for p, v := range deps {
		files[p] = v
	}
	if o.Scope == "full" {
		self, e := os.Executable()
		if e != nil {
			return "", nil, e
		}
		b, e := os.ReadFile(self)
		if e != nil {
			return "", nil, e
		}
		files["bin/agentq"] = installFile{b, 0755}
	}
	generation, e := os.MkdirTemp(filepath.Join(i.share(), "generations"), "gen-")
	if e != nil {
		return "", nil, e
	}
	for p, v := range files {
		if filepath.IsAbs(p) || strings.Contains(p, "..") {
			return "", nil, fmt.Errorf("invalid asset path: %s", p)
		}
		dst := filepath.Join(generation, p)
		if e = os.MkdirAll(filepath.Dir(dst), 0700); e != nil {
			return "", nil, e
		}
		if e = os.WriteFile(dst, v.Data, v.Mode); e != nil {
			return "", nil, e
		}
		if e = os.Chmod(dst, v.Mode); e != nil {
			return "", nil, e
		}
	}
	entries, e := os.ReadDir(filepath.Join(generation, "skills"))
	if e != nil {
		return "", nil, e
	}
	var names []string
	for _, d := range entries {
		if d.IsDir() {
			names = append(names, d.Name())
		}
	}
	if o.Scope == "full" {
		check := i.Check
		if check == nil {
			check = checkCandidate
		}
		if e = check(filepath.Join(generation, "bin", "agentq")); e != nil {
			return "", nil, e
		}
	}
	return generation, names, nil
}
func checkCandidate(bin string) error {
	home, e := os.MkdirTemp("", "agentq-selfcheck-*")
	if e != nil {
		return e
	}
	defer os.RemoveAll(home)
	c := defaultConfig()
	c.Paused = true
	c.PatrolEnabled = false
	c.PatrolAnalyze = false
	c.NotifyEnabled = false
	if e = saveConfig(filepath.Join(home, ".agentq", "config.json"), c); e != nil {
		return e
	}
	for _, arg := range []string{"status", "tick"} {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		cmd := exec.CommandContext(ctx, bin, arg)
		cmd.Env = installEnv(home)
		cmd.Dir = home
		out, e := cmd.CombinedOutput()
		cancel()
		if e != nil {
			return fmt.Errorf("candidate selfcheck %s: %w %s", arg, e, out)
		}
	}
	return nil
}
func (i *installer) setup(o setupOptions) error {
	if o.Scope != "full" && o.Scope != "skills" {
		return errors.New("scope must be full or skills")
	}
	if o.Scope == "skills" && (o.Services || o.Backup || o.Watchdog) {
		return errors.New("services require full scope")
	}
	if e := validateHostSettings(i.Home); e != nil {
		return e
	}
	cfg, exists, e := readInstallConfig(i.Home)
	if e != nil {
		return e
	}
	explicit := o.Claude
	if explicit == "" && exists {
		explicit = cfg.ClaudePath
	}
	if o.Scope == "full" {
		r, e := probeEngine(i.Home, explicit)
		b, _ := json.MarshalIndent(r, "", "  ")
		fmt.Fprintln(i.Out, string(b))
		if e != nil {
			return e
		}
		if exists && o.Claude != "" {
			configured, err := exec.LookPath(expandHome(i.Home, cfg.ClaudePath))
			if err != nil {
				return errors.New("configured claude_path unavailable; update your configuration explicitly before selecting another engine")
			}
			configured, err = filepath.EvalSymlinks(configured)
			selected, selectedErr := filepath.EvalSymlinks(r.Path)
			if err != nil || selectedErr != nil || configured != selected {
				return errors.New("--claude differs from preserved claude_path; update your configuration explicitly, then retry")
			}
		}
		if !exists {
			cfg.ClaudePath = r.Path
		}
	}
	lock, e := i.lock()
	if e != nil {
		return e
	}
	if lock == nil {
		return errors.New("another installation is running")
	}
	defer lock.Close()
	if e = i.noPending(); e != nil {
		return e
	}
	if e = installerIdle(i.Home); e != nil {
		return e
	}
	old, e := i.receipt()
	if e != nil {
		return e
	}
	if e = i.checkOwned(old); e != nil {
		return e
	}
	if old.Version != 0 && old.Scope != o.Scope {
		return errors.New("scope change requires remove then setup; existing install preserved")
	}
	if old.Extras {
		o.Extras = true
	}
	for _, label := range old.Services {
		switch label {
		case cfg.LaunchdLabel:
			o.Services = true
		case cfg.LaunchdLabel + ".backup":
			o.Backup = true
		case cfg.LaunchdLabel + ".watchdog":
			o.Watchdog = true
		}
	}
	if o.Services {
		a := &App{Home: i.Home, Cfg: cfg}
		r, e := probeEngineEnv(i.Home, cfg.ClaudePath, a.launchdEnv())
		b, _ := json.MarshalIndent(r, "", "  ")
		fmt.Fprintf(i.Out, "Service environment (no inherited shell identity/proxy):\n%s\n", b)
		if e != nil {
			return fmt.Errorf("service environment unavailable: %w; configure durable CLI authentication/network settings explicitly, then retry; no secrets are written to plist", e)
		}
	}
	if o.Backup {
		path, e := exec.LookPath("gh")
		if e != nil {
			return errors.New("optional backup requires gh; not installed, no changes made")
		}
		version, e := probeCommand(i.Home, path, "--version")
		if e != nil {
			return fmt.Errorf("optional backup gh version check failed: %w", e)
		}
		fmt.Fprintf(i.Out, "Optional backup tool: %s (%s); remote authorization is checked at backup time\n", path, strings.TrimSpace(string(version)))
	}
	python, e := probeCommand(i.Home, "python3", "--version")
	if e != nil {
		return errors.New("Waza check/think helpers require python3; install Python first")
	}
	fmt.Fprintf(i.Out, "Support interpreter: %s\n", strings.TrimSpace(string(python)))
	if e = os.MkdirAll(filepath.Join(i.share(), "generations"), 0700); e != nil {
		return e
	}
	gen, names, e := i.stage(o)
	if e != nil {
		return e
	}
	desired := map[string]fileImage{}
	link := func(path, target string) { desired[path] = fileImage{Kind: "link", Link: target} }
	link(filepath.Join(i.share(), "current"), gen)
	for _, p := range []string{"templates", "support", "licenses", "dependencies"} {
		if _, e := os.Stat(filepath.Join(gen, p)); e == nil {
			link(filepath.Join(i.share(), p), filepath.Join(gen, p))
		}
	}
	for _, n := range names {
		link(filepath.Join(i.Home, ".claude", "skills", n), filepath.Join(gen, "skills", n))
	}
	if o.Scope == "full" {
		link(filepath.Join(i.Home, ".local", "bin", "agentq"), filepath.Join(gen, "bin", "agentq"))
	}
	var labels []string
	if len(old.Services) > 0 && !o.Services && !o.Backup && !o.Watchdog {
		return errors.New("existing services: repeat original --services/--backup/--watchdog selection, or remove first")
	}
	if o.Services || o.Backup || o.Watchdog {
		a := &App{Home: i.Home, Cfg: cfg}
		if i.Launchctl == nil {
			if e = a.checkRealLaunchd(); e != nil {
				return e
			}
		}
		var svcs []launchdService
		if o.Services {
			svcs = a.services()[:3]
			desired[a.guardPath()] = fileImage{Kind: "file", Data: []byte(i.generationGuard(old.Generation)), Mode: 0700}
		}
		if o.Backup {
			svcs = append(svcs, auxiliaryService(i.Home, cfg, "backup", 86400))
		}
		if o.Watchdog {
			svcs = append(svcs, auxiliaryService(i.Home, cfg, "watchdog", 1800))
		}
		for _, s := range svcs {
			known := false
			for _, label := range old.Services {
				if label == s.label {
					known = true
				}
			}
			if !known && i.loaded(s.label) {
				return fmt.Errorf("service conflict: %s already loaded outside this installer", s.label)
			}
			labels = append(labels, s.label)
			desired[a.plistPathFor(s.label)] = fileImage{Kind: "file", Data: []byte(s.plist), Mode: 0600}
		}
	}
	r := installReceipt{Version: 1, Generation: gen, Scope: o.Scope, Extras: o.Extras, Owned: map[string]fileImage{}, Digests: map[string]string{}, External: map[string]string{}, Services: labels}
	var changes []installChange
	keys := make([]string, 0, len(desired))
	for p := range desired {
		keys = append(keys, p)
	}
	sort.Strings(keys)
	for _, p := range keys {
		after := desired[p]
		if e := checkInstallParents(i.Home, p); e != nil {
			return e
		}
		before, e := image(p)
		if e != nil {
			if strings.Contains(p, filepath.Join(".claude", "skills")+string(os.PathSeparator)) {
				a, ae := treeDigest(p)
				b, be := treeDigest(after.Link)
				if ae == nil && be == nil && a == b {
					fmt.Fprintf(i.Out, "Keep identical external skill: %s\n", p)
					r.External[p] = a
					continue
				}
			}
			return e
		}
		_, owned := old.Owned[p]
		if before.Kind != "absent" && !owned {
			return fmt.Errorf("conflict: existing %s is not owned by agentq; preserved", p)
		}
		r.Owned[p] = after
		if !sameImage(before, after) {
			changes = append(changes, installChange{p, before, after})
		}
	}
	for p, before := range old.Owned {
		if _, ok := desired[p]; !ok {
			changes = append(changes, installChange{p, before, fileImage{Kind: "absent"}})
		}
	}
	digest, e := treeDigest(gen)
	if e != nil {
		return e
	}
	r.Digests[gen] = digest
	if !exists && o.Scope == "full" {
		cfg.Paused = true
		cfg.NightEnabled = false
		cfg.ReviewEnabled = false
		cfg.PatrolEnabled = false
		cfg.PatrolAnalyze = false
		cfg.NotifyEnabled = false
		cfg.PermissionMode = "default"
		b, _ := json.MarshalIndent(cfg, "", "  ")
		changes = append(changes, installChange{filepath.Join(i.Home, ".agentq", "config.json"), fileImage{Kind: "absent"}, fileImage{Kind: "file", Data: append(b, '\n'), Mode: 0600}})
	}
	before, e := image(i.receiptPath())
	if e != nil {
		return e
	}
	b, _ := json.MarshalIndent(r, "", "  ")
	changes = append(changes, installChange{i.receiptPath(), before, fileImage{Kind: "file", Data: append(b, '\n'), Mode: 0600}})
	fmt.Fprintf(i.Out, "Plan: scope=%s; generation=%s; %d skills; %d changed paths; services=%v\nKeep: authentication, permissions, hooks, statusline, user settings, jobs, secrets, existing scheduler switches. Telegram: optional, not requested.\nRollback: agentq rollback; interrupted transaction: agentq restore.\n", o.Scope, gen, len(names), len(changes), labels)
	for _, change := range changes {
		fmt.Fprintf(i.Out, "  %s: %s -> %s\n", change.Path, change.Before.Kind, change.After.Kind)
	}
	fmt.Fprintf(i.Out, "Before activation: restricted transaction backup in %s; user data/settings are not part of code rollback\n", filepath.Join(i.share(), "backups"))
	if o.Plan {
		fmt.Fprintln(i.Out, "Plan only: no activation")
		return nil
	}
	if !o.Yes {
		return errors.New("review plan then repeat with --yes to authorize these installation changes")
	}
	if old.Version != 0 {
		p := filepath.Join(i.share(), "previous.json")
		v, e := image(p)
		if e != nil {
			return e
		}
		data, _ := json.MarshalIndent(old, "", "  ")
		changes = append(changes, installChange{p, v, fileImage{Kind: "file", Data: append(data, '\n'), Mode: 0600}})
	}
	if e = i.transaction(changes, old.Services, labels); e != nil {
		return e
	}
	fmt.Fprintf(i.Out, "Installed %s. New installs remain paused; existing switches preserved. Start a natural-language session with claude; agentq status/run/logs are management commands.\n", o.Scope)
	return nil
}
func (i *installer) generationGuard(previous string) string {
	recovery := filepath.Join(previous, "bin", "agentq")
	if previous == "" {
		recovery = filepath.Join(i.Home, ".local", "bin", "agentq")
	}
	return "#!/bin/sh\n" + "bin=" + shQuote(filepath.Join(i.Home, ".local", "bin", "agentq")) + "\nfails=" + shQuote(filepath.Join(i.Home, ".agentq", "guard-fails")) + "\n\"$bin\" tick\nrc=$?\n[ \"$rc\" = 0 ] && { rm -f \"$fails\"; exit 0; }\nn=0; [ ! -f \"$fails\" ] || read n < \"$fails\"\ncase $n in ''|*[!0-9]*) n=0;; esac\nn=$((n+1)); printf '%s\\n' \"$n\" > \"$fails\"\nif [ \"$n\" -ge 2 ]; then " + shQuote(recovery) + " rollback --yes && rm -f \"$fails\"; fi\nexit \"$rc\"\n"
}
func runInstaller(cmd string, args []string, out, errout io.Writer) int {
	home, e := os.UserHomeDir()
	if e != nil {
		fmt.Fprintln(errout, e)
		return 1
	}
	if custom := os.Getenv("CLAUDE_CONFIG_DIR"); custom != "" && filepath.Clean(expandHome(home, custom)) != filepath.Join(home, ".claude") {
		fmt.Fprintln(errout, "agentq: custom CLAUDE_CONFIG_DIR requires an explicit host migration; default projection not activated")
		return 1
	}
	i := &installer{Home: home, Out: out}
	f := flag.NewFlagSet(cmd, flag.ContinueOnError)
	f.SetOutput(errout)
	var o setupOptions
	f.BoolVar(&o.Yes, "yes", false, "confirm planned changes")
	if cmd == "setup" {
		f.StringVar(&o.Scope, "scope", "full", "full or skills")
		f.StringVar(&o.Claude, "claude", "", "independent CLI path")
		f.BoolVar(&o.Plan, "plan", false, "prepare and print plan without activating")
		f.BoolVar(&o.Services, "services", false, "install inert tick/night/review launchd services")
		f.BoolVar(&o.Backup, "backup", false, "install opt-in private Git backup service")
		f.BoolVar(&o.Watchdog, "watchdog", false, "install opt-in local watchdog service")
		f.BoolVar(&o.Extras, "extras", false, "include optional locked Waza skills")
	}
	if e = f.Parse(args); e != nil {
		return 2
	}
	if f.NArg() != 0 {
		fmt.Fprintln(errout, "unexpected positional arguments")
		return 2
	}
	switch cmd {
	case "setup":
		e = i.setup(o)
	case "doctor":
		e = i.doctor()
	case "restore":
		e = i.restore(o.Yes)
	case "rollback":
		e = i.rollback(o.Yes)
	case "remove":
		e = i.remove(o.Yes)
	}
	if e != nil {
		fmt.Fprintln(errout, "agentq:", e)
		return 1
	}
	return 0
}
