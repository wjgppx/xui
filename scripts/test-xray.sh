#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
if [[ -f .env.local ]]; then
 set -a
 source .env.local
 set +a
fi
export TEST_DATABASE_URL=${TEST_DATABASE_URL:-${DATABASE_URL:?DATABASE_URL or TEST_DATABASE_URL required}}
export XUI_XRAY_BIN=${XUI_XRAY_BIN:-/workspace/shared/tools/xray-26.10.21/xray}
export XUI_MIHOMO_BIN=${XUI_MIHOMO_BIN:-/workspace/shared/tools/mihomo-1.19.15/mihomo}
exec bash scripts/go.sh test -race -count=1 -tags=xrayintegration -v ./internal/integration
