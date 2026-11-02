# Steel browser image

A Chromium the agent drives through [Steel](https://github.com/steel-dev/steel-browser) (Apache-2.0): a REST session API on
`:3000` (with a live viewer at `/ui`), CDP on `:9223`, page-to-markdown and screenshot actions, proxies per session.
The image is the upstream API image pinned by digest (release v0.5.5-beta, 2026-10-31) plus a health check and a data volume.

```sh
docker compose up -d              # builds harness-steel:0.5.5, binds 127.0.0.1:3000 and :9223
open http://localhost:3000/ui     # watch or take over a session
```

**Steel has no authentication.** Anyone who reaches `:3000` or `:9223` drives the browser and every site it is logged in to.
The compose file binds to loopback only; keep it that way, or put an authenticating proxy in front.

## Tested on 2026-11-02 (linux/arm64, Docker on macOS)

| | |
|---|---|
| Health, session create / release | works |
| `POST /v1/scrape` (markdown) and `POST /v1/screenshot` | work |
| Puppeteer (`puppeteer-core` 23) over CDP: navigate, set and read cookies | works |
| Cookies carried to a new session through the context API | works |
| localStorage carried through the context API | **not verified** (read back `null` in the test) |
| Login kept by `persist: true` or `userDataDir` | **does not work in 0.5.5**: nothing reaches disk, and `persist` ignores the given path |
| linux/amd64, a real logged-in site, Playwright | not tested |

## Keeping a login

Steel does not keep a profile. Log in once (by hand in `/ui`), then export the context and store it where the client can
find it; pass it as `sessionContext` when creating the next session:

```sh
curl -s localhost:3000/v1/sessions/<id>/context > context.json         # before releasing the session
curl -s -X POST localhost:3000/v1/sessions -H 'content-type: application/json' \
     -d "{\"sessionContext\": $(cat context.json)}"                    # the next session starts logged in
```

The context file holds session cookies: treat it like a password and keep it out of the repository and out of prompts.
Sites that keep their token in localStorage may need more than cookies; test the site before relying on it.

## Upgrading Steel

```sh
docker buildx imagetools inspect ghcr.io/steel-dev/steel-browser-api:latest   # pick the index digest, set STEEL_IMAGE
```
Upstream publishes only `latest` (no version tags), which is why the Dockerfile pins a digest.
