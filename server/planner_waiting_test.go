package server

import (
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
)

func TestPlannerWaitingCooldown(t *testing.T) {
	e := &Engine{}
	e.beginWaiting("2", time.Hour)
	if !e.inWaiting("2") {
		t.Fatal("冷却期内应处于 waiting")
	}
	e.clearWaiting("2")
	if e.inWaiting("2") {
		t.Fatal("clearWaiting 后不该还在 waiting")
	}
}

func TestPlannerWaitingExpires(t *testing.T) {
	e := &Engine{}
	e.beginWaiting("2", -time.Second) // 已过期
	if e.inWaiting("2") {
		t.Fatal("过期后应自动恢复规划")
	}
	if e.inWaiting("2") {
		t.Fatal("过期的标记应已被清除")
	}
}

func TestPlannerWaitingIsPerTask(t *testing.T) {
	e := &Engine{}
	e.beginWaiting("2", time.Hour)
	if e.inWaiting("3") {
		t.Fatal("waiting 不该串到别的任务")
	}
}

func TestIntentCountNilStore(t *testing.T) {
	if got := intentCount(nil); got != -1 {
		t.Fatalf("nil store 应返回 -1（跳过 waiting 判定），实际 %d", got)
	}
}

// 显式等待必须报出原因与到点时间，并夹逼到 [1min, 24h]。
func TestBeginExplicitWaitClampsAndReports(t *testing.T) {
	e := &Engine{}
	until, err := e.BeginExplicitWait("9", "等人工验收", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Until(until); d < 30*time.Second || d > plannerWaitMin+time.Minute {
		t.Fatalf("等待应被抬到下限 1 分钟，实际剩 %s", d)
	}
	reason, explicit := func() (string, bool) {
		_, r, ex, ok := e.WaitState("9")
		if !ok {
			t.Fatal("WaitState 应报出等待中")
		}
		return r, ex
	}()
	if reason != "等人工验收" || !explicit {
		t.Fatalf("原因/显式标记不对: reason=%q explicit=%v", reason, explicit)
	}

	until, err = e.BeginExplicitWait("10", "等收盘", 100000)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Until(until); d > plannerWaitMax+time.Minute {
		t.Fatalf("等待应被截到上限 24h，实际 %s", d)
	}
}

// 零产出冷却不得覆盖 planner 已声明的显式等待 —— 否则「等到明天收盘」会被
// 降频成 10 分钟一醒，等于没等。
func TestExplicitWaitSurvivesZeroOutputCooldown(t *testing.T) {
	e := &Engine{}
	until, _ := e.BeginExplicitWait("11", "等人工验收", 120)
	if !e.isExplicitWait("11") {
		t.Fatal("刚声明的等待应被识别为显式")
	}
	// 模拟 runRound 的零产出分支：只有非显式才降频。
	if !e.isExplicitWait("11") {
		e.beginWaiting("11", plannerWaitingCooldown)
	}
	got, _, _, _ := e.WaitState("11")
	if !got.Equal(until) {
		t.Fatalf("显式等待的到点时间被覆盖了: %s != %s", got, until)
	}
}

// 外部输入穿透等待；worker 产出与心跳不穿透（否则等待等于没等）。
func TestUrgentTriggersPenetrateWait(t *testing.T) {
	urgent := [][]agent.TriggerEvent{
		{{Kind: "goal"}},
		{{Kind: "goal_edited"}},
		{{Kind: "goal_deleted"}},
		{{Kind: "goal_accepted"}},
		{{Kind: "hint"}},
		{{Kind: "cancelled"}},
	}
	for _, evs := range urgent {
		if !urgentTriggers(evs) {
			t.Fatalf("%s 应穿透等待", evs[0].Kind)
		}
	}
	for _, k := range []string{"done", "finding", ""} {
		if urgentTriggers([]agent.TriggerEvent{{Kind: k}}) {
			t.Fatalf("%q 不该穿透等待（那正是等待期间要攒下的东西）", k)
		}
	}
	if urgentTriggers(nil) {
		t.Fatal("无触发不该算穿透")
	}
}

// waitGate 的完整判据矩阵。判错一次的表现是「planner 睡过去」，
// 而线上只看到「怎么不动了」—— 所以把 {等待种类 × 触发清单} 全穷举。
func TestWaitGateMatrix(t *testing.T) {
	done := []agent.TriggerEvent{{Kind: "done", IntentID: 1}}
	finding := []agent.TriggerEvent{{Kind: "finding", IntentID: 1}}
	goal := []agent.TriggerEvent{{Kind: "goal", Goals: []string{"新目标"}}}
	accepted := []agent.TriggerEvent{{Kind: "goal_accepted", Detail: "目标"}}
	none := []agent.TriggerEvent(nil)

	cases := []struct {
		name     string
		inWait   bool
		explicit bool
		evs      []agent.TriggerEvent
		proceed  bool
		clear    bool
	}{
		// 不在等待 → 一律继续，不碰等待标记
		{"no-wait/empty", false, false, none, true, false},
		{"no-wait/done", false, false, done, true, false},

		// 隐式冷却：任何触发都穿透（任务 44 的核心修复）
		{"cooldown/done", true, false, done, true, true},
		{"cooldown/finding", true, false, finding, true, true},
		{"cooldown/goal", true, false, goal, true, true},
		// 隐式冷却 + 纯心跳（无触发）→ 吞掉，但保留冷却（到点自然恢复）
		{"cooldown/empty", true, false, none, false, false},

		// 显式等待：只认人的输入
		{"wait/done", true, true, done, false, false},
		{"wait/finding", true, true, finding, false, false},
		{"wait/goal", true, true, goal, true, true},
		{"wait/accepted", true, true, accepted, true, true},
		{"wait/empty", true, true, none, false, false},
		// 混合：只要有一条人的输入就穿透
		{"wait/done+goal", true, true, append(append([]agent.TriggerEvent{}, done...), goal...), true, true},
	}
	for _, c := range cases {
		proceed, clear := waitGate(c.inWait, c.explicit, c.evs)
		if proceed != c.proceed || clear != c.clear {
			t.Errorf("%s: waitGate = (%v,%v), want (%v,%v) —— evs=%v",
				c.name, proceed, clear, c.proceed, c.clear, c.evs)
		}
	}
}

// 回归：任务 44 的形态 —— 零产出后 10 分钟冷却期内，6 个 stage 意图陆续 done。
// 修复前第一条 done 就被吞、冷却一直维持到 08:45，planner 08:52 才醒。
// 修复后第一条 done 就穿透，冷却不再挡住流水线产出。
func TestWorkerDoneBreaksCooldown(t *testing.T) {
	e := &Engine{}
	e.beginWaiting("44", time.Hour) // 模拟零产出后的长冷却
	if !e.inWaiting("44") {
		t.Fatal("应处于冷却")
	}
	// 模拟 worker 结束：done 触发
	evs := []agent.TriggerEvent{{Kind: "done", IntentID: 3447}}
	proceed, clear := waitGate(e.inWaiting("44"), e.isExplicitWait("44"), evs)
	if !proceed {
		t.Fatal("worker done 必须唤醒 planner（隐式冷却不该吞掉它）")
	}
	if !clear {
		t.Fatal("穿透后必须清掉冷却，否则下一轮又会睡")
	}
	e.clearWaiting("44")
	if e.inWaiting("44") {
		t.Fatal("清掉后不该还在冷却")
	}
}
