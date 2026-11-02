#!/bin/sh
# Runs when the container starts (nginx image: /docker-entrypoint.d). Turns the optional
#   SENTRY_BROWSER_DSN   a Sentry-protocol DSN (Rustrak, Sentry, GlitchTip) for errors raised in the browser
#   SENTRY_ENVIRONMENT   tag on every event (default: production)
#   SENTRY_RELEASE       tag on every event (optional)
#   UMAMI_URL            https://umami.example.com (origin only)
#   UMAMI_WEBSITE_ID     the Umami website id of this page
# into /client-config.json, which portal.js reads, and into the Content-Security-Policy, which allows exactly those
# origins and nothing else. With none set, the file is {} and the policy stays 'self'.
# A malformed value stops the container: a typo is found at deploy time, not silently ignored.
set -eu

html=/usr/share/nginx/html
fail() { echo "telemetry: $*" >&2; exit 1; }

dsn=${SENTRY_BROWSER_DSN:-}
umami=${UMAMI_URL:-}
umami=${umami%/}
umami_id=${UMAMI_WEBSITE_ID:-}
env_name=${SENTRY_ENVIRONMENT:-production}
release=${SENTRY_RELEASE:-}

script_src="'self'"
connect_src="'self'"
sentry_origin=""

# values go into JSON and a header: only characters that need no escaping
safe() { case "$2" in *[!A-Za-z0-9._:/@%+=~,-]*) fail "$1 has characters that are not allowed";; esac; }
safe SENTRY_ENVIRONMENT "$env_name"
safe SENTRY_RELEASE "$release"
safe UMAMI_WEBSITE_ID "$umami_id"

if [ -n "$dsn" ]; then
  safe SENTRY_BROWSER_DSN "$dsn"
  # https://<key>@<host>[:port]/<project>
  case "$dsn" in
    http://*@*/?*|https://*@*/?*) ;;
    *) fail "SENTRY_BROWSER_DSN is not a DSN (want https://<key>@<host>/<project>)";;
  esac
  scheme=${dsn%%://*}
  rest=${dsn#*://}
  hostpart=${rest#*@}
  sentry_origin="$scheme://${hostpart%%/*}"
fi

if [ -n "$umami" ] || [ -n "$umami_id" ]; then
  [ -n "$umami" ] && [ -n "$umami_id" ] || fail "UMAMI_URL and UMAMI_WEBSITE_ID go together"
  safe UMAMI_URL "$umami"
  case "$umami" in
    http://*|https://*) ;;
    *) fail "UMAMI_URL is not an origin (want https://umami.example.com)";;
  esac
  case "${umami#*://}" in */*) fail "UMAMI_URL must be an origin without a path";; esac
  script_src="$script_src $umami"
  connect_src="$connect_src $umami"
fi
if [ -n "$sentry_origin" ] && [ "$sentry_origin" != "$umami" ]; then
  connect_src="$connect_src $sentry_origin"
fi

# /client-config.json
{
  printf '{'
  sep=
  if [ -n "$dsn" ]; then
    printf '%s"sentry":{"dsn":"%s","environment":"%s"' "$sep" "$dsn" "$env_name"
    [ -n "$release" ] && printf ',"release":"%s"' "$release"
    printf '}'
    sep=,
  fi
  if [ -n "$umami" ]; then
    printf '%s"umami":{"scriptUrl":"%s/script.js","websiteId":"%s"}' "$sep" "$umami" "$umami_id"
  fi
  printf '}\n'
} > "$html/client-config.json"

# The policy lives in its own file, included by nginx.conf.
cat > /etc/nginx/csp.conf <<EOF
add_header Content-Security-Policy "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src $script_src; connect-src $connect_src; frame-ancestors 'none'; base-uri 'none'; form-action 'none'; object-src 'none'" always;
EOF

echo "telemetry: errors ${sentry_origin:-off}, analytics ${umami:-off}"
