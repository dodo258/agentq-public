#!/bin/sh
# Build only a committed, clean public source tree; outputs stay outside the checkout.
set -eu
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
[ -z "$(git -C "$root" status --porcelain -uall)" ] || { echo 'Commit the reviewed source before packaging.' >&2; exit 1; }
out=${1:-$(mktemp -d "${TMPDIR:-/tmp}/agentq-release.XXXXXX")}
python3 -I - "$root" "$out" <<'PY'
from pathlib import Path
import subprocess, sys, tempfile, tarfile, gzip, io, hashlib, re
root, out = map(Path, sys.argv[1:])
if out.exists() and (not out.is_dir() or any(out.iterdir())):
    raise SystemExit('Release output directory must be new or empty')
out.mkdir(parents=True, exist_ok=True)
version = (root/'VERSION').read_text().strip()
if not re.fullmatch(r'\d+\.\d+\.\d+', version):
    raise SystemExit('Invalid VERSION')
epoch = int(subprocess.check_output(['git','-C',str(root),'show','-s','--format=%ct','HEAD']))
installer = (root/'scripts/install.sh').read_text()
if installer.count('@VERSION@') != 1:
    raise SystemExit('Installer version placeholder drift')
(out/'install.sh').write_text(installer.replace('@VERSION@', version)); (out/'install.sh').chmod(0o755)
files = ['LICENSE','THIRD_PARTY_NOTICES.md','licenses/Go-LICENSE','licenses/Waza-LICENSE','COMPONENTS.json','README.md','README.en.md','docs/install.zh-CN.md','docs/install.en.md']
for arch in ['arm64','amd64']:
    name = f'agentq_{version}_darwin_{arch}'
    with tempfile.TemporaryDirectory(prefix='agentq-package-') as t:
        binary = Path(t)/'agentq'
        import os
        env = dict(os.environ, GOOS='darwin', GOARCH=arch, CGO_ENABLED='0')
        subprocess.run(['go','build','-trimpath','-buildvcs=false','-o',str(binary),'.'],cwd=root,env=env,check=True)
        if arch == 'arm64' and sys.platform == 'darwin':
            import platform
            if platform.machine() == 'arm64':
                got = subprocess.check_output([str(binary),'version'],text=True).strip()
                if got != f'agentq {version}': raise SystemExit('Binary VERSION drift')
        with (out/(name+'.tar.gz')).open('wb') as raw, gzip.GzipFile(filename='',mode='wb',fileobj=raw,mtime=epoch) as gz, tarfile.open(fileobj=gz,mode='w') as tf:
            for rel, src, mode in [('bin/agentq',binary,0o755)]+[(n,root/n,0o644) for n in files]:
                data=src.read_bytes(); info=tarfile.TarInfo(name+'/'+rel)
                info.size=len(data); info.mode=mode; info.mtime=epoch
                info.uid=info.gid=0; info.uname=info.gname=''
                tf.addfile(info,io.BytesIO(data))
source=subprocess.check_output(['git','-C',str(root),'archive','--format=tar','--prefix=agentq-'+version+'/','HEAD'])
with (out/f'agentq_{version}_source.tar.gz').open('wb') as raw, gzip.GzipFile(filename='',mode='wb',fileobj=raw,mtime=epoch) as gz: gz.write(source)
entries=[]
for p in sorted(out.iterdir()):
    if p.is_file(): entries.append(hashlib.sha256(p.read_bytes()).hexdigest()+'  '+p.name+'\n')
(out/'SHA256SUMS').write_text(''.join(entries))
print(out)
PY
