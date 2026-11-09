package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	actool "github.com/Autumn-27/norma/tool"

	"github.com/Autumn-27/artex/db"
)

// newEmitTestSet 建一个带真 PG 的 ToolSet（emit 工具都需要 DB）。
//
// 返回 (toolset, cleanup)。无 DSN 时 skip。
func newEmitTestSet(t *testing.T) *ToolSet {
	t.Helper()
	d := testDB(t)
	expID := createTestExploration(t, d)
	// 测试自建节点必须清理（ON DELETE CASCADE 连带删节点），否则残留会
	// 让「真实数据量」不可观测 —— 与之前 defer Close → t.Cleanup 同类问题。
	t.Cleanup(func() {
		if _, err := d.Exec("DELETE FROM explorations WHERE id = $1", expID); err != nil {
			t.Logf("清理 exploration %d 失败: %v", expID, err)
		}
		d.Close()
	})
	return NewToolSet(d.Exploration(expID), "work#1")
}

// createTestExploration 建一个临时 exploration。
func createTestExploration(t *testing.T, d *db.DB) int64 {
	t.Helper()
	id, err := d.CreateExploration("emit 测试", "验证 emit_* 工具")
	if err != nil {
		t.Fatalf("建 exploration: %v", err)
	}
	return id
}

// mkIntent 建一个带 role 的 intent 节点，返回其 id。
// 用于给 ctx 造 intent 上下文（role 校验依赖它）。
func mkIntent(t *testing.T, ts *db.ExplorationStore, role string) int64 {
	t.Helper()
	id, err := ts.AddNode("intent", map[string]any{
		"role": role, "text": "test intent for " + role,
	}, 0, "open", "planner", nil)
	if err != nil {
		t.Fatalf("建 intent: %v", err)
	}
	return id
}

// ctxWithIntent 造一个带 RunInfo.{TaskID, IntentID} 的 ctx。
func ctxWithIntent(taskID, intentID int64) context.Context {
	return WithRunInfo(context.Background(), RunInfo{TaskID: taskID, IntentID: intentID})
}

// findTool 从 ToolSet 的工具清单里按名取一个。
func findTool(t *testing.T, ts *ToolSet, name string) actool.CoreTool {
	t.Helper()
	for _, x := range ts.WorkerTools() {
		if x.Name() == name {
			return x
		}
	}
	t.Fatalf("工具 %q 未注册在 WorkerTools", name)
	return nil
}

// callWithCtx 用指定 ctx 调用工具。
func callWithCtx(t *testing.T, tool actool.CoreTool, ctx context.Context, in map[string]any) (string, bool) {
	t.Helper()
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	res, err := tool.Call(ctx, raw, nil)
	if err != nil {
		t.Fatalf("Call 返回 error: %v", err)
	}
	var sb strings.Builder
	for _, c := range res.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}
	return sb.String(), res.IsError
}

// ---------------------------------------------------------------------
// emit_fact
// ---------------------------------------------------------------------

func TestEmitFactRejectsEmptyClaim(t *testing.T) {
	ts := newEmitTestSet(t)
	tool := findTool(t, ts, "emit_fact")
	intentID := mkIntent(t, ts.ts, "analyst_market")
	ctx := ctxWithIntent(1, intentID)

	for _, in := range []map[string]any{
		{"claim": "", "evidence": "x"},
		{"claim": "   ", "evidence": "x"},
		{"claim": "x", "evidence": ""},
		{"claim": "x"},
	} {
		out, isErr := callWithCtx(t, tool, ctx, in)
		if !isErr {
			t.Errorf("入参 %+v 应被拒绝，实际: %s", in, out)
		}
	}
}

func TestEmitFactRejectsUnknownSnapshotID(t *testing.T) {
	// **最重要的一条**：悬空 snapshot_id 必须拒绝 —— 这是证据回溯的唯一通道。
	ts := newEmitTestSet(t)
	tool := findTool(t, ts, "emit_fact")
	intentID := mkIntent(t, ts.ts, "analyst_news")
	ctx := ctxWithIntent(1, intentID)

	out, isErr := callWithCtx(t, tool, ctx, map[string]any{
		"claim":        "测试结论",
		"evidence":     "来自某处的证据",
		"snapshot_ids": []int{999999999},
		"confidence":   0.8,
	})
	if !isErr {
		t.Fatalf("悬空 snapshot_id 应被拒绝，实际: %s", out)
	}
	if !strings.Contains(out, "不存在") {
		t.Errorf("错误信息应说明 id 不存在: %s", out)
	}
}

