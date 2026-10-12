# Components / 组件清单

The machine-readable manifest is [COMPONENTS.json](../COMPONENTS.json), generated from actual source and the dependency lock. 清单按实际源码和依赖锁生成，不计缓存、禁用项或作者个人插件。

| Type / 类型 | Count / 数量 | Items / 内容 |
| --- | ---: | --- |
| Owned skills / 自有技能 | 8 | `agent-task`, `dispatch`, `feedback-triage`, `new-project`, `nightly-review`, `no-ai-look`, `prior-art`, `project-status` |
| Required external skills / 必需外部技能 | 4 | `check`, `think`, `hunt`, `ui` |
| Optional external skills / 可选外部技能 | 4 | `read`, `write`, `health`, `learn` |
| Templates / 模板 | 5 | `AGENTS.md`, `BACKLOG.md`, `DECISIONS.md`, `LESSONS.md`, `PROGRESS.md` |
| Support scripts / 支持脚本 | 3 | `ask-rule-scan.sh`, `watchdog.sh`, `with-lock.sh` |
| Host plugins / 宿主插件 | 0 | Not bundled / 不捆绑 |
| MCP servers / MCP 服务 | 0 | Not bundled / 不捆绑 |
| Services enabled by default / 默认启用服务 | 0 | Explicit opt-in / 明确选择 |

Required Waza files are downloaded from one locked commit and verified against Git blob hashes. Waza versions and MIT notice are in the lock and THIRD_PARTY_NOTICES.md. 必需 Waza 文件来自锁定提交并验证 Git blob 摘要；版本和 MIT 声明见依赖锁与第三方声明。

Optional services: tick/night/review, private backup and watchdog; Telegram listen is configured separately. Scheduling and notifications start disabled. 可选服务为调度/夜班/复盘、私有备份和看门狗；Telegram 监听另行配置。新安装的调度与通知均关闭。
