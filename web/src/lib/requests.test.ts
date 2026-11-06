// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

// 从上游 internal/panel/frontend_test.go 的 TestAppJSRequestLogFormatting / TestAppJSRequestMatch 平移（期望值原样保留）。
import { describe, expect, it } from 'vitest'
import { cacheLevel, cacheRateText, matchRequest, mergeRequests, outcomeLevel, recentCount, requestCredit, requestLogText, type RequestFilter } from './requests'
import type { RequestEvent } from './types'

describe('请求指标', () => {
  it('摘要行紧凑可读，带调用来源与缓存命中；来源缺失时以 — 兜底', () => {
    const time = new Date(2026, 8, 28, 14, 5, 6).toISOString()
    const good: RequestEvent = {
      time, status: 200, ok: true, outcome: 'success', path: '/v1/chat/completions', model: 'glm-5.3', account: '账号(uid8)',
      duration_ms: 1250, total_tokens: 2300, credit_known: true, credit: 0.12, request_id: 'req-1',
      client_ip: '203.0.113.7', user_agent: 'python-requests/2.31.0',
    }
    const noSource: RequestEvent = { ...good, request_id: 'req-3', client_ip: '', user_agent: '' }
    const cached: RequestEvent = { ...good, request_id: 'req-2', cache_hit_tokens: 2257, cache_miss_tokens: 43 }
    const keyed: RequestEvent = { ...good, request_id: 'req-4', api_key: '张三' }
    expect(requestLogText(good)).toBe('14:05:06 | 200 成功 | glm-5.3 | 账号(uid8) | 203.0.113.7 | python-requests/2.31.0 | 1.25s | 2.3k tok | 0.12 credit | req-1')
    expect(requestLogText(noSource)).toBe('14:05:06 | 200 成功 | glm-5.3 | 账号(uid8) | — | — | 1.25s | 2.3k tok | 0.12 credit | req-3')
    expect(requestLogText(cached)).toBe('14:05:06 | 200 成功 | glm-5.3 | 账号(uid8) | 203.0.113.7 | python-requests/2.31.0 | 1.25s | 2.3k tok | 0.12 credit | 命中 98.1% | req-2')
    expect(requestLogText({ ...good, status: 400, ok: false, outcome: 'http_error', error: 'prompt is too long' }))
      .toBe('14:05:06 | 400 HTTP 错误 | 原因 prompt is too long | glm-5.3 | 账号(uid8) | 203.0.113.7 | python-requests/2.31.0 | 1.25s | 2.3k tok | 0.12 credit | req-1')
    expect(requestLogText(keyed)).toBe('14:05:06 | 200 成功 | glm-5.3 | 账号(uid8) | 密钥 张三 | 203.0.113.7 | python-requests/2.31.0 | 1.25s | 2.3k tok | 0.12 credit | req-4')
    expect(requestLogText({ ...keyed, proxy: '香港1' }))
      .toBe('14:05:06 | 200 成功 | glm-5.3 | 账号(uid8) | 代理 香港1 | 密钥 张三 | 203.0.113.7 | python-requests/2.31.0 | 1.25s | 2.3k tok | 0.12 credit | req-4')
    expect(outcomeLevel('success')).toBe('ok')
    expect(outcomeLevel('http_error')).toBe('error')
    expect(outcomeLevel('interrupted')).toBe('warn')
  })

  it('没返回积分时不显示成 0', () => {
    const e: RequestEvent = { time: '', request_id: '', path: '', status: 200, ok: true, outcome: 'success', duration_ms: 0, prompt_tokens: 3, completion_tokens: 4, credit_known: false }
    expect(requestLogText(e)).toBe('— | 200 成功 | — | — | — | — | — | 7 tok | credit — | —')
    // 扣了 0 积分：后端 omitempty 省掉 credit 字段，但 credit_known 为真
    expect(requestCredit({ ...e, credit_known: true })).toBe('0 credit')
  })

  it('缓存命中率：没有样本显示 —，按 90% / 80% 分级', () => {
    expect(cacheRateText(0, 0)).toBe('—')
    expect(cacheRateText(9, 1)).toBe('90%')
    expect([cacheLevel(95), cacheLevel(85), cacheLevel(50)]).toEqual(['ok', 'warn', 'error'])
  })

  it('筛选：IP / UA / 模型 / 账号 / 代理 / 密钥 / 失败原因 / 请求 ID 包含匹配（空格分词 AND），结果精确匹配', () => {
    const base = { outcome: 'success', client_ip: '203.0.113.7', user_agent: 'python-requests/2.31.0', model: 'cn:glm-5.3', account: '示例(uid8)', proxy: '香港1', api_key: '张三', request_id: 'req-1' } as RequestEvent
    const other = { outcome: 'http_error', client_ip: '198.51.100.4', user_agent: 'Mozilla/5.0 Chrome/120', model: 'global:hy3', account: '甲(uid9)', error: 'Prompt too long: 200000 tokens', request_id: 'req-2' } as RequestEvent
    const pick = (f: RequestFilter) => [base, other].filter((e) => matchRequest(e, f)).map((e) => e.request_id)
    expect(pick({ q: '', outcome: '' })).toEqual(['req-1', 'req-2'])
    expect(pick({ q: '203.0.113', outcome: '' })).toEqual(['req-1'])
    expect(pick({ q: 'chrome/120', outcome: '' })).toEqual(['req-2'])
    expect(pick({ q: 'glm', outcome: '' })).toEqual(['req-1'])
    expect(pick({ q: '张三', outcome: '' })).toEqual(['req-1'])
    expect(pick({ q: '香港', outcome: '' })).toEqual(['req-1'])
    // q 不匹配结果（结果有独立的下拉）
    expect(pick({ q: 'glm success', outcome: '' })).toEqual([])
    expect(pick({ q: 'glm chrome', outcome: '' })).toEqual([])
    expect(pick({ q: '', outcome: 'http_error' })).toEqual(['req-2'])
    expect(pick({ q: '198.51', outcome: 'http_error' })).toEqual(['req-2'])
    expect(pick({ q: 'prompt too', outcome: '' })).toEqual(['req-2'])
    // 账号现在的显示名（换成了备注）也能搜到，记录里的旧昵称照样能搜
    expect([base, other].filter((e) => matchRequest(e, { q: '主力', outcome: '' }, e === base ? '主力(uid8)' : '')).map((e) => e.request_id)).toEqual(['req-1'])
    expect(matchRequest(base, { q: '示例', outcome: '' }, '主力(uid8)')).toBe(true)
  })
})

