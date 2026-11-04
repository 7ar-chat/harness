---
name: dokploy
description: Inspect and redeploy services on the user's Dokploy server (projects, compose stacks, applications, containers, deployments, domains). Use when the user asks about their deployments, why a service is down, or to redeploy one.
connect:
  fields:
    - {key: url, label: "Dokploy URL", type: url, help: "The address of your Dokploy panel, for example https://dokploy.example.com"}
    - {key: token, label: "API key", secret: true, help: "Created in the Dokploy panel under Settings, Profile, API/CLI. Use a dedicated user if you can."}
  base: "${url}/api"
  headers: {x-api-key: "${token}"}
  allow:
    - "GET *"
    - "POST /compose.deploy"
    - "POST /compose.redeploy"
    - "POST /application.deploy"
    - "POST /application.redeploy"
  redact: [env, buildArgs, buildSecrets, refreshToken, password, privateKey, apiKey, token, secret]
  test: /settings.health
---

# Dokploy

You reach the user's Dokploy server through two tools. The server adds the API key for you: never ask for it, never
print it, and never try to read it from the environment.

- `skill_api_get(skill="dokploy", path=..., query=...)` reads.
- `skill_api_call(skill="dokploy", method="POST", path=..., body=...)` changes something. Only the deploy and redeploy
  endpoints below are allowed, and the user confirms each call.

If a call says the skill is not connected, tell the user to connect Dokploy under Capabilities (the project's tab, or
their profile) and stop.

## The API

Endpoints are named `router.procedure`. Reads are GET with the arguments in `query`; changes are POST with a JSON `body`.
Values named `env`, tokens and keys come back as `[REDACTED]`.

| To | Call |
|---|---|
| List projects, with their services | GET `/project.all` |
| Find a compose stack | GET `/compose.search` with `q` |
| Find an application | GET `/application.search` with `q` |
| One compose stack | GET `/compose.one` with `composeId` |
| One application | GET `/application.one` with `applicationId` |
| Recent deployments of a stack | GET `/deployment.allByCompose` with `composeId` |
| Recent deployments of an application | GET `/deployment.all` with `applicationId` |
| Containers of a stack or app, and their state | GET `/docker.getContainersByAppNameMatch` with `appName` |
| Domains | GET `/domain.byComposeId` with `composeId`, or `/domain.byApplicationId` with `applicationId` |
| Redeploy a stack | POST `/compose.redeploy` with `{"composeId": ...}` |
| Redeploy an application | POST `/application.redeploy` with `{"applicationId": ...}` |

When you are unsure of an endpoint or its arguments, read the server's own description of its API:
GET `/settings.getOpenApiDocument`. It is large; ask for it only when you need it.

## How to work

1. Find the service first (`project.all` or a search). Do not guess ids.
2. Look at its state and its latest deployments before changing anything. A `status` of `error` or a failed deployment
   is the usual answer to "why is it down".
3. Say what you will redeploy and why, then call it once. A redeploy restarts the service: do not repeat it to see if
   it helps, and do not redeploy several services in a row without the user asking for each.
4. After a redeploy, check the deployment status with a read, and report what you saw.
5. You cannot stop, delete, or edit services or their environment. If the user asks for that, say it is outside this
   connection and that they can do it in the Dokploy panel.
