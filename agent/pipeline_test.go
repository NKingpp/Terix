package agent

import (
	"strings"
	"testing"
	"time"
)

// fixedOpts 是测试用的固定参数（固定 AsOf，避免依赖当前时间）。
func fixedOpts() ExpandOpts {
	return ExpandOpts{
		InstrumentID: 42,
		AsOf:         time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC),
	}
}

// TestExpandStockNodeCount 断言 stock 的节点/边/跳过项精确数量。
//
// 数字来自 ARCH-W2-W5.md §4.2.3 的表。改了流程就必须改这里，
// 改这里就必须 code review —— 这是防「重构悄悄改流程」的唯一便宜手段。
func TestExpandStockNodeCount(t *testing.T) {
	g, err := Expand(AssetClassStock, fixedOpts())
	if err != nil {
		t.Fatalf("Expand(stock): %v", err)
	}
	// 2026-10-30：分析师的 fundamental + news 数据源落地，从 1 个恢复到 3 个
	// （见 analystsFor 的注释）。节点数 3+1+4+1+1+3+1+1 = 15。
	if len(g.Nodes) != 15 {
		t.Errorf("stock 节点数 = %d, want 15（3分析师+1join+4辩论+1研究+1交易+3风控+1join+1组合）", len(g.Nodes))
	}
	if len(g.Edges) != 16 {
		t.Errorf("stock 边数 = %d, want 16", len(g.Edges))
	}
	// stock 现在有 1 条 skipped：analyst_sentiment（股票情绪源未接入）。
	// 「缺失要可见」—— 不写会让人误以为情绪维度已覆盖。
	if len(g.SkippedAnalysts) != 1 || g.SkippedAnalysts[0].Role != RoleAnalystSentiment {
		t.Errorf("stock skipped 应为 [analyst_sentiment]，实际 %+v", g.SkippedAnalysts)
	}

	// 分析师必须 3 个（market + fundamental + news），且**不含** sentiment
	// （股票情绪源未接入，它在 skipped 里而不是节点里）。
	var analystRoles []string
	for _, n := range g.Nodes {
		if n.GroupID == "analysts" && n.Kind == KindIntent {
			analystRoles = append(analystRoles, n.Role)
		}
	}
	if len(analystRoles) != 3 {
		t.Errorf("stock 分析师数 = %d, want 3（market + fundamental + news）: %v", len(analystRoles), analystRoles)
	}
	for _, want := range []string{RoleAnalystMarket, RoleAnalystFundamental, RoleAnalystNews} {
		if !containsStr(analystRoles, want) {
			t.Errorf("stock 应含 %s: %v", want, analystRoles)
		}
	}
	if containsStr(analystRoles, RoleAnalystSentiment) {
		t.Errorf("stock 不该有 analyst_sentiment 节点（无数据源，应在 skipped 里）: %v", analystRoles)
	}

	// 风控必须 3 个且含 aggressive
	var riskRoles []string
	for _, n := range g.Nodes {
		if n.GroupID == "risk" && n.Kind == KindIntent {
			riskRoles = append(riskRoles, n.Role)
		}
	}
	if len(riskRoles) != 3 {
		t.Errorf("风控数 = %d, want 3: %v", len(riskRoles), riskRoles)
	}
	if !containsStr(riskRoles, RoleRiskAggressive) {
		t.Errorf("stock 应含 aggressive 风控: %v", riskRoles)
	}
}

