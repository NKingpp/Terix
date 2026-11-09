package agent

// 兄弟节点读取 + 各角色 vars 构造的测试。
//
// 四个必测点：
//  1. siblingOutput 过滤作废态（superseded/refuted 不该被读到）
//  2. siblingOutput 取最新一条
//  3. **perRoleLimit 是「每个 role」而非总数** —— 3 role 各 2 条 + limit=1 应得 3 条
//  4. buildDebateVars 的 Round 从 float64 payload 正确读出（C1.9 守卫）

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// mkNode 建一个落库节点，返回 id（依赖 exploration 级联清理）。
func mkNode(t *testing.T, es *db.ExplorationStore, kind, role, state string, extra map[string]any) int64 {
	t.Helper()
	payload := map[string]any{"role": role}
	for k, v := range extra {
		payload[k] = v
	}
	id, err := es.AddNode(kind, payload, 10, state, "worker", nil)
	if err != nil {
		t.Fatalf("建节点 %s/%s: %v", kind, role, err)
	}
	return id
}

// withStore ctx 注入 store。
func withStore(es *db.ExplorationStore) context.Context {
	return WithExplorationStore(context.Background(), es)
}

// TestSiblingOutputFiltersTerminalStates 作废态节点不该被读到。
func TestSiblingOutputFiltersTerminalStates(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { d.Close() })
	expID := mustExploration(t, d)
	es := d.Exploration(expID)

	// thesis 的「被驳倒」状态是 refuted，不是 rejected（见 ck_node_state）
	mkNode(t, es, db.KindThesis, RoleResearchManager, "superseded", map[string]any{"thesis": "过期的裁决"})
	mkNode(t, es, db.KindThesis, RoleResearchManager, "refuted", map[string]any{"thesis": "被驳倒的裁决"})
	mkNode(t, es, db.KindThesis, RoleResearchManager, "standing", map[string]any{"thesis": "当前的裁决"})

	got, found, err := siblingOutput(es, db.KindThesis, RoleResearchManager)
	if err != nil {
		t.Fatalf("siblingOutput: %v", err)
	}
	if !found || got == "" {
		t.Fatal("应读到 standing 那条")
	}
	if !strings.Contains(got, "当前的裁决") {
		t.Errorf("应读到 standing，实际: %s", got)
	}
	if strings.Contains(got, "被驳倒的裁决") {
		t.Error("不该读到 refuted —— 会让下游基于过期信息决策")
	}
	if strings.Contains(got, "过期的裁决") {
		t.Error("不该读到 superseded")
	}
	if !strings.Contains(got, `"_state":"standing"`) {
		t.Errorf("输出应含 _state=standing，实际: %s", got)
	}
}

// TestSiblingOutputReturnsLatest 同 role 多条时取最新。
func TestSiblingOutputReturnsLatest(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { d.Close() })
	expID := mustExploration(t, d)
	es := d.Exploration(expID)

	mkNode(t, es, db.KindThesis, RoleDebateBull, "standing", map[string]any{"thesis": "第一轮论点"})
	mkNode(t, es, db.KindThesis, RoleDebateBull, "standing", map[string]any{"thesis": "第二轮论点"})

	got, found, err := siblingOutput(es, db.KindThesis, RoleDebateBull)
	if err != nil || !found {
		t.Fatalf("siblingOutput: found=%v err=%v", found, err)
	}
	if !strings.Contains(got, "第二轮论点") {
		t.Errorf("应取最新一条，实际: %s", got)
	}
	if strings.Contains(got, "第一轮论点") {
		t.Error("不该返回旧的一条")
	}
}

// TestSiblingOutputEmptyReturnsNotFound 无匹配时 found=false + nil error。
func TestSiblingOutputEmptyReturnsNotFound(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { d.Close() })
	expID := mustExploration(t, d)
	es := d.Exploration(expID)

	got, found, err := siblingOutput(es, db.KindThesis, RoleDebateBear)
	if err != nil {
		t.Errorf("无匹配不该报错: %v", err)
	}
	if found || got != "" {
		t.Errorf("无匹配应返回 (\"\", false)，实际 (%q, %v)", got, found)
	}
	if s, f, e := siblingOutput(nil, db.KindThesis, "x"); s != "" || f || e != nil {
		t.Errorf("nil store 应返回 (\"\", false, nil)，实际 (%q, %v, %v)", s, f, e)
	}
}

