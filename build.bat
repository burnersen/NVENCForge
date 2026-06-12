@echo off
REM ============================================================================
REM  build.bat - NVENCForge build script
REM ----------------------------------------------------------------------------
REM  Run this from the folder that contains the .go sources (e.g. "sourcecode").
REM
REM  Steps:
REM    1) Pack all build-relevant sources into embedded_source.zip. This archive
REM       is embedded into the exe (go:embed) and unpacked into a "sourcecode"
REM       folder on first run, so the exe can always rebuild itself.
REM    2) Resolve dependencies (go mod tidy).
REM    3) Build NVENCForge.exe (stripped: -ldflags="-s -w").
REM ============================================================================
setlocal
cd /d "%~dp0"

set "ZIP=embedded_source.zip"
set "EXE=NVENCForge.exe"

echo [1/3] Packing source into %ZIP% ...
if exist "%ZIP%" del /q "%ZIP%"
powershell -NoProfile -Command "$inc = Get-ChildItem -File | Where-Object { @('.go','.mod','.sum','.md','.bat') -contains $_.Extension }; if ($inc) { Compress-Archive -Path $inc.FullName -DestinationPath '%ZIP%' -Force }"
if not exist "%ZIP%" ( echo [ERROR] Could not create %ZIP%. & pause & exit /b 1 )

echo [2/3] Resolving dependencies (go mod tidy) ...
go mod tidy || ( echo [ERROR] go mod tidy failed. & pause & exit /b 1 )

echo [3/3] Building %EXE% ...
go build -ldflags="-s -w" -o "%EXE%" || ( echo [ERROR] Build failed. & pause & exit /b 1 )

echo.
echo Done: %EXE%
pause
