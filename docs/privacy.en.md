# Privacy and permissions

[简体中文](privacy.md)

This new public repository contains generic code, tests, skills, templates and documentation. The original private history, worktrees, collaboration records, identities, machine configuration, sessions, logs, databases and media are not included. Installation does not copy the author's configuration.

Local `~/.agentq/` records can contain private task content and should be protected as secrets. Configuration and credentials are not packaged. Telegram is optional; enabling it sends relevant messages to the configured service. Claude CLI model requests follow its service terms. agentq does not provide model access, include a subscription or silently change billing providers.

Fresh installations start paused, with notifications off and default permissions. Services and extra capabilities are explicit selections. Upgrades check ownership and conflicts, preserving configuration, authentication, permissions and jobs. Atomic replacement does not make overwriting customization safe.

`python3 -I scripts/privacy_scan.py --patterns /path/to/private-patterns.json --root /path/to/candidate --manifest config/clean-source.json` checks every candidate file, including ignored/untracked files. Keep rules outside the repository in a mode-600 file under a mode-700 directory; do not commit or package them. Format: `{"version":1,"patterns":[{"type":"personal-identifier","literal":"your-private-identifier"}]}`. Replace the example with actual private identifiers.

Exit 0 means no findings under the supplied rules, 1 means findings, and 2 means reliable scanning was not possible. Scanning does not prove all encoded or semantic secrets were identified. Media, archives and binaries require separate content/licensing checks and pinned digest approval. Review public Git author metadata separately.

Default tests use temporary HOME, fake CLI and fake services. Real-model acceptance is opt-in, uses isolated projects and authorized identity; ordinary tests are not real-model evidence. The account holder completes login; copying old authentication tokens does not establish isolation.

Optional legacy configuration migration and safe backup are documented in [config-privacy.en.md](config-privacy.en.md). Do not mirror-delete servers containing state; this project provides no such deployment flow.
