package upstream

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoSelfIdentifyingPrefixInOutbound 守卫：生产代码里禁止再出现项目缩写作为
// 出站标识的前缀字面量。
//
// 背景（issue #239 / PR #248）：wb2api- 与 wb2a- 都曾进入上游请求体/请求头
// （会话/请求 ID、prompt_cache_key），官方客户端同位置恒为 UUID v4 或 32 hex，
// 一条正则即可判别请求不是官方客户端发的。此测试锁死回归：任何人再写
// "wb2api-xxx" / "wb2a-xxx" 字面量都会在这里失败。
//
// 范围：仓库内 internal/** 的生产 .go（跳过 _test.go；测试用假值不算出站）。
// 只扫字面量（带引号），注释里的历史说明不误报。cmd/ 下的本地临时文件名
// （wb2api-login-state.json）也不在出站路径上，故不纳入。
func TestNoSelfIdentifyingPrefixInOutbound(t *testing.T) {
	root := filepath.Join("..", "..", "internal")
	badPrefixes := []string{`"wb2api-`, `"wb2a-`}
	skipDirs := map[string]bool{"testdata": true, "node_modules": true}

	var hits []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 单文件读取失败不阻断整体扫描
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for i, line := range strings.Split(string(b), "\n") {
			for _, p := range badPrefixes {
				if strings.Contains(line, p) {
					hits = append(hits, fmt.Sprintf("%s:%d: %s", path, i+1, strings.TrimSpace(line)))
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/: %v", err)
	}
	if len(hits) > 0 {
		t.Fatalf("出站代码出现项目自证前缀字面量（官方同位置恒为 UUID v4 / 32 hex）：\n%s",
			strings.Join(hits, "\n"))
	}
}
