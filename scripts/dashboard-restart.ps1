# dashboard-restart.ps1 — recreate bit-rag-dashboard on the canonical port 8090.
#
# Run this AFTER restarting Rancher Desktop (or whenever the dashboard misbehaves).
# It:
#   1. Waits for docker daemon
#   2. Applies the iptables ACCEPT fix for inter-container traffic
#   3. Recreates bit-rag-dashboard on port 8090 (stable)
#   4. Ensures qdrant + embedder running on bit-rag-external network
#   5. Verifies endpoints
#
# Usage:
#   powershell -ExecutionPolicy Bypass -File scripts\dashboard-restart.ps1

$ErrorActionPreference = 'Continue'
$DashboardPort = 8090

Write-Host "=== bit-rag dashboard restart on port $DashboardPort ===" -ForegroundColor Cyan

# [1/5] wait for docker
Write-Host "[1/5] Waiting for docker daemon..." -NoNewline
$tries = 0
while ($tries -lt 30) {
    docker ps *>$null 2>$null
    if ($LASTEXITCODE -eq 0) { break }
    Start-Sleep 2; Write-Host '.' -NoNewline; $tries++
}
if ($LASTEXITCODE -ne 0) {
    Write-Host ' FAILED' -ForegroundColor Red
    Write-Host 'Rancher Desktop is not running. Open it from the Start menu first.' -ForegroundColor Red
    exit 1
}
Write-Host ' OK' -ForegroundColor Green

# [2/5] apply iptables fix (inter-container forward)
Write-Host '[2/5] Applying iptables ACCEPT rules for bit-rag-external bridge...'
wsl -d rancher-desktop -- sh /mnt/c/Users/brainplusplus/.fix-docker-iptables.sh 2>&1 | Select-Object -Last 3 | ForEach-Object { Write-Host "    $_" }

# [3/5] ensure qdrant + embedder on bit-rag-external
Write-Host '[3/5] Starting qdrant + embedder...'
docker start bit-rag-qdrant bit-rag-embedder 2>$null | Out-Null
Start-Sleep 3

# Make sure embedder is on bit-rag-external network (with alias 'embedder')
$nets = docker inspect bit-rag-embedder --format '{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}' 2>$null
if ($nets -notmatch 'bit-rag-external') {
    Write-Host '    attaching embedder to bit-rag-external...'
    docker network connect --alias embedder bit-rag-external bit-rag-embedder 2>&1 | Out-Null
}

# [4/5] recreate dashboard on stable port
Write-Host "[4/5] Recreating bit-rag-dashboard on port $DashboardPort..."
docker rm -f bit-rag-dashboard 2>$null | Out-Null
docker run -d --name bit-rag-dashboard `
    --network bit-rag-external `
    --restart unless-stopped `
    -p "${DashboardPort}:8081" `
    -v bit-rag-data:/app/data `
    -v /var/run/docker.sock:/var/run/docker.sock `
    -e ENVIRONMENT=development `
    -e EMBEDDER_URL=http://embedder:8080 `
    -e QDRANT_URL=http://qdrant:6333 `
    -e EMBEDDING_ENDPOINT=http://embedder:8080 `
    -e DATABASE_PATH=/app/data/dashboard.db `
    bit-rag-dashboard:latest 2>&1 | Out-Null
Start-Sleep 6

# [5/5] verify
Write-Host '[5/5] Verifying...'
$tries = 0
$ok = $false
while ($tries -lt 15) {
    $r = curl.exe -s -m 3 "http://localhost:$DashboardPort/healthz" 2>$null
    if ($r -match 'ok') { $ok = $true; break }
    Start-Sleep 2; $tries++
}

Write-Host ''
Write-Host '=== Status ===' -ForegroundColor Cyan
docker ps --filter 'name=bit-rag' --format 'table {{.Names}}\t{{.Status}}\t{{.Ports}}'

Write-Host ''
if ($ok) {
    Write-Host '=== SUCCESS — dashboard reachable ===' -ForegroundColor Green
    Write-Host "  http://localhost:$DashboardPort/ui/settings" -ForegroundColor Yellow
    Write-Host "  http://localhost:$DashboardPort/ui/models" -ForegroundColor Yellow
    Write-Host "  http://localhost:$DashboardPort/ui/projects" -ForegroundColor Yellow
} else {
    Write-Host '=== Windows port forward still recovering ===' -ForegroundColor Yellow
    Write-Host "Dashboard works from WSL: wsl -d rancher-desktop -- curl http://localhost:$DashboardPort/healthz" -ForegroundColor Gray
    Write-Host 'If localhost stays unreachable for > 1 minute, restart Rancher Desktop from the tray icon.' -ForegroundColor Gray
}
