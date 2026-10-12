#!/usr/bin/env bash
# Cut a release from a workstation: scripts/release.sh x.y.z (or make release VERSION=x.y.z).
# Tags origin/main as vX.Y.Z, pushes every image to ghcr.io/coltonkirsten/a12s/<name>:<version> (amd64 + arm64),
# publishes the default harness bundles if missing, runs GoReleaser (binaries + Homebrew formula), attaches the SDK
# tgz/whl to the GitHub release, then publishes to npm / PyPI when NPM_TOKEN / PYPI_TOKEN are set.
# Binaries go after the images so a released binary never points at images that are not there yet.
set -euo pipefail

version=${1:-}
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$ ]] || { echo "usage: $0 x.y.z" >&2; exit 2; }
tag="v$version"
REPO=coltonkirsten/a12s
REGISTRY=ghcr.io/coltonkirsten/a12s
SOURCE=https://github.com/$REPO

cd "$(dirname "$0")/.."
src=$PWD
die() { echo "release: $*" >&2; exit 1; }
step() { echo; echo "== $*"; }

# --- preflight: everything that can fail without side effects fails here ---------------------------------------
step "preflight"
problems=()
for t in git gh docker go jq npm uv; do
  command -v "$t" > /dev/null || problems+=("missing $t")
done
command -v goreleaser > /dev/null || problems+=("goreleaser: brew install goreleaser")
docker info > /dev/null 2>&1 || problems+=("the Docker daemon is not running")
if ! gh auth status > /dev/null 2>&1; then
  problems+=("gh is not logged in. Fix: gh auth login")
else
  scopes=$(gh api -i user 2>/dev/null | tr -d '\r' | awk -F': ' 'tolower($1)=="x-oauth-scopes"{print $2}')
  case ",${scopes// /}," in
    *,write:packages,*) ;;
    *) problems+=("the gh token cannot push to ghcr.io (scopes: ${scopes:-none}). Fix: gh auth refresh -s write:packages,read:packages") ;;
  esac
fi
# goreleaser pushes the formula to the tap over SSH; the key is a path or the key itself.
if [ -z "${HOMEBREW_TAP_DEPLOY_KEY:-}" ]; then
  for k in ~/.ssh/id_ed25519 ~/.ssh/id_rsa; do [ -f "$k" ] && { HOMEBREW_TAP_DEPLOY_KEY=$k; break; }; done
