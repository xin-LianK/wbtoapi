// taskdiscover.go 成长任务的「自动发现」与「事件链族自动跑」。
//
// 解决的问题：任务码完全由服务端下发，本地没有预置清单。此前发现新任务的唯一
// 办法是人工跑 ./deploy.sh tasks 拉列表、和 internal/panel 的 autoActions 做差集，
// 而运行时 growthPending 又要求任务必须在白名单里——于是上游上了新任务，清扫一直在
// 跑、分却一分不涨，直到有人手动发现并补代码。
//
// 本文件做两件事：
//
//	1. 发现：周期拉两个口径（默认 + 小程序）的任务列表，把**上游有、本地没实现**的
//	   码记进 data/task-discover.json，首次出现打日志，面板可查。
//	2. 自动跑：命中 eventChainActions 族前缀的码（判据只是「事件到没到」、runner
//	   不读 TaskCode）直接在 autoActionFor 里回落到现有 runner，于是不用改代码就能
//	   进队列跑。需要业务参数的族（Model_chat_* / Expert_* / Sequential_*）**故意
//	   不收录**，只在面板里列出来等人补映射——漏跑只是少拿分，跑错是错误上报。
//
// 防无限重跑：族码连续 discoverStuckLimit 轮进度不动就判为「跑了也不动」，停止
// 自动跑并在面板说明原因。否则一个判据不符的码会被每轮扫描反复入队，日志刷屏。
package panel

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// discoverStuckLimit 连续多少轮进度无变化就停止自动跑该码。
// 按默认 60 分钟一轮即约 3 小时；给足累计型任务的累加窗口。
const discoverStuckLimit = 3

// discoverFileVersion 落盘格式版本（与 credit-history.json / usage.json 同风格）。
const discoverFileVersion = 1

// discoverAutoRun 事件链族是否允许自动执行（对应配置 schedule.task_discover_autorun）。
// 关闭时 autoActionFor 完全退回逐码白名单，行为与改动前一致。
var discoverAutoRun atomic.Bool

// DiscoveredTask 一个被观察到的上游任务码及其本地覆盖情况。
type DiscoveredTask struct {
	Code        string    `json:"code"`
	Title       string    `json:"title,omitempty"`
	Family      string    `json:"family,omitempty"`      // 命中的事件链族；空 = 需人工实现
	Implemented bool      `json:"implemented"`           // 精确白名单或已登记 mp 码
	Auto        bool      `json:"auto"`                  // 当前是否会被自动执行
	Source      string    `json:"source,omitempty"`      // default | miniprogram
	Credit      int64     `json:"credit,omitempty"`      // 奖励积分
	Target      int64     `json:"target,omitempty"`      // 目标进度
	Current     int64     `json:"current,omitempty"`     // 观察到的最好进度
	Stuck       int       `json:"stuck,omitempty"`       // 连续无进展轮数
	FirstSeen   time.Time `json:"first_seen"`
	LastSeen    time.Time `json:"last_seen"`
	Note        string    `json:"note,omitempty"`
}

// discoverDisk 落盘结构。
type discoverDisk struct {
	Version int               `json:"version"`
	Saved   string            `json:"saved,omitempty"`
	Tasks   []*DiscoveredTask `json:"tasks"`
}

// disc 进程内单例。path 为空表示未启用持久化（纯内存，面板仍可查）。
var disc = struct {
	sync.RWMutex
	path  string
	tasks map[string]*DiscoveredTask
}{tasks: map[string]*DiscoveredTask{}}

// InitTaskDiscover 初始化发现模块：设置落盘路径与自动跑开关，并载入历史。
func InitTaskDiscover(path string, autoRun bool) {
	discoverAutoRun.Store(autoRun)
	disc.Lock()
	defer disc.Unlock()
	disc.path = path
	loadDiscoverLocked()
}

// SetTaskDiscoverAutoRun 热改自动跑开关（面板保存配置后调用）。
func SetTaskDiscoverAutoRun(on bool) { discoverAutoRun.Store(on) }

// TaskDiscoverAutoRun 当前自动跑开关状态（供面板展示）。
func TaskDiscoverAutoRun() bool { return discoverAutoRun.Load() }

