$exe = "E:\grok2api2\data\local-runtime\grok2api.exe"
$cfg = "E:\grok2api2\config.yaml"
$out = "E:\grok2api2\data\local-runtime\grok2api.stdout.log"
$err = "E:\grok2api2\data\local-runtime\grok2api.stderr.log"
$pidFile = "E:\grok2api2\data\local-runtime\grok2api.pid"
$qualityGuardDir = "E:\grok2api2\data\quality-guard"
$upstreamCaptureDir = Join-Path $qualityGuardDir "upstream-packets"

New-Item -ItemType Directory -Force -Path "E:\grok2api2\data\local-runtime", $qualityGuardDir, $upstreamCaptureDir | Out-Null

$env:GROK2API_QUALITY_GUARD_DIR = $qualityGuardDir
$env:GROK2API_UPSTREAM_CAPTURE_DIR = $upstreamCaptureDir
$env:GROK2API_SAVE_VIDEO_QUALITY_EVIDENCE = "false"

$proc = Start-Process -FilePath $exe -ArgumentList @("--config", $cfg, "--listen", "127.0.0.1:8000") -WorkingDirectory "E:\grok2api2" -RedirectStandardOutput $out -RedirectStandardError $err -WindowStyle Hidden -PassThru

$proc.Id | Out-File -FilePath $pidFile -Encoding ascii -NoNewline
Write-Host "Started local version PID: $($proc.Id)"
