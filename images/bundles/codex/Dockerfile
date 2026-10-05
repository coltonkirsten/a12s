# HarnessBundle: OpenAI Codex CLI (static musl build), sha256-verified.
#   docker build --build-arg VERSION=0.154.0 -t a12s/bundle-codex:0.154.0 images/bundles/codex
# The result is FROM scratch: it is never run, only extracted by the Controller.
#
# The plain `codex-<triple>.tar.gz` asset is not listed in codex-package_SHA256SUMS (it only has a
# .sigstore bundle, which is out of scope here). The `codex-package-<triple>.tar.gz` asset IS listed,
# and its bin/codex is byte-identical to the plain asset (checked for 0.154.0), so we fetch the package
# and keep its layout (bin/, codex-resources/ with bwrap, codex-path/ with rg) so codex finds its helpers.
FROM --platform=$BUILDPLATFORM alpine:3.22 AS fetch
RUN apk add --no-cache curl jq
ARG VERSION
ARG TARGETARCH
# Hosts the harness needs to sign in / refresh / reach inference; the Controller adds them to egress.
ARG SIGNIN_HOSTS="api.openai.com chatgpt.com auth.openai.com"
WORKDIR /bundle
RUN set -eu; \
    test -n "${VERSION}" || { echo "VERSION build-arg is required" >&2; exit 1; }; \
    case "${TARGETARCH}" in arm64) triple=aarch64-unknown-linux-musl ;; amd64) triple=x86_64-unknown-linux-musl ;; \
      *) echo "unsupported TARGETARCH ${TARGETARCH}" >&2; exit 1 ;; esac; \
    rel="https://github.com/openai/codex/releases/download/rust-v${VERSION}"; \
    asset="codex-package-${triple}.tar.gz"; \
    cd /tmp; \
    curl -fsSLO "${rel}/${asset}"; \
    curl -fsSL "${rel}/codex-package_SHA256SUMS" -o SHA256SUMS; \
    grep -E "^[0-9a-f]{64}  ${asset}\$" SHA256SUMS > want; \
    test "$(wc -l < want)" -eq 1; \
    sha256sum -c want; \
    tar xzf "${asset}" -C /bundle --no-same-owner; \
    cd /bundle; \
    test "$(jq -er .version codex-package.json)" = "${VERSION}"; \
    test "$(jq -er .entrypoint codex-package.json)" = "bin/codex"; \
    jq -n --arg v "${VERSION}" --arg a "${TARGETARCH}" --arg h "${SIGNIN_HOSTS}" \
      '{kind: "codex", version: $v, arch: $a, bin: "bin/codex", signInHosts: ($h | split(" "))}' > bundle.json; \
    chmod -R a-w,a+rX /bundle

FROM scratch
COPY --from=fetch /bundle /bundle
