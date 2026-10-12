# Install the complete Claude work system

[简体中文](install.zh-CN.md) · [Component inventory](components.md) · [Complete reference](reference.en.md) · [llms.txt](../llms.txt)

## Recommended install: v0.1.0 Release

Download the Release installer to a temporary file, then run it:

```sh
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
curl -fL https://github.com/dodo258/agentq-public/releases/download/v0.1.0/install.sh -o "$tmp"
sh "$tmp"
```

`install.sh` checks the macOS architecture and the presence of `curl`, `python3`, `git`, and `shasum`, downloads the matching archive and `SHA256SUMS`, verifies the archive, then runs the packaged `agentq setup --yes "$@"` from a temporary directory. It does not pipe a remote script directly into a shell, require Go or Node, or silently install Git, Python 3, or Claude Code CLI.

To install from source instead:

```sh
git clone https://github.com/dodo258/agentq-public.git
cd agentq-public
sh scripts/bootstrap.sh --yes
```

The source entry point builds a temporary candidate and invokes the same `agentq setup` engine. A source build requires the Go version declared in `go.mod`. Both paths install the executable, complete skill directories, support scripts, five templates, locked dependencies, and license notices. Neither copies the author's `.claude`, signs in, nor asks for a token.

One sentence for an Agent: **Read this repository's `llms.txt` and install guide, download the v0.1.0 Release `install.sh` to a temporary file, and run it; preserve my configuration, leave scheduling paused, and respect my authorization for services, permissions, and authentication.** Release and source installs use the same installation engine.

## Prerequisites and detection

- macOS 13+, arm64 or amd64. Real acceptance currently covers arm64; Intel execution has not been verified on an Intel Mac.
- Git and Python 3. Some Waza checks use Python. The Release install and the installed scheduler do not require Go; only a source install requires the Go version declared in `go.mod`. Missing prerequisites produce actionable guidance, not a silent system install.
- An independently callable Claude Code CLI. The engine checks its path, version, print/JSON/session/effort capabilities, and authentication in the CLI worker environment. Use `--claude /absolute/path/to/claude` to select an executable, including paths with spaces. Command names already in configuration (such as `claude`) resolve through PATH. If `--claude` differs from the preserved `claude_path`, installation stops; the operator must explicitly update their own configuration first. The installer does not test one engine and leave workers using another.
- CLI-only works. Desktop-only, an uploaded skill ZIP, or an executable hidden inside an app's private bundle does not qualify. If the CLI is absent, the installer shows the official stable command `curl -fsSL https://claude.ai/install.sh | bash -s stable` and `claude auth login`; the operator decides whether to run them.
- With `--services`, a second probe uses the HOME/PATH declared in the plist without inheriting shell tokens or proxy variables. A shell-only identity cannot authorize a service installation. The operator must explicitly configure durable CLI authentication/network settings; secrets are not written into plist files, and the installer does not log in.
- Authentication detection is not proof of a successful model request. Existing, supported gateway/model configuration belongs to the operator; agentq does not change endpoints or billing channels. Parent API variables are not silently inherited by workers; see the environment allowlist in README.
- Claude settings are checked for JSON and common field structure, then left byte-for-byte unchanged. Use the official `claude doctor` for comprehensive host diagnostics. The current projection uses `$HOME/.claude`; a custom `CLAUDE_CONFIG_DIR` is not automatically migrated.

If a prerequisite is missing, install it from the documented official source and repeat the same entry point. You do not assemble individual skills, scripts or services manually.

## Installation scopes

