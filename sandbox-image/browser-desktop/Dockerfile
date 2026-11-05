# syntax=docker/dockerfile:1
# =============================================================================
# A visible browser for a Daytona sandbox: headed Chromium on a virtual display, viewable and drivable by a person
# through noVNC (:6080), and driven by the agent through the Playwright MCP server (:8931) attached to that same
# Chromium over CDP. What the agent clicks is what the person sees, and the person can click too (log in, solve a
# CAPTCHA) and hand the page back.
#
# It is a Daytona snapshot, not a compose service (see browser-desktop/README.md):
#   daytona snapshot create harness-browser --dockerfile sandbox-image/browser-desktop/Dockerfile \
#       --context sandbox-image/browser-desktop --cpu 2 --memory 4 --disk 8
#   daytona sandbox create --snapshot harness-browser      # then run `browser-up` in it
#
# Nothing here has a password: the ports are only reachable through Daytona's preview links, which require a token.
# =============================================================================
ARG PLAYWRIGHT_MCP_VERSION=0.0.84
FROM node:22-bookworm-slim

ARG PLAYWRIGHT_MCP_VERSION
ENV PLAYWRIGHT_BROWSERS_PATH=/ms-playwright \
    DISPLAY=:99 \
    NODE_ENV=production

RUN apt-get update && apt-get install -y --no-install-recommends \
        xvfb x11vnc novnc websockify openbox x11-utils xdotool \
        fonts-liberation fonts-noto-core fonts-noto-color-emoji \
        curl ca-certificates procps \
    && rm -rf /var/lib/apt/lists/*

# Same Chromium build the MCP server expects (playwright pinned to the version @playwright/mcp depends on).
RUN npm install -g --no-fund --no-audit @playwright/mcp@${PLAYWRIGHT_MCP_VERSION} \
    && npx --yes playwright@$(node -p "const d=require('/usr/local/lib/node_modules/@playwright/mcp/package.json').dependencies; (d.playwright||d['playwright-core'])" | sed 's/[^0-9.a-z-]//g') install --with-deps chromium \
    && rm -rf /var/lib/apt/lists/* /root/.npm \
    && chmod -R a+rX /ms-playwright

COPY browser-up /usr/local/bin/browser-up
RUN chmod 0755 /usr/local/bin/browser-up && mkdir -p /data/profile /out /docs

# The sandbox runs as root in Daytona unless told otherwise; Chromium then needs --no-sandbox (browser-up passes it:
# the Daytona sandbox is the isolation boundary).
EXPOSE 6080 8931
WORKDIR /out
