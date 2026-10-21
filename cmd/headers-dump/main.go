// Command headers-dump 打印当前配置下 chat 请求实际会发出的请求头（dry-run，不连上游）。
//
// 用途：改完 upstream.client_version / profile / product 之后，不用等上线失败，
// 先在本地确认出站头与官方客户端是否一致。对比基准见 MAINTENANCE.md §5.2。
//
//	go run ./cmd/headers-dump
//	go run ./cmd/headers-dump -profile legacy            # 看回滚形态
//	go run ./cmd/headers-dump -client-version 5.8.4      # 试新版本号
//	go run ./cmd/headers-dump -enterprise ent-123        # 模拟企业号
package main

import (
	"flag"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func main() {
	profile := flag.String("profile", "", "出站头形态：desktop（默认）/ legacy")
	clientVersion := flag.String("client-version", "", "WorkBuddy 客户端版本（空 = 内置默认）")
	cliVersion := flag.String("cli-version", "", "UA 的 CLI 段版本（空 = 内置默认）")
	product := flag.String("product", "", "X-Product 取值（空 = SaaS）")
	enterprise := flag.String("enterprise", "", "企业号 ID（非空则模拟企业账号）")
	global := flag.Bool("global", false, "模拟国际版账号")
	flag.Parse()

	c := &upstream.Client{
		Profile:       *profile,
		ClientVersion: *clientVersion,
		CliVersion:    *cliVersion,
		Product:       *product,
	}
	a := &auth.Auth{
		UID:          "u_demo",
		AccessToken:  "demo-access-token",
		EnterpriseID: *enterprise,
	}
	if *global {
		a.Domain = "www.workbuddy.ai"
	}

	req, err := http.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/chat/completions", nil)
	if err != nil {
		fmt.Println("构造请求失败:", err)
		return
	}
	c.ChatHeaders(req, a, "", upstream.ChatMeta{})

	keys := make([]string, 0, len(req.Header))
	for k := range req.Header {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return strings.ToLower(keys[i]) < strings.ToLower(keys[j])
	})

	mode := *profile
	if mode == "" {
		mode = "desktop"
	}
	fmt.Printf("# 出站 chat 请求头（profile=%s）\n", mode)
	fmt.Printf("# POST https://copilot.tencent.com/v2/chat/completions\n\n")
	for _, k := range keys {
		fmt.Printf("%-26s %s\n", k+":", strings.Join(req.Header[k], ", "))
	}
	fmt.Printf("\n# 共 %d 个头。与官方客户端的对照表见 MAINTENANCE.md §5.2\n", len(keys))
}
