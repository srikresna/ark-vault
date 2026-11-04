# Pinned patch for reproducible, security-patched builds (floating tags once
# gave us an EOL toolchain).
FROM golang:1.26.10-alpine AS builder

RUN apk --no-cache add git

WORKDIR /app

# Cache dependencies
COPY go.mod ./
COPY go.sum* ./
RUN go mod download

# Build binary
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -trimpath \
    -o /ark-vault \
    ./cmd/bot

# ------- Runtime -------
FROM alpine:3.22

# alpine 3.22 is the oldest supported base that packages postgresql17-client;
# pg_dump refuses to dump a newer server, so the client major must match the
# postgres:17 server image.
RUN apk --no-cache add ca-certificates tzdata postgresql17-client \
    && adduser -D -H -h /nonexistent appuser

COPY --from=builder /ark-vault /usr/local/bin/ark-vault

RUN mkdir -p /data/images && chown -R appuser:appuser /data

USER appuser
EXPOSE 9102 8080

# Readiness, not liveness: the binary can be alive while Postgres is
# unreachable, and a container that reports healthy in that state would pass a
# deploy gate while every journal write fails.
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD wget -qO- http://127.0.0.1:9102/healthz || exit 1

ENTRYPOINT ["ark-vault"]