// TestSiblingOutputsPerRoleLimitIsBalanced **perRoleLimit 均衡性**（核心测试）。
//
// 3 个 role 各 2 条，perRoleLimit=1 应返回 **3 条**（每 role 一条），不是 2 条。
// 若实现用简单 LIMIT（总数限制），会拿到首个 role 的 2 条而丢掉另外两个。
func TestSiblingOutputsPerRoleLimitIsBalanced(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { d.Close() })
	expID := mustExploration(t, d)
	es := d.Exploration(expID)

	roles := []string{RoleRiskAggressive, RoleRiskNeutral, RoleRiskConservative}
	for _, role := range roles {
		for j := 1; j <= 2; j++ {
			mkNode(t, es, db.KindThesis, role, "standing",
				map[string]any{"thesis": role + "-v" + string(rune('0'+j))})
		}
	}

	got, err := siblingOutputs(es, db.KindThesis, roles, 1)
	if err != nil {
		t.Fatalf("siblingOutputs: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("perRoleLimit=1 应返回 3 条（每 role 一条），实际 %d 条 —— "+
			"若为 2 条说明用了总数 LIMIT 而非 PARTITION BY", len(got))
	}
	seen := map[string]bool{}
	for _, n := range got {
		seen[n.Role] = true
		if !strings.Contains(n.Text, "-v2") {
			t.Errorf("%s 应取最新（-v2），实际: %s", n.Role, n.Text)
		}
	}
	for _, role := range roles {
		if !seen[role] {
			t.Errorf("缺 role %q —— 三个风控必须各有一条", role)
		}
	}
}

// TestSiblingOutputsPerRoleLimitZeroMeansAll perRoleLimit=0 表示不限（取全部）。
func TestSiblingOutputsPerRoleLimitZeroMeansAll(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { d.Close() })
	expID := mustExploration(t, d)
	es := d.Exploration(expID)

	// research_manager 场景：多轮辩论，每 role 2 条
	for j := 1; j <= 2; j++ {
		mkNode(t, es, db.KindThesis, RoleDebateBull, "standing", map[string]any{"thesis": "多-" + string(rune('0'+j))})
		mkNode(t, es, db.KindThesis, RoleDebateBear, "standing", map[string]any{"thesis": "空-" + string(rune('0'+j))})
	}

	got, err := siblingOutputs(es, db.KindThesis, debateRoles, 0)
	if err != nil {
		t.Fatalf("siblingOutputs: %v", err)
	}
	if len(got) != 4 {
		t.Errorf("perRoleLimit=0 应取全部 4 条，实际 %d 条", len(got))
	}
}

// TestSummarizeSiblings 摘要器格式。
func TestSummarizeSiblings(t *testing.T) {
	t.Run("空", func(t *testing.T) {
		got := summarizeSiblings(nil, "份事实")
		if !strings.Contains(got, "上游尚未产出") {
			t.Errorf("空时应说明「上游尚未产出」，实际: %s", got)
		}
	})
	t.Run("有内容", func(t *testing.T) {
		nodes := []siblingNode{
			{Role: "analyst_market", State: "confirmed"},
			{Role: "analyst_sentiment_crypto", State: "confirmed"},
		}
		got := summarizeSiblings(nodes, "份事实")
		if !strings.Contains(got, "2 份事实") {
			t.Errorf("应含「2 份事实」，实际: %s", got)
		}
		if !strings.Contains(got, "analyst_market") || !strings.Contains(got, "analyst_sentiment_crypto") {
			t.Errorf("应列出 role，实际: %s", got)
		}
	})
	t.Run("有待评状态时附注", func(t *testing.T) {
		nodes := []siblingNode{
			{Role: "debate_bull", State: "standing"},
			{Role: "debate_bear", State: "open"},
		}
		got := summarizeSiblings(nodes, "条论点")
		if !strings.Contains(got, "state=open") {
			t.Errorf("非 standing 状态应附注，实际: %s", got)
		}
	})
}

