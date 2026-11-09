package agent

// research_manager 必须能读到上游事实与辩论记录（2026-11-06）。
//
// # 事故
//
// 任务 36216 的裁决原文：
//
//	"裁决无法做出：分析师事实与辩论记录在当前 agent 上下文均不可读
//	 （list_facts 报「不在任务探索图内」、list_thesis 入参解析失败），
//	 证据不足以判断多空哪方更强，判 inconclusive。"
//
// 而库里数据是完整的（2 条 analyst fact + 2 条 debate thesis），
// LatestSiblings 的 SQL 实测也返回 2 条。所以问题在「渲染进 prompt」这一环：
// vars 算对了，但模板没引用，或引用后是空。
//
// 这组测试锁住「渲染结果里必须出现上游内容」。

import (
	"strings"
	"testing"
)

// TestResearchManagerVarsRenderFacts 渲染结果必须含分析师事实。
func TestResearchManagerVarsRenderFacts(t *testing.T) {
	vars := ResearchManagerVars{
		AnalystFacts:         "2 份事实（analyst_market / analyst_sentiment_crypto）",
		AnalystFactsJSON:     `[{"role":"analyst_market","claim":"RSI14=37.48"}]`,
		DebateTranscript:     "2 条辩论记录（debate_bull / debate_bear）",
		DebateTranscriptJSON: `[{"role":"debate_bull","thesis":"看多"}]`,
	}
	out := renderTmplMust(t, researchManagerTmpl, vars)

	for _, want := range []string{"RSI14=37.48", "debate_bull"} {
		if !strings.Contains(out, want) {
			t.Errorf("渲染结果应含上游内容 %q（否则模型只能去调工具，而那会失败）", want)
		}
	}
}

// TestResearchManagerVarsEmptyShowsNotWired 空数据要给明确说明，不能是空白。
//
// # 为什么
//
// 空白会让模型以为「prompt 有问题」而去调工具补；而写明「上游尚未产出」
// 它会知道该等或按 inconclusive 裁决。
func TestResearchManagerVarsEmptyShowsNotWired(t *testing.T) {
	// 模拟 buildResearchManagerVars 在「上游无数据」时的产出：
	// 它会给 JSON 字段填明确说明，而不是留空串。
	// 直接用零值测不出这一点 —— 那是构造器的职责，不是渲染的。
	vars := ResearchManagerVars{
		AnalystFacts:         notWiredSibling,
		AnalystFactsJSON:     `[]（上游分析师尚未产出事实）`,
		DebateTranscript:     notWiredSibling,
		DebateTranscriptJSON: `[]（上游辩论尚未产出论点）`,
	}
	out := renderTmplMust(t, researchManagerTmpl, vars)
	if strings.TrimSpace(out) == "" {
		t.Fatal("空 vars 也必须渲染出内容")
	}
	// 空 JSON 时不该留下裸的 ```json 空块 —— 那看起来像数据故障。
	if strings.Count(out, "```json") > 0 {
		// 允许，但块内必须有内容或明确说明。
		idx := strings.Index(out, "```json")
		tail := out[idx:]
		if end := strings.Index(tail[7:], "```"); end >= 0 {
			body := strings.TrimSpace(tail[7 : 7+end])
			if body == "" {
				t.Error("JSON 区块为空 —— 应写明「上游尚未产出」而不是留空白块")
			}
		}
	}
}

// renderTmplMust 渲染模板，失败直接 Fatal。
func renderTmplMust(t *testing.T, tmpl string, vars any) string {
	t.Helper()
	out, err := renderTmpl(tmpl, vars)
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	return out
}
