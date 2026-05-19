# --- build ---
FROM golang:1.21-alpine AS build

WORKDIR /src
# Cache deps separately so source-only edits don't bust the layer.
COPY go.mod go.sum* ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' \
    -o /out/payment ./cmd/server

# --- runtime ---
# alpine (not distroless) because docker-compose healthcheck needs a shell
# and a curl/wget binary. The image is ~10MB more than scratch; acceptable
# trade-off for working `docker compose ps` output and `docker inspect ... |
# grep Health` debuggability.
FROM alpine:3.19

RUN apk add --no-cache curl ca-certificates tzdata && \
    addgroup -S app && adduser -S -G app app

COPY --from=build /out/payment /usr/local/bin/payment

USER app
EXPOSE 3001
ENTRYPOINT ["/usr/local/bin/payment"]
