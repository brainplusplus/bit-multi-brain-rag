#!/usr/bin/env bash
# Install bit-rag dashboard as a background service on Linux/macOS.
#
# Linux:  systemd user service (auto-start on login, auto-restart)
# macOS:  launchd plist (auto-start on login, auto-restart)
#
# Usage:
#   ./scripts/install-service.sh              # Install
#   ./scripts/install-service.sh --uninstall  # Remove
#
# No root required. Uses user-scope services.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SERVICE_NAME="bit-rag-dashboard"

# --- Uninstall ---
if [[ "${1:-}" == "--uninstall" || "${1:-}" == "-u" ]]; then
    echo "Removing bit-rag dashboard service..."
    if [[ "$(uname)" == "Darwin" ]]; then
        launchctl bootout "gui/$(id -u)/com.$SERVICE_NAME" 2>/dev/null || true
        rm -f "$HOME/Library/LaunchAgents/com.$SERVICE_NAME.plist"
    else
        systemctl --user stop "$SERVICE_NAME" 2>/dev/null || true
        systemctl --user disable "$SERVICE_NAME" 2>/dev/null || true
        rm -f "$HOME/.config/systemd/user/$SERVICE_NAME.service"
        systemctl --user daemon-reload
    fi
    echo "Removed."
    exit 0
fi

# --- Load .env ---
ENV_FILE="$REPO_ROOT/.env"
if [[ ! -f "$ENV_FILE" ]]; then
    echo "ERROR: .env not found at $ENV_FILE" >&2
    exit 1
fi
load_env() {
    while IFS='=' read -r key value; do
        key="${key%%#*}"
        [[ -z "$key" ]] && continue
        key=$(echo "$key" | xargs)
        value=$(echo "$value" | xargs)
        eval "export $key=\"$value\""
    done < "$ENV_FILE"
}
load_env

# --- Build binary if needed ---
BIN_PATH="$REPO_ROOT/bin/bit-rag-dashboard"
if [[ ! -x "$BIN_PATH" ]]; then
    echo "Building dashboard binary..."
    cd "$REPO_ROOT"
    CGO_ENABLED=1 go build -o "$BIN_PATH" ./cmd/dashboard/
    if [[ $? -ne 0 ]]; then
        echo "Build failed!" >&2
        exit 1
    fi
fi

# --- Copy zvec shared lib next to binary ---
BIN_DIR="$(dirname "$BIN_PATH")"
LIB_DIR="$REPO_ROOT/lib"
if [[ "$(uname)" == "Darwin" ]]; then
    LIB_SRC="$LIB_DIR/darwin_arm64/libzvec_c_api.dylib"
    [[ ! -f "$LIB_SRC" ]] && LIB_SRC="$LIB_DIR/darwin_amd64/libzvec_c_api.dylib"
else
    LIB_SRC="$LIB_DIR/linux_amd64/libzvec_c_api.so"
fi
if [[ -f "$LIB_SRC" ]]; then
    cp "$LIB_SRC" "$BIN_DIR/" 2>/dev/null || true
    echo "Copied zvec shared lib"
fi

ZVEC_PATH="$REPO_ROOT/data/zvec"
DB_PATH="$REPO_ROOT/data/dashboard-local.db"
mkdir -p "$ZVEC_PATH" "$DB_PATH"
LOG_FILE="$REPO_ROOT/data/dashboard-svc.log"

# In embedded mode, embedder endpoint should be localhost (not Docker service name)
if [[ "${EMBEDDING_ENDPOINT:-}" == *"bit-rag-embedder"* ]]; then
    EMBEDDING_ENDPOINT="http://localhost:8090"
fi

