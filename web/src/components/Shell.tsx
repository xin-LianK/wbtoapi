// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

// 外壳：单列布局 + 底部浮动 Dock（mac 风格，与 v1.15.x 静态面板一致）。
// 顶部只留页面标题、运行状态与操作按钮；导航全部收进底部胶囊，滚动到底不被遮挡。
import { useEffect, useState, type ComponentType, type SVGProps } from 'react'
import {
  ArrowRightArrowLeft, ArrowsRotateRight, Boxes3, ChartColumn, Cpu, Gear, Gift, Globe, Key, ListCheck, ListTimeline, Moon, Persons, Plus, Sun,
} from '@gravity-ui/icons'
import { Button, RouterProvider, Toast, Tooltip, toast, useMediaQuery, useTheme } from '@heroui/react'
import { useQueryClient } from '@tanstack/react-query'
import { Link as RouterLink, Outlet, useLocation, useMatches, useRouter } from '@tanstack/react-router'
import { post } from '../lib/api'
import { uptime } from '../lib/format'
import { isExternalHref } from '../lib/links'
import { toastError } from '../lib/hooks'
import { qk, useOverview } from '../lib/queries'
import { AddAccountModal } from './AddAccountModal'
import { BusyButton } from './Feedback'
import { KeyGate } from './KeyGate'
import { UpdateBadge, UpdateNotice, type ManualPanel } from './UpdateNotice'

type Icon = ComponentType<SVGProps<SVGSVGElement>>

const NAV: { to: string; label: string; icon: Icon }[] = [
  { to: '/accounts', label: '账号池', icon: Persons },
  { to: '/proxies', label: '代理', icon: Globe },
  { to: '/usage', label: '用量', icon: ChartColumn },
  { to: '/requests', label: '请求记录', icon: ArrowRightArrowLeft },
  { to: '/packages', label: '积分构成', icon: Boxes3 },
  { to: '/credits', label: '积分获取', icon: Gift },
  { to: '/taskscenter', label: '任务中心', icon: ListCheck },
  { to: '/models', label: '模型与档位', icon: Cpu },
  { to: '/keys', label: '密钥', icon: Key },
  { to: '/config', label: '配置', icon: Gear },
  { to: '/logs', label: '运行日志', icon: ListTimeline },
]

export function Shell() {
  const router = useRouter()
  const isDesktop = useMediaQuery('(min-width: 1024px)')
  const [adding, setAdding] = useState(false)
  const [manualPanel, setManualPanel] = useState<ManualPanel>(null)
  const overview = useOverview()
  const matches = useMatches()
  const title = matches.map((m) => m.staticData.title).filter(Boolean).pop() ?? ''

  useEffect(() => {
    document.title = (title ? title + ' · ' : '') + 'WorkBuddy2API'
  }, [title])

  return (
    <RouterProvider
      navigate={(to) => void router.navigate({ to })}
      // 面板内部路径交给路由生成 #/xxx 地址；https:// 这类完整地址（外链）原样保留
      useHref={(to) => (isExternalHref(to) ? to : router.history.createHref(router.buildLocation({ to }).href))}
    >
      <div className="flex min-h-dvh flex-col">
        <header className="sticky top-0 z-40 flex h-14 shrink-0 items-center gap-2 border-b border-separator bg-background/80 px-4 backdrop-blur-md sm:gap-3 sm:px-6">
          <h1 className="truncate text-base font-semibold sm:text-lg">{title}</h1>
          <StatusPill />
          {overview.data && <span className="hidden shrink-0 text-xs text-muted tabular-nums lg:inline">v{overview.data.version}</span>}
          <UpdateBadge onPress={() => setManualPanel('update')} />
          {overview.data && <span className="hidden truncate text-xs text-muted tabular-nums md:inline">{uptime(overview.data.uptime_sec)}</span>}
          <div className="ml-auto flex items-center gap-1.5 sm:gap-2">
            <ThemeSwitch />
            <RefreshBalance compact={!isDesktop} />
            <Button variant="primary" isIconOnly={!isDesktop} aria-label="添加账号" onPress={() => setAdding(true)}>
              <Plus />{isDesktop && '添加账号'}
            </Button>
          </div>
        </header>
        {/* pb-28 给底部 Dock 留位，滚动到底最后一行不被压住 */}
        <main className="mx-auto flex w-full max-w-7xl flex-1 flex-col gap-4 px-4 pb-28 pt-5 sm:px-6">
          <Outlet />
        </main>
        <Dock />
      </div>
      <AddAccountModal open={adding} onClose={() => setAdding(false)} />
      <KeyGate />
      <UpdateNotice manual={manualPanel} onManualChange={setManualPanel} />
      {/* Toast 放顶部：底部位置会被 Dock 压住 */}
      <Toast.Provider placement="top end" />
    </RouterProvider>
  )
}

