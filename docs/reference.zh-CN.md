# agentq 命令与行为参考

[English](reference.en.md) · [当前安装指南](install.zh-CN.md)

后台开发任务调度器。macOS launchd 每 5 分钟执行一次 `agentq tick`（每个任务结束时也马上执行一次，见下文「任务结束立刻接下一条」），它扫 `~/projects/*/docs/BACKLOG.md` 的「待做」，把能做的条目交给独立的无界面 Claude Code 进程（`claude -p`），按全局 `agent-task` 技能做到合并。不依赖 Claude 桌面端或终端开着。

agentq 自己不改 BACKLOG、不提交、不合并，只负责挑活、启动、看着、收尾、记账。维护与验证合同见 [AGENTS.md](../AGENTS.md)。

## 使用方式：CLI 与可选 Telegram

**Telegram 是可选的消息入口，不是 agentq 的必需依赖。** 不配置 Telegram，也能在本机调度任务、查看状态和日志，并生成本地汇总与早报。后台任务仍需可用的 Claude CLI 身份，以及相应的项目工具和功能开关。

| 用途 | 本机 CLI | 配置 Telegram 后 |
| --- | --- | --- |
| 用自然语言交代需求、提问或回复拍板事项 | 在 Claude Code 交互会话中使用，需安装相应工作流技能 | 给机器人发消息，由 agentq 交给 AI 处理并回复 |
| 直接管理任务 | `agentq run`、`agentq status`、`agentq logs` 等命令 | 也可以发消息询问状态或派活 |
| 接收消息 | 当前 macOS 版在启用通知时可使用本机通知 | 接收早报、异常、需要拍板等通知，也可发送任务结果 |
| 查看产物 | 读取本地报告、日志和文件 | 发送截图、录屏或文件，受 Telegram 与当前实现的大小限制 |

Telegram Bot API 只负责消息收发，不负责 AI 推理。`TELEGRAM_BOT_TOKEN` 和 `TELEGRAM_CHAT_ID` 是机器人通道配置，不是模型 API Key，也不能替代 Claude CLI 的登录或模型服务凭据。启用 Telegram 收发需要保持 `notify_enabled=true`；关闭这个开关会同时停止推送和 Telegram 收件。

CLI 自然语言沟通发生在 Claude Code 的交互会话中；agentq 自己提供的是任务管理命令，目前没有通用的 `agentq chat` 命令。当前后台执行器使用 Claude CLI，Codex 后端尚在整理，不能视为已经支持。

两种入口可以使用同一批项目文件和 BACKLOG，但不要把它们当作完整聊天记录会自动同步。Telegram 的配置、接收范围与附件限制见下文「推送和 Telegram」。

## 安装

在已取得的源码根目录运行一条命令：

```sh
sh scripts/bootstrap.sh --yes
```

[中文完整指南](install.zh-CN.md) · [English installation guide](install.en.md) · [给 Agent 的一句安装说明 / llms.txt](../llms.txt)

完整安装包括 binary、八个自有技能完整目录、四个锁定 Waza 技能、支持脚本、五模板与许可证，不要求复制作者配置，不需要 Node。先检测 macOS/架构、Git、Python、独立 Claude CLI 路径/版本/无界面能力及认证。源码构建需 Go（见 go.mod）。CLI-only 可用，Desktop/ZIP 不当作后台引擎。

默认暂停、night/review/patrol/notify 关闭、权限 default；已有配置/第三方 Hook/状态栏原样保留。Telegram 可选，不要求机器人 token。`--scope skills` 仅装技能；`--extras` 加扩展；`--services`、`--backup`、`--watchdog` 明确选择服务，均不恢复全局调度。未知同名技能、本地修改或非法配置先报告并停止，不覆盖定制、不替用户删重。

给 Agent：**读本仓 llms.txt 和安装指南，用同一条 `sh scripts/bootstrap.sh --yes` 安装；保留我的配置，默认暂停，服务/权限/认证变更按我的授权处理。**

## 安全升级与恢复

- 重跑正式入口，或受管安装中执行 `scripts/update.sh`：构建、自检、比较冲突、备份、切换完整代；保留所选可选模块和所有用户配置。
- `agentq doctor`：只读配置/安装完整性/CLI 检测。
- `agentq rollback --yes`：恢复上一完整代，数据/配置不回滚。
- `agentq remove --yes`：卸载受管入口与服务，保留数据/配置/恢复材料；用 `sh scripts/bootstrap.sh restore --yes` 恢复卸载或中断事务。
- 活跃任务、未知状态和用户修改阻止替换；不直接构建覆盖运行中二进制。详情与限制见完整安装指南。

