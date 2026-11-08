# Pinned to the builder's platform so multi-arch images cross-compile with Go
# instead of running the whole toolchain under QEMU.
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine3.24 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal

# -s -w drops DWARF/symbol tables, which is
# most of the binary's size and is of no use in the runtime image.
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags="-s -w" -o /iptv-proxy ./cmd/iptv-proxy

FROM alpine:3.24
RUN apk add --no-cache ca-certificates
COPY --from=build /iptv-proxy /iptv-proxy
# Unprivileged by default. docker-compose.yml overrides this with the host
# user that owns config.yaml (mode 600), so the mounted file stays readable.
USER 65532:65532
ENTRYPOINT ["/iptv-proxy", "-config", "/config/config.yaml"]
