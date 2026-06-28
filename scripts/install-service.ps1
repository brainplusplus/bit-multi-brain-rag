# Install bit-rag dashboard as a Windows background service.
#
# Uses Windows Task Scheduler (no admin required for user-scope tasks) to
# auto-start dashboard on login and keep it running.
#
# Alternative: NSSM (https://nssm.cc/) for a true Windows Service that runs
# before login. Run with -UseNssm to use NSSM instead.
#
# Usage:
#   .\scripts\install-service.ps1              # Task Scheduler (user scope)
#   .\scripts\install-service.ps1 -UseNssm     # NSSM (requires nssm.exe on PATH)
#   .\scripts\install-service.ps1 -Uninstall   # Remove the task/service

param(
    [switch]$UseNssm,
    [switch]$Uninstall
)

$RepoRoot = Split-Path -Parent $PSScriptRoot
$TaskName = "bit-rag-dashboard"

# --- Uninstall ---
if ($Uninstall) {
    Write-Host "Removing bit-rag dashboard service..." -ForegroundColor Yellow
    if ($UseNssm) {
        & nssm stop $TaskName 2>$null
        & nssm remove $TaskName confirm 2>$null
    } else {
        schtasks /Delete /TN $TaskName /F 2>$null
    }
    Write-Host "Removed." -ForegroundColor Green
    exit 0
}

# --- Load .env for env vars ---
$envFile = Join-Path $RepoRoot ".env"
$envVars = @{}
if (Test-Path $envFile) {
    Get-Content $envFile | ForEach-Object {
        if ($_ -match "^([^#=]+)=(.*)$") {
            $envVars[$matches[1].Trim()] = $matches[2].Trim()
        }
    }
}

# --- Build binary if needed ---
$binPath = Join-Path $RepoRoot "bin\bit-rag-dashboard.exe"
if (-not (Test-Path $binPath)) {
    Write-Host "Building dashboard binary..." -ForegroundColor Cyan
    Push-Location $RepoRoot
    $env:CGO_ENABLED = "1"
    go build -o $binPath ./cmd/dashboard/
    Pop-Location
    if ($LASTEXITCODE -ne 0) {
        Write-Host "Build failed!" -ForegroundColor Red
        exit 1
    }
}

# --- Copy zvec DLL next to binary ---
$dllSrc = Join-Path $RepoRoot "lib\windows_amd64\zvec_c_api.dll"
$binDir = Split-Path $binPath
if (Test-Path $dllSrc) {
    Copy-Item $dllSrc $binDir -Force
    Write-Host "Copied zvec_c_api.dll" -ForegroundColor DarkGray
}

# --- Set embedded mode env vars ---
$zvecPath = Join-Path $RepoRoot "data\zvec"
$dbPath = Join-Path $RepoRoot "data\dashboard-local.db"

# In embedded mode, embedder endpoint should be localhost (not Docker service name)
$embEndpoint = $envVars['EMBEDDING_ENDPOINT']
if ($embEndpoint -match 'bit-rag-embedder') {
    $embEndpoint = "http://localhost:8090"
}

# --- Create start script ---
$startScript = Join-Path $binDir "start-dashboard.ps1"
$startContent = @"
`$env:HTTP_ADDR = ":8081"
`$env:ENVIRONMENT = "development"
`$env:ZVEC_PATH = "$zvecPath"
`$env:DB_PATH = "$dbPath"
`$env:QDRANT_URL = ""
`$env:MCP_ENABLED = "false"
`$env:EMBEDDING_ENDPOINT = "$embEndpoint"
`$env:EMBEDDING_API_KEY = "$($envVars['EMBEDDING_API_KEY'])"
`$env:LLAMA_API_KEY = "$($envVars['LLAMA_API_KEY'])"
`$env:EMBEDDING_MODEL = "$($envVars['EMBEDDING_MODEL'])"
`$env:EMBEDDING_DIM = "$($envVars['EMBEDDING_DIM'])"
`$env:EMBEDDING_POOLING = "$($envVars['EMBEDDING_POOLING'])"
`$env:ACTIVE_MODEL = "$($envVars['ACTIVE_MODEL'])"
`$env:ACTIVE_BACKEND = "$($envVars['ACTIVE_BACKEND'])"
`$env:DASHBOARD_API_KEYS = "$($envVars['DASHBOARD_API_KEYS'])"
Set-Location "$binDir"
& "$binPath"
"@
Set-Content $startScript $startContent -Force

# --- Install ---
if ($UseNssm) {
    Write-Host "Installing as Windows Service via NSSM..." -ForegroundColor Cyan
    & nssm install $TaskName "powershell.exe" "-ExecutionPolicy Bypass -NoProfile -File `"$startScript`""
    & nssm set $TaskName AppDirectory $binDir
    & nssm set $TaskName AppStdout (Join-Path $RepoRoot "data\dashboard-svc.log")
    & nssm set $TaskName AppStderr (Join-Path $RepoRoot "data\dashboard-svc.log")
    & nssm set $TaskName AppStopMethodSkip 0
    & nssm set $TaskName AppStopMethodConsole 5000
    & nssm start $TaskName
    Write-Host "Service installed and started." -ForegroundColor Green
} else {
    Write-Host "Installing as Task Scheduler job (auto-start on login)..." -ForegroundColor Cyan
    $action = New-ScheduledTaskAction -Execute "powershell.exe" -Argument "-ExecutionPolicy Bypass -NoProfile -WindowStyle Hidden -File `"$startScript`""
    $trigger = New-ScheduledTaskTrigger -AtLogOn -User $env:USERNAME
    $settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1)
    Register-ScheduledTask -TaskName $TaskName -Action $action -Trigger $trigger -Settings $settings -Force | Out-Null

    # Start now
    Start-ScheduledTask -TaskName $TaskName
    Write-Host "Task installed and started." -ForegroundColor Green
}

Write-Host ""
Write-Host "Dashboard: http://localhost:8081" -ForegroundColor Green
Write-Host "Logs:      $RepoRoot\data\dashboard-svc.log" -ForegroundColor DarkGray
Write-Host ""
Write-Host "Uninstall: .\scripts\install-service.ps1 -Uninstall" -ForegroundColor DarkGray
