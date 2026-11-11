// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

// identity.go 面板上改号的出站身份（POST /panel/api/accounts/{uid}/product）。
//
// 身份本身与它是怎么发出去的见 upstream/identity.go；这里只做「读当前值 + 写回凭证文件」。
// 三种形态的实际差异只有请求头，改完下一个请求就生效（不需要重启、不需要重登）。
package panel

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// accountProduct 改号的出站身份。请求体 {"product":"desktop|vscode|cli|auto"}。
//
//	auto 是「被 429 时自己换下一条配额线」（桌面 → VSCode → CLI，见 server/identity.go），
//	钉死其余三个值就只用那一套形态。响应回写进凭证文件的实际值（认不出的值会被归一成 desktop）。
func (p *Panel) accountProduct(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	var body struct {
		Product string `json:"product"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式不对："+err.Error())
		return
	}
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, errAccountNotFound.Error())
		return
	}
	product := auth.NormalizeProduct(body.Product)
	a.SetProduct(product)
	// 凭证文件写失败不回滚内存值：身份是「下次出站怎么发」的软配置，
	// 内存里已经是新值了，只是重启后会回到文件里的旧值——如实告诉运维。
	if err := a.SaveAtomic(); err != nil {
		log.Printf("panel: 号 uid=%s 的出站身份改成 %s，但保存凭证文件失败：%v", uid, product, err)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "product": product, "file_error": err.Error()})
		return
	}
	log.Printf("panel: 号 uid=%s 的出站身份改成 %s", uid, product)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "product": product})
}
