// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

package panel

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// productPanel 一个凭证文件真的能落盘的号（身份写在凭证顶层，必须验到文件）。
func productPanel(t *testing.T) (*Panel, *auth.Auth) {
	t.Helper()
	a := &auth.Auth{
		UID: "u1", AccessToken: "at-u1", RefreshToken: "rt-u1", Nickname: "一号",
		FilePath: filepath.Join(t.TempDir(), "workbuddy_u1.json"),
	}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("初始落盘失败: %v", err)
	}
	p, _ := newPanelFor(t, Config{})
	p.cfg.Pool.Add(a)
	return p, a
}

// TestAccountProductAPI 改出站身份：写回凭证文件、清单里能看见、认不出的值归一成桌面端。
func TestAccountProductAPI(t *testing.T) {
	p, a := productPanel(t)

	rec := postJSON(t, p, "/panel/api/accounts/u1/product", `{"product":"vscode"}`)
	autoWantCode(t, rec, http.StatusOK)
	if got := autoDecode(t, rec)["product"]; got != auth.ProductVSCode {
		t.Fatalf("响应 product = %v, want vscode", got)
	}
	if a.ProductValue() != auth.ProductVSCode {
		t.Fatalf("内存值 = %q", a.ProductValue())
	}
	raw, err := os.ReadFile(a.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"product": "vscode"`) {
		t.Fatalf("凭证文件没写进 product: %s", raw)
	}
	// 账号清单里能看见（面板据此显示身份标记）。
	var found bool
	for _, st := range p.cfg.Pool.List() {
		if st.UID == "u1" && st.Product == auth.ProductVSCode {
			found = true
		}
	}
	if !found {
		t.Fatalf("清单里没有 vscode 身份: %+v", p.cfg.Pool.List())
	}

	// 回到桌面端：文件里的键被删掉（老凭证文件不引入无意义的键）。
	rec = postJSON(t, p, "/panel/api/accounts/u1/product", `{"product":""}`)
	autoWantCode(t, rec, http.StatusOK)
	if got := autoDecode(t, rec)["product"]; got != auth.ProductDesktop {
		t.Fatalf("空值应归一成 desktop: %v", got)
	}
	raw, _ = os.ReadFile(a.FilePath)
	if strings.Contains(string(raw), `"product"`) {
		t.Fatalf("桌面端不该在文件里留 product 键: %s", raw)
	}

	// 认不出的值当桌面端，不报错（面板/手改配置都可能写岔）。
	rec = postJSON(t, p, "/panel/api/accounts/u1/product", `{"product":"nonsense"}`)
	autoWantCode(t, rec, http.StatusOK)
	if got := autoDecode(t, rec)["product"]; got != auth.ProductDesktop {
		t.Fatalf("认不出的值应归一成 desktop: %v", got)
	}
}

// TestAccountProductAPIErrors 号不存在回 404，请求体坏了回 400，都不动文件。
func TestAccountProductAPIErrors(t *testing.T) {
	p, a := productPanel(t)
	autoWantCode(t, postJSON(t, p, "/panel/api/accounts/nope/product", `{"product":"vscode"}`), http.StatusNotFound)
	autoWantCode(t, postJSON(t, p, "/panel/api/accounts/u1/product", `{`), http.StatusBadRequest)
	if a.ProductValue() != auth.ProductDesktop {
		t.Fatalf("失败的请求不该改身份: %q", a.ProductValue())
	}
}