| Scope | Add to the entry point | Includes | Does not imply |
|---|---|---|---|
| Complete system / base capability | Run the Release `install.sh`; the source entry is `sh scripts/bootstrap.sh --yes` | agentq, eight owned skills, four required Waza skills, support, templates, licenses and lock | Active scheduling, Telegram, copied permissions or identity |
| Extended capability | `--extras` | Base plus four optional Waza skills: read, write, health, learn | Other local skills, plugin caches, or private configuration |
| Skills only | `--scope skills` | Complete skill closure, support, templates and licenses | Background executable or services; CLI login is not required |
| Plugins and MCP | Not installed by the initial release | 0 plugins and 0 MCP servers | No ownership of host plugin caches, and no local modules counted as release content |
| Scheduler services | `--services` | tick every 5 minutes, night at 01:00, review at 06:30 | Resuming scheduling or installing Telegram listen |
| Private code backup | `--backup` | A per-user service every 86400 seconds | Remote writes without explicit targets, or a global Git hook replacement |
| Local watchdog | `--watchdog` | An independent shell check every 1800 seconds, local report | Automatic repairs, notifications, permission changes or resumed scheduling |

Owned skill sources: agent-task, dispatch, nightly-review, new-project, project-status, no-ai-look (with references), prior-art and feedback-triage. Templates: AGENTS, BACKLOG, PROGRESS, DECISIONS and LESSONS. Support: with-lock, ask-rule-scan and watchdog.

Required Waza skills: check, think, hunt and ui. Optional Waza skills: read, write, health and learn, installed only with `--extras`. `dependencies/claude.lock.json` records the source URL, full commit, MIT license and per-file Git blob hashes. Only declared files are extracted and verified. Upstream installers are not executed, and vendor/cache/identity trees are not copied. The initial release includes 0 plugins and 0 MCP servers; other local skills and caches are not release content. See [`../COMPONENTS.json`](../COMPONENTS.json) and [`components.md`](components.md) for the exact inventory. The initial release contains no brand images or other media assets.

## Plans, ownership and customization

```sh
sh scripts/bootstrap.sh --plan
```

A plan prepares and self-checks a candidate before printing changes, including each path, before/after entry types and the transaction backup location. Selecting backup also checks the gh executable path and version; remote identity and target authorization are verified when a backup actually runs. **Preparation fetches locked public dependencies and runs agentq's own temporary-HOME checks**; it does not activate entries, run upstream installers or change current settings/services. `--yes` confirms the selected installation changes, not account operations, releases, payments or external messages.

Vendor files live under `~/.local/share/agentq/generations/gen-*/`. The executable and skills are managed links. A receipt records ownership and generation digests; activation keeps restricted-permission transaction backups.

User rules, project instructions, independent skills, third-party hooks/status lines and machine configuration stay outside vendor generations. Do not edit a vendor generation to customize behavior. Installation/update checks before replacing:

- An identical external skill directory stays externally owned and survives uninstall. Its dependency digest is recorded so doctor can detect later removal or edits without adopting or deleting the external directory. A different same-name skill blocks activation; it is not deleted, adopted or overwritten.
- Local edits to managed entries/generations, unknown collisions, loaded services outside the receipt, invalid JSON or common settings-shape errors stop installation. The operator's content remains intact; there is no force-overwrite-all option.
- Existing configuration, unknown fields and scheduler switches remain byte-for-byte unchanged. A fresh installation explicitly writes `paused=true`, disables night/review/patrol/notify, and selects `permission_mode=default`. It does not inherit the author's bypass permissions. Legacy configurations retain their existing missing-field bypass semantics rather than having their policy silently changed.
- Installation shares a lock with tick and refuses replacement while jobs are running or state is unknown. Candidate `status`/`tick` self-checks use a fresh temporary HOME. Executables are not overwritten in place or deleted while in use.
- A failed activation restores prior entries and services. An interrupted activation keeps a pending journal and requires restore. Recovery refuses to overwrite user changes made after interruption. Active, previous and removed receipts share complete validation; missing fields cannot be interpreted as instructions to delete entries. Doctor reports an absent managed receipt as not installed. Runtime jobs, secrets, identity and existing user configuration are not rolled back with code.

For a conflict, compare and back up the reported paths, then decide explicitly how to move customization into the user/project layer. Atomic file replacement alone is not customization preservation.

## Use it without Telegram

Run `~/.local/bin/agentq status`. For natural-language requests, start **`claude`** in a project and say “dispatch this task”. Standard skill descriptions let the host discover dispatch, which writes BACKLOG and calls the scheduler. `agentq run/status/logs` are management commands, not a natural-language chat interface.