describe('实时增量合并', () => {
  const at = (sec: number) => new Date(2026, 10, 6, 10, 0, sec).toISOString()
  const ev = (id: string, sec: number): RequestEvent => ({
    time: at(sec), request_id: id, path: '/v1/chat/completions', status: 200, ok: true,
    outcome: 'success', duration_ms: 10, credit_known: false,
  })

  it('按 request_id 去重，同一条既在快照又进增量时只留一份', () => {
    const base = [ev('b', 20), ev('a', 10)]
    const incoming = [ev('c', 30), ev('b', 20)]
    expect(mergeRequests(base, incoming).map((e) => e.request_id)).toEqual(['c', 'b', 'a'])
  })

  it('按时间倒序，不依赖到达顺序', () => {
    const merged = mergeRequests([ev('a', 10)], [ev('c', 15), ev('b', 12), ev('d', 30)])
    expect(merged.map((e) => e.request_id)).toEqual(['d', 'c', 'b', 'a'])
  })

  it('超过上限截掉最旧的', () => {
    const many = Array.from({ length: 10 }, (_, i) => ev('r' + i, i))
    expect(mergeRequests(many, [ev('new', 99)], 4).map((e) => e.request_id)).toEqual(['new', 'r9', 'r8', 'r7'])
    // 没有增量时原样返回同一引用（页面每 20 秒收一次空心跳，不重建数组才不白重渲染）
    expect(mergeRequests(many, [], 100)).toBe(many)
  })

  it('没有 request_id 的旧记录按时间 + 状态 + 模型兜底去重', () => {
    const a: RequestEvent = { ...ev('', 10), request_id: '' }
    expect(mergeRequests([a], [a])).toHaveLength(1)
  })

  it('近 1 分钟计数只算窗口内的', () => {
    const now = new Date(2026, 10, 6, 10, 2, 0).getTime()
    const rows = [ev('a', 30), ev('b', 20)] // 10:00:30 / 10:00:20 都超出 60s
    expect(recentCount(rows, now)).toBe(0)
    expect(recentCount([{ ...ev('c', 0), time: new Date(now - 30_000).toISOString() }], now)).toBe(1)
  })
})
