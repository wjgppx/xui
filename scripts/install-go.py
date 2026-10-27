#!/usr/bin/env python3
"""Install the pinned official Linux amd64 toolchain, preserving TLS and SHA256 checks."""
import hashlib
import os
from pathlib import Path
import platform
import subprocess
import tarfile
import urllib.request

version = '1.27.3'
if os.environ.get('XUI_GO_BIN'):
    result = subprocess.check_output([os.environ['XUI_GO_BIN'], 'version'], text=True)
    if f'go{version} ' not in result:
        raise SystemExit('XUI_GO_BIN must point to the pinned Go version')
    print('Using explicitly configured Go toolchain')
    raise SystemExit(0)
if platform.system() != 'Linux' or platform.machine() != 'x86_64':
    raise SystemExit('Install Go 1.27.3 for this platform and set XUI_GO_BIN before continuing')
root = Path('/workspace/shared/tools')
target = root / f'go{version}'
if (target / 'go/bin/go').exists():
    result = subprocess.check_output([str(target / 'go/bin/go'), 'version'], text=True)
    if f'go{version} ' in result:
        print('Pinned Go toolchain already available')
        raise SystemExit(0)
root.mkdir(parents=True, exist_ok=True)
archive = root / f'go{version}.linux-amd64.tar.gz'
if not archive.exists():
    urllib.request.urlretrieve(f'https://dl.google.com/go/{archive.name}', archive)
expected = '65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40'
if hashlib.sha256(archive.read_bytes()).hexdigest() != expected:
    raise SystemExit('Go archive checksum mismatch; no installation performed')
target.mkdir(exist_ok=True)
with tarfile.open(archive) as tar:
    tar.extractall(target, filter='data')
print('Verified official Go toolchain installed')
