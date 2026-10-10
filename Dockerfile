FROM golang:1.23-alpine AS builder

RUN apk --no-cache add git

WORKDIR /app

# Cache dependencies
COPY go.mod ./
COPY go.sum* ./
RUN go mod download

# Build binary
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-s -w" \
    -o /ark-vault \
    ./cmd/bot

# ------- Runtime -------
FROM alpine:3.20

RUN apk --no-cache add ca-certificates tzdata \
    && adduser -D -H -h /nonexistent appuser

COPY --from=builder /ark-vault /usr/local/bin/ark-vault

RUN mkdir -p /data/badger && chown appuser:appuser /data/badger

USER appuser

HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
    CMD pgrep ark-vault || exit 1

ENTRYPOINT ["ark-vault"]
