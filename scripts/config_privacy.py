#!/usr/bin/env python3
"""Create a safe Claude settings projection and prepare a bounded backup index."""

from __future__ import annotations

import argparse
import json
import os
import re
import shutil
import stat
import subprocess
import sys
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable, Sequence

SETTINGS_NAME = "settings.json"
PROJECTION_NAME = "config/settings.example.json"
PRIVATE_PATHS = (
    "settings.json",
    "settings.local.json",
    ".claude.json",
    "plugins",
)
EXCLUDE_BLOCK = (
    "# config-privacy: live Claude data stays local\n"
    "/settings.json\n"
    "/settings.local.json\n"
    "/.claude.json\n"
    "/plugins/\n"
    "**/__pycache__/\n"
    "*.pyc\n"
    "*.pyo\n"
    "# config-privacy: end\n"
)
SAFE_BACKUP_PATHS = (
    ".gitignore",
    "CLAUDE.md",
    "CLAUDE_BASE.md",
    "CLAUDE_TEMPLATE.md",
    "ENGINEERING_DECISIONS.md",
    "ONE_LINE_BOOTSTRAP.md",
    "PROJECT_BOOTSTRAP.md",
    "PROJECT_LOG_TEMPLATE.md",
    "SYSTEM_CHANGELOG.md",
    "facts.md",
    "git-hooks",
    "hooks",
    "scripts",
    "skills",
    "statusline.sh",
    "templates",
    PROJECTION_NAME,
)
FORBIDDEN_BASENAMES = {
    ".env",
    ".claude.json",
    "credentials",
    "credentials.json",
    "settings.local.json",
}
FORBIDDEN_SUFFIXES = (
    ".key",
    ".pem",
    ".p12",
    ".pfx",
    ".token",
)
SENSITIVE_WORDS = {
    "auth",
    "authentication",
    "authorization",
    "cookie",
    "cookies",
    "credential",
    "credentials",
    "header",
    "headers",
    "keychain",
    "oauth",
    "bearer",
    "passwd",
    "password",
    "privatekey",
    "secret",
    "token",
}
SENSITIVE_COMPACT_KEYS = {
    "apikey",
    "apikeyhelper",
    "clientsecret",
    "proxyauthhelper",
    "privatekey",
}
STATE_VERSION = 2


class PrivacyError(RuntimeError):
    """An expected, value-free failure safe to show to the caller."""


@dataclass(frozen=True)
class Repo:
    root: Path
    git_dir: Path
    index: Path
    exclude: Path
    state: Path


def _run(
    args: Sequence[str],
    *,
    env: dict[str, str] | None = None,
    input_bytes: bytes | None = None,
    label: str,
) -> bytes:
    proc = subprocess.run(
        list(args),
        env=env,
        input=input_bytes,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )
    if proc.returncode != 0:
        raise PrivacyError(f"{label} failed")
    return proc.stdout


def _git(repo: Repo, *args: str, index: Path | None = None, label: str = "git operation") -> bytes:
    env = os.environ.copy()
    if index is not None:
        env["GIT_INDEX_FILE"] = str(index)
    return _run(("git", "-C", str(repo.root), *args), env=env, label=label)


def _git_optional(repo: Repo, *args: str, index: Path | None = None) -> tuple[int, bytes]:
    env = os.environ.copy()
    if index is not None:
        env["GIT_INDEX_FILE"] = str(index)
    proc = subprocess.run(
        ("git", "-C", str(repo.root), *args),
        env=env,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )
    return proc.returncode, proc.stdout


def _resolve_config_dir(raw: str | None) -> Path:
    if raw:
        path = Path(raw).expanduser()
    else:
        configured = os.environ.get("CLAUDE_CONFIG_DIR")
        path = Path(configured).expanduser() if configured else Path.home() / ".claude"
    path = path.resolve()
    if not path.is_dir():
        raise PrivacyError("config directory is missing or is not a directory")
    return path


