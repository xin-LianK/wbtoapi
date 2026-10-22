package panel

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// resetDiscover 清空进程级发现状态并恢复默认开关，避免测试互相污染。
func resetDiscover(t *testing.T) {
	t.Helper()
	disc.Lock()
	disc.path = ""
	disc.tasks = map[string]*DiscoveredTask{}
	disc.Unlock()
	discoverAutoRun.Store(true)
	t.Cleanup(func() {
		disc.Lock()
		disc.path = ""
		disc.tasks = map[string]*DiscoveredTask{}
		disc.Unlock()
		discoverAutoRun.Store(false)
	})
}

// TestFamilyOf 族前缀匹配：事件链族命中、参数化族必须不命中。
func TestFamilyOf(t *testing.T) {
	hit := map[string]string{
		"template_9":     "template",
		"Template_x":     "template",
		"playbook_new":   "playbook",
		"create_canvas2": "canvas",
		"Library_read2":  "library",
		"automation_3":   "automation",
		"Buddy_App_QQ":   "buddy_app",
		"RichMeow_Pic":   "richmeow",
	}
	for code, want := range hit {
		if got := familyOf(code); got != want {
			t.Errorf("familyOf(%q) = %q, want %q", code, got, want)
		}
	}
	// 这几族必须留空：runner 依赖业务参数，自动跑等于错误上报。
	for _, code := range []string{
		"Model_chat_Kimi", "Model_chat_GLM5.2",
		"Expert_lighthouse", "Expert_Valorant", "Expert_NewOne",
		"Sequential_Tasks_1", "Sequential_Tasks_9",
		"Hp_Appearance", "brand_new_task",
	} {
		if got := familyOf(code); got != "" {
			t.Errorf("familyOf(%q) = %q, want 空（参数化族不该自动跑）", code, got)
		}
	}
}

// TestAutoActionForFamilyFallback 族内新码在开启自动跑时回落到现有 runner，
// 且副本的 TaskCode 被填成实际码（日志/面板要按真实码展示）。
func TestAutoActionForFamilyFallback(t *testing.T) {
	resetDiscover(t)
	act := autoActionFor("template_9")
	if act == nil {
		t.Fatal("template_9 应命中 template 族，得到 nil")
	}
	if act.TaskCode != "template_9" {
		t.Errorf("TaskCode = %q, want template_9", act.TaskCode)
	}
	if act.Family != "template" {
		t.Errorf("Family = %q, want template", act.Family)
	}
	if act.run == nil {
		t.Error("族动作必须带 run，否则入队后无法执行")
	}
	// 精确白名单优先，不被族抢走。
	if a := autoActionFor("template_5"); a == nil || a.Family != "" {
		t.Errorf("template_5 应命中精确白名单条目，得到 %+v", a)
	}
	// 参数化族不回落。
	if a := autoActionFor("Model_chat_Kimi"); a != nil {
		t.Errorf("Model_chat_Kimi 不该有自动动作，得到 %+v", a)
	}
}

// TestAutoActionForFamilyRespectsSwitch 关闭自动跑后完全退回逐码白名单。
func TestAutoActionForFamilyRespectsSwitch(t *testing.T) {
	resetDiscover(t)
	discoverAutoRun.Store(false)
	if a := autoActionFor("template_9"); a != nil {
		t.Errorf("自动跑关闭时不该回落，得到 %+v", a)
	}
	if a := autoActionFor("chat_5"); a == nil {
		t.Error("关闭自动跑不应影响精确白名单")
	}
}

// TestAutoActionForFamilyExhausted 连续多轮无进展的码不再回落（防每轮空跑刷日志）。
func TestAutoActionForFamilyExhausted(t *testing.T) {
	resetDiscover(t)
	disc.Lock()
	disc.tasks["playbook_x"] = &DiscoveredTask{Code: "playbook_x", Stuck: discoverStuckLimit}
	disc.Unlock()
	if a := autoActionFor("playbook_x"); a != nil {
		t.Errorf("已判定停滞的码不该再自动跑，得到 %+v", a)
	}
	// 未达上限仍可跑。
	disc.Lock()
	disc.tasks["playbook_y"] = &DiscoveredTask{Code: "playbook_y", Stuck: discoverStuckLimit - 1}
	disc.Unlock()
	if a := autoActionFor("playbook_y"); a == nil {
		t.Error("停滞轮数未达上限时应继续自动跑")
	}
}

// TestFamilyActionActuallyReports 族回落拿到的动作能被真正执行：调用后假上游必须
// 收到上报请求。只断言 run != nil 是不够的——要证明「新码真的上报了事件」。
func TestFamilyActionActuallyReports(t *testing.T) {
	resetDiscover(t)
	b := &sweepBackend{}
	a := &auth.Auth{UID: "u1", AccessToken: "at"}
	p := newSweepPanel(t, b, a)

	act := autoActionFor("template_9")
	if act == nil {
		t.Fatal("template_9 应命中族")
	}
	if _, err := act.run(p, a); err != nil {
		t.Fatalf("族动作执行失败: %v", err)
	}
	if b.reqs.Load() == 0 {
		t.Error("族动作执行后上游应收到事件上报请求，实际 0 个")
	}
}