func TestEmitFactRejectsBadConfidence(t *testing.T) {
	ts := newEmitTestSet(t)
	tool := findTool(t, ts, "emit_fact")
	intentID := mkIntent(t, ts.ts, "analyst_market")
	ctx := ctxWithIntent(1, intentID)

	for _, c := range []float64{-0.1, 1.1, 2} {
		out, isErr := callWithCtx(t, tool, ctx, map[string]any{
			"claim": "c", "evidence": "e", "confidence": c,
		})
		if !isErr {
			t.Errorf("confidence=%v 应被拒绝，实际: %s", c, out)
		}
	}
}

// TestEmitFactRoleSource 验证 role 来源审计标记。
func TestEmitFactRoleSource(t *testing.T) {
	ts := newEmitTestSet(t)
	tool := findTool(t, ts, "emit_fact")

	t.Run("有 intent → role_source=db", func(t *testing.T) {
		intentID := mkIntent(t, ts.ts, "analyst_sentiment")
		ctx := ctxWithIntent(1, intentID)
		out, isErr := callWithCtx(t, tool, ctx, map[string]any{
			"claim": "情感面偏多", "evidence": "新闻样本 20 条中 14 条正面", "confidence": 0.6,
		})
		if isErr {
			t.Fatalf("应成功: %s", out)
		}
		var res struct {
			NodeID int64 `json:"node_id"`
		}
		if err := json.Unmarshal([]byte(out), &res); err != nil {
			t.Fatalf("解析: %v", err)
		}
		// 读出该节点 payload 验证 role_source
		nodes, err := ts.ts.NodesByIDs([]int64{res.NodeID})
		if err != nil || len(nodes) == 0 {
			t.Fatalf("读节点: %v", err)
		}
		var p struct {
			Role       string `json:"role"`
			RoleSource string `json:"role_source"`
		}
		if err := json.Unmarshal(nodes[0].Payload, &p); err != nil {
			t.Fatalf("解析 payload: %v", err)
		}
		if p.Role != "analyst_sentiment" {
			t.Errorf("payload.role = %q, want analyst_sentiment", p.Role)
		}
		if p.RoleSource != "db" {
			t.Errorf("payload.role_source = %q, want db（来自 intent 上下文）", p.RoleSource)
		}
		if nodes[0].Kind != db.KindFact {
			t.Errorf("kind = %q, want fact", nodes[0].Kind)
		}
		if nodes[0].State != "confirmed" {
			t.Errorf("state = %q, want confirmed（fact 不允许 pending）", nodes[0].State)
		}
	})

	t.Run("无 intent + 传 role → role_source=param", func(t *testing.T) {
		ctx := context.Background() // 无 RunInfo
		out, isErr := callWithCtx(t, tool, ctx, map[string]any{
			"claim": "规划阶段的观察", "evidence": "标的已退市", "confidence": 0.9,
			"role": "planner",
		})
		if isErr {
			t.Fatalf("有 role 入参应成功: %s", out)
		}
		var res struct {
			NodeID int64 `json:"node_id"`
		}
		if err := json.Unmarshal([]byte(out), &res); err != nil {
			t.Fatalf("解析: %v", err)
		}
		nodes, _ := ts.ts.NodesByIDs([]int64{res.NodeID})
		var p struct {
			RoleSource string `json:"role_source"`
		}
		_ = json.Unmarshal(nodes[0].Payload, &p)
		if p.RoleSource != "param" {
			t.Errorf("role_source = %q, want param（自报，审计时需注意）", p.RoleSource)
		}
	})

	t.Run("无 intent 且无 role → 拒绝", func(t *testing.T) {
		out, isErr := callWithCtx(t, tool, context.Background(), map[string]any{
			"claim": "c", "evidence": "e",
		})
		if !isErr {
			t.Errorf("缺 role 应拒绝，实际: %s", out)
		}
	})
}

// ---------------------------------------------------------------------
// emit_thesis
// ---------------------------------------------------------------------

func TestEmitThesisRequiresInvalidIf(t *testing.T) {
	ts := newEmitTestSet(t)
	tool := findTool(t, ts, "emit_thesis")
	intentID := mkIntent(t, ts.ts, "debate_bull")
	ctx := ctxWithIntent(1, intentID)

	out, isErr := callWithCtx(t, tool, ctx, map[string]any{
		"thesis": "估值合理，看多",
		// 缺 invalid_if
	})
	if !isErr {
		t.Fatalf("缺 invalid_if 应拒绝，实际: %s", out)
	}
	if !strings.Contains(out, "invalid_if") {
		t.Errorf("错误信息应指明 invalid_if: %s", out)
	}
}

