# syntax=docker/dockerfile:1

## Build stage
# Pinned to a digest for supply-chain safety; the readable tag documents the
# version. Renovate keeps both the tag and the digest up to date (see docs/ci.md).
FROM golang:1.26-alpine@sha256:ce864e7223ac17b1775e6fd0b4c0db580c2eb50e7953a427916379e4b92a1628 AS builder

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build \
    -ldflags="-s -w -X github.com/mkende/screenshotter_slack_bridge/internal/version.Version=${VERSION}" \
    -o screenshotter-slack-bridge ./cmd/screenshotter-slack-bridge

## Runtime stage
FROM alpine:3.21@sha256:48b0309ca019d89d40f670aa1bc06e426dc0931948452e8491e3d65087abc07d

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app
COPY --from=builder /build/screenshotter-slack-bridge /app/screenshotter-slack-bridge

# Mount your config.toml at /app/config.toml (or provide the tokens via env
# vars). There are no inbound ports to expose: Socket Mode and image uploads
# are both outbound connections.

ENTRYPOINT ["/app/screenshotter-slack-bridge"]
