// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

// 页面分区容器：同一页里的几块内容用页签切换，整页高度锁在视口内（.page-fill），
// 页签内容自己滚，不再靠各页写 max-h-[60dvh] 猜高度——号一多、卡片一多就把页面拖成几屏。
// 用法见 packages/PackagesPage.tsx：把原来平铺的 <Panel> 各包一个 { id, label, content } 即可。
import { useState, type ReactNode } from 'react'
import { Tabs } from '@heroui/react'

export interface PageTab {
  id: string
  label: ReactNode
  /** 标题后的小角标（条数、改动数），不给就不占位 */
  badge?: ReactNode
  /** 内容区自己铺满并内部滚动（表格、日志这类整块）；默认整块跟着页签滚 */
  fill?: boolean
  content: ReactNode
}

export function TabbedPage({ tabs, label = '页面分区', toolbar, value, onChange, className = '' }: {
  tabs: PageTab[]
  /** 页签组的无障碍名，读屏用 */
  label?: string
  /** 页签上方的一行共用控件（时间范围、筛选、刷新）——各页签共用同一份状态，别在每块里复制一遍 */
  toolbar?: ReactNode
  /** 受控选中页签；不传则组件自己记（默认第一个） */
  value?: string
  onChange?: (id: string) => void
  className?: string
}) {
  const [local, setLocal] = useState(tabs[0]?.id ?? '')
  const selected = value ?? local
  const pick = (id: string) => {
    setLocal(id)
    onChange?.(id)
  }
  return (
    <Tabs align="start" selectedKey={selected} onSelectionChange={(k) => pick(String(k))} className={`page-fill ${className}`}>
      <Tabs.ListContainer className="shrink-0 self-start max-w-full">
        <Tabs.List aria-label={label}>
          {tabs.map((t) => (
            <Tabs.Tab key={t.id} id={t.id} className="whitespace-nowrap">
              {t.label}
              {t.badge != null && <span className="ml-1.5 text-xs text-muted tabular-nums">{t.badge}</span>}
              <Tabs.Indicator />
            </Tabs.Tab>
          ))}
        </Tabs.List>
      </Tabs.ListContainer>
      {toolbar && <div className="flex shrink-0 flex-wrap items-center gap-2 pt-3">{toolbar}</div>}
      {tabs.map((t) => (
        <Tabs.Panel
          key={t.id}
          id={t.id}
          // fill：面板本身铺满、由内部表格/日志滚；否则整块内容在这里滚
          className={`min-h-0 flex-1 pt-3 outline-none ${t.fill ? 'flex flex-col' : 'overflow-y-auto'}`}
        >
          {t.content}
        </Tabs.Panel>
      ))}
    </Tabs>
  )
}
