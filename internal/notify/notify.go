// Package notify 出站告警通知（webhook）。
//
// 存在理由：网关此前**没有任何告警出口**——签到失败、账号 session dead 被禁用、
// 余额打穿、上游超时，全都只写进 stdout 和面板日志。无人值守时等于「事情发生了
// 但没人知道」，直到某天打开面板才发现已经空转一周。
//
// 设计取舍：
//   - **异步 + 有界队列**：通知绝不能阻塞签到/对话主链路。队列满了直接丢弃并
//     打日志——漏一条通知远好过卡住一次签到。
//   - **同类去重**：同一 (级别, 标题, 正文) 在 MinInterval 内只发一次。定时任务
//     每轮都会重跑，没有去重的话一个持续失败的账号会每 12 小时刷一条一样的消息。
//   - **只做发信，不做判定**：什么时候该报警由调用方决定（调度器知道哪些是真失败），
//     本包不猜语义。
package notify

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// 支持的推送通道。generic 是裸 JSON POST（自建接收端 / n8n / 自写机器人）。
const (
	FormatGeneric  = "generic"
	FormatWeCom    = "wecom"
	FormatBark     = "bark"
	FormatTelegram = "telegram"
)

// 告警级别（只用于措辞与图标，不改变投递行为）。
const (
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// Config 通知配置。
type Config struct {
	Enabled bool
	URL     string // 目标地址（bark 用 base url；telegram 用 bot token 走 Token 字段）
	Format  string // generic / wecom / bark / telegram
	// Token telegram bot token（其它通道忽略）。
	Token string
	// ChatID telegram 会话 id。
	ChatID string
	// MinInterval 同类消息去重窗口，<=0 回落 10 分钟。
	MinInterval time.Duration
	// Timeout 单次投递超时，<=0 回落 10s。
	Timeout time.Duration
}

// Message 一条告警。
type Message struct {
	Level string
	Title string
	Text  string
}

// Notifier 异步投递器。零值不可用，必须经 New 构造。
type Notifier struct {
	cfg    Config
	client *http.Client
	queue  chan Message
	once   sync.Once

	mu   sync.Mutex
	last map[string]time.Time // 去重键 → 上次发送时刻
}

// New 构造并启动投递 worker（enabled=false 时返回 nil，调用方无需判空即可 Send）。
func New(cfg Config) *Notifier {
	if !cfg.Enabled || strings.TrimSpace(cfg.URL) == "" && cfg.Format != FormatTelegram {
		return nil
	}
	if cfg.MinInterval <= 0 {
		cfg.MinInterval = 10 * time.Minute
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	n := &Notifier{
		cfg:    cfg,
		client: &http.Client{Timeout: cfg.Timeout},
		queue:  make(chan Message, 64),
		last:   map[string]time.Time{},
	}
	go n.loop()
	return n
}

// Enabled 是否可用（供面板展示）。
func (n *Notifier) Enabled() bool { return n != nil }

// Format 返回当前通道名（面板展示用）。
func (n *Notifier) Format() string {
	if n == nil {
		return ""
	}
	return n.cfg.Format
}

func (n *Notifier) loop() {
	for msg := range n.queue {
		if err := n.deliver(msg); err != nil {
			log.Printf("WARN: [notify] deliver failed: %v", err)
		}
	}
}

// Send 非阻塞投递。重复消息在去重窗口内直接丢弃。
func (n *Notifier) Send(level, title, text string) {
	if n == nil {
		return
	}
	if !n.allow(level, title, text) {
		return
	}
	select {
	case n.queue <- Message{Level: level, Title: title, Text: text}:
	default:
		log.Printf("WARN: [notify] queue full, dropped: %s", title)
	}
}

// SendSync 同步投递（面板「测试通知」用：需要拿到真实错误回报给用户）。
func (n *Notifier) SendSync(level, title, text string) error {
	if n == nil {
		return fmt.Errorf("通知未启用")
	}
	return n.deliver(Message{Level: level, Title: title, Text: text})
}

// allow 去重判定：窗口内同一条消息只放行一次。
func (n *Notifier) allow(level, title, text string) bool {
	sum := sha256.Sum256([]byte(level + "\x00" + title + "\x00" + text))
	key := hex.EncodeToString(sum[:8])
	now := time.Now()
	n.mu.Lock()
	defer n.mu.Unlock()
	if at, ok := n.last[key]; ok && now.Sub(at) < n.cfg.MinInterval {
		return false
	}
	n.last[key] = now
	// 惰性清理：条目数超过 512 时把过期键删掉，避免长期运行内存缓增。
	if len(n.last) > 512 {
		for k, at := range n.last {
			if now.Sub(at) >= n.cfg.MinInterval {
				delete(n.last, k)
			}
		}
	}
	return true
}

// deliver 按通道格式投递一次。
func (n *Notifier) deliver(msg Message) error {
	switch n.cfg.Format {
	case FormatWeCom:
		return n.postJSON(n.cfg.URL, map[string]any{
			"msgtype": "markdown",
			"markdown": map[string]any{
				"content": formatMarkdown(msg),
			},
		})
	case FormatBark:
		// Bark 的 push 接口：POST {base}/push，device_key 可由 base 路径携带。
		base := strings.TrimRight(n.cfg.URL, "/")
		endpoint := base
		if !strings.HasSuffix(base, "/push") {
			endpoint = base + "/push"
		}
		return n.postJSON(endpoint, map[string]any{
			"title": msg.Title,
			"body":  msg.Text,
			"group": "WorkBuddy",
			"level": barkLevel(msg.Level),
		})
	case FormatTelegram:
		if n.cfg.Token == "" || n.cfg.ChatID == "" {
			return fmt.Errorf("telegram 通道缺少 token 或 chat_id")
		}
		endpoint := "https://api.telegram.org/bot" + n.cfg.Token + "/sendMessage"
		return n.postJSON(endpoint, map[string]any{
			"chat_id": n.cfg.ChatID,
			"text":    msg.Title + "\n" + msg.Text,
			"disable_web_page_preview": true,
		})
	default: // generic
		return n.postJSON(n.cfg.URL, map[string]any{
			"title": msg.Title,
			"text":  msg.Text,
			"level": msg.Level,
			"time":  time.Now().Format(time.RFC3339),
			"from":  "workbuddy2api",
		})
	}
}

func (n *Notifier) postJSON(endpoint string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s 返回 %d: %s", hostOf(endpoint), resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func hostOf(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "endpoint"
	}
	return u.Host
}

func formatMarkdown(msg Message) string {
	icon := "💡"
	switch msg.Level {
	case LevelWarn:
		icon = "⚠️"
	case LevelError:
		icon = "⛔"
	}
	return fmt.Sprintf("%s **%s**\n%s", icon, msg.Title, msg.Text)
}

func barkLevel(level string) string {
	switch level {
	case LevelError:
		return "critical"
	case LevelWarn:
		return "timeSensitive"
	}
	return "active"
}
