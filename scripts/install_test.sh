#!/usr/bin/env bash
# Exercises scripts/install.sh against a stub docker in a temp copy of the files it needs (no Docker daemon, no network).
# Run: scripts/install_test.sh
set -euo pipefail
SRC="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
T="$(mktemp -d)"
mkdir -p "$T/bin" "$T/repo/scripts"
cp "$SRC/scripts/install.sh" "$T/repo/scripts/"
cp "$SRC/start.sh" "$SRC/docker-compose.quickstart.yml" "$T/repo/"
cat >"$T/bin/docker" <<'EOF'
#!/usr/bin/env bash
echo "docker $*" >>"$STUB_LOG"
case "$1 $2" in
  "compose version") echo "Docker Compose version v2.0.0" ;;
esac
exit 0
EOF
chmod +x "$T/bin/docker"
# a health endpoint
python3 -m http.server 0 --bind 127.0.0.1 -d "$T" >"$T/http.log" 2>&1 &
HP=$!
trap 'kill $HP 2>/dev/null; rm -rf "$T"' EXIT
sleep 1
PORT="$(python3 - <<'EOF'
import socket
s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()
EOF
)"
kill $HP; mkdir -p "$T/www"; echo ok >"$T/www/healthz"
python3 -m http.server "$PORT" --bind 127.0.0.1 -d "$T/www" >"$T/http.log" 2>&1 &
HP=$!
sleep 1
export HARNESS_SKIP_PORT_CHECK=1 STUB_LOG="$T/docker.log" PATH="$T/bin:$PATH" HARNESS_QUICKSTART_URL="http://127.0.0.1:$PORT" HARNESS_QUICKSTART_WAIT=15
export HARNESS_ADMIN_LOGIN=ann HARNESS_ADMIN_PASSWORD=correct-horse LLM_API_KEY=sk-test-123456 LLM_MODELS=gpt-a,gpt-b LLM_DEFAULT_MODEL=gpt-a QUICKSTART_S3=y

cd "$T/repo"
./start.sh </dev/null | tee "$T/out1.txt"
test -f .env && test -f quickstart/dex.yaml
[ "$(stat -f %Lp .env 2>/dev/null || stat -c %a .env)" = 600 ] || { echo "env mode"; exit 1; }
grep -q '^HARNESS_ADMINS=ann$' .env
grep -q '^COMPOSE_PROFILES=s3$' .env
grep -q '^LLM_API_KEY=sk-test-123456$' .env
for k in HARNESS_SECRET_KEY OIDC_SESSION_KEY OIDC_CLIENT_SECRET POSTGRES_PASSWORD; do
  v="$(grep "^$k=" .env | cut -d= -f2)"
  [ "${#v}" -ge 48 ] || { echo "weak $k"; exit 1; }
done
grep -q 'username: "ann"' quickstart/dex.yaml
# shellcheck disable=SC2016
grep -qE 'hash: "\$2[aby]\$10\$' quickstart/dex.yaml
grep -q "secret: \"$(grep '^OIDC_CLIENT_SECRET=' .env | cut -d= -f2)\"" quickstart/dex.yaml
grep -q "correct-horse" .env quickstart/dex.yaml && { echo "password stored in clear"; exit 1; }
grep -q "up -d --build" "$STUB_LOG"
grep -q "harness is running" "$T/out1.txt"

# idempotent: the second run keeps both files byte for byte
cp .env env.before; cp quickstart/dex.yaml dex.before
HARNESS_ADMIN_LOGIN=other ./start.sh </dev/null | tee "$T/out2.txt"
cmp .env env.before && cmp quickstart/dex.yaml dex.before
grep -q "never overwritten" "$T/out2.txt"

# a broken .env is refused, not repaired
printf 'FOO=1\n' >.env.bad
if HARNESS_ENV_FILE=.env.bad ./start.sh </dev/null 2>"$T/err.txt"; then echo "accepted a broken env"; exit 1; fi
grep -q "lacks" "$T/err.txt"
[ "$(cat .env.bad)" = "FOO=1" ]

# the other subcommands reach compose
for c in logs stop restart pull; do ./start.sh $c </dev/null >/dev/null; done
grep -q "compose --env-file .env -f docker-compose.quickstart.yml logs -f" "$STUB_LOG"
grep -q " stop" "$STUB_LOG"; grep -q " restart" "$STUB_LOG"; grep -q " pull" "$STUB_LOG"
./start.sh down </dev/null >/dev/null; grep -q " down$" "$STUB_LOG"
if ./start.sh down --volumes </dev/null 2>/dev/null; then echo "wiped without confirmation"; exit 1; fi
HARNESS_CONFIRM_WIPE=yes ./start.sh down --volumes </dev/null >/dev/null; grep -q "down --volumes" "$STUB_LOG"
./start.sh dev </dev/null | grep -q "go run ./cmd/harness-web"
if ./start.sh bogus </dev/null 2>/dev/null; then echo "accepted a bogus command"; exit 1; fi

# a missing daemon is explained
cat >"$T/bin/docker" <<'EOF'
#!/usr/bin/env bash
[ "$1" = info ] && exit 1
exit 0
EOF
if ./start.sh </dev/null 2>"$T/err2.txt"; then echo "ran without a daemon"; exit 1; fi
grep -q "daemon" "$T/err2.txt"
echo "install.sh: all checks passed"