func TestEmitThesisRejectsWrongKindRef(t *testing.T) {
	ts := newEmitTestSet(t)
	tool := findTool(t, ts, "emit_thesis")
	intentID := mkIntent(t, ts.ts, "debate_bear")
	ctx := ctxWithIntent(1, intentID)

	// 造一个 thesis 节点，然后拿它去 supports（要求 fact）
	thesisID, err := ts.ts.AddNode(KindThesisStr, map[string]any{"thesis": "已有论点"},
		10, "open", "worker", nil)
	if err != nil {
		t.Fatalf("建 thesis: %v", err)
	}

	out, isErr := callWithCtx(t, tool, ctx, map[string]any{
		"thesis": "反驳上一个论点", "invalid_if": "若出现 X 则失效",
		"supports": []int{int(thesisID)}, // 错：supports 要求 fact
	})
	if !isErr {
		t.Fatalf("supports 指向 thesis 应拒绝，实际: %s", out)
	}
	if !strings.Contains(out, "要求") || !strings.Contains(out, "fact") {
		t.Errorf("错误信息应说明需要 fact: %s", out)
	}

	// 反向：refutes 指向 fact（要求 thesis）
	factID, err := ts.ts.AddNode(db.KindFact, map[string]any{"claim": "某事实"},
		5, "confirmed", "worker", nil)
	if err != nil {
		t.Fatalf("建 fact: %v", err)
	}
	out2, isErr2 := callWithCtx(t, tool, ctx, map[string]any{
		"thesis": "t", "invalid_if": "if",
		"refutes": []int{int(factID)}, // 错：refutes 要求 thesis
	})
	if !isErr2 {
		t.Fatalf("refutes 指向 fact 应拒绝，实际: %s", out2)
	}
}

// ---------------------------------------------------------------------
// emit_signal
// ---------------------------------------------------------------------

func TestEmitSignalRoleGuard(t *testing.T) {
	ts := newEmitTestSet(t)
	tool := findTool(t, ts, "emit_signal")

	t.Run("非 trader 被拒", func(t *testing.T) {
		for _, role := range []string{"analyst_market", "debate_bull", "portfolio_manager", "planner"} {
			intentID := mkIntent(t, ts.ts, role)
			ctx := ctxWithIntent(1, intentID)
			out, isErr := callWithCtx(t, tool, ctx, map[string]any{
				"instrument_id": 1, "signal_type": "technical", "action": "entry",
				"direction": "long", "rationale": "测试",
			})
			if !isErr {
				t.Errorf("role=%q 应被拒绝，实际: %s", role, out)
			}
			if !strings.Contains(out, "仅允许 trader") {
				t.Errorf("role=%q 错误信息应说明仅允许 trader: %s", role, out)
			}
		}
	})

	t.Run("无 intent 上下文被拒（不接受自报 role）", func(t *testing.T) {
		out, isErr := callWithCtx(t, tool, context.Background(), map[string]any{
			"instrument_id": 1, "signal_type": "technical", "action": "entry",
			"direction": "long", "rationale": "x",
			"role": "trader", // 自报应被忽略
		})
		if !isErr {
			t.Fatalf("无 intent 上下文应拒绝（防提权），实际: %s", out)
		}
		if !strings.Contains(out, "intent") {
			t.Errorf("错误信息应说明缺 intent 上下文: %s", out)
		}
	})

	t.Run("非法 action 被拒", func(t *testing.T) {
		intentID := mkIntent(t, ts.ts, "trader")
		ctx := ctxWithIntent(1, intentID)
		out, isErr := callWithCtx(t, tool, ctx, map[string]any{
			"instrument_id": 1, "signal_type": "technical", "action": "buy",
			"direction": "long", "rationale": "x",
		})
		if !isErr {
			t.Errorf("action=buy 应拒绝，实际: %s", out)
		}
	})

	t.Run("非法 direction 被拒", func(t *testing.T) {
		intentID := mkIntent(t, ts.ts, "trader")
		ctx := ctxWithIntent(1, intentID)
		out, isErr := callWithCtx(t, tool, ctx, map[string]any{
			"instrument_id": 1, "signal_type": "technical", "action": "entry",
			"direction": "up", "rationale": "x",
		})
		if !isErr {
			t.Errorf("direction=up 应拒绝，实际: %s", out)
		}
	})
}

