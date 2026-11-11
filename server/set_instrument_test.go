package server

// set_task_instrument 死锁修复的验收测试。
//
// # 这组测试守的是什么（2026-11-11）
//
// 原来的实现：任务已有标的 → 一律拒绝。看似是「权限边界」，实际制造了死锁：
//
//	上一次调用把 tasks.instrument_id 写好了（第 ③ 步成功）
//	                 ↓
//	MaterializeDownstream 失败（第 ④ 步，网络/DB 抖动）
//	                 ↓
//	重试 → 被「已绑定标的」挡回去 → **下游永远补不上**
//
// 而新加的 materialize_downstream 工具正是用来救这个场景的，
// 所以两条路径都不能再被「已绑定标的」这个判断挡住。

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// cleanupExploration 在测试结束时删掉 exploration 及其全部节点。
//
// # 为什么必须显式清（2026-11-11 实测）
//
// `newAuthServer` 的 fixture cleanup 只调 `DeleteTask`，而它**不清 exploration**
// （实测残留：explorations=2 nodes=32，触发 db 包的残留守卫让整个包 FAIL）。
// 本文件的测试会通过 MaterializeDownstream 往 exploration 里建十几个阶段节点，
// 不显式删就会越跑越脏。
func cleanupExploration(t *testing.T, d *db.DB, expID int64) {
	t.Helper()
	t.Cleanup(func() {
		// 边与节点都对 explorations.id 有 ON DELETE CASCADE，删父行即可。
		if _, err := d.Exec(`DELETE FROM explorations WHERE id=$1`, expID); err != nil {
			t.Logf("清理 exploration %d: %v", expID, err)
		}
	})
}

