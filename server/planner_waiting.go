package server

// planner 等待态（2026-11-08 起始，2026-11-20 加显式等待工具）。
//
// # 解决什么
//
// 目标依赖未来事件（「等下个交易日收盘再判断入场」「等操作员人工验收」）时，
// 继续按心跳唤醒 planner 只会每 10 分钟烧一轮 LLM，结论永远是「还在等」。
//
// 两级机制：
//
//	① 隐式冷却 —— 本轮零产出时自动降频 10 分钟（plannerWaitingCooldown）；
//	② 显式等待 —— planner 自己调 wait_for 声明「等到什么时候、为什么」，
//	   引擎据此停止一切常规唤醒，只在下面的条件之一满足时恢复：
//
//	   到期            专用定时器到点（心跳间隔与剩余时间取小）
//	   人工/图事件穿透  新增/修改/删除目标、新增 hint、删除意图 —— 这些是
//	                   外部输入，等待的从来不是它们，必须立刻重规划
//	   任务控制        resume / 改目标 / 验收通过（部分达成）走 Notify，同上
//	   pause/delete/超时 —— 已有的门，先于本逻辑生效
//
// # 为什么需要显式等待（2026-11-20，任务 39）
//
// 金融任务的目标只能人工验收（agent 侧 prove_goal 被硬拦）。planner 提交
// 6 个目标后进入「等操作员点验收」，但平台没有「待验收」这个状态，于是它
// 每 10 分钟醒一次、每轮输出「仍在等待」—— 89 轮规划、8000 万 token，
// 任务状态永远 running，看不到尽头。
//
// 有了 wait_for，这种等待是**声明式**的：一轮 LLM 说清「等谁、等到何时」，
// 之后引擎不再唤醒它；UI 显示「等待中：等待人工验收（至 11-20 12:00）」。
// 操作员验收不会触发 planner 继续空转 —— 全部达成时任务直接落 done。

