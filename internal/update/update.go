// Package update 版本更新检查：查 GitHub Releases 的最新 tag 与本地版本比对。
//
// 只做「提示」，不做自动升级。理由：自动升级要在运行期替换自己的二进制并重启，
// 中途失败会把服务留在半坏状态；而个人自用的升级动作本来就要人看一眼变更说明。
// 一条「有新版本」的提示已经把「三个月没更新、上游协议早变了」这个真问题解决了。
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Info 一次检查的结果快照（面板直接展示）。
type Info struct {
	Current     string    `json:"current"`
	Latest      string    `json:"latest"`
	HasUpdate   bool      `json:"has_update"`
	URL         string    `json:"url"`
	Name        string    `json:"name"`
	PublishedAt time.Time `json:"published_at"`
	CheckedAt   time.Time `json:"checked_at"`
	// Err 检查失败的原因（网络/限流/仓库不存在）。失败不清空上次成功的结果，
	// 只标记「这次没查到」——否则一次网络抖动会让面板显示成「已是最新」。
	Err string `json:"err"`
}

// Checker 带缓存的检查器。
type Checker struct {
	repo    string
	current string
	ttl     time.Duration
	client  *http.Client

	mu   sync.Mutex
	snap Info
}

// New 构造。repo 形如 "owner/name"；ttl 为缓存有效期。
func New(repo, current string, ttl time.Duration) *Checker {
	if ttl <= 0 {
		ttl = 12 * time.Hour
	}
	return &Checker{
		repo:    strings.TrimSpace(repo),
		current: current,
		ttl:     ttl,
		client:  &http.Client{Timeout: 10 * time.Second},
		snap:    Info{Current: current},
	}
}

// Snapshot 只读快照（不发网络请求）。
func (c *Checker) Snapshot() Info {
	if c == nil {
		return Info{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snap
}

// Fresh 快照是否仍在有效期内。
func (c *Checker) Fresh() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.snap.CheckedAt.IsZero() && time.Since(c.snap.CheckedAt) < c.ttl
}

// Start 后台定时检查：启动先查一次，之后按 ttl 周期刷新。
func (c *Checker) Start(ctx context.Context) {
	if c == nil || c.repo == "" {
		return
	}
	go func() {
		c.Refresh(ctx)
		t := time.NewTicker(c.ttl)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				c.Refresh(ctx)
			}
		}
	}()
}

// Refresh 立即查一次；失败时保留上次成功的版本信息，只记录错误。
func (c *Checker) Refresh(ctx context.Context) Info {
	if c == nil || c.repo == "" {
		return Info{}
	}
	latest, info, err := c.fetch(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		// 保留上次 latest/has_update（可能仍有效），只更新时间与错误。
		c.snap.Err = err.Error()
		c.snap.CheckedAt = time.Now()
		c.snap.Current = c.current
		return c.snap
	}
	c.snap = Info{
		Current:     c.current,
		Latest:      latest,
		HasUpdate:   newerThan(latest, c.current),
		URL:         info.htmlURL,
		Name:        info.name,
		PublishedAt: info.publishedAt,
		CheckedAt:   time.Now(),
	}
	return c.snap
}

type releaseInfo struct {
	htmlURL     string
	name        string
	publishedAt time.Time
}

type githubRelease struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	HTMLURL     string    `json:"html_url"`
	PublishedAt time.Time `json:"published_at"`
}

// apiErrText 从 GitHub 错误体里只取 message 字段并截断。
// 直接回显整个 body 会让面板的错误行变成一屏 JSON（含 documentation_url），
// 其中真正有用的只有一个短句。
func apiErrText(body []byte) string {
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &e) == nil && strings.TrimSpace(e.Message) != "" {
		return truncateRunes(e.Message, 120)
	}
	return truncateRunes(strings.TrimSpace(string(body)), 120)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func (c *Checker) fetch(ctx context.Context) (string, releaseInfo, error) {
	url := "https://api.github.com/repos/" + c.repo + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", releaseInfo{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	// GitHub API 强制要求 UA，缺失会被 403（且返回的错误体不解释原因）。
	req.Header.Set("User-Agent", "workbuddy2api-update-check")
	resp, err := c.client.Do(req)
	if err != nil {
		return "", releaseInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		// 仓库没有 Release 是常见状态（只打 tag 不发 Release），不是错误路径，
		// 但不该静默——面板要能看出「检查了，但对方没有发布页」。
		return "", releaseInfo{}, fmt.Errorf("仓库 %s 没有 Release", c.repo)
	}
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		// 匿名访问 GitHub API 每小时只有 60 次，个人自用场景下这条路径最常命中。
		// 原始 message 是一整段带 documentation_url 的 JSON，回显到面板只会占满一屏，
		// 所以这里直接换成一句能指导动作的话（其余错误再回显原句）。
		return "", releaseInfo{}, fmt.Errorf("GitHub API 限流（匿名每小时 60 次），稍后再试")
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return "", releaseInfo{}, fmt.Errorf("GitHub 返回 %d: %s", resp.StatusCode, apiErrText(body))
	}
	var rel githubRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return "", releaseInfo{}, err
	}
	tag := strings.TrimSpace(rel.TagName)
	if tag == "" {
		return "", releaseInfo{}, fmt.Errorf("Release 缺少 tag_name")
	}
	return tag, releaseInfo{htmlURL: rel.HTMLURL, name: rel.Name, publishedAt: rel.PublishedAt}, nil
}

// newerThan 判断 latest 是否比 current 新。只比较前三位数字，
// 忽略 "v" 前缀与 "-panel" 之类的后缀（本项目版本号形如 1.14.0-panel）。
func newerThan(latest, current string) bool {
	a, b := parseVersion(latest), parseVersion(current)
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

func parseVersion(v string) [3]int {
	v = strings.TrimSpace(strings.TrimPrefix(strings.ToLower(v), "v"))
	parts := strings.SplitN(v, ".", 4)
	var out [3]int
	for i := 0; i < 3 && i < len(parts); i++ {
		// 段内可能带后缀（1.14.0-panel 的第三段是 "0-panel"），取前导数字。
		digits := parts[i]
		end := 0
		for end < len(digits) && digits[end] >= '0' && digits[end] <= '9' {
			end++
		}
		if end > 0 {
			n, _ := strconv.Atoi(digits[:end])
			out[i] = n
		}
	}
	return out
}
