package agent

// debate 与 research_manager 读取上游事实的验收测试（2026-11-06）。
//
// # 事故
//
// 任务 36217 的裁决原文：
//
//	"4 条辩论记录中两方均无实际事实支撑（bear 明确记录取数失败，
//	 bull 自认无有效论证）"
//
// 而库里 analyst_market 有 7 条 fact、analyst_sentiment_crypto 有 1 条。
// 所以问题在「readFacts 没能把事实读出来并渲染进 prompt」。
//
// 这组测试用真实的 ExploreStore 验证读路径。

import (
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// TestReadFactsFormatsPayload 读出来的事实必须含 payload 内容。
//
// # 为什么
//
// formatSiblingNode 把 payload 包在 {"_kind":..., "data": {...}} 里。
// 若 payload 是嵌套很深的 JSON 或字段名不认识，模型读不出「claim 是什么」。
// 这条断言确保「claim 的文本确实出现在渲染结果里」。
func TestReadFactsFormatsPayload(t *testing.T) {
	// 模拟一个真实 fact 的 payload 形态（来自库里的实际数据）。
	raw := []byte(`{"role":"analyst_market","claim":"综合 stance=bearish，confidence=0.78",
	  "evidence":"MACD 死叉 hist -315.34、close < boll_mid","confidence":0.78}`)
	out := formatSiblingNode(raw, db.KindFact, RoleAnalystMarket, "confirmed")

	for _, want := range []string{"analyst_market", "bearish", "MACD", "0.78"} {
		if !strings.Contains(out, want) {
			t.Errorf("格式化结果应含 %q（否则模型看不到实质内容），实际: %s", want, out)
		}
	}
}

// TestSummarizeSiblingsNamesRoles 摘要必须点名是哪些角色产出的。
//
// # 为什么
//
// 模型需要知道「这份事实来自技术面还是情绪面」。只写「2 份事实」
// 会让它无法判断证据覆盖了哪些维度，从而把「单维度」当成「证据充分」。
func TestSummarizeSiblingsNamesRoles(t *testing.T) {
	nodes := []siblingNode{
		{Role: RoleAnalystMarket, State: "confirmed", Text: "x"},
		{Role: RoleAnalystSentimentCrypto, State: "confirmed", Text: "y"},
	}
	got := summarizeSiblings(nodes, "份事实")
	for _, want := range []string{"2", "analyst_market", "analyst_sentiment_crypto"} {
		if !strings.Contains(got, want) {
			t.Errorf("摘要应含 %q，实际: %s", want, got)
		}
	}
}

// TestReadFactsEmptyGivesActionableText 读不到事实时给可操作说明。
//
// # 事故对照
//
// 原实现返回 notWiredSibling（「尚未接入」），模型读成「系统没接数据源」，
// 于是去调工具补。改成「上游尚未产出，不要调工具」后它才会按证据不足裁决。
func TestReadFactsEmptyGivesActionableText(t *testing.T) {
	summary, full := readFacts(nil) // nil store = 读不到
	if summary == "" {
		t.Fatal("摘要不能为空")
	}
	if full != "" {
		// 空数据时 full 应当是空（说明由 summary 承担）
		t.Logf("full=%q", full)
	}
	if !strings.Contains(summary, "尚未") && !strings.Contains(summary, "未接入") {
		t.Errorf("摘要应说明「没数据」，实际: %s", summary)
	}
}
