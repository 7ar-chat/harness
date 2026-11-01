# Security policy

## Reporting a vulnerability

Please report it privately: <https://github.com/7ar-chat/harness/security/advisories/new> (GitHub private vulnerability
reporting). Do not open a public issue or pull request for a vulnerability.

Include what you found, how to reproduce it, and what it lets an attacker do. This is a small project: reports are handled on a
best-effort basis, and a fix is released as a patch version with an advisory naming the reporter if they wish.

## What is in scope

The harness runs an agent that executes commands and calls tools on behalf of users, so the interesting boundaries are:

- **The sandbox**: anything that lets agent-run code reach the host, another user's sandbox or an internal service.
- **Server-side request forgery**: the guarded transport for user-supplied MCP servers (`features/mcp`, `SafeTransport`) and any other
  place the server fetches a URL a user chose.
- **Secrets**: stored connector tokens and marketplace keys (sealed with `HARNESS_SECRET_KEY`), GitHub App tokens, anything returned
  by an API that should not be.
- **Authentication and authorisation**: project roles, thread ownership, the OIDC session, the webhook signature check.
- **The agent factory**: ways to make an unattended run push to a protected branch, merge without the gate, touch protected paths,
  or exceed its budget. Prompt injection that bypasses the policy layer counts; a model simply being wrong does not.

## Supported versions

The latest tagged release and `main`. The modules are versioned separately (see `docs/releasing.md`); a fix is released for the
module that has the bug and for the modules that require it.

## If you run it

`docs/factory.md` ("Security") and `docs/marketplace.md` ("Server-side request forgery") describe what the code enforces and what it
leaves to you: network egress rules, branch protection on the repository the factory works on, and the secret key.