// TestExpandCryptoNodeCount 断言 crypto 的精确数量 + skipped 原因可读。
func TestExpandCryptoNodeCount(t *testing.T) {
	g, err := Expand(AssetClassCrypto, fixedOpts())
	if err != nil {
		t.Fatalf("Expand(crypto): %v", err)
	}
	if len(g.Nodes) != 11 {
		t.Errorf("crypto 节点数 = %d, want 11（2分析师+1join+2辩论+1研究+1交易+2风控+1join+1组合）", len(g.Nodes))
	}
	if len(g.Edges) != 11 {
		t.Errorf("crypto 边数 = %d, want 11", len(g.Edges))
	}

	// skipped 必须 3 条（fundamental / news / risk_aggressive），且 Reason 非空可读中文
	if len(g.SkippedAnalysts) != 3 {
		t.Fatalf("crypto skipped = %d, want 3: %+v", len(g.SkippedAnalysts), g.SkippedAnalysts)
	}
	wantSkipped := map[string]bool{
		RoleAnalystFundamental: false,
		RoleAnalystNews:        false,
		RoleRiskAggressive:     false,
	}
	for _, s := range g.SkippedAnalysts {
		if _, ok := wantSkipped[s.Role]; ok {
			wantSkipped[s.Role] = true
		}
		if strings.TrimSpace(s.Reason) == "" {
			t.Errorf("skipped %q 的 Reason 为空（Reason 会原样进 prompt，不能空）", s.Role)
		}
		// Reason 必须是中文可读：至少含一个非 ASCII 字符
		if !hasNonASCII(s.Reason) {
			t.Errorf("skipped %q 的 Reason 应为中文可读，实际 %q", s.Role, s.Reason)
		}
	}
	for role, found := range wantSkipped {
		if !found {
			t.Errorf("crypto skipped 应包含 %q，实际: %+v", role, g.SkippedAnalysts)
		}
	}

	// crypto 不应有 fundamental 分析师节点
	for _, n := range g.Nodes {
		if n.Role == RoleAnalystFundamental {
			t.Error("crypto 不应有 fundamental 分析师节点（已 skipped）")
		}
		if n.Role == RoleRiskAggressive {
			t.Error("crypto 不应有 aggressive 风控节点（已 skipped）")
		}
	}
}

// TestExpandDeterministic 同一个 (assetClass, opts) 两次展开必须完全相同。
//
// **这是最关键的一条**：如果用 map 存节点再遍历，Go 的 map 迭代顺序是随机的，
// 两次调用顺序会不同，下游按顺序建节点就会产出不稳定的图。
// 这个测试能抓住「看起来对但不可复现」的实现。
func TestExpandDeterministic(t *testing.T) {
	for _, ac := range []string{AssetClassStock, AssetClassCrypto} {
		g1, err := Expand(ac, fixedOpts())
		if err != nil {
			t.Fatalf("Expand(%s) 第一次: %v", ac, err)
		}
		// 连续调多次（map 迭代随机的表现需要多次采样才稳定暴露）
		for i := 0; i < 5; i++ {
			g2, err := Expand(ac, fixedOpts())
			if err != nil {
				t.Fatalf("Expand(%s) 第 %d 次: %v", ac, i+2, err)
			}
			if len(g2.Nodes) != len(g1.Nodes) {
				t.Fatalf("%s 第 %d 次节点数不同: %d vs %d", ac, i+2, len(g2.Nodes), len(g1.Nodes))
			}
			for j := range g1.Nodes {
				n1, n2 := g1.Nodes[j], g2.Nodes[j]
				if n1.Ref != n2.Ref {
					t.Fatalf("%s 第 %d 次第 %d 个节点 Ref 不同: %q vs %q（顺序不稳定！）",
						ac, i+2, j, n1.Ref, n2.Ref)
				}
				if n1.Kind != n2.Kind || n1.Role != n2.Role || n1.GroupID != n2.GroupID {
					t.Fatalf("%s 第 %d 次节点 %q 属性不同", ac, i+2, n1.Ref)
				}
				if n1.Priority != n2.Priority {
					t.Fatalf("%s 第 %d 次节点 %q Priority 不同: %d vs %d",
						ac, i+2, n1.Ref, n1.Priority, n2.Priority)
				}
				if len(n1.Await) != len(n2.Await) {
					t.Fatalf("%s 第 %d 次节点 %q Await 长度不同", ac, i+2, n1.Ref)
				}
				for k := range n1.Await {
					if n1.Await[k] != n2.Await[k] {
						t.Fatalf("%s 第 %d 次节点 %q Await[%d] 不同: %q vs %q",
							ac, i+2, n1.Ref, k, n1.Await[k], n2.Await[k])
					}
				}
			}
			if len(g2.Edges) != len(g1.Edges) {
				t.Fatalf("%s 第 %d 次边数不同", ac, i+2)
			}
			for j := range g1.Edges {
				if g1.Edges[j] != g2.Edges[j] {
					t.Fatalf("%s 第 %d 次第 %d 条边不同: %+v vs %+v",
						ac, i+2, j, g1.Edges[j], g2.Edges[j])
				}
			}
		}
	}
}

