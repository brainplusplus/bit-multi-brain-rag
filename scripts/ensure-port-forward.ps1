# ensure-port-forward.ps1
#
# Ensures bit-rag dashboard (port 8090) is reachable from Windows localhost.
# Rancher Desktop's vsock proxy is fragile after docker daemon restarts.
# This script:
#   1. Tests if localhost:PORT is reachable.
#   2. If not, sets up netsh portproxy as fallback (bypasses vsock entirely).
#   3. If vsock recovers later, cleans up the portproxy rule.
#
# Run manually or as a Windows Scheduled Task (every 5 min).
#
# Usage:
#   powershell -ExecutionPolicy Bypass -File scripts\ensure-port-forward.ps1
#
# Scheduled Task install (run once as admin):
#   schtasks /Create /TN "bit-rag-port-forward" /SC MINUTE /MO 5 `
#     /TR "powershell -ExecutionPolicy Bypass -WindowStyle Hidden -File \`"D:\golang\bit-rag\bit-multi-brain-rag\scripts\ensure-port-forward.ps1\`"" /F

param(
    [int]$Port = 8090,
    [int]$InternalPort = 8081,
    [string]$HealthPath = "/healthz"
)

$ErrorActionPreference = 'SilentlyContinue'

function Test-PortReachable {
    param([int]$Port, [string]$Path)
    try {
        $r = Invoke-RestMethod -Uri "http://localhost:$Port$Path" -TimeoutSec 3 -ErrorAction Stop
        return $r.status -eq "ok"
    } catch {
        return $false
    }
}

function Get-WSLIP {
    # Try multiple approaches to find the WSL IP that can reach the container.
    # Rancher Desktop mirrored mode: localhost works natively (no proxy needed).
    # Rancher Desktop NAT mode: need WSL eth1/eth0 IP.

    # Approach 1: eth1 (Rancher Desktop default in mirrored mode)
    $raw = (wsl -d rancher-desktop -- sh -c "ip -4 addr show eth1 2>/dev/null" 2>$null)
    if ($raw -match 'inet\s+(\d+\.\d+\.\d+\.\d+)') {
        return $Matches[1]
    }

    # Approach 2: eth0
    $raw = (wsl -d rancher-desktop -- sh -c "ip -4 addr show eth0 2>/dev/null" 2>$null)
    if ($raw -match 'inet\s+(\d+\.\d+\.\d+\.\d+)') {
        return $Matches[1]
    }

    return $null
}

function Add-PortProxy {
    param([int]$ListenPort, [int]$TargetPort, [string]$TargetIP)
    # netsh portproxy requires admin. This will fail gracefully if not elevated.
    $rule = netsh interface portproxy show v4tov4 | Select-String "0.0.0.0\s+$ListenPort"
    if ($rule) {
        Write-Host "[portproxy] rule for port $ListenPort already exists"
        return $true
    }
    $result = netsh interface portproxy add v4tov4 listenport=$ListenPort connectaddress=$TargetIP connectport=$TargetPort 2>&1
    if ($LASTEXITCODE -eq 0) {
        Write-Host "[portproxy] added: localhost:$ListenPort -> ${TargetIP}:$TargetPort" -ForegroundColor Green
        return $true
    } else {
        Write-Host "[portproxy] FAILED to add rule: $result" -ForegroundColor Yellow
        Write-Host "[portproxy] Run as Administrator." -ForegroundColor Yellow
        return $false
    }
}

function Remove-PortProxy {
    param([int]$ListenPort)
    $result = netsh interface portproxy delete v4tov4 listenport=$ListenPort 2>&1
    if ($LASTEXITCODE -eq 0) {
        Write-Host "[portproxy] removed rule for port $ListenPort (vsock recovered)" -ForegroundColor Green
    }
}

# --- Main logic ---

Write-Host "[$(Get-Date -Format 'HH:mm:ss')] Checking localhost:$Port..." -NoNewline

if (Test-PortReachable -Port $Port -Path $HealthPath) {
    Write-Host " OK (vsock/mirrored works)" -ForegroundColor Green
    # Clean up any stale portproxy rule we may have added previously.
    Remove-PortProxy -ListenPort $Port
    exit 0
}

Write-Host " UNREACHABLE" -ForegroundColor Red
Write-Host "[portproxy] vsock proxy broken. Setting up netsh fallback..." -ForegroundColor Yellow

$wslIP = Get-WSLIP
if (-not $wslIP) {
    Write-Host "[portproxy] Cannot determine WSL IP. Is Rancher Desktop running?" -ForegroundColor Red
    exit 1
}

Write-Host "[portproxy] WSL IP: $wslIP"
$added = Add-PortProxy -ListenPort $Port -TargetPort $InternalPort -TargetIP $wslIP
if (-not $added) { exit 1 }

Start-Sleep 2

# Verify the fallback works.
if (Test-PortReachable -Port $Port -Path $HealthPath) {
    Write-Host "[portproxy] Fallback active. localhost:$Port reachable via netsh." -ForegroundColor Green
} else {
    Write-Host "[portproxy] Fallback did not work. Check WSL IP or container status." -ForegroundColor Red
    exit 1
}
