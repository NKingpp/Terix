package server

// planner 等待态（2026-11-08）。
//
// # 解决什么
//
// 目标依赖未来数据（「等下个交易日收盘再判断入场」）时，一轮规划如果没有
// 产出任何新意图，说明当前没有可推进的方向。此时若仍让每次 worker done 都
// 唤醒 planner，就会变成「每 1~2 分钟跑一次 LLM、结论都是等数据」。
//
// 这里给每个任务一个短暂的 waiting 冷却：冷却期内 runRound 直接吞掉唤醒、
// 不调用 LLM；心跳仍按原间隔复查，冷却到期自动恢复规划。配合
// agent.checkIntentDuplicate 的重复意图抑制，等待型目标从「每轮派一条同义
// 意图」变成「每 N 分钟复查一次」。

import (
	"fmt"
	"time"

	"github.com/Autumn-27/artex/db"
)

// plannerWaitingCooldown 是「本轮零产出」后的降频间隔。
// 10 分钟：比 worker 一轮（分钟级）长一个量级，又不会让新数据等太久。
const plannerWaitingCooldown = 10 * time.Minute

// Engine 的 waiting 字段见 engine.go 结构体。
func (e *Engine) beginWaiting(taskID string, d time.Duration) {
	e.waitMu.Lock()
	defer e.waitMu.Unlock()
	if e.waitUntil == nil {
		e.waitUntil = map[string]time.Time{}
	}
	e.waitUntil[taskID] = time.Now().Add(d)
}

// inWaiting 返回是否仍在冷却；到期时顺手清除标记。
func (e *Engine) inWaiting(taskID string) bool {
	e.waitMu.Lock()
	defer e.waitMu.Unlock()
	until, ok := e.waitUntil[taskID]
	if !ok {
		return false
	}
	if time.Now().Before(until) {
		return true
	}
	delete(e.waitUntil, taskID)
	return false
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

func (e *Engine) emitWaitingActivity(t *Task) {
	e.emitActivity(t, db.Activity{
		Worker: "system",
		Kind:   "text",
		Summary: fmt.Sprintf("等待中：本轮没有可推进的新方向（重复意图已抑制），%s 后复查",
			plannerWaitingCooldown),
	})
}