def _repo(config_dir: Path) -> Repo:
    root_raw = _run(
        ("git", "-C", str(config_dir), "rev-parse", "--show-toplevel"),
        label="git repository lookup",
    ).decode().strip()
    root = Path(root_raw).resolve()
    if root != config_dir:
        raise PrivacyError("config directory must be the repository root")
    git_dir_raw = _run(
        ("git", "-C", str(root), "rev-parse", "--absolute-git-dir"),
        label="git metadata lookup",
    ).decode().strip()
    git_dir = Path(git_dir_raw).resolve()
    index_raw = _run(
        ("git", "-C", str(root), "rev-parse", "--git-path", "index"),
        label="git index lookup",
    ).decode().strip()
    index = Path(index_raw)
    if not index.is_absolute():
        index = (root / index).resolve()
    common_raw = _run(
        ("git", "-C", str(root), "rev-parse", "--git-common-dir"),
        label="git metadata lookup",
    ).decode().strip()
    common = Path(common_raw)
    if not common.is_absolute():
        common = (root / common).resolve()
    return Repo(
        root=root,
        git_dir=git_dir,
        index=index,
        exclude=common / "info" / "exclude",
        state=git_dir / "config-privacy-state",
    )


def _read_settings(config_dir: Path) -> tuple[dict[str, Any], bytes, int]:
    path = config_dir / SETTINGS_NAME
    if not path.is_file() or path.is_symlink():
        raise PrivacyError("settings.json must be a regular local file")
    raw = path.read_bytes()
    try:
        value = json.loads(raw)
    except (UnicodeDecodeError, json.JSONDecodeError):
        raise PrivacyError("settings.json is not valid JSON") from None
    if not isinstance(value, dict):
        raise PrivacyError("settings.json must contain a JSON object")
    return value, raw, stat.S_IMODE(path.stat().st_mode)


def _key_words(key: str) -> tuple[set[str], str]:
    expanded = re.sub(r"([a-z0-9])([A-Z])", r"\1_\2", key)
    words = {part.lower() for part in re.findall(r"[A-Za-z0-9]+", expanded)}
    compact = "".join(re.findall(r"[A-Za-z0-9]+", key)).lower()
    return words, compact


def _sensitive_key(key: str) -> bool:
    words, compact = _key_words(key)
    suffixes = ("apikey", "accesskeyid", "privatekey", "sessionkey")
    return (
        bool(words & SENSITIVE_WORDS)
        or compact in SENSITIVE_COMPACT_KEYS
        or any(compact.endswith(suffix) for suffix in suffixes)
    )


def _strings(value: Any) -> Iterable[str]:
    if isinstance(value, str):
        yield value
    elif isinstance(value, dict):
        for child in value.values():
            yield from _strings(child)
    elif isinstance(value, list):
        for child in value:
            yield from _strings(child)


def _secret_candidates(value: str) -> tuple[str, ...]:
    candidates = [value]
    patterns = (
        r"(?i)\b(?:bearer|basic|token)\s+([A-Za-z0-9._~+/=-]{8,})",
        r"(?i)(?:[?&]|\b)(?:access[-_]?token|api[-_]?key|auth|password|secret|token)=([^&#\s]{8,})",
    )
    for pattern in patterns:
        candidates.extend(match.group(1) for match in re.finditer(pattern, value))
    return tuple(dict.fromkeys(candidates))


def _redact_string(value: str, homes: Sequence[str]) -> str:
    result = value
    for home in homes:
        if home:
            result = result.replace(home, "~")
    result = re.sub(r"(?i)(https?://)[^/@\s:]+:[^/@\s]+@", r"\1<redacted>@", result)
    result = re.sub(
        r"(?i)\b(bearer|basic|token)\s+[A-Za-z0-9._~+/=-]+",
        r"\1 <redacted>",
        result,
    )
    result = re.sub(
        r"(?i)(--(?:api[-_]?key|auth|password|secret|token)(?:=|\s+))[^\s]+",
        r"\1<redacted>",
        result,
    )
    result = re.sub(
        r"(?i)([?&](?:access[-_]?token|api[-_]?key|auth|key|password|secret|token)=)[^&#\s]+",
        r"\1<redacted>",
        result,
    )
    result = re.sub(
        r"(?i)\b([A-Z0-9_]*(?:AUTH|TOKEN|SECRET|PASSWORD|API_KEY)[A-Z0-9_]*=)[^\s]+",
        r"\1<redacted>",
        result,
    )
    return result


