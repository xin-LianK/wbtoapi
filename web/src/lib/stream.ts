// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

// 请求记录的实时通道：一个长轮询循环，按 seq 游标只取增量。
//
// 与 TanStack Query 的定时 refetch 不同，这里不用 queryKey：
//   - 轮询间隔由服务端决定（有新事件立刻回，没有就挂 20 秒），空闲时几乎没有请求；
//   - 增量只带新完成的请求，正在翻看的列表不会因为「整表重取」而整体跳动；
//   - 页面切到后台时中断当前等待，回到前台立刻补一次（游标会把离开期间的事件一起带回）。
import { useEffect, useRef, useState } from 'react'
import { api, errorMessage } from './api'
import { mergeRequests } from './requests'
import type { RequestEvent, RequestStream, RequestTotals } from './types'

/** 上一次长轮询最多挂多久（服务端上限 25s，留一点余量给网络） */
const WAIT_MS = 20_000
/** 首次失败后的重试间隔；连续失败时翻倍，最多 10 秒 */
const RETRY_MS = 3_000
/** 内存里保留多少条：面板一次最多显示 300 条，多留没有意义 */
const LIMIT = 300

export interface LiveRequests {
  /** 实时收到的请求，新的在前（已按 request_id 去重） */
  events: RequestEvent[]
  /** 进程级计数：已完成 / 成功率 / 平均耗时 / 进行中 */
  totals: RequestTotals | null
  /** 是否已经成功连上过（区分「还没连上」和「连接断了但保留旧数据」） */
  connected: boolean
  /** 最近一次失败原因，恢复正常后清空 */
  error: string
}

const EMPTY: LiveRequests = { events: [], totals: null, connected: false, error: '' }

/**
 * 订阅请求记录的增量。enabled=false 时不发请求，已收到的数据保留（暂停只是停止刷新）。
 * 恢复订阅时从上次的游标继续；游标过期（网关重启 / 页面挂太久）服务端会回 reset，这里整体替换。
 */
export function useRequestStream(enabled: boolean): LiveRequests {
  const [state, setState] = useState<LiveRequests>(EMPTY)
  const seqRef = useRef(0)

  useEffect(() => {
    if (!enabled) return
    let stopped = false
    let inFlight: AbortController | null = null
    let retry = RETRY_MS

    const loop = async () => {
      while (!stopped) {
        // 后台标签页不占连接：每秒看一眼，回到前台就继续。
        if (document.visibilityState === 'hidden') {
          await sleep(1000)
          continue
        }
        const ctl = new AbortController()
        inFlight = ctl
        try {
          const r = await api<RequestStream>(
            `request_stream?after=${seqRef.current}&wait=${WAIT_MS}`,
            { signal: ctl.signal },
          )
          if (stopped) return
          seqRef.current = Number(r.seq) || 0
          retry = RETRY_MS
          setState((prev) => ({
            events: r.reset ? mergeRequests([], r.events ?? [], LIMIT) : mergeRequests(prev.events, r.events ?? [], LIMIT),
            totals: r.totals ?? prev.totals,
            connected: true,
            error: '',
          }))
        } catch (e) {
          if (stopped) return
          // 主动中断（切后台 / 组件卸载）不是故障，不算错误也不退避。
          if (ctl.signal.aborted) continue
          setState((prev) => ({ ...prev, error: errorMessage(e) }))
          await sleep(retry)
          retry = Math.min(retry * 2, 10_000)
        } finally {
          if (inFlight === ctl) inFlight = null
        }
      }
    }

    const onVisibility = () => {
      if (document.visibilityState === 'visible') inFlight?.abort()
    }
    document.addEventListener('visibilitychange', onVisibility)
    void loop()
    return () => {
      stopped = true
      document.removeEventListener('visibilitychange', onVisibility)
      inFlight?.abort()
    }
  }, [enabled])

  return state
}

const sleep = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms))
