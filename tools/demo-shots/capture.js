// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

// 用 Chrome 的 CDP 给面板截图（README 用）。不依赖 puppeteer：Node 22 自带 WebSocket。
//
// 用法：node tools/demo-shots/capture.js <spec.json> [chrome 路径]
// spec.json 是一个数组，每项：
//   { "url": "...", "out": "docs/images/xxx.webp", "width": 1440, "height": 900,
//     "theme": "light" | "dark", "mobile": false, "wait": 4000 }
//
// 之所以走 CDP 而不是 `chrome --screenshot`：面板是 hash 路由的单页应用，截图前要先把
// 「更新提醒」的 localStorage 记录写好，否则每张图都会被「已更新到 vX」的弹窗挡住。
const fs = require('fs')
const path = require('path')
const { spawn } = require('child_process')

const specPath = process.argv[2]
const chromePath = process.argv[3] || 'C:/Program Files/Google/Chrome/Application/chrome.exe'
if (!specPath) {
  console.error('用法：node capture.js <spec.json> [chrome 路径]')
  process.exit(1)
}
const shots = JSON.parse(fs.readFileSync(specPath, 'utf8'))
const port = 9333 + (process.pid % 200)
const profile = path.join(require('os').tmpdir(), 'wb2api-shot-profile-' + process.pid)

const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

async function json(pathname) {
  const res = await fetch(`http://127.0.0.1:${port}${pathname}`)
  return res.json()
}

function cdp(ws) {
  let id = 0
  const pending = new Map()
  ws.addEventListener('message', (ev) => {
    const msg = JSON.parse(ev.data)
    const p = pending.get(msg.id)
    if (p) {
      pending.delete(msg.id)
      msg.error ? p.reject(new Error(JSON.stringify(msg.error))) : p.resolve(msg.result)
    }
  })
  return (method, params = {}, sessionId) =>
    new Promise((resolve, reject) => {
      const mid = ++id
      pending.set(mid, { resolve, reject })
      ws.send(JSON.stringify({ id: mid, method, params, sessionId }))
    })
}

async function main() {
  const chrome = spawn(chromePath, [
    '--headless=new',
    '--disable-gpu',
    '--hide-scrollbars',
    '--no-first-run',
    '--no-default-browser-check',
    '--disable-extensions',
    `--remote-debugging-port=${port}`,
    `--user-data-dir=${profile}`,
    'about:blank',
  ], { stdio: 'ignore' })

  // 等 DevTools 端口起来
  let version
  for (let i = 0; i < 50; i++) {
    try {
      version = await json('/json/version')
      break
    } catch {
      await sleep(200)
    }
  }
  if (!version) throw new Error('Chrome 没起来（DevTools 端口无响应）')

  const ws = new WebSocket(version.webSocketDebuggerUrl)
  await new Promise((r, j) => {
    ws.addEventListener('open', r, { once: true })
    ws.addEventListener('error', j, { once: true })
  })
  const send = cdp(ws)

  const { targetId } = await send('Target.createTarget', { url: 'about:blank' })
  const { sessionId } = await send('Target.attachToTarget', { targetId, flatten: true })
  const call = (method, params) => send(method, params, sessionId)

  await call('Page.enable')
  await call('Runtime.enable')
  // 首屏前写好「更新提醒已看过」，否则每张图都会被「已更新到 vX」的弹窗挡住。
  // 主题走 'system' + 模拟 prefers-color-scheme，跟面板自己的首屏逻辑同一条路径。
  // 要拍「本次更新」弹窗本身时，spec 里给 keepNotice: true。
  if (!shots.every((s) => s.keepNotice)) {
    await call('Page.addScriptToEvaluateOnNewDocument', {
      source: `try {
        localStorage.setItem('heroui-theme', 'system');
        localStorage.setItem('wb2api.seenVersion', '1.19.0-panel');
        localStorage.setItem('wb2api.dismissedUpdate', '1.19.0-panel');
      } catch (e) {}`,
    })
  }

  for (const shot of shots) {
    const { url, out, width = 1440, height = 900, theme = 'light', mobile = false, wait = 4000, scroll = 0 } = shot
    await call('Emulation.setDeviceMetricsOverride', {
      width, height, deviceScaleFactor: 2, mobile,
      screenWidth: width, screenHeight: height,
    })
    await call('Emulation.setEmulatedMedia', {
      features: [{ name: 'prefers-color-scheme', value: theme }],
    })
    await call('Page.navigate', { url })
    await sleep(wait)
    if (scroll) {
      // 面板的主滚动容器不一定是 window，把所有可滚动元素都滚到位（只有一个会真的动）
      await call('Runtime.evaluate', {
        expression: `(() => {
          const set = (el) => {
            if (!el || el.scrollHeight <= el.clientHeight + 40) return;
            if (getComputedStyle(el).overflowY === 'hidden') return;
            el.scrollTop = ${scroll};
          };
          set(document.scrollingElement);
          set(document.documentElement);
          document.querySelectorAll('main, div, section').forEach(set);
        })()`,
      })
      await sleep(700)
    }
    const { data } = await call('Page.captureScreenshot', { format: 'webp', quality: 82, captureBeyondViewport: false })
    fs.mkdirSync(path.dirname(out), { recursive: true })
    fs.writeFileSync(out, Buffer.from(data, 'base64'))
    console.log(`${out}  ${width}x${height} ${theme}${mobile ? ' mobile' : ''}`)
  }

  ws.close()
  chrome.kill()
  await sleep(300)
  fs.rmSync(profile, { recursive: true, force: true })
}

main().catch((err) => {
  console.error('截图失败：', err.message)
  process.exit(1)
})
