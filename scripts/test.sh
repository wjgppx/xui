#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
set -a
source .env.local
set +a
export TEST_DATABASE_URL=$DATABASE_URL
exec bash scripts/go.sh test -race -count=1 ./...
