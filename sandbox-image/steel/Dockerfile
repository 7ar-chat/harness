# syntax=docker/dockerfile:1
# =============================================================================
# Steel browser (https://github.com/steel-dev/steel-browser, Apache-2.0): a Chromium
# the agent drives through a REST session API (:3000) and CDP (:9223), with persistent
# profiles, proxies, a live session viewer and page-to-markdown / screenshot actions.
#
# This image is the upstream API image pinned by digest (the multi-arch index of the
# build published right after release v0.5.5-beta, 2026-10-31), plus a persistent
# data directory and a health check. To move to a newer Steel, pick a digest:
#   docker buildx imagetools inspect ghcr.io/steel-dev/steel-browser-api:latest
#
#   docker build -t harness-steel:0.5.5 .
#
# Run it with docker-compose.yml (ports bound to 127.0.0.1): Steel has no authentication,
# so anyone who reaches :3000 or :9223 controls the browser and every logged-in site in it.
# =============================================================================
ARG STEEL_IMAGE=ghcr.io/steel-dev/steel-browser-api@sha256:ba93b804d63586ea27a1db5b0f0719ff01f9428d55c56d0eee3e88e6a03a344c
FROM ${STEEL_IMAGE}

LABEL org.opencontainers.image.title="harness-steel" \
      org.opencontainers.image.description="Steel browser pinned for the harness browser tools" \
      org.opencontainers.image.source="https://github.com/steel-dev/steel-browser"

# /data holds the browser logs. Logins are NOT kept by a profile directory in this Steel
# version (tested: neither "persist" nor "userDataDir" writes cookies to disk, and
# "persist" ignores the path it is given). The supported way is the context API:
# GET /v1/sessions/<id>/context before releasing a session, and pass the result as
# "sessionContext" when creating the next one. The client (the harness) stores that file.
RUN mkdir -p /data/logs
VOLUME ["/data"]

HEALTHCHECK --interval=10s --timeout=5s --start-period=30s --retries=5 \
    CMD curl -fsS http://localhost:3000/v1/health || exit 1