旧版只有二进制和 launchd 的兼容操作保留在 [legacy-install](legacy-install.md)，不算完整工作系统安装。当前公开版本及下载见 [Release](https://github.com/dodo258/agentq-public/releases)。安装以完整指南为准。

## 命令

| 命令 | 作用 |
| --- | --- |
| `agentq tick` | 一次调度：收尾已结束的任务 → 杀掉超时的 → 挑活启动。平时由 launchd 和任务外壳调用，手动执行也可以 |
| `agentq run <项目名> [关键字]` | 立刻为该项目启动一个任务。给了关键字就挑「待做」里第一条包含它的。不看暂停、retry_after 和连环失败熔断，仍遵守并发上限、用量冷却和登录检查；订阅额度 ≥98% 时拒绝（见「订阅额度」） |
| `agentq status` | 是否暂停、用量冷却、额度快用完或连环失败熔断，订阅额度各窗口的用量和重置时间，上次巡逻和现有问题数，正在跑的任务（续跑的标「续跑中」，会话记录久没增长的标「疑似卡死」）、各项目「待做」条数、最近 10 个结束的任务（中断的标「中断」和续跑情况） |
| `agentq patrol` | 立刻巡逻一次，列出现有的每个问题和证据、处理说明（见「巡逻」）。不看 `patrol_enabled` 和上次巡逻时间 |
| `agentq quality` | 最近 7 天的一次满意率、返工率，按项目列出每条合并的条目算成了什么、依据是什么（见「一次满意率和返工率」）。只读 BACKLOG 和 git，不改任何东西 |
| `agentq logs <任务id>` | 任务的状态、结果全文、stderr 末尾。id 写前缀就行（至少 4 个字符且不重名） |
| `agentq stop <任务id>` | 杀掉该任务的进程组（先 SIGTERM，10 秒后仍在就 SIGKILL），标为已停止；进程已经自己结束的，按它的结果正常收尾 |
| `agentq pause` / `agentq resume` | 暂停或恢复自动接活。暂停时 tick 只收尾不启动，`run` 不受影响 |
| `agentq night` / `agentq review` | 立刻执行一次夜班 / 复盘（见「夜班和复盘」）。手动执行不看 `night_enabled`/`review_enabled`；launchd 定点叫醒时带 `--scheduled`，开关为假就只记一行日志 |
| `agentq notify setup` / `test` / `send` / `file` / `status` | 推送（见「推送和 Telegram」）：配 Telegram 机器人；发一条测试消息；`send [--security] [--key=K] [--file=路径]… <类别> <一句话> [补充…]` 记一条要推给主人的消息并按规则推（类别 `decide` `alert` `patrol` `release` `done`，夜班和 agent 用；`--file` 附截图、效果图、录屏，跟在消息后面发）；`file <路径> [说明]` 马上把一个文件发给主人（对话任务里回图用）；看用哪个通道、今天推了几条、攒着哪些 |
| `agentq setup --services --yes` / `agentq remove --yes` | 完整受管安装的服务选择和卸载；旧 binary-only 的 install/uninstall 见 legacy-install |
| `agentq listen` | 常驻收主人的 Telegram 消息，随到随回（见「推送和 Telegram」）。由 launchd 服务 `io.agentq.agentq.listen` 调用，不用手动执行 |

## 挑活规则

- 项目：`projects_root` 下一层、`.git` 是目录的 git 主目录，按名字字母序；跳过以 `.` 开头的目录。
- 条目：`## 待做` 到下一个 `## ` 之间、行首是 `- [ ]` 的行；跳过 HTML 注释、含「（进行中」的行和标了卡住的行。
- 卡住标记只认下面两种（agent-task 放下一条时写 `（卡住：<分支名> <卡在哪>）`），正文里提到「卡住」这个词（比如验收标准写「卡住时 status 有提示」）不算，照常可挑：
  - 括号标记：括号紧挨着「卡住」、后面接冒号、右括号或空白——`（卡住：…）`、`(卡住)`、`【卡住】`、`[卡住 …]`，在行里哪里都算；
  - 行尾标记：去掉「（进行中…）」、行尾空白和句末标点后，行以「卡住」结尾，且前面是破折号（— 或 ——）或「；，。|」（如 `… —— 卡住`、`…；卡住。`）。
  - 不算的例子：`进程会卡住`、`标「卡住」`、`（卡住时 status 有提示）`、`[卡住检测](…)`、`修 claude -p 卡住`。
- 同一条目已有任务在跑就跳过；上次结束不到 `retry_after`（默认 12 小时）也跳过。撞用量上限的那次不算。
- 全局最多 `max_parallel` 个任务，同一项目最多 `per_project` 个，每次 tick 每个项目最多启动 1 个。
- 撞到用量上限就暂停启动新任务，见下文「用量上限」；订阅额度快用完就不领新活，见下文「订阅额度」；连续几个任务秒退就熔断，见下文「连环失败熔断」。

**等条件标记**：「等主人拍板」里的条目要等前置条件满足才轮到主人定（如「前面 7 条都合并」）时，在条目第一行（`- [ ]` 那行）写 `（等条件：<条件>）`。带这个标记的条目巡逻不报、不推「要你定」、早报和「状态」的要你定不算，只在早报详情列一行。只认括号紧挨「等条件」再接冒号的写法（全角、半角括号和冒号都行），正文里顺带提到这个词不算。条件满足由做完最后一条前置任务的 agent 去掉标记（agent-task 技能），去掉后 tick 把它当新条目推「要你定」。

## 用量上限

- 只在可能是撞了上限的结果里找上限字样：`is_error` 为真、`api_error_status` 有值、或回合数不超过 3 且不是后台通知回合（`result_index` 为 0）；result.json 读不出时扫 stderr。正常做了很多回合的任务，结果里提到 rate limit（比如修的就是限流代码）不算。
- 字样：`usage limit`、`rate limit`、`limit reached`、`hit your … limit`（官方文案 `You've hit your session limit · resets 3:45pm` 等）。
- 提示里能解析出重置时间（`resets 3:45pm`、`resets Mon 12:00am`、`resets Oct 12, 9am`、带时区括号的 `resets 8pm (Africa/Cairo)`，或旧版的 `usage limit reached|<unix 秒>`）就冷却到重置时间后 1 分钟；解析不出、不在 8 天之内、或重置时刻已经过了（以 claude 退出、打出提示的时刻为参照解析，收尾晚了也不会推到下一天或下一周），就冷却 `limit_cooldown`（默认 1 小时）。冷却期间 tick 不启动、`run` 也拒绝。
- 撞上限的那次不算「试过」，冷却过后可以再挑。做到一半撞上的（BACKLOG 里这一条已标进行中）记为中断，冷却过后用同一会话续跑（见下文「中断与自动续跑」）。

## 订阅额度

「用量上限」是撞上了才知道；这里是提前看，快用完就不领新活，免得任务做到一半撞上限。做法学 magpie（yetone/magpie 的 `internal/provider/claude_usage_cli.go`）。

- 领活前（tick 挑到能做的条目或要续跑、`agentq run`、`agentq night`/`review` 真要启动时）读一次额度：`claude -p /usage --output-format json --tools "" --strict-mcp-config --setting-sources "" --no-session-persistence`，在空的临时目录里跑，环境变量同任务白名单。`/usage` 是 Claude Code 自己回答的本地命令，不问模型、不花 token（实测结果 `num_turns` 0、`total_cost_usd` 0），也不调非公开接口。不设 `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC`（设了 `/usage` 不联网，只给一小时内别处留下的旧读数；白名单本来也不传它）。
- 解析结果里的 `Current session: 2% used · resets Oct 11 at 1:59pm (Asia/Shanghai)`、`Current week (all models): 40% used · resets …`、`Current week (Fable): 0% used · resets …` 这样的行，得到各窗口已用百分比和重置时间（重置时间的写法同「用量上限」，换到本机时区显示）。
- 读数存进 `state.json`，缓存 10 分钟；读完之后有窗口过了重置时刻，不到 10 分钟也重读。
- 计入的窗口：会话、本周（all models），以及只限某个模型、而任务用的正是这个模型的周窗口（config 的 `model` 里含括号里的模型名，如 `Current week (Opus)` 对 `opus`）。只限别的模型的（现在的 `Current week (Fable)` 对 `opus` 任务）照样显示，标「不计入」。
- 任一计入的窗口 **≥90%**：tick 不再启动新任务（包括续跑），正在跑的不动；launchd 定点叫醒的夜班、复盘也不启动（汇总记「夜班没有启动：订阅额度快用完…」）。`agentq run`、手动 `agentq night`/`review` 仍可用，只提示一句。
- **≥98%**：`agentq run` 和手动夜班、复盘也拒绝。
- 到重置时刻（再过 1 分钟，防本机时钟快）那个窗口就算已恢复，自动接着领活；同时重读一次拿新读数。没读出重置时间的窗口，要等下次读（10 分钟一次）。
- 读不出（命令失败、超时 60 秒、报错、输出看不懂，比如没有 `Current … % used` 行）当「未知」，照常跑；tick 日志记「额度未知，照常启动：…」，`agentq status` 的「额度」行标「未知（原因），照常接活」。
- `agentq status` 第二行「额度：会话 2%（10-11 13:59 重置）；本周 40%（10-13 20:59 重置）；本周（Fable） 0%（…）（只限 Fable，任务用 opus，不计入）（10-11 09:20 读取）」；挡住时「调度：」一行写「订阅额度快用完（本周 92%，≥90%）…，到 … 重置后自动恢复」。state 里的读数过期了，status 现读一次（只显示，不写回）。
- 当天汇总 `~/.claude/nightly/<日期>-agentq.md`：每天第一次读到、或档位（正常 / ≥90% / ≥98% / 未知）变了，记一行「额度：<各窗口用量和重置时间>」，挡住时带说明，恢复时带「已恢复，照常接活」。早报的 agentq 健康行会带上（见全局 nightly-review 技能）。

## 每个任务的用量

- 任务结束（跑完、失败、中断、超时、被停止都算）时读它的 `result.json`，记下输入、输出、缓存读、缓存写 token 和费用估算，写进 `meta.json` 和 `state.json` 的 `usage`（`input_tokens`、`output_tokens`、`cache_read_tokens`、`cache_write_tokens`、`cost_usd`）。
- 取数：有 `modelUsage` 就把各模型的 `inputTokens`、`outputTokens`、`cacheReadInputTokens`、`cacheCreationInputTokens` 相加（含子代理和不同模型）；没有才用顶层 `usage`（顶层只算主会话，不含子代理，实测有子代理的任务比 `modelUsage` 少一截）。费用取 `total_cost_usd`（按 API 标价估算，订阅并不按它扣钱），没有就用各模型 `costUSD` 之和。结果文件没有这些字段（被杀、崩溃、旧版 claude）就不记，显示时略过。续跑（`claude -r`）的结果里 `modelUsage`、`total_cost_usd` 是整个会话累计的（实测），所以续跑任务记的是减掉上一个有结果的同会话任务之后的差，只算这次进程；有哪一项比上次小（说明不是累计的）就照原样记。中间某次续跑没留下用量（被杀、崩溃），它花的会算到下一次续跑头上，整条链合计不变。
- `agentq status`「最近结束」每个任务后面带「（token 6.75M，约 $4.61）」；token 是四项之和。
- 当天汇总每个任务那一行带「；用量 token 6.75M（输入 122、输出 69.8K、缓存读 6.39M、缓存写 292.3K），约 $4.61」。文件末尾有一段「当天花费」小结（夹在 `<!-- agentq 当天花费：开始… -->` 和 `<!-- agentq 当天花费：结束 -->` 之间），每有任务结束就按 state 里当天结束、记了用量的任务重写：合计、缓存命中率（缓存读 /（缓存读+缓存写+输入））、按费用排的最费 3 个任务；之后写的行都插在它前面。重写是整个文件读出再原子替换，别的进程（保护脚本、任务结束后的 tick、拿锁前就失败的夜班或复盘）恰好同时追加的那一行可能丢。

## 一次满意率和返工率

展示近期首次完成和返工统计，不代替人工验收。只认固定写法，判断不了的不计，`agentq quality` 列出每条的依据。

- 算哪些条目：各项目 `docs/BACKLOG.md`「已完成」里写了「合并提交 <号>」（agent-task 的写法）或「合并，提交 <号>」的 `- [x]` 条目（写了几处取最后一处：需求里顺带写的「修上次合并提交 X 引入的…」不算），合并时间取那个提交在项目仓库里的提交时间，落在最近 7 天里的算。号在仓库里找不到的（别的仓库的号、改写历史前的旧号）不计，`agentq quality` 里列出来；没写合并提交的（只是定了个决定）不算。HTML 注释里的模板不算。
- 一次满意率 = 没有验收意见的 /（有意见 + 没意见）。验收意见 = BACKLOG 任何一节里写着「验收 <原条目标题> 的意见」或「验收「<标题>」的意见」的条目（dispatch 技能的写法）。标题先找完全相同的（不计空白、反引号、引号）；没有再找标题包含它（至少 4 个字）、本身不是验收意见的条目（意见里只写了标题的一部分）。对得上不止一条（比如两条同名）就是「判断不了」，不计；对不上任何条目的只在 `agentq quality` 里列出。正文顺带写的不算：不带引号时「验收」后面必须有空白（「验收：……有意见」「验收后按审查人的意见改」都不认），模板占位 `<…>` 不认。刚合并、主人还没来得及看的也算「没意见」。
- 返工率 = 是返工的 /（是 + 不是）。是返工：① 条目是 7 天内（相对它自己的合并时间）刚合并条目的验收意见；或 ② 条目的需求原文（「合并提交」之前的部分）或它的合并提交说明里引用了 7 天内合并、不是本条自己分支上的提交。本条自己的提交 = 从合并提交沿第一父提交往回走、到第一个「待办：」开头的提交（认领提交）之前的那些；「合并提交」之后的合并、验收说明不看（引用的多是本条自己的提交）。引用的号在仓库里找不到、或验收意见对不上唯一一条，又没有别的返工依据：判断不了，不计。都没有：不是返工。提交号认 7–40 位小写十六进制，要带数字；只有数字的只认 7 位（更长的多是 CI 运行号、日期）。
- 引用即算：需求里引用 7 天内的别的提交，不管是「修 X 引入的」还是「审查 X 时提出」的跟进，都算返工（都是那次改动没一次做到位）。
- 局限：修的是刚合并的问题、但条目里没写提交号也不是验收意见的，算成「不是返工」，dispatch 技能已要求返工的条目写上那次的提交号；没有认领提交的条目（会话里直接改的），往回找「本条自己的提交」会一直走到更早的「待办：」提交（最多 50 个），把别的条目的提交也当成自己的，返工只会少算；7 位纯数字（单号之类）也当提交号去查，查不到就成了判断不了；合并说明后面的验收方法里如果又写了「合并提交 <别的号>」，会被当成本条的合并提交（取最后一处）；每次重算都对各项目「已完成」里所有写了号的条目跑一次 `git show`（本机 4 个项目约 0.5 秒），随 BACKLOG 变长线性变慢。
- 显示：当天汇总末尾的小结里有「一次满意率和返工率」一节（合计一行、每个项目一行），和「当天花费」同在一个自动重写的块里，每有任务结束就按当时的 BACKLOG 和 git 重算；当天没有记了用量的任务时只有这一节。`agentq quality` 第一行末尾带「各项目 BACKLOG 里的验收意见共 N 处」，为 0 时满意率 100% 只说明还没有意见数据。早报的 agentq 健康行照抄 `agentq quality` 第一行（见全局 nightly-review 技能）。

## 连环失败熔断

系统性故障（claude 一启动就报错、登录态坏了、某个全局配置有问题）会让每个任务都秒退，任务结束马上 tick 又会立刻拉起下一条，几分钟就能把所有待做都烧一遍。所以：

- 秒退 = 没做完（失败、中断、启动失败）且运行不到 `breaker_quick`（默认 3 分钟）。运行时长算到 claude 实际退出（result.json 非空时取它的修改时间，claude 只在退出时写它），没有就用收尾时间。被 `agentq stop` 停掉的、撞用量上限的不算，也不清零计数。
- 连续 `breaker_streak`（默认 3）个任务秒退 → 熔断：`breaker_cooldown`（默认 30 分钟）内 tick 不启动任何任务（包括续跑），当天汇总写一行「熔断：…」报警，带最近一个任务的 id 和摘要。`agentq status` 显示「连环失败熔断中」。
- 熔断到期后，只要还没有任务正常结束过，第一个任务再秒退就直接再熔断，时长翻倍，最多 `breaker_max`（默认 4 小时）。
- 有任务跑完、或跑了超过 `breaker_quick` 才结束（包括超时），计数和翻倍都清零；跑完的还会解除正在生效的熔断，汇总记一行「熔断解除：…」。熔断期间才结束的秒退不再计数。
- `agentq run` 不受熔断限制（主人明确要做），只提示一句；它的结果照样计数。修好问题后想马上恢复自动接活，`agentq run` 试跑一条，跑完熔断就解除。

## 中断与自动续跑

claude 正常退出（结果 `is_error` 为假）不一定是做完了：任务里有人用后台命令、Monitor 等通知时，`claude -p` 会在等待中途退出。所以收尾时还要核验：

- 重读该项目 BACKLOG：同一条仍在「待做」且标「（进行中…）」→ 记为**中断**（`interrupted`）。从标记里取分支名，用 git 看它比 main 多几个提交、有没有合并，写进摘要；结果来自后台通知回合（`origin.kind` 为 `task-notification` 或 `result_index` > 0）也写上。
- 条目已不在进行中，但结果里没写合并情况 → 也记中断，但不续跑，等主人核对。
- 中断且有会话 id 的，下一次能启动任务时（遵守暂停、冷却、并发上限和登录检查；先于挑新条目）用同一会话续跑一次：`claude -r <会话id> -p "<续跑提示>"`，其余参数和 `--name` 不变，记成一个新任务。续跑提示写明后台的活已经不在了、从 git 和分支现状接着做、前台等完。续跑前再看一眼 BACKLOG，条目已不在进行中（比如主人手动处理了）就不续跑。
- 续跑的任务再中断就不再续跑，汇总里写「续跑后仍中断，等主人处理」。
- 撞用量上限的任务不做上面的核验；但 BACKLOG 里这一条仍标进行中（做到一半撞上）时同样记中断，冷却过后续跑（续跑的任务撞上限也一样，上限不是 agent 的问题；同一条最多接连续跑 3 次，再撞上限就等主人处理）。
- 汇总里中断的任务记一行「中断：…」，续跑启动时记一行「续跑中：…」，续跑结束再记一行结果。
- 启动参数带 `--disallowed-tools Monitor`，从根上不让任务用 Monitor 等通知（2026-10-10 实测 claude 2.1.295：这个参数对任务开的子代理同样生效）。后台命令（Bash 的 `run_in_background`）没有禁，靠 agent-task 技能的「一切等待前台等完」兜底，漏网的由上面的核验和续跑接住。

摘要按顺序取：结果里最后一个写着合并提交号的段落（只认「合并提交」「提交号」「merge commit」后面紧跟 7 位以上小写十六进制号的写法，「审查结论是可以合并」这类顺带提到合并的段落不算）；没有就取最后一个说到合并情况的段落（prompt 要求最后一段写，但不一定在最后）；再没有就取最后一段。正常跑完的任务，BACKLOG「已完成」里同标题的条目写了「合并提交 <号>」时，把「合并提交 <号>；」放在摘要开头（摘要前 80 个字里已经有这个号就不重复；「待做」里还有同标题的条目，比如标了卡住没合并，就不取，免得带出以前同名条目的旧号），`agentq status`、当天汇总、早报里一眼看出合没合；条目标题被改过就对不上，只按结果文本取。

## 卡死早发现

任务卡住（等一个永远不来的东西、死循环）以前只能等 `timeout`（4 小时）被杀。现在：

- 新任务启动时用 `--session-id <随机 UUID>` 预先指定会话 id（续跑用 `-r`，沿用原会话 id），记进 state.json。
- 每次 tick 看这个会话的记录 `~/.claude/projects/*/<会话id>.jsonl` 和它的子代理记录 `<会话id>/subagents/*.jsonl` 最新修改时间（都没有或比启动早就从启动时算），超过 `stall_after`（默认 45 分钟）没增长就：日志和当天汇总记一行「疑似卡死：任务 …会话记录 N 分钟没增长；不自动杀…」（同一次卡住只记一次，记录又长起来后解除），`agentq status` 在那个任务后面标「疑似卡死」。
- **不自动杀**：前台跑很久的测试、构建也会让记录一阵子不长。真卡住的照旧由 `timeout` 杀；看了日志确认卡住的，主人可以 `agentq stop <任务id>`。
- 旧版本启动、没有预先指定会话 id 的任务不检查。

## 重启后不误杀

state.json 里记的 pid 和进程组号只在这次开机内有效：重启后它们可能已经属于别的程序。

- 每个任务启动时记下开机时间（`sysctl -n kern.boottime` 的秒数）。收尾时和现在的开机时间相差超过 60 秒，且进程已不在 → 机器在任务启动后重启过：**不碰进程组**；claude 重启前已写好结果的照常按结果收尾，否则记**中断**，摘要写「机器重启过…」。BACKLOG 里这一条仍标进行中、有会话 id、本身不是续跑的，用同一会话自动续跑一次（同「中断与自动续跑」）。重启造成的中断不算连环失败。
- 开机时间变了但进程还在（系统校时会让它变），按校时处理，更新记录，照常看着。读不出开机时间时不判断重启，但清进程组仍要核对下面的标记。
- 残留进程：claude 退出后它拉起的子进程还在同一进程组里时，任务外壳等到 claude 退出就立刻对进程组发 SIGTERM（这时组号不可能已被复用），日志在 `launchd.log`。tick 收尾时如果组里还有进程（外壳没了、离退出可能已经很久），只有组里有进程带着这个任务的标记才发 SIGTERM：`ps -E` 看到环境里有 `AGENTQ_JOB=<任务id>`（启动 claude 时设好，它拉起的子进程都会继承）。命令行里的 `--name agentq <项目> ` 只到项目一级，不算。没有标记就只记一行日志不杀。系统自带命令（`/bin/zsh`、`/bin/sleep` 等）`ps` 看不到环境变量，组里只剩它们时认不出，宁可不杀。

## 状态文件损坏

- `~/.agentq/state.json` **内容坏了**（JSON 不完整、有空记录）时，tick 先把它原样备份成 `state.json.bad-<年月日-时分秒>`，再按 `~/.agentq/jobs/*/meta.json` 重建任务列表：运行中的任务照常收尾、照常按启动时间做超时（进程在不在仍用 `ps` 核对命令行里的 `agentq <项目> ` 标记），日志和当天汇总记一行「状态文件损坏：…已备份为 …，按 N 个任务的 meta.json 重建…」。读不出的 meta.json 跳过并在那一行里列出。重建那次 tick 只收尾不启动新任务，下一次照常接活。
- 重建丢掉的：连环失败熔断的截止时间和计数；用量冷却只能按撞上限任务的结束时间 + `limit_cooldown` 估回来（按提示的重置时间冷却的会变短）。
- **读不了**（没权限、I/O 错误，内容可能是好的）仍当作「未知」：不收尾、不启动、不备份，只记日志。`agentq run`/`stop` 遇到坏文件也只报错，等 tick 重建。
- 启动任务后写 state.json 失败：状态里没有这个任务，以后的 tick 不会收尾它、超时也不会杀它，所以马上对它的进程组发 SIGTERM（10 秒后还在就 SIGKILL），任务记「失败」，摘要写「启动后写状态文件失败，已杀掉刚启动的 claude…」，进当天汇总（meta.json 里有记录）。

## 传给 claude 的环境变量（白名单）

任务的 claude、任务外壳和 `claude auth status` 只拿到这些环境变量，其余一律不传：

- `PATH`、`HOME`、`USER`、`LOGNAME`、`LANG`、`LC_*`、`TMPDIR`、`SHELL`、`SSH_AUTH_SOCK`；
- `CLAUDE_CONFIG_DIR`、`CLAUDE_CODE_OAUTH_TOKEN`（主人有意设置的登录/配置）；
- 代理 `HTTP_PROXY`、`HTTPS_PROXY`、`NO_PROXY`、`ALL_PROXY`（及小写），在终端里 `agentq run` 时联网可能要用；
- agentq 自己的 `AGENTQ_*`（如 `AGENTQ_JOB`）。

所以 `ANTHROPIC_API_KEY`、`ANTHROPIC_BASE_URL` 等 `ANTHROPIC_*` 不会让任务改走 API 计费或别的账号，桌面端 Claude 会话注入的 `CLAUDECODE`、`CLAUDE_CODE_*` 也不会让任务以为自己是子会话。任务要用别的变量（比如某个服务的令牌），在项目里自己配置，不靠 agentq 传。

## 任务结束立刻接下一条

每个任务都经一个外壳启动：`agentq _job <任务目录> <项目目录> <claude> <参数…>`（内部命令，不用手动执行）。外壳在自己的进程组里启动 claude，等它结束后换成（exec）`agentq tick --after <任务id>`：马上收尾这个任务、按上面的规则挑下一条启动，所以排队的待做几秒内就接上，不用等下一次定时 tick。launchd 每 5 分钟的 tick 照旧保留作兜底（外壳被杀、tick 失败时由它收尾和接活）。

- 记在 state.json 里的 pid 仍是 claude 本身，claude 仍在自己的进程组里，收尾、超时、`stop` 的判断和以前一样。`ps` 里会多看到一个 `agentq _job …` 进程，它是这个任务的外壳，任务结束后自己退出。
- 结束后的那次 tick 和定时 tick 共用 `tick.lock`，不会重叠；锁被占着时它最多等 60 秒（定时 tick 是直接跳过），免得正在跑的那次 tick 刚好没看到这个任务结束、空出的位置又要等 5 分钟。
- 它和定时 tick 一样遵守暂停、冷却、并发上限和 retry_after；日志也写进 `~/.agentq/launchd.log`，开头是「tick（任务 <id> 结束后）：」。
- 被 `agentq stop` 停掉或超时杀掉的任务也一样：外壳马上 tick，下一条待做立刻启动（被停的那条受 retry_after 限制，不会马上重来）。想停住不接新活，先 `agentq pause`。
- 每条待做的重试间隔仍由 retry_after 管，所以不会连环重启同一条；但如果把 retry_after 配得很短，又遇上 claude 一启动就失败，就会以秒级反复拉起（以前最多 5 分钟一次）。
- 关掉：config.json 里 `"tick_after_job": false`，之后结束的任务就只靠定时 tick 收尾和接活。

## 夜班和复盘

夜班和复盘由显式选择的 agentq 服务调用；已有桌面端定时任务不会自动迁移或被安装器改写。两套入口应由用户明确选择，避免重复执行：

- `agentq night`、`agentq review` 各用 `claude -p` 执行一份随仓库提供的说明：`prompts/night.md`、`prompts/review.md`（轻量入口，review 委托唯一来源 skills/nightly-review，night 保持本机调度开关；不从桌面任务反向覆盖源码）。前面加一段固定的前言：`-p` 回合一结束进程就退出，所以一切等待都用前台 Bash 的 `until` 循环做（实测 `-p` 下前台单独 `sleep` 很久会被拦，`until … do sleep 30; done` 可以），不用后台命令、Monitor 或「等通知」。
- 说明文件编译进二进制：改了 `prompts/*.md` 要合并进 main 后执行 `scripts/update.sh` 才生效。
- 启动方式和待做任务一样：同样的 `--model`/`--effort`、本机配置的 `permission_mode`、禁用 Monitor、环境变量白名单、任务外壳；工作目录是 `projects_root`。任务记录在 `~/.agentq/jobs/<id>/`（id 形如 `1011-010000-夜班`），结束时当天汇总记一行「夜班「agentq night」跑完：…」，`agentq logs <id>` 看结果全文。
- 超时单独配：`night_timeout` 默认 6 小时（夜班要等到 05:00 才收工，从 01:00 起算要能跑满 5 小时），`review_timeout` 默认 4 小时（复盘最多等夜班报告到 08:00）。到时间由 tick 杀掉整个进程组，记「超时」。
- 不占 `max_parallel`/`per_project` 的名额（夜班等的就是这些任务做完），`agentq status` 把它们单列成「定点任务在跑：夜班，任务 …」，不算进「运行中（n/m）」。同一种已经在跑时不重复启动。
- 收尾：没有 BACKLOG 条目，不做待做那套中断核验，也不续跑；但正常退出却没写出说明要求的报告（夜班 `~/.claude/nightly/<日期>-night.md`，复盘 `<日期>.md` 早报，启动那天或收尾那天的都算）时记「中断」，摘要写「正常退出但没写出报告…」，多半是中途退出了。
- 启动前不看 `paused`（但夜班 prompt 不会执行 resume，保持自动调度暂停状态）和连环失败熔断；用量冷却中、终端版 Claude 没登录时不启动，定点叫醒时还在当天汇总记一行「夜班没有启动：…」，早报能看到。它们秒退照样计入连环失败，但跑完不会清零计数、不会解除熔断。
- 已知限制：`agentq night` 里**用不了 computer-use**，不能点 Xcode Organizer 上传 TestFlight。上传要走不开窗口的 App Store Connect API 脚本（示例记账App、示例阅读App的发版技能都已把 `Tools/upload-api.sh` 写成首选，这条路不受影响）。
  - 官方文档（[computer use in the CLI](https://code.claude.com/docs/en/computer-use)）原文：「It requires an interactive session, so it is not available in non-interactive mode with the `-p` flag.」
  - 实测（2026-10-10，claude 2.1.295）：用一次性 launchd 服务（PATH 同夜班 plist，`env -i` 只留白名单变量）照 agentq 的参数（`--session-id`、`-p`、`--model opus --effort high`、`bypassPermissions`、`--output-format json`、`--disallowed-tools Monitor`、`--name agentq …`）各起一次 `claude -p`，让它找 computer-use 工具并截屏。一次的工作目录是 `~/projects`（夜班的工作目录），一次在主人唯一在 `/mcp` 里打开了 `computer-use` 的目录（`~/.claude.json` 里 `enabledMcpServers: ["computer-use"]`）。两次结论相同：用 ToolSearch 按名字要求含 `computer`、`screenshot` 搜都返回 `No matching deferred tools found`（原文），回答「没有 computer-use 工具，所以没截屏」，最后一行 `CU=没有`，stderr 为空。当时没锁屏。
  - 锁屏：computer-use 本来就没有，锁不锁屏都一样。API 脚本在锁屏时能传（推断）：示例阅读App 1.0.3 (6) 由桌面端夜班用 `Tools/upload-api.sh` 上传（05:05 升版本号，05:28 记录 VALID，输出 `UPLOAD SUCCEEDED`），`pmset -g log` 显示屏幕从 01:58:15 关掉、到 09:42 才再亮，`sysadminctl -screenLock status` 是关屏 900 秒后要密码，按此推算上传时已锁屏（pmset 不直接记锁屏）。登录钥匙串设置是 `no-timeout`，不会自己锁上。由 agentq 任务（`claude -p`）跑这个脚本也传成功过：任务 1010-011438-my-reader（不是 launchd 定时 tick 拉起的，当时 `launchd.log` 没有启动记录）在 01:16 升版本号、01:46 记录 VALID 之间传了 1.0.3 (5)；屏幕 01:28–01:58 亮着，01:12–01:28 关着，推断上传时没锁屏。launchd 拉起、并且锁着屏的 `claude -p` 跑这个脚本，还没在真实上传里试过。
  - 所以夜班遇到「技能里只写了 Organizer 上传」的项目，或 API 脚本失败后想退回 Organizer，都做不了，会按说明记进夜班报告（「上传失败…记原因」），早报能看到。
- `agentq setup --services --yes` 装两个 launchd 定点任务：`io.agentq.agentq.night` 每天 01:00、`io.agentq.agentq.review` 每天 06:30（`StartCalendarInterval`），执行 `~/.local/bin/agentq night|review --scheduled`，PATH 和日志（`~/.agentq/launchd.log`）同 tick。装上时不执行。机器在那个时刻睡着的，launchd 在醒来后补跑一次。
- 开机后补跑：机器在那个时刻关着、断电或还没登录（launchd 的用户服务登录后才启动），launchd 不会补叫。tick 每次看一眼：开关打开的夜班或复盘今天的定点时刻已过 15 分钟，今天既没有这一种的任务记录（手动跑的、失败的都算），也没有它要写的报告（`<日期>-night.md` / `<日期>.md`，别处跑的也算），并且还在窗口内（夜班 08:00 前，复盘当天任何时候），就补跑一次，当天汇总记「夜班开机后补跑：今天 01:00 那次没跑…」，任务标题带「（开机后补跑）」。补跑的任务今天有了记录，同一天不会再补。同一种还在跑（比如前一天深夜手动跑、过了 0 点还没完）不补；夜班还在跑时先不补复盘（复盘要汇总夜班报告）。补跑的夜班 prompt 前多一句：说明里等待的截止「早上 05:00」改成今天 08:00。和定点叫醒一样不看暂停和熔断，遵守用量冷却、登录和额度；没能启动只在 `launchd.log` 记一行，下次 tick 再试。配合断电来电自动开机（`pmset autorestart 1`）和自动登录（要关掉 FileVault），停电后能无人恢复。
- 开关 `night_enabled`、`review_enabled` 默认 false：定点叫醒时只在 `launchd.log` 记一行「夜班：night_enabled=false，launchd 定点叫醒但不执行…」，什么都不做。`agentq status` 第二行显示两个开关。

从桌面端切换到 agentq（两边都开着会跑两次夜班）：

1. 确认装好了：`agentq setup --services --yes` 之后 `launchctl print gui/$(id -u)/io.agentq.agentq.night`、`….review` 都能看到。
2. 先在 Claude 桌面端停用两个定时任务 `night-shift`、`nightly-review`（设为不启用，不用删）。
3. 再编辑 `~/.agentq/config.json`，加上或改成 `"night_enabled": true`、`"review_enabled": true`（只改这两个键），用 `agentq status` 确认第二行显示「夜班 01:00 开」「复盘 06:30 开」。不用重新 install，开关每次叫醒时现读。
4. 第二天早上看 `~/.claude/nightly/<日期>-night.md`、早报和 `<日期>-agentq.md` 里的「夜班「agentq night」…」「复盘「agentq review」…」两行。

切回桌面端：反过来，先把两个开关改回 false，再启用桌面端的两个定时任务。

## 巡逻

agentq 主动发现问题，不等主人问（愿景「主动开口」），agentq 和项目自己的新问题（任务失败、卡住）推给主人（见「推送和 Telegram」）；基础设施类（launchd 服务退出码和监听没在跑、自动备份推送、当天备份报告、早报、磁盘）照常检查、写进 patrol.json 和当天汇总，但不推，这些由看门狗（`~/.claude/scripts/watchdog.py`，不依赖 agentq，agentq 挂了也能报）推，免得同一件事推两遍；看门狗查不到的（定点服务、监听服务没装上，缺夜班报告）巡逻照推。tick 离上次巡逻满 1 小时就巡逻一次（`patrol_enabled`，默认开），`agentq patrol` 立刻巡逻一次。巡逻本身是纯脚本，不调用 claude、不花额度，查这几类：

| 检查 | 算问题的条件 | 证据 |
| --- | --- | --- |
| launchd 服务 | `launchctl list` 里和 agentq 自己的服务同一前缀（服务名去掉最后一段）的服务上次退出码不是 0；agentq 装着、`night_enabled`/`review_enabled` 打开，夜班/复盘的定点服务却不在列表里；配了 Telegram，监听服务 `….listen` 不在列表里或没有 pid | 那一行的退出码（负数是被信号终止）、pid，服务 plist 里日志的末尾 3 行 |
| 自动备份推送 | `~/.claude/state/autopush.log` 最近 24 小时有 `FAIL` 行（只认「日期 时间 FAIL 仓库: 报错」这个固定格式）、之后同仓库还没有 `ok` 的，每个仓库报最近一段连续失败（中间没有 `ok`，起点按这一段真正的第一次 FAIL 算，不受 24 小时限制，连着失败几天也只报一次）；之后同仓库推成功了算已恢复、不报，推成功过之后又 FAIL 算新问题 | 这一段从几点起、几次、最后一行原文 |
| 当天备份报告 | `~/.claude/nightly/<日期>-backup.md` 第一行有备份脚本写的「**N 个失败**」 | 第一行和「**推送失败**」那几行 |
| agentq 任务 | 最近 24 小时失败、超时、中断的任务（撞用量上限的、等着自动续跑或已续跑的不算）；运行中、会话记录超过 `stall_after` 没增长的 | 任务 id、起止时间、摘要；卡住的写没增长多久 |
| 订阅额度 | state 里缓存的读数（tick 领活前读的，巡逻不另读）有计入的窗口 ≥90% | 各窗口用量、重置时间、读取时间 |
| 早报、夜班报告 | 08:00 后，开关打开的复盘还没写 `<日期>.md`、夜班还没写 `<日期>-night.md`（正在跑的不算） | 今天那次任务的状态和摘要，或「今天没有记录」 |
| 等主人拍板 | 各项目 BACKLOG「等主人拍板」里的 `- [ ]` 条目（带等条件标记的不算，见「挑活规则」）；报过的条目后来加上标记，悄悄去掉、不写「已处理」 | 条目原文 |
| 磁盘 | HOME 所在磁盘剩余不到 20 GB | 剩余和总量 |

- 每个问题带证据写进 `~/.agentq/patrol.json`（现有问题、首次发现时间、处理说明）和当天汇总（一行「巡逻发现：标题；证据：…；处理：…」）。疑似卡住和额度 tick 发现时已经各写过一行（「疑似卡死：…」「额度：…」），巡逻只记进 patrol.json、不重复写汇总。
- 不重复报：每个问题有指纹（如 `launchd:<服务>`、`autopush:<仓库>@<这一段第一次 FAIL 的时间>`、`job:<任务id>`、`decide:<项目>:<标题哈希>`、`report:night:<日期>`）。问题还在，之后的巡逻只更新最后看到的时间，不再写汇总；再出现就重新报。不在了分几种：真的好了（服务还在列表里、退出码回到 0，推送失败之后同仓库推成功了，当天的报告文件有了，磁盘清出来了，卡住的任务又在长，额度各窗口都回到 90% 以下）记「已恢复」，拍板条目不在那一节了记「已处理」，各汇总一行；补跑的复盘/夜班还没写出报告时照旧记着；只是过了 24 小时窗口的任务失败和推送失败、前一天的报告、卡住后结束了的任务、从 launchd 里卸掉的服务，悄悄从 patrol.json 去掉，不写「已恢复」。某类检查这次没做成（比如读不了 launchd 列表），那一类上次的问题照旧记着。
- 自动处理：失败、超时、中断的待做任务，BACKLOG 里那一条还标着进行中、之后也没有别的任务在接手时，在那个项目 BACKLOG「待做」末尾写一条「接手中断的「标题」」并只提交这个文件，agentq 会像别的待做一样接着做：看分支现状做到合并，或给原条目标卡住。写之前按 `with-lock.sh` 的约定占 `<项目>-main` 锁（`~/.claude/state/locks/`，最多等 2 分钟），主目录不在 main、BACKLOG 有没提交的改动、暂停着（`paused=true`）时不写，处理说明里写原因；接手的待做自己又没做完时也不再写（只报，等主人），免得一层套一层。其余问题没有稳妥的自动修法，处理说明写主人要做什么（如「执行 agentq install 重新装上」「agentq stop <id>」）。
- 分析：有新的异常（拍板条目、额度不算）且 `patrol_analyze` 开着（默认开）时，启动一次 `claude -p` 只读分析原因（每天最多 3 次；暂停、冷却、熔断、额度 ≥90%、上一次分析还在跑时不启动）。它和夜班一样记成任务（id 形如 `1011-120000-巡逻分析`，超时 30 分钟，不占并发名额），结束时当天汇总记一行「巡逻分析「agentq patrol 分析」跑完：<结论>」。
- `agentq status` 在「定点任务」下一行显示「巡逻：上次 <时间>，现有 N 个问题（agentq patrol 看证据）」。

## 推送和 Telegram

agentq 有事主动找主人（愿景「主动开口」），主人在手机上回一句就能派活、拍板、问状态。`notify_enabled`（默认开）管这一整块。

**通道**：配了 Telegram 机器人就推到 Telegram，没配就弹 macOS 本机通知（`osascript`），双向聊天只在 Telegram 上有。凭据只从 `~/.agentq/secrets/telegram.env` 读（两行 `TELEGRAM_BOT_TOKEN=…`、`TELEGRAM_CHAT_ID=…`，权限必须是 600，否则不用、`agentq status` 的「推送」一行报原因），agentq 不打印它，日志和报错里出现的 token 一律打码成 `***`。只给这个 chat id 发消息，也只认它发来的消息（别的 chat 发来的只在日志记一行「忽略 chat id …」，不回、不处理）。`agentq notify setup` 配机器人（有 `~/.agentq/secrets/telegram-setup.sh` 就执行它，没有就打印手动配置的四步），`agentq notify test` 发一条测试消息。Telegram 发不出去时消息留在队列里下次 tick 再发，并先弹一次本机通知。

**推什么**（都在 tick 末尾发，`agentq notify send` 记完马上发）：

| 类别 | 什么时候 | 谁记 |
| --- | --- | --- |
| ☀️ 早报 | 每天一次：当天早报 `~/.claude/nightly/<日期>.md` 写完（2 分钟没再改、复盘任务没在跑）且不早于 08:00。要你定的、出问题的（巡逻现有的问题，基础设施类除外）放最前，再写 24 小时内完成的、任务成败和额度，最后是早报第一行。推到 Telegram 成功后写 `~/.claude/state/report-pushed-<日期>`，会话里的早报转告看到它就不再转告；已经有这个标记就不推 | tick |
| 🔴 要你定 | 各项目「等主人拍板」出现新条目（tick 每 5 分钟看一遍；第一次运行只记下现有的、不推；带等条件标记的不推，标记去掉后才推）。主人回复这条消息说选哪个，就按决定写回条目并派出 | tick |
| 🟠 巡逻发现 | 巡逻新发现、没自动处理的问题：任务失败、超时、中断、疑似卡住，服务没装上，缺夜班报告（写了接手待做的、已有任务在接手的不推；拍板、额度各有自己的推送；基础设施类归看门狗推，见「巡逻」） | 巡逻 |
| 🔴 要你知道 | 订阅额度到 90%（每天第一次和档位变化时）、连环失败熔断 | tick |
| 📦 新版可装 / 🟢 完成 | 新 TestFlight 可装、新项目第一版做好（附怎么看） | 夜班、agent 用 `agentq notify send release|done …` |

不推：每个任务完成、巡逻没发现问题、自动重试成功、教训和规则变化——这些进当天汇总和早报。

**规则**：
- 同类 30 分钟内只发一条：第一条马上发，之后的攒着，到点合成一条发。队列里同一件事（同一个 key）只留最新的说法。
- 23:00–08:00 只攒着，08:00 后发；那时早报已经写完就合进早报一起发，否则各类各发一条。
- 每天最多 10 条。早报、「要你定」不受上限挡（都算条数）；超出上限的普通消息不丢，跟着下一条发出的消息（要你定、安全问题）一起带上，没有就等早报或第二天。
- 安全问题（`agentq notify send --security` 标的，如以后加的泄露检查；备份失败归看门狗推，见「巡逻」）不等合并窗口、不管夜间和上限，当时就推。

**格式**（HTML parse_mode）：

```
🟠 <b>巡逻发现 · 10月11日</b>

🟠 自动备份推送失败：my-app
~/.claude/state/autopush.log 里 10-11 12:21 起连续 2 次 FAIL …
没自动处理：每天 05:30 的定时备份会再推一次 …

🟠 磁盘剩余不到 20 GB
<i>12:30 · agentq</i>
```

消息里的时间一律由程序写成「10-13 周二 21:00」（星期是算好的），对话任务、夜班和复盘的 prompt 也要求模型照抄、要别的日期的星期用 `date` 查，不自己换算。标题行 `<表情> <b>类别 · 日期</b>`，每段开头一个表情当标记、段内不再加，项目名后用全角空格对齐，要主人做的放最前，最后一行 `<i>时间 · 来源</i>`，不用感叹号、不客套。不算空行超过 12 行时只发前面的摘要，最后一行写「其余 N 行见 ~/.agentq/notify/<时间>-<类别>.txt」。动态文字一律转义；Telegram 说 HTML 解析不了（400 can't parse entities）时去掉标签按纯文本重发。

**在 Telegram 里说话**（自然语言，不用固定命令）：常驻进程 `agentq listen`（launchd 服务 `io.agentq.agentq.listen`，`KeepAlive`，挂了 launchd 10 秒内拉起）用 `getUpdates` 长轮询（timeout 50 秒）收消息，主人一发它就收到。offset 和收到的消息一起存进 `notify.json`，同一条不会处理两次，重启后接着上次的。它拿着 `~/.agentq/listen.lock` 期间 tick 不收消息（两边不会同时 `getUpdates`，Telegram 也不允许）；它没在跑（没装、挂了还没拉起）时 tick 每 5 分钟短轮询一次兜底。换了新版本（`scripts/update.sh` 替换了 `~/.local/bin/agentq`）它会在一轮长轮询内发现、自己退出，launchd 用新版本拉起。每轮重读配置和凭据，改 `notify_enabled`、换 token 不用重启；没配 Telegram 时只歇着（每分钟看一次），不反复退出。
- 整条消息就是「状态」（或 `status`、`/status`、`/start`）：当场回一条 📊 状态摘要（要你定、调度、额度、在做、待做、最近结束），不起 claude、不花额度，几秒内到。
- 其他话：先回一句「收到，交给 claude 了，答完回你。」（上一个对话还在答时是「收到，前面还有 N 条在答，答完就轮到这条。」；冷却中、额度 ≥98% 时不说收到，直接回原因），再交给一次 `claude -p`（带主人的全局规则和技能，在 `projects_root` 里跑）理解意图：派活（「my-app：加个按月对比」→ 按 dispatch 写 BACKLOG 并 `agentq run`）、拍板（回复某条「要你定」→ 写回条目、移回「待做」并派出）、问状态或问问题（查完简短回答）、小事当场办（合理又能撤回的直接做，必须等主人的那几类不做）。它的最终回答按上面的格式原样回发，挂在主人最后一条消息下面；没做成就回「这条没答上来」和原因。回复不进推送队列，不受合并、夜间和每天上限限制；当时没发出去的存进 notify.json，之后每次 tick 补发（最多 24 小时）。
- 对话任务和巡逻分析一样记成任务（id 形如 `1011-120000-Telegram对话`，超时 20 分钟，不占并发名额，进当天汇总，巡逻不把它的失败当问题报）。同一时间只跑一个，期间来的消息排队，上一个结束时（任务外壳触发的那次 tick）一起交给下一个。不看暂停和熔断（主人亲口问的，同 `agentq run`）；用量冷却中、额度 ≥98%、没登录时不启动，直接回原因。
- 收图片、录屏、文件（Telegram 只让机器人下载 20 MB 以内的）：截图、相册、视频、文件用 `getFile` 下载到 `~/.agentq/inbox/<消息号>-<文件名>`，交给 claude 时挪进那次对话任务目录 `~/.agentq/jobs/<id>/files/`，路径写进 prompt：图片它直接看，视频抽一帧看；派活时把路径原样写进 BACKLOG 条目（「这里怎么歪了」配截图就能派活）。超过 20 MB 的回一句「没收到，压缩一下再发或告诉我路径」；语音回一句「听不了，打字发我」；相册的几张图只回一次「收到」。
- 发图片、录屏：对话里 claude 执行 `agentq notify file <路径> [说明]` 马上发出（挂在主人的消息下面）；夜班和 agent 的推送用 `agentq notify send --file=<路径> …`，附件跟在那条消息后面发（没配 Telegram 时本机通知里写附件路径）。图片（png、jpg、webp，10 MB 内）用 `sendPhoto`；Telegram 说图片参数不对（如长截图宽高比超过 20）时改按文件发；录屏（mp4、mov、m4v）用 `sendVideo`，其余用 `sendDocument`，都在 50 MB 内。录屏超过 50 MB 先压缩（有 `ffmpeg` 用它，没有用系统自带的 `/usr/bin/avconvert`，依次试 1280x720、960x540、640x480，结果存 `~/.agentq/notify/media/`），压不下或别的文件超过 50 MB 就改发一句带文件路径的话。`notify send --file` 在调用的那一刻检查和压缩，不让 tick 拿着锁去压。上限出处：core.telegram.org/bots/api「Sending files」「getFile」。
- 已知限制：设了 webhook 的机器人收不到消息（Telegram 规定两者只能选一个，长轮询会报 409，`listen` 每分钟重试一次并记日志）；贴纸等别的消息回一句「只看得懂文字、图片、视频和文件」。

`agentq status` 多一行「推送：Telegram，今天已推 N/10 条，攒着 M 条」，配了 Telegram 时再多一行「监听：在跑（pid N）…」或「没在跑，tick 每 5 分钟兜底收一次消息…」；`agentq notify status` 列出攒着的消息、排队等回答的消息数和最近的错误。

## 配置 `~/.agentq/config.json`

以下是旧运行配置兼容默认值，缺键沿用。正式安装为新配置显式覆盖 paused=true、night/review/patrol/notify=false、permission_mode=default；既有文件逐字节保留。

| 键 | 默认 | 说明 |
| --- | --- | --- |
| `projects_root` | `~/projects` | 扫这个目录下一层 |
| `claude_path` | `claude` | 完整安装检测后，新配置写独立 CLI 绝对路径 |
| `permission_mode` | 旧配置兼容 bypass；新安装 default | 显式用户策略，不由安装继承作者权限 |
| `model` / `effort` | `opus` / `high` | 传给 `--model` / `--effort` |
| `max_parallel` | 2 | 全局同时运行的任务数 |
| `per_project` | 1 | 同一项目同时运行的任务数 |
| `timeout` | `4h` | 超时杀掉整个进程组 |
| `retry_after` | `12h` | 同一条目再试的间隔 |
| `limit_cooldown` | `1h` | 撞用量上限后暂停启动的时长 |
| `paused` | false | 为真时 tick 只收尾不启动 |
| `tick_after_job` | true | 任务结束时马上 tick 一次接下一条；为假时只靠 launchd 的定时 tick |
| `breaker_streak` | 3 | 连续这么多个任务秒退就熔断 |
| `breaker_quick` | `3m` | 运行不到这么久就结束且没做完，算秒退 |
| `breaker_cooldown` | `30m` | 第一次熔断暂停启动的时长 |
| `breaker_max` | `4h` | 熔断后再秒退时长翻倍的上限 |
| `stall_after` | `45m` | 运行中的任务会话记录这么久没增长，标「疑似卡死」（只提醒，不杀） |
| `night_enabled` / `review_enabled` | false / false | launchd 每天 01:00 / 06:30 叫醒时是否执行夜班 / 复盘（见「夜班和复盘」） |
| `night_timeout` / `review_timeout` | `6h` / `4h` | 夜班 / 复盘的超时（待做任务用 `timeout`） |
| `patrol_enabled` | true | tick 是否每小时巡逻一次（见「巡逻」；`agentq patrol` 不看它） |
| `patrol_analyze` | true | 巡逻有新异常时是否启动一次 `claude -p` 分析原因（每天最多 3 次） |
| `notify_enabled` | true | 是否推送和收 Telegram 消息（见「推送和 Telegram」；没配 Telegram 时只弹本机通知） |

## 文件

- `~/.agentq/state.json`：任务列表、用量冷却和熔断的截止时间、连续秒退计数、最近一次读到的订阅额度。内容坏了时备份成 `state.json.bad-<时间>` 并按 meta.json 重建，读不了时当作「未知」（见「状态文件损坏」）。
- `~/.agentq/jobs/<id>/`：`prompt.txt`、`result.json`（claude 的 JSON 输出）、`stderr.log`、`meta.json`（任务记录，结束后带用量，见「每个任务的用量」）。
- `~/.agentq/launchd.log`：launchd 下每次 tick 的日志（包括保护脚本记的失败和回滚）、夜班和复盘定点叫醒的日志，以及任务外壳和任务结束后那次 tick 的日志。
- `~/Library/LaunchAgents/io.agentq.agentq{,.night,.review,.listen}.plist`：`agentq install` 写的四个 launchd 服务。
- `~/.agentq/listen.lock`、`~/.agentq/listen.pid`：常驻监听拿着的锁（tick 据此知道它在不在跑、不和它抢着收消息）和它的 pid。`~/.agentq/inbox/`：主人发来、还没交给 claude 的附件。
- `~/.agentq/tick-guard.sh`、`~/.agentq/guard-fails`：launchd 调用的保护脚本和它记的连续失败次数（见「自动回滚」）。
- `~/.local/bin/agentq.prev`、`agentq.bad`：`scripts/update.sh` 留下的上一个版本；自动回滚时换下来的出错版本。
- `~/.agentq/patrol.json`：巡逻的现有问题（指纹、证据、处理、首次和最后发现时间）、上次巡逻时间、当天分析次数。内容坏了时备份成 `patrol.json.bad-<时间>` 从空的开始（报过的问题会再报一次）。
- `~/.agentq/notify.json`：推送队列（攒着的消息）、各类上次发送时间、今天推了几条、哪天推过早报、见过的拍板条目、Telegram 的 offset、排队等回答的主人消息、没发出去待补发的对话回答、最近的推送错误（不含 token）。内容坏了时备份成 `notify.json.bad-<时间>` 从空的开始。`~/.agentq/notify.lock` 防止 tick、`notify send` 同时改它。
- `~/.agentq/notify/`：超过 12 行的推送和太长的对话回答的全文。
- `~/.agentq/secrets/telegram.env`：Telegram 凭据（权限 600，agentq 只读不写）。
- `~/.claude/state/report-pushed-<日期>`：早报已推到 Telegram 的标记。
- `~/.agentq/tick.lock`：防止两个 tick 重叠（定时 tick、任务结束后的 tick、`run`、`stop` 共用）。
- `~/.claude/nightly/<日期>-agentq.md`：每个结束的任务一行（续跑启动时也记一行，订阅额度每天第一次读到和档位变化时记一行「额度：…」，熔断时记一行「熔断：…」，疑似卡死时记一行「疑似卡死：…」，状态文件损坏重建时记一行「状态文件损坏：…」，保护脚本回滚时记一行「已自动回滚：…」，巡逻发现新问题时每个一行「巡逻发现：…」、问题消失时一行「巡逻：已恢复…」，夜班、复盘结束时各一行、定点叫醒却没能启动时记一行「夜班没有启动：…」），末尾是「当天花费」小结，每晚复盘会读。

## 开发

```bash
test -z "$(gofmt -l .)" && go vet ./... && go test ./...
```

测试用临时 HOME、临时 projects_root 和假的 claude 脚本；`install` 的测试用假 launchctl；保护脚本和 `scripts/update.sh` 的测试用临时 HOME 里的假 agentq 和假源码目录。测试不读写真实的 `~/.agentq`、`~/projects`、`~/.claude`，也不碰 launchd。

## 清洁源与配置备份

公开源码的隐私边界与扫描操作见 [docs/privacy.md](privacy.md)，配置安全投影/回滚见 [docs/config-privacy.md](config-privacy.md)。`launchd_label` 默认 `io.agentq.agentq`；已有安装在本机 `~/.agentq/config.json` 保留原服务标识，不因源码脱敏重装服务。
