#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
set -a
source .env.local
set +a
export TEST_DATABASE_URL=$DATABASE_URL
# Database-backed packages share PostgreSQL. Keep package execution sequential;
# individual tests still exercise concurrent requests, workers and transactions.
exec bash scripts/go.sh test -race -count=1 -p 1 ./...
