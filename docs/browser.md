# Browser and documents

The agent can drive a web browser and use the files a person keeps in their **document library**. Together they let it do
work like finding jobs and filling in application forms.

## The browser

`docker-compose.prod.yml` runs a second service, `browser` (`sandbox-image/browser/`): headless Chromium behind the
[Playwright MCP server](https://github.com/microsoft/playwright-mcp). `HARNESS_BROWSER_URL` (default `http://browser:8931/mcp`)
adds it to the agent's tools as the MCP server `browser`. Empty it to turn the browser off.

### Choosing where the browser runs: `BROWSER_BACKEND`

The code sandboxes (`SANDBOX_BACKEND`) and the browser are chosen separately, so the code can run on Daytona while the
browser runs on E2B:

| `BROWSER_BACKEND` | The browser is |
|---|---|
| `service` (default) | the always-on container above, at `HARNESS_BROWSER_URL`. Its state does not carry from one message to the next |
| `daytona`, `e2b`, `opensandbox` | a sandbox on that provider, one per conversation, started on the first use and deleted after `BROWSER_IDLE_MINUTES` (20) without use. Image `BROWSER_IMAGE` (default `harness-browser`: a Daytona snapshot, an E2B template or an OpenSandbox image of that name), built from [`sandbox-image/browser-sandbox`](../sandbox-image/browser-sandbox/README.md) |
| `none` | no browser |

A browser sandbox runs [Steel](https://github.com/steel-dev/steel-browser) (its Chromium, session API and live viewer) with the Playwright MCP server attached to that Chromium, so what the agent does shows in Steel's viewer, and
the page, the cookies and a half-filled form are still there in the conversation's next message. Its user's documents are
copied to `/docs/<tag>/` (the path `documents_list` reports) when it starts and when the library changes. The provider's
access token (Daytona's preview token, E2B's traffic token) is sent with every MCP request; it never reaches the model.

Browser sandboxes get **open internet access** whatever `SANDBOX_EGRESS_ALLOWLIST` says for the code sandboxes (private and
link-local ranges stay denied), because a browser must reach any site. The provider must be able to expose a port of a
sandbox (Daytona preview links, E2B ports and OpenSandbox endpoints can). Daytona tiers 1 and 2 restrict sandbox internet
access to a fixed list of hosts, so there `BROWSER_BACKEND=daytona` cannot reach job sites; use `e2b` or the `service`.

The sandboxes are tracked in the memory of one harness process: with several replicas each keeps its own, and a restart
leaves the running ones to the provider's own sandbox timeout (`SANDBOX_TIMEOUT_SECONDS`).

**Why the default is a service and not part of the code sandbox.** The sandbox's network is deny-by-default (Daytona tiers 1 and 2 allow
package registries, Git hosts and AI APIs only), and a browser has to reach job boards. The MCP calls come from the harness
process, so the sandbox rules do not apply to them.

| | |
|---|---|
| Tools the agent gets | navigate, snapshot, click, type, fill form, select, key, hover, wait, tabs, screenshot, file upload, dialog, find, close (`config.BrowserTools`). `browser_run_code_unsafe`, `browser_evaluate`, the network log and the console are not offered |
| Session | one throw-away browser profile per run (`--isolated`): no cookies or logins carry over between runs, users or conversations |
| Network | the service is on its own network (`browser-net`); only `harness-web` joins it. A page cannot reach the database, the identity provider or the model router. It can reach the internet and the host's other private addresses; block those at the firewall if the host has services that trust its network |
| Container | all capabilities dropped, `no-new-privileges`, 3 GB, 2 CPUs, 512 processes |

What it cannot do: log in to a site for the user (LinkedIn, Indeed and similar ban automated sessions and use CAPTCHAs; the
agent is told to stop and hand over the link), solve a CAPTCHA, or keep a page open between two messages. That last point shapes
the job flow: the agent first writes the answers for each application as a draft and stops, and after the user approves it
opens the form again, fills it in and submits. **The "ask before submitting" rule is an instruction to the model, not a
technical gate.** Review what it says it submitted.

## The document library

`Settings > Documents` (or `PUT /api/me/documents/{name}`) stores files per user under `HARNESS_DOCUMENTS_DIR` (default
`/data/documents` in production, on the `harness-docs` volume): PDF, Word, text, spreadsheet, slides or image, up to 20 MB each,
50 files and 200 MB per user. Each user's files are in a directory named by a hash of the login.

The agent finds them three ways:

* the prompt names the user's documents;
* `documents_list` returns each file's path in the sandbox (`/workspace/documents/<name>`) and in the browser;
* `documents_to_workspace` copies them into the conversation's sandbox, to read, convert or edit.
* The agent also keeps the library up to date: `documents_save_from_url` downloads a file (a résumé linked on the user's website)
  and saves it; `documents_save_from_sandbox` stores an edited or generated file; `documents_write` saves text; `documents_rename`
  and `documents_delete` tidy up. Downloads are made by the harness, never to a private or loopback address, at most 20 MiB.

The browser service mounts the same volume read-only at `/docs` (`HARNESS_DOCUMENTS_BROWSER_ROOT`), so
`browser_file_upload` can attach a document to a form. Anyone who can run commands as the agent can read the documents of the
user whose conversation it is, and nobody else's; the browser path contains a hash, but the browser itself can open any path
under `/docs`, so treat the library as readable by the people running agents on your server.

## Configuration

| Variable | Default | |
|---|---|---|
| `HARNESS_BROWSER_URL` | `http://browser:8931/mcp` (production compose) | MCP endpoint of the browser service (`BROWSER_BACKEND=service`); empty: no browser |
| `BROWSER_BACKEND` | `service` | `service`, `daytona`, `e2b`, `opensandbox` or `none`: where the browser runs, independently of `SANDBOX_BACKEND` |
| `BROWSER_IMAGE` | `harness-browser` | image/template/snapshot of browser sandboxes on the provider named by `BROWSER_BACKEND` |
| `BROWSER_IDLE_MINUTES` | `20` | minutes a conversation's browser sandbox is kept after its last use |
| `HARNESS_DOCUMENTS_DIR` | `/data/documents` (production compose) | where the library is stored; empty: no library |
| `HARNESS_DOCUMENTS_BROWSER_ROOT` | `/docs` | where the browser service sees that directory |