/**
 * 底部浮动 Dock：mac 风格胶囊，图标在上、10px 文字在下。
 * 结构与 v1.15.x 静态面板的 .nav 一致——fixed 底部 16px 居中、毛玻璃、
 * 圆角 999、横向可滚（窄屏不换行、不折成两排）。
 */
function Dock() {
  const { pathname } = useLocation()
  return (
    <nav
      aria-label="主导航"
      className="fixed bottom-4 left-1/2 z-50 flex max-w-[calc(100vw-24px)] -translate-x-1/2 items-center overflow-x-auto rounded-full border border-separator bg-surface/85 px-2 py-1.5 shadow-[0_8px_30px_rgba(29,33,41,0.12)] backdrop-blur-xl backdrop-saturate-150 [scrollbar-width:none] [&::-webkit-scrollbar]:hidden"
    >
      <ul className="flex flex-none items-center gap-0.5">
        {NAV.map((item) => {
          const active = pathname === item.to
          return (
            <li key={item.to} className="flex-none">
              <RouterLink
                to={item.to}
                aria-current={active ? 'page' : undefined}
                className={`flex flex-col items-center gap-1 rounded-xl px-3 pb-1.5 pt-2 text-[10px] font-medium leading-none transition-colors ${
                  active
                    ? 'bg-(--ds-color-brand-tint-2) text-(--ds-color-brand)'
                    : 'text-(--ds-text-3) hover:bg-surface-secondary hover:text-foreground'
                }`}
              >
                <item.icon className="size-4" />
                {item.label}
              </RouterLink>
            </li>
          )
        })}
      </ul>
    </nav>
  )
}

/** 顶栏的运行状态胶囊：颜色 + 文字双编码（不只靠色点） */
function StatusPill() {
  const { data } = useOverview()
  const state = !data ? { text: '连接中', dot: 'bg-default' }
    : data.healthy > 0 ? { text: '服务正常', dot: 'bg-success' }
    : data.total ? { text: '无可用账号', dot: 'bg-warning' }
    : { text: '待添加账号', dot: 'bg-danger' }
  return (
    <span className="hidden shrink-0 items-center gap-1.5 rounded-full bg-surface-secondary px-2 py-1 text-xs text-muted sm:flex">
      <span className={`size-1.5 rounded-full ${state.dot}`} />
      {state.text}
    </span>
  )
}

function ThemeSwitch() {
  const { resolvedTheme, setTheme } = useTheme('system')
  const isDark = resolvedTheme === 'dark'
  const label = isDark ? '切换到浅色' : '切换到深色'
  return (
    <Tooltip delay={0}>
      <Button isIconOnly aria-label={label} variant="ghost" onPress={() => setTheme(isDark ? 'light' : 'dark')}>
        {isDark ? <Sun /> : <Moon />}
      </Button>
      <Tooltip.Content><p>{label}</p></Tooltip.Content>
    </Tooltip>
  )
}

/** 从上游刷新全部账号余额，顺带同步昵称（改名后点一下即可，免重新登录；5 秒轮询只读内存，不打上游） */
function RefreshBalance({ compact }: { compact: boolean }) {
  const qc = useQueryClient()
  const [busy, setBusy] = useState(false)
  const run = async () => {
    setBusy(true)
    try {
      await post('balance_all')
      toast.success('余额与昵称已从上游刷新')
    } catch (e) {
      toastError(e, '刷新失败：')
    } finally {
      setBusy(false)
      void qc.invalidateQueries({ queryKey: qk.overview })
      void qc.invalidateQueries({ queryKey: qk.logs })
    }
  }
  return (
    <BusyButton variant="secondary" busy={busy} isIconOnly={compact} aria-label="刷新余额与昵称" onPress={() => void run()}>
      {!busy && <ArrowsRotateRight />}{!compact && '刷新余额'}
    </BusyButton>
  )
}
