// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

// cache_stats_test.go 钉住缓存命中率观测链路（issue #92）：SSE 末帧的缓存字段采集，以及按对话判断的
// 整段未命中提示（issue #2）。
package server

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"
)

func feedStatsLines(s *chatStatsReader, lines ...string) {
	for _, l := range lines {
		s.parseSSELine(l)
	}
}

// 流式末帧带 hit/miss：CacheTokens 原样返回；只带 hit 时 miss 按 prompt-hit 推导；命中写在
// prompt_tokens_details.cached_tokens 里也认（同非流式的别名口径）；命中为 0 也算有观测；
// 上游完全没回缓存维度时 ok=false（不参与统计）。
func TestStatsReaderCacheTokens(t *testing.T) {
	cases := []struct {
		name      string
		line      string
		hit, miss int64
		ok        bool
	}{
		{"显式命中与未命中", `data: {"usage":{"prompt_tokens":1000,"completion_tokens":5,"total_tokens":1005,"credit":0.1,"prompt_cache_hit_tokens":900,"prompt_cache_miss_tokens":100}}`, 900, 100, true},
		{"只有命中，未命中按 prompt 推导", `data: {"usage":{"prompt_tokens":8500,"prompt_cache_hit_tokens":8400}}`, 8400, 100, true},
		{"命中写在 prompt_tokens_details 里", `data: {"usage":{"prompt_tokens":8500,"prompt_tokens_details":{"cached_tokens":8000}}}`, 8000, 500, true},
		{"命中为 0、未命中缺省，按 prompt 全量算", `data: {"usage":{"prompt_tokens":288109,"prompt_cache_hit_tokens":0}}`, 0, 288109, true},
		{"命中为 0、未命中显式", `data: {"usage":{"prompt_tokens":5000,"prompt_cache_hit_tokens":0,"prompt_cache_miss_tokens":5000}}`, 0, 5000, true},
		{"命中为 0、未命中远小于 prompt：反推命中（上游 #170）", `data: {"usage":{"prompt_tokens":52703,"prompt_cache_hit_tokens":0,"prompt_cache_miss_tokens":3}}`, 52700, 3, true},
		{"没有缓存字段", `data: {"usage":{"prompt_tokens":100,"completion_tokens":5}}`, 0, 0, false},
		{"usage 为 null", `data: {"usage":null}`, 0, 0, false},
		{"usage 字段类型不对", `data: {"usage":{"prompt_tokens":"x","prompt_cache_hit_tokens":5}}`, 0, 0, false},
		{"不是 JSON 的帧", `data: {oops`, 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newChatStatsReaderSince(strings.NewReader(""), time.Now())
			feedStatsLines(s, tc.line)
			if hit, miss, ok := s.CacheTokens(); hit != tc.hit || miss != tc.miss || ok != tc.ok {
				t.Fatalf("hit=%d miss=%d ok=%v, want %d/%d/%v", hit, miss, ok, tc.hit, tc.miss, tc.ok)
			}
		})
	}
}

// issue #170：上游在接近全命中时可能回 hit=0 + miss=3（+ prompt 很大）——按
// prompt - miss 对称反推 hit，不再误判为整段未命中；hit 字段完全缺失但 miss
// 存在时同理。prompt <= miss 时保持 hit=0（真实的整段未命中，不反推）。
func TestStatsReaderCacheTokensDeriveHit(t *testing.T) {
	// hit=0 + miss=3 + prompt=52703 → hit 反推为 52700。
	s := newChatStatsReaderSince(strings.NewReader(""), time.Now())
	feedStatsLines(s,
		`data: {"usage":{"prompt_tokens":52703,"prompt_cache_hit_tokens":0,"prompt_cache_miss_tokens":3}}`,
	)
	if hit, miss, ok := s.CacheTokens(); !ok || hit != 52700 || miss != 3 {
		t.Fatalf("derived hit: hit=%d miss=%d ok=%v, want 52700/3/true", hit, miss, ok)
	}

	// hit 字段完全缺失、只有 miss → 同样反推。
	s2 := newChatStatsReaderSince(strings.NewReader(""), time.Now())
	feedStatsLines(s2,
		`data: {"usage":{"prompt_tokens":8500,"prompt_cache_miss_tokens":100}}`,
	)
	if hit, miss, ok := s2.CacheTokens(); !ok || hit != 8400 || miss != 100 {
		t.Fatalf("miss-only derive: hit=%d miss=%d ok=%v, want 8400/100/true", hit, miss, ok)
	}

	// prompt == miss → 真实整段未命中，hit 保持 0。
	s3 := newChatStatsReaderSince(strings.NewReader(""), time.Now())
	feedStatsLines(s3,
		`data: {"usage":{"prompt_tokens":8500,"prompt_cache_hit_tokens":0,"prompt_cache_miss_tokens":8500}}`,
	)
	if hit, miss, ok := s3.CacheTokens(); !ok || hit != 0 || miss != 8500 {
		t.Fatalf("full miss keep hit=0: hit=%d miss=%d ok=%v, want 0/8500/true", hit, miss, ok)
	}
}

