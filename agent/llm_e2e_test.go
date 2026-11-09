package agent

// 真实 LLM 端到端验证：用真实 provider 跑一次分析师，看它能不能产出结构化 fact。
//
// # 为什么单独一个文件 + 环境变量门控
//
// 它依赖外部 LLM 端点，会花 token。用 TEST_LLM_E2E=1 门控，
// 避免日常跑测试时误触发。
//
// 运行：
//   TEST_LLM_E2E=1 \
//   TEST_LLM_BASE_URL=http://192.168.101.1:20128/v1 \
//   TEST_LLM_API_KEY=sk-xxx TEST_LLM_MODEL=dsf \
//   ARTEX_PG_DSN="postgres://artex:artex@127.0.0.1:5433/artex?sslmode=disable" \
//     go test ./agent/ -run TestRealLLMAnalystEndToEnd -v -count=1
//
// # 它验证单元测试覆盖不到的四件事
//
//   1. prompt 渲染后 LLM 能读懂（不是「渲染没报错」，是「模型能据此产出结构化输出」）
//   2. LLM 会调用 emit_fact（工具名对、schema 对）
//   3. emit_fact 的 role 守卫不误拦（analyst_market 能写 fact）
//   4. 产出落库后下游能通过 siblingOutput 读到
//
// 这四件事只能真跑才知道 —— 前面所有单元测试都是「结构正确」，这里是「语义可用」。

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
)

func llmE2EConfig(t *testing.T) (baseURL, apiKey, model string) {
	t.Helper()
	if os.Getenv("TEST_LLM_E2E") != "1" {
		t.Skip("跳过真实 LLM 测试（设 TEST_LLM_E2E=1 开启）")
	}
	baseURL = os.Getenv("TEST_LLM_BASE_URL")
	apiKey = os.Getenv("TEST_LLM_API_KEY")
	model = os.Getenv("TEST_LLM_MODEL")
	if baseURL == "" || apiKey == "" || model == "" {
		t.Skip("缺 TEST_LLM_BASE_URL / TEST_LLM_API_KEY / TEST_LLM_MODEL")
	}
	return
}

