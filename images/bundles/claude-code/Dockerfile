# HarnessBundle: Claude Code native binary, sha256-verified against the release manifest.
#   docker build --build-arg VERSION=2.1.280 -t a12s/bundle-claude-code:2.1.280 images/bundles/claude-code
# The result is FROM scratch: it is never run, only extracted by the Controller.
# Not verified here: the manifest's own signature (manifest.json.sig); TLS to downloads.claude.ai is the trust root.
FROM --platform=$BUILDPLATFORM alpine:3.22 AS fetch
RUN apk add --no-cache curl jq
ARG VERSION
ARG TARGETARCH
# Hosts the harness needs to sign in / refresh / reach inference; the Controller adds them to egress.
ARG SIGNIN_HOSTS="api.anthropic.com platform.claude.com console.anthropic.com claude.ai statsig.anthropic.com"
ARG BASE=https://downloads.claude.ai/claude-code-releases
WORKDIR /bundle
RUN set -eu; \
    test -n "${VERSION}" || { echo "VERSION build-arg is required" >&2; exit 1; }; \
    case "${TARGETARCH}" in arm64) plat=linux-arm64 ;; amd64) plat=linux-x64 ;; \
      *) echo "unsupported TARGETARCH ${TARGETARCH}" >&2; exit 1 ;; esac; \
    curl -fsSL "${BASE}/${VERSION}/manifest.json" -o /tmp/manifest.json; \
    test "$(jq -er .version /tmp/manifest.json)" = "${VERSION}"; \
    sum="$(jq -er --arg p "${plat}" '.platforms[$p].checksum' /tmp/manifest.json)"; \
    mkdir bin; \
    curl -fsSL "${BASE}/${VERSION}/${plat}/claude" -o bin/claude; \
    echo "${sum}  bin/claude" | sha256sum -c -; \
    chmod 0555 bin/claude; \
    jq -n --arg v "${VERSION}" --arg a "${TARGETARCH}" --arg h "${SIGNIN_HOSTS}" \
      '{kind: "claude-code", version: $v, arch: $a, bin: "bin/claude", signInHosts: ($h | split(" "))}' > bundle.json

FROM scratch
COPY --from=fetch /bundle /bundle
