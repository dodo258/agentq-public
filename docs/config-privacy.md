# 配置备份的隐私边界

[English](config-privacy.en.md)

`settings.json` 是运行配置，不是可分发模板。认证、权限和 Hook 调度都继续从本机真实文件读取；Git 只保存由工具生成的安全投影 `config/settings.example.json`。

## 为什么不用 `settings.local.json` 拆全局秘密

Claude Code 官方文档只把 `~/.claude/settings.json` 定义为全局用户设置；`.claude/settings.local.json` 是“当前项目”的本机覆盖，路径位于项目内。官方还说明，同名键按 managed、命令行、项目 local、项目 shared、用户设置的顺序取值，而列表通常跨层合并。把文件放成 `~/.claude/settings.local.json` 不是受支持的全局覆盖方案，也可能令权限或认证行为与原配置不同。

依据：[Settings files and precedence](https://code.claude.com/docs/en/settings)，其中的范围表分别列出 `~/.claude/settings.json`（every project）和 `.claude/settings.local.json`（this one project only）；“Lists merge instead of overriding”说明 `permissions.allow` 等列表的合并行为。

因此迁移不拆配置、不改权限默认值、不移动认证：真实 `settings.json` 原样留在本机，权限设为 `0600`，只从 Git 索引移除。

## 工具接口

脚本只用 Python 标准库：

```sh
python3 scripts/config_privacy.py project  [--config-dir DIR]
python3 scripts/config_privacy.py migrate  [--config-dir DIR]
python3 scripts/config_privacy.py rollback [--config-dir DIR]
python3 scripts/config_privacy.py prepare  [--config-dir DIR]
```

未给 `--config-dir` 时，依次使用 `$CLAUDE_CONFIG_DIR`、`$HOME/.claude`。测试和试运行应设置临时 `HOME` 或显式传临时目录，不能指向真实配置。

### `project`

读取 `settings.json`，原子写入 `config/settings.example.json`，不碰 Git：

- 整体省略 `env`；
- 递归移除 `headers`、`auth`、`token`、`secret`、`password`、`credential`、`cookie`、`apiKey`、`accessKeyId`、`privateKey` 等字段；
- 把本机 HOME 绝对前缀改写成 `~`，遮盖 URL 用户信息和命令中的常见认证参数；
- 若已移除的长敏感值仍出现在投影其他位置，失败关闭；
- 不打印值或对象摘要。

仓库里的 `config/settings.example.json` 只是无秘密的格式样例。安装后的文件是生成物；不要手工维护。

### `migrate`

在配置仓根目录执行一次迁移：

1. 校验真实配置是 JSON object，保留文件字节，设置权限为 `0600`；
2. 从临时 Git 索引移除 `settings.json`、`settings.local.json`、`.claude.json` 和 `plugins/`，不删除工作区原件；
3. 在配置仓 `.gitignore` 末尾与本机 `.git/info/exclude` 同时加入保护块，并用Git实际确认忽略生效；前者优先于旧的否定规则，不依赖项目级 local settings；
4. 暂存安全投影，不提交、不推送、不改写历史；
5. 在 `.git/config-privacy-state/` 保存仅供回滚的私有状态。

迁移使用临时索引和原子替换；校验或暂存失败时恢复投影、gitignore、exclude、索引和权限位，不留下半迁移。重复执行是幂等的。

`~/.claude.json` 位于配置仓外，是 Claude Code 本机认证状态，工具不会读取、复制或暂存它；配置仓内若存在同名副本则按私有文件排除。`plugins/` 也整体排除，避免插件缓存或认证状态进入备份。

### `rollback`

只用于“迁移后、提交前”撤销。它恢复迁移前的索引、原有投影、`.gitignore`、`.git/info/exclude` 和权限位，绝不改写 `settings.json` 内容。若 HEAD、索引、投影或 exclude 在迁移后发生了其他变化，工具拒绝自动回滚，避免覆盖后续工作。

回滚会把迁移前已受控的真实配置重新放回索引；回滚后不要运行备份提交，除非重新迁移。迁移已提交后，下一次 `prepare` 或 `migrate` 会确认 HEAD 中已无私有路径，再清理过期回滚状态。

### `prepare`

给备份任务调用。它先重建投影，再只在临时索引中暂存以下受控白名单：

- `config/settings.example.json`
- `.gitignore`
- `CLAUDE.md`、`CLAUDE_BASE.md`、`CLAUDE_TEMPLATE.md`
- `ENGINEERING_DECISIONS.md`、`ONE_LINE_BOOTSTRAP.md`、`PROJECT_BOOTSTRAP.md`、`PROJECT_LOG_TEMPLATE.md`、`SYSTEM_CHANGELOG.md`、`facts.md`
- `git-hooks/`、`hooks/`、`scripts/`、`skills/`、`templates/`、`statusline.sh`

它不会执行全仓 `git add -A`。`projects/`、`nightly/`、运行状态、`settings.local.json`、`.claude.json`、`plugins/` 和常见密钥文件都不会自动暂存。已有暂存区若包含白名单外路径则失败；唯一例外是迁移造成的私有路径删除。工具还会检查“下一棵提交树”：真实配置私有路径、凭据型文件名或当前配置中的已知敏感值仍在其中时拒绝继续。

秘密字段单独变化时，投影字节不变，真实配置又不受 Git 控制，因此下一次 `prepare` 不产生对应提交。工具本身不提交、不推送。

## 安装与真实迁移（由主流程执行）

先在临时 HOME 跑测试；确认后再安装。以下命令是建议步骤，不应由开发分支自动执行：

```sh
install -m 0755 scripts/config_privacy.py "$HOME/.claude/scripts/config_privacy.py"
install -d -m 0755 "$HOME/.claude/config"
install -m 0644 config/settings.example.json "$HOME/.claude/config/settings.example.json"
python3 "$HOME/.claude/scripts/config_privacy.py" migrate --config-dir "$HOME/.claude"
```

迁移后先人工确认：

```sh
test "$(stat -f '%Lp' "$HOME/.claude/settings.json")" = 600
git -C "$HOME/.claude" diff --cached --name-status
python3 "$HOME/.claude/scripts/config_privacy.py" prepare --config-dir "$HOME/.claude"
```

认证验证只运行 Claude Code 的只读状态检查，不发模型请求。不要打印配置、令牌、投影差异中的敏感行或任何秘密摘要。

## `backup-push.sh` 的精确改法

把现有配置仓分支里的全仓暂存：

```sh
(cd "$c" && git add -A && { git diff --cached --quiet || git ... commit ...; })
```

替换为“准备失败就不提交、不推这个配置仓”的结构：

```sh
if ! python3 "$c/scripts/config_privacy.py" prepare --config-dir "$c" >/dev/null 2>&1; then
  bad=$((bad+1))
  lines="$lines\n- 配置仓：**安全投影或暂存失败，未提交、未推送**"
elif git -C "$c" diff --cached --quiet; then
  push_repo "$c"
elif "$c/scripts/backup-config.sh" "$c"; then
  push_repo "$c"
else
  bad=$((bad+1))
  lines="$lines\n- 配置仓：**提交失败，未推送**"
fi
```

`scripts/backup-config.sh` 使用本机已有 Git 提交身份与正常 Hook，不在通用脚本内写作者身份，也不跳过 Hook。保留原有私有远程检查、定时频率、锁和推送策略。关键点是彻底移除配置仓路径上的 `git add -A`，且 `prepare` 或提交失败后不能仍调用 `push_repo "$c"`。

## 限制

- 工具保证当前真实配置中的已知秘密不进入下一棵备份提交树，不声称清除了旧私有历史；本阶段不重写历史。
- 任意普通字符串都可能承载未知秘密。递归字段过滤、敏感值反查和暂存白名单是失误防线，不替代提交前的全仓凭据扫描。
- 投影不是可直接启用的配置，也不能拿来覆盖真实 `settings.json`；其中省略认证和环境设置是有意的。
