@echo off
REM NVENCForge (https://github.com/burnersen/NVENCForge)
REM Copyright (C) 2026 burnersen
REM SPDX-License-Identifier: GPL-3.0-only
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
