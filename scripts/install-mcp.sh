#!/usr/bin/env bash
# =============================================================================
# install-mcp.sh — Install bit-rag MCP binary on Linux/macOS.
#
# What it does:
#   1. Verifies Go is installed
#   2. Builds bit-rag-mcp from ./cmd/mcp
#   3. Installs to ~/.local/bin/bit-rag-mcp (or $INSTALL_DIR if overridden)
#   4. (optional) Probes the configured DASHBOARD_URL to verify connectivity
#   5. Prints copy-paste-ready MCP client config snippets
#
# Usage:
#   ./scripts/install-mcp.sh                                    # build + install
#   DASHBOARD_URL=... DASHBOARD_API_KEY=... ./scripts/install-mcp.sh --test
#   INSTALL_DIR=/usr/local/bin ./scripts/install-mcp.sh         # custom dir
#   ./scripts/install-mcp.sh --uninstall                        # remove
#
# Run from REPO ROOT (so go build can find cmd/mcp).
# =============================================================================
set -euo pipefail

INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"
BINARY_NAME="bit-rag-mcp"
INSTALL_PATH="$INSTALL_DIR/$BINARY_NAME"

TEST_MODE=0
UNINSTALL_MODE=0
for arg in "$@"; do
    case "$arg" in
        --test) TEST_MODE=1 ;;
        --uninstall) UNINSTALL_MODE=1 ;;
        --help|-h)
            grep -E '^# ' "$0" | sed 's/^# //' | head -30
            exit 0
            ;;
        *) echo "Unknown arg: $arg" >&2; exit 2 ;;
    esac
done

step() { printf "\033[36m→ %s\033[0m\n" "$*"; }
ok()   { printf "\033[32m✓ %s\033[0m\n" "$*"; }
warn() { printf "\033[33m! %s\033[0m\n" "$*"; }
err()  { printf "\033[31m✗ %s\033[0m\n" "$*" >&2; }

# --- Uninstall ---
if [[ $UNINSTALL_MODE -eq 1 ]]; then
    if [[ -f "$INSTALL_PATH" ]]; then
        rm -f "$INSTALL_PATH"
        ok "Removed $INSTALL_PATH"
    else
        warn "Not installed at $INSTALL_PATH (nothing to remove)"
    fi
    exit 0
fi

# --- 1. Check Go ---
step "Checking for Go toolchain..."
if ! command -v go >/dev/null 2>&1; then
    err "Go is not installed. Install from https://go.dev/dl/ (1.24+ required)."
    exit 1
fi
GO_VERSION=$(go version | awk '{print $3}' | sed 's/go//')
ok "Go $GO_VERSION found at $(command -v go)"

# --- 2. Verify repo layout ---
step "Verifying repo layout..."
if [[ ! -f ./cmd/mcp/main.go ]]; then
    err "Cannot find ./cmd/mcp/main.go. Run this script from the bit-multi-brain-rag repo root."
    exit 1
fi
if [[ ! -f ./go.mod ]]; then
    err "Cannot find ./go.mod. Run from repo root."
    exit 1
fi
ok "Repo layout OK"

# --- 3. Check C toolchain (tree-sitter needs CGO) ---
step "Checking C toolchain (required by tree-sitter)..."
if ! command -v cc >/dev/null 2>&1 && ! command -v gcc >/dev/null 2>&1 && ! command -v clang >/dev/null 2>&1; then
    err "No C compiler found. Install build-essential (Debian/Ubuntu), Xcode CLT (macOS), or base-devel (Arch)."
    exit 1
fi
ok "C compiler available"

# --- 4. Build ---
step "Building $BINARY_NAME..."
TMP_OUT=$(mktemp)
CGO_ENABLED=1 go build -trimpath -ldflags "-s -w" -o "$TMP_OUT" ./cmd/mcp
SIZE_MB=$(du -m "$TMP_OUT" | awk '{print $1}')
ok "Built $TMP_OUT (${SIZE_MB} MB)"

# --- 5. Install ---
step "Installing to $INSTALL_PATH..."
mkdir -p "$INSTALL_DIR"
mv -f "$TMP_OUT" "$INSTALL_PATH"
chmod +x "$INSTALL_PATH"
ok "Installed $INSTALL_PATH"

# --- 6. Optional connectivity test ---
if [[ $TEST_MODE -eq 1 ]]; then
    step "Running connectivity probe..."
    if [[ -z "${DASHBOARD_URL:-}" ]]; then
        err "--test requires DASHBOARD_URL env"
        exit 1
    fi
    if [[ -z "${DASHBOARD_API_KEY:-}" ]]; then
        err "--test requires DASHBOARD_API_KEY env"
        exit 1
    fi
    # MCP boots, probes /healthz, then waits for stdio. EOF immediately.
    if echo "" | "$INSTALL_PATH" 2>&1; then
        ok "Dashboard reachable; MCP server healthy"
    else
        err "MCP boot failed"
        exit 1
    fi
fi

# --- 7. Print next steps ---
cat <<EOF

═══════════════════════════════════════════════════════════════════════
INSTALLED
═══════════════════════════════════════════════════════════════════════

Binary: $INSTALL_PATH

Make sure $INSTALL_DIR is on your PATH. Add to ~/.bashrc / ~/.zshrc:
  export PATH="\$HOME/.local/bin:\$PATH"

Add this to your MCP client config (see docs/INSTALL-MCP-LOCAL.md):

{
  "mcpServers": {
    "bit-rag": {
      "command": "$INSTALL_PATH",
      "env": {
        "DASHBOARD_URL":     "https://bit-rag.your-domain.com",
        "DASHBOARD_API_KEY": "your-strong-key"
      }
    }
  }
}

Client config paths:
  Claude Desktop (macOS):   ~/Library/Application Support/Claude/claude_desktop_config.json
  Claude Desktop (Linux):   ~/.config/Claude/claude_desktop_config.json
  Factory:                  ~/.factory/config.json (mcp section)
  OpenCode:                 ~/.opencode/config.json (mcp section)
  Cursor:                   ~/.cursor/mcp.json
  Continue:                 ~/.continue/config.json
  Windsurf:                 ~/.codeium/windsurf/mcp_config.json

EOF
