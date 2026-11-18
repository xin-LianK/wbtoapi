// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

import { describe, expect, it } from 'vitest'
import { ALL_FIELDS, CONFIG_TABS, changedFields, fromForm, tabOfField, toForm, validateField } from './config'

const field = (name: string) => ALL_FIELDS.find((f) => f.name === name)!

describe('配置表单', () => {
  it('回填与提交互为逆运算：数组变「9, 21」，空输入不提交，开关总是提交', () => {
    const cfg = {
      listen: ':7863',
      schedule: { checkin_hours: [9, 21], checkin_enabled: true },
      pool: { max_in_flight: 3 },
      prompt: { mode: 'custom' },
    }
    const form = toForm(cfg)
    expect(form.checkin_hours).toBe('9, 21')
    expect(form.max_in_flight).toBe('3')
    expect(form.api_key).toBe('')
    expect(form.prefer_expiring).toBe(false)
    const body = fromForm(form) as Record<string, Record<string, unknown>>
    expect(body.listen).toBe(':7863')
    expect(body.schedule.checkin_hours).toEqual([9, 21])
    expect(body.pool.max_in_flight).toBe(3)
    expect('api_key' in body).toBe(false)
    expect(body.pool.prefer_expiring).toBe(false)
    expect(body.schedule.include_disabled_in_tasks).toBe(false)
    expect('server' in body).toBe(false)
  })

  // 从上游 internal/panel/frontend_test.go 的 TestAppJSCollectConfigClearable 平移（期望值原样保留）：
  // 覆盖型字段（user_agent / prompt_file）空串必须照发，其余文本字段空串仍不下发。
  it('覆盖型字段清空也提交，其余空输入不提交', () => {
    const form = { ...toForm({}), listen: '', api_key: 'secret', user_agent: '', prompt_file: '', checkin_hours: '' }
    const out = fromForm(form) as Record<string, Record<string, unknown> | string>
    const has = (o: unknown, k: string) => Object.prototype.hasOwnProperty.call(o || {}, k)
    const up = out.upstream as Record<string, unknown>
    const prompt = out.prompt as Record<string, unknown>
    expect([
      has(up, 'user_agent'), up.user_agent,
      has(prompt, 'file'), prompt.file,
      has(out, 'listen'),
      has(out.schedule, 'checkin_hours'),
      out.api_key,
    ]).toEqual([true, '', true, '', false, false, 'secret'])
  })

  // 上游 TestConfigFormMatchesCFGMap 钉的是「表单控件 ↔ CFG_MAP」一一对应；这里两者是同一份
  // 字段定义，对应关系天然成立，剩下要钉的是：名字和路径不重复，且关键热改键确实在表单里。
  it('字段名与配置路径不重复，request_client_info 在表单里', () => {
    const names = ALL_FIELDS.map((f) => f.name)
    const paths = ALL_FIELDS.map((f) => f.path.join('.'))
    expect(new Set(names).size).toBe(names.length)
    expect(new Set(paths).size).toBe(paths.length)
    expect(field('request_client_info').path).toEqual(['logging', 'request_client_info'])
    expect(field('request_client_info').kind).toBe('switch')
  })

  it('跨域负载均衡是三选一：回填后端规范化过的取值，原样提交', () => {
    expect(field('cross_realm').kind).toBe('select')
    expect(field('cross_realm').options!.map((o) => o.id)).toEqual(['off', 'merge', 'cn_first'])
    const form = toForm({ pool: { cross_realm: 'cn_first' } })
    expect(form.cross_realm).toBe('cn_first')
    const body = fromForm(form) as Record<string, Record<string, unknown>>
    expect(body.pool.cross_realm).toBe('cn_first')
  })

  it('时长、时点、数字校验', () => {
    expect(validateField(field('soft_rate'), '1h30m')).toBeNull()
    expect(validateField(field('soft_rate'), '30 分钟')).not.toBeNull()
    expect(validateField(field('soft_rate'), '')).toBeNull()
    // 单写 0 是 Go 合法时长（「0 关闭 / 不限制」），其余数字必须带单位
    expect(validateField(field('read_timeout'), '0')).toBeNull()
    expect(validateField(field('expiring_soon'), '0')).toBeNull()
    expect(validateField(field('read_timeout'), '300')).not.toBeNull()
    expect(validateField(field('read_timeout'), '00')).not.toBeNull()
    expect(validateField(field('checkin_hours'), '9，21')).toBeNull()
    expect(validateField(field('checkin_hours'), '9, 24')).not.toBeNull()
    expect(validateField(field('max_in_flight_global'), '0')).not.toBeNull()
    // 保留天数 0 = 永久保留，容量上限至少 1 MiB
    expect(validateField(field('request_retention_days'), '0')).toBeNull()
    expect(validateField(field('task_log_retention_days'), '-1')).not.toBeNull()
    expect(validateField(field('system_log_max_mb'), '0')).not.toBeNull()
  })

  it('日志留存：五类各一张卡片，保存即生效，不标需重启', () => {
    const tab = CONFIG_TABS.find((t) => t.id === 'logs')!
    expect(tab.sections.map((s) => s.store)).toEqual(['requests', 'task', 'system', 'usage', 'credit'])
    expect(tab.sections.flatMap((s) => s.fields).filter((f) => f.restart)).toEqual([])
    expect(field('task_log_enabled').path).toEqual(['logging', 'task_log_enabled'])
    expect(field('system_log_max_mb').path).toEqual(['logging', 'system_log_max_mb'])
    expect(field('usage_retention_days').path).toEqual(['logging', 'usage_retention_days'])
    expect(field('credit_log_retention_days').path).toEqual(['logging', 'credit_log_retention_days'])
    expect(tabOfField('request_client_info')).toBe('logs')
    const body = fromForm({ ...toForm({}), usage_retention_days: '0', task_log_retention_days: '14' }) as { logging: Record<string, unknown> }
    expect(body.logging).toMatchObject({ usage_retention_days: 0, task_log_retention_days: 14, task_log_enabled: false })
  })

  it('统计改动项（忽略首尾空格）', () => {
    const a = toForm({ listen: ':7863' })
    expect(changedFields(a, { ...a, listen: ' :7863 ' })).toEqual([])
    expect(changedFields(a, { ...a, listen: ':8000', prefer_expiring: true })).toEqual(['listen', 'prefer_expiring'])
  })

  // 键值对字段（upstream.effort_overrides）：对象在表单里是「k=v, k2=v2」文本。
  it('键值对字段：回填按 key 排序、提交回对象、清空发 null', () => {
    const cfg = { upstream: { effort_overrides: { 'glm-5.2': 'high', 'deepseek-v4.1-flash': 'high' } } }
    const form = toForm(cfg)
    expect(form.effort_overrides).toBe('deepseek-v4.1-flash=high, glm-5.2=high')
    const body = fromForm({ ...form, effort_overrides: 'a=low, b=max' }) as { upstream: Record<string, unknown> }
    expect(body.upstream.effort_overrides).toEqual({ a: 'low', b: 'max' })
    // 清空必须发 null：后端保存是逐键深合并，发 {} 等于没改动，删不掉已有条目
    const cleared = fromForm({ ...form, effort_overrides: '' }) as { upstream: Record<string, unknown> }
    expect(cleared.upstream.effort_overrides).toBeNull()
  })

  it('键值对字段校验：格式与档位白名单', () => {
    const f = field('effort_overrides')
    expect(validateField(f, '')).toBeNull()
    expect(validateField(f, 'deepseek-v4.1-flash=high')).toBeNull()
    expect(validateField(f, 'cn:deepseek-v4.1-flash=HIGH')).toBeNull()
    expect(validateField(f, 'deepseek-v4.1-flash')).toContain('模型=档位')
    expect(validateField(f, 'deepseek-v4.1-flash=ultra')).toContain('档位只能是')
  })

  // 紧迫度加权三参数（pool.urgency_*）：纯数字字段，空值 = 沿用现值。
  it('紧迫度加权参数能回填与提交，负值被校验拦住', () => {
    const cfg = { pool: { urgency_baseline: 2.5, urgency_max_boost: 8, urgency_min_credits: 50 } }
    const form = toForm(cfg)
    expect(form.urgency_baseline).toBe('2.5')
    expect(form.urgency_max_boost).toBe('8')
    expect(form.urgency_min_credits).toBe('50')

    const body = fromForm({ ...form, urgency_max_boost: '6', urgency_min_credits: '20' }) as {
      pool: Record<string, unknown>
    }
    expect(body.pool.urgency_max_boost).toBe(6)
    expect(body.pool.urgency_min_credits).toBe(20)
    // 清空 = 不提交该键（沿用服务端现值）
    const cleared = fromForm({ ...form, urgency_baseline: '' }) as { pool: Record<string, unknown> }
    expect('urgency_baseline' in cleared.pool).toBe(false)

    expect(validateField(field('urgency_max_boost'), '-1')).toBeTruthy()
    expect(validateField(field('urgency_min_credits'), '-5')).toBeTruthy()
  })
})