// TestBuildDebateVarsRoundFromFloat64Payload C1.9 守卫：round 从 float64 正确读出。
func TestBuildDebateVarsRoundFromFloat64Payload(t *testing.T) {
	raw := []byte(`{"role":"debate_bull","round":2,"max_rounds":3,"asset_class":"stock"}`)
	intent := &db.Node{ID: 1, Kind: db.KindIntent, Payload: raw}

	vars := buildDebateVars(context.Background(), RoleDebateBull, intent)
	if vars.Round != 2 {
		t.Errorf("Round = %d, want 2 —— 若为 0 说明类型断言失败（C1.9）", vars.Round)
	}
	if vars.MaxRounds != 3 {
		t.Errorf("MaxRounds = %d, want 3", vars.MaxRounds)
	}
}

// TestBuildDebateVarsPriorRoundOnlyFromRound2 第 1 轮不读上一轮，第 2 轮才读。
func TestBuildDebateVarsPriorRoundOnlyFromRound2(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { d.Close() })
	expID := mustExploration(t, d)
	es := d.Exploration(expID)
	ctx := withStore(es)

	mkNode(t, es, db.KindThesis, RoleDebateBear, "standing", map[string]any{"thesis": "空头第一轮"})

	t.Run("第 1 轮不读", func(t *testing.T) {
		intent := &db.Node{ID: 1, Kind: db.KindIntent,
			Payload: []byte(`{"role":"debate_bull","round":1}`)}
		vars := buildDebateVars(ctx, RoleDebateBull, intent)
		if vars.PriorRoundSummary != "" {
			t.Errorf("第 1 轮不该有上一轮摘要，实际: %s", vars.PriorRoundSummary)
		}
	})
	t.Run("第 2 轮读对手", func(t *testing.T) {
		intent := &db.Node{ID: 1, Kind: db.KindIntent,
			Payload: []byte(`{"role":"debate_bull","round":2}`)}
		vars := buildDebateVars(ctx, RoleDebateBull, intent)
		if !strings.Contains(vars.PriorRoundSummary, "空头第一轮") {
			t.Errorf("第 2 轮应读到空头上一轮，实际: %q", vars.PriorRoundSummary)
		}
	})
}

// TestBuildDebateVarsDualLayer 双层：摘要与全量都填，且摘要不含全量内容。
func TestBuildDebateVarsDualLayer(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { d.Close() })
	expID := mustExploration(t, d)
	es := d.Exploration(expID)
	ctx := withStore(es)

	mkNode(t, es, db.KindFact, RoleAnalystMarket, "confirmed", map[string]any{"claim": "RSI=76.4"})

	intent := &db.Node{ID: 1, Kind: db.KindIntent,
		Payload: []byte(`{"role":"debate_bull","round":1}`)}
	vars := buildDebateVars(ctx, RoleDebateBull, intent)

	if !strings.Contains(vars.AnalystFacts, "1 份事实") {
		t.Errorf("摘要应含「1 份事实」，实际: %q", vars.AnalystFacts)
	}
	if !strings.Contains(vars.AnalystFactsJSON, "RSI=76.4") {
		t.Errorf("全量应含 claim，实际: %q", vars.AnalystFactsJSON)
	}
	// 摘要不该含全量内容（否则表格会被撑断）
	if strings.Contains(vars.AnalystFacts, "RSI=76.4") {
		t.Error("摘要不该含全量内容 —— 会撑断表格")
	}
}

// TestBuildTraderVarsSessionErrorWhenMarketUnknown market 未知时走 SessionError 三态。
func TestBuildTraderVarsSessionErrorWhenMarketUnknown(t *testing.T) {
	intent := &db.Node{ID: 1, Kind: db.KindIntent,
		Payload: []byte(`{"role":"trader","asset_class":"stock"}`)}

	vars := buildTraderVars(context.Background(), intent)
	if vars.SessionError == "" {
		t.Error("market 未知时必须给 SessionError（不能假装「已闭市」）")
	}
	if vars.TradingSession.NextOpen != "" {
		t.Error("SessionError 非空时不该给 NextOpen")
	}
}

