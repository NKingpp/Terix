@echo off
REM Stop the three Terix dev processes (keeps Docker / PostgreSQL running).
REM
REM Why not stop Docker: PG holds real data (credentials, drafts, paper ledger).
REM Stopping the container slows the next start and makes Docker Desktop
REM re-do daemon warmup. Only kill the three that hold ports and need
REM frequent restarts during coding.
REM
REM ASCII-only on purpose: cmd.exe reads .bat as GBK, and UTF-8 Chinese
REM in comments truncates commands mid-character (observed failure).

setlocal
cd /d "%~dp0"

echo.
echo Stopping Terix dev processes ...

REM Resolve ports to PIDs first, then kill by PID.
REM
REM Why not match CommandLine: the akshare sidecar starts with cwd set to
REM sidecars\akshare, so its CommandLine is just "...python.exe server.py" --
REM no path substring to match. Port-to-PID is the only reliable handle.
REM
REM Why not taskkill /IM python.exe: that would take out every other
REM python process on the machine.
powershell -NoProfile -Command ^
  "$ports = 8787,8791,3000; $pids = @(); foreach ($p in $ports) { $pids += (Get-NetTCPConnection -LocalPort $p -State Listen -ErrorAction SilentlyContinue).OwningProcess }; $pids = $pids | Where-Object { $_ -and $_ -ne 0 } | Sort-Object -Unique; if ($pids) { foreach ($id in $pids) { $proc = Get-Process -Id $id -ErrorAction SilentlyContinue; if ($proc) { Write-Host ('  killing ' + $proc.ProcessName + ' (pid ' + $id + ')'); Stop-Process -Id $id -Force -ErrorAction SilentlyContinue } } } else { Write-Host '  nothing listening on 8787/8791/3000' }"

REM Give Windows a moment to release the sockets.
REM Not "timeout /t": under Git Bash, the shell's timeout.exe shadows cmd's
REM builtin and fails with "invalid time interval".
powershell -NoProfile -Command "Start-Sleep -Seconds 2"

echo.
echo Port status:
for %%P in (8787 8791 3000) do (
    netstat -ano 2>nul | findstr ":%%P " | findstr LISTENING >nul && echo   port %%P  STILL LISTENING || echo   port %%P  released
)
echo.
echo PostgreSQL (:5433) and Docker left running.
echo.
pause
endlocal
