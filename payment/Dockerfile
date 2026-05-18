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
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/payment /payment

USER nonroot:nonroot
EXPOSE 3001
ENTRYPOINT ["/payment"]
