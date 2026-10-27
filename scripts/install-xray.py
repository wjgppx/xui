#!/usr/bin/env python3
"""Install a pinned official Xray release for local capability tests."""
from pathlib import Path
import hashlib
import os
import platform
import urllib.request
import zipfile

if platform.system() != 'Linux' or platform.machine() != 'x86_64':
    raise SystemExit('This installer supports Linux amd64; use an independently verified matching release on other platforms')
root = Path(os.environ.get('XUI_TOOLS_DIR', '/workspace/shared/tools')) / 'xray-26.10.21'
root.mkdir(parents=True, exist_ok=True)
archive = root / 'Xray-linux-64.zip'
if not archive.exists():
    urllib.request.urlretrieve('https://github.com/XTLS/Xray-core/releases/download/v26.10.21/Xray-linux-64.zip', archive)
expected = '0eec3ed4163c273d8c8bf821f7d219e257921cf584aa3fb970715e458e631978'
if hashlib.sha256(archive.read_bytes()).hexdigest() != expected:
    raise SystemExit('Xray checksum mismatch; no executable used')
with zipfile.ZipFile(archive) as z:
    z.extractall(root)
(root / 'xray').chmod(0o755)
print('Verified Xray 26.10.21 installed')
