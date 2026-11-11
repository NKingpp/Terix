package agent

// graph_overview 的 stage_graph_missing 验收测试。
//
// # 这组测试守的是什么
//
// 2026-11-11 task 11：screener 因 model_error 被标 blocked → 不调
// set_task_instrument → 下游 13 个节点（由 MaterializeDownstream 创建）
// **从未存在**。而在此之前 planner 完全看不出来 —— graph_overview 里
// 「没跑过 trader」与「压根没建 trader」长得一模一样，它只能派无 role 的
// 通用意图瞎补（实际就是这么干的：#655/#656）。
//
// 所以这里钉住三件事：
//   1. 图只建了一半时必须报出缺失角色（否则 planner 无从得知要补）；
//   2. 完整图不许报缺失（否则每轮刷屏，planner 会一直派补建意图）；
//   3. blocked 的节点**不算**缺失（否则每轮派一条新的同角色意图）。

import (
	"database/sql"
	"strings"
	"testing"
)

// newGraphOverviewToolSet 建带阶段图的 ToolSet。
//
// instID 传 0 表示建「无标任务」的图（只有 screener）。
func newGraphOverviewToolSet(t *testing.T, assetClass string, instID int64, screenerOnly bool) *ToolSet {
	t.Helper()
	return newGraphWithOpts(t, assetClass, ExpandOpts{
		InstrumentID: instID,
		ScreenerOnly: screenerOnly,
	})
}

// newGraphWithOpts 按给定 ExpandOpts 建图并返回 ToolSet。
func newGraphWithOpts(t *testing.T, assetClass string, opts ExpandOpts) *ToolSet {
	t.Helper()
	d := testDB(t)
	expID := mustExploration(t, d)
	es := d.Exploration(expID)

	g, err := Expand(assetClass, opts)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if err := withTx(t, d, func(tx *sql.Tx) error {
		_, e := MaterializeGraph(tx, expID, g)
		return e
	}); err != nil {
		t.Fatalf("MaterializeGraph: %v", err)
	}
	return NewToolSet(es, "planner")
}

// missingOf 从 graphOverviewData 里取 stage_graph_missing。
func missingOf(t *testing.T, ts *ToolSet) []string {
	t.Helper()
	data := ts.graphOverviewData()
	raw, ok := data["stage_graph_missing"]
	if !ok {
		return nil
	}
	list, ok := raw.([]string)
	if !ok {
		t.Fatalf("stage_graph_missing 类型异常: %T", raw)
	}
	return list
}

// TestGraphOverviewNoMissingWhenGraphComplete 完整图不许报缺失。
//
// 这是最重要的一条：误报会让 planner 每轮都派补建意图，刷屏且烧 token。
func TestGraphOverviewNoMissingWhenGraphComplete(t *testing.T) {
	ts := newGraphOverviewToolSet(t, AssetClassStock, 42, false)
	if m := missingOf(t, ts); len(m) != 0 {
		t.Errorf("完整图不该报缺失，实际: %v", m)
	}
}

// TestGraphOverviewScreenerOnlyNoFalsePositive ScreenerOnly 建完、还没选标的时不该报缺失。
//
// 这是 task 11 的**前置**状态：screener 还没跑完，下游本来就该是空的。
// 若这里就报缺失，planner 会在选品还没结束时就去补建下游 —— 建出来的全是
// 「标的未知」的空节点。
//
// 关键机制：screener 节点的 payload 不带 asset_class，所以 assetClassOfGraph
// 返回空 → 不判为金融任务 → 不报缺失。这个「不带 asset_class」是 Expand 的
// 刻意设计（只有下游节点带），本测试把它固化下来。
func TestGraphOverviewScreenerOnlyNoFalsePositive(t *testing.T) {
	ts := newGraphOverviewToolSet(t, AssetClassStock, 0, true)
	if m := missingOf(t, ts); len(m) != 0 {
		t.Errorf("选品尚未完成时不该报下游缺失（会建出一堆空标的节点），实际: %v", m)
	}
}