// TestGrowthPendingAcceptsFamilyCode 端到端：族内新码能进待办（从而被队列 accept+执行），
// 无族规则的码仍然被挡在待办外（不改变原有保守行为）。
func TestGrowthPendingAcceptsFamilyCode(t *testing.T) {
	resetDiscover(t)
	if !growthPending(upstream.Task{TaskCode: "template_9", Target: 5}) {
		t.Error("族内新码应进待办")
	}
	if !growthPending(upstream.Task{TaskCode: "playbook_fresh", Target: 1}) {
		t.Error("族内新码应进待办")
	}
	if growthPending(upstream.Task{TaskCode: "brand_new_task", Target: 1}) {
		t.Error("无族规则的新码不该进待办")
	}
	if growthPending(upstream.Task{TaskCode: "Model_chat_Kimi", Target: 1}) {
		t.Error("参数化族不该进待办（model 参数错会白跑）")
	}
	// locked 的族码也不进待办（上游未解锁，跑了只会 accept 失败）。
	if growthPending(upstream.Task{TaskCode: "template_9", Target: 5, Locked: true}) {
		t.Error("locked 任务不该进待办")
	}
}

// TestDiscoverFollowsDailyReset 上游重置进度（每日任务）时：current 必须跟随上游回落，
// 停滞计数不能把「重置」当成「无进展」，且要重新恢复自动跑。
// 卡住历史最大值的写法会让 met 恒为 true，该码从此再也不跑。
func TestDiscoverFollowsDailyReset(t *testing.T) {
	resetDiscover(t)
	cur := 5
	b := &sweepBackend{listFn: func(bool) []map[string]any {
		return []map[string]any{sweepTask("template_9", 5, int64(cur), "accepted", nil)}
	}}
	p := newSweepPanel(t, b, &auth.Auth{UID: "u1", AccessToken: "at"})

	p.RunTaskDiscoverOnce() // 5/5 达标
	disc.RLock()
	d := disc.tasks["template_9"]
	disc.RUnlock()
	if d.Auto {
		t.Error("达标未领时不该自动跑（交给清扫）")
	}

	cur = 0 // 上游重置（次日）
	p.RunTaskDiscoverOnce()
	disc.RLock()
	d = disc.tasks["template_9"]
	disc.RUnlock()
	if d.Current != 0 {
		t.Errorf("current = %d, want 0（必须跟随上游重置）", d.Current)
	}
	if d.Stuck != 0 {
		t.Errorf("stuck = %d, want 0（重置不等于无进展）", d.Stuck)
	}
	if !d.Auto {
		t.Errorf("重置后应恢复自动跑，note=%q", d.Note)
	}
}

// TestSweepFeedsDiscover 清扫顺带完成发现：一轮清扫后 discover 表里应出现该账号的
// 任务码，且**不产生任何额外列表请求**（发现复用的是清扫已经拉到的数据）。
func TestSweepFeedsDiscover(t *testing.T) {
	resetDiscover(t)
	b := &sweepBackend{listFn: func(bool) []map[string]any {
		return []map[string]any{sweepTask("template_9", 5, 0, "accepted", nil)}
	}}
	p := newSweepPanel(t, b, &auth.Auth{UID: "u1", AccessToken: "at"})

	before := b.reqs.Load()
	p.RunTaskSweepOnce()

	disc.RLock()
	d := disc.tasks["template_9"]
	disc.RUnlock()
	if d == nil {
		t.Fatal("清扫应顺带把拉到的列表喂给发现")
	}
	if !d.Auto || d.Family != "template" {
		t.Errorf("template_9 = %+v，应为族 template 自动跑", d)
	}
	// 清扫本身固定 2 次列表（默认 + mp）；发现若自拉就必然多出来。
	if got := b.reqs.Load() - before; got != 2 {
		t.Errorf("requests = %d, want 2（发现不该产生额外列表请求）", got)
	}
}

