// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

// 许可与出处声明：「版本信息」窗口（AGPL-3.0 第 5 条 d 款的适当法律声明）和浏览器控制台共用。
// 条款原文在仓库的 LICENSE、NOTICE。
//
// 两套地址分开：面板里凡是「去看代码 / 看更新日志 / 看发版」的链接都用 PROJECT_URL，
// 不要指到上游项目去；上游地址只出现在必须保留的署名声明里（AGPL-3.0 第 7 条附加条款）。

/** 本项目仓库 */
export const PROJECT_URL = 'https://github.com/xin-LianK/wbtoapi'
/** 仓库里的文档目录（更新日志、升级方法等按它补全相对链接） */
export const DOCS_URL = PROJECT_URL + '/blob/main/docs/'
export const LICENSE_URL = PROJECT_URL + '/blob/main/LICENSE'
export const NOTICE_URL = PROJECT_URL + '/blob/main/NOTICE'

/** 署名对象：AGPL-3.0 附加条款要求写明的出处，只用于声明，不用于功能链接 */
export const UPSTREAM_URL = 'https://github.com/yreidev/workbuddy2api-panel'
export const UPSTREAMS = [
  { name: 'Sliverkiss/workbuddy2api', url: 'https://github.com/Sliverkiss' },
  { name: 'linguo2625469/workbuddy2api-panel', url: 'https://github.com/linguo2625469/workbuddy2api-panel' },
  { name: 'yreidev/workbuddy2api-panel', url: UPSTREAM_URL },
]

/** 控制台里打印的几行：项目、许可、署名要求、上游、无担保 */
export function consoleNotice(): string[] {
  return [
    'WorkBuddy2API · ' + PROJECT_URL,
    '以 AGPL-3.0-only 授权，附署名条款（见 ' + NOTICE_URL + '）。',
    '复制或改写本项目的代码须保留版权声明，在 README 和界面里注明出处并附上链接；修改版须标明修改，不得冒充原版。',
    '基于 workbuddy2api-panel（' + UPSTREAM_URL + '）、' + UPSTREAMS.slice(0, 2).map((u) => u.name).join(' 与 ') + ' 等上游项目的代码。本程序按原样提供，不提供任何担保。',
  ]
}

/** 页面加载时在浏览器控制台打一次声明 */
export function printConsoleNotice(log: (...args: unknown[]) => void = console.info) {
  const [title, ...rest] = consoleNotice()
  log('%c' + title + '\n%c' + rest.join('\n'), 'font-weight:600;font-size:13px', 'font-weight:400')
}
