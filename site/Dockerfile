# The 7ar.chat home page: static files behind nginx. Build context is this directory.
FROM nginx:1.27-alpine
COPY nginx.conf /etc/nginx/conf.d/default.conf
COPY index.html styles.css favicon.svg /usr/share/nginx/html/
COPY assets /usr/share/nginx/html/assets
EXPOSE 80
