#!/usr/bin/env bash
# Local team-mode stack: Dex (OIDC provider, users alice@example.com and bob@example.com, password "password")
# in Docker, and harness-web on http://localhost:8080 with OIDC login. First logins register the users.
set -euo pipefail
cd "$(dirname "$0")/../.."
docker rm -f harness-dex >/dev/null 2>&1 || true
docker run -d --name harness-dex -p 5556:5556 -v "$PWD/examples/dev-oidc/dex.yaml:/etc/dex/config.yaml:ro" dexidp/dex:v2.41.1 dex serve /etc/dex/config.yaml
make web
go build -o bin/harness-web ./cmd/harness-web
export OIDC_ISSUER=http://localhost:5556/dex OIDC_CLIENT_ID=harness OIDC_CLIENT_SECRET=harness-dev-secret \
  OIDC_REDIRECT_URL=http://localhost:8080/auth/callback OIDC_LOGIN_CLAIM=name \
  OIDC_SESSION_KEY="${OIDC_SESSION_KEY:-$(openssl rand -hex 32)}"
sleep 3
exec ./bin/harness-web -local -addr 127.0.0.1:8080
