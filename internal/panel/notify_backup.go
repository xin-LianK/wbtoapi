// notify_backup.go 面板侧的告警测试与备份操作接口。
//
// 都只做「触发 + 如实回报结果」，不做任何自动补救：备份失败时要让用户看到真实
// 原因（磁盘满、权限不足），而不是给一个「已自动重试」的假安心。
package panel

import (
	"encoding/json"
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/notify"
)

// notifyStatus 返回告警通道是否启用与当前格式（前端据此决定是否显示测试按钮）。
func (p *Panel) notifyStatus(w http.ResponseWriter, r *http.Request) {
	enabled := p.cfg.Notify != nil
	format := ""
	if enabled {
		format = p.cfg.Notify.Format()
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": enabled, "format": format})
}

// notifyTest 同步投递一条测试消息，把通道真实错误回给前端。
func (p *Panel) notifyTest(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Notify == nil {
		writeErr(w, http.StatusNotImplemented, "notify disabled")
		return
	}
	var in struct {
		Title string `json:"title"`
		Text  string `json:"text"`
		Level string `json:"level"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if in.Title == "" {
		in.Title = "WorkBuddy2API 测试通知"
	}
	if in.Text == "" {
		in.Text = "能看到这条消息，说明告警通道已经接通。"
	}
	level := in.Level
	if level == "" {
		level = notify.LevelInfo
	}
	if err := p.cfg.Notify.SendSync(level, in.Title, in.Text); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "format": p.cfg.Notify.Format()})
}

// backups 列出已有备份（未启用时返回空集，前端按「未启用」展示）。
func (p *Panel) backups(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Backup == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "dir": "", "items": []any{}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": true,
		"dir":     p.cfg.Backup.Dir(),
		"items":   p.cfg.Backup.List(),
	})
}

// backupCreate 立即生成一份备份。
func (p *Panel) backupCreate(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Backup == nil {
		writeErr(w, http.StatusNotImplemented, "backup disabled")
		return
	}
	info, err := p.cfg.Backup.Create()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "item": info})
}

// updateInfo 返回版本检查快照（只读，不发网络请求）。
func (p *Panel) updateInfo(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Updater == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "info": p.cfg.Updater.Snapshot()})
}

// updateCheck 立即查一次（页面上的「立即检查」按钮）。
func (p *Panel) updateCheck(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Updater == nil {
		writeErr(w, http.StatusNotImplemented, "update check disabled")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "info": p.cfg.Updater.Refresh(r.Context())})
}

// SetChatEndpoint 注入 chat 流水线入口（main 在 handler 构造完成后调用）。
//
// 之所以用 setter 而不是构造参数：panel 必须先于 server.Handler 构造
// （server.Config 要拿 panel 当 http.Handler 挂到 /panel/ 上），此时 chat 流水线
// 还不存在。Setter 是这里唯一不引入循环依赖的接法。
func (p *Panel) SetChatEndpoint(h http.Handler) { p.cfg.chatEndpoint = h }

// playground 聊天测试台：把面板请求原样转给真实 chat 流水线，SSE 直接透传。
func (p *Panel) playground(w http.ResponseWriter, r *http.Request) {
	if p.cfg.chatEndpoint == nil {
		writeErr(w, http.StatusNotImplemented, "chat playground disabled")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	req := r.Clone(r.Context())
	req.URL.Path = "/v1/chat/completions"
	p.cfg.chatEndpoint.ServeHTTP(w, req)
}
