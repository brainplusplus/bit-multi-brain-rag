# =============================================================================
# install-mcp.ps1 — Install bit-rag MCP binary on Windows.
#
# What it does:
#   1. Verifies Go is installed
#   2. Builds bit-rag-mcp.exe from ./cmd/mcp
#   3. Installs to $env:LOCALAPPDATA\Programs\bit-rag\bit-rag-mcp.exe
#   4. (optional) Probes the configured DASHBOARD_URL to verify connectivity
#   5. Prints copy-paste-ready MCP client config snippets
#
# Usage:
#   .\scripts\install-mcp.ps1                                                # build + install
#   .\scripts\install-mcp.ps1 -DashboardUrl "https://x" -ApiKey "y" -Test    # + connectivity test
#   .\scripts\install-mcp.ps1 -InstallDir "C:\Tools\bit-rag"                 # custom install dir
#   .\scripts\install-mcp.ps1 -Uninstall                                     # remove
#
# Run from REPO ROOT (so go build can find cmd/mcp).
# =============================================================================

[CmdletBinding()]
param(
    [string] $DashboardUrl = $env:DASHBOARD_URL,
    [string] $ApiKey = $env:DASHBOARD_API_KEY,
    [string] $InstallDir = "$env:LOCALAPPDATA\Programs\bit-rag",
    [switch] $Test,
    [switch] $Uninstall
)

$ErrorActionPreference = "Stop"

function Write-Step($msg) { Write-Host "→ $msg" -ForegroundColor Cyan }
function Write-OK($msg)   { Write-Host "✓ $msg" -ForegroundColor Green }
function Write-Warn($msg) { Write-Host "! $msg" -ForegroundColor Yellow }
function Write-Err($msg)  { Write-Host "✗ $msg" -ForegroundColor Red }

$binaryName = "bit-rag-mcp.exe"
$installPath = Join-Path $InstallDir $binaryName

# --- Uninstall mode ---
if ($Uninstall) {
    if (Test-Path $installPath) {
        Remove-Item $installPath -Force
        Write-OK "Removed $installPath"
    } else {
        Write-Warn "Not installed at $installPath (nothing to remove)"
    }
    exit 0
}

# --- 1. Check Go ---
Write-Step "Checking for Go toolchain..."
$go = Get-Command go -ErrorAction SilentlyContinue
if (-not $go) {
    Write-Err "Go is not installed. Install from https://go.dev/dl/ (1.24+ required)."
    exit 1
}
$goVersion = (go version) -replace "go version go([\d\.]+).*", '$1'
Write-OK "Go $goVersion found at $($go.Source)"

# --- 2. Verify repo layout ---
Write-Step "Verifying repo layout..."
if (-not (Test-Path ".\cmd\mcp\main.go")) {
    Write-Err "Cannot find .\cmd\mcp\main.go. Run this script from the bit-multi-brain-rag repo root."
    exit 1
}
if (-not (Test-Path ".\go.mod")) {
    Write-Err "Cannot find .\go.mod. Run this script from the bit-multi-brain-rag repo root."
    exit 1
}
Write-OK "Repo layout OK"

# --- 3. Build ---
Write-Step "Building $binaryName (CGO required for tree-sitter dep)..."
$tmpOut = Join-Path $env:TEMP "bit-rag-mcp-build-$(Get-Random).exe"
$env:CGO_ENABLED = "1"
& go build -trimpath -ldflags "-s -w" -o $tmpOut .\cmd\mcp
if ($LASTEXITCODE -ne 0) {
    Write-Err "go build failed (exit $LASTEXITCODE)"
    exit $LASTEXITCODE
}
$size = [math]::Round((Get-Item $tmpOut).Length / 1MB, 1)
Write-OK "Built $tmpOut (${size} MB)"

# --- 4. Install ---
Write-Step "Installing to $installPath..."
if (-not (Test-Path $InstallDir)) {
    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
}
Move-Item -Path $tmpOut -Destination $installPath -Force
Write-OK "Installed $installPath"

# --- 5. Optional connectivity test ---
if ($Test) {
    Write-Step "Running connectivity probe..."
    if ([string]::IsNullOrWhiteSpace($DashboardUrl)) {
        Write-Err "-Test requires -DashboardUrl (or `$env:DASHBOARD_URL)"
        exit 1
    }
    if ([string]::IsNullOrWhiteSpace($ApiKey)) {
        Write-Err "-Test requires -ApiKey (or `$env:DASHBOARD_API_KEY)"
        exit 1
    }
    $env:DASHBOARD_URL = $DashboardUrl
    $env:DASHBOARD_API_KEY = $ApiKey
    # MCP boots, runs /healthz probe, then waits for stdio. We send EOF immediately.
    $result = "" | & $installPath 2>&1
    if ($LASTEXITCODE -eq 0) {
        Write-OK "Dashboard reachable; MCP server healthy"
    } else {
        Write-Err "MCP boot failed (exit $LASTEXITCODE)"
        Write-Host $result
        exit $LASTEXITCODE
    }
}

# --- 6. Print next steps ---
Write-Host ""
Write-Host "═══════════════════════════════════════════════════════════════════════" -ForegroundColor DarkGray
Write-Host "INSTALLED" -ForegroundColor Green
Write-Host "═══════════════════════════════════════════════════════════════════════" -ForegroundColor DarkGray
Write-Host ""
Write-Host "Binary: $installPath"
Write-Host ""
Write-Host "Add this to your MCP client config (see docs/INSTALL-MCP-LOCAL.md for full guide):"
Write-Host ""
$cfg = @"
{
  "mcpServers": {
    "bit-rag": {
      "command": "$($installPath -replace '\\', '\\')",
      "env": {
        "DASHBOARD_URL":     "https://bit-rag.your-domain.com",
        "DASHBOARD_API_KEY": "your-strong-key"
      }
    }
  }
}
"@
Write-Host $cfg -ForegroundColor DarkCyan
Write-Host ""
Write-Host "Client config paths:"
Write-Host "  Claude Desktop: %APPDATA%\Claude\claude_desktop_config.json"
Write-Host "  Factory:        ~\.factory\config.json (mcp section)"
Write-Host "  OpenCode:       ~\.opencode\config.json (mcp section)"
Write-Host "  Cursor:         ~\.cursor\mcp.json"
Write-Host ""
