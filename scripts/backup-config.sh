#!/bin/sh
# Prepare only the safe projection and bounded config source, then commit if changed.
set -eu
config_dir=${1:-${CLAUDE_CONFIG_DIR:-$HOME/.claude}}
python3 -I "$config_dir/scripts/config_privacy.py" prepare --config-dir "$config_dir"
if ! git -C "$config_dir" diff --cached --quiet; then
    attribution="Co-Authored-By: Claude Opus 5 <noreply$(printf '\100')anthropic.com>"
    git -C "$config_dir" commit -q -m "备份：$(date +%F) 配置安全投影" \
        -m "安全投影与受控源已检查；本机配置不入提交，旧私有历史保留。" \
        -m "$attribution"
fi
