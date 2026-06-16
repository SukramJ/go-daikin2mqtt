# SPDX-License-Identifier: MIT
# Multi-stage build for go-daikin2mqtt.
#
# CGO is disabled so the resulting binary is statically linked against
# Go's net package — that means the runtime image can be distroless,
# carrying no shell, no package manager and no userland to attack.
#
# characteristics.yaml is copied alongside the binary (the operator-editable
# ONECTA characteristic catalog is deliberately NOT embedded) so it can be
# overridden via a bind-mount without rebuilding.

# ---------- Stage 1: build ----------
FROM golang:1.26-alpine AS builder
WORKDIR /src

# Cache go.mod / go.sum separately so unrelated source edits don't
# bust the dependency-download layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

ENV CGO_ENABLED=0
RUN go build -trimpath \
      -ldflags="-s -w \
        -X github.com/SukramJ/go-daikin2mqtt/internal/version.Version=${VERSION} \
        -X github.com/SukramJ/go-daikin2mqtt/internal/version.Commit=${COMMIT} \
        -X github.com/SukramJ/go-daikin2mqtt/internal/version.BuildDate=${BUILD_DATE}" \
      -o /out/daikin2mqtt ./cmd/daikin2mqtt && \
    go build -trimpath \
      -ldflags="-s -w \
        -X github.com/SukramJ/go-daikin2mqtt/internal/version.Version=${VERSION} \
        -X github.com/SukramJ/go-daikin2mqtt/internal/version.Commit=${COMMIT} \
        -X github.com/SukramJ/go-daikin2mqtt/internal/version.BuildDate=${BUILD_DATE}" \
      -o /out/daikin2mqtt-util ./cmd/daikin2mqtt-util

# ---------- Stage 2: runtime ----------
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app

# Binaries + YAML assets — characteristics.yaml lives next to the binary
# so the daemon's locator picks it up via os.Executable.
COPY --from=builder /out/daikin2mqtt /out/daikin2mqtt-util /app/
COPY --from=builder /src/characteristics.yaml /src/config-template.yaml /app/

# /config is the canonical mount point for the operator's config.yaml.
# XDG_CONFIG_HOME steers config.Locate at the mount so a `docker run -v
# ./my-config:/config:ro` Just Works.
VOLUME ["/config"]
ENV XDG_CONFIG_HOME=/config

# Diagnostic web UI / OAuth callback. When enabled set WEB_BIND to
# 0.0.0.0:8080 (the 127.0.0.1 default is unreachable from outside the
# container) and publish the port with `docker run -p 8080:8080`.
EXPOSE 8080

USER nonroot:nonroot
ENTRYPOINT ["/app/daikin2mqtt"]
