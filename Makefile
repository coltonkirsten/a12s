GO ?= go

.PHONY: build test vet fmt check check-full release

build:
	$(GO) build ./...
	$(GO) build -o bin/a12s ./cmd/a12s

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

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