// loadDiscoverLocked 载入历史；文件缺失/损坏按空表处理（不能因为一个坏文件
// 让整个发现功能起不来，代价只是首轮把所有码当新码报一次）。
func loadDiscoverLocked() {
	if disc.path == "" {
		return
	}
	raw, err := os.ReadFile(disc.path)
	if err != nil {
		return
	}
	var d discoverDisk
	if err := json.Unmarshal(raw, &d); err != nil {
		log.Printf("panel: 任务发现记录损坏，按空表处理: %v", err)
		return
	}
	for _, t := range d.Tasks {
		if t == nil || strings.TrimSpace(t.Code) == "" {
			continue
		}
		disc.tasks[t.Code] = t
	}
}

// saveDiscoverLocked 原子落盘（临时文件 + rename，避免半截 JSON）。
func saveDiscoverLocked() {
	if disc.path == "" {
		return
	}
	out := discoverDisk{Version: discoverFileVersion, Saved: time.Now().Format(time.RFC3339)}
	for _, t := range disc.tasks {
		out.Tasks = append(out.Tasks, t)
	}
	sort.Slice(out.Tasks, func(i, j int) bool { return out.Tasks[i].Code < out.Tasks[j].Code })
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return
	}
	if dir := filepath.Dir(disc.path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := disc.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("panel: 任务发现记录写入失败: %v", err)
		return
	}
	if err := os.Rename(tmp, disc.path); err != nil {
		log.Printf("panel: 任务发现记录落盘失败: %v", err)
	}
}

// discoverExhausted 该码是否已被判为「跑了也不动」（连续多轮进度无变化）。
// autoActionFor 用它把这类码挡在队列外，避免每轮空跑。
func discoverExhausted(code string) bool {
	disc.RLock()
	defer disc.RUnlock()
	d := disc.tasks[strings.TrimSpace(code)]
	return d != nil && d.Stuck >= discoverStuckLimit
}

// exactActionFor 只看逐码白名单（不含族回落）。发现模块用它判断「是否已实现」，
// 不能直接用 autoActionFor——那样所有族码都会被当成本地已支持。
func exactActionFor(code string) *autoAction {
	code = strings.TrimSpace(code)
	for i := range autoActions {
		if autoActions[i].TaskCode == code {
			return &autoActions[i]
		}
	}
	return nil
}

// TaskDiscoverStats 一轮发现的汇总。
type TaskDiscoverStats struct {
	Accounts int `json:"accounts"` // 参与扫描的账号数
	Tasks    int `json:"tasks"`    // 观察到的任务码总数
	New      int `json:"new"`      // 本轮首次出现的码
	AutoRun  int `json:"auto_run"` // 族内可自动跑的码
	Manual   int `json:"manual"`   // 需人工实现（无族规则）的码
	Errors   int `json:"errors"`   // 拉取失败次数（不中断）
}