// TestRunTaskDiscoverOnce 发现流程：新码记录、已实现/族自动跑/待人工实现三分类。
func TestRunTaskDiscoverOnce(t *testing.T) {
	resetDiscover(t)
	b := &sweepBackend{
		listFn: func(isMP bool) []map[string]any {
			if isMP {
				return []map[string]any{
					sweepTask("Sequential_Tasks_1", 1, 0, "accepted", nil), // 已登记 mp 码 → 已实现
					sweepTask("mp_only_new", 1, 0, "accepted", nil),       // mp 独有且无族 → 待人工
				}
			}
			return []map[string]any{
				sweepTask("chat_5", 5, 0, "accepted", nil),   // 精确白名单 → 已实现
				sweepTask("template_9", 5, 0, "accepted", nil), // 族 template → 自动跑
				sweepTask("brand_new_task", 1, 0, "accepted", map[string]any{"reward_credit": 50}),
			}
		},
	}
	p := newSweepPanel(t, b, &auth.Auth{UID: "u1", AccessToken: "at"})
	st := p.RunTaskDiscoverOnce()

	if st.Accounts != 1 {
		t.Errorf("accounts = %d, want 1", st.Accounts)
	}
	if st.Tasks != 5 {
		t.Errorf("tasks = %d, want 5（chat_5/template_9/brand_new_task/mp_only_new/Sequential_Tasks_1）", st.Tasks)
	}
	if st.New != 5 {
		t.Errorf("new = %d, want 5（首轮全部是新码）", st.New)
	}
	if st.AutoRun != 1 {
		t.Errorf("auto_run = %d, want 1（只有 template_9 命中事件链族）", st.AutoRun)
	}
	if st.Manual != 2 {
		t.Errorf("manual = %d, want 2（brand_new_task / mp_only_new 无族规则）", st.Manual)
	}
	if st.Errors != 0 {
		t.Errorf("errors = %d, want 0", st.Errors)
	}

	byCode := map[string]DiscoveredTask{}
	for _, d := range DiscoveredTaskList() {
		byCode[d.Code] = d
	}
	if d := byCode["chat_5"]; !d.Implemented || d.Auto {
		t.Errorf("chat_5 = %+v，应为已实现且不自动跑", d)
	}
	if d := byCode["Sequential_Tasks_1"]; !d.Implemented {
		t.Errorf("Sequential_Tasks_1 = %+v，mp 已登记应判为已实现", d)
	}
	if d := byCode["template_9"]; d.Implemented || !d.Auto || d.Family != "template" {
		t.Errorf("template_9 = %+v，应为族 template 自动跑", d)
	}
	if d := byCode["brand_new_task"]; d.Auto || d.Note == "" {
		t.Errorf("brand_new_task = %+v，应待人工实现且给出说明", d)
	}
	if d := byCode["brand_new_task"]; d.Credit != 50 {
		t.Errorf("credit = %d, want 50（奖励字段应被记录）", d.Credit)
	}
}

// TestDiscoverStuckStopsAutoRun 进度连续多轮不动 → 停止自动跑并给出原因。
func TestDiscoverStuckStopsAutoRun(t *testing.T) {
	resetDiscover(t)
	listFn := func(bool) []map[string]any {
		return []map[string]any{sweepTask("template_9", 5, 0, "accepted", nil)}
	}
	b := &sweepBackend{listFn: listFn}
	p := newSweepPanel(t, b, &auth.Auth{UID: "u1", AccessToken: "at"})

	// 首轮只建立基线（isNew 不计停滞），因此要再跑 discoverStuckLimit 轮才达上限。
	for i := 0; i < discoverStuckLimit+1; i++ {
		p.RunTaskDiscoverOnce()
	}
	disc.RLock()
	d := disc.tasks["template_9"]
	disc.RUnlock()
	if d == nil {
		t.Fatal("template_9 应已记录")
	}
	if d.Stuck < discoverStuckLimit {
		t.Fatalf("stuck = %d, want >= %d（%d 轮无进展）", d.Stuck, discoverStuckLimit, discoverStuckLimit)
	}
	if d.Auto {
		t.Error("停滞达到上限后应停止自动跑")
	}
	if autoActionFor("template_9") != nil {
		t.Error("停滞的码不该再被 autoActionFor 回落")
	}
}

// TestDiscoverProgressResetsStuck 进度一旦上涨就清零停滞计数，恢复自动跑。
func TestDiscoverProgressResetsStuck(t *testing.T) {
	resetDiscover(t)
	cur := 0
	b := &sweepBackend{listFn: func(bool) []map[string]any {
		return []map[string]any{sweepTask("template_9", 5, int64(cur), "accepted", nil)}
	}}
	p := newSweepPanel(t, b, &auth.Auth{UID: "u1", AccessToken: "at"})

	for i := 0; i < discoverStuckLimit+1; i++ {
		p.RunTaskDiscoverOnce()
	}
	disc.RLock()
	stuckBefore := disc.tasks["template_9"].Stuck
	disc.RUnlock()
	if stuckBefore < discoverStuckLimit {
		t.Fatalf("stuck = %d, want >= %d", stuckBefore, discoverStuckLimit)
	}

	cur = 3 // 进度涨了
	p.RunTaskDiscoverOnce()
	disc.RLock()
	d := disc.tasks["template_9"]
	disc.RUnlock()
	if d.Stuck != 0 {
		t.Errorf("stuck = %d, want 0（进度上涨应清零）", d.Stuck)
	}
	if !d.Auto {
		t.Error("进度回升后应恢复自动跑")
	}
}