def _projection(settings: dict[str, Any], config_dir: Path) -> tuple[bytes, tuple[bytes, ...]]:
    removed_values: list[str] = []
    home_candidates = {str(Path.home().resolve())}
    if config_dir.name == ".claude":
        home_candidates.add(str(config_dir.parent.resolve()))

    def clean(value: Any, path: tuple[str, ...]) -> Any:
        if isinstance(value, dict):
            out: dict[str, Any] = {}
            for key, child in value.items():
                if not path and key == "env":
                    for env_key, env_value in child.items() if isinstance(child, dict) else ():
                        if _sensitive_key(str(env_key)):
                            removed_values.extend(_strings(env_value))
                    continue
                if _sensitive_key(str(key)):
                    removed_values.extend(_strings(child))
                    continue
                out[key] = clean(child, path + (str(key),))
            return out
        if isinstance(value, list):
            return [clean(child, path + ("[]",)) for child in value]
        if isinstance(value, str):
            return _redact_string(value, tuple(sorted(home_candidates, key=len, reverse=True)))
        return value

    projected = clean(settings, ())
    projected_strings = tuple(_strings(projected))
    rendered = (json.dumps(projected, ensure_ascii=False, indent=2) + "\n").encode("utf-8")
    secrets: list[bytes] = []
    for value in removed_values:
        for candidate in _secret_candidates(value):
            encoded = candidate.encode("utf-8")
            if len(encoded) >= 8 and encoded not in secrets:
                secrets.append(encoded)
            if candidate and any(candidate in item for item in projected_strings):
                raise PrivacyError("projection still contains a sensitive settings value")
    return rendered, tuple(secrets)


def _atomic_write(path: Path, content: bytes, mode: int) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, name = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
    temp = Path(name)
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(content)
            handle.flush()
            os.fsync(handle.fileno())
        os.chmod(temp, mode)
        os.replace(temp, path)
    finally:
        if temp.exists():
            temp.unlink()


def _file_snapshot(path: Path) -> tuple[bool, bytes, int]:
    if not path.exists():
        return False, b"", 0o644
    if not path.is_file() or path.is_symlink():
        raise PrivacyError("managed path must be a regular file")
    return True, path.read_bytes(), stat.S_IMODE(path.stat().st_mode)


def _restore_file(path: Path, existed: bool, content: bytes, mode: int) -> None:
    if existed:
        _atomic_write(path, content, mode)
    elif path.exists():
        if not path.is_file() or path.is_symlink():
            raise PrivacyError("cannot restore a managed path safely")
        path.unlink()


def _with_exclude_block(content: bytes) -> bytes:
    try:
        text = content.decode("utf-8")
    except UnicodeDecodeError:
        raise PrivacyError("git exclude file is not UTF-8") from None
    if EXCLUDE_BLOCK in text:
        return content
    if text and not text.endswith("\n"):
        text += "\n"
    return (text + EXCLUDE_BLOCK).encode("utf-8")


def _temporary_index(repo: Repo) -> Path:
    fd, name = tempfile.mkstemp(prefix="config-privacy-index.", dir=repo.git_dir)
    os.close(fd)
    temp = Path(name)
    if repo.index.exists():
        shutil.copyfile(repo.index, temp)
    else:
        temp.unlink()
        _git(repo, "read-tree", "--empty", index=temp, label="temporary index setup")
    return temp


