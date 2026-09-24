Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

$repoRoot = Split-Path -Parent $PSScriptRoot
$backendDir = Join-Path $repoRoot "backend"
$configPath = Join-Path $repoRoot "config.yaml"
$runtimeDir = Join-Path $repoRoot "data\\local-runtime"
$binaryPath = Join-Path $runtimeDir "grok2api.exe"
$pidPath = Join-Path $runtimeDir "grok2api.pid"
$stdoutPath = Join-Path $runtimeDir "grok2api.stdout.log"
$stderrPath = Join-Path $runtimeDir "grok2api.stderr.log"
$qualityGuardDir = Join-Path $repoRoot "data\\quality-guard"
$healthURL = "http://127.0.0.1:8000/healthz"

if (-not (Test-Path -LiteralPath $configPath -PathType Leaf)) {
    throw "Missing local config: $configPath"
}

New-Item -ItemType Directory -Force -Path $runtimeDir, $qualityGuardDir | Out-Null

Write-Host "Building Grok2API..."
Push-Location $backendDir
try {
    & go build -o $binaryPath ./cmd/grok2api
    if ($LASTEXITCODE -ne 0) {
        throw "go build failed with exit code $LASTEXITCODE"
    }
} finally {
    Pop-Location
}

if (Test-Path -LiteralPath $pidPath -PathType Leaf) {
    $pidText = (Get-Content -LiteralPath $pidPath -Raw).Trim()
    [int]$managedPid = 0
    if ([int]::TryParse($pidText, [ref]$managedPid)) {
        $managedProcess = Get-Process -Id $managedPid -ErrorAction SilentlyContinue
        if ($null -ne $managedProcess) {
            Write-Host "Stopping the previous Grok2API process (PID $managedPid)..."
            Stop-Process -Id $managedPid -Force
        }
    }
    Remove-Item -LiteralPath $pidPath -Force -ErrorAction SilentlyContinue
}

# The desktop launcher owns the local 8000 endpoint. A previous go run process
# has no pid file, so stop its listener before starting the freshly built binary.
$listeners = @(Get-NetTCPConnection -State Listen -LocalPort 8000 -ErrorAction SilentlyContinue)
foreach ($listener in $listeners) {
    $listenerPid = [int]$listener.OwningProcess
    Write-Host "Stopping the existing port 8000 listener (PID $listenerPid)..."
    Stop-Process -Id $listenerPid -Force -ErrorAction Stop
}

$portDeadline = (Get-Date).AddSeconds(10)
while (@(Get-NetTCPConnection -State Listen -LocalPort 8000 -ErrorAction SilentlyContinue).Count -gt 0) {
    if ((Get-Date) -ge $portDeadline) {
        throw "Port 8000 did not close after stopping the previous service."
    }
    Start-Sleep -Milliseconds 250
}

Remove-Item -LiteralPath $stdoutPath, $stderrPath -Force -ErrorAction SilentlyContinue
$env:GROK2API_QUALITY_GUARD_DIR = $qualityGuardDir
Write-Host "Starting Grok2API..."
$process = Start-Process -FilePath $binaryPath -ArgumentList @("--config", $configPath, "--listen", "127.0.0.1:8000") -WorkingDirectory $repoRoot -RedirectStandardOutput $stdoutPath -RedirectStandardError $stderrPath -PassThru
Set-Content -LiteralPath $pidPath -Value $process.Id -Encoding ascii -NoNewline

$healthDeadline = (Get-Date).AddSeconds(45)
while ((Get-Date) -lt $healthDeadline) {
    try {
        $response = Invoke-WebRequest -UseBasicParsing -TimeoutSec 3 -Uri $healthURL
        if ($response.StatusCode -eq 200) {
            Write-Host "Grok2API is ready at http://127.0.0.1:8000"
            exit 0
        }
    } catch {
        $running = Get-Process -Id $process.Id -ErrorAction SilentlyContinue
        if ($null -eq $running) {
            Remove-Item -LiteralPath $pidPath -Force -ErrorAction SilentlyContinue
            throw "Grok2API exited before becoming healthy. See $stderrPath"
        }
    }
    Start-Sleep -Seconds 1
}

Stop-Process -Id $process.Id -Force -ErrorAction SilentlyContinue
Remove-Item -LiteralPath $pidPath -Force -ErrorAction SilentlyContinue
throw "Grok2API did not become healthy within 45 seconds. See $stdoutPath and $stderrPath"
