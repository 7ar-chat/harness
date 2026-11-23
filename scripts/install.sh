#!/usr/bin/env bash
# One-command installer and manager of the team app on one machine (docs/quickstart.md).
#
#   ./start.sh [up]     create .env and the sign-in config on first run, start everything, wait until it answers,
#                       print the address and the admin login. Safe to run again: it never overwrites .env.
#   ./start.sh logs [service]   follow the logs
#   ./start.sh stop             stop the containers (data is kept)
#   ./start.sh restart [service]
#   ./start.sh pull             fetch newer images (the published harness image when HARNESS_IMAGE is set)
#   ./start.sh down [--volumes] remove the containers; --volumes also deletes the data (asks first)
#   ./start.sh dev              start only postgres, redis and the sign-in, for running harness-web from source
#
# Non-interactive use: set HARNESS_ADMIN_LOGIN, HARNESS_ADMIN_PASSWORD, LLM_BASE_URL, LLM_API_KEY, LLM_MODELS,
# LLM_DEFAULT_MODEL and QUICKSTART_S3 in the environment; nothing is asked when stdin is not a terminal.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

ENV_FILE="${HARNESS_ENV_FILE:-.env}"
COMPOSE_FILE="docker-compose.quickstart.yml"
DEX_FILE="quickstart/dex.yaml"
WEB_URL="${HARNESS_QUICKSTART_URL:-http://localhost:8080}"
WAIT_SECONDS="${HARNESS_QUICKSTART_WAIT:-240}"