// TestGraphOverviewReportsMissingWithInstrument 有标的但下游图缺失 → 必须报。
func TestGraphOverviewReportsMissingWithInstrument(t *testing.T) {
	d := testDB(t)
	expID := mustExploration(t, d)
	es := d.Exploration(expID)

	// 手工造一个「带 asset_class 的孤立意图」：模拟图建到一半的状态
	// （比如 Expand 之后落库失败，或 screener 成功但下游展开挂了）。
	_, err := es.AddIntent(map[string]any{
		"summary":       "手工造的孤立意图",
		"role":          RoleAnalystMarket,
		"instrument_id": 42,
		"asset_class":   AssetClassStock,
	}, 5, nil, "planner")
	if err != nil {
		t.Fatalf("AddIntent: %v", err)
	}

	ts := NewToolSet(es, "planner")
	m := missingOf(t, ts)
	if len(m) == 0 {
		t.Fatal("图里只有 analyst_market，必须报出其余缺失角色")
	}
	// trader / portfolio_manager 一定在内（它们在最下游）
	want := map[string]bool{RoleTrader: false, RolePortfolioManager: false}
	for _, r := range m {
		if _, ok := want[r]; ok {
			want[r] = true
		}
	}
	for role, found := range want {
		if !found {
			t.Errorf("缺失列表应含 %q，实际: %v", role, m)
		}
	}
	// 已存在的 analyst_market 不该被报成缺失
	for _, r := range m {
		if r == RoleAnalystMarket {
			t.Error("analyst_market 已存在，不该报缺失")
		}
	}
}

// TestGraphOverviewMissingCountsBlocked blocked 的节点不算缺失。
//
// 否则 planner 每轮都会看到「缺 trader」→ 每轮派一条新的 role=trader 意图。
// blocked 的重跑归提示词里既有的「blocked/exhausted 处置」段管。
func TestGraphOverviewMissingCountsBlocked(t *testing.T) {
	d := testDB(t)
	expID := mustExploration(t, d)
	es := d.Exploration(expID)

	// 造一条 blocked 的 trader
	id, err := es.AddIntent(map[string]any{
		"summary":       "被传输故障打断的交易提案",
		"role":          RoleTrader,
		"instrument_id": 42,
		"asset_class":   AssetClassStock,
	}, 5, nil, "planner")
	if err != nil {
		t.Fatalf("AddIntent: %v", err)
	}
	if _, err := d.Exec(`UPDATE exploration_nodes SET state='blocked' WHERE id=$1`, id); err != nil {
		t.Fatalf("标 blocked: %v", err)
	}

	ts := NewToolSet(es, "planner")
	for _, r := range missingOf(t, ts) {
		if r == RoleTrader {
			t.Error("blocked 的 trader 节点存在，不该报成缺失（会刷屏）")
		}
	}
}

// TestGraphOverviewNoMissingForNonFinance 非金融任务（无 asset_class）不报缺失。
func TestGraphOverviewNoMissingForNonFinance(t *testing.T) {
	d := testDB(t)
	expID := mustExploration(t, d)
	es := d.Exploration(expID)

	// 渗透式意图：只有 summary，没有 asset_class
	if _, err := es.AddIntent(map[string]any{
		"summary": "扫一下 10.0.0.0/24 的开放端口",
	}, 5, nil, "planner"); err != nil {
		t.Fatalf("AddIntent: %v", err)
	}

	ts := NewToolSet(es, "planner")
	if m := missingOf(t, ts); len(m) != 0 {
		t.Errorf("非金融任务不该报阶段图缺失，实际: %v", m)
	}
}

