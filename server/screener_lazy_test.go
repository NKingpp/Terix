package server

// set_task_instrument 与「懒展开下游」的验收测试（2026-11-05）。
//
// # 事故背景
//
// 无标的任务原来一次性建 14 个意图。但 13 个都要 instrument_id，
// 而标的要等 screener（选品员）跑完才知道。于是那 13 个节点建出来
// 全是空标的：
//
//	analyst_market      → {"asset_class":"stock","instrument_id":0}
//	analyst_fundamental → "标的未知...无法进行分析" → stance=unavailable
//
// 三个分析师全部判 unavailable，整条链断在 analysis 阶段。
//
// 修法两条：
//  1. 创建任务时只建选品节点（ExpandOpts.ScreenerOnly）
//  2. screener 调 set_task_instrument 选定标的 → 补建下游

import (
	"testing"

	"github.com/Autumn-27/artex/agent"
)

// TestExpandScreenerOnlyHasSingleIntent 无标的任务只建一个意图。
//
// # 这是「去除一开始就派发 intent」的核心断言
//
// 之前 14 个节点全部建出来，用户看到满屏「intent 168xxx 」待办，
// 而实际只有一个能跑（其余缺标的）。现在只有一个。
func TestExpandScreenerOnlyHasSingleIntent(t *testing.T) {
	g, err := agent.Expand(agent.AssetClassStock, agent.ExpandOpts{
		InstrumentID: 0,
		ScreenerOnly: true,
	})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	intents := 0
	for _, n := range g.Nodes {
		if n.Kind == agent.KindIntent {
			intents++
		}
	}
	if intents != 1 {
		t.Errorf("ScreenerOnly 应只建 1 个意图（screener），实际 %d 个", intents)
	}
	if g.Nodes[0].Role != agent.RoleScreener {
		t.Errorf("唯一意图应是 screener，实际 %q", g.Nodes[0].Role)
	}
}

// TestExpandScreenerOnlySkipsWhenInstrumentBound 有标的任务不受 ScreenerOnly 影响。
//
// 已绑标的时不需要选品，ScreenerOnly 应当无效 —— 否则会建出一个
// 只含分析师的半张图（缺选品节点本身没问题，但语义上该走完整图）。
func TestExpandScreenerOnlySkipsWhenInstrumentBound(t *testing.T) {
	g, err := agent.Expand(agent.AssetClassStock, agent.ExpandOpts{
		InstrumentID: 42,
		ScreenerOnly: true,
	})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	intents := 0
	for _, n := range g.Nodes {
		if n.Kind == agent.KindIntent {
			intents++
		}
	}
	if intents < 10 {
		t.Errorf("有标的任务 ScreenerOnly 应无效（建完整图），实际只有 %d 个意图", intents)
	}
}

// TestFullExpandStillHasAllStages 完整展开仍是全量图。
//
// 懒展开只作用于「无标 + ScreenerOnly」这条路。有标任务的图不能缩水。
func TestFullExpandStillHasAllStages(t *testing.T) {
	g, err := agent.Expand(agent.AssetClassStock, agent.ExpandOpts{InstrumentID: 7})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	want := map[string]bool{
		"analyst_market": false, "analyst_fundamental": false, "analyst_news": false,
		"debate_bull": false, "research_manager": false, "trader": false,
		"portfolio_manager": false,
	}
	for _, n := range g.Nodes {
		if _, ok := want[n.Role]; ok {
			want[n.Role] = true
		}
	}
	for role, found := range want {
		if !found {
			t.Errorf("完整图缺角色 %q", role)
		}
	}
}
