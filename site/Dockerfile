# The 7ar.chat home page: a static Astro site (built to dist/) served by nginx. Build context is this directory.
# The page itself is prerendered HTML; src/scripts/portal.js (optional error reporting and Umami) is bundled by Astro.
#
# Environment (all optional; 40-telemetry.sh turns them into /client-config.json and the Content-Security-Policy,
# and stops the container when one is malformed):
#   SENTRY_BROWSER_DSN, SENTRY_ENVIRONMENT, SENTRY_RELEASE   errors raised in the visitor's browser
#   UMAMI_URL, UMAMI_WEBSITE_ID                              visit statistics (anonymous: the page has no login)
FROM node:22-bookworm-slim AS build
WORKDIR /src
ENV ASTRO_TELEMETRY_DISABLED=1
COPY package.json package-lock.json ./
RUN npm ci
COPY astro.config.mjs tsconfig.json ./
COPY public ./public
COPY src ./src
RUN npm run build

FROM nginx:1.27-alpine
COPY nginx.conf /etc/nginx/conf.d/default.conf
COPY 40-telemetry.sh /docker-entrypoint.d/40-telemetry.sh
# a valid default so nginx -t passes before the start script has run; the script overwrites it
RUN chmod +x /docker-entrypoint.d/40-telemetry.sh \
 && printf '%s\n' "add_header Content-Security-Policy \"default-src 'self'\" always;" > /etc/nginx/csp.conf
COPY --from=build /src/dist /usr/share/nginx/html
EXPOSE 80
