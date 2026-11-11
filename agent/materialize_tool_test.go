package agent

// materialize_downstream 工具的验收测试。
//
// # 这组测试守的是什么
//
// 这个工具是 planner 的**唯一自救手段**：screener 失败后下游节点从未建出，
// 除了它没有别的办法。所以几条边界必须钉死：
//   1. 回调未装配时报「不可用」——而不是静默成功（那会让 planner 以为补上了）；
//   2. 回调返回 0 时要说清「已存在」，而不是「补建了 0 个」这种歧义表述
//      （planner 会据此判断是否还要重试）；
//   3. 后端错误要带出可执行的下一步（「改派 role=screener」）。
//
// # 为什么这些测试都需要真实 DB
//
// writeExpTool 会用 needExploration 包一层：`ts == nil` 时**在回调之前**就返回
// 「需要任务上下文」。所以只造空 ToolSet 会让每条测试都因为同一个无关原因通过/失败 ——
// 那种测试什么也没守住（我第一版就踩了：Unavailable 那条是假通过）。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
)

// runTool 执行工具并按 content block 拼出文本。
func runTool(t *testing.T, tl actool.CoreTool, in map[string]any) (string, bool) {
	t.Helper()
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	res, err := tl.Call(context.Background(), raw, nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var sb strings.Builder
	for _, c := range res.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}
	return sb.String(), res.IsError
}

// newMaterializeToolSet 造一个挂在真实 exploration 上的 ToolSet（可注入回调）。
func newMaterializeToolSet(t *testing.T, fn func() (int, error)) *ToolSet {
	t.Helper()
	d := testDB(t)
	expID := mustExploration(t, d)
	ts := NewToolSet(d.Exploration(expID), "planner")
	ts.materializeDownstream = fn
	return ts
}

// TestMaterializeDownstreamUnavailable 未装配回调时必须明确报错。
//
// 静默返回成功是最坏的：planner 会以为图补好了，实际什么都没发生，
// 下一轮发现还缺，再补一次，再"成功"一次 —— 无限循环且无任何线索。
func TestMaterializeDownstreamUnavailable(t *testing.T) {
	ts := newMaterializeToolSet(t, nil) // 有 store，但回调未装配
	out, isErr := runTool(t, ts.MaterializeDownstreamTool(), map[string]any{})
	if !isErr {
		t.Errorf("未装配回调时应报错，实际成功返回: %s", out)
	}
	if !strings.Contains(out, "不可用") {
		t.Errorf("错误文案应说明「不可用」，实际: %s", out)
	}
	// 防止退化成「需要任务上下文」那种无关错误（说明 store 没接上）
	if strings.Contains(out, "需要任务上下文") {
		t.Error("退回 needExploration 的兜底错误 —— 测试没测到目标分支")
	}
}

// TestMaterializeDownstreamCreated 补建成功时报出节点数。
func TestMaterializeDownstreamCreated(t *testing.T) {
	ts := newMaterializeToolSet(t, func() (int, error) { return 13, nil })
	out, isErr := runTool(t, ts.MaterializeDownstreamTool(), map[string]any{})
	if isErr {
		t.Fatalf("不该失败: %s", out)
	}
	if !strings.Contains(out, "13") {
		t.Errorf("应报出补建的节点数，实际: %s", out)
	}
}

// TestMaterializeDownstreamNoop 已存在时必须说清「无缺失」。
//
// 含糊的「补建了 0 个节点」会让 planner 反复重试 —— 它读不出「已经好了」。
func TestMaterializeDownstreamNoop(t *testing.T) {
	ts := newMaterializeToolSet(t, func() (int, error) { return 0, nil })
	out, isErr := runTool(t, ts.MaterializeDownstreamTool(), map[string]any{})
	if isErr {
		t.Fatalf("no-op 不该算失败: %s", out)
	}
	if !strings.Contains(out, "已存在") {
		t.Errorf("应明确说「阶段图已存在」，实际: %s", out)
	}
}

// TestMaterializeDownstreamPropagatesError 后端错误原样带出。
//
// 尤其「未绑标的」那条：planner 要据此改派 role=screener，而不是重试本工具。
func TestMaterializeDownstreamPropagatesError(t *testing.T) {
	want := "本任务尚未绑定标的 —— 请先派一条 role=screener 的意图"
	ts := newMaterializeToolSet(t, func() (int, error) { return 0, errors.New(want) })
	out, isErr := runTool(t, ts.MaterializeDownstreamTool(), map[string]any{})
	if !isErr {
		t.Fatalf("后端报错时应返回错误结果: %s", out)
	}
	if !strings.Contains(out, "role=screener") {
		t.Errorf("应带出「改用 screener」的指引，实际: %s", out)
	}
}

// TestMaterializeDownstreamInPlannerTools 必须真的在 planner 的工具列表里。
//
// 不加进 PlannerTools() 的话，工具实现得再对，模型也永远看不到 ——
// 这是最容易漏的一步（schema 在、seed 在、就是没进列表）。
func TestMaterializeDownstreamInPlannerTools(t *testing.T) {
	ts := &ToolSet{}
	for _, tl := range ts.PlannerTools() {
		if tl.Name() == "materialize_downstream" {
			return
		}
	}
	t.Error("materialize_downstream 不在 PlannerTools() 里 —— planner 看不到它")
}

// TestMaterializeDownstreamSchemaIsEmptyObject 无入参，schema 不能要求任何字段。
//
// 有 required 字段会让模型每次得多传一个无意义的参数（甚至可能因为漏传被拒）。
func TestMaterializeDownstreamSchemaIsEmptyObject(t *testing.T) {
	ts := &ToolSet{}
	schema := ts.MaterializeDownstreamTool().InputSchema()
	if _, ok := schema["required"]; ok {
		t.Error("无入参的工具不该有 required 字段")
	}
	props, _ := schema["properties"].(map[string]any)
	if len(props) != 0 {
		t.Errorf("不该声明任何参数，实际: %v", props)
	}
}
