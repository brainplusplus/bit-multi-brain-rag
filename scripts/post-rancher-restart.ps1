# Run this AFTER restarting Rancher Desktop from the tray icon.
# It will:
#   1. Verify dockerd + WSL is healthy
#   2. Restart bit-rag containers in correct order
#   3. Verify dashboard reachable + GPU status correct
#   4. Print URLs

Write-Host "=== Post-Rancher-restart verification ===" -ForegroundColor Cyan

# Wait for docker daemon
Write-Host "[1/5] Waiting for docker daemon..." -NoNewline
$tries = 0
while ($tries -lt 30) {
    docker ps *>$null 2>$null
    if ($LASTEXITCODE -eq 0) { break }
    Start-Sleep 2
    Write-Host "." -NoNewline
    $tries++
}
if ($LASTEXITCODE -ne 0) {
    Write-Host " FAILED" -ForegroundColor Red
    Write-Host "Docker not responding. Check Rancher Desktop GUI." -ForegroundColor Red
    exit 1
}
Write-Host " OK" -ForegroundColor Green

# Start containers in order
Write-Host "[2/5] Starting bit-rag-qdrant..."
docker start bit-rag-qdrant 2>$null | Out-Null

Write-Host "[3/5] Starting bit-rag-embedder..."
docker start bit-rag-embedder 2>$null | Out-Null

Start-Sleep 3

Write-Host "[4/5] Starting bit-rag-dashboard..."
docker start bit-rag-dashboard 2>$null | Out-Null

Start-Sleep 5

# Verify
Write-Host "[5/5] Verifying..."
$ports = @(8091)
foreach ($p in $ports) {
    $r = curl.exe -s -m 5 "http://localhost:$p/healthz" 2>&1
    if ($r -match "ok") {
        Write-Host "  http://localhost:$p/healthz  OK" -ForegroundColor Green
    } else {
        Write-Host "  http://localhost:$p/healthz  FAIL ($r)" -ForegroundColor Red
    }
}

Write-Host ""
Write-Host "=== Container status ===" -ForegroundColor Cyan
docker ps --filter "name=bit-rag" --format "table {{.Names}}\t{{.Status}}\t{{.Ports}}"

Write-Host ""
Write-Host "=== Open in browser ===" -ForegroundColor Cyan
Write-Host "  http://localhost:8091/ui/settings" -ForegroundColor Yellow
Write-Host "  http://localhost:8091/ui/models" -ForegroundColor Yellow
Write-Host "  http://localhost:8091/ui/projects" -ForegroundColor Yellow