// TestExpandUnknownAssetClass 未知 asset_class 必须报错，不能返回空图。
//
// 空图让上层以为「该资产类别不需要任何阶段」，实际是「我们不认识它」——
// 两种情况的正确处置完全相反。
func TestExpandUnknownAssetClass(t *testing.T) {
	for _, ac := range []string{"", "option", "futures", "STOCK", "股票", "crypto "} {
		g, err := Expand(ac, fixedOpts())
		if err == nil {
			t.Errorf("Expand(%q) 应报错，实际返回 %d 个节点", ac, len(g.Nodes))
			continue
		}
		if g != nil {
			t.Errorf("Expand(%q) 报错时应返回 nil 图", ac)
		}
		if !strings.Contains(err.Error(), "未知 asset_class") {
			t.Errorf("错误信息应说明未知 asset_class: %v", err)
		}
	}
}

// TestExpandCustomRounds 自定义轮次下的节点数。
func TestExpandCustomRounds(t *testing.T) {
	t.Run("stock debate=1 → 11 节点", func(t *testing.T) {
		g, err := Expand(AssetClassStock, ExpandOpts{MaxDebateRounds: 1, InstrumentID: 1})
		if err != nil {
			t.Fatal(err)
		}
		// 3+1+2+1+1+3+1+1 = 13（2026-10-30 恢复 fundamental + news，分析师 1→3）
		if len(g.Nodes) != 13 {
			t.Errorf("节点数 = %d, want 13（3分析师+1join+2辩论+1研究+1交易+3风控+1join+1组合）", len(g.Nodes))
		}
	})

	t.Run("stock debate=3 → 15 节点（辩论 6 个）", func(t *testing.T) {
		g, err := Expand(AssetClassStock, ExpandOpts{MaxDebateRounds: 3, InstrumentID: 1})
		if err != nil {
			t.Fatal(err)
		}
		// 3+1+6+1+1+3+1+1 = 17（分析师 3 个 + 辩论 6 个）
		if len(g.Nodes) != 17 {
			t.Errorf("节点数 = %d, want 17（3分析师+1join+6辩论+1研究+1交易+3风控+1join+1组合）", len(g.Nodes))
		}
		var debate int
		for _, n := range g.Nodes {
			if n.GroupID == "debate" {
				debate++
			}
		}
		if debate != 6 {
			t.Errorf("辩论节点 = %d, want 6（2 × 3 轮）", debate)
		}
	})

	t.Run("stock risk=2 → 风控 6 个", func(t *testing.T) {
		g, err := Expand(AssetClassStock, ExpandOpts{MaxRiskDiscussRounds: 2})
		if err != nil {
			t.Fatal(err)
		}
		var risk int
		for _, n := range g.Nodes {
			if n.GroupID == "risk" && n.Kind == KindIntent {
				risk++
			}
		}
		if risk != 6 {
			t.Errorf("风控节点 = %d, want 6（3 × 2 轮）", risk)
		}
	})

	t.Run("crypto risk=2 → 风控 4 个（只有 2 角色）", func(t *testing.T) {
		g, err := Expand(AssetClassCrypto, ExpandOpts{MaxRiskDiscussRounds: 2, InstrumentID: 1})
		if err != nil {
			t.Fatal(err)
		}
		var risk int
		for _, n := range g.Nodes {
			if n.GroupID == "risk" && n.Kind == KindIntent {
				risk++
			}
		}
		if risk != 4 {
			t.Errorf("crypto 风控节点 = %d, want 4（2 × 2 轮）", risk)
		}
		// 2+1+2+1+1+4+1+1 = 13（v1.7 分析师收缩到 2）
		if len(g.Nodes) != 13 {
			t.Errorf("节点数 = %d, want 13", len(g.Nodes))
		}
	})

	t.Run("crypto debate=3 risk=2 → 17 节点", func(t *testing.T) {
		g, err := Expand(AssetClassCrypto, ExpandOpts{MaxDebateRounds: 3, MaxRiskDiscussRounds: 2, InstrumentID: 1})
		if err != nil {
			t.Fatal(err)
		}
		// 2+1+6+1+1+4+1+1 = 17（v1.7 分析师收缩到 2）
		if len(g.Nodes) != 17 {
			t.Errorf("节点数 = %d, want 17", len(g.Nodes))
		}
	})
}

