#!/bin/sh
# Exercises svc-mcp end to end with a fake stdio server. Usage: sh services/svc-mcp/test/run.sh
set -e
d=$(mktemp -d); trap 'kill $pid 2>/dev/null; rm -rf "$d"' EXIT
here=$(cd "$(dirname "$0")" && pwd)
mkdir -p "$d/secrets"; printf 'pw123\n' > "$d/secrets/postgres"
cat > "$d/config.json" <<JSON
{"command": ["node", "$here/fake-mcp.mjs"], "args": ["postgresql://postgres@\${PGHOST}:\${PGPORT}/postgres"],
 "envFrom": {"PGPASSWORD": "postgres"}, "hostPortFrom": {"A12S_SERVICE_PG_DB": ["PGHOST", "PGPORT"]}}
JSON
A12S_CONFIG="$d/config.json" A12S_SECRETS="$d/secrets" A12S_SERVICE_PG_DB=127.0.0.1:41233 SVC_MCP_PORT=18931 node "$here/../server.mjs" & pid=$!
sleep 0.5
h=$(curl -s -D - -o /dev/null -X POST localhost:18931/mcp -H 'content-type: application/json' -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' | tr -d '\r' | awk -F': ' 'tolower($1)=="mcp-session-id"{print $2}')
test -n "$h"
test "$(curl -s -o /dev/null -w '%{http_code}' -X POST localhost:18931/mcp -H "mcp-session-id: $h" -d '{"jsonrpc":"2.0","method":"notifications/initialized"}')" = 202
out=$(curl -s -X POST localhost:18931/mcp -H "mcp-session-id: $h" -d '{"jsonrpc":"2.0","id":2,"method":"tools/list"}')
echo "$out" | grep -q '127.0.0.1:41233 pw=pw123 arg=postgresql://postgres@127.0.0.1:41233/postgres'
test "$(curl -s -o /dev/null -w '%{http_code}' -X POST localhost:18931/mcp -d '{"jsonrpc":"2.0","id":3,"method":"tools/list"}')" = 400
echo "svc-mcp ok"
