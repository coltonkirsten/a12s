GO ?= go

.PHONY: build test vet fmt sdk-bundle check check-full release

build:
	$(GO) build ./...
	$(GO) build -o bin/a12s ./cmd/a12s

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

# The browser SDK the gateway serves at /a12s.js; `make check` checks it matches sdk/typescript.
sdk-bundle:
	cd sdk/typescript && npm ci && npm run build
	cp sdk/typescript/dist/a12s.js internal/gateway/web/a12s.js

# The fast gate, the same steps PR CI runs (.github/workflows/ci.yml): Go, the API fixtures, both SDKs. No Docker.
check:
	scripts/check.sh go sdk python

# Everything: the above plus computerd, Services, images and bundles (needs Docker). Run before release-affecting changes.
check-full:
	scripts/check.sh

# Cut a release from this machine: make release VERSION=x.y.z (see CONTRIBUTING.md#releasing).
release:
	@test -n "$(VERSION)" || { echo "usage: make release VERSION=x.y.z" >&2; exit 1; }
	scripts/release.sh $(VERSION)

# Image targets live in images/images.mk, included last so they never become the default goal.
include images/images.mk
