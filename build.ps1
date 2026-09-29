# OctPlugins 一键构建/启动脚本
#   - 核 心：预留 go build（本机 kernel 源码缺 internal/deps 且 Go 版本 < go.mod 要求 1.27，构建失败时回退使用预编译 kerneld.exe）
#   - UI：  在 ui/ 安装依赖
# 用法： .\build.ps1            # 构建（自动回退预编译内核）
#        .\build.ps1 -start     # 构建后启动宿主
#        .\build.ps1 -rebuild   # 强制重新编译内核（需要可用 Go toolchain + 已补回 internal/deps）
param([switch]$start, [switch]$rebuild)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $MyInvocation.MyCommand.Path
$kernelDir = Join-Path $root "kernel"
$uiDir = Join-Path $root "ui"
$kernelExe = Join-Path $kernelDir "kerneld.exe"
$hasKernelExe = Test-Path $kernelExe

Write-Host ""
Write-Host "═══ OctPlugins 构建 ═══" -ForegroundColor Cyan

# ── 1) 内核 ─────────────────────────────────────────────
if ($rebuild) {
    Write-Host "[1/2] 重新编译内核 (Rebuild)..." -ForegroundColor Yellow
    Push-Location $kernelDir
    try {
        go build -o kerneld.exe ./cmd/kerneld
        Write-Host "      内核编译成功: $kernelExe" -ForegroundColor Green
    } catch {
        Write-Host "      WARN: 内核编译失败 → 保留预编译 kerneld.exe" -ForegroundColor Yellow
        Write-Host "      原因: kernel/go.mod 要求 Go 1.27，本机 $((go version 2>$null))；且 internal/deps 包缺失（详见 方案文档）。" -ForegroundColor DarkYellow
        if (-not $hasKernelExe) { Write-Host "      ERROR: 无可用内核可执行文件，无法继续。" -ForegroundColor Red; Pop-Location; exit 1 }
    } finally { Pop-Location }
} else {
    if (-not $hasKernelExe) {
        Write-Host "[1/2] 未找到 kerneld.exe，尝试编译..." -ForegroundColor Yellow
        Push-Location $kernelDir
        try { go build -o kerneld.exe ./cmd/kerneld; Write-Host "      内核编译成功" -ForegroundColor Green }
        catch { Write-Host "      ERROR: 内核编译失败且无预编译产物。残留原因: go 版本/toolchain 或缺 internal/deps。见方案文档。" -ForegroundColor Red; Pop-Location; exit 1 }
        Pop-Location
    } else {
        Write-Host "[1/2] 使用预编译内核: $kernelExe" -ForegroundColor Green
    }
}

# ── 2) UI 依赖 ──────────────────────────────────────────
Write-Host "[2/2] 安装宿主 UI 依赖 (npm install)..." -ForegroundColor Yellow
Push-Location $uiDir
try { npm install } finally { Pop-Location }

Write-Host ""
Write-Host "构建完成。启动方式：" -ForegroundColor Cyan
Write-Host "    cd ui && npm start    （或 .\build.ps1 -start）" -ForegroundColor White

if ($start) {
    Write-Host ""
    Write-Host "═══ 启动宿主 Electron ═══" -ForegroundColor Cyan
    Push-Location $uiDir
    npm start
    Pop-Location
}