// ---------------------------------------------------------------------
// emit_rating
// ---------------------------------------------------------------------

func TestEmitRatingReviewRequiresReasons(t *testing.T) {
	ts := newEmitTestSet(t)
	tool := findTool(t, ts, "emit_rating")
	intentID := mkIntent(t, ts.ts, "portfolio_manager")
	ctx := ctxWithIntent(1, intentID)

	out, isErr := callWithCtx(t, tool, ctx, map[string]any{
		"instrument_id": 1, "rating": "review", "confidence": 0.3,
		"confidence_reason": "证据不足以形成方向性判断",
		// 缺 review_reasons
	})
	if !isErr {
		t.Fatalf("review 缺 reasons 应拒绝，实际: %s", out)
	}
	if !strings.Contains(out, "review_reasons") {
		t.Errorf("错误信息应指明 review_reasons: %s", out)
	}

	// next_step 为空也要拒
	out2, isErr2 := callWithCtx(t, tool, ctx, map[string]any{
		"instrument_id": 1, "rating": "review", "confidence": 0.3,
		"confidence_reason": "证据不足以形成方向性判断",
		"review_reasons": map[string]any{
			"data_gaps": []string{"缺少最新财报"}, "conflicting_facts": []string{},
			"skipped_analysts": []string{"fundamental"}, "next_step": "",
		},
	})
	if !isErr2 {
		t.Fatalf("next_step 为空应拒绝，实际: %s", out2)
	}
}

func TestEmitRatingNonReviewRejectsReasons(t *testing.T) {
	ts := newEmitTestSet(t)
	tool := findTool(t, ts, "emit_rating")
	intentID := mkIntent(t, ts.ts, "portfolio_manager")
	ctx := ctxWithIntent(1, intentID)

	out, isErr := callWithCtx(t, tool, ctx, map[string]any{
		"instrument_id": 1, "rating": "buy", "confidence": 0.7,
		"confidence_reason": "基本面与技术面共振，风险可控",
		"review_reasons": map[string]any{
			"data_gaps": []string{}, "conflicting_facts": []string{},
			"skipped_analysts": []string{}, "next_step": "无需",
		},
	})
	if !isErr {
		t.Fatalf("非 review 带 reasons 应拒绝，实际: %s", out)
	}
	if !strings.Contains(out, "不应提供") {
		t.Errorf("错误信息应说明不该提供 review_reasons: %s", out)
	}
}

func TestEmitRatingRequiresSubstantiveReason(t *testing.T) {
	ts := newEmitTestSet(t)
	tool := findTool(t, ts, "emit_rating")
	intentID := mkIntent(t, ts.ts, "portfolio_manager")
	ctx := ctxWithIntent(1, intentID)

	for _, reason := range []string{"", "ok", "好", "还行吧"} {
		out, isErr := callWithCtx(t, tool, ctx, map[string]any{
			"instrument_id": 1, "rating": "hold", "confidence": 0.5,
			"confidence_reason": reason,
		})
		if !isErr {
			t.Errorf("confidence_reason=%q 太短应拒绝，实际: %s", reason, out)
		}
	}
}

func TestEmitRatingRoleGuard(t *testing.T) {
	ts := newEmitTestSet(t)
	tool := findTool(t, ts, "emit_rating")
	intentID := mkIntent(t, ts.ts, "trader")
	ctx := ctxWithIntent(1, intentID)

	out, isErr := callWithCtx(t, tool, ctx, map[string]any{
		"instrument_id": 1, "rating": "hold", "confidence": 0.5,
		"confidence_reason": "当前无明确方向，维持持有",
	})
	if !isErr {
		t.Fatalf("非 portfolio_manager 应被拒，实际: %s", out)
	}
	if !strings.Contains(out, "portfolio_manager") {
		t.Errorf("错误信息应说明仅允许 portfolio_manager: %s", out)
	}
}

// ---------------------------------------------------------------------
// 全局：四个 emit 工具都必须是写操作
// ---------------------------------------------------------------------

func TestEmitToolsAreNotReadOnly(t *testing.T) {
	ts := newEmitTestSet(t)
	for _, name := range []string{"emit_fact", "emit_thesis", "emit_signal", "emit_rating"} {
		tool := findTool(t, ts, name)
		if tool.IsReadOnly(nil) {
			t.Errorf("%s 是写操作，IsReadOnly 必须为 false（否则并发保护失效）", name)
		}
	}
}