import (
	"fmt"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// plannerWaitingCooldown 是「本轮零产出」后的降频间隔。
// 10 分钟：比 worker 一轮（分钟级）长一个量级，又不会让新数据等太久。
const plannerWaitingCooldown = 10 * time.Minute

// 显式等待的边界。下限 1 分钟（短于此说明该做的事还没做完，别等）；
// 上限 24 小时（超过一律按 24 小时截断 —— 忘了取消的等待不该让任务永远静默）。
const (
	plannerWaitMin = time.Minute
	plannerWaitMax = 24 * time.Hour
)

// waitState 是一个任务的等待状态。
type waitState struct {
	until    time.Time
	reason   string
	explicit bool // true = planner 调 wait_for 声明；false = 零产出后的隐式冷却
}

// Engine 的 waiting 字段见 engine.go 结构体。
func (e *Engine) beginWaiting(taskID string, d time.Duration) {
	e.setWait(taskID, waitState{until: time.Now().Add(d), explicit: false})
}

// BeginExplicitWait 是 planner 的 wait_for 工具落库的入口（engine 回调）。
//
// d 会被夹逼到 [plannerWaitMin, plannerWaitMax]，失败只在参数本身不可用时返回。
func (e *Engine) BeginExplicitWait(taskID string, reason string, d time.Duration) (time.Time, error) {
	if d < plannerWaitMin {
		d = plannerWaitMin
	}
	if d > plannerWaitMax {
		d = plannerWaitMax
	}
	st := waitState{until: time.Now().Add(d), reason: reason, explicit: true}
	e.setWait(taskID, st)
	return st.until, nil
}

// waitUntilUnix 把等待到点时间转 unix 秒（零值 → 0，不进窗口期）。
func waitUntilUnix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// beginPlannerWait 返回 planner 的 wait_for 回调（绑定到具体任务）。
//
// 包装而不是直接暴露 Engine.BeginExplicitWait：工具回调拿不到任务 id，
// 而等待必须落在发起它的那个任务上。
func (s *Server) beginPlannerWait(t *Task) func(reason string, minutes int) (string, error) {
	return func(reason string, minutes int) (string, error) {
		if t == nil {
			return "", fmt.Errorf("wait_for 需要任务上下文")
		}
		until, err := s.engine.BeginExplicitWait(t.ID, reason, time.Duration(minutes)*time.Minute)
		if err != nil {
			return "", err
		}
		s.engine.emitExplicitWaitActivity(t, reason, until)
		return until.Local().Format("2006-01-02 15:04"), nil
	}
}

func (e *Engine) setWait(taskID string, st waitState) {
	e.waitMu.Lock()
	defer e.waitMu.Unlock()
	if e.waitUntil == nil {
		e.waitUntil = map[string]waitState{}
	}
	e.waitUntil[taskID] = st
}

// inWaiting 返回是否仍在等待；到期时顺手清除标记。
func (e *Engine) inWaiting(taskID string) bool {
	e.waitMu.Lock()
	defer e.waitMu.Unlock()
	st, ok := e.waitUntil[taskID]
	if !ok {
		return false
	}
	if time.Now().Before(st.until) {
		return true
	}
	delete(e.waitUntil, taskID)
	return false
}

// WaitState 返回等待中任务的「到点时间 + 原因 + 是否显式」，供 stats/DTO 展示。
func (e *Engine) WaitState(taskID string) (until time.Time, reason string, explicit bool, ok bool) {
	e.waitMu.Lock()
	defer e.waitMu.Unlock()
	st, has := e.waitUntil[taskID]
	if !has || !time.Now().Before(st.until) {
		return time.Time{}, "", false, false
	}
	return st.until, st.reason, st.explicit, true
}

// waitRemaining 返回到点前的剩余时长（不在等待中返回 0）。
func (e *Engine) waitRemaining(taskID string) time.Duration {
	until, _, _, ok := e.WaitState(taskID)
	if !ok {
		return 0
	}
	return time.Until(until)
}

// isExplicitWait 报告当前等待是否由 planner 显式声明 ——
// 是的话零产出分支不得用 10 分钟冷却把它覆盖掉。
func (e *Engine) isExplicitWait(taskID string) bool {
	_, _, explicit, ok := e.WaitState(taskID)
	return ok && explicit
}

func (e *Engine) clearWaiting(taskID string) {
	e.waitMu.Lock()
	defer e.waitMu.Unlock()
	delete(e.waitUntil, taskID)
}

// intentCount 用于判断本轮规划有没有真正产出新意图。
// 返回 -1 表示读失败（调用方应跳过 waiting 判定，避免误判）。
func intentCount(ts *db.ExplorationStore) int {
	if ts == nil {
		return -1
	}
	nodes, err := ts.ListByKind(db.KindIntent, 10000)
	if err != nil {
		return -1
	}
	return len(nodes)
}

// waitGate 决定一次唤醒是否要继续跑规划轮。返回 (proceed, clearWait)。
//
// 抽成纯函数是为了可测：真实的 runRound 要起 engine/LLM，而这里的判据
// （等待态 + 触发事件类型）是纯逻辑，{等待种类 × 触发清单} 只有十几种组合，
// 全部值得穷举 —— 判错一次的表现就是「planner 睡过去」，靠线上观察发现太慢。
//
// # 规则（2026-11-20 任务 44 之后）
//
//	不在等待           → 继续
//	隐式冷却 + 任意触发 → 继续（清冷却）
//	显式等待 + 人的输入 → 继续（清等待）
//	显式等待 + 其它     → 吞掉（保留等待）
//	等待中 + 无触发     → 吞掉
//
// 「隐式冷却 + 无触发」也算吞掉：没有触发说明是周期心跳，确认一下还在等
// 就够了，没必要烧一轮 LLM。
func waitGate(inWait, explicit bool, evs []agent.TriggerEvent) (proceed, clearWait bool) {
	if !inWait {
		return true, false
	}
	if !explicit {
		// 隐式冷却：任何触发（尤其 worker done/finding）都值得立刻规划。
		if len(evs) > 0 {
			return true, true
		}
		return false, false
	}
	if urgentTriggers(evs) {
		return true, true
	}
	return false, false
}

// urgentTriggers 报告这批触发事件里有没有「必须穿透等待」的。
//
// 只有外部输入穿透：人等不及、改了目标、加了提示、删了意图。worker 自己
// 完成的 done/finding **不**穿透 —— 那正是等待期间不该处理的东西，
// 留到到点那一轮一次性看，否则等待等于没等。
func urgentTriggers(evs []agent.TriggerEvent) bool {
	for _, ev := range evs {
		switch ev.Kind {
		case "goal", "goal_edited", "goal_deleted", "goal_accepted", "hint", "cancelled":
			return true
		}
	}
	return false
}

func (e *Engine) emitWaitingActivity(t *Task) {
	e.emitActivity(t, db.Activity{
		Worker: "system",
		Kind:   "text",
		Summary: fmt.Sprintf("等待中：本轮没有可推进的新方向（重复意图已抑制），%s 后复查",
			plannerWaitingCooldown),
	})
}

// emitExplicitWaitActivity 记录一次 planner 主动声明的等待（含到点时间）。
func (e *Engine) emitExplicitWaitActivity(t *Task, reason string, until time.Time) {
	e.emitActivity(t, db.Activity{
		Worker: "system",
		Kind:   "text",
		Summary: fmt.Sprintf("planner 进入等待：%s（至 %s 自动复查；目标/提示/意图变更会提前唤醒）",
			reason, until.Local().Format("01-02 15:04")),
	})
}