// TestExpandAcyclic 阶段图必须无环且 Await 引用的 Ref 都存在。
//
// Await 引用不存在的节点会在运行时永久阻塞（等一个永远不来的上游），
// 排查成本极高 —— 必须在展开时就保证。
func TestExpandAcyclic(t *testing.T) {
	for _, ac := range []string{AssetClassStock, AssetClassCrypto} {
		g, err := Expand(ac, fixedOpts())
		if err != nil {
			t.Fatalf("Expand(%s): %v", ac, err)
		}
		refs := map[string]bool{}
		for _, n := range g.Nodes {
			refs[n.Ref] = true
		}
		// Await 必须都存在
		for _, n := range g.Nodes {
			for _, a := range n.Await {
				if !refs[a] {
					t.Errorf("%s: 节点 %q 的 Await 引用不存在的 %q", ac, n.Ref, a)
				}
			}
		}
		// 边两端必须都存在
		for _, e := range g.Edges {
			if !refs[e.From] || !refs[e.To] {
				t.Errorf("%s: 边 %q→%q 有端点不存在", ac, e.From, e.To)
			}
		}

		// 无环：对 edges 做拓扑排序，若无法处理完所有节点则说明有环
		indeg := map[string]int{}
		out := map[string][]string{}
		for r := range refs {
			indeg[r] = 0
		}
		for _, e := range g.Edges {
			out[e.From] = append(out[e.From], e.To)
			indeg[e.To]++
		}
		var queue []string
		for r, d := range indeg {
			if d == 0 {
				queue = append(queue, r)
			}
		}
		seen := 0
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			seen++
			for _, nx := range out[cur] {
				indeg[nx]--
				if indeg[nx] == 0 {
					queue = append(queue, nx)
				}
			}
		}
		if seen != len(refs) {
			t.Errorf("%s: 阶段图有环（拓扑排序处理了 %d/%d 个节点）", ac, seen, len(refs))
		}
	}
}

// TestExpandJoinAwait 汇合节点必须等待正确的上游。
func TestExpandJoinAwait(t *testing.T) {
	g, err := Expand(AssetClassStock, fixedOpts())
	if err != nil {
		t.Fatal(err)
	}
	byRef := map[string]StageNode{}
	for _, n := range g.Nodes {
		byRef[n.Ref] = n
	}

	ja, ok := byRef["join_analysts"]
	if !ok {
		t.Fatal("缺 join_analysts")
	}
	// stock 的 join 现在等 3 个分析师（market + fundamental + news）。
	// join 少等一个会让下游辩论在某个分析师还没写完时开跑 —— 那一条事实就丢了。
	if len(ja.Await) != 3 {
		t.Errorf("join_analysts Await = %d, want 3: %v", len(ja.Await), ja.Await)
	}
	for _, want := range []string{RoleAnalystMarket, RoleAnalystFundamental, RoleAnalystNews} {
		if !containsStr(ja.Await, want) {
			t.Errorf("join_analysts 应等待 %q: %v", want, ja.Await)
		}
	}

	jr, ok := byRef["join_risk"]
	if !ok {
		t.Fatal("缺 join_risk")
	}
	if len(jr.Await) != 3 {
		t.Errorf("join_risk Await = %d, want 3: %v", len(jr.Await), jr.Await)
	}

	// v1.7：crypto 等 2 个（market + sentiment_crypto）
	gc, err := Expand(AssetClassCrypto, fixedOpts())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range gc.Nodes {
		if n.Ref == "join_analysts" && len(n.Await) != 2 {
			t.Errorf("crypto join_analysts Await = %d, want 2: %v", len(n.Await), n.Await)
		}
	}
}

