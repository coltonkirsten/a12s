# Image targets, included at the end of the root Makefile (so they never become the default goal).
# Build context is the repo root.
DOCKER                 ?= docker
SLIM_IMAGE             ?= a12s/slim:dev
BROWSER_IMAGE          ?= a12s/browser:dev
CONTROLLER_IMAGE       ?= a12s/controller:dev
BUNDLE_CLAUDE_VERSIONS ?= 2.1.280
BUNDLE_CODEX_VERSIONS  ?= 0.154.0

.PHONY: images image-slim image-browser image-controller bundles bundle-claude-code bundle-codex

images: image-slim image-browser image-controller

image-slim:
	$(DOCKER) build -f images/slim/Dockerfile -t $(SLIM_IMAGE) .

image-browser: image-slim
	$(DOCKER) build -f images/browser/Dockerfile --build-arg SLIM_IMAGE=$(SLIM_IMAGE) -t $(BROWSER_IMAGE) .

image-controller:
	$(DOCKER) build -f images/controller/Dockerfile -t $(CONTROLLER_IMAGE) .

bundles:
	@set -e; for v in $(BUNDLE_CLAUDE_VERSIONS); do $(MAKE) --no-print-directory bundle-claude-code VERSION=$$v; done
	@set -e; for v in $(BUNDLE_CODEX_VERSIONS); do $(MAKE) --no-print-directory bundle-codex VERSION=$$v; done

bundle-claude-code bundle-codex:
	@test -n "$(VERSION)" || { echo "usage: make $@ VERSION=x.y.z" >&2; exit 1; }
	$(DOCKER) build --build-arg VERSION=$(VERSION) -t a12s/$@:$(VERSION) images/bundles/$(@:bundle-%=%)

# Reference Services (services/): ordinary images, pinned like any other.
SVC_TAG ?= 0.1
.PHONY: services svc-cron svc-webhook svc-mcp svc-mcp-postgres
services: svc-cron svc-webhook svc-mcp svc-mcp-postgres

svc-cron svc-webhook:
	$(DOCKER) build -f services/go.Dockerfile --build-arg SERVICE=$@ -t a12s/$@:$(SVC_TAG) .

svc-mcp:
	$(DOCKER) build -f services/svc-mcp/Dockerfile -t a12s/svc-mcp:$(SVC_TAG) .

svc-mcp-postgres: svc-mcp
	$(DOCKER) build -f services/svc-mcp/postgres.Dockerfile --build-arg SVC_MCP_IMAGE=a12s/svc-mcp:$(SVC_TAG) -t a12s/svc-mcp-postgres:$(SVC_TAG) services/svc-mcp
