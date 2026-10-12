# Contributing / 参与贡献

Issues and pull requests are welcome. Include the agentq/CLI versions, OS/architecture, reproduction and expected/actual behavior. Remove credentials, personal paths and private project details from reports. Do not upload raw sessions or authentication files.

欢迎提交 Issue 和 PR。请附版本、系统与架构、复现步骤和预期/实际行为；去除凭据、个人路径和私人项目细节，不上传原始会话或认证文件。

Read AGENTS.md, create a task branch, make scoped changes, add regression tests, and run formatting, go vet and go test. Tests use temporary HOME and fake engines by default. Real-model tests are opt-in, use authorized identity and consume model usage. Keep both language versions consistent and include upstream licenses with third-party files.

先读 AGENTS.md，在任务分支做范围明确的改动，补回归测试并运行格式检查、go vet 和 go test。默认测试用临时 HOME 和假执行器；真实模型测试需明确启用、获得身份授权，并消耗模型额度。中英内容保持对应，第三方文件保留原许可证。

Do not work around authentication, sandbox, ownership or privacy checks to make a test pass. Unverified platforms/backends must be labeled rather than presented as supported.

不绕过认证、沙箱、文件所有权或隐私检查来通过测试。未验证的平台和后端如实标注，不写成已支持。
