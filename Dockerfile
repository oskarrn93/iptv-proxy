FROM golang:1.27.1-alpine3.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./

# -s -w drops DWARF/symbol tables, which is
# most of the binary's size and is of no use in the runtime image.
RUN  CGO_ENABLED=0 go build -ldflags="-s -w" -o /iptv-proxy .

FROM alpine:3.24
RUN apk add --no-cache ca-certificates
COPY --from=build /iptv-proxy /iptv-proxy
ENTRYPOINT ["/iptv-proxy", "-config", "/config/config.yaml"]