// TestEmitNodeRoundtrip 集成测试：建 exploration → emit_fact → 查库确认。
func TestEmitNodeRoundtrip(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { d.Close() })

	// 造一条真实 snapshot，供 snapshot_ids 校验通过
	snap, _, err := d.Snapshots().InsertSnapshot(db.SnapshotInput{
		Source:      "test",
		Endpoint:    "/emit-roundtrip",
		ParamsHash:  "h-" + time.Now().Format("150405.000000"),
		CollectedAt: time.Now().UTC().Format(time.RFC3339),
		BodyRef:     "",
		Checksum:    "sum-" + time.Now().Format("150405.000000000"),
	})
	if err != nil {
		t.Fatalf("建 snapshot: %v", err)
	}
	t.Cleanup(func() {
		if _, err := d.Exec("DELETE FROM snapshots WHERE id=$1", snap.ID); err != nil {
			t.Logf("清理 snapshot %d 失败: %v", snap.ID, err)
		}
	})

	eid, err := d.CreateExploration("roundtrip", "验证 emit_fact 落库")
	if err != nil {
		t.Fatalf("建 exploration: %v", err)
	}
	t.Cleanup(func() {
		if _, err := d.Exec("DELETE FROM explorations WHERE id = $1", eid); err != nil {
			t.Logf("清理 exploration %d 失败: %v", eid, err)
		}
	})
	ts := NewToolSet(d.Exploration(eid), "work#1")
	intentID := mkIntent(t, ts.ts, "analyst_market")
	ctx := ctxWithIntent(1, intentID)

	tool := findTool(t, ts, "emit_fact")
	out, isErr := callWithCtx(t, tool, ctx, map[string]any{
		"claim":        "600519 最新收盘 1349.98",
		"evidence":     "来自 akshare 日线最后一条记录",
		"snapshot_ids": []int64{snap.ID},
		"confidence":   0.95,
	})
	if isErr {
		t.Fatalf("应成功: %s", out)
	}
	var res struct {
		NodeID int64 `json:"node_id"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if res.NodeID <= 0 {
		t.Fatal("node_id 应 > 0")
	}

	nodes, err := ts.ts.NodesByIDs([]int64{res.NodeID})
	if err != nil {
		t.Fatalf("查节点: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("应查到 1 个节点，实际 %d", len(nodes))
	}
	n := nodes[0]
	if n.Kind != "fact" {
		t.Errorf("kind = %q, want fact", n.Kind)
	}
	if n.State != "confirmed" {
		t.Errorf("state = %q, want confirmed", n.State)
	}
	if n.Origin != "worker" {
		t.Errorf("origin = %q, want worker", n.Origin)
	}
	var p map[string]any
	if err := json.Unmarshal(n.Payload, &p); err != nil {
		t.Fatalf("解析 payload: %v", err)
	}
	if p["claim"] != "600519 最新收盘 1349.98" {
		t.Errorf("payload.claim = %v", p["claim"])
	}
	if p["role"] != "analyst_market" {
		t.Errorf("payload.role = %v", p["role"])
	}
	sids, ok := p["snapshot_ids"].([]any)
	if !ok || len(sids) != 1 {
		t.Errorf("payload.snapshot_ids = %v", p["snapshot_ids"])
	}
}

// TestEmitFactAllowsEmptySnapshotIDs 空 snapshot_ids 应成功，且带缺失原因标记。
//
// 允许空的理由：盘中调 get_quote 拿现价产 fact 的场景没有快照（is_partial 不落库），
// 硬要求非空会让盘中分析不可用。但必须留痕，否则「无证据链」与「有证据链」
// 在库里长得一样。
func TestEmitFactAllowsEmptySnapshotIDs(t *testing.T) {
	ts := newEmitTestSet(t)
	tool := findTool(t, ts, "emit_fact")
	intentID := mkIntent(t, ts.ts, "analyst_market")
	ctx := ctxWithIntent(1, intentID)

	out, isErr := callWithCtx(t, tool, ctx, map[string]any{
		"claim": "盘中观察到的现价水平", "evidence": "当前 ticker 显示 1349.98",
		"confidence": 0.5,
		// snapshot_ids 故意省略
	})
	if isErr {
		t.Fatalf("空 snapshot_ids 应允许，实际: %s", out)
	}
	var res struct {
		NodeID int64 `json:"node_id"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("解析: %v", err)
	}
	nodes, err := ts.ts.NodesByIDs([]int64{res.NodeID})
	if err != nil || len(nodes) == 0 {
		t.Fatalf("读节点: %v", err)
	}
	var p map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &p); err != nil {
		t.Fatalf("解析 payload: %v", err)
	}
	if got := p["snapshot_absence_reason"]; got != "no_snapshot_captured" {
		t.Errorf("snapshot_absence_reason = %v, want no_snapshot_captured", got)
	}
	if _, has := p["snapshot_ids"]; has {
		t.Error("空 snapshot_ids 时不应写 snapshot_ids 字段")
	}
}