// TestExpandPayloadIsolated 每个节点的 Payload 必须是独立 map。
//
// 共享同一个 map 会导致「改一个节点的 payload 影响全部节点」——
// 这类 bug 在写的时候完全看不出来，运行到中途才发现所有节点的 as_of 都变了。
func TestExpandPayloadIsolated(t *testing.T) {
	g, err := Expand(AssetClassStock, fixedOpts())
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Nodes) < 2 {
		t.Fatal("节点太少")
	}
	// 改第一个节点的 payload，验证第二个不受影响
	g.Nodes[0].Payload["injected"] = true
	if _, leaked := g.Nodes[1].Payload["injected"]; leaked {
		t.Error("Payload map 被共享：修改节点 0 影响了节点 1")
	}
	// base payload 必须正确落到每个节点
	for _, n := range g.Nodes {
		if n.Payload["asset_class"] != AssetClassStock {
			t.Errorf("节点 %q 的 asset_class = %v", n.Ref, n.Payload["asset_class"])
		}
		if n.Payload["instrument_id"] != int64(42) {
			t.Errorf("节点 %q 的 instrument_id = %v", n.Ref, n.Payload["instrument_id"])
		}
	}
}

// TestExpandAsOfOptional AsOf 为零时不应写入 as_of 键。
func TestExpandAsOfOptional(t *testing.T) {
	g, err := Expand(AssetClassStock, ExpandOpts{InstrumentID: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range g.Nodes {
		if _, has := n.Payload["as_of"]; has {
			t.Errorf("AsOf 为零时不应有 as_of 键，节点 %q 有 %v", n.Ref, n.Payload["as_of"])
			break
		}
	}
}

// ---- 测试辅助 ----

func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func hasNonASCII(s string) bool {
	for _, r := range s {
		if r > 127 {
			return true
		}
	}
	return false
}

// TestExpandAllStageNodesAreDispatchable 守卫：除 join 外，所有 stage 节点必须是 intent。
//
// 为什么需要这条测试：v1.5 时 research_manager/trader/portfolio_manager/conclusion
// 四个节点被误设为 fact/thesis/signal/conclusion。它们节点数、边数全对，
// 其他测试也全过 —— 但 Frontier 的 SQL 硬编码 kind='intent'，
// 所以这 4 个阶段永远不会被 claim。**建出来的东西不会运行，比建不出来危险得多**：
// 建不出来会报错，建出来不运行会静默失效，直到真实跑分析流程才发现。
func TestExpandAllStageNodesAreDispatchable(t *testing.T) {
	for _, tc := range []struct {
		name       string
		assetClass string
	}{
		{"stock", AssetClassStock},
		{"crypto", AssetClassCrypto},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, err := Expand(tc.assetClass, fixedOpts())
			if err != nil {
				t.Fatal(err)
			}
			for _, n := range g.Nodes {
				if n.Kind == KindJoin {
					continue // join 不由 Frontier 派发，由 ReconcileJoins 兑现
				}
				if n.Kind != KindIntent {
					t.Errorf("节点 %q 的 kind=%q —— stage 节点必须是 intent。"+
						"产物类型（fact/thesis/signal/conclusion）由 emit_* 工具写，"+
						"不是节点 kind。否则 Frontier 永远不会派发它。",
						n.Ref, n.Kind)
				}
			}
		})
	}
}

// TestExpandEdgeEndpointsAreStageKinds 守卫：gates 边两端必须是 intent/join。
func TestExpandEdgeEndpointsAreStageKinds(t *testing.T) {
	g, err := Expand(AssetClassStock, fixedOpts())
	if err != nil {
		t.Fatal(err)
	}
	kindOf := map[string]string{}
	for _, n := range g.Nodes {
		kindOf[n.Ref] = n.Kind
	}
	stageKinds := map[string]bool{KindIntent: true, KindJoin: true}
	for _, e := range g.Edges {
		if !stageKinds[kindOf[e.From]] || !stageKinds[kindOf[e.To]] {
			t.Errorf("边 %q→%q 的端点 kind 为 %q/%q，gates 只允许 intent/join",
				e.From, e.To, kindOf[e.From], kindOf[e.To])
		}
	}
}

