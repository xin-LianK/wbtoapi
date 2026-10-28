// Package backup 本地备份：把凭证、状态与配置打成一个 tar.gz。
//
// 为什么不做「PostgreSQL 异地镜像」：个人自用场景真正会丢数据的方式只有三种——
// 误删 data 目录、换机器没带凭证、磁盘挂了。前两种一份带保留策略的本地压缩包
// 就能覆盖，第三种本地镜像也救不了（得靠异地，那是另一套运维）。引入数据库只会
// 多一个会挂、会需要迁移、会因为密码写错而静默失败的下游组件。
//
// 内容边界（有意为之）：只收 auths/*.json、data/*.json 与 config.json。
// data/request-logs 下的 JSONL 归档不进包——它按天增长、体积远大于其余全部数据，
// 且是可再生观测数据；把它塞进来会让备份从「几 KB」变成「几百 MB」。
package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Source 一个待备份来源。
type Source struct {
	// Path 文件或目录。
	Path string
	// Exts 目录递归时只收这些扩展名（小写，含点）；空 = 全收。
	Exts []string
	// Optional 路径不存在时不报错（用于首次部署尚无 data 目录的情况）。
	Optional bool
}

// Config 备份器配置。
type Config struct {
	// Dir 输出目录。
	Dir string
	// Sources 待备份来源。
	Sources []Source
	// Keep 保留份数，<=0 回落 7。
	Keep int
}

// Info 一份备份的元信息。
type Info struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
}

// Manager 备份管理器（并发安全：面板手动触发与定时任务可能同时到）。
type Manager struct {
	cfg Config
	mu  sync.Mutex
}

// New 构造；Dir 为空时返回 nil（调用方按「未启用」处理）。
func New(cfg Config) *Manager {
	if strings.TrimSpace(cfg.Dir) == "" {
		return nil
	}
	if cfg.Keep <= 0 {
		cfg.Keep = 7
	}
	return &Manager{cfg: cfg}
}

// Dir 备份目录（面板展示用）。
func (m *Manager) Dir() string {
	if m == nil {
		return ""
	}
	return m.cfg.Dir
}

// Create 立即生成一份备份，并按 Keep 清理旧包。
func (m *Manager) Create() (Info, error) {
	if m == nil {
		return Info{}, errors.New("备份未启用")
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := os.MkdirAll(m.cfg.Dir, 0o700); err != nil {
		return Info{}, fmt.Errorf("创建备份目录: %w", err)
	}
	name := "backup-" + time.Now().Format("20060102-150405") + ".tar.gz"
	path := filepath.Join(m.cfg.Dir, name)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return Info{}, fmt.Errorf("创建备份文件: %w", err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	written := 0
	var addErr error
	for _, src := range m.cfg.Sources {
		n, err := addSource(tw, src)
		if err != nil {
			if src.Optional && os.IsNotExist(err) {
				continue
			}
			addErr = err
			break
		}
		written += n
	}
	if addErr == nil && written == 0 {
		addErr = errors.New("没有可备份的文件（auths/ 与 data/ 都是空的）")
	}
	if err := tw.Close(); err != nil && addErr == nil {
		addErr = err
	}
	if err := gz.Close(); err != nil && addErr == nil {
		addErr = err
	}
	if err := f.Close(); err != nil && addErr == nil {
		addErr = err
	}
	if addErr != nil {
		// 半成品包比没有包更危险（看起来有备份，实际打不开），务必删掉。
		_ = os.Remove(path)
		return Info{}, addErr
	}

	st, err := os.Stat(path)
	if err != nil {
		return Info{}, err
	}
	m.prune()
	return Info{Name: name, Size: st.Size(), ModTime: st.ModTime()}, nil
}

// prune 按文件名排序保留最新 Keep 份（文件名含时间戳，字典序即时间序）。
func (m *Manager) prune() {
	items := m.list()
	if len(items) <= m.cfg.Keep {
		return
	}
	for _, it := range items[m.cfg.Keep:] {
		if err := os.Remove(filepath.Join(m.cfg.Dir, it.Name)); err != nil {
			log.Printf("WARN: [backup] prune %s: %v", it.Name, err)
			continue
		}
		log.Printf("backup pruned: %s", it.Name)
	}
}

// List 返回已有备份（新→旧）。
func (m *Manager) List() []Info { return m.list() }

func (m *Manager) list() []Info {
	if m == nil {
		return nil
	}
	entries, err := os.ReadDir(m.cfg.Dir)
	if err != nil {
		return nil
	}
	out := make([]Info, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "backup-") || !strings.HasSuffix(e.Name(), ".tar.gz") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, Info{Name: e.Name(), Size: info.Size(), ModTime: info.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out
}

// Start 定时备份循环；interval <= 0 或未启用时立即返回（不阻塞）。
func (m *Manager) Start(ctx context.Context, interval time.Duration) {
	if m == nil || interval <= 0 {
		return
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				info, err := m.Create()
				if err != nil {
					log.Printf("WARN: [backup] auto backup failed: %v", err)
					continue
				}
				log.Printf("backup created: %s (%.1f KiB)", info.Name, float64(info.Size)/1024)
			}
		}
	}()
}

// addSource 把一个来源写进 tar，返回写入的文件数。
func addSource(tw *tar.Writer, src Source) (int, error) {
	st, err := os.Stat(src.Path)
	if err != nil {
		return 0, err
	}
	base := filepath.Base(src.Path)
	if !st.IsDir() {
		return 1, addFile(tw, src.Path, base)
	}
	n := 0
	err = filepath.WalkDir(src.Path, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if !matchExt(path, src.Exts) {
			return nil
		}
		rel, relErr := filepath.Rel(src.Path, path)
		if relErr != nil {
			return relErr
		}
		// tar 内统一用正斜杠，跨平台解包才不会多出反斜杠文件名。
		name := base + "/" + filepath.ToSlash(rel)
		if err := addFile(tw, path, name); err != nil {
			return err
		}
		n++
		return nil
	})
	return n, err
}

func matchExt(path string, exts []string) bool {
	if len(exts) == 0 {
		return true
	}
	lower := strings.ToLower(filepath.Ext(path))
	for _, e := range exts {
		if lower == strings.ToLower(e) {
			return true
		}
	}
	return false
}

func addFile(tw *tar.Writer, path, name string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	hdr, err := tar.FileInfoHeader(st, "")
	if err != nil {
		return err
	}
	hdr.Name = name
	// 权限收紧：包内是明文 accessToken / refreshToken。
	hdr.Mode = 0o600
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(tw, f)
	return err
}
