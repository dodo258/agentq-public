# Privacy boundaries for configuration backups

[简体中文](config-privacy.md)

`settings.json` is runtime configuration, not a distributable template. Authentication, permissions and hooks continue using the real local file. Git stores only the generated safe projection, `config/settings.example.json`.

## Why secrets are not moved into global settings.local.json

Claude Code documents `~/.claude/settings.json` as user settings for every project, and `.claude/settings.local.json` as a local override for one project. Precedence is managed, command line, project local, project shared, then user settings; lists generally merge across levels. A file at `~/.claude/settings.local.json` is not a documented global override and can change authentication or permission behavior.

Source: [settings files and precedence](https://code.claude.com/docs/en/settings), including the scope table and “Lists merge instead of overriding”. Migration therefore does not split configuration, change permission defaults or relocate authentication. It leaves settings bytes intact, sets mode 0600 and removes the file only from Git's index.

## Interface

The Python-standard-library tool provides:

```sh
python3 scripts/config_privacy.py project  [--config-dir DIR]
python3 scripts/config_privacy.py migrate  [--config-dir DIR]
python3 scripts/config_privacy.py rollback [--config-dir DIR]
python3 scripts/config_privacy.py prepare  [--config-dir DIR]
```

Without `--config-dir`, it uses `$CLAUDE_CONFIG_DIR`, then `$HOME/.claude`. Tests and trial runs must use a temporary HOME or explicit temporary directory, not real configuration.

### project

Reads settings and atomically writes the projection without touching Git:

- Omits the entire env object.
- Recursively removes headers, auth, token, secret, password, credential, cookie, apiKey, accessKeyId, privateKey and similar fields.
- Rewrites the absolute HOME prefix as `~`, redacts URL user information and common authentication arguments in commands.
- Fails closed if a removed long sensitive value still appears elsewhere in the projection.
- Does not print values or object summaries.

The repository projection is a secret-free format example. Installed projections are generated, not manually maintained.

### migrate

Run once against the configuration repository:

1. Validate settings as a JSON object, preserve its bytes and set mode 0600.
2. Remove settings.json, settings.local.json, `.claude.json` and plugins/ from a temporary Git index without deleting working files.
3. Add protection blocks to the end of `.gitignore` and local `.git/info/exclude`, and verify effective ignoring with Git; the former overrides older negation rules.
4. Stage the safe projection without committing, pushing or rewriting history.
5. Keep private rollback state in `.git/config-privacy-state/`.

Temporary-index and atomic replacement failures restore the projection, ignore files, index and permissions. Repeated migration is idempotent. The external `~/.claude.json` is not read, copied or staged; any same-named file inside the configuration repository is excluded. plugins/ is excluded as a whole to keep caches and authentication out of backups.

### rollback

For migration before commit only. Restores the previous index, projection, ignore files and mode, never the settings content. Changes to HEAD, index, projection or exclude after migration block automatic rollback to preserve later work.

Rollback can put previously tracked real configuration back into the index. Do not run a backup commit until it has been migrated again. After migration is committed, the next prepare/migrate verifies HEAD has no private paths and removes stale rollback state.

### prepare

For backup jobs. Regenerates the projection and stages only an explicit allowlist using a temporary index:

- config/settings.example.json and `.gitignore`.
- CLAUDE.md, CLAUDE_BASE.md, CLAUDE_TEMPLATE.md.
- ENGINEERING_DECISIONS.md, ONE_LINE_BOOTSTRAP.md, PROJECT_BOOTSTRAP.md, PROJECT_LOG_TEMPLATE.md, SYSTEM_CHANGELOG.md and facts.md.
- git-hooks/, hooks/, scripts/, skills/, templates/ and statusline.sh.

It does not run whole-repository git add -A. projects/, nightly/, runtime state, local settings, `.claude.json`, plugins/ and common credential filenames are not automatically staged. Existing staged paths outside the allowlist fail, except private-path deletions introduced by migration. The proposed commit tree is rejected if it still contains private configuration paths, credential filenames or known sensitive values from current settings.

Secret-only changes leave projection bytes unchanged and real settings are untracked, so they do not create a backup commit. The tool itself never commits or pushes.

## Installation and real migration

Test with temporary HOME first. These are operator-controlled steps, not actions a development branch should execute automatically:

```sh
install -m 0755 scripts/config_privacy.py "$HOME/.claude/scripts/config_privacy.py"
install -d -m 0755 "$HOME/.claude/config"
install -m 0644 config/settings.example.json "$HOME/.claude/config/settings.example.json"
python3 "$HOME/.claude/scripts/config_privacy.py" migrate --config-dir "$HOME/.claude"
```

Then inspect:

```sh
test "$(stat -f '%Lp' "$HOME/.claude/settings.json")" = 600
git -C "$HOME/.claude" diff --cached --name-status
python3 "$HOME/.claude/scripts/config_privacy.py" prepare --config-dir "$HOME/.claude"
```

Authentication validation uses read-only CLI status, not a model request. Do not print configuration, tokens, sensitive projection diff lines or secret hashes.

## Updating a backup-push.sh job

Replace a configuration-repository branch that stages everything:

```sh
(cd "$c" && git add -A && { git diff --cached --quiet || git ... commit ...; })
```

with fail-closed preparation and commit handling:

```sh
if ! python3 "$c/scripts/config_privacy.py" prepare --config-dir "$c" >/dev/null 2>&1; then
  bad=$((bad+1))
  lines="$lines\n- Configuration: preparation failed; not committed or pushed"
elif git -C "$c" diff --cached --quiet; then
  push_repo "$c"
elif "$c/scripts/backup-config.sh" "$c"; then
  push_repo "$c"
else
  bad=$((bad+1))
  lines="$lines\n- Configuration: commit failed; not pushed"
fi
```

backup-config.sh uses the existing local Git identity and normal hooks, not hardcoded authors or bypass flags. Preserve existing private-remote checks, timing, locks and push policy. Remove whole-repository staging on the configuration path, and do not push after preparation or commit fails.

## Limitations

Known secrets in current configuration are excluded from the next backup tree; old private history is not cleaned or rewritten. Arbitrary strings can contain unknown secrets: field filtering, sensitive-value checks and staging allowlists complement, not replace, whole-tree credential scanning. The projection cannot replace live settings; omitted authentication and environment settings are intentional.
