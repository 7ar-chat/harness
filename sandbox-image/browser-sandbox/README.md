# Browser sandbox image (Steel)

The image behind `BROWSER_BACKEND=daytona|e2b|opensandbox`: [Steel](https://github.com/steel-dev/steel-browser) (Apache-2.0)
with the Playwright MCP server attached to Steel's Chromium. Steel owns the browser and its session (REST API, CDP and a live
session viewer, all on port 3000); Playwright MCP (port 8931, `/mcp`) gives the agent its `browser_*` tools on that same
browser. The base is the upstream Steel API image pinned by digest, like [`../steel`](../steel/README.md).

The harness starts one sandbox per conversation by itself (it runs `browser-up`, waits for `/tmp/browser.ready`, connects to
port 8931), so none of the commands below is needed to use it; they build the image on a provider:

```sh
# Daytona (builds remotely)
daytona snapshot create harness-browser --dockerfile sandbox-image/browser-sandbox/Dockerfile \
    --context sandbox-image/browser-sandbox --cpu 2 --memory 4 --disk 8
# E2B
e2b template create harness-browser --path sandbox-image/browser-sandbox --cpu-count 2 --memory-mb 4096
# OpenSandbox / local Docker
docker build -t harness-browser sandbox-image/browser-sandbox
```

The name is `BROWSER_IMAGE` (default `harness-browser`). Run it by hand with
`docker run --shm-size 1g -p 3000:3000 -p 8931:8931 --entrypoint sh harness-browser -c 'browser-up'`.

## Notes

* **Viewer:** `/v1/sessions/debug` on port 3000 is Steel's "Session Player". The `/ui` the standalone [`../steel`](../steel/README.md)
  README mentions is not in this API image (404). Whether the viewer lets a person click in the page is **not tested**.
* **Chromium's sandbox is off** (`DISABLE_CHROME_SANDBOX`): as an unprivileged user (E2B) it cannot start otherwise. The provider's
  sandbox is the isolation boundary, so run nothing else in it.
* **No login survives the sandbox.** Steel keeps no profile on disk (see `../steel`); a browser sandbox is deleted after
  `BROWSER_IDLE_MINUTES`. Keeping a login would mean exporting and restoring Steel's session context.
* **Nothing has a password.** Ports are reachable only through the provider's access (Daytona's preview token, E2B's traffic
  token). Port 3000 is Steel's whole API, so the viewer link gives control of the browser: show it only to people who may open
  the conversation.

## Tested on 2026-11-05

| | |
|---|---|
| `docker build` (linux/arm64) | works |
| `browser-up` as an unprivileged user (uid 1000), ready in a few seconds | works |
| MCP `browser_navigate` and `browser_snapshot` through the published port: github.com/about loads in Steel's Chromium | works |
| `/v1/sessions/debug` answers (200) and the Steel session is live | works |
| E2B template `harness-browser` (linux/amd64, 2 CPU / 4 GB): a sandbox started by the harness's manager in 33 s | works |
| On E2B through the provider's address: MCP navigation to example.com and news.ycombinator.com (open internet), viewer answers 200, the port answers 403 without the traffic token | works |
| Daytona snapshot | rebuilt from this Dockerfile; Daytona tiers 1 and 2 still restrict the network |
| linux/amd64 local build | not tested (the E2B build is amd64) |
| Viewer in a browser, take-over by a person | **not tested** |
