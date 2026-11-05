# Browser and documents

The agent can drive a web browser and use the files a person keeps in their **document library**. Together they let it do
work like finding jobs and filling in application forms.

## The browser

`docker-compose.prod.yml` runs a second service, `browser` (`sandbox-image/browser/`): headless Chromium behind the
[Playwright MCP server](https://github.com/microsoft/playwright-mcp). `HARNESS_BROWSER_URL` (default `http://browser:8931/mcp`)
adds it to the agent's tools as the MCP server `browser`. Empty it to turn the browser off.

**Why it is a service and not part of the sandbox.** The sandbox's network is deny-by-default (Daytona tiers 1 and 2 allow
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

The browser service mounts the same volume read-only at `/docs` (`HARNESS_DOCUMENTS_BROWSER_ROOT`), so
`browser_file_upload` can attach a document to a form. Anyone who can run commands as the agent can read the documents of the
user whose conversation it is, and nobody else's; the browser path contains a hash, but the browser itself can open any path
under `/docs`, so treat the library as readable by the people running agents on your server.

## Configuration

| Variable | Default | |
|---|---|---|
| `HARNESS_BROWSER_URL` | `http://browser:8931/mcp` (production compose) | MCP endpoint of the browser service; empty: no browser |
| `HARNESS_DOCUMENTS_DIR` | `/data/documents` (production compose) | where the library is stored; empty: no library |
| `HARNESS_DOCUMENTS_BROWSER_ROOT` | `/docs` | where the browser service sees that directory |
