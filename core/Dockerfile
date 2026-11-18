# gavety-vision-core: all Gavety Vision logic (cameras, Frigate lifecycle, storage limits, AI).
# Multi-arch (amd64 + arm64 for the Raspberry Pi). Pure Go, no CGO.
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
COPY ui ./ui
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/gavety-vision-core .

FROM alpine:3.20
# docker-cli: the core starts/stops Frigate through the HOST's Docker daemon (via the mounted socket).
RUN apk add --no-cache docker-cli ca-certificates tzdata
COPY --from=build /out/gavety-vision-core /usr/local/bin/gavety-vision-core
# Runs as root on purpose: the Docker socket is root-equivalent anyway. The API only listens on
# 127.0.0.1 and needs a bearer token (GAVETY_CORE_TOKEN).
ENV GAVETY_CORE_ADDR=127.0.0.1:5160
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- http://127.0.0.1:5160/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/gavety-vision-core"]
