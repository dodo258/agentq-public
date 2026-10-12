# agentq

While I was developing agentq, my Claude Code account was suddenly banned. The ban made no sense to me and disrupted the final stretch of the project.

There is still plenty to improve. I am open-sourcing agentq in the hope that interested contributors will help make it better. Issues, pull requests, and reports about your experience are all welcome.

[简体中文](README.md)

agentq is a local background scheduler for Claude Code. It takes pending work from each project's `docs/BACKLOG.md`, starts independent `claude -p` processes, and follows the project's rules through implementation, verification, and handoff.

## Initial release support

| Environment | v0.1.0 status |
| --- | --- |
| macOS 13+ on Apple Silicon arm64 | Verified on real hardware |
| macOS 13+ on Intel amd64 | Build verified, not run on an Intel Mac |
| Claude Code CLI | Verified; it must already be installed and authenticated |
| Codex | Not supported in the initial release; related work remains on a private development branch |
| Linux, WSL, Windows | Not supported in the initial release |

The initial release contains no brand images and does not depend on a logo or other media assets.

## Install

The recommended path is to download the v0.1.0 Release installer to a temporary file, then run it:

```sh
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
curl -fL https://github.com/dodo258/agentq-public/releases/download/v0.1.0/install.sh -o "$tmp"
sh "$tmp"
```

The installer checks the macOS architecture and the presence of `curl`, `python3`, `git`, and `shasum`, downloads the matching archive and `SHA256SUMS`, verifies the archive, then runs the packaged `agentq setup --yes "$@"` from a temporary directory. It does not require Go or Node, and it never installs missing dependencies silently.

Prerequisites:

- macOS 13+;
- Git and Python 3;
- an installed and authenticated Claude Code CLI.

If a local tool is missing, the Release script names the missing command and exits. If Claude Code CLI is missing, setup prints the official stable installer and `claude auth login` instructions. It does not sign in for you, request a token, or change your authentication method.

A source install requires the Go version declared in `go.mod`:

```sh
git clone https://github.com/dodo258/agentq-public.git
cd agentq-public
sh scripts/bootstrap.sh --yes
```

A fresh installation leaves scheduling paused, disables night, review, patrol, and notifications, and uses `permission_mode=default`. The installer does not rewrite existing permissions, authentication, or user settings. Services, code backup, and the watchdog are explicit options:

```sh
~/.local/bin/agentq setup --yes --services
~/.local/bin/agentq setup --yes --backup
~/.local/bin/agentq setup --yes --watchdog
```

For installation, updates, rollback, removal, and recovery, see the [English install guide](docs/install.en.md) or [Chinese install guide](docs/install.zh-CN.md).

If your shell cannot find `agentq`, use `~/.local/bin/agentq` or add `$HOME/.local/bin` to PATH. Installation does not silently edit your shell configuration.

## Three common examples

Open Claude Code in a project and describe the task naturally:

```text
Dispatch this task: add retry handling and tests to the export flow.
```

Check scheduling, quota, running jobs, and pending work:

```sh
agentq status
```

Start one pending item explicitly, even while automatic scheduling remains paused:

```sh
agentq run my-project "export flow"
```

## End-to-end flow

1. You describe a request in a Claude Code session. `dispatch` writes it to the project's `docs/BACKLOG.md`.
2. An agentq `tick` selects an eligible pending item and starts an independent `claude -p` process.
3. The worker follows `agent-task` and the project's rules for branching, implementation, tests, checks, and merge.
4. agentq handles concurrency, timeouts, quota gates, failure circuit breaking, continuation, logs, and local summaries.
5. You inspect the result with `agentq status`, `agentq logs <job-id>`, the project BACKLOG, and local reports.

agentq does not edit BACKLOG files, product code, commits, or merges itself. It selects, starts, supervises, and records work performed by Claude Code sessions and skills.

## Release components

| Category | Included in v0.1.0 |
| --- | --- |
| agentq-owned skills | 8: agent-task, dispatch, nightly-review, new-project, project-status, no-ai-look, prior-art, feedback-triage |
| Required Waza skills | 4: check, think, hunt, ui |
| Optional Waza skills | 4: read, write, health, learn, installed with `--extras` |
| Project templates | 5: AGENTS, BACKLOG, PROGRESS, DECISIONS, LESSONS |
| Support scripts | 3: `ask-rule-scan.sh`, `watchdog.sh`, `with-lock.sh` |
| Source and release scripts | 8: `backup-config.sh`, `bootstrap.sh`, `config_privacy.py`, `install.sh`, `package-release.sh`, `pre-commit`, `privacy_scan.py`, `update.sh` |
| Plugins / MCP servers | 0 / 0 |
| Resident process | `agentq listen` is optional and is not installed or enabled by default |

See [`COMPONENTS.json`](COMPONENTS.json) for the exact file inventory and [`docs/components.md`](docs/components.md) for a readable guide. Other skills, plugin caches, and private configuration found on the author's machine are not part of this release.

## Optional Telegram integration

Telegram is only a messaging channel. It does not perform model inference and cannot replace Claude CLI authentication. Scheduling, status, logs, and local reports work without Telegram. Installation does not request a bot token or enable the resident listener by default.

## Cost, permissions, and security

- agentq code is MIT licensed and does not provide model access. Workers use your configured Claude identity and may consume subscription quota or incur API charges; unknown usage is not a balance or an actual bill.
- Fresh installations use `permission_mode=default`. Whether unattended work may write files is controlled by your Claude Code and project permissions.
- The installer creates no account, performs no login, changes no authentication, copies none of the author's settings, and writes no secrets to launchd plist files.
- Release archives are checked against `SHA256SUMS`. Third-party Waza files are pinned to a full commit and per-file hashes, with the upstream license retained.
- Run `agentq doctor` before an update if needed. Managed installations support whole-generation rollback, transaction recovery, and removal limited to managed entries.

If GitHub private vulnerability reporting is enabled for the repository, use it for security issues. Otherwise, open a sanitized issue asking for a private contact path. Never paste tokens, configuration, private paths, chat content, or unredacted job logs into a public issue.

## Detailed guides

- [Complete English reference](docs/reference.en.md)
- [完整中文参考](docs/reference.zh-CN.md)
- [Configuration and privacy](docs/config-privacy.en.md)
- [Automation entry point](llms.txt)

For installation entry points and initial release support, this README and the install guides are authoritative.

## Contributing

Issues, pull requests, and reports from real use are welcome. Include your macOS version, architecture, reproduction steps, expected result, and actual result. Redact usernames, paths, tokens, chat content, and private project information from logs.

## License

agentq is released under the [MIT License](LICENSE). Third-party sources, revisions, install policies, and license records are listed in [`dependencies/claude.lock.json`](dependencies/claude.lock.json) and the release inventory.
