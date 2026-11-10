// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

// 生成 README 截图用的演示数据（假账号、假用量、假请求日志），不含任何真实信息。
// 用法：node tools/demo-shots/gen-demo-data.js <输出目录>
const fs = require('fs')
const path = require('path')

const out = process.argv[2]
if (!out) {
  console.error('用法：node gen-demo-data.js <输出目录>')
  process.exit(1)
}

const now = new Date()
const iso = (d) => d.toISOString()
const day = (d) => iso(d).slice(0, 10)
const back = (mins) => new Date(now.getTime() - mins * 60000)
const fwd = (mins) => new Date(now.getTime() + mins * 60000)

const accounts = [
  { uid: 'demo-01', nick: '研发主号', realm: 'cn', credits: 8420, total: 9600, ok: 428, fail: 3, lastUse: 3, buckets: [[2, 1200], [10, 3200], [25, 4020]] },
  { uid: 'demo-02', nick: '日常备用', realm: 'cn', credits: 5310, total: 6000, ok: 216, fail: 1, lastUse: 41, buckets: [[4, 800], [18, 4510]] },
  { uid: 'demo-03', nick: '国际号', realm: 'global', credits: 2180, total: 2400, ok: 96, fail: 12, lastUse: 2, buckets: [[6, 600], [22, 1580]], cool: { kind: 'soft_rate', mins: 9 } },
  { uid: 'demo-04', nick: '活动号', realm: 'cn', credits: 640, total: 800, ok: 54, fail: 0, lastUse: 128, buckets: [[1, 320], [3, 320]] },
  { uid: 'demo-05', nick: '旧设备号', realm: 'cn', credits: 120, total: 500, ok: 33, fail: 9, lastUse: 2880, buckets: [], disabled: '上游返回账号状态异常（11140），已停止选号' },
  { uid: 'demo-06', nick: '新注册号', realm: 'cn', credits: 1950, total: 2000, ok: 12, fail: 0, lastUse: 76, buckets: [[9, 950], [30, 1000]] },
]

for (const dir of ['auths', 'data', 'data/request-logs']) {
  fs.mkdirSync(path.join(out, dir), { recursive: true })
}

// 凭证：只为过账池加载，token 是占位串，expiresAt 放远期避免触发出站刷新
for (const a of accounts) {
  fs.writeFileSync(
    path.join(out, 'auths', `workbuddy-${a.uid}.json`),
    JSON.stringify({
      account: { enterpriseId: '', nickname: a.nick, uid: a.uid },
      auth: {
        accessToken: 'demo-access-token-' + a.uid,
        domain: a.realm === 'global' ? 'www.workbuddy.ai' : 'www.codebuddy.cn',
        expiresAt: Math.floor(fwd(60 * 24 * 300).getTime() / 1000),
        realm: a.realm,
        refreshToken: 'demo-refresh-token-' + a.uid,
      },
    }, null, 2),
  )
}

// 池状态
const state = { accounts: {} }
for (const a of accounts) {
  const buckets = a.buckets.map(([days, remain]) => {
    const at = new Date(now.getTime() + days * 86400000)
    return { date: day(at), expires_at: iso(at), remain }
  })
  const expiring = buckets.filter((b) => new Date(b.expires_at) - now <= 7 * 86400000)
  const st = {
    credits: a.credits,
    credits_total: a.total,
    disabled: Boolean(a.disabled),
    until: '0001-01-01T00:00:00Z',
    cool_kind: 0,
    success_count: a.ok,
    last_success: iso(back(a.lastUse)),
    last_err: '0001-01-01T00:00:00Z',
    last_checkin_day: day(now),
    token_usage: {
      request_count: a.ok + a.fail,
      usage_count: a.ok + a.fail,
      prompt_tokens: a.ok * 3120 + 900,
      completion_tokens: a.ok * 740 + 210,
      total_tokens: a.ok * 3860 + 1110,
      last_latency_ms: 2400 + a.ok,
      last_tokens_per_second: 62.4,
      last_used_at: iso(back(a.lastUse)),
      last_model: 'deepseek-v4.1-flash',
    },
    soft_streak: a.cool ? 2 : 0,
    session_dead_fails: a.fail,
    consecutive_fails: a.cool ? 1 : 0,
    credits_expiring: expiring.reduce((n, b) => n + b.remain, 0),
    credits_earliest_expiry: buckets.length ? buckets[0].expires_at : '0001-01-01T00:00:00Z',
    credits_earliest_remaining: buckets.length ? buckets[0].remain : 0,
    credits_expiry_buckets: buckets,
  }
  if (a.cool) {
    st.cool_kind = 1
    st.until = iso(fwd(a.cool.mins))
    st.last_err = iso(back(2))
  }
  if (a.disabled) {
    st.disabled_reason = a.disabled
    st.until = iso(back(180))
  }
  state.accounts[a.uid] = st
}
fs.writeFileSync(path.join(out, 'data', 'state.json'), JSON.stringify(state, null, 2))

