package update

import "testing"

func TestNewerThan(t *testing.T) {
	cases := []struct {
		latest, current string
		want            bool
	}{
		{"v1.15.0", "1.14.0-panel", true},
		{"1.14.0", "1.14.0-panel", false},
		{"1.13.9", "1.14.0", false},
		{"v2.0.0", "1.99.99", true},
		{"1.14.1", "1.14.0", true},
		{"", "1.14.0", false},
	}
	for _, c := range cases {
		if got := newerThan(c.latest, c.current); got != c.want {
			t.Errorf("newerThan(%q,%q)=%v want %v", c.latest, c.current, got, c.want)
		}
	}
}

func TestSnapshotZeroValue(t *testing.T) {
	var nilChecker *Checker
	if (nilChecker.Snapshot() != Info{}) {
		t.Error("nil checker 快照应为零值")
	}
	if nilChecker.Fresh() {
		t.Error("nil checker 不应被视为新鲜")
	}
	c := New("owner/repo", "1.0.0", 0)
	if c.Snapshot().Current != "1.0.0" {
		t.Errorf("初始快照 current=%q", c.Snapshot().Current)
	}
	if c.Fresh() {
		t.Error("未检查过不应算新鲜")
	}
}
