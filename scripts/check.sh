#!/usr/bin/env bash
# Local checks: `make check` (go sdk python: what PR CI runs), `make check-full` (all steps, needs Docker) or
# `scripts/check.sh <step>...` with steps go computerd sdk python services tiers controller bundles.
# Stops at the first failure. The Docker steps prune dangling images and build cache when they finish.
# Runs on macOS (bash 3.2 is enough) and Linux. CI (.github/workflows/ci.yml) runs each step as its own job and sets
# CHECK_DOCKER_CACHE=gha so image builds reuse GitHub's build cache; locally Docker's own layer cache does that.
set -euo pipefail
cd "$(dirname "$0")/.."
root=$PWD

all=(go computerd sdk python services tiers controller bundles)
steps=("$@")
[ ${#steps[@]} -gt 0 ] || steps=("${all[@]}")

BUNDLE_CLAUDE=$(sed -n "s/^BUNDLE_CLAUDE_VERSIONS *?= *\([^ ]*\).*/\1/p" images/images.mk)
BUNDLE_CODEX=$(sed -n "s/^BUNDLE_CODEX_VERSIONS *?= *\([^ ]*\).*/\1/p" images/images.mk)
PYTHONS=${CHECK_PYTHONS:-"3.10 3.14"}

current=""
started=$SECONDS
log=$(mktemp -d)
used_docker=0
cleanup() {
  local rc=$?
  if [ "$used_docker" = 1 ] && [ "${CHECK_KEEP_CACHE:-0}" != 1 ]; then
    echo "== prune: dangling images and build cache"
    docker image prune -f > /dev/null || true
    docker builder prune -f > /dev/null || true
  fi
  rm -rf "$log"
  if [ $rc -ne 0 ]; then
    echo
    echo "FAIL  ${current:-check} (exit $rc)"
  else
    echo
    echo "PASS  check: ${steps[*]} ($((SECONDS - started))s)"
  fi
}
trap cleanup EXIT

step() { current="$*"; echo "== $*"; }
need() { command -v "$1" > /dev/null || { echo "missing $1: $2" >&2; exit 1; }; }
# Quiet on success; the full output on failure.
q() { "$@" > "$log/out" 2>&1 || { cat "$log/out"; return 1; }; }

run_go() {
  need go "Go 1.26+ (CONTRIBUTING.md)"
  step "go build";  q go build ./...
  step "gofmt"
  out="$(gofmt -l .)"
  if [ -n "$out" ]; then echo "gofmt needed:"; echo "$out"; return 1; fi
  step "go vet";    q go vet ./...
  step "go test (includes the Go -> TS/Python API fixture drift check)"; q go test ./...
}

run_computerd() {
  need npm "Node 24+ and npm"
  step "computerd: npm ci";   (cd computerd && q npm ci)
  step "computerd: lint";     (cd computerd && q npm run lint)
  step "computerd: test";     (cd computerd && q npm test)
}

run_sdk() {
  need npm "Node 24+ and npm"
  step "sdk/typescript: npm ci"; (cd sdk/typescript && q npm ci)
  step "sdk/typescript: lint";   (cd sdk/typescript && q npm run lint)
  step "sdk/typescript: test (build, browser bundle, node --test)"; (cd sdk/typescript && q npm test)
  step "gateway embeds this SDK build"
  cmp -s sdk/typescript/dist/a12s.js internal/gateway/web/a12s.js \
    || { echo "internal/gateway/web/a12s.js is stale; run make sdk-bundle"; return 1; }
}

run_python() {
  need uv "uv (https://docs.astral.sh/uv/)"
  for py in $PYTHONS; do
    step "sdk/python $py: pytest";       (cd sdk/python && q uv run -q --python "$py" --extra dev pytest -q)
    step "sdk/python $py: mypy --strict"; (cd sdk/python && q uv run -q --python "$py" --extra dev mypy --strict a12s)
  done
}

# dbuild <cache-scope> <docker build args...>: docker build, or with CHECK_DOCKER_CACHE=gha (CI) a buildx build that
# reads and writes the GitHub Actions cache. Images stay in the local image store either way, so a later build can
# start FROM them (browser on slim, svc-mcp-postgres on svc-mcp).
dbuild() {
  local scope=$1; shift
  if [ "${CHECK_DOCKER_CACHE:-}" = gha ]; then
    docker buildx build --load --cache-from "type=gha,scope=$scope" --cache-to "type=gha,mode=max,scope=$scope" "$@"
  else
    docker build "$@"
  fi
}

docker_up() {
  need docker "Docker Desktop or Docker Engine"
  docker info > /dev/null 2>&1 || { echo "the Docker daemon is not running" >&2; return 1; }
  used_docker=1
}

run_services() {
  need node "Node 24+"
  step "svc-mcp against a stub MCP server"; q sh services/svc-mcp/test/run.sh
  docker_up
  step "reference Service images (as make services)"
  q dbuild svc-cron -f services/go.Dockerfile --build-arg SERVICE=svc-cron -t a12s/svc-cron:0.1 .
  q dbuild svc-webhook -f services/go.Dockerfile --build-arg SERVICE=svc-webhook -t a12s/svc-webhook:0.1 .
  q dbuild svc-mcp -f services/svc-mcp/Dockerfile -t a12s/svc-mcp:0.1 .
  q dbuild svc-mcp-postgres -f services/svc-mcp/postgres.Dockerfile --build-arg SVC_MCP_IMAGE=a12s/svc-mcp:0.1 \
    -t a12s/svc-mcp-postgres:0.1 services/svc-mcp
  step "Service manifests are in the images"
  for s in svc-cron svc-webhook svc-mcp svc-mcp-postgres; do
    docker run --rm --entrypoint cat "a12s/$s:0.1" /a12s/service.json | python3 -m json.tool > /dev/null
  done
  q docker run --rm --entrypoint ls a12s/svc-mcp-postgres:0.1 /opt/mcp/bin/mcp-server-postgres
}

run_tiers() {
  docker_up
  step "slim image";    q dbuild slim -f images/slim/Dockerfile -t a12s/slim:dev .
  step "browser image"; q dbuild browser -f images/browser/Dockerfile --build-arg SLIM_IMAGE=a12s/slim:dev -t a12s/browser:dev .
  local sandbox=(--user 1000:1000 --cap-drop ALL --security-opt no-new-privileges
    --security-opt seccomp=images/browser/seccomp.json --read-only --tmpfs /tmp
    --tmpfs /home/agent:uid=1000,gid=1000 -e HOME=/home/agent -w /home/agent
    --network none -v "$root/images/browser/test:/test:ro" --entrypoint node)
  step "Chromium sandbox (docs/images.md)"
  docker run --rm "${sandbox[@]}" a12s/browser:dev /test/render.mjs > "$log/render" 2>&1 || { cat "$log/render"; return 1; }
  grep -q 'hello from chromium' "$log/render" && grep -q 'adequately sandboxed' "$log/render" || { cat "$log/render"; return 1; }
  step "agent scripts can require playwright"
  docker run --rm --user 1000:1000 --network none --entrypoint node \
    -e NODE_PATH=/opt/a12s/playwright-mcp/node_modules -w /tmp \
    a12s/browser:dev -e "require('playwright'); console.log('playwright ok')" | grep -q 'playwright ok'
  step "Playwright MCP navigates"; q docker run --rm "${sandbox[@]}" a12s/browser:dev /test/mcp.mjs
}

run_controller() {
  docker_up
  step "controller image"; q dbuild controller -f images/controller/Dockerfile -t a12s/controller:dev .
  step "controller binary runs (expects a config error, not a loader error)"
  if docker run --rm a12s/controller:dev > "$log/ctl" 2>&1; then cat "$log/ctl"; echo "expected non-zero exit"; return 1; fi
  grep -q 'A12S_' "$log/ctl" || { cat "$log/ctl"; return 1; }
}

run_bundles() {
  docker_up
  need jq "jq"
  step "claude-code bundle $BUNDLE_CLAUDE"
  q dbuild bundle-claude-code --build-arg VERSION="$BUNDLE_CLAUDE" -t "a12s/bundle-claude-code:$BUNDLE_CLAUDE" images/bundles/claude-code
  step "codex bundle $BUNDLE_CODEX"
  q dbuild bundle-codex --build-arg VERSION="$BUNDLE_CODEX" -t "a12s/bundle-codex:$BUNDLE_CODEX" images/bundles/codex
  step "bundle conformance"
  q images/bundles/conformance.sh claude-code "$BUNDLE_CLAUDE" "a12s/bundle-claude-code:$BUNDLE_CLAUDE"
  q images/bundles/conformance.sh codex "$BUNDLE_CODEX" "a12s/bundle-codex:$BUNDLE_CODEX"
}

for s in "${steps[@]}"; do
  case " ${all[*]} " in *" $s "*) ;; *) echo "unknown step '$s' (steps: ${all[*]})" >&2; exit 2 ;; esac
done
for s in "${steps[@]}"; do "run_$s"; done
current=""