// TestEmitFactWithSnapshotsHasNoAbsenceReason 有 snapshot_ids 时**不得**有缺失标记。
//
// 这条防的是「把 snapshot_absence_reason 写成永远存在」——
// 那样「有完整证据链」的 fact 也带着「无快照」标记，审计时会误判。
func TestEmitFactWithSnapshotsHasNoAbsenceReason(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { d.Close() })

	snap, _, err := d.Snapshots().InsertSnapshot(db.SnapshotInput{
		Source: "test", Endpoint: "/emit-absence-test",
		ParamsHash:  "ph-" + time.Now().Format("150405.000000000"),
		CollectedAt: time.Now().UTC().Format(time.RFC3339),
		Checksum:    "ck-" + time.Now().Format("150405.000000000"),
	})
	if err != nil {
		t.Fatalf("建 snapshot: %v", err)
	}
	t.Cleanup(func() {
		if _, err := d.Exec("DELETE FROM snapshots WHERE id=$1", snap.ID); err != nil {
			t.Logf("清理 snapshot %d 失败: %v", snap.ID, err)
		}
	})

	expID, err := d.CreateExploration("absence-test", "验证有快照时无缺失标记")
	if err != nil {
		t.Fatalf("建 exploration: %v", err)
	}
	t.Cleanup(func() {
		if _, err := d.Exec("DELETE FROM explorations WHERE id = $1", expID); err != nil {
			t.Logf("清理 exploration %d 失败: %v", expID, err)
		}
	})

	ts := NewToolSet(d.Exploration(expID), "work#1")
	intentID := mkIntent(t, ts.ts, "analyst_news")
	ctx := ctxWithIntent(1, intentID)

	tool := findTool(t, ts, "emit_fact")
	out, isErr := callWithCtx(t, tool, ctx, map[string]any{
		"claim": "有完整证据链的结论", "evidence": "来自快照的数据",
		"snapshot_ids": []int64{snap.ID}, "confidence": 0.9,
	})
	if isErr {
		t.Fatalf("应成功: %s", out)
	}
	var res struct {
		NodeID int64 `json:"node_id"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("解析: %v", err)
	}
	nodes, err := ts.ts.NodesByIDs([]int64{res.NodeID})
	if err != nil || len(nodes) == 0 {
		t.Fatalf("读节点: %v", err)
	}
	var p map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &p); err != nil {
		t.Fatalf("解析 payload: %v", err)
	}
	if _, has := p["snapshot_absence_reason"]; has {
		t.Errorf("有 snapshot_ids 时不应有 snapshot_absence_reason，实际 payload: %v", p)
	}
	if ids, ok := p["snapshot_ids"].([]any); !ok || len(ids) != 1 {
		t.Errorf("snapshot_ids 应存在且含 1 个 id，实际: %v", p["snapshot_ids"])
	}
}

// TestEmitFactRoleWhitelist 校验 emit_fact 的角色白名单（role 来自 intent 上下文时生效）。
func TestEmitFactRoleWhitelist(t *testing.T) {
	ts := newEmitTestSet(t)
	tool := findTool(t, ts, "emit_fact")

	allowed := []string{
		"analyst_market", "analyst_news", "analyst_sentiment",
		"analyst_fundamental", "analyst_sentiment_crypto", "research_manager",
		"planner", "mainagent",
	}
	for _, role := range allowed {
		intentID := mkIntent(t, ts.ts, role)
		ctx := ctxWithIntent(1, intentID)
		out, isErr := callWithCtx(t, tool, ctx, map[string]any{
			"claim": "c", "evidence": "e", "confidence": 0.5,
		})
		if isErr {
			t.Errorf("role=%q 应允许，实际: %s", role, out)
		}
	}

	// trader / portfolio_manager / debate_* 不该能写 fact
	denied := []string{"trader", "portfolio_manager", "debate_bull", "debate_bear",
		"risk_aggressive", "risk_conservative", "risk_neutral"}
	for _, role := range denied {
		intentID := mkIntent(t, ts.ts, role)
		ctx := ctxWithIntent(1, intentID)
		out, isErr := callWithCtx(t, tool, ctx, map[string]any{
			"claim": "c", "evidence": "e", "confidence": 0.5,
		})
		if !isErr {
			t.Errorf("role=%q 不该能写 fact，实际通过: %s", role, out)
		}
		if !strings.Contains(out, "不允许角色") {
			t.Errorf("role=%q 错误信息应说明角色不允许: %s", role, out)
		}
	}
}

// TestEmitThesisRoleWhitelist 校验 emit_thesis 的角色白名单。
func TestEmitThesisRoleWhitelist(t *testing.T) {
	ts := newEmitTestSet(t)
	tool := findTool(t, ts, "emit_thesis")

	for _, role := range []string{"debate_bull", "debate_bear",
		"risk_aggressive", "risk_conservative", "risk_neutral"} {
		intentID := mkIntent(t, ts.ts, role)
		ctx := ctxWithIntent(1, intentID)
		out, isErr := callWithCtx(t, tool, ctx, map[string]any{
			"thesis": "t", "invalid_if": "if",
		})
		if isErr {
			t.Errorf("role=%q 应允许，实际: %s", role, out)
		}
	}

	// 分析师只陈述事实，不该下论点
	for _, role := range []string{"analyst_market", "analyst_news", "trader", "portfolio_manager"} {
		intentID := mkIntent(t, ts.ts, role)
		ctx := ctxWithIntent(1, intentID)
		out, isErr := callWithCtx(t, tool, ctx, map[string]any{
			"thesis": "t", "invalid_if": "if",
		})
		if !isErr {
			t.Errorf("role=%q 不该能写 thesis，实际通过: %s", role, out)
		}
	}
}

// TestEmitFactSelfReportedRoleSkipsWhitelist fallback（自报 role）不走白名单。
//
// 理由：自报 role 是模型自己填的，拿它做权限判断等于让模型自己授权 ——
// 比不校验更糟（会给人「有校验」的错觉）。payload 里会记 role_source=param
// 让审计能看出这条记录的角色未经系统确认。
func TestEmitFactSelfReportedRoleSkipsWhitelist(t *testing.T) {
	ts := newEmitTestSet(t)
	tool := findTool(t, ts, "emit_fact")
	ctx := context.Background() // 无 intent 上下文 → 走 fallback

	out, isErr := callWithCtx(t, tool, ctx, map[string]any{
		"claim": "c", "evidence": "e", "confidence": 0.5,
		"role": "trader", // 自报一个不在白名单里的角色
	})
	if isErr {
		t.Fatalf("fallback 自报 role 不该走白名单校验，实际被拒: %s", out)
	}
	var res struct {
		NodeID int64 `json:"node_id"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("解析: %v", err)
	}
	nodes, _ := ts.ts.NodesByIDs([]int64{res.NodeID})
	var p struct {
		RoleSource string `json:"role_source"`
	}
	_ = json.Unmarshal(nodes[0].Payload, &p)
	if p.RoleSource != "param" {
		t.Errorf("role_source = %q, want param（自报，审计可见）", p.RoleSource)
	}
}

