# The 7ar.chat home page: static files behind nginx. Build context is this directory.
# portal.js (optional error reporting and Umami) is built here; the page itself is plain HTML.
#
# Environment (all optional; 40-telemetry.sh turns them into /client-config.json and the Content-Security-Policy,
# and stops the container when one is malformed):
#   SENTRY_BROWSER_DSN, SENTRY_ENVIRONMENT, SENTRY_RELEASE   errors raised in the visitor's browser
#   UMAMI_URL, UMAMI_WEBSITE_ID                              visit statistics (anonymous: the page has no login)
FROM node:22-bookworm-slim AS build
WORKDIR /src
COPY package.json package-lock.json ./
RUN npm ci
COPY portal.src.js ./
RUN npm run build

FROM nginx:1.27-alpine
COPY nginx.conf /etc/nginx/conf.d/default.conf
COPY 40-telemetry.sh /docker-entrypoint.d/40-telemetry.sh
# a valid default so nginx -t passes before the start script has run; the script overwrites it
RUN chmod +x /docker-entrypoint.d/40-telemetry.sh \
 && printf '%s\n' "add_header Content-Security-Policy \"default-src 'self'\" always;" > /etc/nginx/csp.conf
COPY index.html styles.css favicon.svg /usr/share/nginx/html/
COPY assets /usr/share/nginx/html/assets
COPY --from=build /src/dist/portal.js /usr/share/nginx/html/portal.js
COPY --from=build /src/dist/chunks /usr/share/nginx/html/chunks
EXPOSE 80