// callSetInstrument 调 set_task_instrument，返回文本与是否错误。
func callSetInstrument(t *testing.T, s *Server, ctx context.Context, args map[string]any) (string, bool) {
	t.Helper()
	tl := NewSetInstrumentToolSet(s.m.pg).Tools()[0]
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	res, err := tl.Call(ctx, raw, nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var out []byte
	for _, c := range res.Content {
		if c.Type == "text" {
			out = append(out, c.Text...)
		}
	}
	return string(out), res.IsError
}

// TestSetInstrumentHealsMissingDownstream 已绑标的但下游没建 → 必须补建，而不是拒绝。
//
// 这是死锁的直接回归测试。构造方法：先把 tasks.instrument_id 写好（模拟第 ③ 步成功），
// 但不建下游（模拟第 ④ 步失败）。
func TestSetInstrumentHealsMissingDownstream(t *testing.T) {
	s, task, _ := newAuthServer(t)
	cleanupExploration(t, s.m.pg, task.ExpID)

	instID := upsertTestInstrument(t, s.m.pg, db.InstrumentInput{
		Market: "us", Symbol: "TINSTR-HEAL", AssetClass: "stock",
	})
	tid, _ := strconv.ParseInt(task.ID, 10, 64)

	// 模拟「标的已写、下游没建」
	if _, err := s.m.pg.Exec(`UPDATE tasks SET instrument_id=$1 WHERE id=$2`, instID, tid); err != nil {
		t.Fatalf("写标的: %v", err)
	}
	_, before := countStageNodes(t, s.m.pg, task.ExpID)
	if before != 0 {
		t.Fatalf("前置：fixture 任务不该有阶段节点，实际 %d", before)
	}

	ctx := agent.WithRunInfo(context.Background(), agent.RunInfo{
		TaskID:        tid,
		ExplorationID: task.ExpID,
	})
	out, isErr := callSetInstrument(t, s, ctx, map[string]any{
		"symbol": "TINSTR-HEAL", "market": "us", "reason": "测试补建",
	})
	if isErr {
		t.Fatalf("已绑标的 + 下游缺失时应补建而非报错，实际: %s", out)
	}
	if _, after := countStageNodes(t, s.m.pg, task.ExpID); after == 0 {
		t.Fatal("仍然没建出下游节点 —— 死锁没修好")
	}
}

// TestSetInstrumentRefusesOverwriteWhenComplete 下游已完整 → 仍要拒绝改标的。
//
// 修复不能把权限边界一起放掉：用户明确指定标的时，选品员不该覆盖它。
func TestSetInstrumentRefusesOverwriteWhenComplete(t *testing.T) {
	s, task, _ := newAuthServer(t)
	cleanupExploration(t, s.m.pg, task.ExpID)

	instID := upsertTestInstrument(t, s.m.pg, db.InstrumentInput{
		Market: "us", Symbol: "TINSTR-FULL", AssetClass: "stock",
	})
	tid, _ := strconv.ParseInt(task.ID, 10, 64)

	// 建完整下游（模拟「标的 + 阶段图都在」的正常状态）
	if _, err := s.m.pg.Exec(`UPDATE tasks SET instrument_id=$1 WHERE id=$2`, instID, tid); err != nil {
		t.Fatalf("写标的: %v", err)
	}
	if _, err := MaterializeDownstream(context.Background(), s.m.pg, task.ExpID, instID, "stock"); err != nil {
		t.Fatalf("建下游: %v", err)
	}

	ctx := agent.WithRunInfo(context.Background(), agent.RunInfo{
		TaskID:        tid,
		ExplorationID: task.ExpID,
	})
	// 换一个标的再试 —— 必须被拒
	otherID := upsertTestInstrument(t, s.m.pg, db.InstrumentInput{
		Market: "us", Symbol: "TINSTR-OTHER", AssetClass: "stock",
	})
	out, isErr := callSetInstrument(t, s, ctx, map[string]any{
		"symbol": "TINSTR-OTHER", "market": "us", "reason": "试图覆盖",
	})
	if !isErr {
		t.Fatalf("下游已完整时不该允许改标的，实际成功: %s", out)
	}

	// 标的必须还是原来那个
	var got *int64
	if err := s.m.pg.QueryRow(`SELECT instrument_id FROM tasks WHERE id=$1`, tid).Scan(&got); err != nil {
		t.Fatalf("读标的: %v", err)
	}
	if got == nil || *got != instID {
		t.Errorf("标的被改动了：期望 %d，实际 %v", instID, got)
	}
	if got != nil && *got == otherID {
		t.Error("覆盖成功了 —— 权限边界失守")
	}
}

// TestSetInstrumentFreshSelection 无标的时正常选定并建图（原有行为不回归）。
func TestSetInstrumentFreshSelection(t *testing.T) {
	s, task, _ := newAuthServer(t)
	cleanupExploration(t, s.m.pg, task.ExpID)
	tid, _ := strconv.ParseInt(task.ID, 10, 64)

	ctx := agent.WithRunInfo(context.Background(), agent.RunInfo{
		TaskID:        tid,
		ExplorationID: task.ExpID,
	})
	out, isErr := callSetInstrument(t, s, ctx, map[string]any{
		"symbol": "TINSTR-FRESH", "market": "us", "reason": "初选",
	})
	if isErr {
		t.Fatalf("无标的任务应能正常选定: %s", out)
	}
	if _, total := countStageNodes(t, s.m.pg, task.ExpID); total == 0 {
		t.Error("选定标的后应建出下游阶段图")
	}
}

// TestSetInstrumentRequiresTaskContext 没有任务上下文必须拒绝（防跨任务越权）。
func TestSetInstrumentRequiresTaskContext(t *testing.T) {
	s, _, _ := newAuthServer(t)
	out, isErr := callSetInstrument(t, s, context.Background(), map[string]any{
		"symbol": "X", "market": "us", "reason": "无上下文",
	})
	if !isErr {
		t.Fatalf("无任务上下文应拒绝，实际: %s", out)
	}
}

// TestSetInstrumentRequiresReason 没有 reason 必须拒绝（可审计性）。
func TestSetInstrumentRequiresReason(t *testing.T) {
	s, task, _ := newAuthServer(t)
	cleanupExploration(t, s.m.pg, task.ExpID)
	tid, _ := strconv.ParseInt(task.ID, 10, 64)
	ctx := agent.WithRunInfo(context.Background(), agent.RunInfo{
		TaskID: tid, ExplorationID: task.ExpID,
	})
	out, isErr := callSetInstrument(t, s, ctx, map[string]any{
		"symbol": "X", "market": "us",
	})
	if !isErr {
		t.Fatalf("缺 reason 应拒绝，实际: %s", out)
	}
}
