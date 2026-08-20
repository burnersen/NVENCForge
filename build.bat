@echo off
REM NVENCForge — Required Notice: Copyright (c) 2026 burnersen — NVENCForge
REM Licensed under the PolyForm Noncommercial License 1.0.0 (non-commercial use only).
REM Full terms: LICENSE.md · https://polyformproject.org/licenses/noncommercial/1.0.0
REM ============================================================================
REM  build.bat - NVENCForge build script
REM ----------------------------------------------------------------------------
REM  Run this from the folder that contains the .go sources.
REM
REM  Steps:
REM    1) Resolve dependencies (go mod tidy).
REM    2) Build NVENCForge.exe (stripped: -ldflags="-s -w").
REM ============================================================================
setlocal
cd /d "%~dp0"

set "EXE=NVENCForge.exe"

echo [1/2] Resolving dependencies (go mod tidy) ...
go mod tidy || ( echo [ERROR] go mod tidy failed. & pause & exit /b 1 )

echo [2/2] Building %EXE% ...
go build -ldflags="-s -w" -o "%EXE%" || ( echo [ERROR] Build failed. & pause & exit /b 1 )

echo.
echo Done: %EXE%
pause
