@echo off
setlocal
cd /d "%~dp0"

:: --- Init fnm (Fast Node Manager) so npm/node/electron resolve ----------
where fnm >nul 2>&1
if %errorlevel% neq 0 (
    echo [FNM ] fnm not found on PATH; npm/node may fail.
) else (
    echo [FNM ] Initializing fnm node env ...
    for /f "usebackq delims=" %%i in (`fnm env --shell cmd`) do %%i
)

echo ============================================
echo    OctPlugins launcher
echo ============================================

:: 1) Build kernel if missing
if not exist "kernel\kerneld.exe" (
    echo [1/3] Building kernel...
    pushd kernel
    go env -w GOTOOLCHAIN=local >nul 2>&1
    set GOARCH=amd64
    call go build -o kerneld.exe ./cmd/kerneld
    popd
    if not exist "kernel\kerneld.exe" (
        echo [ERROR] Kernel build failed. Check 'go version' version, need Go 1.21 or newer.
        pause
        exit /b 1
    )
) else (
    echo [1/3] Kernel found.
)

:: 2) Install UI deps on first run
if not exist "ui\node_modules" (
    echo [2/3] Installing UI deps ...
    pushd ui
    call npm install
    popd
) else (
    echo [2/3] UI deps found.
)

:: 3) Build TypeScript UI
echo [3/3] Building UI ...
pushd ui
call npm run build
popd
if errorlevel 1 (
    echo [ERROR] TypeScript build failed.
    echo [HINT ] Run 'npm run build' in the ui folder to see details.
    pause
    exit /b 1
)

:: 4) Launch Electron host
echo Launching Electron host ...
pushd ui
call node_modules\.bin\electron.cmd .
set EC=%ERRORLEVEL%
popd

echo [OK] OctPlugins exited with code %EC%.
pause
endlocal