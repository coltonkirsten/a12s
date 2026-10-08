#!/usr/bin/env bash
# HarnessBundle conformance: conformance.sh <kind> <version> <image>
# Extracts the bundle into a Debian container (as the Controller would, at
# /opt/a12s/bundles/<kind>-<version>), runs it as uid 1000 and checks the
# harness still offers what computerd's adapters rely on.
# Set DOCKER_DEFAULT_PLATFORM (e.g. linux/arm64) to test another architecture.
# Needs docker and jq on the host. Exits non-zero on any failed check.
set -euo pipefail
[ $# -eq 3 ] || { echo "usage: $0 <claude-code|codex> <version> <image>" >&2; exit 2; }
kind=$1 version=$2 image=$3
dir=/opt/a12s/bundles/$kind-$version
name=a12s-test-conformance-$$
fail=0
pass() { echo "PASS  $*"; }
bad()  { echo "FAIL  $*"; fail=1; }

src=$(docker create "$image" /x)
trap 'docker rm -f "$src" "$name" >/dev/null 2>&1 || true' EXIT
docker run -d -q --name "$name" debian:bookworm-slim sleep 600 >/dev/null
docker exec "$name" sh -c "mkdir -p $dir /home/agent && chown 1000:1000 /home/agent"
docker cp "$src:/bundle/." - | docker exec -i "$name" tar x -C "$dir" --strip-components=1
run() { docker exec -u 1000:1000 -e HOME=/home/agent -e CODEX_HOME=/home/agent/.codex -w /home/agent "$name" "$@"; }

meta=$(docker exec "$name" cat "$dir/bundle.json")
bin=$(jq -r .bin <<<"$meta")
[ "$(jq -r .kind <<<"$meta")" = "$kind" ] && pass "bundle.json kind=$kind" || bad "bundle.json kind: $(jq -r .kind <<<"$meta")"
[ "$(jq -r .version <<<"$meta")" = "$version" ] && pass "bundle.json version=$version" || bad "bundle.json version: $(jq -r .version <<<"$meta")"
[ "$(jq '.signInHosts | length' <<<"$meta")" -gt 0 ] && pass "bundle.json signInHosts: $(jq -c .signInHosts <<<"$meta")" || bad "bundle.json signInHosts empty"
echo "      arch=$(jq -r .arch <<<"$meta") bin=$bin"

out=$(run "$dir/$bin" --version 2>/dev/null) || true
grep -qF "$version" <<<"$out" && pass "--version: $out" || bad "--version printed: $out"

has() { grep -qE -- "(^|[^A-Za-z0-9/_-])$1([^A-Za-z0-9/_-]|\$)" <<<"$2"; }
case $kind in
claude-code)
  help=$(run "$dir/$bin" --help 2>&1) || true
  for f in --session-id --resume --output-format --input-format --permission-mode --permission-prompts \
           --setting-sources --mcp-config --include-partial-messages --replay-user-messages; do
    has "$f" "$help" && pass "claude --help lists $f" || bad "claude --help lacks $f"
  done ;;
codex)
  run sh -c "mkdir -p /home/agent/.codex /tmp/schema && $dir/$bin app-server generate-json-schema --out /tmp/schema >/dev/null 2>&1" \
    || bad "codex app-server generate-json-schema failed"
  schema=$(docker exec "$name" sh -c 'cat /tmp/schema/*.json' 2>/dev/null || true)
  for m in initialize initialized thread/start thread/resume turn/start turn/interrupt \
           item/agentMessage/delta item/started item/completed turn/completed thread/tokenUsage/updated \
           item/commandExecution/requestApproval item/fileChange/requestApproval item/permissions/requestApproval \
           item/tool/requestUserInput mcpServer/elicitation/request; do
    grep -qF "\"$m\"" <<<"$schema" && pass "app-server schema has $m" || bad "app-server schema lacks $m"
  done ;;
*) bad "unknown kind $kind" ;;
esac

[ $fail -eq 0 ] && echo "OK    $kind $version ($image)" || echo "NOT CONFORMANT  $kind $version ($image)"
exit $fail
