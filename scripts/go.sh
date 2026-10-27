#!/usr/bin/env bash
set -euo pipefail
repo_dir=$(cd "$(dirname "$0")/.." && pwd)
if [[ -n "${XUI_GO_BIN:-}" ]]; then
  go_bin=$XUI_GO_BIN
elif [[ -x /workspace/shared/tools/go1.27.3/go/bin/go ]]; then
  go_bin=/workspace/shared/tools/go1.27.3/go/bin/go
else
  go_bin=$(command -v go)
fi
export GOCACHE=${GOCACHE:-$repo_dir/.local/go-build}
export GOPATH=${GOPATH:-$repo_dir/.local/go}
export GOTOOLCHAIN=local
exec "$go_bin" "$@"