// TestExpandCryptoUsesCryptoSentiment 守卫：crypto 必须用加密专用情绪分析师。
//
// 为什么需要这条：v1.6 前 analystsFor("crypto") 返回的是股票版 analyst_sentiment，
// 而股票版 prompt 的示例全是 Reddit/StockTwits/分析师评级 —— 对加密是误导。
// 架构文档 §4.1 要求独立版本（基于资金费率/持仓量），不是复用删段。
//
// 这个 bug 隐藏在一个错误的 role 名（analyst_flow）后面：
// 白名单里写了不存在的名字，所以「crypto 用错 role」这件事没人注意到。
func TestExpandCryptoUsesCryptoSentiment(t *testing.T) {
	g, err := Expand(AssetClassCrypto, fixedOpts())
	if err != nil {
		t.Fatal(err)
	}
	var hasCrypto, hasStock bool
	for _, n := range g.Nodes {
		switch n.Role {
		case RoleAnalystSentimentCrypto:
			hasCrypto = true
		case RoleAnalystSentiment:
			hasStock = true
		}
	}
	if !hasCrypto {
		t.Error("crypto 缺少 analyst_sentiment_crypto —— 会退化成股票版 prompt（对加密误导）")
	}
	if hasStock {
		t.Error("crypto 不该有 analyst_sentiment（股票版）—— 架构 §4.1 要求独立版本")
	}

	// stock 反过来：不能有加密版。
	//
	// v1.7 之后 stock 没有 analyst_sentiment（工具 get_social_sentiment 未实现，
	// 见 analystsFor 注释），所以这里只断言「不该有加密版」——
	// 不断言「必须有股票版」，因为那取决于工具是否就绪，不是 kind 规则。
	gs, err := Expand(AssetClassStock, fixedOpts())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range gs.Nodes {
		if n.Role == RoleAnalystSentimentCrypto {
			t.Error("stock 不该有 analyst_sentiment_crypto（加密版）")
		}
	}
}

// TestExpandStockSkippedIsEmptySliceNotNil stock 的 SkippedAnalysts 必须是 []，不是 nil。
//
// # 为什么 nil 和 [] 不同
//
// JSON 序列化：nil → null，[] → []。
// 而 SkippedAnalysts 会落进 begin.payload.skipped_analysts（JSONB）：
//
//	前端拿到 null → 「这个字段不存在」
//	前端拿到 []   → 「没有跳过任何分析师」
//
// 前者会让前端写多余的判空，而且「字段不存在」意味着「可能是旧版本数据」。
func TestExpandStockSkippedIsEmptySliceNotNil(t *testing.T) {
	g, err := Expand(AssetClassStock, fixedOpts())
	if err != nil {
		t.Fatalf("Expand(stock): %v", err)
	}
	if g.SkippedAnalysts == nil {
		t.Fatal("stock 的 SkippedAnalysts 必须是 []Skipped{}，不能是 nil —— nil 会序列化成 JSON null")
	}
	// 不为 nil（必须 []），但内容非空 —— stock 有 1 条（analyst_sentiment 未接入）。
	if len(g.SkippedAnalysts) != 1 {
		t.Errorf("stock 应有 1 条 skipped（analyst_sentiment），实际 %d 条：%+v", len(g.SkippedAnalysts), g.SkippedAnalysts)
	}
}

// TestExpandCryptoSkippedOrderIsStable crypto 的 skipped 顺序必须稳定。
//
// # 为什么顺序重要
//
// 同一 (assetClass, opts) 必须产出完全相同的图 —— 文档 §4.2.2 的确定性要求。
// 而 skipped 顺序不稳会让 begin.payload 的内容在两次展开间变化，
// 那个 payload 是要落库的（会被比对/审计）。
func TestExpandCryptoSkippedOrderIsStable(t *testing.T) {
	want := []string{"analyst_fundamental", "analyst_news", "risk_aggressive"}

	for i := 0; i < 3; i++ {
		g, err := Expand(AssetClassCrypto, fixedOpts())
		if err != nil {
			t.Fatalf("Expand(crypto) 第 %d 次: %v", i+1, err)
		}
		if len(g.SkippedAnalysts) != len(want) {
			t.Fatalf("skipped 应 %d 条，实际 %d", len(want), len(g.SkippedAnalysts))
		}
		for j, w := range want {
			if g.SkippedAnalysts[j].Role != w {
				t.Errorf("第 %d 次展开的 skipped[%d].Role = %q, want %q（顺序必须稳定）",
					i+1, j, g.SkippedAnalysts[j].Role, w)
			}
			if g.SkippedAnalysts[j].Reason == "" {
				t.Errorf("skipped[%d].Reason 不能为空（必须是可读中文）", j)
			}
		}
	}
}