# --- Install ---
if [[ "$(uname)" == "Darwin" ]]; then
    # macOS: launchd plist
    PLIST_DIR="$HOME/Library/LaunchAgents"
    PLIST_FILE="$PLIST_DIR/com.$SERVICE_NAME.plist"
    mkdir -p "$PLIST_DIR"

    cat > "$PLIST_FILE" << EOFPLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.$SERVICE_NAME</string>
    <key>ProgramArguments</key>
    <array>
        <string>$BIN_PATH</string>
    </array>
    <key>WorkingDirectory</key>
    <string>$BIN_DIR</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>HTTP_ADDR</key>
        <string>:8081</string>
        <key>ENVIRONMENT</key>
        <string>development</string>
        <key>ZVEC_PATH</key>
        <string>$ZVEC_PATH</string>
        <key>DB_PATH</key>
        <string>$DB_PATH</string>
        <key>QDRANT_URL</key>
        <string></string>
        <key>MCP_ENABLED</key>
        <string>false</string>
        <key>EMBEDDING_ENDPOINT</key>
        <string>${EMBEDDING_ENDPOINT:-}</string>
        <key>EMBEDDING_API_KEY</key>
        <string>${EMBEDDING_API_KEY:-}</string>
        <key>LLAMA_API_KEY</key>
        <string>${LLAMA_API_KEY:-}</string>
        <key>EMBEDDING_MODEL</key>
        <string>${EMBEDDING_MODEL:-voyage-4-nano}</string>
        <key>EMBEDDING_DIM</key>
        <string>${EMBEDDING_DIM:-1024}</string>
        <key>EMBEDDING_POOLING</key>
        <string>${EMBEDDING_POOLING:-mean}</string>
        <key>ACTIVE_MODEL</key>
        <string>${ACTIVE_MODEL:-voyage_nano_1024}</string>
        <key>ACTIVE_BACKEND</key>
        <string>${ACTIVE_BACKEND:-llama_q8}</string>
        <key>DASHBOARD_API_KEYS</key>
        <string>${DASHBOARD_API_KEYS:-}</string>
    </dict>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>$LOG_FILE</string>
    <key>StandardErrorPath</key>
    <string>$LOG_FILE</string>
</dict>
</plist>
EOFPLIST

    launchctl bootout "gui/$(id -u)/com.$SERVICE_NAME" 2>/dev/null || true
    launchctl bootstrap "gui/$(id -u)" "$PLIST_FILE"
    echo "launchd service installed and started."

else
    # Linux: systemd user service
    SVC_DIR="$HOME/.config/systemd/user"
    SVC_FILE="$SVC_DIR/$SERVICE_NAME.service"
    mkdir -p "$SVC_DIR"

    cat > "$SVC_FILE" << EOFUNIT
[Unit]
Description=bit-rag Dashboard (embedded mode)
After=network.target

[Service]
Type=simple
WorkingDirectory=$BIN_DIR
ExecStart=$BIN_PATH
Restart=always
RestartSec=3
Environment=HTTP_ADDR=:8081
Environment=ENVIRONMENT=development
Environment=ZVEC_PATH=$ZVEC_PATH
Environment=DB_PATH=$DB_PATH
Environment=QDRANT_URL=
Environment=MCP_ENABLED=false
Environment=EMBEDDING_ENDPOINT=${EMBEDDING_ENDPOINT:-}
Environment=EMBEDDING_API_KEY=${EMBEDDING_API_KEY:-}
Environment=LLAMA_API_KEY=${LLAMA_API_KEY:-}
Environment=EMBEDDING_MODEL=${EMBEDDING_MODEL:-voyage-4-nano}
Environment=EMBEDDING_DIM=${EMBEDDING_DIM:-1024}
Environment=EMBEDDING_POOLING=${EMBEDDING_POOLING:-mean}
Environment=ACTIVE_MODEL=${ACTIVE_MODEL:-voyage_nano_1024}
Environment=ACTIVE_BACKEND=${ACTIVE_BACKEND:-llama_q8}
Environment=DASHBOARD_API_KEYS=${DASHBOARD_API_KEYS:-}

[Install]
WantedBy=default.target
EOFUNIT

    # Enable lingering so service runs without active login session
    loginctl enable-linger "$(id -un)" 2>/dev/null || true

    systemctl --user daemon-reload
    systemctl --user enable "$SERVICE_NAME"
    systemctl --user start "$SERVICE_NAME"
    echo "systemd service installed and started."
fi

echo ""
echo "Dashboard: http://localhost:8081"
echo "Logs:      $LOG_FILE"
echo ""
echo "Uninstall: ./scripts/install-service.sh --uninstall"
