# =============================================================================
# setup.ps1 — Full setup wizard for bit-multi-brain-rag (Windows).
#
# What it does:
#   1. Check prerequisites (Docker, Go)
#   2. Generate .env with a random API key (if not exists)
#   3. Create Docker network + deploy Qdrant + embedder + dashboard
#   4. Build MCP binary locally
#   5. Print ready-to-paste MCP config for your AI tool
#
# Usage:
#   .\scripts\setup.ps1                          # full setup
#   .\scripts\setup.ps1 -SkipDocker              # skip Docker deploy (already running)
#   .\scripts\setup.ps1 -SkipMCP                 # skip MCP build
#   .\scripts\setup.ps1 -Uninstall               # stop + remove containers
#
# Run from REPO ROOT.
# =============================================================================
[CmdletBinding()]
param(
    [switch] $SkipDocker,
    [switch] $SkipMCP,
    [switch] $Uninstall
)

$ErrorActionPreference = "Stop"

function Write-Step($msg)  { Write-Host "`n=== $msg ===" -ForegroundColor Cyan }
function Write-OK($msg)    { Write-Host "  [OK] $msg" -ForegroundColor Green }
function Write-Warn($msg)  { Write-Host "  [!]  $msg" -ForegroundColor Yellow }
function Write-Err($msg)   { Write-Host "  [X]  $msg" -ForegroundColor Red }
function Write-Info($msg)  { Write-Host "  ..  $msg" -ForegroundColor DarkGray }

$RepoRoot = Split-Path -Parent $PSScriptRoot

# ---------------------------------------------------------------------------
# Uninstall mode
# ---------------------------------------------------------------------------
if ($Uninstall) {
    Write-Step "Uninstalling bit-rag"
    docker stop bit-rag-dashboard bit-rag-embedder bit-rag-qdrant 2>$null
    docker rm bit-rag-dashboard bit-rag-embedder bit-rag-qdrant 2>$null
    Write-OK "Containers stopped and removed"
    Write-Warn "Docker volumes (data) preserved. Remove manually if needed:"
    Write-Info "  docker volume rm bit-rag-data qdrant-storage qdrant-snapshots"
    exit 0
}

# ---------------------------------------------------------------------------
# 1. Check prerequisites
# ---------------------------------------------------------------------------
Write-Step "Checking prerequisites"

# Docker
$docker = Get-Command docker -ErrorAction SilentlyContinue
if (-not $docker) {
    Write-Err "Docker is not installed."
    Write-Info "Install Docker Desktop or Rancher Desktop:"
    Write-Info "  https://www.docker.com/products/docker-desktop/"
    Write-Info "  https://rancherdesktop.io/"
    exit 1
}
Write-OK "Docker found"

# Check docker daemon is running
$ErrorActionPreference = "SilentlyContinue"
docker info 2>$null | Out-Null
$dockerExit = $LASTEXITCODE
$ErrorActionPreference = "Stop"
if ($dockerExit -ne 0) {
    Write-Err "Docker daemon is not running. Start Docker Desktop / Rancher Desktop."
    exit 1
}
Write-OK "Docker daemon is running"

# Go (only needed if building MCP)
if (-not $SkipMCP) {
    $go = Get-Command go -ErrorAction SilentlyContinue
    if (-not $go) {
        Write-Err "Go is not installed (needed to build MCP binary)."
        Write-Info "Install from https://go.dev/dl/ (1.24+ required)."
        Write-Info "Or run with -SkipMCP if you already have the binary."
        exit 1
    }
    $goVer = (go version) -replace "go version go([\d\.]+).*", '$1'
    Write-OK "Go $goVer found"
}

# ---------------------------------------------------------------------------
# 2. Generate .env
# ---------------------------------------------------------------------------
Write-Step "Generating .env"

