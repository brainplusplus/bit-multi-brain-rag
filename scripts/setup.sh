#!/usr/bin/env bash
# =============================================================================
# setup.sh — Full setup wizard for bit-multi-brain-rag (Linux / macOS).
#
# What it does:
#   1. Check prerequisites (Docker, Go)
#   2. Generate .env with a random API key (if not exists)
#   3. Create Docker network + deploy Qdrant + embedder + dashboard
#   4. Build MCP binary locally
#   5. Print ready-to-paste MCP config for your AI tool
#
# Usage:
#   ./scripts/setup.sh                # full setup
#   ./scripts/setup.sh --skip-docker  # skip Docker deploy
#   ./scripts/setup.sh --skip-mcp     # skip MCP build
#   ./scripts/setup.sh --uninstall    # stop + remove containers
#
# Run from REPO ROOT.
# =============================================================================
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_ROOT"

SKIP_DOCKER=0
SKIP_MCP=0
UNINSTALL=0
for arg in "$@"; do
    case "$arg" in
        --skip-docker) SKIP_DOCKER=1 ;;
        --skip-mcp)    SKIP_MCP=1 ;;
        --uninstall)   UNINSTALL=1 ;;
        --help|-h)
            grep -E '^# ' "$0" | sed 's/^# //'
            exit 0
            ;;
        *) echo "Unknown arg: $arg" >&2; exit 2 ;;
    esac
done

step() { printf "\n\033[36m=== %s ===\033[0m\n" "$*"; }
ok()   { printf "  \033[32m[OK]\033[0m %s\n" "$*"; }
warn() { printf "  \033[33m[!]\033[0m  %s\n" "$*"; }
err()  { printf "  \033[31m[X]\033[0m  %s\n" "$*" >&2; }
info() { printf "  \033[2m..\033[0m  %s\n" "$*"; }

# ---------------------------------------------------------------------------
# Uninstall
# ---------------------------------------------------------------------------
if [[ $UNINSTALL -eq 1 ]]; then
    step "Uninstalling bit-rag"
    docker stop bit-rag-dashboard bit-rag-embedder bit-rag-qdrant 2>/dev/null || true
    docker rm bit-rag-dashboard bit-rag-embedder bit-rag-qdrant 2>/dev/null || true
    ok "Containers stopped and removed"
    warn "Docker volumes preserved. Remove manually:"
    info "  docker volume rm bit-rag-data qdrant-storage qdrant-snapshots"
    exit 0
fi

# ---------------------------------------------------------------------------
# 1. Check prerequisites
# ---------------------------------------------------------------------------
step "Checking prerequisites"

if ! command -v docker &>/dev/null; then
    err "Docker is not installed."
    info "Install: https://docs.docker.com/engine/install/"
    exit 1
fi
ok "Docker found"

if ! docker info &>/dev/null; then
    err "Docker daemon is not running. Start it first."
    exit 1
fi
ok "Docker daemon is running"

if [[ $SKIP_MCP -eq 0 ]]; then
    if ! command -v go &>/dev/null; then
        err "Go is not installed (needed to build MCP binary)."
        info "Install from https://go.dev/dl/ (1.24+ required)."
        info "Or run with --skip-mcp if you already have the binary."
        exit 1
    fi
    GO_VER=$(go version | sed 's/go version go\([0-9.]*\).*/\1/')
    ok "Go $GO_VER found"
fi

# ---------------------------------------------------------------------------
# 2. Generate .env
# ---------------------------------------------------------------------------
step "Generating .env"

if [[ -f .env ]]; then
    warn ".env already exists, keeping existing config"
else
    API_KEY=$(openssl rand -hex 32)
    EMB_TOKEN=$(openssl rand -hex 16)
    cat > .env << ENVEOF
HTTP_ADDR=:8081
ENVIRONMENT=development
DASHBOARD_API_KEYS=$API_KEY
EMBEDDING_ENDPOINT=http://bit-rag-embedder:8080
EMBEDDING_MODEL=voyage-4-nano
EMBEDDING_DIM=1024
EMBEDDING_POOLING=mean
EMBEDDING_TIMEOUT_S=30
EMBEDDING_API_KEY=$EMB_TOKEN
LLAMA_API_KEY=$EMB_TOKEN
QDRANT_URL=http://bit-rag-qdrant:6333
QDRANT_API_KEY=
DB_PATH=/app/data/dashboard.db
ACTIVE_MODEL=voyage_nano_1024
ACTIVE_BACKEND=llama_q8
MCP_ENABLED=false
ENVEOF
    ok ".env created with random API key"
    info "Your API key: $API_KEY"
fi

# Export .env for docker compose
set -a
source .env
set +a

# ---------------------------------------------------------------------------
# 3. Deploy Docker stack
# ---------------------------------------------------------------------------
if [[ $SKIP_DOCKER -eq 1 ]]; then
    step "Skipping Docker deploy (--skip-docker)"
