#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
command -v gcc >/dev/null || { echo "GCC is required to build the Agent SQLite driver" >&2; exit 1; }
python3 scripts/install-go.py
if [[ "$(bash scripts/go.sh env CGO_ENABLED)" != "1" ]]; then
 echo "CGO_ENABLED=1 is required for Agent SQLite storage" >&2
 exit 1
fi
if ! bash scripts/go.sh mod download; then
 GOPROXY=direct bash scripts/go.sh mod download
fi
npm --prefix web ci --cache "$PWD/.local/npm-cache" --no-audit --no-fund
python3 scripts/init-dev.py
