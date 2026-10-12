# Third-party notices / 第三方声明

agentq-owned source is MIT licensed; see LICENSE. 自有源码使用 MIT 许可证。

- Waza by Tw93, MIT, commit `318d6c2c219894798b09c2b39d7f83d0be1a313c`: required check/think/hunt/ui, optional read/write/learn/health. Exact files and Git blob integrity are locked in `dependencies/claude.lock.json`; installation downloads only declared files and preserves the upstream license. See `licenses/Waza-LICENSE`. 安装仅下载锁定的公开文件，不复制本机缓存或身份，保留原许可证。
- Go runtime and standard library: BSD-3-Clause, see `licenses/Go-LICENSE` in binary distributions. Go 运行时和标准库许可证随二进制分发。
- Claude Code is an external, separately licensed execution engine. It is not redistributed here, and its subscription/API usage is not included in agentq's open-source license. Claude Code 为外部执行器，不随此仓库分发；模型订阅与 API 费用另计。

No third-party host plugins, MCP servers, private identities, plugin caches or unverified-brand media are bundled. 不包含宿主插件、MCP 服务、私人身份、插件缓存或权属未核实的品牌媒体。
