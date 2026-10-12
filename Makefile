GO ?= go

.PHONY: build test vet fmt sdk-bundle

build:
	$(GO) build ./...
	$(GO) build -o bin/a12s ./cmd/a12s

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

# The browser SDK the gateway serves at /a12s.js; CI checks it matches sdk/typescript.
sdk-bundle:
	cd sdk/typescript && npm ci && npm run build
	cp sdk/typescript/dist/a12s.js internal/gateway/web/a12s.js

# Image targets live in images/images.mk, included last so they never become the default goal.
include images/images.mk
