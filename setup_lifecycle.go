package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func (i *installer) activate(r installReceipt, old installReceipt) error {
	if e := i.validateReceipt(r); e != nil {
		return e
	}
	if e := i.checkExternal(r); e != nil {
		return e
	}
	if e := i.checkOwned(old); e != nil {
		return e
	}
	for p, want := range r.Digests {
		got, e := treeDigest(p)
		if e != nil {
			return e
		}
		if got != want {
			return fmt.Errorf("restore target modified: %s", p)
		}
	}
	var changes []installChange
	keys := map[string]bool{}
	for p := range old.Owned {
		keys[p] = true
	}
	for p := range r.Owned {
		keys[p] = true
	}
	var paths []string
	for p := range keys {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		before, e := image(p)
		if e != nil {
			return e
		}
		after, ok := r.Owned[p]
		if !ok {
			after = fileImage{Kind: "absent"}
		}
		if _, managed := old.Owned[p]; !managed && before.Kind != "absent" {
			return fmt.Errorf("restore conflict: %s", p)
		}
		if !sameImage(before, after) {
			changes = append(changes, installChange{p, before, after})
		}
	}
	before, e := image(i.receiptPath())
	if e != nil {
		return e
	}
	b, e := json.MarshalIndent(r, "", "  ")
	if e != nil {
		return e
	}
	changes = append(changes, installChange{i.receiptPath(), before, fileImage{Kind: "file", Data: append(b, '\n'), Mode: 0600}})
	return i.transaction(changes, old.Services, r.Services)
}
func (i *installer) rollback(yes bool) error {
	if !yes {
		return errors.New("rollback requires --yes; code/skills/services change, data and configuration stay")
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
	if old.Version == 0 {
		return errors.New("no active installation")
	}
	var r installReceipt
	b, e := os.ReadFile(filepath.Join(i.share(), "previous.json"))
	if e != nil {
		return e
	}
	if e = json.Unmarshal(b, &r); e != nil {
		return e
	}
	if r.Version != 1 {
		return errors.New("invalid previous receipt")
	}
	if e = i.activate(r, old); e != nil {
		return e
	}
	if e = os.Remove(filepath.Join(i.share(), "previous.json")); e != nil {
		return e
	}
	fmt.Fprintf(i.Out, "Rolled back complete generation to %s; data/config unchanged\n", r.Generation)
	return nil
}
func (i *installer) remove(yes bool) error {
	if !yes {
		return errors.New("remove requires --yes; removes owned entries/services only, keeps data/config and recovery generations")
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
	r, e := i.receipt()
	if e != nil {
		return e
	}
	if r.Version == 0 {
		return errors.New("no managed installation to remove")
	}
	if e = i.checkOwned(r); e != nil {
		return e
	}
	if e = saveInstallJSON(filepath.Join(i.share(), "removed.json"), r); e != nil {
		return e
	}
	var changes []installChange
	var keys []string
	for p := range r.Owned {
		keys = append(keys, p)
	}
	sort.Strings(keys)
	for _, p := range keys {
		changes = append(changes, installChange{p, r.Owned[p], fileImage{Kind: "absent"}})
	}
	before, e := image(i.receiptPath())
	if e != nil {
		return e
	}
	changes = append(changes, installChange{i.receiptPath(), before, fileImage{Kind: "absent"}})
	if e = i.transaction(changes, r.Services, nil); e != nil {
		return e
	}
	restore := shQuote(filepath.Join(r.Generation, "bin", "agentq")) + " restore --yes"
	if r.Scope == "skills" {
		restore = "sh scripts/bootstrap.sh restore --yes (from the source checkout)"
	}
	fmt.Fprintf(i.Out, "Removed owned entries/services. Keep data/config/generations. Restore using: %s\n", restore)
	return nil
}
func (i *installer) restore(yes bool) error {
	if !yes {
		return errors.New("restore requires --yes; first review installation backup and preserved conflicts")
	}
	lock, e := i.lock()
	if e != nil {
		return e
	}
	if lock == nil {
		return errors.New("another installation is running")
	}
	defer lock.Close()
	if e = installerIdle(i.Home); e != nil {
		return e
	}
	b, e := os.ReadFile(i.pendingPath())
	if e == nil {
		var j installJournal
		if e = json.Unmarshal(b, &j); e != nil {
			return e
		}
		if e = i.recover(&j); e != nil {
			return e
		}
		fmt.Fprintln(i.Out, "Recovered interrupted transaction; previous installation restored")
		return nil
	}
	if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	old, e := i.receipt()
	if e != nil {
		return e
	}
	if old.Version != 0 {
		return errors.New("active installation exists; use rollback, not uninstall restore")
	}
	b, e = os.ReadFile(filepath.Join(i.share(), "removed.json"))
	if e != nil {
		return e
	}
	var r installReceipt
	if e = json.Unmarshal(b, &r); e != nil {
		return e
	}
	if r.Version != 1 {
		return errors.New("invalid removed receipt")
	}
	if e = i.activate(r, old); e != nil {
		return e
	}
	fmt.Fprintln(i.Out, "Restored removed installation; data and user config unchanged")
	return nil
}
func (i *installer) doctor() error {
	if e := validateHostSettings(i.Home); e != nil {
		return e
	}
	if e := i.noPending(); e != nil {
		return e
	}
	cfg, _, e := readInstallConfig(i.Home)
	if e != nil {
		return e
	}
	r, e := i.receipt()
	if e != nil {
		return e
	}
	if r.Version == 0 {
		return errors.New("no managed installation; run bootstrap setup before doctor")
	}
	if e = i.checkOwned(r); e != nil {
		return e
	}
	if e = i.checkExternal(r); e != nil {
		return e
	}
	fmt.Fprintf(i.Out, "Installation: scope=%s generation=%s; settings unchanged; Telegram optional\n", r.Scope, r.Generation)
	if r.Scope == "skills" {
		fmt.Fprintln(i.Out, "Skills-only integrity checked; background CLI/authentication not required")
		return nil
	}
	report, e := probeEngine(i.Home, cfg.ClaudePath)
	b, _ := json.MarshalIndent(report, "", "  ")
	fmt.Fprintln(i.Out, string(b))
	return e
}

func checkInstallParents(home, path string) error {
	rel, e := filepath.Rel(home, path)
	if e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("install path outside HOME: %s", path)
	}
	for p := filepath.Dir(path); p != home && p != filepath.Dir(p); p = filepath.Dir(p) {
		st, e := os.Lstat(p)
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			return e
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("install ancestor is a symlink: %s; not following it", p)
		}
	}
	return nil
}
