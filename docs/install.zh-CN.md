# 安装完整 Claude 工作系统

[English](install.en.md) · [组件清单](components.md) · [完整参考](reference.zh-CN.md) · [给 Agent 的入口](../llms.txt)

## 推荐安装：v0.1.0 Release

先把 Release 中的安装脚本下载到临时文件，再执行：

```sh
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
curl -fL https://github.com/dodo258/agentq-public/releases/download/v0.1.0/install.sh -o "$tmp"
sh "$tmp"
```

`install.sh` 检查 macOS 架构以及 `curl`、`python3`、`git`、`shasum` 是否存在，下载同版本压缩包和 `SHA256SUMS`，校验后在临时目录调用包内的 `agentq setup --yes "$@"`。它不直接用管道执行远程脚本，不要求 Go 或 Node，也不会静默安装 Git、Python 3 或 Claude Code CLI。

如需从源码安装：

```sh
git clone https://github.com/dodo258/agentq-public.git
cd agentq-public
sh scripts/bootstrap.sh --yes
```

源码入口会构建临时候选，再调用同一个 `agentq setup` 安装引擎。源码构建需要 `go.mod` 指定的 Go 版本。两个入口都会安装程序、完整技能目录、支持脚本、五模板及固定第三方依赖；不复制作者的 `.claude`，不登录，不索要 token。

给 Agent 的一句话：**请阅读本仓 `llms.txt` 和安装指南，把 v0.1.0 Release 的 `install.sh` 下载到临时文件后执行；保留我的配置，默认暂停，任何服务、权限或认证变更都按我的授权处理。** Release 与源码入口调用同一个安装引擎。

## 安装前检测

- macOS 13+，arm64 或 amd64。当前验收主机为 macOS arm64；amd64 构建可交叉检查，但尚未在 Intel Mac 实机验收。
- Git、Python 3。Waza 的部分检查脚本需要 Python。Release 安装与运行调度器不需要 Go；只有源码安装需要 `go.mod` 指定的 Go 版本。安装器检测缺项后给出处，不静默安装系统软件。
- 可独立调用的 Claude Code CLI：检测真实路径、`--version`、print/JSON/session/effort 能力和 CLI 任务环境中的 `auth status`。可用 `--claude /absolute/path/to/claude` 明确选择，路径可含空格。CLI-only 可用，不需要 Desktop。已有配置中的命令名（如 `claude`）按 PATH 解析；已有 `claude_path` 与 `--claude` 不同时拒绝安装，先由安装者明确修改自己的配置，不会只探测新引擎却继续运行旧引擎。
- 未找到 CLI 时提示官方稳定安装方式 `curl -fsSL https://claude.ai/install.sh | bash -s stable` 与 `claude auth login`。安装者决定是否执行；安装器不登录、不索要 token，不读取 Desktop 私有 bundle 路径。检测到 Desktop、上传技能 ZIP 或打开桌面会话均不算后台引擎可用。
- 选择 `--services` 时，再按 plist 声明的 HOME/PATH 做服务环境探测，不继承安装 shell 的 token 或代理变量。只依赖临时 shell 身份时拒绝服务安装；安装者需明确配置持久的 CLI 认证/网络设置。安装器不把秘密写进 plist、不代登录。
- 认证检测只证明 CLI 在该环境识别身份，不证明远端模型请求必然成功；真实模型验收单独执行。用户自己的受支持网关/模型映射属于本机配置，安装器不迁移或切换收费入口。父进程 API 环境变量不自动传入任务，详见 README 白名单。
- `settings.json`/全局 local 设置只校验 JSON 和常用结构，绝不重写；更全面的宿主诊断可用官方 `claude doctor`。当前安装投影使用默认 `$HOME/.claude`；自定义 `CLAUDE_CONFIG_DIR` 不会被自动迁移。

只有缺项时才按提示补齐前置工具，再重复同一条安装命令；不用手工拼装技能、Hook、脚本或服务。

## 范围和清单