say() { printf '%s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}
interactive() { [ -t 0 ] && [ -t 1 ]; }

usage() {
  sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed '$d' | sed 's/^# \{0,1\}//'
}

# ---- prerequisites ---------------------------------------------------------------------------------------------------

COMPOSE=()
check_docker() {
  command -v docker >/dev/null 2>&1 || die "Docker is not installed. Install Docker Desktop or Docker Engine: https://docs.docker.com/get-docker/"
  docker info >/dev/null 2>&1 || die "Docker is installed but its daemon does not answer. Start Docker and run this again."
  if docker compose version >/dev/null 2>&1; then
    COMPOSE=(docker compose)
  elif command -v docker-compose >/dev/null 2>&1; then
    COMPOSE=(docker-compose)
  else
    die "Docker Compose v2 is missing (\`docker compose version\` fails). Install the compose plugin: https://docs.docker.com/compose/install/"
  fi
}

compose() { "${COMPOSE[@]}" --env-file "$ENV_FILE" -f "$COMPOSE_FILE" "$@"; }

# ---- helpers ---------------------------------------------------------------------------------------------------------

random_hex() { # random_hex BYTES
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex "$1"
  else
    head -c "$1" /dev/urandom | od -An -tx1 | tr -d ' \n'
  fi
}

random_password() { random_hex 9; }

ask() { # ask VAR "Prompt" "default" [secret]
  local var="$1" prompt="$2" def="${3:-}" secret="${4:-}" val=""
  if [ -n "${!var:-}" ]; then return 0; fi
  if interactive; then
    if [ -n "$secret" ]; then
      read -r -s -p "$prompt${def:+ [$def]}: " val || true
      printf '\n'
    else
      read -r -p "$prompt${def:+ [$def]}: " val || true
    fi
  fi
  printf -v "$var" '%s' "${val:-$def}"
}

env_get() { # env_get KEY: the value in the env file ("" when absent)
  [ -f "$ENV_FILE" ] || return 0
  local line
  line="$(grep -E "^$1=" "$ENV_FILE" | tail -n 1 || true)"
  printf '%s' "${line#*=}"
}

env_has() { grep -qE "^$1=." "$ENV_FILE" 2>/dev/null; }

REQUIRED_KEYS=(HARNESS_SECRET_KEY OIDC_SESSION_KEY OIDC_CLIENT_SECRET POSTGRES_PASSWORD)

# ---- .env ------------------------------------------------------------------------------------------------------------

CREATED_ENV=0
ADMIN_PASSWORD_SHOWN=""

ensure_env() {
  if [ -f "$ENV_FILE" ]; then
    local missing=()
    for k in "${REQUIRED_KEYS[@]}"; do env_has "$k" || missing+=("$k"); done
    if [ "${#missing[@]}" -gt 0 ]; then
      die "$ENV_FILE exists but lacks ${missing[*]}. It is never overwritten: add them (a random value each, for example \`openssl rand -hex 32\`) or move the file away and run this again."
    fi
    say "Using the existing $ENV_FILE (never overwritten)."
    return 0
  fi

  say "First run: creating $ENV_FILE with secrets of its own."
  local login="${HARNESS_ADMIN_LOGIN:-}" password="${HARNESS_ADMIN_PASSWORD:-}"
  ask login "Admin login (letters, digits, - and _)" "admin"
  if ! [[ "$login" =~ ^[a-z][a-z0-9_-]{1,31}$ ]]; then
    die "The admin login must start with a lower-case letter and use only a-z, 0-9, - and _ (2 to 32 characters); got \"$login\"."
  fi
  if [ -z "$password" ]; then
    ask password "Admin password (empty: generate one)" "" secret
  fi
  if [ -z "$password" ]; then
    password="$(random_password)"
    ADMIN_PASSWORD_SHOWN="$password"
  fi
  [ "${#password}" -ge 8 ] || die "The admin password needs at least 8 characters."

  local llm_url="${LLM_BASE_URL:-}" llm_key="${LLM_API_KEY:-}" llm_models="${LLM_MODELS:-}" llm_default="${LLM_DEFAULT_MODEL:-}"
  ask llm_url "Model provider URL (OpenAI-compatible, empty: set it later in $ENV_FILE)" "https://api.openai.com/v1"
  ask llm_key "Model provider API key (empty: set it later)" "" secret
  ask llm_models "Models to offer, comma separated" "${llm_default:-}"
  ask llm_default "Default model" "${llm_models%%,*}"
  [ -n "$llm_key" ] || warn "no model API key: the app starts, but runs fail until LLM_API_KEY is set in $ENV_FILE (then \`./start.sh restart\`)."

  local s3="${QUICKSTART_S3:-}"
  ask s3 "Also run RustFS (S3-compatible storage) for artifacts and attachments? [y/N]" "n"

  umask 077
  {
    echo "# Written by start.sh on $(date -u +%Y-%m-%dT%H:%M:%SZ). Edit freely; start.sh never overwrites this file."
    echo "# After a change: ./start.sh restart"
    echo
    echo "# secrets (unique to this install; losing HARNESS_SECRET_KEY makes the saved tokens unreadable)"
    echo "HARNESS_SECRET_KEY=$(random_hex 32)"
    echo "OIDC_SESSION_KEY=$(random_hex 32)"
    echo "OIDC_CLIENT_SECRET=$(random_hex 24)"
    echo "POSTGRES_PASSWORD=$(random_hex 24)"
    echo
    echo "# the administrator (the sign-in account is in $DEX_FILE)"
    echo "HARNESS_ADMIN_LOGIN=$login"
    echo "HARNESS_ADMINS=$login"
    echo
    echo "# your model provider"
    echo "LLM_PROVIDER=openai"
    echo "LLM_BASE_URL=$llm_url"
    echo "LLM_API_KEY=$llm_key"
    echo "LLM_MODELS=$llm_models"
    echo "LLM_DEFAULT_MODEL=$llm_default"
    echo
    echo "# where the agent's commands run: local (inside the harness container) or opensandbox"
    echo "SANDBOX_BACKEND=local"
    echo "#OPENSANDBOX_URL="
    echo "#OPENSANDBOX_API_KEY="
    echo
    echo "# a published image instead of a local build (then ./start.sh pull)"
    echo "#HARNESS_IMAGE="
    if [[ "$s3" =~ ^([yY]|1|true) ]]; then
      echo
      echo "# S3-compatible storage (RustFS)"
      echo "QUICKSTART_S3=1"
      echo "COMPOSE_PROFILES=s3"
      echo "AWS_ACCESS_KEY_ID=harness$(random_hex 4)"
      echo "AWS_SECRET_ACCESS_KEY=$(random_hex 24)"
      echo "ARTIFACTS_S3_ENDPOINT=http://rustfs:9000"
      echo "ARTIFACTS_S3_BUCKET=harness"
      echo "ARTIFACTS_S3_CREATE_BUCKET=true"
    fi
  } >"$ENV_FILE"
  chmod 600 "$ENV_FILE"
  CREATED_ENV=1
  ADMIN_LOGIN="$login"
  ADMIN_PASSWORD="$password"
  say "Wrote $ENV_FILE (mode 600)."
}

# ---- the sign-in (Dex) -----------------------------------------------------------------------------------------------

bcrypt_hash() { # bcrypt_hash LOGIN: reads the password on stdin
  if command -v htpasswd >/dev/null 2>&1; then
    htpasswd -niBC 10 "$1" | cut -d: -f2-
  else
    docker run --rm -i httpd:2.4-alpine htpasswd -niBC 10 "$1" | cut -d: -f2-
  fi
}

ensure_dex() {
  if [ -f "$DEX_FILE" ]; then return 0; fi
  if [ "$CREATED_ENV" != 1 ]; then
    die "$DEX_FILE is missing but $ENV_FILE exists, so the admin password is unknown. Remove $ENV_FILE only if you want a fresh install; otherwise recreate $DEX_FILE from examples/dev-oidc/dex.yaml (client id harness, secret = OIDC_CLIENT_SECRET)."
  fi
  mkdir -p "$(dirname "$DEX_FILE")"
  local hash secret uid
  hash="$(printf '%s' "$ADMIN_PASSWORD" | bcrypt_hash "$ADMIN_LOGIN" | tr -d '\r\n')"
  [ "${hash:0:2}" = "\$2" ] || die "could not hash the admin password (need htpasswd, or Docker to run it)"
  secret="$(env_get OIDC_CLIENT_SECRET)"
  uid="$(random_hex 16)"
  uid="${uid:0:8}-${uid:8:4}-${uid:12:4}-${uid:16:4}-${uid:20:12}"
  umask 077
  cat >"$DEX_FILE" <<EOF
# Written by start.sh: the sign-in of this install. Add people under staticPasswords (bcrypt hashes:
# \`htpasswd -niBC 10 name\`), then \`./start.sh restart dex\`. Only HARNESS_ADMINS are administrators.
issuer: http://localhost:5556/dex
storage:
  type: memory
web:
  http: 0.0.0.0:5556
oauth2:
  skipApprovalScreen: true
enablePasswordDB: true
staticClients:
  - id: harness
    name: Harness
    secret: "$secret"
    redirectURIs:
      - http://localhost:8080/auth/callback
staticPasswords:
  - email: "$ADMIN_LOGIN@harness.local"
    hash: "$hash"
    username: "$ADMIN_LOGIN"
    userID: "$uid"
EOF
  chmod 600 "$DEX_FILE"
  say "Wrote $DEX_FILE (the admin account; mode 600)."
}

# ---- ports -----------------------------------------------------------------------------------------------------------

port_in_use() { # port_in_use PORT
  if command -v lsof >/dev/null 2>&1; then
    lsof -nP -iTCP:"$1" -sTCP:LISTEN >/dev/null 2>&1
  elif command -v nc >/dev/null 2>&1; then
    nc -z 127.0.0.1 "$1" >/dev/null 2>&1
  else
    return 1
  fi
}

check_ports() {
  [ "${HARNESS_SKIP_PORT_CHECK:-}" = 1 ] && return 0
  # Only ports of services that are not already ours: a re-run finds its own containers listening.
  local running
  running="$(compose ps --status running --format '{{.Service}}' 2>/dev/null || true)"
  [ -n "$running" ] && return 0
  for p in "$@"; do
    if port_in_use "$p"; then
      die "port $p is already in use on this machine. Free it, or (for 8080 and 5556) note they are fixed because the sign-in redirects to them."
    fi
  done
}

# ---- commands --------------------------------------------------------------------------------------------------------

wait_healthy() {
  say "Waiting for $WEB_URL/healthz (up to ${WAIT_SECONDS}s; the first build takes a few minutes) ..."
  local waited=0
  while [ "$waited" -lt "$WAIT_SECONDS" ]; do
    if curl -fsS --max-time 3 "$WEB_URL/healthz" >/dev/null 2>&1; then return 0; fi
    sleep 3
    waited=$((waited + 3))
  done
  warn "the app did not answer in ${WAIT_SECONDS}s. Look at the logs: ./start.sh logs"
  return 1
}

cmd_up() {
  check_docker
  ensure_env
  ensure_dex
  check_ports 8080 5556
  local image
  image="$(env_get HARNESS_IMAGE)"
  if [ -n "$image" ]; then
    say "Pulling $image (building from this checkout if that fails) ..."
    compose pull harness-web >/dev/null 2>&1 || warn "could not pull $image; building from this checkout instead."
  fi
  say "Starting ..."
  compose up -d --build
  if wait_healthy; then
    say
    say "harness is running:  $WEB_URL"
    local login
    login="$(env_get HARNESS_ADMIN_LOGIN)"
    if [ "$CREATED_ENV" = 1 ]; then
      say "Sign in as:        $ADMIN_LOGIN"
      if [ -n "$ADMIN_PASSWORD_SHOWN" ]; then
        say "Password:          $ADMIN_PASSWORD_SHOWN   (shown once; it is only stored as a hash in $DEX_FILE)"
      else
        say "Password:          the one you typed"
      fi
    else
      say "Administrator:     ${login:-see HARNESS_ADMINS in $ENV_FILE}"
    fi
    say "Logs: ./start.sh logs    Stop: ./start.sh stop    Remove: ./start.sh down"
  else
    exit 1
  fi
}

need_installed() {
  [ -f "$ENV_FILE" ] || die "$ENV_FILE does not exist: run ./start.sh first."
}

cmd_logs() { check_docker; need_installed; compose logs -f --tail=200 "$@"; }
cmd_stop() { check_docker; need_installed; compose stop; }
cmd_restart() { check_docker; need_installed; compose restart "$@"; }
cmd_pull() {
  check_docker
  need_installed
  compose pull --ignore-buildable
}

cmd_down() {
  check_docker
  need_installed
  if [ "${1:-}" = "--volumes" ] || [ "${1:-}" = "-v" ]; then
    local ok="${HARNESS_CONFIRM_WIPE:-}"
    if [ "$ok" != "yes" ] && interactive; then
      read -r -p "This deletes the database and every saved session. Type 'delete' to continue: " ok || true
      [ "$ok" = "delete" ] && ok=yes
    fi
    [ "$ok" = "yes" ] || die "not confirmed (set HARNESS_CONFIRM_WIPE=yes to confirm without a terminal)."
    compose down --volumes
  else
    compose down
    say "Containers removed; the data volumes stay. \`./start.sh down --volumes\` deletes them too."
  fi
}

cmd_dev() {
  check_docker
  ensure_env
  ensure_dex
  # harness-web runs from source on the host, so Dex's own copy of port 8080 moves aside.
  export WEB_BIND="127.0.0.1:18080"
  check_ports 5556
  say "Starting postgres, redis and the sign-in ..."
  compose up -d --wait postgres redis dex 2>/dev/null || compose up -d postgres redis dex
  local pgport redisport
  pgport="$(env_get POSTGRES_PORT)"; pgport="${pgport:-15432}"
  redisport="$(env_get REDIS_PORT)"; redisport="${redisport:-16379}"
  cat <<EOF

Dependencies are up. Run the app from source (two terminals):

  # 1. the API and UI
  set -a; . ./$ENV_FILE; set +a
  export POSTGRES_HOST=127.0.0.1 POSTGRES_PORT=$pgport POSTGRES_SSLMODE=disable POSTGRES_DB=harness POSTGRES_USER=harness
  export REDIS_HOST=127.0.0.1 REDIS_PORT=$redisport
  export OIDC_ISSUER=http://localhost:5556/dex OIDC_CLIENT_ID=harness OIDC_REDIRECT_URL=http://localhost:8080/auth/callback OIDC_LOGIN_CLAIM=name
  export HARNESS_PUBLIC_URL=http://localhost:8080 HARNESS_ALLOW_LOCAL_SANDBOX=1
  (cd web && npm ci && npm run build) && (cd app && go run ./cmd/harness-web -team -local -addr 127.0.0.1:8080)

  # 2. optional, the UI with hot reload on http://localhost:5173
  (cd web && npm run dev)

Sign in as ${HARNESS_ADMIN_LOGIN:-$(env_get HARNESS_ADMIN_LOGIN)}. Stop the dependencies with ./start.sh stop.
EOF
}

main() {
  local cmd="${1:-up}"
  [ "$#" -gt 0 ] && shift
  case "$cmd" in
    up | start | install) cmd_up "$@" ;;
    logs) cmd_logs "$@" ;;
    stop) cmd_stop "$@" ;;
    down) cmd_down "$@" ;;
    restart) cmd_restart "$@" ;;
    pull) cmd_pull "$@" ;;
    dev) cmd_dev "$@" ;;
    -h | --help | help) usage ;;
    *)
      usage >&2
      die "unknown command \"$cmd\""
      ;;
  esac
}

main "$@"
