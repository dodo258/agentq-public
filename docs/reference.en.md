# agentq command and behavior reference

[简体中文](reference.zh-CN.md) · [Current install guide](install.en.md)

> For installation entry points and the v0.1.0 support boundary, follow the [English install guide](install.en.md) or [Chinese install guide](install.zh-CN.md). They override older installation examples in this long-form reference.

agentq is a background scheduler originally built for one person's own workflow. On macOS, launchd runs `agentq tick` every five minutes. A tick also runs as soon as each job ends; see [Start the next item as soon as a job ends](#start-the-next-item-as-soon-as-a-job-ends). It scans the Pending section of `~/projects/*/docs/BACKLOG.md`, then gives eligible items to independent, headless Claude Code processes (`claude -p`). Each worker follows the global `agent-task` skill through merge. Claude Desktop and an open terminal window are not required.

agentq does not edit BACKLOG files, commit, or merge. It selects work, starts workers, supervises and finalizes them, and records results. The maintenance and verification contract is in [`AGENTS.md`](../AGENTS.md).

## Using the CLI and optional Telegram integration

**Telegram is an optional messaging channel, not a required dependency.** Without Telegram, agentq can schedule local jobs, show status and logs, and generate local summaries and morning reports. Background work still needs an authenticated Claude CLI plus the project tools and feature switches required by that work.

| Purpose | Local CLI | With Telegram configured |
| --- | --- | --- |
| Describe work in natural language, ask a question, or answer a decision item | Use an interactive Claude Code session with the relevant workflow skills installed | Send a message to the bot; agentq passes it to AI and returns the answer |
| Manage jobs directly | Use `agentq run`, `agentq status`, `agentq logs`, and related commands | You may also ask about status or dispatch work by message |
| Receive messages | The current macOS version can use local notifications when notifications are enabled | Receive morning reports, alerts, and decision requests; job outputs can also be sent |
| Inspect artifacts | Read local reports, logs, and files | Receive screenshots, recordings, or files, subject to Telegram and implementation size limits |

The Telegram Bot API only sends and receives messages. It does not perform AI inference. `TELEGRAM_BOT_TOKEN` and `TELEGRAM_CHAT_ID` configure the bot channel; they are not model API keys and cannot replace Claude CLI authentication or model-service credentials. Telegram sending and receiving require `notify_enabled=true`. Turning that switch off stops both outbound notifications and inbound Telegram messages.

Natural-language CLI conversation happens in an interactive Claude Code session. agentq itself exposes job-management commands and has no general `agentq chat` command. The public v0.1.0 worker backend is Claude CLI only. Codex is not supported in this release; related work remains on a private development branch.