| 范围 | 入口 | 安装内容 | 不包含 |
|---|---|---|---|
| 完整系统（基础能力） | 执行 Release `install.sh`；源码入口为 `sh scripts/bootstrap.sh --yes` | agentq、8 个自有技能完整目录、4 个必需 Waza 技能、templates/support/license/lock | 自动接活、Telegram、作者规则/权限/身份、第三方缓存 |
| 完整扩展能力 | 同一入口加 `--extras` | 基础内容 + 4 个可选 Waza 技能：read、write、health、learn | 其他本机技能、插件缓存或私有配置 |
| 仅技能 | 同一入口加 `--scope skills` | 完整技能闭包、支持文件、模板与许可证 | 后台二进制、服务；不要求 Claude 登录 |
| 插件与 MCP | 首发不安装 | 0 个插件、0 个 MCP 服务器 | 不接管宿主插件 cache，也不把本机模块算入发行 |
| 可选调度服务 | 完整入口加 `--services` | tick 每 5 分钟、night 01:00、review 06:30，使用当前配置 | 不自动 resume；不装 Telegram listen |
| 可选代码备份 | 完整入口加 `--backup` | 每 86400 秒运行一次的用户级备份服务 | 无显式备份目标就不写远端；不改全局 Git Hook |
| 可选看门狗 | 完整入口加 `--watchdog` | 每 1800 秒运行一次的独立 shell 看门狗、本地报告 | 不恢复调度、不发送消息、不改权限 |

自有技能：agent-task、dispatch、nightly-review、new-project、project-status、no-ai-look（含 references）、prior-art、feedback-triage。五模板：AGENTS、BACKLOG、PROGRESS、DECISIONS、LESSONS。支持：with-lock、ask-rule-scan、watchdog。

必需 Waza：check、think、hunt、ui。可选 Waza：read、write、health、learn，仅在指定 `--extras` 时安装。来源、完整 commit、许可证和逐文件 Git blob 哈希见 `dependencies/claude.lock.json`。安装器只取声明文件，验证哈希，保留 MIT 许可证，不执行上游安装脚本、不复制 vendor/cache。首发包含 0 个插件、0 个 MCP 服务器；本机其他技能和缓存不算发行内容。精确清单见 [`../COMPONENTS.json`](../COMPONENTS.json) 和 [`components.md`](components.md)。首发不包含品牌图片或其他媒体资源。

## 计划、所有权与定制

```sh
sh scripts/bootstrap.sh --plan
```

先检测并构建可自检的候选，再打印计划，不激活入口。计划列出具体变化路径、前后类型及事务备份位置；选用 backup 时额外检测 gh 的路径与版本，远程身份和目标授权在实际备份时核实。**准备计划会联网获取锁定的公开依赖并执行自己的临时 HOME 自检**，不执行第三方安装脚本，不改当前技能/服务/配置。`--yes` 确认该范围的安装文件变化；服务旗标是额外显式选择，不授予账号/发布/付款等权限。

- 厂商资产位于 `~/.local/share/agentq/generations/gen-*/`。受管二进制与技能入口是指向该代的链接；receipt 记录所有权和整代摘要。
- 用户定制保留在项目规则、用户规则、独立技能/Hook/状态栏和本机配置，不直接编辑厂商 generation。它们不随代码回滚。
- 已有同名技能：完整内容及权限位相同则保留为外部所有，卸载不删除；不同则报告冲突、停止激活，原件不变。相同的外部技能在回执中记录依赖摘要，doctor 会报告随后丢失或修改，但不会因此接管或删除它们。不替用户删重、选旧版或覆盖手改内容。
- 已受管技能链接或 generation 有本地修改、未知文件/服务同名、非法 JSON/常用配置结构错误时停止，保留冲突证据。不把原子替换说成“自动保留定制”。
- 既有配置逐字节保留，包括未知字段、第三方 Hook/状态栏和开关。新配置显式 `paused=true`，night/review/patrol/notify 全关、`permission_mode=default`；不继承作者 bypass 权限。旧版缺省 bypass 的配置兼容语义保留，不静默改现有策略。
- 安装/更新与 tick 共用锁；有运行任务或未知状态时拒绝替换。启动前做临时 HOME status/tick 自检。正在执行的文件不被就地覆盖或删掉。
- 激活前保存受限权限事务备份，逐路径写入；失败恢复旧文件/服务，崩溃则保留 pending 并拒绝继续，使用 restore 恢复。活动、上一代和卸载恢复回执共用完整校验；不完整回执拒绝恢复，不能把缺失字段当成删除指令。没有受管回执时 doctor 明确报未安装。数据、身份、秘密不进入安装事务备份。

遇到冲突时先按计划路径备份并比较，由用户在自己的规则层调整或明确决定迁移；安装器没有“强制覆盖所有定制”开关。

## 使用：不配置 Telegram

安装后运行 `~/.local/bin/agentq status` 查看暂停状态。在项目目录运行 **`claude`**，用自然语言说“派活：……”；宿主根据标准 skill description 发现 dispatch，写入 BACKLOG 并调用 agentq。`agentq run/status/logs` 是管理命令，不是聊天窗口。

