# OctPlugins build/start script (ASCII-only for PS 5.1 compat)
#   - kernel: go build (fallback to prebuilt kerneld.exe on failure)
#   - UI:     npm install in ui/
# Usage: .\build.ps1            # build (auto-fallback to prebuilt kernel)
#        .\build.ps1 -start     # build then start host
#        .\build.ps1 -rebuild   # force rebuild kernel (needs Go toolchain)
param([switch]$start, [switch]$rebuild)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $MyInvocation.MyCommand.Path
$kernelDir = Join-Path $root "kernel"
$uiDir = Join-Path $root "ui"
$kernelExe = Join-Path $kernelDir "kerneld.exe"
$hasKernelExe = Test-Path $kernelExe

Write-Host ""
Write-Host "=== OctPlugins Build ===" -ForegroundColor Cyan

# -- 1) Kernel -----------------------------------------
if ($rebuild) {
    Write-Host "[1/2] Rebuilding kernel..." -ForegroundColor Yellow
    Push-Location $kernelDir
    try {
        go mod tidy 2>$null
        go build -o kerneld.exe ./cmd/kerneld
        Write-Host "      Kernel built: $kernelExe" -ForegroundColor Green
    } catch {
        Write-Host "      WARN: kernel build failed -> keeping prebuilt kerneld.exe" -ForegroundColor Yellow
        Write-Host "      Reason: go.mod requires Go 1.27, found $((go version 2>$null)); or internal/deps missing." -ForegroundColor DarkYellow
        if (-not $hasKernelExe) { Write-Host "      ERROR: no usable kernel exe." -ForegroundColor Red; Pop-Location; exit 1 }
    } finally { Pop-Location }
} else {
    if (-not $hasKernelExe) {
        Write-Host "[1/2] kerneld.exe not found, building..." -ForegroundColor Yellow
        Push-Location $kernelDir
        try { go mod tidy 2>$null; go build -o kerneld.exe ./cmd/kerneld; Write-Host "      Kernel built" -ForegroundColor Green }
        catch { Write-Host "      ERROR: build failed and no prebuilt exe. Check Go version/toolchain or internal/deps." -ForegroundColor Red; Pop-Location; exit 1 }
        Pop-Location
    } else {
        Write-Host "[1/2] Using prebuilt kernel: $kernelExe" -ForegroundColor Green
    }
}

# -- 2) UI deps ----------------------------------------
Write-Host "[2/2] Installing UI deps (npm install)..." -ForegroundColor Yellow
Push-Location $uiDir
try { npm install } finally { Pop-Location }

Write-Host ""
Write-Host "Build done. Start with:" -ForegroundColor Cyan
Write-Host "    cd ui && npm start    (or .\build.ps1 -start)" -ForegroundColor White

if ($start) {
    Write-Host ""
    Write-Host "=== Starting Electron host ===" -ForegroundColor Cyan
    Push-Location $uiDir
    npm start
    Pop-Location
}