// RunTaskDiscoverOnce 对所有账号做一轮任务发现（只读列表，不 accept/不跑动作）。
// 不加 per-account 锁：列表是纯读接口，没必要和正在执行的动作互斥。
func (p *Panel) RunTaskDiscoverOnce() TaskDiscoverStats {
	var st TaskDiscoverStats

	// 账号间去重：同一个码只留一条，取观察到的最大进度（各账号进度不同）。
	seen := map[string]upstream.Task{}
	src := map[string]string{}
	var order []string

	for _, ps := range p.cfg.Pool.List() {
		if ps.Disabled {
			continue
		}
		a := p.cfg.Pool.AuthByUID(ps.UID)
		if a == nil || a.IsGlobal() || a.IsEnterprise() {
			continue // 无个人成长体系：零上游请求（与清扫/扫描同口径）
		}
		st.Accounts++

		tasks, err := p.cfg.Upstream.ListTasks(a)
		if err != nil {
			st.Errors++
			continue
		}
		merge := func(t upstream.Task, source string) {
			if t.TaskCode == "" {
				return
			}
			old, ok := seen[t.TaskCode]
			if !ok {
				order = append(order, t.TaskCode)
				src[t.TaskCode] = source
				seen[t.TaskCode] = t
				return
			}
			if t.Current > old.Current {
				old.Current = t.Current
			}
			if t.Target > 0 {
				old.Target = t.Target
			}
			if old.Title == "" {
				old.Title = t.Title
			}
			seen[t.TaskCode] = old
		}
		for _, t := range tasks {
			merge(t, "default")
		}
		// 小程序口径是默认口径的超集（含 mp 专属任务），失败静默：活动结束或
		// 账号无 mp 任务都是常态，不该算发现失败。
		if mpTasks, mErr := p.cfg.Upstream.ListTasksMP(a); mErr == nil {
			for _, t := range mpTasks {
				merge(t, "miniprogram")
			}
		}
	}

	now := time.Now()
	disc.Lock()
	for _, code := range order {
		t := seen[code]
		d := disc.tasks[code]
		isNew := d == nil
		if isNew {
			d = &DiscoveredTask{Code: code, FirstSeen: now}
			disc.tasks[code] = d
			st.New++
		}

		// 进度停滞计数：进度涨了就清零，没涨且不是首次见到就 +1。
		prev := d.Current
		if t.Current > prev {
			d.Current = t.Current
		}
		if t.Current > prev || isNew {
			d.Stuck = 0
		} else if d.Target == 0 || d.Current < d.Target {
			d.Stuck++
		}

		d.Title = firstNonEmptyStr(t.Title, d.Title)
		d.Family = familyOf(code)
		d.Implemented = exactActionFor(code) != nil || isMPTaskCode(code)
		d.Source = src[code]
		d.Credit = t.Credit
		if t.Target > 0 {
			d.Target = t.Target
		}
		d.LastSeen = now

		met := d.Target > 0 && d.Current >= d.Target
		switch {
		case d.Implemented:
			d.Auto, d.Note = false, ""
		case d.Family == "":
			d.Auto = false
			d.Note = "需人工实现：无事件链族规则，在 autoActions 补一行映射"
		case d.Stuck >= discoverStuckLimit:
			d.Auto = false
			d.Note = "已停止自动跑：连续多轮进度无变化，判据可能不是事件驱动"
		case met:
			d.Auto = false // 已达标：交给补领清扫，不必再跑行为
			d.Note = "已达标，等清扫领奖"
		default:
			d.Auto = true
			d.Note = "事件链族 " + d.Family + " 自动跑"
		}

		if isNew {
			log.Printf("panel: 发现新任务 code=%s title=%q 族=%q 自动跑=%v", code, d.Title, d.Family, d.Auto)
		}
		if d.Auto {
			st.AutoRun++
		} else if !d.Implemented {
			st.Manual++
		}
	}
	st.Tasks = len(order)
	saveDiscoverLocked()
	disc.Unlock()

	if st.New > 0 || st.Manual > 0 {
		log.Printf("panel: 任务发现：%d 个账号 · 观察 %d 个码 · 新增 %d · 族内自动跑 %d · 需人工或已停跑 %d",
			st.Accounts, st.Tasks, st.New, st.AutoRun, st.Manual)
	}
	return st
}

// DiscoveredTaskList 面板用：按「待处理优先」排序的快照。
// 排序：未实现且未自动跑（需人工）→ 自动跑中 → 已实现；同组内按最后观察时间倒序。
func DiscoveredTaskList() []DiscoveredTask {
	disc.RLock()
	defer disc.RUnlock()
	out := make([]DiscoveredTask, 0, len(disc.tasks))
	for _, t := range disc.tasks {
		out = append(out, *t)
	}
	rank := func(t DiscoveredTask) int {
		switch {
		case t.Implemented:
			return 2
		case t.Auto:
			return 1
		default:
			return 0
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if ri, rj := rank(out[i]), rank(out[j]); ri != rj {
			return ri < rj
		}
		return out[i].LastSeen.After(out[j].LastSeen)
	})
	return out
}

// discoveredTasks 面板接口：本进程观察到的全部上游任务码及其本地覆盖情况。
func (p *Panel) discoveredTasks(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"autorun": TaskDiscoverAutoRun(),
		"tasks":   DiscoveredTaskList(),
	})
}

// firstNonEmptyStr 取第一个非空串（上游字段时有时无，别让空值覆盖已有记录）。
func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
