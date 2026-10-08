# The reference Services written in Go (svc-cron, svc-webhook): a static
# binary on Alpine, with the manifest at /a12s/service.json.
# Build context: repo root.  docker build -f services/go.Dockerfile --build-arg SERVICE=svc-cron -t a12s/svc-cron:0.1 .
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS
ARG TARGETARCH
ARG SERVICE
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY services/ services/
RUN --mount=type=cache,target=/root/.cache/go-build CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/svc ./services/$SERVICE

FROM alpine:latest
ARG SERVICE
COPY --from=build /out/svc /usr/local/bin/svc
COPY services/$SERVICE/service.json /a12s/service.json
USER 1000:1000
CMD ["/usr/local/bin/svc"]
