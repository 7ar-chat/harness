# Capabilities: the MCP marketplace

MCP servers can come from the database, not only from `MCP_SERVERS` and `.mcp.json`. The product ships a **catalog**
of servers; a project owner enables them for the project (filling in an API key where one is needed) or adds a custom
server of their own; every user can do the same for their personal sessions. The page is the **Capabilities** tab of a
project and the Capabilities card of the profile.

## What a run gets

| Thread | Servers |
|---|---|
| a project thread | the project's: the catalog as the project configured it, plus its custom servers |
| a personal thread | the user's own, configured the same way |

The two are never mixed. A member's personal keys do not reach a session the whole project shares, and a project's keys
do not follow a member into their personal sessions. Servers of `MCP_SERVERS`, `.mcp.json` and the config files still apply
to every run, as before; a marketplace server whose name is already taken by one of those is skipped with a warning.

A catalog entry is **on by default** when its fixture says so and it needs no key (today: Exa). A scope can turn it off, and
can turn on one that is off. An entry whose required field is empty is never on, whatever the switch says.

## The catalog

`product.Catalog()` (`app/product/catalog.go`) is the fixture. It is seeded into the database at startup
(`SeedCatalog`: new entries added, changed ones updated, shipped entries that were removed from the fixture are deleted;
installs that point at a removed entry stay, are shown as orphans and are never active). Adding a server to the product
is one entry in that file; the test `TestCatalogFixture` checks every entry (https URL, valid name, header placeholders
that name real fields, no required field on a default-on entry).

| Entry | URL | Key | Default |
|---|---|---|---|
| `exa` | `https://mcp.exa.ai/mcp` | optional `api_key` (header `x-api-key`) | on |
| `context7` | `https://mcp.context7.com/mcp` | optional | off |
| `deepwiki` | `https://mcp.deepwiki.com/mcp` | none | off |
| `github` | `https://api.githubcopilot.com/mcp/` | required `token` (`Authorization: Bearer`) | off |

An entry's URL is fixed by the catalog, so enabling one never lets a user point the server somewhere else.

## Which tools a server may use

A server a user supplied is **untrusted**, and its tools are allow-listed:

* the install's own list, if the owner set one, else the catalog entry's default list;
* with neither, **only the tools the server marks read-only** (`readOnlyHint`). A custom server's "send message" tool is not
  offered until an owner names it.

The factory's policy treats an MCP tool as a network call, which `sandboxed` and `autonomous` allow unattended: the allow-list is
what keeps an unattended run from using a write tool nobody switched on.

## Secrets

Keys are sealed with AES-256-GCM before they are stored (`HARNESS_SECRET_KEY`, else the OIDC session key), under a different
derived key than the connector tokens, so one kind of sealed value never opens as the other. The API never returns a secret
or a header value: a field is reported as set or not, a custom server lists its header **names**. Every process that
reads or writes them (`harness-web`, each worker) must have the same key. If it changes, the affected servers are skipped
with a warning and a write to them is refused (`secrets_unreadable`) until the keys are entered again.

## Server-side request forgery

The MCP calls come from the harness process, not from the sandbox, so the sandbox's egress rules do not cover them. A user-supplied
URL could name the router, OpenSandbox or the database. Three layers:

1. **When saving**: `https` only, no user name or password, no IP literal in a blocked range. This gives a clear early error.
2. **When connecting** (the one that matters): `features/mcp.SafeTransport` resolves the name once, refuses if any answer is not a plain
   public unicast address, dials the checked address itself, and checks the connected address again. DNS rebinding therefore
   cannot change the destination. Blocked: loopback, private (RFC 1918, `fc00::/7`), link-local including cloud metadata,
   CGNAT, multicast, unspecified, reserved and benchmarking ranges, and the IPv6 forms that embed an IPv4 address (IPv4-mapped,
   IPv4-compatible, NAT64, 6to4, Teredo). It ignores the proxy environment and follows at most 3 redirects, never from https to http.
3. **Outside the code**: run `harness-web` where it cannot reach what it should not (a network without Postgres or the sandbox
   manager's admin port). The code check is not a substitute for that.

A refusal and a timeout look the same to the caller (`could not connect`), so the test button cannot be used to map the network.

## API

All under `/api`, JSON. Reads: any member of the project. Writes and the test: project owners (`/projects/{pid}/capabilities`)
or the user themself (`/me/capabilities`). Changes to a project are audited (`capability.enable`, `.disable`, `.add`, `.remove`).

```
GET    /projects/{pid}/capabilities          GET    /me/capabilities
PUT    /projects/{pid}/capabilities/{name}   PUT    /me/capabilities/{name}     {enabled?, fields?, headers?, tools?, url?, transport?}
POST   /projects/{pid}/capabilities          POST   /me/capabilities            {name, url, transport?, headers?, tools?, enabled?}
DELETE /projects/{pid}/capabilities/{name}   DELETE /me/capabilities/{name}     (a catalog entry goes back to its default)
POST   /projects/{pid}/capabilities/{name}/test      POST /me/capabilities/{name}/test    -> {ok, error?, tools?[{name, description, readOnly}]}
```

Filling in a field does not enable the server: send `enabled: true` as well. At most 20 custom servers per scope, 8 headers each.

## Setting it up

Nothing to set. With `POSTGRES_HOST` the catalog and installs live in Postgres (migration `0500_capabilities.sql`); without it
`harness-web` keeps them in `capabilities.json` next to `team.json`. `harness-worker` shares the Postgres ones and has no marketplace
without it. Stdio servers are not offered: nothing here starts a process.
