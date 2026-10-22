'use strict'
// Injected into the WorkBuddy desktop process via NODE_OPTIONS=--require so that
// the ACP chat transport (which uses the global `fetch`, i.e. Node's bundled
// undici) also honours HTTP_PROXY/HTTPS_PROXY.
//
// Why this is needed at all
// -------------------------
// The client makes its requests over two completely different stacks:
//   * axios + HttpsProxyAgent  -> reads HTTP(S)_PROXY from the environment
//     (bootstrap: /v3/config, /v2/report, telemetry). These DO show up on the
//     mitmproxy capture.
//   * global fetch (undici)    -> the ACP transport in main/server.js
//     (`tryEstablishGetSse` / `BgAgentApiClient`). undici ignores the proxy
//     environment variables entirely, so chat goes straight out and can never
//     be captured. That is why the proxy log shows /v3/config but not a single
//     copilot.tencent.com request.
//
// undici keeps its process-wide dispatcher on a well-known symbol
// (Symbol.for('undici.globalDispatcher.1')) which Node's *built-in* undici also
// reads, so replacing it here redirects the built-in `fetch` as well.
//
// NODE_USE_ENV_PROXY would have been the one-liner, but it landed in Node
// v24.0.0 and this client ships Electron 37.10.3 / Node 22.21.1.
//
// Everything is wrapped in try/catch and logged: a failure here must never stop
// the app from booting. Also loaded by CLI child processes, where
// process.resourcesPath is absent -- the vendored copy below covers those.
const fs = require('fs')

const LOG = 'D:\\demo\\workspace\\_wb2api\\capture\\undici-patch.log'

function log(msg) {
  try {
    fs.appendFileSync(LOG, new Date().toISOString() + ' [pid ' + process.pid + '] ' + msg + '\n')
  } catch (e) { /* logging must never break the host */ }
}

const CANDIDATES = [
  process.resourcesPath ? process.resourcesPath + '\\app.asar\\node_modules\\undici' : null,
  'D:\\demo\\workspace\\_wb2api\\tools\\vendor\\undici',
].filter(Boolean)

log('entry resourcesPath=' + (process.resourcesPath || 'n/a') + ' type=' + (process.type || 'node'))

let undici = null
let used = null
for (const c of CANDIDATES) {
  try {
    undici = require(c)
    used = c
    break
  } catch (e) {
    log('require failed ' + c + ': ' + (e && e.message))
  }
}

if (!undici) {
  log('no undici module found, giving up')
} else {
  try {
    const proxy = process.env.HTTPS_PROXY || process.env.HTTP_PROXY
    if (!proxy) {
      log('no HTTPS_PROXY/HTTP_PROXY in env, skip')
    } else if (typeof undici.EnvHttpProxyAgent !== 'function') {
      log('EnvHttpProxyAgent missing in ' + used)
    } else {
      undici.setGlobalDispatcher(new undici.EnvHttpProxyAgent())
      log('dispatcher replaced via ' + used + ' proxy=' + proxy +
          ' no_proxy=' + (process.env.NO_PROXY || ''))
    }
  } catch (e) {
    log('FAILED: ' + ((e && e.stack) || e))
  }
}
