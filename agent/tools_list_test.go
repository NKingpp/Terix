package agent

// list_thesis / list_signals 的结构与过滤测试。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// TestListThesisReturnsInvalidIf thesis 必须暴露 invalid_if（可证伪条件）。
//
// 这是 thesis 与 fact 的本质区别：没有 invalid_if，下游无法评估「对手的论点
// 是否已被证伪」，辩论与风控就退化成立场宣示。
func TestListThesisReturnsInvalidIf(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { d.Close() })
	expID, err := d.CreateExploration("list_thesis 测试", "验证 invalid_if 透出")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := d.Exec("DELETE FROM explorations WHERE id=$1", expID); err != nil {
			t.Logf("清理 exploration %d 失败: %v", expID, err)
		}
	})
	es := d.Exploration(expID)

	// 建一条 thesis（带 invalid_if），一条 fact（不应出现在 list_thesis）
	if _, err := es.AddNode(KindThesis, map[string]any{
		"thesis": "中期看多", "invalid_if": "跌破 MA60 且量能萎缩",
		"role": "debate_bull", "round": 1,
	}, 30, "standing", "worker", nil); err != nil {
		t.Fatalf("建 thesis: %v", err)
	}
	if _, err := es.AddNode(KindFact, map[string]any{
		"claim": "RSI=72.3", "role": "analyst_market",
	}, 10, "confirmed", "worker", nil); err != nil {
		t.Fatalf("建 fact: %v", err)
	}

	ts := NewToolSet(es, "work#1")
	tool := findTool(t, ts, "list_thesis")
	out, isErr := callTool(t, tool, map[string]any{})
	if isErr {
		t.Fatalf("list_thesis 失败: %s", out)
	}
	var res struct {
		Theses []struct {
			ID        int64  `json:"id"`
			InvalidIf string `json:"invalid_if"`
			Role      string `json:"role"`
			Round     any    `json:"round"`
			Summary   string `json:"summary"`
		} `json:"theses"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("解析: %v (%s)", err, out)
	}
	if len(res.Theses) != 1 {
		t.Fatalf("应只返回 1 条 thesis（不含 fact），实际 %d: %s", len(res.Theses), out)
	}
	got := res.Theses[0]
	if got.InvalidIf != "跌破 MA60 且量能萎缩" {
		t.Errorf("invalid_if = %q —— 这是 thesis 的核心字段，必须透出", got.InvalidIf)
	}
	if got.Role != "debate_bull" {
		t.Errorf("role = %q", got.Role)
	}
	// fact 的关键字段不该出现
	if strings.Contains(out, "RSI=72.3") {
		t.Error("list_thesis 不应返回 fact 的内容")
	}
}

// TestListFactsStillWorks list_facts 重构后行为不变（回归）。
func TestListFactsStillWorks(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { d.Close() })
	expID, err := d.CreateExploration("list_facts 回归", "重构后行为不变")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := d.Exec("DELETE FROM explorations WHERE id=$1", expID); err != nil {
			t.Logf("清理 exploration %d 失败: %v", expID, err)
		}
	})
	es := d.Exploration(expID)
	if _, err := es.AddNode(KindFact, map[string]any{"claim": "事实一", "summary": "事实一"}, 10, "confirmed", "worker", nil); err != nil {
		t.Fatal(err)
	}

	ts := NewToolSet(es, "work#1")
	tool := findByAnyTool(t, ts, "list_facts")
	out, isErr := callTool(t, tool, map[string]any{})
	if isErr {
		t.Fatalf("list_facts 失败: %s", out)
	}
	var res struct {
		Facts []struct {
			ID int64 `json:"id"`
		} `json:"facts"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("解析: %v (%s)", err, out)
	}
	// 响应键必须仍是 "facts"（不是 "theses"）—— 这是重构时最容易改错的地方
	if res.Total != 1 || len(res.Facts) != 1 {
		t.Errorf("list_facts 应返回 1 条，实际 total=%d len=%d: %s", res.Total, len(res.Facts), out)
	}
}

// TestListSignalsEmptyWhenNoSignals 无信号时返回空数组（不是 null）。
func TestListSignalsEmptyWhenNoSignals(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { d.Close() })
	expID, err := d.CreateExploration("list_signals 空", "无信号")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := d.Exec("DELETE FROM explorations WHERE id=$1", expID); err != nil {
			t.Logf("清理 exploration %d 失败: %v", expID, err)
		}
	})
	es := d.Exploration(expID)
	ts := NewToolSet(es, "work#1")
	ts.SetTaskID(1) // 任意 task；signals 表里没有记录
	tool := findTool(t, ts, "list_signals")
	out, isErr := callTool(t, tool, map[string]any{})
	if isErr {
		t.Fatalf("无信号不该报错: %s", out)
	}
	if strings.Contains(out, `"signals":null`) {
		t.Errorf("空结果应是 []，不是 null（null 会让 LLM 误判为字段缺失）: %s", out)
	}
	var res struct {
		Signals []any `json:"signals"`
		Total   int   `json:"total"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if res.Total != 0 || len(res.Signals) != 0 {
		t.Errorf("应为空: %s", out)
	}
}

// TestListSignalsRequiresTaskID 无任务上下文时明确报错（不静默返回空）。
func TestListSignalsRequiresTaskID(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { d.Close() })
	expID, err := d.CreateExploration("list_signals 无 task", "应报错")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := d.Exec("DELETE FROM explorations WHERE id=$1", expID); err != nil {
			t.Logf("清理 exploration %d 失败: %v", expID, err)
		}
	})
	es := d.Exploration(expID)
	ts := NewToolSet(es, "work#1") // 不设 taskID
	tool := findTool(t, ts, "list_signals")
	out, isErr := callTool(t, tool, map[string]any{})
	if !isErr {
		t.Errorf("无 task_id 应报错（不静默返回空，否则 PM 会以为「本次没信号」）: %s", out)
	}
	if !strings.Contains(out, "task_id") {
		t.Errorf("错误信息应提到 task_id: %s", out)
	}
}

// TestPctOf pctOf 的边界（除零、非数）。
func TestPctOf(t *testing.T) {
	if _, ok := pctOf("1", "0"); ok {
		t.Error("除零应返回 ok=false")
	}
	if _, ok := pctOf("abc", "1"); ok {
		t.Error("非数应返回 ok=false")
	}
	got, ok := pctOf("2", "100")
	if !ok || got != "2.0000" {
		t.Errorf("pctOf(2,100) = %q,%v want 2.0000,true", got, ok)
	}
}

var _ = context.Background
var _ = db.KindThesis
