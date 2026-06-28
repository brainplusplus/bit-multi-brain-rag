# install-port-forward-task.ps1
#
# Installs a Windows Scheduled Task that runs ensure-port-forward.ps1 every
# 5 minutes. This auto-recovers port forward when Rancher Desktop's vsock
# proxy crashes (known issue after docker daemon restarts).
#
# Run ONCE as Administrator:
#   powershell -ExecutionPolicy Bypass -File scripts\install-port-forward-task.ps1
#
# To uninstall:
#   schtasks /Delete /TN "bit-rag-port-forward" /F

$ErrorActionPreference = 'Stop'

# Must be admin for netsh portproxy + schtasks
$isAdmin = ([Security.Principal.WindowsPrincipal] [Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $isAdmin) {
    Write-Host "ERROR: Run as Administrator." -ForegroundColor Red
    exit 1
}

$scriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$scriptPath = Join-Path $scriptDir "ensure-port-forward.ps1"

if (-not (Test-Path $scriptPath)) {
    Write-Host "ERROR: ensure-port-forward.ps1 not found at $scriptPath" -ForegroundColor Red
    exit 1
}

# Create scheduled task
$action = New-ScheduledTaskAction `
    -Execute "powershell.exe" `
    -Argument "-ExecutionPolicy Bypass -WindowStyle Hidden -File `"$scriptPath`""

$trigger = New-ScheduledTaskTrigger -Once -At (Get-Date) -RepetitionInterval (New-TimeSpan -Minutes 5)

$settings = New-ScheduledTaskSettingsSet `
    -AllowStartIfOnBatteries `
    -DontStopIfGoingOnBatteries `
    -StartWhenAvailable `
    -RunOnlyIfNetworkAvailable `
    -ExecutionTimeLimit (New-TimeSpan -Minutes 2)

Register-ScheduledTask `
    -TaskName "bit-rag-port-forward" `
    -Action $action `
    -Trigger $trigger `
    -Settings $settings `
    -Description "Auto-recover bit-rag dashboard port forward (bypass Rancher vsock proxy)" `
    -Force | Out-Null

Write-Host "Scheduled task 'bit-rag-port-forward' installed." -ForegroundColor Green
Write-Host "Runs every 5 minutes. Checks localhost:8090, falls back to netsh portproxy if vsock broken." -ForegroundColor Cyan
Write-Host ""
Write-Host "To verify: schtasks /Query /TN bit-rag-port-forward"
Write-Host "To uninstall: schtasks /Delete /TN bit-rag-port-forward /F"
