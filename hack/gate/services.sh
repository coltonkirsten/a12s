#!/usr/bin/env bash
# Builds the reference Services the enhancements exit gate runs (cron, webhook, MCP bridge, Postgres MCP) from the
# v0.2.1 source: they are no longer part of a12s, but they exercise every Service feature the gate checks.
set -euo pipefail
REPO=$(cd "$(dirname "$0")/../.." && pwd)
src=$(mktemp -d)
trap 'rm -rf "$src"' EXIT
git -C "$REPO" archive v0.2.1 go.mod go.sum services | tar -x -C "$src"
cd "$src"
for s in svc-cron svc-webhook; do docker build -q -f services/go.Dockerfile --build-arg SERVICE=$s -t a12s/$s:0.1 .; done
docker build -q -f services/svc-mcp/Dockerfile -t a12s/svc-mcp:0.1 .
docker build -q -f services/svc-mcp/postgres.Dockerfile --build-arg SVC_MCP_IMAGE=a12s/svc-mcp:0.1 -t a12s/svc-mcp-postgres:0.1 services/svc-mcp
