---
title: OIDC
description: "Log users in to harness-web team mode with any OpenID Connect provider: authorization code with PKCE, a signed session cookie, domain limits."
---

Team mode of `harness-web` authenticates people with OpenID Connect. The server runs the whole flow; the browser only ever holds an `HttpOnly` session cookie. Use this page to configure an identity provider (Dex for development, Keycloak, Google, Okta, ... in production). For what team mode does with the logins, see [Run team mode](/docs/guides/team-mode/).

## Minimal setup

```bash
OIDC_ISSUER=https://idp.example.com \
OIDC_CLIENT_ID=harness \
OIDC_CLIENT_SECRET=... \
OIDC_REDIRECT_URL=https://harness.example.com/auth/callback \
OIDC_SESSION_KEY=$(openssl rand -hex 32) \
POSTGRES_HOST=db \
harness-web -team -addr 0.0.0.0:8080
```

Register `https://harness.example.com/auth/callback` as the redirect URI of the client at your provider.

## Environment variables

These are read by `harness-web` (they are not part of `config.Load`).

| Variable | Default | Meaning |
|---|---|---|
| `OIDC_ISSUER` | none | Issuer URL; discovery is read from it. Setting it turns team mode on. Without it there is no OIDC. |
| `OIDC_CLIENT_ID` | none | Required with an issuer. |
| `OIDC_CLIENT_SECRET` | none | Client secret. |
| `OIDC_REDIRECT_URL` | none | Required with an issuer: `https://host/auth/callback`. If it starts with `https://`, cookies are marked `Secure`. |
| `OIDC_SCOPES` | `profile,email` | Comma-separated scopes besides `openid`. |
| `OIDC_LOGIN_CLAIM` | see below | The claim used as the user's login. |
| `OIDC_ALLOWED_DOMAINS` | none | Comma-separated email domains allowed to log in. |
| `OIDC_SESSION_KEY` | random per start | Signs the cookies. 16 bytes or more; use 32 random bytes. |
| `HARNESS_PUBLIC_URL` | derived | Public address for OAuth callbacks of the GitHub connector; see [GitHub and Telegram](/docs/integrations/github-telegram/). |
| `HARNESS_SECRET_KEY` | the session key | Encrypts connector tokens in the store. |

Startup errors, verbatim:

- `OIDC_ISSUER needs team mode (-team)` when `-team` is explicitly set to false.
- `OIDC_ISSUER needs OIDC_CLIENT_ID and OIDC_REDIRECT_URL (https://host/auth/callback)`.
- `httpserver: OIDC discovery: ...` when the issuer cannot be reached.

<div class="callout warning">

**Set `OIDC_SESSION_KEY`.** If it is empty or shorter than 16 bytes, the server generates a random key at start and logs `httpserver: no OIDC session key; sessions end when the server restarts`. With several replicas, every replica must share the same key, or a cookie signed by one is rejected by another.

</div>

## The flow

The server mounts three routes: `GET /auth/login`, `GET /auth/callback` and `POST /auth/logout`.

<svg class="diagram" viewBox="0 0 760 170" role="img" aria-label="Browser, harness-web and identity provider exchange for the authorization code flow with PKCE">
<rect class="box" x="20" y="20" width="150" height="46"/>
<text class="b m" x="95" y="48">Browser</text>
<rect class="box hot" x="305" y="20" width="150" height="46"/>
<text class="b m" x="380" y="48">harness-web</text>
<rect class="box" x="590" y="20" width="150" height="46"/>
<text class="b m" x="665" y="48">Identity provider</text>
<path class="arrow" d="M170 36 L300 36"/>
<path class="head" d="M300 36 l-8 -4 v8 z"/>
<text class="s" x="180" y="30">/auth/login</text>
<path class="arrow" d="M455 36 L585 36"/>
<path class="head" d="M585 36 l-8 -4 v8 z"/>
<text class="s" x="465" y="30">302 authorize + PKCE</text>
<path class="arrow" d="M590 98 L175 98"/>
<path class="head" d="M175 98 l8 -4 v8 z"/>
<text class="s" x="200" y="92">302 /auth/callback?code&amp;state</text>
<path class="arrow" d="M170 130 L300 130"/>
<path class="head" d="M300 130 l-8 -4 v8 z"/>
<text class="s" x="180" y="124">callback</text>
<path class="arrow" d="M455 130 L585 130"/>
<path class="head" d="M585 130 l-8 -4 v8 z"/>
<text class="s" x="465" y="124">code + verifier</text>
</svg>

