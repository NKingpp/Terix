@echo off
REM One-click launcher for Terix dev environment (double-click to run).
REM
REM Why a .bat: double-clicking .sh does nothing useful on Windows.
REM All logic lives in dev-up.sh -- duplicating it here would drift.
REM
REM Two hard requirements for this file:
REM   1. ASCII-only. cmd.exe reads .bat as GBK; UTF-8 Chinese gets
REM      truncated mid-character and cmd runs the fragments as commands.
REM   2. CRLF line endings. cmd.exe parses .bat by CRLF; a bare-LF file
REM      makes "if ( ... )" blocks and goto misparse -- the window flashes
REM      and closes. Both broke us once each already.

setlocal enableextensions
cd /d "%~dp0"

echo.
echo ============================================
echo   Terix dev environment
echo ============================================

REM Double-click inherits a minimal PATH that often lacks Git and Docker.
REM Prepend the known install locations so `bash`, `docker`, `curl` resolve.
REM This is additive: existing PATH entries stay, nothing is removed.
if exist "C:\Program Files\Git\bin\bash.exe" set "PATH=C:\Program Files\Git\bin;%PATH%"
if exist "C:\Program Files\Docker\Docker\resources\bin" set "PATH=C:\Program Files\Docker\Docker\resources\bin;%PATH%"

where bash >nul 2>&1
if errorlevel 1 (
    echo.
    echo [FAIL] bash not found.
    echo        Install Git for Windows, then run start.bat again.
    echo.
    pause
    exit /b 1
)

where docker >nul 2>&1
if errorlevel 1 (
    echo.
    echo [FAIL] docker not found in PATH.
    echo        Start Docker Desktop once, then run start.bat again.
    echo.
    pause
    exit /b 1
)

REM Run the real launcher. Output goes to this console so a failure is
REM readable without hunting log files.
bash dev-up.sh
set RC=%ERRORLEVEL%

echo.
if "%RC%"=="0" (
    echo ============================================
    echo   ALL READY
    echo ============================================
    echo   frontend  http://localhost:3000/login
    echo   backend   http://localhost:8787
    echo   account   ARTEX / terix2026
    echo.
    echo   logs      /tmp/be.log  /tmp/fe.log  /tmp/sc.log
) else (
    echo ============================================
    echo   SOME SERVICES FAILED ^(exit %RC%^)
    echo ============================================
    echo   Scroll up to find the [FAIL] line.
)
echo.

REM Keep the window open when launched by double-click.
pause
endlocal
