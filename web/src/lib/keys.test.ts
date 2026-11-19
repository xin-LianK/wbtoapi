// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

import { describe, expect, it } from 'vitest'
import {
  addModelPattern, allowedModels, bareModelIds, emptyKeyForm, keyFormFrom, keyInput, keyStatus, limitRows, maskSecret,
  secretPreview, validateKeyForm,
} from './keys'
import type { ApiKey } from './types'

const key = (over: Partial<ApiKey> = {}): ApiKey => ({
  id: 'k1', name: '张三', secret: 'sk-AbCdEfGhIjKlMnOpQrStUvWxYz0123456', enabled: true, created_at: '2026-10-20T00:00:00Z',
  daily_limit: {}, total_limit: {}, used: { requests: 0, tokens: 0, credits: 0 }, today: { requests: 0, tokens: 0, credits: 0 },
  expired: false, ...over,
})

describe('keyStatus', () => {
  it('先看停用再看到期', () => {
    expect(keyStatus(key())).toBe('active')
    expect(keyStatus(key({ expired: true }))).toBe('expired')
    expect(keyStatus(key({ enabled: false, expired: true }))).toBe('disabled')
  })
})

describe('maskSecret', () => {
  it('遮住中间', () => {
    expect(maskSecret('sk-AbCdEfGhIjKlMnOpQrStUvWxYz0123456')).toBe('sk-AbCd••••3456')
    expect(maskSecret('sk-short')).toBe('sk-••••')
  })
})

describe('limitRows', () => {
  it('只列设了上限的项，已用 ≥ 上限算满', () => {
    const rows = limitRows({ requests: 10, credits: 2.5 }, { requests: 10, tokens: 999, credits: 1 })
    expect(rows).toEqual([
      { kind: 'requests', label: '请求次数', used: 10, limit: 10, pct: 100, full: true },
      { kind: 'credits', label: '积分', used: 1, limit: 2.5, pct: 40, full: false },
    ])
    expect(limitRows({}, { requests: 5, tokens: 5, credits: 5 })).toEqual([])
    expect(limitRows({ tokens: 10 }, { requests: 0, tokens: 30, credits: 0 })[0].pct).toBe(100)
  })
})

describe('表单与提交', () => {
  it('从密钥回填再提交，往返不丢字段', () => {
    const k = key({
      note: '朋友', expires_at: '2026-11-01T00:00:00Z', daily_limit: { credits: 10 }, total_limit: { requests: 100, tokens: 5000 },
      models: ['glm-*'], groups: ['g1'],
    })
    const f = keyFormFrom(k)
    expect(f.daily).toEqual({ requests: null, tokens: null, credits: 10 })
    expect(keyInput(f)).toEqual({
      name: '张三', note: '朋友', enabled: true, expires_at: '2026-11-01T00:00:00.000Z',
      daily_limit: { credits: 10 }, total_limit: { requests: 100, tokens: 5000 }, models: ['glm-*'], groups: ['g1'],
    })
  })
  it('没有到期时间、零值上限、零值时间', () => {
    const f = keyFormFrom(key({ expires_at: '0001-01-01T00:00:00Z', models: null, groups: null }))
    expect(f.expires).toBeNull()
    f.daily.requests = 0
    f.total.tokens = 12.7
    f.total.credits = Number.NaN
    f.name = '  李四 '
    expect(keyInput(f)).toMatchObject({ name: '李四', expires_at: null, daily_limit: {}, total_limit: { tokens: 12 }, models: [], groups: [] })
  })
  it('校验', () => {
    const f = emptyKeyForm()
    expect(validateKeyForm(f)).toBe('密钥名称不能为空')
    f.name = '名'.repeat(65)
    expect(validateKeyForm(f)).toBe('密钥名称最多 64 个字')
    f.name = 'a'
    f.note = 'x'.repeat(501)
    expect(validateKeyForm(f)).toBe('备注最多 500 个字')
    f.note = ''
    f.total.credits = -1
    expect(validateKeyForm(f)).toBe('用量上限不能是负数')
    f.total.credits = null
    f.models = Array.from({ length: 101 }, (_, i) => 'm' + i)
    expect(validateKeyForm(f)).toBe('模型白名单最多 100 项')
    f.models = []
    expect(validateKeyForm(f)).toBeNull()
  })
})

describe('密钥形态', () => {
  it('默认形态不带任何原文字段', () => {
    const f = emptyKeyForm()
    expect(f.secretMode).toBe('random')
    f.name = 'a'
    expect(keyInput(f, true)).not.toHaveProperty('secret')
    expect(keyInput(f, true)).not.toHaveProperty('secret_prefix')
    expect(validateKeyForm(f, true)).toBeNull()
    expect(secretPreview(f)).toBeNull()
  })
  it('自定义前缀：去掉首尾空白后提交，预览只到前缀', () => {
    const f = emptyKeyForm()
    f.name = 'a'
    f.secretMode = 'prefix'
    f.prefix = '  teamA_ '
    expect(keyInput(f, true)).toMatchObject({ secret_prefix: 'teamA_' })
    expect(keyInput(f, true)).not.toHaveProperty('secret')
    expect(secretPreview(f)).toBe('teamA_••••••••')
    expect(validateKeyForm(f, true)).toBeNull()
  })
  it('编辑已有密钥时不提交原文字段，改了形态也不带', () => {
    const f = keyFormFrom(key())
    f.secretMode = 'prefix'
    f.prefix = 'zz_'
    expect(keyInput(f)).not.toHaveProperty('secret')
    expect(keyInput(f)).not.toHaveProperty('secret_prefix')
    expect(validateKeyForm(f)).toBeNull()
  })
  it('前缀的本地校验与网关一致', () => {
    const f = emptyKeyForm()
    f.name = 'a'
    f.secretMode = 'prefix'
    expect(validateKeyForm(f, true)).toMatch(/填一下前缀/)
    f.prefix = 'z'.repeat(33)
    expect(validateKeyForm(f, true)).toBe('自定义前缀最多 32 个字符')
    f.prefix = 'zz  _'
    expect(validateKeyForm(f, true)).toBe('自定义前缀只能用字母、数字和 . _ -')
    f.prefix = 'teamA_'
    expect(validateKeyForm(f, true)).toBeNull()
  })
})

describe('模型白名单', () => {
  it('加一项：去空白、拒前缀、不区分大小写去重', () => {
    expect(addModelPattern(['glm-*'], '  ')).toEqual(['glm-*'])
    expect(addModelPattern(['glm-*'], ' GLM-* ')).toEqual(['glm-*'])
    expect(addModelPattern(['glm-*'], 'kimi-k2')).toEqual(['glm-*', 'kimi-k2'])
    expect(addModelPattern([], 'cn:glm-5.2')).toMatch(/不带 cn: \/ global: 前缀/)
    expect(addModelPattern([], 'x'.repeat(129))).toMatch(/最长 128/)
    expect(addModelPattern([], 'glm 5')).toMatch(/不能有空格/)
  })
  it('目录候选与放行范围', () => {
    const bare = bareModelIds(['cn:glm-5.2', 'glm-5.2', 'global:gpt-5.5', 'deepseek-v4.1-flash'])
    expect(bare).toEqual(['deepseek-v4.1-flash', 'glm-5.2', 'gpt-5.5'])
    expect(allowedModels([], bare)).toEqual(bare)
    expect(allowedModels(['GLM-*', 'gpt-5.5'], bare)).toEqual(['glm-5.2', 'gpt-5.5'])
  })
})
