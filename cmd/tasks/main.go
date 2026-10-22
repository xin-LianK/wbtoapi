// tasks 诊断工具：拉取账号真正的上游成长任务列表，标出哪些任务还没有自动化实现。
//
// 做这件事的原因：任务码由服务端下发，代码里看不到。官方加了新任务时，
// 面板上点「自动完成」只会得到 501「该任务需要客户端内交互」，看不出是哪个码。
// 这个工具直接把上游列表与 internal/panel 里的 autoActions 做差集。
//
// 用法:
//
//	go run ./cmd/tasks                # 默认读 ./auths
//	go run ./cmd/tasks <auth 目录>
//
// 不写任何东西，只读上游任务列表（与客户端打开任务页同口径）。
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/panel"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func main() {
	dir := "auths"
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	supported := map[string]bool{}
	for _, c := range panel.AutoTaskCodes() {
		supported[c] = true
	}
	fmt.Printf("已实现自动化的任务码：%d 个\n\n", len(supported))

	files, err := filepath.Glob(filepath.Join(dir, "workbuddy-*.json"))
	if err != nil || len(files) == 0 {
		fmt.Fprintf(os.Stderr, "没找到账号文件（%s/workbuddy-*.json）。先跑 ./login.sh 完成 OAuth 登录，\n登录后再跑本工具就能列出上游任务与缺口的差集。\n", dir)
		os.Exit(1)
	}
	sort.Strings(files)

	up := upstream.New()
	// 跨账号汇总：同一个新任务在一个账号上看到就够了
	missing := map[string]string{} // code -> 标题
	total := 0

	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			fmt.Printf("%s: 读取失败 %v\n", filepath.Base(f), err)
			continue
		}
		a, err := auth.Parse(raw)
		if err != nil {
			fmt.Printf("%s: 解析失败 %v\n", filepath.Base(f), err)
			continue
		}
		a.FilePath = f

		tasks, err := up.ListTasks(a)
		if err != nil {
			fmt.Printf("%s(%s): 拉取任务失败 %v\n", a.Nickname, a.UID, err)
			continue
		}
		// 合并小程序口径（mp 列表是默认口径的超集），失败静默——与面板 accountTasks 同口径
		if mpTasks, mpErr := up.ListTasksMP(a); mpErr == nil {
			seen := map[string]bool{}
			for _, t := range tasks {
				seen[t.TaskCode] = true
			}
			for _, t := range mpTasks {
				if !seen[t.TaskCode] {
					tasks = append(tasks, t)
				}
			}
		}

		fmt.Printf("== %s(%s)  %d 个任务\n", a.Nickname, shortUID(a.UID), len(tasks))
		fmt.Printf("   %-34s %-9s %-11s %-6s %s\n", "task_code", "进度", "状态", "自动化", "标题")
		for _, t := range tasks {
			total++
			mark := "✅"
			if !supported[t.TaskCode] {
				mark = "❌"
				if _, ok := missing[t.TaskCode]; !ok {
					missing[t.TaskCode] = t.Title
				}
			}
			fmt.Printf("   %-34s %-9s %-11s %-6s %s\n",
				t.TaskCode, fmt.Sprintf("%d/%d", t.Current, t.Target),
				taskState(t), mark, trunc(t.Title, 28))
		}
		fmt.Println()
	}

	if len(missing) == 0 {
		fmt.Printf("✅ 全部 %d 个任务都有自动化实现。\n", total)
		return
	}
	codes := make([]string, 0, len(missing))
	for c := range missing {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	fmt.Printf("❌ %d 个任务没有自动化实现：\n", len(codes))
	for _, c := range codes {
		fmt.Printf("   %-34s %s\n", c, missing[c])
	}
	fmt.Println("\n要加的话：在 internal/panel/autotask.go 的 autoActions 里补一条（TaskCode + run 函数），")
	fmt.Println("判据靠解包客户端或抓一次真实操作样本；跑完用本工具复查是否还有缺口。")
}

func taskState(t upstream.Task) string {
	switch {
	case t.Claimed:
		return "已领取"
	case t.Claimable:
		return "可领取"
	case t.Locked:
		return "未解锁"
	case t.AcceptStatus != "":
		return t.AcceptStatus
	case t.Status != "":
		return t.Status
	}
	return "-"
}

func shortUID(uid string) string {
	if len(uid) <= 8 {
		return uid
	}
	return uid[:8]
}

func trunc(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}