// TestAssetClassOfGraph 判据本身：有 asset_class 才算金融任务。
func TestAssetClassOfGraph(t *testing.T) {
	d := testDB(t)
	expID := mustExploration(t, d)
	es := d.Exploration(expID)

	if got := assetClassOfGraph(es); got != "" {
		t.Errorf("空图应返回空串，实际 %q", got)
	}

	if _, err := es.AddIntent(map[string]any{
		"summary": "分析 BTC", "asset_class": AssetClassCrypto,
	}, 5, nil, "planner"); err != nil {
		t.Fatalf("AddIntent: %v", err)
	}
	if got := assetClassOfGraph(es); got != AssetClassCrypto {
		t.Errorf("assetClassOfGraph = %q, want %q", got, AssetClassCrypto)
	}
	// 与 explorationIsFinancial 必须同源
	if !explorationIsFinancial(es) {
		t.Error("explorationIsFinancial 应与 assetClassOfGraph 同源")
	}
}

// stallOf 从 graphOverviewData 里取 stage_graph_stalled。
func stallOf(t *testing.T, ts *ToolSet) map[string]any {
	t.Helper()
	data := ts.graphOverviewData()
	raw, ok := data["stage_graph_stalled"]
	if !ok {
		return nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("stage_graph_stalled 类型异常: %T", raw)
	}
	return m
}

// TestGraphOverviewStalledWhenScreenerBlocked 选品失败时必须报「卡住」。
//
// # 这是 task 11 的确切形态
//
// screener blocked → 不调 set_task_instrument → 下游 13 个节点从未建出。
// 图上只有 screener 一个节点，与「选品还在跑」**不可区分** ——
// 所以不能塞进 stage_graph_missing（那会误报进行中的任务），
// 而是单独报 stage_graph_stalled，让 planner 看到 screener 的终态。
func TestGraphOverviewStalledWhenScreenerBlocked(t *testing.T) {
	d := testDB(t)
	expID := mustExploration(t, d)
	es := d.Exploration(expID)

	id, err := es.AddIntent(map[string]any{
		"summary":       "筛选加密永续候选",
		"role":          RoleScreener,
		"instrument_id": 0,
		"asset_class":   AssetClassCrypto,
		"stage":         "screen",
	}, 5, nil, "planner")
	if err != nil {
		t.Fatalf("AddIntent: %v", err)
	}
	if _, err := d.Exec(`UPDATE exploration_nodes SET state='blocked' WHERE id=$1`, id); err != nil {
		t.Fatalf("标 blocked: %v", err)
	}

	ts := NewToolSet(es, "planner")
	st := stallOf(t, ts)
	if st == nil {
		t.Fatal("选品 blocked 且无下游时必须报 stage_graph_stalled")
	}
	reason, _ := st["reason"].(string)
	if !strings.Contains(reason, "screener") {
		t.Errorf("处置建议应指向 role=screener，实际: %s", reason)
	}
	// 同时不该报 missing（那会误导 planner 去补建，而不是重跑选品）
	if m := missingOf(t, ts); len(m) != 0 {
		t.Errorf("卡住时应报 stalled 而不是 missing，实际 missing: %v", m)
	}
}

// TestGraphOverviewNoStalledWhileScreenerRunning 选品还在跑时不该报卡住。
func TestGraphOverviewNoStalledWhileScreenerRunning(t *testing.T) {
	d := testDB(t)
	expID := mustExploration(t, d)
	es := d.Exploration(expID)

	id, err := es.AddIntent(map[string]any{
		"summary":     "筛选候选",
		"role":        RoleScreener,
		"asset_class": AssetClassCrypto,
	}, 5, nil, "planner")
	if err != nil {
		t.Fatalf("AddIntent: %v", err)
	}
	if _, err := d.Exec(`UPDATE exploration_nodes SET state='running' WHERE id=$1`, id); err != nil {
		t.Fatalf("标 running: %v", err)
	}

	if st := stallOf(t, NewToolSet(es, "planner")); st != nil {
		t.Errorf("选品进行中不该报 stalled，实际: %v", st)
	}
}
