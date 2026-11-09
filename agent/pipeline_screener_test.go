package agent

// 选品阶段（阶段 0）的验收测试（2026-11-05）。
//
// # 为什么需要这个阶段
//
// 用户允许不指定标的、让系统自己挑。但原来阶段图从分析师开始，
// 而分析师 prompt 里是 {{.Instrument.Symbol}} —— 无标的时为空，
// 分析师只能瞎猜。实测（任务 36191）它连搜 6 次 SearchExtraTools
// 都找不到「标的查询」工具，最后靠任务描述里的文字巧合对上。
//
// 修法：无标的任务在最前面插一个 screener 意图，分析师等它完成。

import "testing"

// TestExpandAddsScreenerWhenNoInstrument 无标的任务必须在最前面有选品节点。
func TestExpandAddsScreenerWhenNoInstrument(t *testing.T) {
	g, err := Expand(AssetClassStock, ExpandOpts{InstrumentID: 0})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}

	var screener *StageNode
	for i := range g.Nodes {
		if g.Nodes[i].Role == RoleScreener {
			screener = &g.Nodes[i]
			break
		}
	}
	if screener == nil {
		t.Fatal("无标的任务应包含 screener 节点")
	}
	if screener.Priority != 5 {
		t.Errorf("screener 优先级 = %d, want 5（必须最小，才能排最前）", screener.Priority)
	}
}

// TestExpandSkipsScreenerWhenInstrumentBound 有标的任务不该多一个选品节点。
//
// # 为什么要这条
//
// 有标的时标的已知，选品阶段纯属浪费一轮 LLM 调用。
// 若实现里忘了判断，所有任务都会多一次无意义的调用。
func TestExpandSkipsScreenerWhenInstrumentBound(t *testing.T) {
	g, err := Expand(AssetClassStock, ExpandOpts{InstrumentID: 42})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	for _, n := range g.Nodes {
		if n.Role == RoleScreener {
			t.Fatal("有标的任务不该有 screener 节点")
		}
	}
}

// TestAnalystsWaitForScreener 分析师必须先等选品完成。
//
// # 这是修复的核心
//
// 只加节点不加依赖 = 分析师与选品并行启动 → 分析师仍然在空标的下瞎猜，
// 等于没修。这条断言锁住「screener → analyst」的边。
func TestAnalystsWaitForScreener(t *testing.T) {
	g, err := Expand(AssetClassStock, ExpandOpts{InstrumentID: 0})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}

	// 找出所有 analyst_* 节点，每个都应有来自 screener 的入边。
	analysts := 0
	for _, n := range g.Nodes {
		if n.Kind != KindIntent || !isAnalystRole(n.Role) {
			continue
		}
		analysts++
		found := false
		for _, e := range g.Edges {
			if e.To == n.Ref && e.From == "screener" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("分析师 %q 没有来自 screener 的依赖边 —— 它会在选品完成前启动", n.Ref)
		}
	}
	if analysts == 0 {
		t.Fatal("没有分析师节点，测试前提不成立")
	}
}

// TestScreenerIsFinancialRole screener 必须被当作金融角色。
//
// # 为什么关键
//
// 金融角色的工具集被收窄（不给 Bash/Write/Edit）。若 screener 不被识别，
// 它会拿到 Bash —— 又回到「自己写 Python 爬行情」的老路。
func TestScreenerIsFinancialRole(t *testing.T) {
	if !isFinancialPrompt(RoleScreener) {
		t.Error("screener 应被 isFinancialPrompt 识别为金融角色（否则它会拿到 Bash）")
	}
}

// TestScreenerHasPrompt screener 必须有 prompt 模板。
func TestScreenerHasPrompt(t *testing.T) {
	if BuiltinPromptSeeds()[RoleScreener] == "" {
		t.Fatal("screener 缺 prompt 模板 —— 会导致 renderRolePrompt 退回 worker 渗透 prompt")
	}
}

// isAnalystRole 判断 role 是否分析师。
func isAnalystRole(role string) bool {
	return len(role) > 8 && role[:8] == "analyst_"
}

// TestScreenerCanEmitFact 选品员必须能写 fact。
//
// # 为什么这条关键（2026-11-05 实测）
//
// 选品员的核心产出就是一条候选清单 fact。白名单漏加时：
//
//	emit_fact 不允许角色 "screener" 调用
//	→ 它已算出 AAPL 349.51 / RSI14=57.8，却写不进去
//	→ 开始试 mainagent / 省略 role 等各种绕过
//	→ 白烧十几轮，候选清单丢失
//
// 这不是「模型笨」，是权限表没跟上新角色。
func TestScreenerCanEmitFact(t *testing.T) {
	if !emitFactAllowedRoles[RoleScreener] {
		t.Error("screener 必须在 emitFactAllowedRoles 里（否则它的候选清单写不进图）")
	}
}

// TestScreenerCannotEmitThesis 选品员不该写论点。
//
// thesis 是「可证伪的主张」。选品只陈述观察（价格/动量），不做主张 ——
// 允许它写 thesis 会让它越界下多空判断，而那是辩论段的工作。
func TestScreenerCannotEmitThesis(t *testing.T) {
	if emitThesisAllowedRoles[RoleScreener] {
		t.Error("screener 不该能写 thesis（越界做多空判断）")
	}
}
