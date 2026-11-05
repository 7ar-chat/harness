# Browser for a Daytona sandbox

A headed Chromium on a virtual display that a person can watch and take over (noVNC, port 6080) and that the agent drives
(Playwright MCP, port 8931, attached to the same Chromium over CDP). It is the on-demand alternative to the always-on
`browser/` service: start it when a task needs a browser, delete it when done.

```sh
# once: build the snapshot on Daytona
daytona snapshot create harness-browser --dockerfile sandbox-image/browser-desktop/Dockerfile \
    --context sandbox-image/browser-desktop --cpu 2 --memory 4 --disk 8

# per use
daytona sandbox create --snapshot harness-browser --name my-browser
daytona exec my-browser -- sh -c 'setsid browser-up >/tmp/browser.log 2>&1 &'
# live view (a Daytona preview link; it carries a token):   port 6080, path /vnc.html?autoconnect=1&resize=scale
# the agent's MCP endpoint:                                  port 8931, path /mcp
```

`browser-up` options (environment): `SCREEN` (default `1366x900x24`), `START_URL`, `PROFILE_DIR` (default
`/data/profile`; put a Daytona volume there and a login survives the sandbox).

## Tested on 2026-11-05 (Daytona eu, snapshot `harness-browser`, 2 CPU / 4 GB)

| | |
|---|---|
| Snapshot builds on Daytona (1.58 GB) | works |
| `browser-up` starts Xvfb, openbox, Chromium, x11vnc, websockify and playwright-mcp; ready in under a minute | works |
| noVNC `/vnc.html` and the MCP endpoint `/mcp` through signed Daytona preview links (`daytona preview-url <sandbox> -p <port>`) | work |
| MCP `browser_navigate` moves the same Chromium tab the VNC view shows (page loaded: github.com) | works |
| Looking at the noVNC view in a browser, clicking in it, a persistent volume at `/data/profile` | **not tested** |
| Any site other than GitHub | **not reachable** on this account's tier (below) |

A preview link is a capability: it carries the access token in its host name, so whoever has it can drive the browser.
Create them with a short `--expires` and do not paste them into prompts or logs.

## Network: the account tier decides what a browser can reach

Daytona's tiers 1 and 2 allow only a fixed list of hosts (package registries, Git hosts, AI APIs). A browser there reaches
GitHub and little else (checked on this account on 2026-11-05: github.com answers, example.com, google.com and
news.ycombinator.com do not). Browsing job sites needs a tier whose sandboxes have open internet (3 or 4).
The image itself does not depend on the tier.

## Safety

* Nothing in the image has a password. Port 6080 and 8931 are reachable only through Daytona preview links, which carry a
  token (do not make the sandbox `--public`). Whoever holds the link drives the browser and every site it is logged in to.
* x11vnc listens on 127.0.0.1 only; websockify is the single way in.
* Chromium runs with `--no-sandbox` as root: the Daytona sandbox is the isolation boundary, so do not run anything else in it.
