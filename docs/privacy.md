# 隐私与权限

[English](privacy.en.md)

公开源码只包含通用代码、测试、技能、模板和文档。原私有仓库的历史、工作树、协作记录、身份、机器配置、会话、日志、数据库和媒体没有进入这个新仓库。安装不复制作者配置。

运行记录位于本机 `~/.agentq/`，可能包含私人任务内容，应按秘密保护。配置与秘密不进入发行包；Telegram 可选，启用后相应消息会发给所配置的服务。Claude CLI 的模型请求遵循其服务协议，agentq 不提供模型服务、不包含订阅，也不暗中切换计费。

新安装默认暂停调度、关闭通知并使用 default 权限。服务与额外能力明确选择。升级比较所有权与冲突，保留用户配置、认证、权限和任务；原子替换并不代表覆盖用户定制是安全的。

`python3 -I scripts/privacy_scan.py --patterns /path/to/private-patterns.json --root /path/to/candidate --manifest config/clean-source.json` 检查全部候选文件，包括 ignored/untracked 文件。规则保存在仓库外的600文件、700父目录，不能包含在提交或包中。格式为 `{"version":1,"patterns":[{"type":"personal-identifier","literal":"your-private-identifier"}]}`，替换为自己的实际私人标识。

扫描0表示本次已定义规则没有发现，1表示发现，2表示无法可靠检查；不证明所有语义或编码秘密都被识别。图片、归档和二进制默认需独立内容/权属检查与固定摘要批准。公开 Git 作者元数据也须单独检查。

默认测试使用临时 HOME、假 CLI 和假服务。真实模型验收仅显式启用，使用隔离项目与获授权身份；普通测试通过不能代替真实模型证据。账户本人登录由本人完成，不能从旧认证文件复制令牌来伪装隔离。

可选旧配置迁移与安全备份说明见 [config-privacy.md](config-privacy.md)。不要用镜像删除方式同步带状态的服务器；本项目不提供这种部署流程。
