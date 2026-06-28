# Run bit-rag in embedded (zero-Docker-storage) mode on Windows.
# Prereqs:
#   - Docker embedder running (bit-rag-embedder) + embedder-proxy on port 8090
#   - Or: local llama-server binary (see EMBEDDER_BINARY mode in .env)
#
# Usage: .\scripts\run-local.ps1

$RepoRoot = Split-Path -Parent $PSScriptRoot
Set-Location $RepoRoot

# Load .env
$envFile = Join-Path $RepoRoot ".env"
if (Test-Path $envFile) {
    Get-Content $envFile | ForEach-Object {
        if ($_ -match "^([^#=]+)=(.*)$") {
            Set-Item -Path "Env:$($matches[1].Trim())" -Value $matches[2].Trim()
        }
    }
}

# Override for embedded mode
$env:HTTP_ADDR = ":8081"
$env:ENVIRONMENT = "development"
$env:ZVEC_PATH = Join-Path $RepoRoot "data\zvec"
$env:DB_PATH = Join-Path $RepoRoot "data\dashboard-local.db"
$env:EMBEDDING_ENDPOINT = "http://localhost:8090"  # embedder proxy
$env:QDRANT_URL = ""  # force zvec mode
$env:MCP_ENABLED = "false"

$binPath = Join-Path $RepoRoot "bin\bit-rag-dashboard.exe"
$dllPath = Join-Path $RepoRoot "lib\windows_amd64\zvec_c_api.dll"

# Copy DLL next to binary if not there
$binDir = Split-Path $binPath
if (-not (Test-Path (Join-Path $binDir "zvec_c_api.dll"))) {
    Copy-Item $dllPath $binDir -Force
}

Write-Host "Starting bit-rag dashboard (embedded mode)..." -ForegroundColor Cyan
Write-Host "  Dashboard:  http://localhost:8081" -ForegroundColor Green
Write-Host "  Storage:    zvec embedded ($env:ZVEC_PATH)" -ForegroundColor Green
Write-Host "  Embedder:   $env:EMBEDDING_ENDPOINT" -ForegroundColor Green
Write-Host "  Press Ctrl+C to stop." -ForegroundColor DarkGray
Write-Host ""

Set-Location $binDir
& $binPath
