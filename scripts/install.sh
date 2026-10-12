#!/bin/sh
# Install a checksum-verified release through the same setup engine as source installs.
set -eu
VERSION='@VERSION@'
BASE="https://github.com/dodo258/agentq-public/releases/download/v$VERSION"
case "$(uname -s)/$(uname -m)" in
 Darwin/arm64) arch=arm64 ;;
 Darwin/x86_64) arch=amd64 ;;
 *) echo 'This release supports macOS arm64/amd64 only.' >&2; exit 1 ;;
esac
for tool in curl python3 git shasum; do
 command -v "$tool" >/dev/null 2>&1 || { echo "Required tool missing: $tool" >&2; exit 1; }
done
tmp=$(mktemp -d "${TMPDIR:-/tmp}/agentq-download.XXXXXX")
trap 'rm -rf "$tmp"' EXIT HUP INT TERM
archive="agentq_${VERSION}_darwin_${arch}.tar.gz"
curl --fail --location --silent --show-error --retry 2 "$BASE/$archive" -o "$tmp/$archive"
curl --fail --location --silent --show-error --retry 2 "$BASE/SHA256SUMS" -o "$tmp/SHA256SUMS"
awk -v name="$archive" '$2 == name { print; n++ } END { if (n != 1) exit 1 }' "$tmp/SHA256SUMS" > "$tmp/expected.sha256"
(cd "$tmp" && shasum -a 256 -c expected.sha256)
python3 -I - "$tmp/$archive" "$tmp" <<'PY'
import pathlib, sys, tarfile
archive, dest = sys.argv[1:]
with tarfile.open(archive, 'r:gz') as tf:
    members = tf.getmembers()
    if not members:
        raise SystemExit('Empty release archive')
    for m in members:
        p = pathlib.PurePosixPath(m.name)
        if p.is_absolute() or '..' in p.parts or not (m.isfile() or m.isdir()):
            raise SystemExit('Unsafe release archive member')
    tf.extractall(dest, members=members)
PY
"$tmp/agentq_${VERSION}_darwin_${arch}/bin/agentq" setup --yes "$@"
