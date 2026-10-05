GO ?= go

.PHONY: build test vet fmt

build:
	$(GO) build ./...
	$(GO) build -o bin/a12s ./cmd/a12s

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

# Image targets live in images/images.mk, included last so they never become the default goal.
include images/images.mk
