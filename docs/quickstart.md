# Quickstart: the whole team app with one command

```sh
git clone https://github.com/7ar-chat/harness && cd harness
./start.sh
```

You need Docker with Compose v2 (Docker Desktop, or Docker Engine and the compose plugin) and a model provider with an
OpenAI-compatible API. `./start.sh`:

1. checks that Docker and Compose work;
2. on the first run, asks for the administrator's login and password (empty password: it generates one and shows it
   once) and for your model provider (URL, key, models), and writes **`.env`** with secrets of its own
   (`HARNESS_SECRET_KEY`, `OIDC_SESSION_KEY`, `OIDC_CLIENT_SECRET`, `POSTGRES_PASSWORD`, each random) and
   **`quickstart/dex.yaml`**, the sign-in with that one account (the password is stored as a bcrypt hash);
3. runs `docker compose -f docker-compose.quickstart.yml up -d --build`: Postgres, Redis, Dex (the sign-in) and
   harness-web, which serves the API and the UI and runs the agents;
4. waits for `/healthz` and prints `http://localhost:8080` and who to sign in as.

Everything is published on `127.0.0.1` only. Run it again at any time: it never overwrites `.env` or the sign-in file, and
starting what is already running changes nothing. Without a terminal (CI, a provisioning script) nothing is asked: set
`HARNESS_ADMIN_LOGIN`, `HARNESS_ADMIN_PASSWORD`, `LLM_BASE_URL`, `LLM_API_KEY`, `LLM_MODELS`, `LLM_DEFAULT_MODEL` and
`QUICKSTART_S3` in the environment.

## Commands

| Command | Does |
|---|---|
| `./start.sh` or `./start.sh up` | creates `.env` and the sign-in on the first run, starts everything, waits until it answers |
| `./start.sh logs [service]` | follows the logs (`harness-web`, `postgres`, `redis`, `dex`) |
| `./start.sh stop` | stops the containers; the data stays |
| `./start.sh restart [service]` | restarts them (after editing `.env`) |
| `./start.sh pull` | fetches newer images; with `HARNESS_IMAGE` set in `.env` this is the published harness image |
| `./start.sh down` | removes the containers and keeps the volumes. `down --volumes` also deletes the database and the sessions, after you type `delete` |
| `./start.sh dev` | starts only Postgres, Redis and the sign-in and prints how to run harness-web from source with the UI on hot reload |

`scripts/install.sh` is the implementation (`./start.sh` calls it); `scripts/install_test.sh` tests it against a stub docker.

## What runs where

| Service | Role | Port on 127.0.0.1 |
|---|---|---|
| `harness-web` | the app (team mode, OIDC through Dex) and the agent runs. It shares Dex's network, so `localhost:5556` is the same issuer address for your browser and the server | 8080 |
| `dex` | the sign-in. Add people under `staticPasswords` in `quickstart/dex.yaml` (`htpasswd -niBC 10 name` makes the hash), then `./start.sh restart dex`. Only the logins in `HARNESS_ADMINS` are administrators | 5556 |
| `postgres` | the team data and the sessions | 15432 (`POSTGRES_PORT`) |
| `redis` | the live feed between processes | 16379 (`REDIS_PORT`) |
| `rustfs` | S3-compatible storage for artifacts, attachments and the files of projects without a repository. Off by default: answer yes when asked, or set `QUICKSTART_S3=1`, `COMPOSE_PROFILES=s3` and the `AWS_*` / `ARTIFACTS_S3_*` variables in `.env` | 9000 |

By default the agent's commands run **inside the harness-web container** (`SANDBOX_BACKEND=local`), which is fine to try it
and for a small trusted team, and not an isolation boundary. For real isolation run an
[OpenSandbox](https://github.com/alibaba/OpenSandbox) lifecycle server (see `sandbox-image/README.md`) and set
`SANDBOX_BACKEND=opensandbox`, `OPENSANDBOX_URL` and `OPENSANDBOX_API_KEY` in `.env`. A separate worker process is not
needed here: harness-web executes the runs itself (`harness-worker` is for deployments that split the web tier from the
run executors, see [embedding.md](embedding.md)).

## The `.env` file

It is plain `KEY=value`, mode 600, and yours to edit; after a change run `./start.sh restart`. The keys that matter:

- `HARNESS_SECRET_KEY` seals the tokens and SSH keys users save (connectors, git hosts). Back it up: without it they are unreadable.
- `LLM_BASE_URL`, `LLM_API_KEY`, `LLM_MODELS`, `LLM_DEFAULT_MODEL`: the model provider.
- `HARNESS_ADMINS`: who administers (logins, or emails of verified accounts). Admins approve nothing here (accounts made in
  `dex.yaml` are approved) but own the **Admin** page, including [Git hosts](team.md#git-hosts-gitlab-gitea-and-plain-git).
- optional integrations, off while empty: `GITHUB_APP_*` (private repositories, project import), `FACTORY*` (the
  [agent factory](factory.md)).

An existing `.env` is never rewritten. If it lacks one of the four secrets the script stops and says which.

## Upgrading and uninstalling

`git pull && ./start.sh` rebuilds the image from the new checkout (the database migrates itself at start). With
`HARNESS_IMAGE` set, `./start.sh pull && ./start.sh` upgrades from the registry. `./start.sh down --volumes` plus deleting
`.env` and `quickstart/` removes every trace.

## Troubleshooting

- *Docker is installed but its daemon does not answer*: start Docker.
- *port 8080 / 5556 is already in use*: those two are fixed because the sign-in redirects to them. Stop the other process
  (`HARNESS_SKIP_PORT_CHECK=1` skips the check when you know better).
- The first build takes minutes (Go and the UI are compiled in the image). `./start.sh logs` shows how far it is once it runs.
- *Signed in but runs fail*: `LLM_API_KEY` is empty or wrong in `.env`; fix it and `./start.sh restart harness-web`.