// TestBuildTraderVarsRealMarketHours market 已知时真的调 market_hours。
func TestBuildTraderVarsRealMarketHours(t *testing.T) {
	intent := &db.Node{ID: 1, Kind: db.KindIntent,
		Payload: []byte(`{"role":"trader","asset_class":"crypto"}`)}

	vars := buildTraderVars(context.Background(), intent)
	if vars.SessionError != "" {
		t.Errorf("crypto 有 7x24 时段定义，不该报 SessionError: %s", vars.SessionError)
	}
	if vars.TradingSession.Timezone == "" {
		t.Error("应填时区（crypto 是 UTC）")
	}
}

// TestBuildPMVarsThreeDualLayers PM 三个输入各自双层，互不串。
func TestBuildPMVarsThreeDualLayers(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { d.Close() })
	expID := mustExploration(t, d)
	es := d.Exploration(expID)
	ctx := withStore(es)

	mkNode(t, es, db.KindFact, RoleAnalystMarket, "confirmed", map[string]any{"claim": "MARKER_FACT"})
	mkNode(t, es, db.KindSignal, RoleTrader, "open", map[string]any{"rationale": "MARKER_SIGNAL"})
	mkNode(t, es, db.KindThesis, RoleRiskAggressive, "standing", map[string]any{"thesis": "MARKER_RISK_A"})
	mkNode(t, es, db.KindThesis, RoleRiskConservative, "standing", map[string]any{"thesis": "MARKER_RISK_C"})

	intent := &db.Node{ID: 1, Kind: db.KindIntent,
		Payload: []byte(`{"role":"portfolio_manager","asset_class":"stock"}`)}
	vars := buildPMVars(ctx, intent)

	if !strings.Contains(vars.AnalystFactsJSON, "MARKER_FACT") {
		t.Error("AnalystFactsJSON 应含事实")
	}
	if strings.Contains(vars.AnalystFactsJSON, "MARKER_SIGNAL") {
		t.Error("AnalystFactsJSON 不该含信号 —— 三个输入串了")
	}
	if !strings.Contains(vars.TraderProposalJSON, "MARKER_SIGNAL") {
		t.Error("TraderProposalJSON 应含信号")
	}
	if !strings.Contains(vars.RiskDebateSummaryJSON, "MARKER_RISK_A") ||
		!strings.Contains(vars.RiskDebateSummaryJSON, "MARKER_RISK_C") {
		t.Error("RiskDebateSummaryJSON 应含两方意见")
	}
	if strings.Contains(vars.RiskDebateSummaryJSON, "MARKER_FACT") {
		t.Error("RiskDebateSummaryJSON 不该含事实 —— 串了")
	}
	if !strings.Contains(vars.AnalystFacts, "份事实") {
		t.Errorf("AnalystFacts 摘要格式不对: %q", vars.AnalystFacts)
	}
	if !strings.Contains(vars.RiskDebateSummary, "份风控意见") {
		t.Errorf("RiskDebateSummary 摘要格式不对: %q", vars.RiskDebateSummary)
	}
}

// TestFormatSiblingNodeValidJSON 输出是合法 JSON（多行证据不破坏结构）。
func TestFormatSiblingNodeValidJSON(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { d.Close() })
	expID := mustExploration(t, d)
	es := d.Exploration(expID)

	mkNode(t, es, db.KindThesis, RoleDebateBull, "standing",
		map[string]any{"thesis": "多行\n证据", "invalid_if": "跌破 MA60"})

	got, found, err := siblingOutput(es, db.KindThesis, RoleDebateBull)
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("输出不是合法 JSON: %v\n%s", err, got)
	}
	for _, want := range []string{"_kind", "_role", "_state", "data"} {
		if _, ok := parsed[want]; !ok {
			t.Errorf("输出缺字段 %q", want)
		}
	}
}
