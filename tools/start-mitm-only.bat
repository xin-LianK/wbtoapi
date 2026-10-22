@echo off
REM Start ONLY the mitmproxy addon, leaving WorkBuddy untouched.
REM Use this after editing mitm-upstream-dump.py while the client already runs
REM with the capture env (NODE_EXTRA_CA_CERTS + HTTPS_PROXY): only the proxy
REM needs a restart, the client just reconnects on the same port.
setlocal
set "PORT=8888"
set "HERE=%~dp0.."
set "MITM_OUT=%HERE%\capture\upstream-live.jsonl"
set "MITMDUMP=%USERPROFILE%\.workbuddy\binaries\python\envs\mitm\Scripts\mitmdump.exe"
set "ADDON=%~dp0mitm-upstream-dump.py"

if not exist "%MITMDUMP%" (
  echo [x] mitmdump not found: %MITMDUMP%
  pause
  exit /b 2
)
if not exist "%ADDON%" (
  echo [x] addon not found: %ADDON%
  pause
  exit /b 2
)

REM mitmproxy binds 0.0.0.0, not 127.0.0.1: filter LISTENING rows then match the
REM local port with a non-digit guard (see restart-client-capture.bat).
netstat -ano | findstr /c:"LISTENING" | findstr /r /c:":%PORT%[^0-9]" >nul
if not errorlevel 1 (
  echo [i] port %PORT% already listening, nothing to do
  exit /b 0
)

start "mitmproxy-%PORT%" /min "%MITMDUMP%" -p %PORT% -q -s "%ADDON%"
timeout /t 6 /nobreak >nul
netstat -ano | findstr /c:"LISTENING" | findstr /r /c:":%PORT%[^0-9]" >nul
if errorlevel 1 (
  echo [x] proxy failed to start on port %PORT%
  pause
  exit /b 2
)
echo [ok] proxy up, out=%MITM_OUT%
