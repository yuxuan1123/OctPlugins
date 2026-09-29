@echo off
REM OCTplugins launcher: build TypeScript host and start Electron UI.
REM Requires: Node.js + npm installed.
setlocal

set "ROOT=%~dp0"
set "UI=%ROOT%ui"

echo [OCTplugins] Building UI (TypeScript) ...
pushd "%UI%"
call npm install
if errorlevel 1 ( echo [OCTplugins] npm install failed. & popd & exit /b 1 )
call npm run build
if errorlevel 1 ( echo [OCTplugins] TypeScript build failed. & popd & exit /b 1 )
popd

echo [OCTplugins] Starting Electron host ...
pushd "%UI%"
call npx electron .
popd

endlocal