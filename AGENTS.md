# agentq contributor guide

agentq schedules BACKLOG tasks through an independently installed Claude CLI. This release supports macOS 13+; Codex and Linux/WSL are not supported by this release. Telegram is optional.

## Source and validation
- Go standard library only. VERSION is the release version source; do not hardcode another version in code.
- Source assets: skills/, templates/, support/, prompts/, dependencies/claude.lock.json. Never modify installed generations as source.
- Validate: `test -z "$(gofmt -l .)" && go vet ./... && go test ./...`.
- Formal install acceptance: `AGENTQ_BOOTSTRAP_ACCEPTANCE=1 go test -count=1 -v -run '^TestBootstrapAcceptance$' ./...`.
- Real-model tests require explicit opt-in and authorized credentials, consume model usage, and use temporary HOME/Git fixtures. Normal tests use fake CLI/services and never touch real state.

## Maintenance
- Implement and self-test first. One independent medium reviewer checks real functionality/security/data/privacy problems; reviewers do not spawn reviewers. A second pass only verifies substantive fixes.
- Preserve all concurrent/uncommitted work. Work in a task branch/worktree; do not reset, clean or stash another person's changes.
- Use setup/rollback/remove/restore through the same installation engine. Preserve authentication, permissions, settings, custom skills, jobs and secrets. Never overwrite an active executable or bypass conflicts.
- Do not silently switch model billing providers or increase permissions. Publication, remote messages, payments and destructive data actions require authorization covering the target.

## Release
- `sh scripts/package-release.sh` builds macOS archives and checksums outside the source directory.
- Verify the exact source, archive contents, fresh-HOME installation, privacy scan, version, CI and downloaded Release assets before calling a release complete.
- Keep README.md/README.en.md and docs/install.zh-CN.md/docs/install.en.md equivalent. Support claims must distinguish actual execution from cross-compilation.
- Keep credentials, private project records, machine paths, sessions, logs and caches out of source and packages. See docs/privacy.md and docs/privacy.en.md.