Both entry points can use the same project files and BACKLOG, but they do not automatically synchronize a complete chat history. See [Notifications and Telegram](#notifications-and-telegram) for channel configuration, inbound-message scope, and attachment limits.

## Installation

The recommended v0.1.0 path downloads the Release installer to a temporary file, then runs it. The script checks the macOS architecture and the presence of `curl`, `python3`, `git`, and `shasum` before downloading and verifying the matching archive:

```sh
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
curl -fL https://github.com/dodo258/agentq-public/releases/download/v0.1.0/install.sh -o "$tmp"
sh "$tmp"
```

[Chinese install guide](install.zh-CN.md) · [English install guide](install.en.md) · [One-line instructions for agents](../llms.txt)

A complete base installation contains the executable, all files for eight agentq-owned skills, four pinned required Waza skills, support scripts, five templates, and license material. It does not copy the author's configuration and does not require Node. The setup engine checks macOS and architecture, Git, Python, the independent Claude CLI path and version, headless capabilities, and authentication. A Release install does not need Go; a source build needs the version declared in `go.mod`. CLI-only operation is supported. Desktop or an uploaded skill ZIP does not count as a background engine.

A fresh installation remains paused, disables night, review, patrol, and notifications, and uses the default permission mode. Existing configuration, third-party hooks, and status-line settings are preserved byte-for-byte. Telegram is optional and no bot token is requested. `--scope skills` installs only skills; `--extras` adds the four optional Waza skills; `--services`, `--backup`, and `--watchdog` are explicit selections. None of these choices resumes global scheduling. Unknown same-name skills, local changes, or invalid configuration stop installation and are reported rather than overwritten or deduplicated.

Instruction for an Agent: **Read this repository's `llms.txt` and install guide, download the v0.1.0 Release installer to a temporary file, and run it. Preserve my configuration, leave scheduling paused, and follow my authorization for service, permission, and authentication changes.**

## Safe updates and recovery

- Download the target release's `install.sh` to a temporary file and run it again, or run `scripts/update.sh` from a managed source checkout. The installer builds and self-checks a candidate, compares conflicts, creates backups, and switches a complete generation while preserving selected optional modules and all user configuration.
- `agentq doctor` performs read-only configuration, installation-integrity, and CLI checks.
- `agentq rollback --yes` restores the previous complete generation without rolling back data or configuration.
- `agentq remove --yes` removes managed entry points and services while preserving data, configuration, and recovery material. Use `sh scripts/bootstrap.sh restore --yes` from a source checkout, or the retained generation executable printed by removal, to recover an interrupted transaction or undo a removal.
- Running jobs, unknown state, and user changes block replacement. The installer never builds directly over an executable that is in use. See the install guide for details and limits.

Legacy binary-and-launchd-only operations remain documented in [legacy-install](legacy-install.md); they are not a complete work-system installation. Current public downloads are on [GitHub Releases](https://github.com/dodo258/agentq-public/releases). Follow the complete install guide for installation.

## Commands

| Command | Purpose |
| --- | --- |
| `agentq version` | Print the release version |
| `agentq tick` | Run one scheduling cycle: finalize ended jobs, kill timed-out jobs, then select and start work. Normally called by launchd and the job wrapper, but it can also be run manually |
| `agentq run <project> [keyword]` | Start a job for a project immediately. With a keyword, select the first Pending item containing it. Ignores pause, `retry_after`, and the rapid-failure circuit breaker, but still obeys concurrency, usage cooldown, and login checks. Refuses at subscription usage of 98% or more; see [Subscription quota](#subscription-quota) |
| `agentq status` | Show pause state, usage cooldown, near-exhausted quota or rapid-failure breaker, subscription usage and reset time for each window, last patrol and current issue count, running jobs, Pending counts by project, and the 10 most recent ended jobs. Continued jobs are marked as such; stale session records are marked possibly stalled; interrupted jobs show interruption and continuation state |
| `agentq patrol` | Run a patrol immediately and list every current issue with evidence and handling notes. Ignores `patrol_enabled` and the previous patrol time; see [Patrol](#patrol) |
| `agentq quality` | Show first-pass satisfaction and rework rates for the last seven days, with a per-project account of how each merged item was classified and the evidence used. Reads only BACKLOG and git; see [First-pass satisfaction and rework rates](#first-pass-satisfaction-and-rework-rates) |
| `agentq logs <job-id>` | Show job state, full result, and the tail of stderr. A unique prefix of at least four characters is enough |
| `agentq stop <job-id>` | Stop the job's process group with SIGTERM, followed by SIGKILL after 10 seconds if needed, and mark it stopped. If the process already ended on its own, finalize it from its result |
| `agentq pause` / `agentq resume` | Pause or resume automatic work intake. While paused, ticks only finalize work and do not start new jobs. `run` is unaffected |
| `agentq night` / `agentq review` | Run a night shift or review immediately; see [Night shift and review](#night-shift-and-review). Manual runs ignore `night_enabled` and `review_enabled`. Scheduled launchd calls add `--scheduled`; when the switch is false, they only write one log line |
| `agentq notify setup` / `test` / `send` / `file` / `status` | Manage notifications; see [Notifications and Telegram](#notifications-and-telegram). Configure a Telegram bot; send a test; use `send [--security] [--key=K] [--file=path]... <category> <one-line summary> [details...]` to queue and deliver a `decide`, `alert`, `patrol`, `release`, or `done` message; use `file <path> [caption]` to send a file immediately; or inspect the active channel, today's count, and queued messages |
| `agentq setup --services --yes` / `agentq remove --yes` | Select services in a complete managed installation or remove the installation. See legacy-install for old binary-only install/uninstall behavior |
| `agentq listen` | Receive Telegram messages continuously and reply as they arrive. The `io.agentq.agentq.listen` KeepAlive launchd service calls it; it is not normally run by hand |

## Work-selection rules

- Projects are direct children of `projects_root` whose `.git` is a directory, which means git main working directories. They are processed in alphabetical order. Names beginning with `.` are skipped.
- An item is a line beginning with `- [ ]` between `## Pending` and the next `## ` heading. HTML comments, lines containing the in-progress marker `（进行中`, and blocked lines are skipped.
- A blocked marker is recognized only in either of these forms. When `agent-task` leaves work behind, it writes `（卡住：<branch> <reason>）`. Merely mentioning the Chinese word `卡住` in the body, such as an acceptance criterion saying status should show a stalled job, does not block the item.
  - Bracket marker: an opening bracket immediately followed by `卡住`, then a colon, closing bracket, or whitespace. Examples: `（卡住：…）`, `(卡住)`, `【卡住】`, `[卡住 …]`. It is recognized anywhere in the line.
  - End marker: after removing the in-progress marker, trailing whitespace, and final punctuation, the line ends in `卡住` and is preceded by an em-dash-like separator from the source syntax (`U+2014` once or twice) or by `；，。|`. Examples include an ellipsis followed by two `U+2014` characters and `卡住`, or `…；卡住。`.
  - Non-matches include `进程会卡住`, `标「卡住」`, `（卡住时 status 有提示）`, `[卡住检测](…)`, and `修 claude -p 卡住`.
- An item is skipped if the same item already has a running job or its last attempt ended less than `retry_after` ago, 12 hours by default. A usage-limit attempt does not count as an attempt.
- At most `max_parallel` jobs run globally and `per_project` jobs run in one project. A tick starts no more than one job per project.
- New work pauses after a usage-limit hit, when subscription quota is nearly exhausted, or after a rapid-failure streak. The following sections describe each gate.

**Deferred-decision marker:** A Decision needed item may depend on earlier work, such as all seven preceding items being merged. Put `（等条件：<condition>）` on the item's first `- [ ]` line. Patrol does not report or push decision alerts for it, and it does not count in the morning report's decision summary; the detail section still lists one line. Only a bracket immediately followed by `等条件` and a colon is recognized, with full-width or half-width brackets and colons accepted. Mentioning the phrase elsewhere does not count. The agent that completes the final prerequisite removes the marker according to the `agent-task` skill. Once removed, a tick treats it as a new decision item and sends the alert.

## Usage-limit cooldown

- Limit wording is searched only in results that could represent a real limit hit: `is_error` is true, `api_error_status` is present, or the run used no more than three turns and is not a background-notification turn (`result_index` is 0). If `result.json` cannot be read, stderr is scanned. A normal long-running job that merely discusses a rate limit in the code it is fixing does not count.
- Recognized wording includes `usage limit`, `rate limit`, `limit reached`, and `hit your ... limit`, including official messages such as `You've hit your session limit · resets 3:45pm`.
- If a reset time can be parsed from the message, such as `resets 3:45pm`, `resets Mon 12:00am`, `resets Oct 12, 9am`, `resets 8pm (Africa/Cairo)`, or the old `usage limit reached|<Unix seconds>` form, cooldown lasts until one minute after that reset. If it cannot be parsed, lies more than eight days away, or is already past relative to the time Claude exited and printed the message, cooldown uses `limit_cooldown`, one hour by default. A late finalization never pushes an already-past reset into the next day or week.
- During cooldown, ticks start nothing and `run` is refused.
- A limit-hit attempt does not count as tried and can be selected again after cooldown. If the BACKLOG item is still marked in progress because the worker had started it, the job is marked interrupted and continues in the same session after cooldown; see [Interruption and automatic continuation](#interruption-and-automatic-continuation).

## Subscription quota

The previous section reacts only after a limit has been hit. This gate checks in advance so a job is less likely to stop halfway through. The implementation follows the approach used by `internal/provider/claude_usage_cli.go` in yetone/magpie.

- Before taking work, meaning when a tick finds a selectable item or continuation, `agentq run` starts, or `agentq night` or `review` is about to start, agentq reads quota once with `claude -p /usage --output-format json --tools "" --strict-mcp-config --setting-sources "" --no-session-persistence`. It runs in an empty temporary directory with the same environment allowlist as a job. `/usage` is a local Claude Code command, does not ask the model or consume tokens, and has been observed to return `num_turns` 0 and `total_cost_usd` 0. `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC` is not set because that would prevent `/usage` from refreshing and expose only a reading cached elsewhere for up to an hour; the allowlist does not pass it anyway.
- Lines such as `Current session: 2% used · resets Oct 11 at 1:59pm (Asia/Shanghai)`, `Current week (all models): 40% used · resets ...`, and `Current week (Fable): 0% used · resets ...` are parsed into percent used and reset time for each window. Reset-time formats are the same as in Usage-limit cooldown, and times are displayed in the local time zone.
- Readings are stored in `state.json` and cached for 10 minutes. If a window has passed its reset time, agentq reads again even within the cache period.
- Counted windows are the current session, the current week across all models, and a model-specific week only when the task model matches the model name in parentheses. For example, `Current week (Opus)` counts for a configured model containing `opus`. A window limited to another model, such as Fable while jobs use Opus, is still displayed but marked not counted.
- At 90% or more in any counted window, ticks stop starting new work, including continuations. Running work is untouched. Scheduled night and review jobs do not start, and the summary records why. `agentq run` and manually invoked night or review remain available with a warning.
- At 98% or more, `agentq run` and manual night or review are also refused.
- One minute after reset, to tolerate a fast local clock, the window is considered recovered and automatic intake resumes. agentq also refreshes the reading. A window without a parsed reset time remains blocked until a later read, normally within 10 minutes.
- A failed, timed-out after 60 seconds, erroneous, or unrecognized read, including output with no `Current ... % used` line, is treated as unknown and work continues. Tick logs say that quota is unknown and work is starting anyway. `agentq status` marks the cause and says work continues.
- The second line of `agentq status` follows a shape such as `Quota: session 2% (resets 10-11 13:59); week 40% (resets 10-13 20:59); week (Fable) 0% (...) (Fable only, task uses opus, not counted) (read 10-11 09:20)`. When blocked, the Scheduling line says which counted window reached 90% and when automatic intake will resume. If the cached reading is stale, status performs a fresh display-only read without writing it back.
- In `~/.claude/nightly/<date>-agentq.md`, the first reading of each day and every change among normal, 90% or more, 98% or more, and unknown adds one quota line with all windows and reset times. Blocking and recovery are stated explicitly. The morning report's agentq health line includes the same information according to the global `nightly-review` skill.

## Per-job usage

- Whenever a job ends, including success, failure, interruption, timeout, or manual stop, agentq reads `result.json`, records input, output, cache-read, and cache-write tokens plus estimated cost, then stores them in `meta.json` and the `usage` object in `state.json` as `input_tokens`, `output_tokens`, `cache_read_tokens`, `cache_write_tokens`, and `cost_usd`.
- When `modelUsage` exists, token counts are summed across all models, including subagents and mixed-model work. Top-level `usage` is used only as a fallback because it covers the main session only and has been observed to omit subagent usage. Cost comes from `total_cost_usd`, an API-list-price estimate rather than the amount charged under a subscription, or from the sum of per-model `costUSD` as a fallback. If a killed, crashed, or older CLI result has none of these fields, usage is omitted. A continued session's `modelUsage` and `total_cost_usd` have been observed to be cumulative, so the continuation records the difference from the previous result in the same session. If any field is smaller than before, it is treated as non-cumulative and recorded as-is. If an intermediate continuation left no usage because it was killed or crashed, that usage will appear in the next continuation, so the chain total remains correct.
- Recent entries in `agentq status` include a suffix such as `(tokens 6.75M, about $4.61)`, where token count is the sum of all four token fields.
- Each job line in the daily summary includes a breakdown such as `usage 6.75M tokens (input 122, output 69.8K, cache read 6.39M, cache write 292.3K), about $4.61`. A `Daily spend` section near the end, between `<!-- agentq 当天花费：开始… -->` and `<!-- agentq 当天花费：结束 -->`, is rewritten after each usage-bearing completion from all jobs that ended that day. It contains totals, cache-hit rate as cache reads divided by cache reads plus cache writes plus input, and the three highest-cost jobs. New lines are inserted before this block. Because rewriting reads and atomically replaces the whole file, a line appended at exactly the same time by the guard, a post-job tick, or a night/review run that failed before taking the lock can be lost.

## First-pass satisfaction and rework rates

These metrics track whether agentq gets better at matching the user's expectations. Only fixed formats count, and `agentq quality` lists the evidence for each classification. Ambiguous cases are excluded.

- Eligible items are checked `- [x]` entries in each project's Completed section that include `合并提交 <hash>` or `合并，提交 <hash>`, the formats written by `agent-task`. If more than one appears, the last one is used so a request that merely mentions an earlier merge is not mistaken for this item's merge. The merge time is the commit time in that project repository and must fall within the last seven days. A hash not found in that repository is excluded and listed by `agentq quality`. Items without a merge hash, such as decision-only entries, are excluded. HTML-comment templates do not count.
- First-pass satisfaction equals items with no acceptance feedback divided by items with or without feedback. Acceptance feedback means an item anywhere in BACKLOG whose title has the Chinese fixed form `验收 <original title> 的意见` or `验收「<title>」的意见`, as written by the dispatch skill. Matching first tries exact titles after ignoring whitespace, backticks, and quotation marks. Otherwise it tries a contained title of at least four Chinese characters, excluding feedback items themselves. Multiple possible original items are ambiguous and excluded; unmatched feedback is listed by `agentq quality`. A body sentence does not count. Without quotation marks, whitespace must follow `验收`; phrases such as `验收：……有意见` and `验收后按审查人的意见改` do not match. Template placeholders like `<...>` do not match. A just-merged item counts as having no feedback even if the user has not reviewed it yet.
- Rework rate equals rework items divided by items classified either rework or not rework. An item is rework if either: (1) it is acceptance feedback for an item merged within the seven days preceding its own merge time; or (2) its original request, before its own merge notation, or its merge commit message refers to another commit merged in that period. Commits on the current item's own branch do not count. The implementation follows first parents backward from the merge commit to the first commit whose subject begins with `待办：`, the claim commit, and treats the commits before that boundary as external. Text after the item's merge notation, such as acceptance instructions, is not scanned. A referenced hash that cannot be found, or feedback that cannot map to exactly one item, makes the item ambiguous unless there is another independent rework signal. With no signal, the item is not rework. Hashes are 7 to 40 lowercase hexadecimal characters and must contain a digit; an all-digit value is accepted only when exactly seven characters long so CI run numbers and dates are less likely to match.
- Any reference counts. Whether the request says a change fixes something introduced by commit X or follows up on a review of X, referring to another commit merged within seven days counts as rework because the previous change did not finish in one pass.
- Limitations: a fix for a recent merge that neither names the commit nor uses the acceptance-feedback form is classified as not rework; the dispatch skill therefore requires rework items to record that commit. An item without a claim commit, such as work edited directly in a session, can cause the first-parent scan to continue to an older claim commit for up to 50 commits and treat other items' commits as its own, undercounting rework. A seven-digit number is tried as a commit and becomes ambiguous if it cannot be found. If acceptance instructions after an item's merge note contain another `合并提交 <hash>`, the final occurrence is treated as this item's merge. Every calculation runs `git show` for every completed item with a hash; on the original machine, four projects took about 0.5 seconds, and time grows linearly with BACKLOG size.
- The daily summary's automatic block contains a `First-pass satisfaction and rework rates` section with one aggregate line and one line per project, beside the Daily spend section. It is recalculated from current BACKLOG and git state after each completed job. On a day with no usage-bearing jobs, only this metrics section remains. The first line of `agentq quality` also reports the total number of acceptance-feedback entries across project BACKLOG files. A 100% satisfaction rate when that count is zero only means no feedback data exists. The morning report copies the first line of `agentq quality` into agentq health according to the global `nightly-review` skill.

## Rapid-failure circuit breaker

A system-wide failure, such as Claude exiting immediately, broken authentication, or a bad global configuration, can make every job fail within seconds. Because each ending job immediately triggers another tick, the queue could otherwise be exhausted in minutes.

- A rapid failure means an unfinished job, including failure, interruption, or startup failure, whose runtime is shorter than `breaker_quick`, three minutes by default. Runtime ends at Claude's actual exit. When `result.json` is nonempty, its modification time is used because Claude writes it only on exit; otherwise finalization time is used. Jobs stopped with `agentq stop` and usage-limit hits neither count nor reset the streak.
- After `breaker_streak` consecutive rapid failures, three by default, the breaker opens. Ticks start no jobs, including continuations, for `breaker_cooldown`, 30 minutes by default. The daily summary records a breaker alert with the latest job ID and summary. `agentq status` reports an active rapid-failure breaker.
- If the first job after the breaker expires also fails rapidly and no job has completed normally in between, the breaker opens again immediately for twice as long, capped at `breaker_max`, four hours by default.
- A completed job, or any job that runs longer than `breaker_quick` even if it later times out, clears the streak and backoff. A completion also closes an active breaker and records that recovery. Rapid failures that finish while the breaker is already open are not counted again.
- `agentq run` ignores the breaker because it is an explicit user request, but prints a warning. Its result still affects the streak. After fixing the underlying issue, run one job explicitly; a successful completion clears the breaker immediately.

## Interruption and automatic continuation

A normal Claude exit with `is_error=false` does not always mean the work is complete. A worker that used a background command or notification mechanism can exit while waiting. Finalization therefore verifies the project state.

- agentq rereads the project's BACKLOG. If the same item remains in Pending with an in-progress marker, the job is marked **interrupted**. The branch name is extracted from the marker, git is used to report how many commits it is ahead of main and whether it has merged, and the summary says when the result came from a background-notification turn (`origin.kind` is `task-notification` or `result_index` is greater than 0).
- If the item is no longer marked in progress but the result does not say whether it merged, the job is also marked interrupted, but it is not continued automatically. The user must verify it.
- For an interrupted job with a session ID, the next opportunity to start work, subject to pause, cooldown, concurrency, and login checks and before selecting a new item, runs `claude -r <session-id> -p "<continuation prompt>"`. Other arguments and `--name` stay unchanged, and this is recorded as a new job. The prompt says the previous background process no longer exists, asks the worker to continue from git and branch state, and requires waiting in the foreground. Immediately before continuation, agentq checks BACKLOG again and cancels if the item is no longer in progress, for example because the user handled it.
- If the continuation is interrupted again, it is not continued a second time. The summary says it still interrupted and needs user handling.
- A usage-limit job skips the normal completion verification. If its item remains marked in progress, it is still marked interrupted and continued after cooldown. A continuation that hits the limit can continue again because the limit is not the agent's fault, but one item gets at most three consecutive process runs before it is left for the user.
- The daily summary records `Interrupted`, `Continuing`, and the final continuation result as separate lines.
- New workers include `--disallowed-tools Monitor`, preventing Monitor and similar notification use. This was observed on Claude 2.1.295 on 2026-10-10 to apply to subagents launched by the worker as well. Background Bash processes remain possible, so the `agent-task` skill requires all waits to complete in a foreground loop. Finalization and continuation catch anything that slips through.

Summary selection follows this order: the last paragraph containing a merge hash in a fixed form, where `合并提交`, `提交号`, or `merge commit` is immediately followed by at least seven lowercase hexadecimal characters; otherwise the last paragraph that states merge status; otherwise the final paragraph. A sentence that merely says a review concluded something can merge does not match. For a normally completed job, when the Completed section contains an item with the same title and `合并提交 <hash>`, that hash is prefixed to the summary unless it already appears in the first 80 characters. If Pending still contains an item with the same title, such as a blocked unmerged item, no older hash is reused. A renamed title may no longer match, in which case only the result text is used.

## Early stall detection

Previously, a job waiting forever or looping could remain until the four-hour `timeout`. agentq now detects inactivity sooner.

- A new job starts with `--session-id <random UUID>`. A continuation uses `-r` and keeps the same session ID. The ID is stored in `state.json`.
- Each tick checks the latest modification time of `~/.claude/projects/*/<session-id>.jsonl` and child-agent logs under `<session-id>/subagents/*.jsonl`. If none exist or they predate startup, startup time is the baseline. After no growth for `stall_after`, 45 minutes by default, the log and daily summary record one possible-stall line, and `agentq status` marks the job. When records grow again, the flag clears.
- **agentq does not kill a job only because it looks stalled.** A long foreground test or build can leave session records unchanged. The normal timeout still stops a truly stuck job. After inspecting logs, the user can run `agentq stop <job-id>`.
- Jobs started by an older version without a preselected session ID are not checked.

## Avoiding the wrong kill after reboot

A PID and process-group ID stored in `state.json` are valid only for the current boot. After a reboot, the operating system can reuse them for unrelated processes.

- Each job records boot time from `sysctl -n kern.boottime`. During finalization, if current boot time differs by more than 60 seconds and the original process is gone, agentq concludes the machine rebooted after startup and does not signal the recorded process group. A result written before the reboot is finalized normally; otherwise the job is marked interrupted and its summary says the machine rebooted. If the BACKLOG item remains in progress, has a session ID, and was not itself a continuation, it is continued once under the same rules as other interruptions. Reboot interruptions do not count as rapid failures.
- If boot time changed but the process still exists, which can happen after clock adjustment, the recorded boot value is updated and supervision continues. If boot time cannot be read, reboot detection is skipped, but any later process-group cleanup still requires the marker described below.
- A job wrapper sends SIGTERM to the process group immediately after Claude exits if children remain in it. At that instant the process-group ID cannot have been reused. If a tick later finds remaining group members after the wrapper is gone, it sends SIGTERM only when `ps -E` shows a process with `AGENTQ_JOB=<job-id>`, an environment marker inherited from Claude. The command-line fragment `--name agentq <project> ` identifies only a project and is not enough. macOS system commands such as `/bin/zsh` and `/bin/sleep` may hide environment variables from `ps`; if only those remain, agentq leaves them alone rather than risk killing the wrong process.

## Corrupted state files

- If `~/.agentq/state.json` has malformed content, such as incomplete JSON or an empty record, a tick first copies it unchanged to `state.json.bad-<timestamp>`, then rebuilds the job list from `~/.agentq/jobs/*/meta.json`. Running jobs can still finalize and time out based on their original start times; process identity is checked against the `agentq <project> ` command-line marker. Logs and the daily summary record the backup path, how many job records were rebuilt, and any unreadable `meta.json` files. The recovery tick only finalizes and starts no new work; the next tick resumes normal intake.
- Reconstruction loses the rapid-failure breaker deadline and streak. A usage cooldown can only be estimated from the limit-hit job's end time plus `limit_cooldown`, so a cooldown originally tied to a later parsed reset time may become shorter.
- A state file that cannot be read because of permissions or I/O failure, even if its content may be valid, remains unknown. agentq does not finalize, start, or back it up and only writes a log. `agentq run` and `stop` also report the error until a tick can rebuild malformed content.
- If writing `state.json` fails after a process starts, later ticks would not know about that process or enforce its timeout. agentq therefore sends SIGTERM to the new process group immediately, follows with SIGKILL after 10 seconds if needed, marks the job failed, records in its summary that startup-state persistence failed, and adds it to the daily summary. `meta.json` still preserves the job record.

## Environment-variable allowlist passed to Claude

The job's Claude process, the job wrapper, and `claude auth status` receive only these variables. Everything else is omitted.

- `PATH`, `HOME`, `USER`, `LOGNAME`, `LANG`, `LC_*`, `TMPDIR`, `SHELL`, and `SSH_AUTH_SOCK`;
- `CLAUDE_CONFIG_DIR` and `CLAUDE_CODE_OAUTH_TOKEN`, when deliberately set by the user for login or configuration;
- `HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY`, `ALL_PROXY`, and lowercase equivalents, because an interactive `agentq run` can need them for network access;
- agentq's own `AGENTQ_*` variables, including `AGENTQ_JOB`.

`ANTHROPIC_API_KEY`, `ANTHROPIC_BASE_URL`, and other `ANTHROPIC_*` variables therefore cannot silently route a worker to API billing or another account. `CLAUDECODE` and other `CLAUDE_CODE_*` values injected by a Desktop session also cannot make the worker believe it is a nested session. A project that needs another variable, such as a service credential, must configure it in the project rather than rely on agentq inheritance.

## Start the next item as soon as a job ends

Every Pending job starts through an internal wrapper: `agentq _job <job-dir> <project-dir> <claude> <args...>`. The wrapper starts Claude in its own process group, waits for it, then replaces itself with `agentq tick --after <job-id>`. That tick immediately finalizes the ended job and selects the next item, so queued work can continue within seconds instead of waiting for the next five-minute launchd tick. The scheduled tick remains as a fallback if the wrapper dies or the post-job tick fails.

- `state.json` still records Claude's own PID. Claude retains its own process group, so timeout, finalization, and `stop` behavior are unchanged. `ps` also shows one `agentq _job ...` wrapper, which exits after the job.
- Post-job and scheduled ticks share `tick.lock`. They cannot overlap. A post-job tick waits up to 60 seconds for the lock; a scheduled tick skips immediately when the lock is held. This prevents a job that ended just after another tick inspected it from leaving an empty slot for five minutes.
- The post-job tick obeys pause, cooldown, concurrency, and `retry_after` exactly like a scheduled tick. It writes to `~/.agentq/launchd.log` with a prefix identifying the job whose exit triggered it.
- A manual stop or timeout also lets the wrapper trigger a tick and start other work immediately. The stopped item remains subject to `retry_after`, so it is not retried at once. Run `agentq pause` before stopping if no new work should start.
- Pending-item retry spacing still comes from `retry_after`, preventing repeated attempts of one item. A very short configured retry interval combined with Claude exiting immediately can still create retries only seconds apart; before post-job ticks, the minimum spacing was usually five minutes.
- Set `"tick_after_job": false` in `config.json` to disable this behavior. Jobs then finalize and new work starts only on scheduled ticks.

## Night shift and review

Night and review jobs are called by explicitly selected agentq services. Existing Desktop scheduled tasks are not migrated or changed automatically. The user must choose one scheduling path to avoid duplicate runs.

- `agentq night` and `agentq review` each use `claude -p` with repository instructions from `prompts/night.md` and `prompts/review.md`. The review prompt is a light entry point that delegates to the single source in `skills/nightly-review`; night keeps the local scheduling switch. Desktop tasks do not overwrite these source files. A fixed preamble explains that the process exits when the `-p` turn ends, so every wait must use a foreground Bash `until` loop rather than a background process, Monitor, or waiting for a notification. In observed `-p` behavior, a standalone long foreground `sleep` was blocked while `until ...; do sleep 30; done` worked.
- Prompt files are compiled into the executable. Changes to `prompts/*.md` take effect only after they are merged to main and `scripts/update.sh` updates the installed generation.
- Startup uses the same `--model`, `--effort`, locally configured `permission_mode`, Monitor prohibition, environment allowlist, and job wrapper as Pending work. The working directory is `projects_root`. Records live under `~/.agentq/jobs/<id>/`, with IDs such as `1011-010000-夜班`. Completion writes a night or review line to the daily summary, and `agentq logs <id>` shows the full result.
- Separate timeouts apply. `night_timeout` defaults to six hours because a 01:00 night shift can wait until 05:00. `review_timeout` defaults to four hours because review can wait for the night report until 08:00. A tick kills the whole process group at the deadline and records a timeout.
- Night and review do not consume `max_parallel` or `per_project` slots because night may wait for ordinary jobs to finish. `agentq status` lists them separately as scheduled jobs rather than in the ordinary running count. A second job of the same kind is not started while one is running.
- These jobs have no BACKLOG item, so finalization does not run Pending-item interruption checks or continuation. A normal exit without the required report, `~/.claude/nightly/<date>-night.md` for night or `<date>.md` for review, is marked interrupted. A report dated either the start or finish day counts.
- Startup ignores `paused` and the rapid-failure breaker, although the night prompt must not resume automatic scheduling. Usage cooldown and CLI authentication still apply. A scheduled run that cannot start writes one summary line explaining why. Rapid exits affect the breaker streak, but successful night and review jobs do not clear the streak or close the breaker.
- **Computer use is unavailable inside `agentq night`.** It cannot click Xcode Organizer to upload a TestFlight build. Uploads must use a non-window App Store Connect API script. Existing release skills prefer that path. If a project only documents Organizer or an API upload fails and would need an Organizer fallback, the night shift records the failure and the morning report exposes it.
  - The Claude Code documentation for computer use says it requires an interactive session and is unavailable in non-interactive mode with `-p`.
  - On 2026-10-10 with Claude 2.1.295, one-shot launchd services were tested with the same environment and arguments as agentq night, once from `~/projects` and once from a directory whose interactive `/mcp` configuration enabled computer use. ToolSearch found no deferred tool matching computer or screenshot in either run, both runs reported no computer-use tool, and stderr was empty.
  - Locking the screen does not change the absence of computer use. API-based upload can work while the screen is locked when the login keychain remains available. This does not claim that every launchd, locked-screen, real upload combination has been tested.
- `agentq setup --services --yes` installs launchd calendar jobs `io.agentq.agentq.night` at 01:00 and `io.agentq.agentq.review` at 06:30. They call `~/.local/bin/agentq night|review --scheduled` with the tick service's PATH and log file. Installation does not run them. If the machine is asleep at the scheduled time, launchd starts them after wake.
- A user launchd service cannot run before login. If the machine was off, disconnected from power, or not logged in at the scheduled time, launchd does not replay it. Every tick checks whether the scheduled time passed at least 15 minutes ago, no job of that kind ran today, and the expected report does not exist. Night can be recovered before 08:00; review can be recovered any time that day. A recovered run is recorded and marked in its title. An already-running job of the same kind blocks recovery; a running night job also blocks review recovery. A recovered night prompt moves its wait deadline from 05:00 to 08:00. Recovery ignores pause and breaker but obeys usage cooldown, authentication, and quota. A failed recovery attempt writes only to `launchd.log` and can be retried by the next tick. Automatic power restart plus automatic login can restore operation after an outage, but automatic login requires FileVault to be disabled.
- `night_enabled` and `review_enabled` default to false. When launchd wakes a disabled job, agentq writes one log line and does nothing. `agentq status` shows the 01:00 and 06:30 switch states.

To move from Desktop scheduling to agentq without running both:

1. Confirm services exist after `agentq setup --services --yes` with `launchctl print gui/$(id -u)/io.agentq.agentq.night` and the equivalent review label.
2. Disable the Desktop tasks named `night-shift` and `nightly-review`; they do not need to be deleted.
3. Edit only `night_enabled` and `review_enabled` in `~/.agentq/config.json` to `true`. Confirm with `agentq status`. Reinstalling is unnecessary because switches are read at invocation time.
4. The next morning, inspect `~/.claude/nightly/<date>-night.md`, the morning report, and the night and review lines in `<date>-agentq.md`.

To move back to Desktop scheduling, first set both switches to false, then re-enable the two Desktop tasks.

## Patrol

agentq proactively detects problems. New agentq or project problems, such as failed or stalled jobs, can be sent to the user through [Notifications and Telegram](#notifications-and-telegram). Infrastructure checks, including launchd exit status and listener availability, backup pushes and reports, morning reports, and disk space, are still recorded in `patrol.json` and the daily summary but are not pushed by patrol when an independent watchdog is responsible, preventing duplicate alerts. Checks the watchdog cannot perform, such as missing scheduled or listener services and missing night reports, still produce patrol notifications. A tick patrols once an hour when `patrol_enabled` is true. `agentq patrol` runs immediately.

| Check | What counts as an issue | Evidence |
| --- | --- | --- |
| launchd services | Any service sharing agentq's configured label prefix has a nonzero last exit code; the managed installation exists and night or review is enabled but its scheduled service is absent; or Telegram is configured but the listen service is absent or has no PID | Exit code, where a negative value is a signal; PID; last three lines of the log named by the service plist |
| Automatic backup push | `~/.claude/state/autopush.log` has a fixed-format `FAIL` line in the last 24 hours with no later `ok` for that repository. One issue covers the latest uninterrupted failure streak, even if it began more than 24 hours ago. A success closes it; a later failure starts a new issue | Actual start time, failure count, and final source line |
| Daily backup report | The first line of `~/.claude/nightly/<date>-backup.md` says there were one or more failures | The first line and lines marked as push failures |
| agentq jobs | A failed, timed-out, or interrupted job in the last 24 hours, excluding usage-limit jobs and jobs waiting for or already receiving automatic continuation; or a running job whose session record has not grown for `stall_after` | Job ID, start and end times, summary, and inactivity duration for stalls |
| Subscription quota | A cached reading in state has a counted window at 90% or more. Patrol does not refresh quota | Window percentages, reset times, and read time |
| Morning and night reports | After 08:00, review is enabled but `<date>.md` is missing, or night is enabled but `<date>-night.md` is missing. A report job still running is not an issue | Today's job state and summary, or a statement that no job exists today |
| Decision needed | Unchecked items in each project's Decision needed section, excluding deferred-decision markers. If a previously reported item gains a marker, it disappears silently rather than being recorded as handled | Original item text |
| Disk | The filesystem containing HOME has less than 20 GB free | Free and total space |

- Each issue, including fingerprint, evidence, handling note, first-seen time, and last-seen time, is stored in `~/.agentq/patrol.json`. A new issue also adds one line to the daily summary. Possible stalls and quota states already recorded by the tick are added only to patrol state, avoiding duplicate summary lines.
- Fingerprints prevent repeat reports, for example `launchd:<service>`, `autopush:<repo>@<first-failure-time>`, `job:<job-id>`, `decide:<project>:<title-hash>`, or `report:night:<date>`. Persistent issues only update last-seen time. A resolved service, successful push, newly created report, freed disk, resumed session record, or quota below 90% records recovery. A decision item leaving its section records handled. A night or review recovery job that has not yet written its report remains open. Issues that only age out of the 24-hour window, yesterday's missing report, a stalled job that ends, or a service that was removed rather than repaired disappear silently. If one class of check cannot run, previous issues in that class remain.
- Automatic handling applies to failed, timed-out, or interrupted Pending jobs whose original BACKLOG item remains in progress and has no other worker taking it. agentq appends a `Take over interrupted <title>` item to Pending and commits only BACKLOG. Before writing, it takes the `<project>-main` lock through `with-lock.sh` for up to two minutes. It does not write when the main checkout is not on main, BACKLOG has uncommitted changes, or scheduling is paused, and the handling note explains why. If a takeover item itself fails, no nested takeover is created. Other issues have no sufficiently safe automatic repair and receive a user-action note such as reinstalling a service or stopping a job.
- When a new anomaly appears, excluding decision items and quota, and `patrol_analyze` is enabled, agentq can start one read-only `claude -p` analysis, up to three times per day. It does not start while paused, cooling down, circuit-broken, at quota of 90% or more, or while another patrol analysis is running. Analysis is recorded like a night job, with an ID such as `1011-120000-巡逻分析`, a 30-minute timeout, and no concurrency-slot use. Completion writes one conclusion line to the daily summary.
- `agentq status` shows the last patrol time and current issue count, with a hint to run `agentq patrol` for evidence.

## Notifications and Telegram

agentq can proactively contact the user, allowing a short phone reply to dispatch work, answer a decision, or ask for status. `notify_enabled` controls this entire subsystem. A fresh managed installation sets it to false.

**Channels:** With a Telegram bot configured, messages go to Telegram. Otherwise, macOS local notifications use `osascript`. Two-way chat is available only through Telegram. Credentials are read only from `~/.agentq/secrets/telegram.env`, containing `TELEGRAM_BOT_TOKEN=...` and `TELEGRAM_CHAT_ID=...`. The file must have mode 600 or it is ignored and status explains why. agentq never prints the credentials, and any token found in logs or errors is replaced with `***`. It sends only to the configured chat ID and accepts messages only from that chat. Other chat IDs are logged as ignored without a reply or action. `agentq notify setup` runs `~/.agentq/secrets/telegram-setup.sh` when that file exists; otherwise it prints four manual setup steps. `agentq notify test` sends one test message. A failed Telegram delivery remains queued for the next tick, with one local notification as fallback.

**What gets sent:** regular notifications flush at the end of a tick. `agentq notify send` tries immediately after adding a message.

| Category | When | Producer |
| --- | --- | --- |
| Morning report | Once per day when `~/.claude/nightly/<date>.md` has stopped changing for two minutes, no review job is running, and local time is at least 08:00. Decisions and non-infrastructure patrol issues come first, then work completed in the last 24 hours, job outcomes and quota, then the report's first line. A successful Telegram push creates `~/.claude/state/report-pushed-<date>` so a separate interactive handoff does not send the same report again | tick |
| Decision needed | A new unchecked item appears in a project's Decision needed section. Ticks scan every five minutes. The first run records existing items without alerting. Deferred-decision items do not alert until their marker is removed. A reply choosing an option updates the item and dispatches it | tick |
| Patrol alert | A newly detected issue with no automatic repair, including failed, timed-out, interrupted, or possibly stalled jobs; missing services; and missing night reports. A takeover item already written or being handled suppresses the corresponding job alert. Decisions and quota have separate categories. Infrastructure handled by the independent watchdog is not pushed by patrol | patrol |
| Important notice | Counted subscription quota reaches 90%, once per day and on tier changes, or the rapid-failure breaker opens | tick |
| Release available / Done | A new test build is available or a new project's first usable version is ready, including how to inspect it | night shift or agents using `agentq notify send release|done ...` |

Individual job completions, clean patrols, successful automatic retries, and lesson or rule changes are not pushed. They go into the daily summary and morning report.

**Delivery rules:**

- The first message of a category is sent immediately. Additional messages in the same category within 30 minutes are held and combined when the window ends. For one key identifying the same issue, only the latest wording remains.
- From 23:00 through 08:00, ordinary notifications accumulate. After 08:00, they join the morning report if it is ready; otherwise each category sends one combined message.
- The normal daily limit is 10 messages. Morning reports and decision requests bypass the limit but still count toward it. Ordinary messages above the limit are not discarded; they attach to the next decision or security message, otherwise wait for the morning report or next day.
- Security messages marked with `agentq notify send --security`, such as a future leak alert, bypass combination, quiet hours, and the daily limit. Backup failure remains a watchdog responsibility.

**Format:** Telegram uses HTML `parse_mode`. The implementation emits messages in this structure, with localized content:

```text
🟠 <b>巡逻发现 · 10月11日</b>

🟠 自动备份推送失败：my-app
~/.claude/state/autopush.log 里 10-11 12:21 起连续 2 次 FAIL …
没自动处理：每天 05:30 的定时备份会再推一次 …

🟠 磁盘剩余不到 20 GB
<i>12:30 · agentq</i>
```

Times are written by the program in a form such as `10-13 周二 21:00`; prompts instruct workers to copy the computed weekday and use `date` rather than calculating another date's weekday mentally. The title line is `<marker> <b>category · date</b>`. Each section begins with one marker and does not repeat markers within the paragraph. Project names use full-width spaces for alignment. Required user actions come first. The final line is `<i>time · source</i>`. Messages avoid exclamation marks and pleasantries. If nonblank content exceeds 12 lines, Telegram receives the leading summary and a final line pointing to the remaining local text at `~/.agentq/notify/<time>-<category>.txt`. Dynamic text is HTML-escaped. If Telegram returns a 400 entity-parsing error, agentq strips HTML tags and retries as plain text.

**Talking through Telegram:** `agentq listen` is a resident process run by the `io.agentq.agentq.listen` launchd KeepAlive service. It uses `getUpdates` long polling with a 50-second timeout and receives a message as soon as the user sends it. Offset and message receipt are stored atomically in `notify.json`, preventing duplicate processing across restarts. While it holds `~/.agentq/listen.lock`, ticks do not call `getUpdates`, because Telegram does not allow competing consumers. If the listener is absent or between restarts, the five-minute tick performs a short poll as fallback. After `scripts/update.sh` replaces the executable, the listener notices within one long-poll cycle, exits, and launchd restarts it with the new version. It rereads configuration and credentials every cycle, so changing `notify_enabled` or replacing the token does not require a restart. Without Telegram configuration, it waits and checks once a minute rather than repeatedly crashing.

- A whole message equal to the localized word for status, or `status`, `/status`, or `/start`, receives a status summary immediately without starting Claude or consuming model quota. The summary includes decisions, scheduling, quota, running and pending work, and recent completions.
- Any other text gets an immediate acknowledgement that it has been given to Claude, or that it is queued behind N earlier conversations. During usage cooldown or at 98% quota, it returns the reason instead. Then one `claude -p` process runs under the user's global rules and skills from `projects_root` to infer intent. It can dispatch work through the dispatch skill, apply a decision reply and move the item back to Pending, answer questions or status requests, or complete a small reversible action immediately while leaving decisions that require the user untouched. The final response is returned under the user's latest message. A failed response says it could not answer and includes the cause. Conversation replies do not enter the notification combination queue, quiet hours, or daily limit. An undelivered reply stays in `notify.json` for retry by later ticks for up to 24 hours.
- Conversation and patrol-analysis runs are recorded as jobs, with IDs such as `1011-120000-Telegram对话`, a 20-minute timeout, no normal concurrency-slot use, and a daily-summary entry. Patrol does not itself report their failures. Only one conversation runs at a time; messages arriving meanwhile queue and are combined for the next worker when the current job's wrapper triggers a tick. Conversation ignores pause and breaker like an explicit `agentq run`, but usage cooldown, 98% quota, and missing authentication block it with an immediate explanation.
- Telegram lets a bot download photos, videos, and files up to 20 MB. agentq saves an incoming attachment under `~/.agentq/inbox/<message-number>-<filename>`, moves it into `~/.agentq/jobs/<id>/files/` when creating the conversation job, and includes that path in the prompt. Images can be viewed directly; video gets one extracted frame. A dispatched BACKLOG item keeps the exact attachment path. Files above 20 MB receive a request to compress or provide a local path. Voice messages receive a request for text. A multi-photo album receives one acknowledgement.
- In a conversation, Claude can call `agentq notify file <path> [caption]` to send an image or recording immediately beneath the user's message. Night shifts and agents use `agentq notify send --file=<path> ...`; the attachment follows the notification. Without Telegram, a local notification contains the attachment path. PNG, JPG, and WebP up to 10 MB use `sendPhoto`. If Telegram rejects image dimensions, such as an extreme long screenshot, agentq retries it as a document. MP4, MOV, and M4V use `sendVideo`; other files use `sendDocument`, each up to 50 MB. A larger recording is compressed under `~/.agentq/notify/media/` using `ffmpeg` when available or `/usr/bin/avconvert` otherwise, trying 1280x720, 960x540, then 640x480. If it remains too large, or another file exceeds 50 MB, agentq sends the local path instead. Compression occurs when `notify send --file` is called, not while a tick holds its lock. Limits come from the Telegram Bot API sections on sending files and `getFile`.
- Known limitation: a bot configured with a webhook cannot use `getUpdates`. Telegram returns 409; the listener retries once per minute and logs the error. Stickers and unsupported message types receive a localized reply saying only text, images, video, and files are understood.

`agentq status` adds a notification line with channel, today's count out of 10, and queued count. With Telegram configured, it also reports whether the listener PID is running or the five-minute tick fallback is active. `agentq notify status` lists queued notifications, conversation messages awaiting answers, and the latest delivery error.

## Configuration: `~/.agentq/config.json`

The table below describes legacy runtime defaults applied to missing keys. A fresh managed installation explicitly overrides them with `paused=true`, night, review, patrol, and notifications disabled, and `permission_mode=default`. An existing file is preserved byte-for-byte.

| Key | Default | Meaning |
| --- | --- | --- |
| `projects_root` | `~/projects` | Scan direct child directories here |
| `claude_path` | `claude` | A fresh complete installation records the absolute independent CLI path after validation |
| `permission_mode` | Legacy missing-field behavior is bypass; fresh install is default | Explicit user policy, never inherited from the author's permissions |
| `model` / `effort` | `opus` / `high` | Passed to `--model` and `--effort` |
| `max_parallel` | 2 | Maximum ordinary jobs running globally |
| `per_project` | 1 | Maximum ordinary jobs running in one project |
| `timeout` | `4h` | Kill the entire job process group after this duration |
| `retry_after` | `12h` | Wait before trying the same item again |
| `limit_cooldown` | `1h` | Fallback cooldown after a usage-limit hit |
| `paused` | false | When true, ticks finalize but do not start work |
| `tick_after_job` | true | Tick immediately when a job ends; false relies only on scheduled launchd ticks |
| `breaker_streak` | 3 | Consecutive rapid failures required to open the breaker |
| `breaker_quick` | `3m` | An unfinished job ending sooner than this is a rapid failure |
| `breaker_cooldown` | `30m` | Initial rapid-failure breaker duration |
| `breaker_max` | `4h` | Maximum breaker duration after repeated failures |
| `stall_after` | `45m` | Mark a running job possibly stalled after no session-record growth for this long; do not kill it |
| `night_enabled` / `review_enabled` | false / false | Whether scheduled launchd calls at 01:00 and 06:30 actually run night and review |
| `night_timeout` / `review_timeout` | `6h` / `4h` | Separate deadlines for night and review |
| `patrol_enabled` | true | Whether a tick patrols hourly. `agentq patrol` ignores this switch |
| `patrol_analyze` | true | Whether a new patrol anomaly may start read-only `claude -p` analysis, at most three per day |
| `notify_enabled` | true | Whether notifications and Telegram receiving are active. Without Telegram, outbound notifications use macOS locally |

## Files

- `~/.agentq/state.json`: job list, usage cooldown and breaker deadlines, rapid-failure streak, and latest subscription readings. Malformed content is copied to `state.json.bad-<time>` and rebuilt from job metadata. An unreadable file is treated as unknown; see [Corrupted state files](#corrupted-state-files).
- `~/.agentq/jobs/<id>/`: `prompt.txt`, Claude JSON in `result.json`, `stderr.log`, and job record `meta.json`, including usage after completion.
- `~/.agentq/launchd.log`: logs from scheduled ticks, guard failures and rollback, scheduled night/review calls, job wrappers, and post-job ticks.
- `~/Library/LaunchAgents/io.agentq.agentq{,.night,.review,.listen}.plist`: four launchd definitions installed by the legacy `agentq install` path or managed service installation, depending on the installation generation.
- `~/.agentq/listen.lock` and `~/.agentq/listen.pid`: the resident listener's lock and PID. Ticks use them to avoid competing for inbound messages. `~/.agentq/inbox/` holds attachments not yet assigned to a conversation job.
- `~/.agentq/tick-guard.sh` and `~/.agentq/guard-fails`: the launchd guard and its consecutive-failure count; see automatic rollback in the install guide.
- `~/.local/bin/agentq.prev` and `agentq.bad`: compatibility files left by legacy `scripts/update.sh`, containing the previous version and a rolled-back bad version.
- `~/.agentq/patrol.json`: current issues with fingerprints, evidence, handling notes, and first and last seen times, plus last patrol time and daily analysis count. Malformed content is copied to `patrol.json.bad-<time>` and rebuilt empty, so previously reported issues can be reported again.
- `~/.agentq/notify.json`: notification queue, last-send times, daily count, report dates, seen decision items, Telegram offset, conversation messages awaiting answers, undelivered replies, and latest sanitized delivery error. Malformed content is copied to `notify.json.bad-<time>` and rebuilt empty. `~/.agentq/notify.lock` serializes updates from ticks and `notify send`.
- `~/.agentq/notify/`: full text for notifications over 12 nonblank lines and long conversation replies.
- `~/.agentq/secrets/telegram.env`: read-only Telegram credentials with required mode 600.
- `~/.claude/state/report-pushed-<date>`: marker that the morning report reached Telegram.
- `~/.agentq/tick.lock`: prevents overlap among scheduled ticks, post-job ticks, `run`, and `stop` state changes.
- `~/.claude/nightly/<date>-agentq.md`: one line per ended job, plus continuation starts, quota tier changes, breaker events and recovery, possible stalls, state-file recovery, guard rollback, patrol discovery and recovery, night and review results, and reasons a scheduled job did not start. An automatically rewritten section at the end contains daily spend and quality metrics.

## Development

```bash
test -z "$(gofmt -l .)" && go vet ./... && go test ./...
```

Tests use a temporary HOME, temporary `projects_root`, and fake Claude executable. Installation tests use a fake launchctl. Guard and `scripts/update.sh` tests use fake agentq executables and source directories under a temporary HOME. Normal tests do not read or write real `~/.agentq`, `~/projects`, `~/.claude`, or launchd state.

## Clean source and configuration backup

The public-source privacy boundary and scan procedures are documented in [privacy.md](privacy.md) and [privacy.en.md](privacy.en.md). Safe configuration projection and rollback are documented in [config-privacy.md](config-privacy.md) and [config-privacy.en.md](config-privacy.en.md). The default `launchd_label` is `io.agentq.agentq`. An existing `~/.agentq/config.json` keeps its service label across a sanitized-source reinstall; source cleanup does not rename an installed service.
