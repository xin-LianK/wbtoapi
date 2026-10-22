@echo off
REM Restart WorkBuddy with MITM cert + proxy, so tools/capture-upstream.sh can dump
REM real upstream requests.
REM
REM Why a full restart is required: business requests go through axios +
REM HttpsProxyAgent, and TLS trust comes from Node's built-in CA list -- Windows
REM system root store is NOT read (the --use-system-ca constant in the bundle is
REM defined but never appendSwitch'd). So the CA can only be injected at process
REM start via NODE_EXTRA_CA_CERTS; there is no hot-reload path.
REM
REM Why this script also starts the proxy: whoever launches mitmdump owns it. If the
REM proxy sits in the WorkBuddy process tree, killing WorkBuddy kills the proxy too --
REM and then nothing gets captured after the restart. Started here, it lives in its
REM own console window and survives.
REM
REM ASCII-only on purpose: cmd.exe defaults to codepage 936 on zh-CN Windows and
REM would garble UTF-8 text in a .bat.
REM
REM After capturing: quit the app and start it normally to drop these variables.

setlocal

set "CA=%USERPROFILE%\.mitmproxy\mitmproxy-ca-cert.pem"
set "PORT=8888"
set "EXE=%ProgramFiles%\WorkBuddy\WorkBuddy.exe"
set "MITMDUMP=%USERPROFILE%\.workbuddy\binaries\python\envs\mitm\Scripts\mitmdump.exe"
set "ADDON=%~dp0mitm-upstream-dump.py"
set "HERE=%~dp0.."
set "MITM_OUT=%HERE%\capture\upstream-live.jsonl"

if not exist "%CA%" (
  echo [x] MITM CA not found: %CA%
  echo     Run mitmdump once to generate it.
  pause
  exit /b 2
)

if not exist "%EXE%" (
  echo [x] WorkBuddy not found: %EXE%
  pause
  exit /b 2
)

if not exist "%ADDON%" (
  echo [x] Addon not found: %ADDON%
  pause
  exit /b 2
)

if not exist "%HERE%\capture" mkdir "%HERE%\capture"

REM Detection note: mitmproxy binds 0.0.0.0:8888 (plus [::]:8888), NOT 127.0.0.1.
REM Matching "127.0.0.1:8888.*LISTENING" never hits and the script bails out
REM before closing the app -- which looks exactly like "the bat did nothing".
REM So: filter LISTENING rows, then match the local port with a non-digit guard
REM so :88880 or a remote port :8888 on a TIME_WAIT row can't false-positive.
echo [*] Checking proxy on port %PORT% ...
netstat -ano | findstr /c:"LISTENING" | findstr /r /c:":%PORT%[^0-9]" >nul
if errorlevel 1 goto start_proxy
echo     already running
goto kill_app

:start_proxy
if not exist "%MITMDUMP%" (
  echo [x] mitmdump not found: %MITMDUMP%
  echo     Install mitmproxy into that venv, then rerun this script.
  pause
  exit /b 2
)
echo [*] Starting proxy in its own window ...
start "mitmproxy-%PORT%" /min "%MITMDUMP%" -p %PORT% -q -s "%ADDON%"
REM mitmproxy cold-starts slowly on Windows (python + addon import): 6s is not
REM always enough, and a single-shot check turns a slow start into "failed".
timeout /t 6 /nobreak >nul
netstat -ano | findstr /c:"LISTENING" | findstr /r /c:":%PORT%[^0-9]" >nul
if not errorlevel 1 goto proxy_ready
timeout /t 6 /nobreak >nul
netstat -ano | findstr /c:"LISTENING" | findstr /r /c:":%PORT%[^0-9]" >nul
if errorlevel 1 (
  echo [x] Proxy failed to start on port %PORT% after 12s.
  echo     Check the minimized mitmproxy window for a traceback.
  echo     Current listeners on that port:
  netstat -ano | findstr /c:":%PORT%"
  pause
  exit /b 2
)
:proxy_ready
echo     proxy up

:kill_app
echo [*] Closing WorkBuddy ...
taskkill /F /IM WorkBuddy.exe >nul 2>&1
REM Verify it actually died. taskkill is silent on "access denied" (elevated app),
REM and the Electron helper processes (crashpad, gpu) take a moment to unwind --
REM checking once after a fixed sleep would either false-fail or false-pass.
timeout /t 4 /nobreak >nul
tasklist /FI "IMAGENAME eq WorkBuddy.exe" 2>nul | findstr /i "WorkBuddy.exe" >nul
if errorlevel 1 goto app_closed
timeout /t 4 /nobreak >nul
tasklist /FI "IMAGENAME eq WorkBuddy.exe" 2>nul | findstr /i "WorkBuddy.exe" >nul
if errorlevel 1 goto app_closed
echo [x] WorkBuddy is still running - taskkill was denied.
echo     Right-click this file - Run as administrator, then try again.
pause
exit /b 2

:app_closed
echo     closed

set "NODE_EXTRA_CA_CERTS=%CA%"
set "HTTPS_PROXY=http://127.0.0.1:%PORT%"
set "HTTP_PROXY=http://127.0.0.1:%PORT%"
set "NO_PROXY=127.0.0.1,localhost,::1"

echo [*] Starting WorkBuddy with capture env ...
start "" "%EXE%"

echo [ok] Started with capture env. Dumps go to capture\upstream-*.jsonl
echo.
echo     Keep the minimized "mitmproxy-%PORT%" window open while capturing --
echo     closing it stops the proxy and nothing more gets recorded.
timeout /t 3 /nobreak >nul