// 用量桶：最近 7 天，每小时若干，覆盖三个模型与全部账号
const models = ['deepseek-v4.1-flash', 'step-5-preview', 'glm-5.2']
const buckets = []
for (let h = 7 * 24; h >= 1; h--) {
  const at = new Date(now.getTime() - h * 3600000)
  const hour = at.getHours()
  // 工作时间多、深夜少，形成好看的柱状分布
  const load = hour >= 9 && hour <= 22 ? 2 + ((h * 7) % 4) : (h % 2)
  for (let i = 0; i < load; i++) {
    const a = accounts[(h + i) % accounts.length]
    const m = models[(h + i) % models.length]
    const req = 1
    const failed = (h * 13 + i) % 37 === 0 ? 1 : 0
    const p = 1800 + ((h * 37 + i * 13) % 2600)
    const c = 320 + ((h * 19 + i * 7) % 780)
    const t = p + c
    buckets.push({
      s: 'h:' + iso(at).slice(0, 13),
      r: a.realm,
      u: a.uid,
      m,
      x: '0.11',
      q: req,
      e: failed,
      p, c, t,
      l: 1800 + ((h * 91 + i * 29) % 5200),
      ln: req,
      v: 38 + ((h * 13 + i * 5) % 60),
      vn: req,
      cr: Number(((t / 1000) * 0.09).toFixed(2)),
      cn: req,
      ct: t,
      cm: p,
    })
  }
}
fs.writeFileSync(path.join(out, 'data', 'usage.json'), JSON.stringify({ version: 4, saved: iso(now), buckets }, null, 2))

// 积分历史：签到、补偿、任务、对话消耗
const entries = []
const pushEntry = (mins, uid, delta, after) => entries.push({ time: iso(back(mins)), uid, delta, before: after - delta, after })
let base = 8420
pushEntry(60 * 20, 'demo-01', 500, base); base += 0
const flows = [
  [60 * 18, 'demo-02', 100, 5310],
  [60 * 16, 'demo-03', -18, 2180],
  [60 * 12, 'demo-01', -34, 8420],
  [60 * 9, 'demo-06', 300, 1950],
  [60 * 7, 'demo-04', 100, 640],
  [60 * 5, 'demo-02', -12, 5310],
  [60 * 3, 'demo-03', 200, 2180],
  [60 * 1, 'demo-01', -9, 8420],
]
for (const [m, uid, d, after] of flows) pushEntry(m, uid, d, after)
const snapshot = {}
for (const a of accounts) snapshot[a.uid] = a.credits
fs.writeFileSync(path.join(out, 'data', 'credit-history.json'), JSON.stringify({ version: 1, saved: iso(now), snapshot, entries }, null, 2))

// 请求记录：两天，成功为主，混少量 429 / 402 / 502
const paths = ['/v1/chat/completions', '/v1/messages', '/v1/responses']
const uas = [
  'OpenAI/Python 1.99.1',
  'claude-cli/2.0.14 (external, cli)',
  'codex_cli_rs/0.52.0',
  'CherryStudio/1.6.4',
  'curl/8.11.1',
]
const byDay = new Map()
let seq = 0
for (let i = 0; i < 220; i++) {
  const mins = 5 + i * 11 + (i % 7) * 3
  const at = back(mins)
  const a = accounts[i % accounts.length]
  const m = models[i % models.length]
  const roll = (i * 37) % 100
  const bad = a.uid === 'demo-05' || roll < 6
  const status = !bad ? 200 : roll < 3 ? 429 : roll < 5 ? 402 : 502
  const p = 1500 + ((i * 211) % 3000)
  const c = 280 + ((i * 97) % 900)
  const rec = {
    time: iso(at),
    request_id: 'req-' + (0x100000 + i * 7919).toString(16).padStart(12, '0'),
    path: paths[i % paths.length],
    account: `${a.nick}(${a.uid})`,
    model: (a.realm === 'global' ? 'global:' : 'cn:') + m,
    status,
    ok: status === 200,
    outcome: status === 200 ? 'success' : status === 429 ? 'rate_limited' : status === 402 ? 'credits' : 'upstream',
    duration_ms: 900 + ((i * 173) % 7200),
    attempts: status === 200 ? 1 + (i % 3 === 0 ? 1 : 0) : 2,
    prompt_tokens: status === 200 ? p : 0,
    completion_tokens: status === 200 ? c : 0,
    total_tokens: status === 200 ? p + c : 0,
    credit_known: status === 200,
    cache_miss_tokens: status === 200 ? p : 0,
    client_ip: '203.0.113.' + (10 + (i % 40)),
    user_agent: uas[i % uas.length],
  }
  if (bad) rec.error = status === 429 ? '429 使用量已超出频率限制' : status === 402 ? '402 积分不足' : '502 上游无响应'
  const key = day(at)
  if (!byDay.has(key)) byDay.set(key, [])
  byDay.get(key).push(rec)
  seq++
}
for (const [d, rows] of byDay) {
  fs.writeFileSync(
    path.join(out, 'data', 'request-logs', `requests-${d}.jsonl`),
    rows.map((r) => JSON.stringify(r)).join('\n') + '\n',
  )
}

// 运行配置：不鉴权、不联网、不跑任务
const cfg = {
  listen: ':17863',
  api_key: '',
  auth_dir: path.join(out, 'auths').replace(/\\/g, '/'),
  state_file: path.join(out, 'data', 'state.json').replace(/\\/g, '/'),
  panel: { update_check: false, package_detail_limit: 5 },
  logging: { request_archive_enabled: true, request_client_info: true },
  schedule: {
    checkin_enabled: false, growth_enabled: false, travel_enabled: false, activity_enabled: false,
    keepalive_enabled: false, blackcat_enabled: false, balance_refresh_enabled: false, task_sweep_enabled: false,
  },
  notify: { enabled: false },
  update: { enabled: false },
}
fs.writeFileSync(path.join(out, 'config.json'), JSON.stringify(cfg, null, 2))

console.log('演示数据已生成：', out, '账号', accounts.length, '用量桶', buckets.length, '请求记录', seq)
