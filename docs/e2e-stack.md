# Production-shaped end-to-end suite (`e2e:stack`)

`npm run e2e` (in `web/`) drives one `harness-web` with no login: everyone is the user `local`. That cannot show what breaks in
the shape the product is deployed in. `npm run e2e:stack` can: real OIDC sign-in, several people, two web replicas behind one
origin, a separate worker, Postgres, Redis and an S3 store. It is **opt-in**: it is never part of `npm run e2e`, and it skips
(exit 0, with a message on stdout) when Docker, Go, the `playwright` package or Chromium is missing.

```sh
cd web && npm ci && npx playwright install chromium   # once
npm run e2e:stack                                       # or: make e2e-stack
node e2e/stack/run.mjs --only s3,s6                     # some scenarios
node e2e/stack/run.mjs --only s5 --step "spend cap"     # one step (its scenario's earlier steps may be needed)
node e2e/stack/run.mjs --keep-logs                      # keep the process logs (path in the temp dir)
```

Requirements: Docker (images `postgres:16-alpine`, `redis:7-alpine`, `dexidp/dex:v2.41.1`, `rustfs/rustfs:latest`; pulled when
missing), Go (the binaries are built from the working tree), Node 20+, free loopback ports (chosen at random per run; a clash
is retried), about 6 minutes (the scheduler step waits for two minute boundaries and the SIGKILL step for the sweeper, about
one minute each). It never touches a database or server outside its own compose project.

## The stack

| Piece | How |
|---|---|
| Postgres 16, Redis 7, Dex (OIDC), RustFS (S3) | one docker compose project with a unique name (`h7e2e<random>`), the file is generated in a temp directory, ports bound on `127.0.0.1` at random; `down -v` at the end, also on failure and on Ctrl-C |
| 2 x `harness-web` | **host processes** built with `go build` (faster than building the Dockerfile image, and the code under test is the working tree), `-team`, different ports, the same Postgres, Redis, `OIDC_SESSION_KEY` and `HARNESS_SECRET_KEY`; started at the same moment on an empty database (migrations must be safe to run concurrently) |
| 1 x `harness-worker` | host process; the replicas run with `HARNESS_RUN_QUEUE=redis` and `HARNESS_WEB_RUNS=0`, so they only queue runs and the worker executes every one |
| reverse proxy | a tiny Node proxy: one origin for the browser (`OIDC_REDIRECT_URL=http://127.0.0.1:<port>/auth/callback`, registered in Dex), round-robin per request with **no stickiness**, adds `X-Replica: 0|1` to every response and logs which replica served each request; a request with `X-Pin-Replica: <i>` goes to replica i |
| fake LLM, webhook receiver, fake TAPD | on the host (`web/e2e/fakellm.mjs` and two small servers) |
| sandbox | the local backend (`HARNESS_ALLOW_LOCAL_SANDBOX=1`) in one state directory shared by the three processes |

Settings worth knowing: `OIDC_LOGIN_CLAIM=name` (the logins are `alice`, `bob`, `carol`, `dave`, `erin`; password `password`),
`OIDC_REQUIRE_APPROVAL=1`, `HARNESS_ADMINS=alice`, `FACTORY=1`, `FACTORY_NO_REVIEW=1`, `ARTIFACTS_S3_*` (RustFS, bucket created at
start), `RUNTIME_SHUTDOWN_GRACE_SECONDS=2`.

Each person is one Playwright browser context (own cookie jar) signed in through the real Dex form. A person's API calls use the
same cookies (`person.api(method, path, body, {replica})`). Failures print the tail of every process log and save a screenshot of
each person's page to `<tmp>/harness-stack-failures/`.

## Scenarios

| | What it proves |
|---|---|
| s1 sign-in | alice signs in (admin, approved), the first-run welcome opens; bob lands on the pending screen (every route 403 `pending_approval`); alice approves him in the Admin page; carol is rejected and stays rejected after signing in again; logout then 401 and the sign-in page; a removed cookie in an open app shows "Your session expired" (`harness:unauthorized`) |
| s2 sharing | project, invites by email (member, viewer; claimed at first login), a session bob creates on replica 0 appears live on a stream served by replica 1 (Redis feed) and in alice's browser, both replicas served streams; a personal session is invisible to others; the viewer reads but gets 403 on sessions, messages, tasks, invites; a non-member gets 404 and an empty overview |
| s3 approvals | bob's run waits for an approval (executed by the worker), alice sees it in Needs you and approves it in the browser, a viewer cannot answer it, the run finishes, the audit log names both, Insights People has the figures |
| s4 worker | a run queued on replica 0 is executed by the worker (`/metrics` counter +1) and its events stream from replica 1; SIGTERM mid-run: drained within the grace, run `interrupted`, partial text kept, a new run completes on the restarted worker; SIGKILL mid-run: the sweeper ends the orphan (`interrupted`) after about a minute, a new run completes |
| s5 scheduler, caps, digest | a schedule due every minute yields one run per minute across both replicas; a project cap is refused (402) by both replicas and the cost of a worker-run run is in the ledger; "send one now" reaches a signed webhook exactly once |
| s6 durability | the queue (thread state on Postgres) survives a restart of both replicas and the queued message is sent when the held reply ends; the transcript reloads on either replica; published artifacts survive and their content is in S3; preferences follow a person to a second browser |
| s7 security | cookie flags; foreign `Origin` refused (403 `forbidden_origin`, also `null`); CSP header on the UI; 401 without a cookie on every API route; stored secrets (git host token and service token, TAPD password, webhook secret, executor credential) appear in no response of about 40 endpoints on both replicas and in no process log; test-message rate limit; and the production smoke kit run against the stack |

## What it cannot cover

* `Secure` on the cookie needs an https redirect URL; the stack is plain http (unit-tested in `app/product`).
* The sandbox is the local backend; remote backends (OpenSandbox, Daytona, E2B) and the Docker image build are not exercised.
  A web replica cannot read the files of a sandbox the worker created (the local backend keeps its registry in memory), so file
  downloads of worker-run sessions are not asserted; with a remote backend every process reaches the sandbox.
* One worker (there is no assertion about two workers sharing the queue), one Redis (no Sentinel), one Postgres (no failover).
* Real identity providers (Dex stands in; token refresh, MFA, group claims), SMTP, GitHub, Slack and Telegram.
* Load and timing: the scheduler and sweeper tests are about correctness, not throughput.

Known behaviours the suite works around, by design of the product: each replica caches a person's sign-up status for 5 seconds
(an approval or a rejection is seen by all replicas within 5 s); the notification test rate limit is per replica.

## Production smoke kit: `scripts/prod-smoke.mjs`

Read-only checks of a running server with the session of a real person, to run after a deploy. It sends only GET requests
(healthz, `/api/me`, `/api/overview`, the GET endpoints of every area of one project, and the queue, published artifacts,
environment and usage of one session), prints a table and exits 1 when a check fails.

```sh
# the Cookie header of a signed-in browser: DevTools > Application > Cookies > the site's session cookie (HttpOnly), as name=value
BASE_URL=https://app.example.com SESSION_COOKIE='<name>=<value>' node scripts/prod-smoke.mjs
# optional: PROJECT_ID, THREAD_ID, TIMEOUT_MS
```

The cookie is a credential: keep it out of shell history and logs, and use a session that can be ended (sign out). The script
never prints it. The suite runs it against the stack as its last step, so the script stays in step with the API.