def _replace_index(repo: Repo, source: Path) -> None:
    lock = Path(str(repo.index) + ".lock")
    try:
        fd = os.open(lock, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    except FileExistsError:
        raise PrivacyError("git index is busy") from None
    try:
        with os.fdopen(fd, "wb") as handle, source.open("rb") as incoming:
            shutil.copyfileobj(incoming, handle)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(lock, repo.index)
    finally:
        if lock.exists():
            lock.unlink()


def _head(repo: Repo) -> str:
    code, output = _git_optional(repo, "rev-parse", "--verify", "HEAD")
    return output.decode().strip() if code == 0 else ""


def _tree(repo: Repo, index: Path | None = None) -> str:
    return _git(repo, "write-tree", index=index, label="git index validation").decode().strip()


def _index_paths(repo: Repo, index: Path) -> list[tuple[str, str]]:
    raw = _git(repo, "ls-files", "-s", "-z", index=index, label="git index inspection")
    entries: list[tuple[str, str]] = []
    for record in raw.split(b"\0"):
        if not record:
            continue
        try:
            left, path_raw = record.split(b"\t", 1)
            fields = left.decode("ascii").split()
            path = path_raw.decode("utf-8")
        except (ValueError, UnicodeDecodeError):
            raise PrivacyError("git index contains an unsupported path") from None
        if len(fields) != 3 or fields[2] != "0":
            raise PrivacyError("git index contains unresolved entries")
        entries.append((path, fields[1]))
    return entries


def _is_private_path(path: str) -> bool:
    return any(path == item or path.startswith(item + "/") for item in PRIVATE_PATHS)


def _is_allowed_backup_path(path: str) -> bool:
    return any(path == item or path.startswith(item + "/") for item in SAFE_BACKUP_PATHS)


def _forbidden_filename(path: str) -> bool:
    name = Path(path).name.lower()
    return name in FORBIDDEN_BASENAMES or name.endswith(FORBIDDEN_SUFFIXES)


def _validate_worktree_allowlist(repo: Repo, secrets: Sequence[bytes]) -> None:
    for allowed in SAFE_BACKUP_PATHS:
        path = repo.root / allowed
        if not path.exists() and not path.is_symlink():
            continue
        candidates = (path,) if path.is_symlink() or not path.is_dir() else tuple(
            child for child in path.rglob("*") if child.is_file() or child.is_symlink()
        )
        for candidate in candidates:
            relative = candidate.relative_to(repo.root).as_posix()
            if _forbidden_filename(relative):
                raise PrivacyError("a credential-like file is present in the backup allowlist")
            if candidate.is_symlink():
                content = os.readlink(candidate).encode("utf-8")
            elif candidate.is_file():
                content = candidate.read_bytes()
            else:
                continue
            if any(secret in content for secret in secrets):
                raise PrivacyError("a sensitive settings value is present in the backup allowlist")


def _validate_index_has_no_secrets(repo: Repo, index: Path, secrets: Sequence[bytes]) -> None:
    for path, oid in _index_paths(repo, index):
        if _is_private_path(path):
            raise PrivacyError("private Claude data remains in the next backup tree")
        if _forbidden_filename(path):
            raise PrivacyError("a credential-like file remains in the next backup tree")
        if not secrets:
            continue
        blob = _git(repo, "cat-file", "blob", oid, label="git object inspection")
        if any(secret in blob for secret in secrets):
            raise PrivacyError("a sensitive settings value remains in the next backup tree")


def _staged_paths(repo: Repo, index: Path) -> list[str]:
    raw = _git(
        repo,
        "diff",
        "--cached",
        "--name-only",
        "-z",
        index=index,
        label="staged path inspection",
    )
    try:
        return [part.decode("utf-8") for part in raw.split(b"\0") if part]
    except UnicodeDecodeError:
        raise PrivacyError("staged changes contain an unsupported path") from None


def _validate_prepare_boundary(repo: Repo, index: Path) -> None:
    for path in _staged_paths(repo, index):
        if _is_allowed_backup_path(path):
            continue
        if _is_private_path(path):
            raw = _git(
                repo,
                "diff",
                "--cached",
                "--diff-filter=ACMRTUXB",
                "--name-only",
                "-z",
                "--",
                path,
                index=index,
                label="private path validation",
            )
            if not raw:
                continue
        raise PrivacyError("staging area contains a path outside the backup allowlist")


def _stage_projection(repo: Repo, index: Path, projection: bytes) -> None:
    # Keep the object id internal and pass projection content on stdin.
    object_id = _run(
        ("git", "-C", str(repo.root), "hash-object", "-w", "--stdin"),
        input_bytes=projection,
        label="projection object creation",
    ).decode("ascii").strip()
    _git(
        repo,
        "update-index",
        "--add",
        "--cacheinfo",
        f"100644,{object_id},{PROJECTION_NAME}",
        index=index,
        label="projection staging",
    )


def _remove_private_paths(repo: Repo, index: Path) -> None:
    _git(
        repo,
        "rm",
        "-r",
        "--cached",
        "--ignore-unmatch",
        "--",
        *PRIVATE_PATHS,
        index=index,
        label="private path untracking",
    )


def _stage_allowlist(repo: Repo, index: Path) -> None:
    for path in SAFE_BACKUP_PATHS:
        if path == PROJECTION_NAME:
            continue
        code, tracked = _git_optional(repo, "ls-files", "-z", "--", path, index=index)
        if code != 0:
            raise PrivacyError("backup allowlist inspection failed")
        if not (repo.root / path).exists() and not tracked:
            continue
        _git(repo, "add", "-A", "--", path,
             ":(exclude)**/__pycache__/**", ":(exclude)**/*.pyc", ":(exclude)**/*.pyo",
             index=index, label="backup allowlist staging")
    for name, _ in _index_paths(repo, index):
        if "__pycache__" in Path(name).parts or name.endswith((".pyc", ".pyo")):
            _git(repo, "update-index", "--force-remove", "--", name,
                 index=index, label="generated cache untracking")


def _write_state_snapshot(
    state: Path,
    *,
    head: str,
    before_tree: str,
    after_tree: str,
    index_before: bytes,
    settings_mode: int,
    projection_before: tuple[bool, bytes, int],
    projection_after: bytes,
    exclude_before: tuple[bool, bytes, int],
    exclude_after: bytes,
    ignore_before: tuple[bool, bytes, int],
    ignore_after: bytes,
) -> None:
    state.mkdir(mode=0o700)
    _atomic_write(state / "index.before", index_before, 0o600)
    _atomic_write(state / "projection.after", projection_after, 0o600)
    _atomic_write(state / "exclude.after", exclude_after, 0o600)
    _atomic_write(state / "ignore.after", ignore_after, 0o600)
    if ignore_before[0]:
        _atomic_write(state / "ignore.before", ignore_before[1], 0o600)
    if projection_before[0]:
        _atomic_write(state / "projection.before", projection_before[1], 0o600)
    if exclude_before[0]:
        _atomic_write(state / "exclude.before", exclude_before[1], 0o600)
    meta = {
        "version": STATE_VERSION,
        "head": head,
        "before_tree": before_tree,
        "after_tree": after_tree,
        "settings_mode": settings_mode,
        "projection_existed": projection_before[0],
        "projection_mode": projection_before[2],
        "exclude_existed": exclude_before[0],
        "exclude_mode": exclude_before[2],
        "ignore_existed": ignore_before[0],
        "ignore_mode": ignore_before[2],
    }
    _atomic_write(
        state / "state.json",
        (json.dumps(meta, sort_keys=True) + "\n").encode("utf-8"),
        0o600,
    )


def _load_state(repo: Repo) -> dict[str, Any]:
    try:
        raw = (repo.state / "state.json").read_text(encoding="utf-8")
        state = json.loads(raw)
    except (OSError, UnicodeDecodeError, json.JSONDecodeError):
        raise PrivacyError("migration rollback state is missing or invalid") from None
    if not isinstance(state, dict) or state.get("version") != STATE_VERSION:
        raise PrivacyError("migration rollback state has an unsupported version")
    return state


def _update_state_after(repo: Repo, projection: bytes, exclude: bytes, after_tree: str) -> None:
    if not repo.state.is_dir():
        return
    state = _load_state(repo)
    if _head(repo) != state.get("head"):
        return
    paths = (
        repo.state / "projection.after",
        repo.state / "exclude.after",
        repo.state / "state.json",
    )
    snapshots = [_file_snapshot(path) for path in paths]
    state["after_tree"] = after_tree
    try:
        _atomic_write(paths[0], projection, 0o600)
        _atomic_write(paths[1], exclude, 0o600)
        _atomic_write(
            paths[2],
            (json.dumps(state, sort_keys=True) + "\n").encode("utf-8"),
            0o600,
        )
    except Exception:
        for path, snapshot in zip(paths, snapshots):
            _restore_file(path, *snapshot)
        raise


def _cleanup_committed_state(repo: Repo) -> None:
    if not repo.state.is_dir():
        return
    state = _load_state(repo)
    if _head(repo) == state.get("head"):
        return
    for path in PRIVATE_PATHS:
        code, staged = _git_optional(repo, "ls-files", "-z", "--", path)
        if code != 0:
            raise PrivacyError("private path tracking check failed")
        if staged:
            return
    code, committed = _git_optional(
        repo,
        "ls-tree",
        "-r",
        "--name-only",
        "-z",
        "HEAD",
        "--",
        *PRIVATE_PATHS,
    )
    if code != 0 or committed:
        return
    shutil.rmtree(repo.state)


def _validate_private_ignored(repo: Repo) -> None:
    for path in PRIVATE_PATHS:
        probe = path + "/" if path == "plugins" else path
        code, _ = _git_optional(repo, "check-ignore", "--no-index", "-q", "--", probe)
        if code != 0:
            raise PrivacyError("a private config path is not effectively ignored")


def _validate_projection_destination(config_dir: Path) -> Path:
    path = config_dir / PROJECTION_NAME
    if path.exists() and (not path.is_file() or path.is_symlink()):
        raise PrivacyError("projection destination must be a regular file")
    parent = path.parent
    if parent.exists() and not parent.is_dir():
        raise PrivacyError("projection parent is not a directory")
    return path


def command_project(config_dir: Path) -> None:
    settings, _, _ = _read_settings(config_dir)
    projection, _ = _projection(settings, config_dir)
    destination = _validate_projection_destination(config_dir)
    _atomic_write(destination, projection, 0o644)
    print("safe settings projection updated")


def _migrate_existing_state(
    repo: Repo,
    settings_path: Path,
    projection_path: Path,
    projection: bytes,
    secrets: Sequence[bytes],
) -> None:
    state = _load_state(repo)
    if _head(repo) != state.get("head") or _tree(repo) != state.get("after_tree"):
        raise PrivacyError("migration state no longer matches HEAD and the index")
    expected_projection = (repo.state / "projection.after").read_bytes()
    expected_exclude = (repo.state / "exclude.after").read_bytes()
    if (repo.root / ".gitignore").read_bytes() != (repo.state / "ignore.after").read_bytes():
        raise PrivacyError("gitignore changed after migration")
    _validate_private_ignored(repo)
    if not projection_path.is_file() or projection_path.read_bytes() != expected_projection:
        raise PrivacyError("generated projection changed after migration")
    if not repo.exclude.is_file() or repo.exclude.read_bytes() != expected_exclude:
        raise PrivacyError("git exclude file changed after migration")

    index_temp = _temporary_index(repo)
    old_projection = projection_path.read_bytes()
    old_index = repo.index.read_bytes()
    try:
        _remove_private_paths(repo, index_temp)
        _stage_projection(repo, index_temp, projection)
        _validate_index_has_no_secrets(repo, index_temp, secrets)
        after_tree = _tree(repo, index_temp)
        _atomic_write(projection_path, projection, 0o644)
        os.chmod(settings_path, 0o600)
        _replace_index(repo, index_temp)
        _update_state_after(repo, projection, expected_exclude, after_tree)
    except Exception:
        _atomic_write(projection_path, old_projection, 0o644)
        _atomic_write(repo.index, old_index, 0o644)
        raise
    finally:
        if index_temp.exists():
            index_temp.unlink()


def command_migrate(config_dir: Path) -> None:
    settings, _, settings_mode = _read_settings(config_dir)
    projection, secrets = _projection(settings, config_dir)
    projection_path = _validate_projection_destination(config_dir)
    repo = _repo(config_dir)
    _cleanup_committed_state(repo)
    settings_path = config_dir / SETTINGS_NAME
    repo.exclude.parent.mkdir(parents=True, exist_ok=True)
    exclude_before = _file_snapshot(repo.exclude)
    exclude_after = _with_exclude_block(exclude_before[1])
    ignore_path = repo.root / ".gitignore"
    if ignore_path.is_symlink():
        raise PrivacyError("gitignore must be a regular local file")
    ignore_before = _file_snapshot(ignore_path)
    ignore_after = _with_exclude_block(ignore_before[1])

    if repo.state.exists():
        if not repo.state.is_dir():
            raise PrivacyError("migration state path is not a directory")
        _migrate_existing_state(repo, settings_path, projection_path, projection, secrets)
        print("configuration migration already prepared")
        return

    projection_before = _file_snapshot(projection_path)
    if not repo.index.is_file():
        raise PrivacyError("git index is missing")
    index_before = repo.index.read_bytes()
    before_tree = _tree(repo)
    head = _head(repo)
    index_temp = _temporary_index(repo)
    state_temp = Path(tempfile.mkdtemp(prefix="config-privacy-state.", dir=repo.git_dir))
    applied_projection = False
    applied_ignore = False
    applied_exclude = False
    applied_index = False
    try:
        _atomic_write(ignore_path, ignore_after, ignore_before[2] if ignore_before[0] else 0o644)
        applied_ignore = True
        _validate_private_ignored(repo)
        _remove_private_paths(repo, index_temp)
        _git(repo, "add", "--", ".gitignore", index=index_temp, label="private ignore staging")
        _stage_projection(repo, index_temp, projection)
        _validate_index_has_no_secrets(repo, index_temp, secrets)
        after_tree = _tree(repo, index_temp)
        shutil.rmtree(state_temp)
        _write_state_snapshot(
            state_temp,
            head=head,
            before_tree=before_tree,
            after_tree=after_tree,
            index_before=index_before,
            settings_mode=settings_mode,
            projection_before=projection_before,
            projection_after=projection,
            exclude_before=exclude_before,
            exclude_after=exclude_after,
            ignore_before=ignore_before,
            ignore_after=ignore_after,
        )
        _atomic_write(projection_path, projection, 0o644)
        applied_projection = True
        _atomic_write(repo.exclude, exclude_after, exclude_before[2] if exclude_before[0] else 0o644)
        applied_exclude = True
        os.chmod(settings_path, 0o600)
        _replace_index(repo, index_temp)
        applied_index = True
        os.replace(state_temp, repo.state)
    except Exception:
        if applied_index:
            _atomic_write(repo.index, index_before, 0o644)
        if applied_ignore:
            _restore_file(ignore_path, *ignore_before)
        if applied_exclude:
            _restore_file(repo.exclude, *exclude_before)
        if applied_projection:
            _restore_file(projection_path, *projection_before)
        os.chmod(settings_path, settings_mode)
        raise
    finally:
        if index_temp.exists():
            index_temp.unlink()
        if state_temp.exists():
            shutil.rmtree(state_temp)
    print("configuration migration prepared; no commit was created")


def command_rollback(config_dir: Path) -> None:
    repo = _repo(config_dir)
    state = _load_state(repo)
    projection_path = config_dir / PROJECTION_NAME
    settings_path = config_dir / SETTINGS_NAME
    if _head(repo) != state.get("head"):
        raise PrivacyError("HEAD changed after migration; automatic rollback refused")
    if _tree(repo) != state.get("after_tree"):
        raise PrivacyError("git index changed after migration; automatic rollback refused")
    projection_after = (repo.state / "projection.after").read_bytes()
    exclude_after = (repo.state / "exclude.after").read_bytes()
    ignore_path = repo.root / ".gitignore"
    if not ignore_path.is_file() or ignore_path.read_bytes() != (repo.state / "ignore.after").read_bytes():
        raise PrivacyError("gitignore changed after migration; automatic rollback refused")
    if not projection_path.is_file() or projection_path.read_bytes() != projection_after:
        raise PrivacyError("generated projection changed after migration; automatic rollback refused")
    if not repo.exclude.is_file() or repo.exclude.read_bytes() != exclude_after:
        raise PrivacyError("git exclude file changed after migration; automatic rollback refused")
    if not settings_path.is_file() or settings_path.is_symlink():
        raise PrivacyError("settings.json is no longer a regular local file")

    index_before = (repo.state / "index.before").read_bytes()
    projection_before = (
        bool(state.get("projection_existed")),
        (repo.state / "projection.before").read_bytes() if state.get("projection_existed") else b"",
        int(state.get("projection_mode", 0o644)),
    )
    exclude_before = (
        bool(state.get("exclude_existed")),
        (repo.state / "exclude.before").read_bytes() if state.get("exclude_existed") else b"",
        int(state.get("exclude_mode", 0o644)),
    )
    ignore_before = (
        bool(state.get("ignore_existed")),
        (repo.state / "ignore.before").read_bytes() if state.get("ignore_existed") else b"",
        int(state.get("ignore_mode", 0o644)),
    )
    current_ignore = _file_snapshot(ignore_path)
    current_index = _file_snapshot(repo.index)
    current_projection = _file_snapshot(projection_path)
    current_exclude = _file_snapshot(repo.exclude)
    current_settings_mode = stat.S_IMODE(settings_path.stat().st_mode)
    try:
        _atomic_write(repo.index, index_before, current_index[2])
        _restore_file(projection_path, *projection_before)
        _restore_file(repo.exclude, *exclude_before)
        _restore_file(ignore_path, *ignore_before)
        os.chmod(settings_path, int(state.get("settings_mode", 0o600)))
    except Exception:
        _restore_file(repo.index, *current_index)
        _restore_file(projection_path, *current_projection)
        _restore_file(repo.exclude, *current_exclude)
        _restore_file(ignore_path, *current_ignore)
        os.chmod(settings_path, current_settings_mode)
        raise
    shutil.rmtree(repo.state)
    print("configuration migration rolled back")


def command_prepare(config_dir: Path) -> None:
    settings, _, mode = _read_settings(config_dir)
    if mode != 0o600:
        raise PrivacyError("settings.json must have mode 0600 before backup preparation")
    projection, secrets = _projection(settings, config_dir)
    projection_path = _validate_projection_destination(config_dir)
    repo = _repo(config_dir)
    _cleanup_committed_state(repo)
    _validate_private_ignored(repo)
    for path in PRIVATE_PATHS:
        code, output = _git_optional(repo, "ls-files", "-z", "--", path)
        if code != 0:
            raise PrivacyError("private path tracking check failed")
        if output:
            raise PrivacyError("private Claude data is still tracked; run migrate first")

    projection_before = _file_snapshot(projection_path)
    index_before = repo.index.read_bytes()
    exclude = repo.exclude.read_bytes() if repo.exclude.is_file() else b""
    index_temp = _temporary_index(repo)
    projection_applied = False
    index_applied = False
    try:
        _validate_worktree_allowlist(repo, secrets)
        _stage_allowlist(repo, index_temp)
        _stage_projection(repo, index_temp, projection)
        _validate_prepare_boundary(repo, index_temp)
        _validate_index_has_no_secrets(repo, index_temp, secrets)
        after_tree = _tree(repo, index_temp)
        _atomic_write(projection_path, projection, 0o644)
        projection_applied = True
        _replace_index(repo, index_temp)
        index_applied = True
        _update_state_after(repo, projection, exclude, after_tree)
    except Exception:
        if index_applied:
            _atomic_write(repo.index, index_before, 0o644)
        if projection_applied:
            _restore_file(projection_path, *projection_before)
        raise
    finally:
        if index_temp.exists():
            index_temp.unlink()
    print("safe backup index prepared; no commit was created")


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description="Keep live Claude settings local while backing up a secret-free projection.",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)
    for name, help_text in (
        ("project", "write config/settings.example.json without touching Git"),
        ("migrate", "untrack private config and stage a safe projection"),
        ("rollback", "undo an uncommitted migration"),
        ("prepare", "stage only the safe projection and backup allowlist"),
    ):
        command = subparsers.add_parser(name, help=help_text)
        command.add_argument(
            "--config-dir",
            help="Claude config repository (default: $CLAUDE_CONFIG_DIR or $HOME/.claude)",
        )
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    args = _parser().parse_args(argv)
    try:
        config_dir = _resolve_config_dir(args.config_dir)
        {
            "project": command_project,
            "migrate": command_migrate,
            "rollback": command_rollback,
            "prepare": command_prepare,
        }[args.command](config_dir)
    except PrivacyError as error:
        print(f"config privacy: {error}", file=sys.stderr)
        return 1
    except OSError:
        print("config privacy: local filesystem operation failed", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
