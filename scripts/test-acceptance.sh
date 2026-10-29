#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
if [[ -f .env.local ]]; then
 set -a
 source .env.local
 set +a
fi
export TEST_DATABASE_URL=${TEST_DATABASE_URL:-${DATABASE_URL:?TEST_DATABASE_URL or DATABASE_URL required}}
export XUI_ACCEPTANCE_DURATION=${XUI_ACCEPTANCE_DURATION:-10s}
export XUI_ACCEPTANCE_INTERVAL=${XUI_ACCEPTANCE_INTERVAL:-200ms}
export XUI_ACCEPTANCE_WORKERS=${XUI_ACCEPTANCE_WORKERS:-4}
umask 077
if [[ -z "${XUI_ACCEPTANCE_REPORT:-}" ]]; then
 mkdir -p .local/acceptance
 export XUI_ACCEPTANCE_REPORT="$PWD/.local/acceptance/usage-$(date -u +%Y%m%dT%H%M%SZ)-$$.json"
fi
acceptance_timeout=$(python3 - <<'PY'
import math, os, re, sys
match = re.fullmatch(r'([0-9]+(?:\.[0-9]+)?)(ms|s|m|h)', os.environ['XUI_ACCEPTANCE_DURATION'])
if not match:
 sys.exit('invalid acceptance duration: use one numeric ms/s/m/h unit')
seconds = float(match[1]) * {'ms': .001, 's': 1, 'm': 60, 'h': 3600}[match[2]]
if not 1 <= seconds <= 86400:
 sys.exit('acceptance duration must be between one second and 24 hours')
print(f'{math.ceil(seconds) + 120}s')
PY
)
exec bash scripts/go.sh test -race -count=1 -p 1 -tags=acceptance -timeout "$acceptance_timeout" -v ./internal/acceptance
