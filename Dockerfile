FROM --platform=$BUILDPLATFORM caddy:builder-alpine AS builder

ARG CADDY_VERSION=v2.11.4
ARG TARGETARCH

COPY . /src/caddy-pangolin

RUN GOARCH=${TARGETARCH} xcaddy build "${CADDY_VERSION}" \
    --output /caddy \
    --with github.com/abs3ntdev/caddy-pangolin=/src/caddy-pangolin \
    --with github.com/caddy-dns/cloudflare \
    --with github.com/mholt/caddy-ratelimit

FROM ghcr.io/hotio/caddy:latest

ARG BASE_DIGEST=unknown
LABEL org.opencontainers.image.source="https://github.com/abs3ntdev/caddy-pangolin" \
      io.abs3ntdev.base-digest="${BASE_DIGEST}"

COPY --from=builder /caddy /app/caddy
