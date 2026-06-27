# syntax=docker/dockerfile:1.6
#
# Multi-stage Dockerfile untuk bit-multi-brain-rag DASHBOARD.
#
# - Stage 1 (builder): Alpine + gcc + musl. Build binary dengan CGO=1 karena
#   smacker/go-tree-sitter butuh C bindings. Output: /out/dashboard (statis).
# - Stage 2 (runtime): Alpine minimal. Hanya CA certs + binary + data dir.
#   Image akhir ~25-30 MB.
#
# Build:  docker build -t bit-rag-dashboard:latest .
# Run:    docker run --rm -p 8081:8081 --env-file .env bit-rag-dashboard:latest
#
# Easypanel: pakai docker-compose.yml di project root. Easypanel otomatis
# baca compose dan build dari Dockerfile ini.
# =============================================================================

# ----- Stage 1: builder ------------------------------------------------------
FROM golang:1.25-alpine AS builder

# Tools yang dibutuhkan tree-sitter (C compiler + headers) + ca-certs.
RUN apk add --no-cache \
        build-base \
        ca-certificates \
        git

WORKDIR /src

# Layer caching: copy go.mod + go.sum dulu, download deps, baru copy source.
# Source change tidak invalidate go mod download cache.
COPY go.mod go.sum ./
RUN go mod download

# Copy source.
COPY . .

# Build dashboard binary.
# - CGO_ENABLED=1: tree-sitter butuh CGO
# - -ldflags "-s -w": strip symbol table + DWARF debug info (size -30%)
# - -trimpath: hilangkan path absolut dari binary (reproducible build)
# - GOOS=linux: explicit untuk cross-platform safety
ENV CGO_ENABLED=1 \
    GOOS=linux \
    GOFLAGS="-mod=mod"

RUN go build \
        -trimpath \
        -ldflags "-s -w" \
        -o /out/dashboard \
        ./cmd/dashboard

# ----- Stage 2: runtime ------------------------------------------------------
FROM alpine:3.20

# CA certs untuk HTTPS calls (Qdrant cloud, embedding server, dll).
# tzdata supaya log timestamp benar.
# wget untuk healthcheck (Alpine: tidak ada curl by default).
RUN apk add --no-cache \
        ca-certificates \
        tzdata \
        wget \
    && addgroup -S app \
    && adduser -S -G app -h /app app

WORKDIR /app

# Copy binary + ownership ke non-root user.
COPY --from=builder --chown=app:app /out/dashboard /app/dashboard

# Data directory untuk SQLite. Volume-mount di compose.
RUN mkdir -p /app/data && chown app:app /app/data

USER app

# Default config (dapat di-override via env di docker-compose).
ENV HTTP_ADDR=":8081" \
    ENVIRONMENT="production" \
    DB_PATH="/app/data/dashboard.db" \
    QDRANT_URL="http://qdrant:6333" \
    EMBEDDING_ENDPOINT="http://embedder:8080" \
    EMBEDDING_MODEL="voyage-4-nano" \
    EMBEDDING_DIM="1024" \
    EMBEDDING_POOLING="mean" \
    EMBEDDING_TIMEOUT_S="30" \
    ACTIVE_MODEL="voyage_nano_1024" \
    ACTIVE_BACKEND="llama_q8" \
    MCP_ENABLED="false"

# HANYA expose port HTTP dashboard. Internal services (SQLite, MCP stdio)
# tidak butuh port. MCP binary di-run terpisah via stdio (tidak di image ini).
EXPOSE 8081

# Healthcheck: /healthz adalah endpoint public (tidak butuh API key).
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -qO- http://localhost:8081/healthz || exit 1

ENTRYPOINT ["/app/dashboard"]