$envFile = Join-Path $RepoRoot ".env"
if (Test-Path $envFile) {
    Write-Warn ".env already exists, keeping existing config"
} else {
    # Generate random API key
    $bytes = New-Object byte[] 32
    [System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
    $apiKey = -join ($bytes | ForEach-Object { "{0:x2}" -f $_ })

    # Generate random embedder token
    $embBytes = New-Object byte[] 16
    [System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($embBytes)
    $embToken = -join ($embBytes | ForEach-Object { "{0:x2}" -f $_ })

    $envContent = @"
HTTP_ADDR=:8081
ENVIRONMENT=development
DASHBOARD_API_KEYS=$apiKey
EMBEDDING_ENDPOINT=http://bit-rag-embedder:8080
EMBEDDING_MODEL=voyage-4-nano
EMBEDDING_DIM=1024
EMBEDDING_POOLING=mean
EMBEDDING_TIMEOUT_S=30
EMBEDDING_API_KEY=$embToken
LLAMA_API_KEY=$embToken
QDRANT_URL=http://bit-rag-qdrant:6333
QDRANT_API_KEY=
DB_PATH=/app/data/dashboard.db
ACTIVE_MODEL=voyage_nano_1024
ACTIVE_BACKEND=llama_q8
MCP_ENABLED=false
"@
    Set-Content -Path $envFile -Value $envContent -Encoding ascii
    Write-OK ".env created with random API key"
    Write-Info "Your API key: $apiKey"
}

# Load .env for docker compose
Get-Content $envFile | ForEach-Object {
    if ($_ -match "^([^#=]+)=(.*)$") {
        Set-Item -Path "Env:$($matches[1].Trim())" -Value $matches[2].Trim()
    }
}

# ---------------------------------------------------------------------------
# 3. Deploy Docker stack
# ---------------------------------------------------------------------------
if ($SkipDocker) {
    Write-Step "Skipping Docker deploy (-SkipDocker)"
} else {
    Write-Step "Deploying Docker stack (Qdrant + embedder + dashboard)"

    # Create external network if not exists
    $netExists = docker network ls --filter name=bit-rag-external --format "{{.Name}}" 2>&1
    if (-not ($netExists -match "bit-rag-external")) {
        docker network create bit-rag-external 2>&1 | Out-Null
        Write-OK "Created network bit-rag-external"
    } else {
        Write-OK "Network bit-rag-external exists"
    }

    # Deploy Qdrant first
    Write-Info "Starting Qdrant..."
    $ErrorActionPreference = "SilentlyContinue"
    docker compose -f (Join-Path $RepoRoot "docker-compose.qdrant.yml") up -d 2>$null
    $qdrantExit = $LASTEXITCODE
    $ErrorActionPreference = "Stop"
    if ($qdrantExit -ne 0) {
        $qdrantRunning = docker ps --filter name=bit-rag-qdrant --format "{{.Names}}" 2>$null
        if ($qdrantRunning -match "bit-rag-qdrant") {
            Write-OK "Qdrant already running"
        } else {
            Write-Err "Qdrant deploy failed"
            exit 1
        }
    } else {
        Write-OK "Qdrant started"
    }

    # Wait for Qdrant health
    Write-Info "Waiting for Qdrant to be healthy..."
    $maxWait = 30
    for ($i = 0; $i -lt $maxWait; $i++) {
        Start-Sleep -Seconds 1
        $status = docker inspect --format "{{.State.Health.Status}}" bit-rag-qdrant 2>&1
        if ($status -eq "healthy") { break }
    }
    if ($status -eq "healthy") {
        Write-OK "Qdrant is healthy"
    } else {
        Write-Warn "Qdrant not healthy yet (continuing anyway)"
    }

    # Deploy embedder + dashboard.
    # Strategy: only recreate containers that don't exist or aren't running.
    # If embedder is already running (e.g. user switched to GPU mode via
    # dashboard UI), keep it — don't destroy GPU config.
    Write-Info "Starting embedder + dashboard..."

    # Check what's already running
    $embRunning = docker ps --filter name=bit-rag-embedder --format "{{.Names}}" 2>$null
    $dashRunning = docker ps --filter name=bit-rag-dashboard --format "{{.Names}}" 2>$null

    # Build images (always, to pick up code changes)
    Write-Info "Building dashboard image..."
    $ErrorActionPreference = "SilentlyContinue"
    docker compose build dashboard 2>$null
    $buildExit = $LASTEXITCODE
    $ErrorActionPreference = "Stop"
    if ($buildExit -ne 0) {
        Write-Err "Dashboard image build failed"
        Write-Info "Try manually: docker compose build dashboard"
        exit 1
    }
    Write-OK "Dashboard image built"

    # Embedder: only build + start if not already running
    if ($embRunning -notmatch "bit-rag-embedder") {
        # Remove stale container if exists (stopped/crashed)
        $embStale = docker ps -a --filter name=bit-rag-embedder --format "{{.Names}}" 2>$null
        if ($embStale -match "bit-rag-embedder") {
            $ErrorActionPreference = "SilentlyContinue"
            docker rm -f bit-rag-embedder 2>$null
            $ErrorActionPreference = "Stop"
        }

        Write-Info "Building embedder image..."
        $ErrorActionPreference = "SilentlyContinue"
        docker compose build embedder 2>$null
        $embBuildExit = $LASTEXITCODE
        $ErrorActionPreference = "Stop"
        if ($embBuildExit -ne 0) {
            Write-Err "Embedder image build failed"
            exit 1
        }
        Write-OK "Embedder image built"

        $ErrorActionPreference = "SilentlyContinue"
        docker compose up -d --no-deps embedder 2>$null
        $ErrorActionPreference = "Stop"
        $embCheck = docker ps --filter name=bit-rag-embedder --format "{{.Names}}" 2>$null
        if ($embCheck -notmatch "bit-rag-embedder") {
            Write-Err "Embedder failed to start"
            exit 1
        }
        Write-OK "Embedder started"
    } else {
        Write-OK "Embedder already running (keeping existing config)"
    }

    # Dashboard: always recreate to pick up code changes + new mounts
    if ($dashRunning -match "bit-rag-dashboard") {
        $ErrorActionPreference = "SilentlyContinue"
        docker rm -f bit-rag-dashboard 2>$null
        $ErrorActionPreference = "Stop"
    }
    $ErrorActionPreference = "SilentlyContinue"
    docker compose up -d --no-deps dashboard 2>$null
    $dashExit = $LASTEXITCODE
    $ErrorActionPreference = "Stop"
    if ($dashExit -ne 0) {
        Write-Err "Dashboard failed to start"
        exit 1
    }
    Write-OK "Dashboard started"

    # Wait for dashboard health
    Write-Info "Waiting for dashboard..."
    Start-Sleep -Seconds 3
    for ($i = 0; $i -lt 20; $i++) {
        try {
            $healthz = Invoke-RestMethod -Uri "http://localhost:8081/healthz" -TimeoutSec 3
            if ($healthz.status -eq "ok") { break }
        } catch { Start-Sleep -Seconds 1 }
    }
    try {
        $healthz = Invoke-RestMethod -Uri "http://localhost:8081/healthz" -TimeoutSec 5
        Write-OK "Dashboard is healthy ($($healthz.status))"
    } catch {
        Write-Warn "Dashboard not responding on :8081 yet"
        Write-Info "Check: docker logs bit-rag-dashboard"
    }
}

# ---------------------------------------------------------------------------
# 4. Build MCP binary
# ---------------------------------------------------------------------------
if ($SkipMCP) {
    Write-Step "Skipping MCP build (-SkipMCP)"
} else {
    Write-Step "Building MCP binary"

    $installDir = "$env:LOCALAPPDATA\Programs\bit-rag"
    $installPath = Join-Path $installDir "bit-rag-mcp.exe"

    Push-Location $RepoRoot
    $env:CGO_ENABLED = "1"
    & go build -trimpath -ldflags "-s -w" -o $installPath ./cmd/mcp
    $buildExit = $LASTEXITCODE
    Pop-Location

    if ($buildExit -ne 0) {
        Write-Err "MCP build failed"
        exit 1
    }

    $size = [math]::Round((Get-Item $installPath).Length / 1MB, 1)
    Write-OK "MCP binary built ($size MB): $installPath"
}

# ---------------------------------------------------------------------------
# 5. Print MCP config
# ---------------------------------------------------------------------------
Write-Step "Setup complete!"

$dashboardUrl = "http://localhost:8081"
$apiKey = $env:DASHBOARD_API_KEYS

Write-Host ""
Write-Host "Dashboard: $dashboardUrl" -ForegroundColor White
Write-Host "API Key:   $apiKey" -ForegroundColor White
Write-Host ""

if (-not $SkipMCP) {
    $installPath = "$env:LOCALAPPDATA\Programs\bit-rag\bit-rag-mcp.exe"
    $escapedPath = $installPath -replace '\\', '\\\\'

    Write-Host "Add this to your AI client config:" -ForegroundColor White
    Write-Host ""

    # Detect common tools
    Write-Host "--- Factory Droid ---" -ForegroundColor DarkCyan
    Write-Host "  droid mcp add bit-rag `"$installPath`" --env DASHBOARD_URL=$dashboardUrl --env DASHBOARD_API_KEY=$apiKey" -ForegroundColor DarkGray
    Write-Host ""

    Write-Host "--- Claude Desktop / Cursor / Continue ---" -ForegroundColor DarkCyan
    $cfg = @"
{
  "mcpServers": {
    "bit-rag": {
      "command": "$escapedPath",
      "env": {
        "DASHBOARD_URL": "$dashboardUrl",
        "DASHBOARD_API_KEY": "$apiKey"
      }
    }
  }
}
"@
    Write-Host $cfg -ForegroundColor DarkGray
    Write-Host ""

    Write-Host "Config file locations:" -ForegroundColor DarkCyan
    Write-Host "  Claude Desktop: %APPDATA%\Claude\claude_desktop_config.json"
    Write-Host "  Cursor:         %USERPROFILE%\.cursor\mcp.json"
    Write-Host "  Factory Droid:  use 'droid mcp add' command above"
    Write-Host "  OpenCode:       ~/.opencode/config.json"
    Write-Host ""

    Write-Host "Skill files (auto-onboard for agents):" -ForegroundColor DarkCyan
    Write-Host "  Factory:  copy skills\factory\bit-rag.md to ~\.factory\skills\"
    Write-Host "  OpenCode: copy skills\opencode\bit-rag.md to ~/.opencode/skills/"
    Write-Host ""
}

Write-Host "Next steps:" -ForegroundColor Yellow
Write-Host "  1. Add MCP config to your AI tool (see above)"
Write-Host "  2. Restart your AI tool"
Write-Host "  3. Ask your agent: 'search for JWT validation in my project'"
Write-Host ""
