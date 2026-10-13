GO ?= go

.PHONY: build test vet fmt dashboard check check-full release

build:
	$(GO) build ./...
	$(GO) build -o bin/a12s ./cmd/a12s

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

# The gateway's built-in dashboard: internal/gateway/dashboard/main.js bundled with the SDK source. `make check` checks
# the embedded copy matches this build.
dashboard:
	cd sdk/typescript && npm ci && npm run build
	cp sdk/typescript/dist/dashboard.js internal/gateway/web/dashboard.js

# Fast local feedback: Go, the API fixtures, both SDKs. No Docker. PR CI (.github/workflows/ci.yml) is the gate.
check:
	scripts/check.sh go sdk python

# Everything PR CI can run: the above plus computerd, Services, images and bundles (needs Docker).
check-full:
	scripts/check.sh

# Emergency fallback: releases normally come from a v* tag (release.yml). make release VERSION=x.y.z, see CONTRIBUTING.md.
release:
	@test -n "$(VERSION)" || { echo "usage: make release VERSION=x.y.z" >&2; exit 1; }
	scripts/release.sh $(VERSION)

# Image targets live in images/images.mk, included last so they never become the default goal.
include images/images.mk
