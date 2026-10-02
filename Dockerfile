# The backend with the built UI: one image serves the app host and proxies
# the session hosts.
FROM node:22-alpine AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# The tag must satisfy the go directive in backend/go.mod.
FROM golang:1.26.8 AS backend
WORKDIR /src
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
RUN CGO_ENABLED=0 go build -trimpath -o /out/server ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=backend /out/server /server
COPY --from=web /web/dist /srv/web
EXPOSE 8080
ENTRYPOINT ["/server"]