// issue #170 非流式侧：cacheTokensFromUsage 与流式推导口径对称。
func TestCacheTokensFromUsage(t *testing.T) {
	// hit 显式为 0、miss=3、prompt 很大 → 反推 hit=prompt-miss，不误判。
	usage := map[string]any{"prompt_tokens": 52703.0, "prompt_cache_hit_tokens": 0.0, "prompt_cache_miss_tokens": 3.0}
	if hit, miss, ok := cacheTokensFromUsage(usage); !ok || hit != 52700 || miss != 3 {
		t.Fatalf("hit=%d miss=%d ok=%v, want 52700/3/true", hit, miss, ok)
	}
	// 正常命中：hit 为正，原样返回。
	usage2 := map[string]any{"prompt_tokens": 1000.0, "prompt_cache_hit_tokens": 900.0, "prompt_cache_miss_tokens": 100.0}
	if hit, miss, ok := cacheTokensFromUsage(usage2); !ok || hit != 900 || miss != 100 {
		t.Fatalf("hit=%d miss=%d ok=%v, want 900/100/true", hit, miss, ok)
	}
	// 真实整段未命中（miss == prompt）→ hit 保持 0。
	usage3 := map[string]any{"prompt_tokens": 8500.0, "prompt_cache_miss_tokens": 8500.0}
	if hit, miss, ok := cacheTokensFromUsage(usage3); !ok || hit != 0 || miss != 8500 {
		t.Fatalf("hit=%d miss=%d ok=%v, want 0/8500/true", hit, miss, ok)
	}
	// 无任何缓存维度 → ok=false。
	usage4 := map[string]any{"prompt_tokens": 100.0}
	if _, _, ok := cacheTokensFromUsage(usage4); ok {
		t.Fatal("无缓存字段应 ok=false")
	}
}

// 反推后 hit>0 的观测不应触发 WARN（钉住 issue #170 假阳性：上游只回 miss、
// hit 由 prompt-miss 反推出来时，命中过的前缀不能又被判成整段未命中）。
func TestCacheMissSignalDerivedHitNoWarn(t *testing.T) {
	buf, _ := captureCacheLog(t)
	obs := func(hit, miss int64) cacheObservation {
		return cacheObservation{
			key: "conv-1", model: "gpt-6-luna", uid: "u1", acct: "u1(甲)",
			convID: "c-1", msgs: 3, prompt: hit + miss, hit: hit, miss: miss,
		}
	}
	cacheMissWarn.note(obs(52697, 6006))
	cacheMissWarn.note(obs(52700, 3))
	if buf.Len() != 0 {
		t.Fatalf("反推后 hit>0 的观测不应记 WARN，实际输出: %s", buf.String())
	}
}

// captureCacheLog 截获未命中提示的日志，并把信号和时钟换成测试自己的。
func captureCacheLog(t *testing.T) (*bytes.Buffer, *time.Time) {
	t.Helper()
	var buf bytes.Buffer
	oldOut, oldSig, oldNow, oldEvery := log.Writer(), cacheMissWarn, cacheMissNow, cacheMissWarnEvery
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	log.SetOutput(&buf)
	cacheMissWarn = newCacheMissSignal()
	cacheMissNow = func() time.Time { return now }
	t.Cleanup(func() {
		log.SetOutput(oldOut)
		cacheMissWarn, cacheMissNow, cacheMissWarnEvery = oldSig, oldNow, oldEvery
	})
	return &buf, &now
}

