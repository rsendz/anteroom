# The front-end is built first and copied into the Go source tree, because the
# Go binary embeds it. Node is a build-time dependency only; it does not exist
# in the image that ships.
FROM node:22-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --fund=false
COPY web/ ./
RUN npm run build

FROM golang:1.24-alpine AS build
WORKDIR /src
ARG VERSION=dev
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/internal/httpserver/web/static/ ./internal/httpserver/web/static/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/anteroom ./cmd/anteroom
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/demo-origin ./cmd/demo-origin

FROM gcr.io/distroless/static-debian12:nonroot AS anteroom
COPY --from=build /out/anteroom /anteroom
EXPOSE 8080
ENTRYPOINT ["/anteroom"]

FROM gcr.io/distroless/static-debian12:nonroot AS demo-origin
COPY --from=build /out/demo-origin /demo-origin
EXPOSE 9000
ENTRYPOINT ["/demo-origin"]