`agentq run <项目名> <关键词>` 是明确启动，即使自动调度暂停也会尝试运行，仍遵守认证/额度/并发限制。未授权无人值守工具时，默认权限可能拒绝写入；安装不会替用户改权限，用户应按自己的项目授权选择允许的权限策略。

本地结果：`~/.agentq/jobs/<ID>/`、`agentq logs <ID>`、`~/.claude/nightly/<日期>-agentq.md`。不用 Telegram 也完整生成。Telegram 配置和 `listen` 是用户之后主动启用的通道；本入口不强制索要机器人 token、不自动装监听、不发送外部消息。完整系统退出桌面/终端窗口后仍可由已选择安装的 launchd 服务运行，但新安装保持暂停，须用户明确启用调度。

## 升级、回滚、卸载与恢复

- 升级：从 GitHub Releases 下载目标版本的 `install.sh` 到临时文件后重新执行；源码检出也可运行 `scripts/update.sh`。两种方式都会构建候选、保留原来的可选模块/服务选择并重新比较，不覆盖用户配置。未受管旧版 `update.sh` 保留原 binary-only 兼容流程，不算完整安装。
- 整代回滚：`agentq rollback --yes`，恢复上一代 binary/skills/support/templates/service 入口；配置和任务状态不回滚。回滚目标被消耗，避免在两版间反复摆动。
- 卸载：`agentq remove --yes`（`agentq uninstall --yes` 同义）。只移除回执拥有且仍一致的入口及服务，保留配置、任务、秘密、generations 和备份。用户后来修改受管文件时先停，不丢内容。
- 恢复卸载：`sh scripts/bootstrap.sh restore --yes`；或卸载时打印的保留代 `bin/agentq restore --yes`。不需要重新拼装技能。skills-only 同样可由 bootstrap 恢复。
- 中断恢复：`sh scripts/bootstrap.sh restore --yes` 优先撤回 pending 事务；发现用户在中断后修改过冲突路径时停止并保留备份，不强盖。
- 受管 tick 连续两次非 0，保护脚本尝试通过上代二进制执行整代回滚；活跃任务、用户修改或未知状态会阻止自动替换。仅服务加载失败不会丢掉旧定义。无上代时不能凭空回滚；watchdog 留本地报告。
- 不自动清理旧代、秘密或用户数据。卸载不是抹除数据；保留代的磁盘清理由用户另行确认。

可选备份目标必须由用户在本机 `~/.agentq/backup.json` 明确授权：`{"targets":[{"project":"/absolute/project","remote":"backup","url":"https://example.invalid/owner/repo.git"}]}`。每次检查 remote push URL 未变且 gh 识别为 PRIVATE，才推分支和 tags；没配目标不推，未知/公开/URL 改变均拒绝。实际 URL 由用户填写，不使用样例地址。看门狗和备份报告在 `~/.agentq/*-report.txt`，默认只在本地。

## 验证边界与官方资料

v0.1.0 的实际支持范围是 macOS 13+ 与 Claude Code CLI。arm64 已在真实硬件验证；amd64 已验证构建，但尚未在 Intel Mac 实机运行。Linux、WSL、Windows 和 Codex 不在首发支持范围内；Codex 相关实现保留在私有开发分支，公开版本不依赖它。

常规离线测试：`go test ./...`，全部使用临时 HOME、假 CLI 和假 launchctl。显式网络入口验收：`AGENTQ_BOOTSTRAP_ACCEPTANCE=1 go test -count=1 -v -run '^TestBootstrapAcceptance$' ./...`。真实模型闭环：`AGENTQ_REAL_E2E=1 go test -count=1 -v -run '^TestRealClaudeInstallationE2E$' ./...`；连接型身份只有另显式设置 `AGENTQ_REAL_E2E_CONNECTION=1` 时，才把调用方现有连接环境最小投影到临时 HOME，用后删除，不读取真实 HOME。它会消耗已授权模型额度，不默认在 CI 运行。

官方资料：[Claude Code 安装与 stable 通道](https://code.claude.com/docs/en/setup)、[CLI 参数与 auth status](https://code.claude.com/docs/en/cli-reference)、[GitHub CLI repo view](https://cli.github.com/manual/gh_repo_view)。原生 Claude 自动更新保留自定义 launcher，但普通 launcher 仍由官方管理；npm 支持自动更新，Homebrew 默认通过包管理器手动更新。这些策略不等于保留所有用户定制；agentq 不覆盖它们，也不接管用户自定义 CLI launcher。