An explicit `agentq run <project> <keyword>` can start a task even while automatic scheduling is paused; authentication, quota and concurrency limits still apply. Default permissions may reject unattended writes until the operator authorizes an appropriate policy. Installation does not bypass that decision.

Results remain local in `~/.agentq/jobs/<ID>/`, `agentq logs <ID>` and `~/.claude/nightly/<date>-agentq.md`. Telegram credentials and listen are optional, operator-enabled messaging features, not engine credentials. The installer does not request a bot token, install listen by default or send external messages. Selected launchd services can run without an open Desktop/terminal window, but a fresh installation remains paused until explicitly enabled.

## Update, rollback, remove and restore

- **Update:** download the target release's `install.sh` from GitHub Releases to a temporary file and run it again; a source checkout can run `scripts/update.sh`. Both paths build a candidate, preserve optional selections and services, and repeat conflict checks without rewriting user configuration. For an unmanaged legacy installation, update.sh retains its binary-only compatibility flow; that is not a complete installation.
- **Rollback:** `agentq rollback --yes` restores the previous executable, skills, support, templates and service entries, not runtime state or configuration. The rollback target is consumed to avoid oscillating between broken versions.
- **Remove:** `agentq remove --yes` (alias `agentq uninstall --yes`) unloads owned services and removes only still-matching owned entries. Configuration, jobs, secrets, generations and backups remain. Local edits block removal rather than being discarded.
- **Restore a removal:** `sh scripts/bootstrap.sh restore --yes`, or the retained generation executable printed by remove. Skills-only installations can also restore via bootstrap.
- **Restore an interruption:** the same restore entry first reverses a pending transaction. Conflicts created after interruption are preserved and reported.
- **Runtime fallback:** after two failing scheduled ticks, the guard tries a whole-generation rollback through the previous executable. Active jobs, unknown state or local modifications block automatic replacement. Without a previous generation there is nothing to roll back to. The independent watchdog writes a local report even if agentq cannot execute.
- Old generations and user data are never automatically purged. Removing the installation is not deleting private data.

The optional backup service requires explicitly authorized local `~/.agentq/backup.json` targets, for example `{"targets":[{"project":"/absolute/project","remote":"backup","url":"https://example.invalid/owner/repo.git"}]}`. Substitute an actual authorized URL; the example is not a working target. Each run checks that the remote push URL is unchanged and gh verifies PRIVATE visibility before pushing branches/tags. Missing targets do nothing; unknown/public/changed targets fail closed. Reports are local `~/.agentq/*-report.txt` files.

## Verification boundary and upstream ownership

The verified v0.1.0 scope is macOS 13+ with Claude Code CLI. arm64 has been tested on real hardware. amd64 has been build-verified but has not been run on an Intel Mac. Linux, WSL, Windows, and Codex are outside the initial release scope. Codex-related work remains on a private development branch and is not required by the public release.

Normal tests use a temporary HOME and fake CLI/launchctl: `go test ./...`.

Opt-in formal-entry/network acceptance: `AGENTQ_BOOTSTRAP_ACCEPTANCE=1 go test -count=1 -v -run '^TestBootstrapAcceptance$' ./...`.

Opt-in real-model acceptance: `AGENTQ_REAL_E2E=1 go test -count=1 -v -run '^TestRealClaudeInstallationE2E$' ./...`. A connection-based identity is projected from the caller's existing connection environment only with the additional explicit `AGENTQ_REAL_E2E_CONNECTION=1`; no real HOME is read, and the temporary settings projection is removed afterwards. This consumes authorized model usage and is not a default CI test.

Official references: [Claude Code setup and stable channel](https://code.claude.com/docs/en/setup), [CLI and auth reference](https://code.claude.com/docs/en/cli-reference), [gh repo view](https://cli.github.com/manual/gh_repo_view). Native Claude updates preserve custom launchers, while ordinary launchers remain managed by the official updater. npm supports automatic updates; Homebrew defaults to manual package-manager updates. These policies do not promise preservation of every user customization. agentq neither overwrites them nor takes over custom CLI launchers.