fi
[ -n "${HOMEBREW_TAP_DEPLOY_KEY:-}" ] || problems+=("set HOMEBREW_TAP_DEPLOY_KEY to an SSH key with write access to coltonkirsten/homebrew-tap")
if [ ${#problems[@]} -gt 0 ]; then
  printf 'release: %s\n' "${problems[@]}" >&2
  exit 1
fi
export HOMEBREW_TAP_DEPLOY_KEY
GITHUB_TOKEN=$(gh auth token); export GITHUB_TOKEN
gh_user=$(gh api user -q .login)

git fetch -q origin main --tags
git rev-parse -q --verify "refs/tags/$tag" > /dev/null && die "$tag already exists"
gh release view "$tag" -R "$REPO" > /dev/null 2>&1 && die "a GitHub release $tag already exists"
[ -n "${NPM_TOKEN:-}" ]  || echo "warning: NPM_TOKEN is not set; the npm publish of a12s will be skipped"
[ -n "${PYPI_TOKEN:-}" ] || echo "warning: PYPI_TOKEN is not set; the PyPI publish of a12s will be skipped"

# Build from a clean checkout of origin/main, never the working tree.
work=$(mktemp -d)
tree=$work/a12s
git worktree add -q --detach "$tree" origin/main
cleanup() {
  git -C "$src" worktree remove --force "$tree" > /dev/null 2>&1 || true
  rm -rf "$work"
  docker image prune -f > /dev/null 2>&1 || true
  docker builder prune -f > /dev/null 2>&1 || true
}
trap cleanup EXIT
cd "$tree"
commit=$(git rev-parse --short HEAD)
echo "releasing $tag at origin/main $commit as $gh_user"

gh auth token | docker login ghcr.io -u "$gh_user" --password-stdin > /dev/null

# --- images ---------------------------------------------------------------------------------------------------
build() { # name dockerfile context [build-args...]
  local name="$1" file="$2" ctx="$3"; shift 3
  local args=()
  for a in "$@"; do args+=(--build-arg "$a"); done
  step "image $name"
  docker buildx build --platform linux/amd64,linux/arm64 --push \
    --label "org.opencontainers.image.source=$SOURCE" \
    --label "org.opencontainers.image.version=$version" \
    "${args[@]}" -f "$file" -t "$REGISTRY/$name:$version" "$ctx"
}
build slim images/slim/Dockerfile . "A12S_VERSION=$tag"
build browser images/browser/Dockerfile . "SLIM_IMAGE=$REGISTRY/slim:$version"
build controller images/controller/Dockerfile . "A12S_VERSION=$tag" "A12S_REGISTRY=$REGISTRY"
build svc-cron services/go.Dockerfile . SERVICE=svc-cron
build svc-webhook services/go.Dockerfile . SERVICE=svc-webhook
build svc-mcp services/svc-mcp/Dockerfile .
build svc-mcp-postgres services/svc-mcp/postgres.Dockerfile services/svc-mcp "SVC_MCP_IMAGE=$REGISTRY/svc-mcp:$version"

step "computer images carry the computerd label"
for img in slim browser; do
  got=$(docker buildx imagetools inspect "$REGISTRY/$img:$version" --format '{{ json .Image }}' \
    | jq -r '.["linux/amd64"].config.Labels["ai.a12s.computerd.version"]')
  echo "$img: $got"; [ "$got" = "$tag" ] || die "$img carries computerd version '$got', want $tag"
done

# --- default harness bundles (what `a12s init` writes must resolve) ------------------------------------------
claude=$(sed -n "s/^BUNDLE_CLAUDE_VERSIONS *?= *\([^ ]*\).*/\1/p" images/images.mk)
codex=$(sed -n "s/^BUNDLE_CODEX_VERSIONS *?= *\([^ ]*\).*/\1/p" images/images.mk)
for kv in "claude-code=$claude" "codex=$codex"; do
  kind="${kv%%=*}" v="${kv#*=}"
  step "bundle $kind $v"
  if docker manifest inspect "$REGISTRY/bundle-$kind:$v" > /dev/null 2>&1; then echo "already published"; continue; fi
  docker buildx build --platform linux/amd64,linux/arm64 --push --build-arg VERSION="$v" \
    --label "org.opencontainers.image.source=$SOURCE" -t "$REGISTRY/bundle-$kind:$v" "images/bundles/$kind"
done

# --- tag, binaries and the Homebrew formula -------------------------------------------------------------------
step "tag $tag"
git tag -a "$tag" -m "$tag"
git push -q origin "$tag"
step "goreleaser"
goreleaser release --clean

# --- SDK packages on the GitHub release ----------------------------------------------------------------------
out=$work/dist
mkdir -p "$out"
step "npm package"
(cd sdk/typescript && npm ci --silent && npm version --no-git-tag-version --allow-same-version "$version" > /dev/null \
  && npm run build && npm pack --pack-destination "$out")
step "Python wheel"
(cd sdk/python && perl -pi -e "s/^version = .*/version = \"$version\"/" pyproject.toml && uv build --wheel --out-dir "$out")
step "attach SDK packages to the release"
gh release upload "$tag" -R "$REPO" "$out"/a12s-*.tgz "$out"/a12s-*.whl --clobber

# --- registries (optional) -----------------------------------------------------------------------------------
if [ -n "${NPM_TOKEN:-}" ]; then
  step "npm publish"
  printf '//registry.npmjs.org/:_authToken=%s\n' "$NPM_TOKEN" > "$work/npmrc"
  (cd sdk/typescript && npm test && NPM_CONFIG_USERCONFIG="$work/npmrc" npm publish --access public)
else
  echo "warning: NPM_TOKEN is not set; skipped the npm publish of a12s"
fi
if [ -n "${PYPI_TOKEN:-}" ]; then
  step "PyPI publish"
  (cd sdk/python && uv run -q --extra dev pytest -q && uv run -q --extra dev mypy --strict a12s \
    && rm -rf dist && uv build && uv publish --token "$PYPI_TOKEN")
else
  echo "warning: PYPI_TOKEN is not set; skipped the PyPI publish of a12s"
fi

echo
echo "released $tag: https://github.com/$REPO/releases/tag/$tag"