1. `/auth/login` creates a random `state`, a `nonce` and a PKCE verifier, seals them with the optional `next` path in a short-lived cookie `harness_oidc_flow` (10 minutes), and redirects to the provider with an S256 code challenge. `next` is kept only if it is a local path; anything else becomes `/`.
2. The provider redirects to `/auth/callback`. The server checks the flow cookie, the `state`, exchanges the code with the PKCE verifier, verifies the ID token against the client ID and checks the `nonce`.
3. If the ID token has no `email` claim, the server also reads the provider's userinfo endpoint and fills in the missing claims.
4. It builds the user (below), records it in the team store, clears the flow cookie and sets `harness_session`, then redirects to `next`.

The session cookie is `HttpOnly`, `SameSite=Lax`, `Secure` when the redirect URL is `https`, signed with HMAC-SHA256, and holds the login and an expiry (12 hours). Because it is a cookie, event streams, which cannot set headers, authenticate too. `POST /auth/logout` clears it.

Failures end the flow with a JSON `login_failed` error and a plain message: `The login attempt expired; try again.`, `Invalid state.`, `The identity token is invalid.`, `The login could not be completed.`.

## Login claim

The login is the key of threads, memberships and memory, so pick a claim that never changes for a person.

- With `OIDC_LOGIN_CLAIM=name` (or any claim), that claim's value is used.
- Without it: `preferred_username`, else the email, else the `sub`. An email counts only if verified: when `email_verified` is `false` the email is dropped, so an unverified address cannot take over a login or claim project invites.
- If the chosen claim is empty, the `sub` is used as a last resort.
- The login is lower-cased; one containing whitespace or `/` is refused with `The identity provider returned no usable login.`.

The display name defaults to `given_name` and `family_name`, else `name`, else the login; the user's own choice survives later logins. The avatar URL comes from `picture`.

## Allowed domains

`OIDC_ALLOWED_DOMAINS=example.com,example.org` restricts logins to those email domains, compared case-insensitively against the verified email. Anyone else gets `This account is not allowed to use this server.` (HTTP 403). With the list set, an account without a verified email is refused.

## Development with Dex

`examples/dev-oidc/` runs a Dex provider with two users, `alice@example.com` and `bob@example.com`, both with the password `password`. Its `dex.yaml` has issuer `http://localhost:5556/dex`, one static client `harness` with secret `harness-dev-secret` and redirect URI `http://localhost:8080/auth/callback`, and an in-memory store.

```bash
examples/dev-oidc/run.sh
```

The script starts Dex in Docker (`dexidp/dex:v2.41.1` on port 5556), builds `harness-web` and runs it with:

```bash
OIDC_ISSUER=http://localhost:5556/dex OIDC_CLIENT_ID=harness OIDC_CLIENT_SECRET=harness-dev-secret \
OIDC_REDIRECT_URL=http://localhost:8080/auth/callback OIDC_LOGIN_CLAIM=name \
OIDC_SESSION_KEY=$(openssl rand -hex 32) \
./bin/harness-web -local -addr 127.0.0.1:8080
```

`OIDC_LOGIN_CLAIM=name` makes the logins `alice` and `bob`. The first login registers a user; projects can then be shared by invite or login.

<div class="callout note">

**`-local` together with OIDC.** `harness-web` refuses `SANDBOX_BACKEND=local` in team mode unless `HARNESS_ALLOW_LOCAL_SANDBOX=1`, and `-local` sets that backend. Export `HARNESS_ALLOW_LOCAL_SANDBOX=1` before running the script's last command by hand; the script does not set it.

</div>

`docker compose up --build` is the all-in-one variant: Postgres, Dex and `harness-web` on http://localhost:8080. `harness-web` shares Dex's network namespace so `localhost:5556` is the same issuer URL for your browser and the server (the issuer must match what the browser is redirected to). The compose file sets `HARNESS_ALLOW_LOCAL_SANDBOX=1` and a development session key; replace both.

## Production

`docker-compose.prod.yml` runs `harness-web -team` with these values from the deployment environment:

```yaml
HARNESS_PRODUCTION: "1"
HARNESS_PUBLIC_URL: https://${HARNESS_HOST}
OIDC_ISSUER: ${OIDC_ISSUER}
OIDC_CLIENT_ID: ${OIDC_CLIENT_ID}
OIDC_CLIENT_SECRET: ${OIDC_CLIENT_SECRET}
OIDC_REDIRECT_URL: https://${HARNESS_HOST}/auth/callback
OIDC_SESSION_KEY: ${OIDC_SESSION_KEY}      # openssl rand -hex 32
HARNESS_SECRET_KEY: ${HARNESS_SECRET_KEY}  # openssl rand -hex 32
```

The identity provider needs a client whose redirect URI is `https://<HARNESS_HOST>/auth/callback`. A public host name is fine behind OIDC; only a server with neither OIDC nor `HARNESS_WEB_TOKEN` is limited to loopback hosts. See [Production checklist](/docs/operations/production/) and [Security model](/docs/operations/security/).