// TestRealLLMAnalystEndToEnd 真实 LLM 跑 analyst_market，验证产出可解析、可落库。
func TestRealLLMAnalystEndToEnd(t *testing.T) {
	baseURL, apiKey, model := llmE2EConfig(t)

	d := testDB(t)

	prov, err := ConfigFrom("openai", model, baseURL, apiKey, "").NewProvider()
	if err != nil {
		t.Fatalf("构造 provider: %v", err)
	}
	t.Logf("provider: %s model=%s", baseURL, model)

	// 建 exploration + intent（模拟 MaterializeGraph 产物）
	expID, err := d.CreateExploration("e2e LLM 验证", "验证分析师能产出 fact")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		d.Exec(`DELETE FROM exploration_nodes WHERE exploration_id=$1`, expID)
		d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	})
	ts := d.Exploration(expID)

	intentPayload := map[string]any{
		"role":          "analyst_market",
		"asset_class":   "etf",
		"instrument_id": float64(0),
		"stage":         "analysis",
	}
	intentID, err := ts.AddNode("intent", intentPayload, 10, "open", "system", nil)
	if err != nil {
		t.Fatal(err)
	}

	// 构造 vars（stub：无 MarketClient）
	vars := AnalystVars{
		Instrument:  InstrumentView{AssetClass: "etf", Market: "us"},
		AsOf:        time.Now().UTC().Format("2006-01-02"),
		Bars:        "（行情数据尚未接入 —— 本次分析无 K 线可用，请在 rationale 里明确说明此限制，不要推测走势）",
		BarsJSON:    "[]",
		Indicators:  `{"_unavailable":"技术指标未计算（行情数据未接入）"}`,
		SnapshotIDs: nil,
		DataGaps:    []string{"行情数据未接入", "标的信息未加载"},
		PastContext: "",
	}

	tmpl := BuiltinPromptSeeds()["analyst_market"]
	if tmpl == "" {
		t.Fatal("analyst_market prompt 不存在")
	}
	sysPrompt := renderSystem("analyst_market", tmpl, vars)
	if strings.Contains(sysPrompt, "{{") {
		t.Fatal("prompt 有未解析占位符")
	}
	t.Logf("prompt 长度: %d 字符", len(sysPrompt))

	// 拿 emit_fact 的 schema（让 LLM 知道有这个工具）
	toolSet := NewToolSet(ts, "e2e-test")
	emitFact := toolSet.emitFact()

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	// 注入 RunInfo，让 emit_* 能反查 role
	ctx = WithRunInfo(ctx, RunInfo{
		ExplorationID: expID,
		IntentID:      intentID,
	})
	ctx = WithExplorationStore(ctx, ts)

	req := llm.CompletionRequest{
		System: []string{sysPrompt},
		Messages: []llm.Message{{
			Role:    "user",
			Content: []llm.ContentBlock{{Type: "text", Text: "请按 system 里的要求完成本次分析。"}},
		}},
		Tools: []llm.ToolSchema{{
			Name:        emitFact.Name(),
			Description: emitFact.Description(),
			InputSchema: emitFact.InputSchema(),
		}},
		MaxTokens: 2048,
	}

	var text strings.Builder

	// 每个 tool_call 单独收集 —— 并行工具调用时 LLM 会一次返回多个 call，
	// 如果用单个 builder 累加，参数会拼成多个 JSON 对象黏在一起
	// （实测症状：`invalid character '{' after top-level value`）。
	type toolCall struct {
		name  string
		input strings.Builder
	}
	var calls []*toolCall
	var cur *toolCall

	for ev, e := range prov.Stream(ctx, req) {
		if e != nil {
			t.Fatalf("stream: %v", e)
		}
		switch ev.Type {
		case llm.SETextDelta:
			text.WriteString(ev.Text)
		case llm.SEToolUseStart:
			cur = &toolCall{name: ev.ToolName}
			calls = append(calls, cur)
			t.Logf("LLM 调用工具 #%d: %s (id=%s)", len(calls), ev.ToolName, ev.ToolID)
		case llm.SEToolInputJSON:
			// 参数是分片流式返回的，按当前 call 累加
			if cur != nil {
				cur.input.WriteString(ev.Text)
			}
		}
	}

	out := text.String()
	t.Logf("=== LLM 文本输出（前 500 字符）===\n%s", truncateE2E(out, 500))

	// ---- 断言 1：LLM 调用了 emit_fact ----
	if len(calls) == 0 {
		t.Fatalf("LLM 没有调用任何工具 —— 这是关键失败：\n"+
			"  可能原因：prompt 没说清要调工具 / 工具 schema 不对 / 模型不遵循指令\n"+
			"  文本输出: %s", truncateE2E(out, 300))
	}
	var emitCalls []*toolCall
	for _, c := range calls {
		if c.name == "emit_fact" {
			emitCalls = append(emitCalls, c)
		} else {
			t.Logf("忽略非 emit_fact 的调用: %s", c.name)
		}
	}
	if len(emitCalls) == 0 {
		t.Fatalf("调用了 %d 个工具但都不是 emit_fact", len(calls))
	}
	t.Logf("✓ LLM 调用了 %d 次 emit_fact（每个 findings 条目一条，符合 prompt 要求）", len(emitCalls))

	// ---- 断言 2：每个参数都能被 emit_fact 接受（真实落库）----
	succeeded := 0
	for i, c := range emitCalls {
		inputJSON := []byte(c.input.String())
		t.Logf("  call #%d 参数: %s", i+1, truncateE2E(string(inputJSON), 250))
		res, callErr := emitFact.Call(ctx, inputJSON, nil)
		if callErr != nil {
			t.Errorf("emit_fact 拒绝了 call #%d: %v", i+1, callErr)
			continue
		}
		if res.IsError {
			var ct strings.Builder
			for _, cb := range res.Content {
				ct.WriteString(cb.Text)
			}
			t.Errorf("emit_fact 对 call #%d 返回错误: %s", i+1, ct.String())
			continue
		}
		succeeded++
	}
	if succeeded == 0 {
		t.Fatalf("所有 emit_fact 调用都失败了")
	}
	t.Logf("✓ %d/%d 个 emit_fact 调用成功落库", succeeded, len(emitCalls))

	// ---- 断言 3：fact 真的在库里，且能被 siblingOutput 读到 ----
	facts, hmore, total, err := ts.ListByKindPageWithSources(db.KindFact, 0, 10, "")
	if err != nil {
		t.Fatalf("查 fact: %v", err)
	}
	_ = hmore
	var found bool
	for _, f := range facts {
		var p map[string]any
		if err := json.Unmarshal(f.Payload, &p); err != nil {
			continue
		}
		if r, _ := p["role"].(string); r == "analyst_market" {
			found = true
			t.Logf("✓ 库里有 analyst_market 的 fact: id=%d payload=%s",
				f.ID, truncateE2E(string(f.Payload), 200))
		}
	}
	if !found {
		t.Errorf("total=%d 但没找到 analyst_market 的 fact —— 落库了但 role 字段丢了？", total)
	}

	// ---- 断言 4：输出提到数据缺失（证明 prompt 的缺失处理章节生效）----
	combined := out
	for _, c := range emitCalls {
		combined += c.input.String()
	}
	mentionsGap := strings.Contains(combined, "缺") || strings.Contains(combined, "未接入") ||
		strings.Contains(combined, "不可用") || strings.Contains(strings.ToLower(combined), "unavailable") ||
		strings.Contains(strings.ToLower(combined), "missing") || strings.Contains(strings.ToLower(combined), "no data")
	if !mentionsGap {
		t.Errorf("输出完全没提数据缺失 —— prompt 的「缺失数据处理」章节可能没生效\n文本: %s",
			truncateE2E(combined, 400))
	} else {
		t.Log("✓ 输出提到了数据缺失（缺失处理章节生效）")
	}
}

func truncateE2E(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
