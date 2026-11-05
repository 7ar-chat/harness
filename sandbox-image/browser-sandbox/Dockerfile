# syntax=docker/dockerfile:1
# =============================================================================
# The browser of a browser sandbox (BROWSER_BACKEND=daytona|e2b|opensandbox): Steel (https://github.com/steel-dev/steel-browser,
# Apache-2.0) with the Playwright MCP server attached to Steel's Chromium. Steel owns the browser and its session
# (live viewer at /v1/sessions/debug, REST session API, CDP on the same port); Playwright MCP gives the agent its
# browser_* tools on that very browser, so what the agent does is what a person watching the viewer sees.
#
# Start it with `browser-up` (the harness does). Ports: 3000 (Steel: viewer, API, CDP), 8931 (MCP, at /mcp).
#
#   daytona snapshot create harness-browser --dockerfile sandbox-image/browser-sandbox/Dockerfile \
#       --context sandbox-image/browser-sandbox --cpu 2 --memory 4 --disk 8
#   e2b template create harness-browser --path sandbox-image/browser-sandbox --cpu-count 2 --memory-mb 4096
#
# The Steel base is the upstream API image pinned by digest, the same one as sandbox-image/steel.
# =============================================================================
# (Literal values, not build arguments: E2B's Dockerfile reader does not expand them.)
FROM ghcr.io/steel-dev/steel-browser-api@sha256:ba93b804d63586ea27a1db5b0f0719ff01f9428d55c56d0eee3e88e6a03a344c

# No browser download: the MCP server attaches to Steel's Chromium over CDP.
RUN PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1 npm install -g --no-fund --no-audit @playwright/mcp@0.0.84 \
    && rm -rf /root/.npm \
    && apt-get update && apt-get install -y --no-install-recommends curl ca-certificates procps fonts-noto-core fonts-noto-color-emoji \
    && rm -rf /var/lib/apt/lists/*

COPY browser-up /usr/local/bin/browser-up
# E2B runs commands as an unprivileged user (Daytona as root): these directories must be writable by both.
RUN chmod 0755 /usr/local/bin/browser-up && mkdir -p /data/logs /out /docs && chmod 1777 /data /data/logs /out /docs

# Replaces Steel's ENTRYPOINT (browser-up starts Steel itself, then the MCP server).
ENTRYPOINT []
EXPOSE 3000 8931
WORKDIR /out
