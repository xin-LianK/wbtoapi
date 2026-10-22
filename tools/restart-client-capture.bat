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

REM Milestone log. The window closes/pauses on failure and the reason scrolls
REM away; writing it here lets the trace be read afterwards without a retype.
set "RUNLOG=%HERE%\capture\bat-run.log"
>"%RUNLOG%" echo [%DATE% %TIME%] start pid-owner=%USERNAME%

REM Detection note: mitmproxy binds 0.0.0.0:8888 (plus [::]:8888), NOT 127.0.0.1.
REM Matching "127.0.0.1:8888.*LISTENING" never hits and the script bails out
REM before closing the app -- which looks exactly like "the bat did nothing".
REM So: filter LISTENING rows, then match the local port with a non-digit guard
REM so :88880 or a remote port :8888 on a TIME_WAIT row can't false-positive.
echo [*] Checking proxy on port %PORT% ...
netstat -ano | findstr /c:"LISTENING" | findstr /r /c:":%PORT%[^0-9]" >nul
if errorlevel 1 goto start_proxy
echo     already running
>>"%RUNLOG%" echo proxy: already listening on %PORT%
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
>>"%RUNLOG%" echo proxy: up on %PORT%

:kill_app
echo [*] Closing WorkBuddy ...
>>"%RUNLOG%" echo app: taskkill requested
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
>>"%RUNLOG%" echo app: TASKKILL DENIED - still running
echo     Right-click this file - Run as administrator, then try again.
pause
exit /b 2

:app_closed
echo     closed
>>"%RUNLOG%" echo app: closed

set "NODE_EXTRA_CA_CERTS=%CA%"
set "HTTPS_PROXY=http://127.0.0.1:%PORT%"
set "HTTP_PROXY=http://127.0.0.1:%PORT%"
set "NO_PROXY=127.0.0.1,localhost,::1"

REM NODE_OPTIONS is the ONLY way to catch chat traffic. The ACP transport uses
REM the global fetch (Node's bundled undici) and undici ignores the proxy env
REM vars -- so with the vars above alone you capture /v3/config and telemetry
REM but never a single copilot.tencent.com chat request. The --require shim
REM swaps undici's global dispatcher for EnvHttpProxyAgent. NODE_USE_ENV_PROXY
REM would be simpler but landed in Node v24 and this build is Node 22.21.1.
set "NODE_OPTIONS=--require=%~dp0undici-env-proxy.js"

REM Wipe the shim log so the check below can tell "loaded" from "stale line".
set "SHIMLOG=%HERE%\capture\undici-patch.log"
del "%SHIMLOG%" >nul 2>&1

echo [*] Starting WorkBuddy with capture env ...
>>"%RUNLOG%" echo app: launching with NODE_OPTIONS=%NODE_OPTIONS%
start "" "%EXE%"

REM Self-check: the shim writes one line as soon as --require runs. If the log
REM stays empty, NODE_OPTIONS did not reach the Electron main process and the
REM chat capture would silently record nothing again. 15s covers cold boot.
echo [*] Waiting for the shim to load ...
timeout /t 15 /nobreak >nul
if exist "%SHIMLOG%" (
  findstr /c:"dispatcher replaced" "%SHIMLOG%" >nul
  if errorlevel 1 (
    echo [!] Shim loaded but the dispatcher was NOT swapped.
    >>"%RUNLOG%" echo shim: LOADED but no dispatch swap
    echo     Open a chat once, then check: type capture\undici-patch.log
  ) else (
    echo     shim ok - fetch now follows HTTPS_PROXY
    >>"%RUNLOG%" echo shim: OK
  )
) else (
  echo [!] Shim did NOT load: NODE_OPTIONS was ignored.
  >>"%RUNLOG%" echo shim: NOT LOADED - NODE_OPTIONS ignored
  echo     chat requests will bypass the proxy. Send this window's output back.
)

echo.
echo [ok] Dumps go to capture\upstream-live.jsonl
echo     Keep the minimized "mitmproxy-%PORT%" window open while capturing --
echo     closing it stops the proxy and nothing more gets recorded.
timeout /t 3 /nobreak >nul
