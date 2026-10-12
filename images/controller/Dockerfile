# a12s Controller: the `a12s` binary run as `a12s controller`.
# Build context: repo root.  docker build -f images/controller/Dockerfile -t a12s/controller:dev .
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS
ARG TARGETARCH
# A release passes both so the Controller resolves the images published with it.
ARG A12S_VERSION=dev
ARG A12S_REGISTRY=a12s
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
# The Go build cache is a cache mount, so the layer holds only the binary.
RUN --mount=type=cache,target=/root/.cache/go-build CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X github.com/coltonkirsten/a12s/internal/version.Version=${A12S_VERSION} -X github.com/coltonkirsten/a12s/internal/version.Registry=${A12S_REGISTRY}" -o /out/a12s ./cmd/a12s

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/a12s /usr/local/bin/a12s
ENTRYPOINT ["a12s", "controller"]