// TestCacheMissSignal 按对话判断：这段对话命中过、这一轮大前缀整段未命中才提示；换了模型、换了号、
// 会话 id 变了、消息变少是预期内的，记 INFO 并写明原因；都没变才记 WARN。新对话、从没命中过的对话、
// 小前缀、没有会话键都不提示；每段对话冷却期内只记一行。
func TestCacheMissSignal(t *testing.T) {
	buf, now := captureCacheLog(t)
	base := cacheObservation{key: "conv-1", model: "glm-5.3-flash", uid: "u1", acct: "u1(甲)", convID: "c-1", msgs: 3, prompt: 9000}
	obs := func(change func(o *cacheObservation), hit int64) cacheObservation {
		o := base
		o.hit = hit
		if change != nil {
			change(&o)
		}
		return o
	}
	step := func(d time.Duration) { *now = now.Add(d) }
	expect := func(t *testing.T, want string) {
		t.Helper()
		got := buf.String()
		buf.Reset()
		if want == "" {
			if got != "" {
				t.Fatalf("不应记日志，got %q", got)
			}
			return
		}
		if !strings.Contains(got, want) {
			t.Fatalf("日志应含 %q，got %q", want, got)
		}
	}

	cacheMissWarn.note(obs(nil, 0)) // 新对话的第一轮：不提示
	expect(t, "")
	cacheMissWarn.note(obs(func(o *cacheObservation) { o.msgs = 5 }, 0)) // 这段对话还没命中过：不提示
	expect(t, "")
	cacheMissWarn.note(obs(func(o *cacheObservation) { o.msgs = 7 }, 8000)) // 命中
	expect(t, "")

	step(20 * time.Minute)
	cacheMissWarn.note(obs(func(o *cacheObservation) { o.msgs = 9; o.miss = 9000 }, 0))
	expect(t, "WARN: [server] cache miss: model=glm-5.3-flash acct=u1(甲) 同一个号、同一个会话 id 这一轮大前缀仍未命中（prompt=9000 miss=9000，距上一轮 20 分钟）")
	cacheMissWarn.note(obs(func(o *cacheObservation) { o.msgs = 11 }, 0)) // 冷却期内不再记
	expect(t, "")

	cases := []struct {
		name   string
		change func(o *cacheObservation)
		want   string
	}{
		{"换了号", func(o *cacheObservation) { o.uid, o.acct = "u2", "u2(乙)" }, "acct=u2(乙) 这一轮大前缀未命中（prompt=9000 miss=9000）：换了号（上一轮在 u1(甲)），部分模型按号缓存，属预期"},
		{"会话 id 变了", func(o *cacheObservation) { o.convID = "c-2" }, "会话 id 变了"},
		{"消息变少", func(o *cacheObservation) { o.msgs = 2 }, "消息比上一轮少（3 → 2 条），客户端压缩或改写了历史"},
		{"换了模型", func(o *cacheObservation) { o.model = "glm-5.3" }, "换了模型（上一轮是 glm-5.3-flash）"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k := "conv-reason-" + string(rune('a'+i))
			cacheMissWarn.note(obs(func(o *cacheObservation) { o.key = k }, 8000))
			cacheMissWarn.note(obs(func(o *cacheObservation) { o.key = k; tc.change(o) }, 0))
			expect(t, tc.want)
		})
	}

	// 不提示的：小前缀、没有会话键或模型名。
	cacheMissWarn.note(obs(func(o *cacheObservation) { o.key = "conv-small" }, 500))
	cacheMissWarn.note(obs(func(o *cacheObservation) { o.key = "conv-small"; o.prompt = 800 }, 0))
	cacheMissWarn.note(obs(func(o *cacheObservation) { o.key = "" }, 0))
	cacheMissWarn.note(obs(func(o *cacheObservation) { o.model = "" }, 0))
	expect(t, "")

	// 冷却过了再记；间隔按小时写。
	step(3 * time.Hour)
	cacheMissWarn.note(obs(func(o *cacheObservation) { o.msgs = 13 }, 0))
	expect(t, "距上一轮 3.0 小时")
}

// 记的对话超过上限：先清一天没动的，再随便清掉一成。
func TestCacheMissSignalEvict(t *testing.T) {
	_, now := captureCacheLog(t)
	for i := 0; i < cacheMissTrackMax; i++ {
		cacheMissWarn.convs[string(rune(i))] = convCacheState{at: now.Add(-time.Duration(i%2) * 48 * time.Hour)}
	}
	cacheMissWarn.note(cacheObservation{key: "new", model: "m", prompt: 10})
	if n := len(cacheMissWarn.convs); n > cacheMissTrackMax/2+1 {
		t.Fatalf("一天没动的应清掉，剩 %d", n)
	}
	for i := len(cacheMissWarn.convs); i < cacheMissTrackMax; i++ {
		cacheMissWarn.convs["fill-"+string(rune(i))] = convCacheState{at: *now}
	}
	cacheMissWarn.note(cacheObservation{key: "new-2", model: "m", prompt: 10})
	if n := len(cacheMissWarn.convs); n > cacheMissTrackMax*9/10+1 {
		t.Fatalf("还超限时应清到九成，剩 %d", n)
	}
}

func TestGapText(t *testing.T) {
	for d, want := range map[time.Duration]string{
		30 * time.Second: "30 秒",
		5 * time.Minute:  "5 分钟",
		90 * time.Minute: "1.5 小时",
	} {
		if got := gapText(d); got != want {
			t.Errorf("gapText(%v) = %q, want %q", d, got, want)
		}
	}
}