else
    step "Deploying Docker stack (Qdrant + embedder + dashboard)"

    # Create external network
    if ! docker network ls --format '{{.Name}}' | grep -q '^bit-rag-external$'; then
        docker network create bit-rag-external >/dev/null
        ok "Created network bit-rag-external"
    else
        ok "Network bit-rag-external exists"
    fi

    # Qdrant
    info "Starting Qdrant..."
    if ! docker compose -f docker-compose.qdrant.yml up -d 2>/dev/null; then
        if docker ps --format '{{.Names}}' | grep -q '^bit-rag-qdrant$'; then
            ok "Qdrant already running"
        else
            err "Qdrant deploy failed"
            exit 1
        fi
    else
        ok "Qdrant started"
    fi

    # Wait for Qdrant
    info "Waiting for Qdrant..."
    for i in $(seq 1 30); do
        status=$(docker inspect --format '{{.State.Health.Status}}' bit-rag-qdrant 2>/dev/null || echo "")
        [[ "$status" == "healthy" ]] && break
        sleep 1
    done
    [[ "$status" == "healthy" ]] && ok "Qdrant is healthy" || warn "Qdrant not healthy yet"

    # Embedder + dashboard
    # Strategy: only recreate containers that don't exist or aren't running.
    # If embedder is already running (e.g. user switched to GPU mode via
    # dashboard UI), keep it — don't destroy GPU config.
    EMB_RUNNING=$(docker ps --filter name=bit-rag-embedder --format '{{.Names}}' 2>/dev/null | grep -c '^bit-rag-embedder$' || true)
    DASH_RUNNING=$(docker ps --filter name=bit-rag-dashboard --format '{{.Names}}' 2>/dev/null | grep -c '^bit-rag-dashboard$' || true)

    # Build dashboard image (always, to pick up code changes)
    info "Building dashboard image..."
    docker compose build dashboard >/dev/null 2>&1 || {
        err "Dashboard image build failed"
        info "Try: docker compose build dashboard"
        exit 1
    }
    ok "Dashboard image built"

    # Embedder: only build + start if not already running
    if [[ "$EMB_RUNNING" -eq 0 ]]; then
        # Remove stale container if exists (stopped/crashed)
        docker rm -f bit-rag-embedder 2>/dev/null || true

        info "Building embedder image..."
        docker compose build embedder >/dev/null 2>&1 || {
            err "Embedder image build failed"
            exit 1
        }
        ok "Embedder image built"

        docker compose up -d --no-deps embedder >/dev/null 2>&1 || {
            err "Embedder failed to start"
            exit 1
        }
        ok "Embedder started"
    else
        ok "Embedder already running (keeping existing config)"
    fi

    # Dashboard: always recreate to pick up code changes + new mounts
    if [[ "$DASH_RUNNING" -gt 0 ]]; then
        docker rm -f bit-rag-dashboard >/dev/null 2>&1 || true
    fi
    docker compose up -d --no-deps dashboard >/dev/null 2>&1 || {
        err "Dashboard failed to start"
        exit 1
    }
    ok "Dashboard started"

    # Wait for dashboard
    info "Waiting for dashboard..."
    DASHBOARD_URL="http://localhost:8081"
    for i in $(seq 1 20); do
        if curl -sf "$DASHBOARD_URL/healthz" >/dev/null 2>&1; then
            ok "Dashboard is healthy"
            break
        fi
        sleep 1
    done
    curl -sf "$DASHBOARD_URL/healthz" >/dev/null 2>&1 || {
        warn "Dashboard not responding on :8081 yet"
        info "Check: docker logs bit-rag-dashboard"
    }
fi

# ---------------------------------------------------------------------------
# 4. Build MCP binary
# ---------------------------------------------------------------------------
if [[ $SKIP_MCP -eq 1 ]]; then
    step "Skipping MCP build (--skip-mcp)"
else
    step "Building MCP binary"

    INSTALL_DIR="${HOME}/.local/bin"
    mkdir -p "$INSTALL_DIR"
    INSTALL_PATH="$INSTALL_DIR/bit-rag-mcp"

    CGO_ENABLED=1 go build -trimpath -ldflags "-s -w" -o "$INSTALL_PATH" ./cmd/mcp || {
        err "MCP build failed"
        exit 1
    }

    SIZE=$(du -h "$INSTALL_PATH" | cut -f1)
    ok "MCP binary built ($SIZE): $INSTALL_PATH"

    # Ensure on PATH
    case ":$PATH:" in
        *":$INSTALL_DIR:"*) ;;
        *) warn "$INSTALL_DIR is not on your PATH"
           info "Add to shell: export PATH=\"$INSTALL_DIR:\$PATH\"" ;;
    esac
fi

# ---------------------------------------------------------------------------
# 5. Print MCP config
# ---------------------------------------------------------------------------
step "Setup complete!"

DASHBOARD_URL="http://localhost:8081"
API_KEY="${DASHBOARD_API_KEYS:-}"

echo ""
printf "Dashboard: \033[1m%s\033[0m\n" "$DASHBOARD_URL"
printf "API Key:   \033[1m%s\033[0m\n" "$API_KEY"
echo ""

if [[ $SKIP_MCP -eq 0 ]]; then
    INSTALL_PATH="$HOME/.local/bin/bit-rag-mcp"

    echo "Add this to your AI client config:"
    echo ""

    printf "\033[36m--- Claude Desktop / Cursor / Continue ---\033[0m\n"
    cat << CFGEOF
{
  "mcpServers": {
    "bit-rag": {
      "command": "$INSTALL_PATH",
      "env": {
        "DASHBOARD_URL": "$DASHBOARD_URL",
        "DASHBOARD_API_KEY": "$API_KEY"
      }
    }
  }
}
CFGEOF
    echo ""

    printf "\033[36m--- Config file locations ---\033[0m\n"
    echo "  Claude Desktop: ~/Library/Application Support/Claude/claude_desktop_config.json"
    echo "  Cursor:         ~/.cursor/mcp.json"
    echo "  OpenCode:       ~/.opencode/config.json"
    echo ""

    printf "\033[36m--- Skill files ---\033[0m\n"
    echo "  Factory:  cp skills/factory/bit-rag.md ~/.factory/skills/"
    echo "  OpenCode: cp skills/opencode/bit-rag.md ~/.opencode/skills/"
    echo ""
fi

printf "\033[33mNext steps:\033[0m\n"
echo "  1. Add MCP config to your AI tool (see above)"
echo "  2. Restart your AI tool"
echo "  3. Ask your agent: 'search for JWT validation in my project'"
echo ""
