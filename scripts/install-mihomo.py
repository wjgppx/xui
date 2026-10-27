#!/usr/bin/env python3
"""Install a pinned official HTTPS Mihomo release for compatibility tests."""
from pathlib import Path
import gzip
import hashlib
import os
import platform
import urllib.request

if platform.system() != 'Linux' or platform.machine() != 'x86_64':
    raise SystemExit('Mihomo fixture installer supports Linux amd64')
root = Path(os.environ.get('XUI_TOOLS_DIR', '/workspace/shared/tools')) / 'mihomo-1.19.15'
root.mkdir(parents=True, exist_ok=True)
archive = root / 'mihomo-linux-amd64-v1-v1.19.15.gz'
if not archive.exists():
    urllib.request.urlretrieve('https://github.com/MetaCubeX/mihomo/releases/download/v1.19.15/' + archive.name, archive)
# Pinned from the official GitHub release asset fetched over verified HTTPS.
# This is a reproducibility checksum, not an upstream signature claim.
expected = 'c52a766af250f789fa8e979f8c6a1c279a526bf2c7dd5d6aaedef74db2f8f366'
if hashlib.sha256(archive.read_bytes()).hexdigest() != expected:
    raise SystemExit('Mihomo checksum mismatch; executable not used')
with gzip.open(archive, 'rb') as source:
    (root / 'mihomo').write_bytes(source.read())
(root / 'mihomo').chmod(0o755)
print('Pinned official Mihomo 1.19.15 installed')
