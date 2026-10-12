# agentq

开发 agentq 期间，我的 Claude Code 账号突然被封。这次封禁在我看来莫名其妙，也打乱了项目最后的收尾工作。

这套工具还有不少可以完善的地方。我选择把它开源，希望有兴趣的朋友能一起改进。欢迎提 Issue、提交 PR，或分享你的使用体验，让这套工具变得更好用。

[English](README.en.md)

agentq 是一个面向 Claude Code 的本地后台调度器：它从项目 `docs/BACKLOG.md` 领取待做事项，启动独立的 `claude -p` 进程，并按项目规则完成实现、验证和交接。

## 首发支持范围

| 环境 | v0.1.0 状态 |
| --- | --- |
| macOS 13+，Apple Silicon arm64 | 已实机验证 |
| macOS 13+，Intel amd64 | 已验证构建，尚未在 Intel Mac 实机运行 |
| Claude Code CLI | 已通过，需已安装并登录 |
| Codex | 首发不支持；相关实现保留在私有开发分支 |
| Linux、WSL、Windows | 首发不支持 |

首发不包含品牌图片，也不依赖徽标或媒体资源。

## 安装

推荐从 v0.1.0 Release 下载安装脚本到临时文件，再执行：

```sh
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
curl -fL https://github.com/dodo258/agentq-public/releases/download/v0.1.0/install.sh -o "$tmp"
sh "$tmp"
```

安装脚本检查 macOS 架构以及 `curl`、`python3`、`git`、`shasum` 是否存在，下载同版本压缩包和 `SHA256SUMS`，校验后在临时目录调用包内的 `agentq setup --yes "$@"`。它不要求 Go 或 Node，也不会静默安装依赖。

需要预先具备：

- macOS 13+；
- Git、Python 3；
- 已安装并登录的 Claude Code CLI。

缺少本地工具时，Release 脚本会指出缺少的命令并退出；找不到 Claude Code CLI 时，setup 会显示官方 stable 安装命令和 `claude auth login` 提示。安装器不会替你登录、索要 token，或改动认证方式。

从源码安装需要 `go.mod` 指定的 Go 版本：

```sh
git clone https://github.com/dodo258/agentq-public.git
cd agentq-public
sh scripts/bootstrap.sh --yes
```

新安装默认暂停调度，关闭 night、review、patrol 和通知，并使用 `permission_mode=default`。安装器不会改写已有权限、认证或用户配置。后台服务、代码备份和看门狗都要显式选择：

```sh
~/.local/bin/agentq setup --yes --services
~/.local/bin/agentq setup --yes --backup
~/.local/bin/agentq setup --yes --watchdog
```

完整安装、升级、回滚和卸载说明见[中文安装指南](docs/install.zh-CN.md)或[英文安装指南](docs/install.en.md)。

若终端找不到 `agentq`，可用 `~/.local/bin/agentq`，或把 `$HOME/.local/bin` 加入 PATH；安装不会静默修改你的 shell 配置。

## 三个常用例子

在项目目录打开 Claude Code，用自然语言派活：

```text
派活：给导出流程加失败重试并补测试
```

查看调度、额度、运行任务和待做数量：

```sh
agentq status
```

即使自动调度仍暂停，也可以明确启动某个项目中的一条待做：

```sh
agentq run my-project "导出流程"
```

## 完整流程

1. 你在 Claude Code 会话里说明需求，`dispatch` 把它写进项目 `docs/BACKLOG.md`。
2. agentq 的 `tick` 从待做事项中挑选可执行条目，并启动独立的 `claude -p` 进程。
3. 后台进程按 `agent-task` 和项目规则工作，包括分支、实现、测试、检查与合并。
4. agentq 负责并发、超时、额度、失败熔断、续跑、日志和本地汇总。
5. 你可以用 `agentq status`、`agentq logs <任务id>`、项目 BACKLOG 和本地报告查看结果。

agentq 本身不修改 BACKLOG，不写产品代码，也不提交或合并。它负责挑活、启动、看护和记录，实际工作由 Claude Code 会话及技能完成。

## 发行组件

| 类别 | v0.1.0 内容 |
| --- | --- |
| agentq 自有技能 | 8 个：agent-task、dispatch、nightly-review、new-project、project-status、no-ai-look、prior-art、feedback-triage |
| 必需 Waza 技能 | 4 个：check、think、hunt、ui |
| 可选 Waza 技能 | 4 个：read、write、health、learn，使用 `--extras` 安装 |
| 项目模板 | 5 个：AGENTS、BACKLOG、PROGRESS、DECISIONS、LESSONS |
| 支持脚本 | 3 个：`ask-rule-scan.sh`、`watchdog.sh`、`with-lock.sh` |
| 源码与发行脚本 | 8 个：`backup-config.sh`、`bootstrap.sh`、`config_privacy.py`、`install.sh`、`package-release.sh`、`pre-commit`、`privacy_scan.py`、`update.sh` |
| 插件 / MCP | 0 / 0 |
| 常驻进程 | `agentq listen` 可选，不默认安装或启用 |

精确文件清单见 [`COMPONENTS.json`](COMPONENTS.json)，便于阅读的说明见 [`docs/components.md`](docs/components.md)。本机其他技能、插件缓存和私有配置不属于发行内容。

## 可选 Telegram

Telegram 只用于收发消息，不负责模型推理，也不能代替 Claude CLI 登录。未配置 Telegram 时，任务调度、状态、日志和本地报告仍可正常使用。安装不会索要机器人 token，也不会默认安装常驻监听。

## 费用、权限与安全

- agentq 代码以 MIT 开源，不提供模型服务。后台任务使用你配置的 Claude 身份，可能消耗订阅额度或产生 API 费用；未知用量不当作余额或实际账单。
- 新安装使用 `permission_mode=default`。无人值守写入是否允许，由你自己的 Claude Code 和项目权限决定。
- 安装器不新增账号、不登录、不改认证、不复制作者配置，也不把秘密写进 launchd plist。
- 发布资产通过 `SHA256SUMS` 校验。第三方 Waza 文件固定到明确 commit 和逐文件哈希，并保留上游许可证。
- 更新前可先运行 `agentq doctor`。受管安装支持整代回滚、事务恢复和只删除受管入口的卸载。

如果仓库启用了 GitHub 私密漏洞报告，安全问题优先走该渠道；否则先提交不含敏感细节的 Issue 询问私下联系方式。不要在公开 Issue 中粘贴 token、配置或任务日志里的私有内容。

## 详细指南

- [完整中文参考](docs/reference.zh-CN.md)
- [Complete English reference](docs/reference.en.md)
- [配置与隐私](docs/config-privacy.md)
- [给自动化 Agent 的入口](llms.txt)

安装入口和首发支持范围以本 README 与安装指南为准。

## 贡献

欢迎提交 Issue、PR 和真实使用反馈。请说明系统版本、架构、复现步骤、期望结果和实际结果；日志请先删除用户名、路径、token、聊天内容和项目私有信息。

## 许可证

agentq 使用 [MIT License](LICENSE)。第三方组件的来源、版本、安装策略和许可证记录在 [`dependencies/claude.lock.json`](dependencies/claude.lock.json) 与发行清单中。
