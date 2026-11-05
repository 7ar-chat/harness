# syntax=docker/dockerfile:1
# =============================================================================
# The browser service: a Chromium the agent drives through the Playwright MCP server (@playwright/mcp). It is not a
# sandbox profile. It runs as its own service next to harness-web (docker-compose.prod.yml), because the sandbox's
# network is deny-by-default and a browser has to reach arbitrary sites. The harness process connects to it over the
# compose network (MCP_SERVERS), so nothing here is published to the internet.
#
# Pages are untrusted input to the agent: the agent reads what a site shows, never what a site tells it to do.
# =============================================================================
ARG PLAYWRIGHT_MCP_VERSION=0.0.84
FROM node:22-bookworm-slim

ARG PLAYWRIGHT_MCP_VERSION
ENV PLAYWRIGHT_BROWSERS_PATH=/ms-playwright \
    NODE_ENV=production

# --with-deps installs the shared libraries Chromium needs; fonts so pages (and CJK/Arabic job ads) render.
RUN npm install -g --no-fund --no-audit @playwright/mcp@${PLAYWRIGHT_MCP_VERSION} \
    && npx --yes playwright@$(node -p "require('/usr/local/lib/node_modules/@playwright/mcp/package.json').dependencies.playwright || require('/usr/local/lib/node_modules/@playwright/mcp/package.json').dependencies['playwright-core']" | sed 's/[^0-9.a-z-]//g') install --with-deps chromium \
    && apt-get install -y --no-install-recommends fonts-liberation fonts-noto-core fonts-noto-color-emoji curl \
    && rm -rf /var/lib/apt/lists/* /root/.npm

RUN useradd -m -u 10001 browser && mkdir -p /out /docs && chown browser /out
USER browser
WORKDIR /out

EXPOSE 8931
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s CMD curl -s -o /dev/null -m 4 http://127.0.0.1:8931/ || exit 1

# --isolated: each MCP session gets its own throw-away profile (no cookies shared between users or conversations).
# --allowed-hosts '*': the service is only reachable on the compose network; the Host header is "browser:8931".
# --allow-unrestricted-file-access: the file chooser may read /docs, the users' documents (mounted read-only).
# --no-sandbox: Chromium's own sandbox needs privileges the container does not have; the container is the sandbox.
ENTRYPOINT ["playwright-mcp", "--headless", "--browser", "chromium", "--no-sandbox", "--isolated", \
            "--host", "0.0.0.0", "--port", "8931", "--allowed-hosts", "*", \
            "--allow-unrestricted-file-access", "--output-dir", "/out", \
            "--viewport-size", "1366x900", "--timeout-action", "10000", "--timeout-navigation", "45000"]
