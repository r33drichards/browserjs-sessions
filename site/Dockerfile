# The public site: VitePress builds static files, nginx serves them.
# node:22-alpine
FROM node@sha256:0a7108bf6c7bf5de370ffb1a3ed6be93d405b43ff159f681a8d18c0e2bc2e402 AS build
WORKDIR /site
COPY package.json package-lock.json ./
RUN npm ci
COPY . ./
RUN npm run build

# nginxinc/nginx-unprivileged:1.29-alpine: runs as uid 101, listens on 8080,
# and writes only under /tmp (mount one there for a read-only root).
FROM nginxinc/nginx-unprivileged@sha256:0c79d56aee561a1d81c63f00eee5fb5fe29279560cdc55e91425133104c7fbe6
COPY nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /site/.vitepress/dist /srv/site
# By number, so that runAsNonRoot can tell.
USER 101
EXPOSE 8080