// findByAnyTool 在 Worker/Planner/MainAgent 三套工具里按名查找。
// 用于测跨 agent 共享的工具（如 list_facts 只给 planner/main，不在 worker）。
func findByAnyTool(t *testing.T, ts *ToolSet, name string) actool.CoreTool {
	t.Helper()
	for _, set := range [][]actool.CoreTool{ts.WorkerTools(), ts.PlannerTools(), ts.MainAgentTools()} {
		for _, x := range set {
			if x.Name() == name {
				return x
			}
		}
	}
	t.Fatalf("工具 %q 在三套工具里都未注册", name)
	return nil
}

// TestEmitRatingFallbackNormalizesToReview 非法评级归一为 review 且自动填 review_reasons。
//
// # 这条测的是 W3-14 与 emit_rating 的接缝
//
// 改动前：非法值直接报错拒绝（`validRatingValues[rt]` 检查）。
// 改动后：ParseRating 归一到 review，并由系统填 review_reasons —— 因为模型
// 输出非法值时不会提供理由，而 DB CHECK 要求 review 必须带四个键。
//
// # 为什么这是行为改进而不是退让
//
// 报错会让整条链中断（pipeline 白跑），而 review 是「需人工看」——
// 那正是非法值的正确归宿。且文档 §3.4:196 要求 review 不生成草案，所以安全。
func TestEmitRatingFallbackNormalizesToReview(t *testing.T) {
	ts := newEmitTestSet(t)
	tool := findTool(t, ts, "emit_rating")
	intentID := mkIntent(t, ts.ts, "portfolio_manager")
	ctx := ctxWithIntent(1, intentID)

	// ratings.instrument_id 有外键约束 —— 必须建真实标的，不能用 id=1
	// （其它 emit_rating 测试用 1 能过，是因为它们在「参数校验」阶段就失败了，
	//  没走到 INSERT。而本测试要验证「写入成功」，所以必须满足外键。）
	//
	// 用 upsertTestInstrument 而非直接 Upsert：本测试原先是**完全没有 cleanup** 的，
	// 每跑一次留 1 条标的 + 1 条 rating（已积累 12 条 ratings 残留）。
	instID := upsertTestInstrument(t, ts.ts.DB(), db.InstrumentInput{
		Market: "crypto", Symbol: "TINSTR-RATING-FALLBACK", AssetClass: "crypto",
	})

	// "Buy"（大写）是非法值 —— DB 只接受小写
	out, isErr := callWithCtx(t, tool, ctx, map[string]any{
		"instrument_id": instID, "rating": "Buy", "confidence": 0.6,
		"confidence_reason": "技术面与资金面共振向上，突破前高",
	})
	if isErr {
		t.Fatalf("非法评级应被归一为 review 而不是报错，实际: %s", out)
	}

	// 断言库里存的是 review，且 review_reasons 已由系统填
	var rating string
	var reasons []byte
	if err := ts.ts.DB().QueryRow(`
SELECT rating, review_reasons FROM ratings WHERE instrument_id=$1 ORDER BY id DESC LIMIT 1`,
		instID).Scan(&rating, &reasons); err != nil {
		t.Fatalf("读评级: %v", err)
	}
	if rating != "review" {
		t.Errorf("rating = %q, want \"review\"（非法值归一）", rating)
	}
	if len(reasons) == 0 {
		t.Fatal("review 必须带 review_reasons（DB CHECK 要求），实际为空")
	}
	var rr map[string]any
	if err := json.Unmarshal(reasons, &rr); err != nil {
		t.Fatalf("解析 review_reasons: %v", err)
	}
	for _, k := range []string{"data_gaps", "conflicting_facts", "skipped_analysts", "next_step"} {
		if _, ok := rr[k]; !ok {
			t.Errorf("review_reasons 缺键 %q，实际：%s", k, string(reasons))
		}
	}
	// data_gaps 里应含原始输入（"Buy"）—— 那是改进 prompt 的依据
	if gaps, ok := rr["data_gaps"].([]any); !ok || len(gaps) == 0 {
		t.Errorf("data_gaps 应含「模型输出了什么」，实际：%v", rr["data_gaps"])
	} else if !strings.Contains(fmt.Sprint(gaps[0]), "Buy") {
		t.Errorf("data_gaps 应指出原始输入 %q，实际：%v", "Buy", gaps[0])
	}
}

// TestEmitRatingLiteralReviewStillRequiresReasons 合法 review 仍要求模型自带理由。
//
// # 与上一条的对照
//
// 上一条（fallback）系统代填理由；这一条（literal）不代填 ——
// 因为模型主动说 review 时，它知道自己在做什么，理由必须由它给。
//
// 判据：系统只在「模型连格式都没搞对」时代填 —— 那时模型不知道该说什么。
func TestEmitRatingLiteralReviewStillRequiresReasons(t *testing.T) {
	ts := newEmitTestSet(t)
	tool := findTool(t, ts, "emit_rating")
	intentID := mkIntent(t, ts.ts, "portfolio_manager")
	ctx := ctxWithIntent(1, intentID)

	out, isErr := callWithCtx(t, tool, ctx, map[string]any{
		"instrument_id": 1, "rating": "review", "confidence": 0.3,
		"confidence_reason": "证据不足以形成方向性判断",
	})
	if !isErr {
		t.Fatalf("合法 review 缺 reasons 应拒绝（不该代填），实际: %s", out)
	}
	if !strings.Contains(out, "review_reasons") {
		t.Errorf("错误信息应指明 review_reasons: %s", out)
	}
}
