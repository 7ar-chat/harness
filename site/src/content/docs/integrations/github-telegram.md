---
title: GitHub and Telegram
description: "Connect team-mode projects to GitHub through a GitHub App, and let users talk to their assistant from a Telegram bot."
---

Team mode of `harness-web` has two optional connectors. A **GitHub App** gives each project a checkout of its repository in the sandbox and lets users import repositories. A **Telegram bot** lets a user talk to their assistant and answer approvals from a chat. Both need team mode (`-team`, see [Run team mode](/docs/guides/team-mode/)) and a [login](/docs/integrations/oidc/); the connector tokens are stored encrypted.

## Secret key

Connector tokens (a user's GitHub OAuth token, the linked Telegram chat) are kept in the store encrypted with AES-256-GCM. The key comes from `HARNESS_SECRET_KEY`, else the OIDC session key (`OIDC_SESSION_KEY`).

```bash
export HARNESS_SECRET_KEY=$(openssl rand -hex 32)
```

<div class="callout warning">

**No key, no connectors.** Without either key, `GET /api/connectors` reports both connectors as unavailable with `Connectors need a secret key on the server (HARNESS_SECRET_KEY).` If you change the key, tokens stored under the old one can no longer be read and users must link again.

</div>

## GitHub App

Create a GitHub App with repository **contents: read and write**, and **pull requests: read and write** if agents should open them. Give the server:

| Variable | Meaning |
|---|---|
| `GITHUB_APP_ID` | The App id. Setting it enables the App; the key is then required. |
| `GITHUB_APP_SLUG` | The App's URL name, used for the install link `https://github.com/apps/<slug>/installations/new`. |
| `GITHUB_APP_PRIVATE_KEY` | The PEM private key (PKCS#1 or PKCS#8, RSA). In an env file, put it on one line in double quotes with `\n` for the line breaks. |
| `GITHUB_APP_PRIVATE_KEY_FILE` | A path to the PEM. Wins over `GITHUB_APP_PRIVATE_KEY`. |
| `GITHUB_APP_CLIENT_ID`, `GITHUB_APP_CLIENT_SECRET` | The App's OAuth credentials, for linking a user's GitHub account (project import). |
| `HARNESS_PUBLIC_URL` | The address users reach the server at; the OAuth callback is `<public url>/auth/github/callback`. Default: derived from `OIDC_REDIRECT_URL`, else from the request. Set it behind a proxy. |

Startup errors: `GITHUB_APP_ID needs GITHUB_APP_PRIVATE_KEY or GITHUB_APP_PRIVATE_KEY_FILE`, `read GITHUB_APP_PRIVATE_KEY_FILE: ...`, and `github app: the private key is not PEM`.

Register `<public url>/auth/github/callback` as the App's callback URL when you use the OAuth credentials.

### Connect a project

1. A project owner sets the repository URL (`https://github.com/OWNER/REPO`, `ssh://git@github.com/...` and `git@github.com:o/r.git` are understood; only github.com repositories can be connected).
2. They install the App on that repository (`GET /api/projects/{id}/github` returns the install link).
3. They press *Check again*, which calls `POST /api/projects/{id}/github/connect`. The server finds the installation and stores it on the project.

Without an App, public repositories still clone. The status text says `This server has no GitHub App configured; public repositories can still be cloned.`

### Import from GitHub

A user links their GitHub account from the profile page (`GET /api/connectors/github/connect`, an OAuth round trip). `GET /api/connectors/github/repos` then lists the repositories they can reach through the App's installations, and `POST /api/projects/import {repo}` creates a project already connected to its installation. The server looks the repository up with the user's own token; it does not trust the client's word. This needs `GITHUB_APP_CLIENT_ID` and `GITHUB_APP_CLIENT_SECRET`: otherwise the connector says `The server has no GitHub App with OAuth credentials (GITHUB_APP_CLIENT_ID / GITHUB_APP_CLIENT_SECRET).`

### Clone and credentials

When a thread of a project gets its sandbox, the harness clones the repository into `/workspace/repo` on a branch `har/<first 8 characters of the thread id>`, so parallel agents never share a working tree. The default branch of the project is checked out first; the git identity defaults to `harness agent <agent@harness.local>`. The clone has a 5-minute limit; if it fails, the agent gets a short notice with the (redacted) error.

The clone and later pushes use a short-lived **installation token**, limited to the project's repository (an installation may cover many) and never a member's personal token. It is renewed at the start of each run. How it reaches git depends on the sandbox:

| Sandbox | What happens | Token inside the sandbox |
|---|---|---|
| A backend with a credential broker: OpenSandbox with `OPENSANDBOX_CREDENTIAL_PROXY=true` | The token goes to the egress sidecar's Credential Vault. Requests to `github.com` for the repository and to `api.github.com/repos/OWNER/REPO` are authenticated on the way out. The clone runs without any token. | no |
| Any other backend | The token is passed to the clone in an environment variable, then written to `/workspace/.work/git-credentials` (mode 0600) and set as git's `store` credential helper. It is never put on a command line. | yes, readable by anything in the sandbox |

<div class="callout warning">

**Prefer the credential proxy for untrusted code.** Without a broker, any command the agent runs can read the token. If a broker refuses the token, the checkout does not fall back to the file: a private repository then fails to clone. The broker needs a default-deny `SANDBOX_EGRESS_ALLOWLIST` that includes `github.com` and `api.github.com`. See [Sandbox backends](/docs/integrations/sandbox-backends/#credential-proxy).

</div>

With a broker, the agent's prompt says that `git push` and the GitHub API are already authenticated for this repository, so it opens pull requests with plain `curl https://api.github.com/repos/OWNER/REPO/pulls`.

## Telegram bot

Create a bot with [@BotFather](https://t.me/BotFather) and set:

| Variable | Meaning |
|---|---|
| `TELEGRAM_BOT_TOKEN` | The bot token. Setting it (in team mode) starts the bot. |
| `TELEGRAM_BOT_USERNAME` | The bot's name, with or without `@`. Optional: when empty it is read from Telegram. |

```bash
TELEGRAM_BOT_TOKEN=123456:ABC... TELEGRAM_BOT_USERNAME=my_harness_bot harness-web -team ...
```

A user links their chat from the profile page: the page shows a deep link with a one-time code (valid 10 minutes), and the bot's `/start <code>` ties that private chat to the account (`POST /api/connectors/telegram/link`). Only linked private chats are served.

In a linked chat:

- A message goes to the user's assistant (the supervisor): it steers the running assistant run, answers its pending question, or starts a new run. Its replies come back as messages.
- An approval the assistant waits for arrives with Approve and Deny buttons.
- `/status` lists the user's sessions; `/help` and a bare `/start` print a short hint.
- A session that waits for a user with Telegram linked produces a short note, unless the watcher is on, which hands it to the assistant instead.

The bot long-polls the Bot API (25-second polls), so no public webhook is needed and it works on a laptop. `DELETE /api/connectors/telegram` unlinks.

<div class="callout warning">

**Run the bot in one replica only.** Telegram allows one poller per token. With several `harness-web` replicas, set `TELEGRAM_BOT_TOKEN` on a single one; the others, which have no token, still publish their feed events to it through [Redis](/docs/integrations/redis/) so the bot hears them.

</div>

## Production example

`docker-compose.prod.yml` passes all of these from the deployment environment, empty by default:

```yaml
TELEGRAM_BOT_TOKEN: ${TELEGRAM_BOT_TOKEN:-}
TELEGRAM_BOT_USERNAME: ${TELEGRAM_BOT_USERNAME:-}
GITHUB_APP_ID: ${GITHUB_APP_ID:-}
GITHUB_APP_SLUG: ${GITHUB_APP_SLUG:-}
GITHUB_APP_CLIENT_ID: ${GITHUB_APP_CLIENT_ID:-}
GITHUB_APP_CLIENT_SECRET: ${GITHUB_APP_CLIENT_SECRET:-}
GITHUB_APP_PRIVATE_KEY: ${GITHUB_APP_PRIVATE_KEY:-}
OPENSANDBOX_CREDENTIAL_PROXY: "true"
```

That file runs one `harness-web` replica, which is what the Telegram bot requires. See [Production checklist](/docs/operations/production/).
