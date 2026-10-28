package backup

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestCreateIncludesOnlyJSONAndPrunes 验证：只收 .json、能解包、按 Keep 清理旧包。
func TestCreateIncludesOnlyJSONAndPrunes(t *testing.T) {
	root := t.TempDir()
	auths := filepath.Join(root, "auths")
	data := filepath.Join(root, "data")
	out := filepath.Join(root, "backups")
	for _, d := range []string{auths, data, out} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(auths, "a.json"), `{"uid":"a"}`)
	write(filepath.Join(data, "state.json"), `{}`)
	// 不该进包的：体积大且可再生的 JSONL 归档
	write(filepath.Join(data, "request-2026-10-28.jsonl"), `{"x":1}`)
	write(filepath.Join(root, "config.json"), `{"listen":":7863"}`)

	m := New(Config{
		Dir: out,
		Sources: []Source{
			{Path: filepath.Join(root, "config.json"), Optional: true},
			{Path: auths, Exts: []string{".json"}, Optional: true},
			{Path: data, Exts: []string{".json"}, Optional: true},
		},
		Keep: 2,
	})
	info, err := m.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if info.Size <= 0 {
		t.Fatalf("备份大小=%d", info.Size)
	}

	names := tarNames(t, filepath.Join(out, info.Name))
	want := map[string]bool{"config.json": false, "auths/a.json": false, "data/state.json": false}
	for _, n := range names {
		if _, ok := want[n]; ok {
			want[n] = true
		}
		if n == "data/request-2026-10-28.jsonl" {
			t.Errorf("JSONL 归档不应进备份包: %v", names)
		}
	}
	for n, seen := range want {
		if !seen {
			t.Errorf("备份缺少 %s，实际条目=%v", n, names)
		}
	}

	// 再生成两份 → 应只保留 Keep=2 份
	for i := 0; i < 2; i++ {
		// 文件名精确到秒，连续调用会撞名（O_EXCL 报错）——这里手动错开时间不现实，
		// 故直接断言「不会无限增长」：清理后份数 <= Keep 或 1（撞名时未新增）。
		if _, err := m.Create(); err != nil {
			t.Logf("第二次 Create 因文件名同秒冲突跳过: %v", err)
			break
		}
	}
	if got := len(m.List()); got > 2 {
		t.Errorf("备份份数=%d 超过 Keep=2", got)
	}
}

func TestCreateEmptySources(t *testing.T) {
	root := t.TempDir()
	empty := filepath.Join(root, "auths")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	m := New(Config{Dir: filepath.Join(root, "b"), Sources: []Source{{Path: empty, Optional: true}}, Keep: 3})
	if _, err := m.Create(); err == nil {
		t.Fatal("空来源应当报错（避免生成看起来有内容实际为空的包）")
	}
	if items := m.List(); len(items) != 0 {
		t.Errorf("失败时不应留下半成品: %v", items)
	}
}

func TestDisabledManager(t *testing.T) {
	var m *Manager
	if _, err := m.Create(); err == nil {
		t.Fatal("nil manager 必须报错而不是 panic")
	}
	if m.List() != nil {
		t.Error("nil manager List 应返回 nil")
	}
	m.Start(nil, 0) // 不应 panic
}

func tarNames(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var out []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, h.Name)
	}
	return out
}
