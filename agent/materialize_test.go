package agent

// materialize 的测试。
//
// 核心断言是「事务原子性」：图建失败时不能留下半个图。
// 只断言「返回错误」不够 —— 必须查表确认没有残留节点/边。

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// withTx 在一个事务里跑 fn，fn 返回错误则回滚。
// 用真 DB 的事务语义验证 materialize 的原子性。
func withTx(t *testing.T, d *db.DB, fn func(tx *sql.Tx) error) error {
	t.Helper()
	tx, err := d.Begin()
	if err != nil {
		t.Fatalf("开事务: %v", err)
	}
	if fnErr := fn(tx); fnErr != nil {
		_ = tx.Rollback()
		return fnErr
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("提交事务: %v", err)
	}
	return nil
}

// mustExploration 建一个临时 exploration，测试结束自动清理。
func mustExploration(t *testing.T, d *db.DB) int64 {
	t.Helper()
	id, err := d.CreateExploration("materialize 测试", "验证阶段图落库")
	if err != nil {
		t.Fatalf("建 exploration: %v", err)
	}
	t.Cleanup(func() {
		if _, err := d.Exec("DELETE FROM explorations WHERE id=$1", id); err != nil {
			t.Logf("清理 exploration %d 失败: %v", id, err)
		}
	})
	return id
}

// TestMaterializeStockGraph stock 图落库：16 节点 + 17 条 gates 边。
func TestMaterializeStockGraph(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { d.Close() })
	expID := mustExploration(t, d)
	es := d.Exploration(expID)

	g, err := Expand(AssetClassStock, ExpandOpts{InstrumentID: 1})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}

	var res MaterializeResult
	if err := withTx(t, d, func(tx *sql.Tx) error {
		var e error
		res, e = MaterializeGraph(tx, expID, g)
		return e
	}); err != nil {
		t.Fatalf("MaterializeGraph: %v", err)
	}

	if res.NodeCount != len(g.Nodes) {
		t.Errorf("NodeCount = %d, want %d", res.NodeCount, len(g.Nodes))
	}
	if res.EdgeCount != len(g.Edges) {
		t.Errorf("EdgeCount = %d, want %d", res.EdgeCount, len(g.Edges))
	}

	// 库里真的建了这么多节点与边
	var nodeCount, edgeCount int
	if err := d.QueryRow(`SELECT count(*) FROM exploration_nodes
WHERE exploration_id=$1 AND kind IN ('intent','join')`, expID).Scan(&nodeCount); err != nil {
		t.Fatal(err)
	}
	if nodeCount != len(g.Nodes) {
		t.Errorf("库里 intent/join 节点 = %d, want %d", nodeCount, len(g.Nodes))
	}
	if err := d.QueryRow(`SELECT count(*) FROM exploration_edges
WHERE exploration_id=$1 AND rel='gates'`, expID).Scan(&edgeCount); err != nil {
		t.Fatal(err)
	}
	if edgeCount != len(g.Edges) {
		t.Errorf("库里 gates 边 = %d, want %d", edgeCount, len(g.Edges))
	}

	// 节点都是 open 状态（等待派发/兑现）
	var notOpen int
	if err := d.QueryRow(`SELECT count(*) FROM exploration_nodes
WHERE exploration_id=$1 AND kind IN ('intent','join') AND state <> 'open'`, expID).Scan(&notOpen); err != nil {
		t.Fatal(err)
	}
	if notOpen != 0 {
		t.Errorf("有 %d 个节点不是 open 状态", notOpen)
	}

	// payload 里有 role（emit_* 与 ReconcileJoins 靠它）
	nodes, err := es.NodesByIDs([]int64{res.NodeIDs["analyst_market"]})
	if err != nil || len(nodes) == 0 {
		t.Fatalf("查 analyst_market 节点: %v", err)
	}
	var p map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &p); err != nil {
		t.Fatalf("解析 payload: %v", err)
	}
	if p["role"] != RoleAnalystMarket {
		t.Errorf("payload.role = %v, want %s", p["role"], RoleAnalystMarket)
	}
}

// TestMaterializeCryptoWritesSkippedByDesign crypto 的 skipped 写进对应 join。
func TestMaterializeCryptoWritesSkippedByDesign(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { d.Close() })
	expID := mustExploration(t, d)
	es := d.Exploration(expID)

	g, err := Expand(AssetClassCrypto, ExpandOpts{InstrumentID: 1})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(g.SkippedAnalysts) == 0 {
		t.Fatal("crypto 应有 skipped（fundamental + aggressive）")
	}

	var res MaterializeResult
	if err := withTx(t, d, func(tx *sql.Tx) error {
		var e error
		res, e = MaterializeGraph(tx, expID, g)
		return e
	}); err != nil {
		t.Fatalf("MaterializeGraph: %v", err)
	}

	// fundamental 被裁 → 应写进 join_analysts
	jaNodes, err := es.NodesByIDs([]int64{res.NodeIDs["join_analysts"]})
	if err != nil || len(jaNodes) == 0 {
		t.Fatalf("查 join_analysts: %v", err)
	}
	var jaPayload map[string]any
	if err := json.Unmarshal(jaNodes[0].Payload, &jaPayload); err != nil {
		t.Fatalf("解析 join_analysts payload: %v", err)
	}
	skippedArr, ok := jaPayload["skipped_by_design"].([]any)
	if !ok || len(skippedArr) == 0 {
		t.Fatalf("join_analysts.payload.skipped_by_design 应为非空数组，实际 %v",
			jaPayload["skipped_by_design"])
	}
	first := skippedArr[0].(map[string]any)
	if first["role"] != RoleAnalystFundamental {
		t.Errorf("第一条 skipped role = %v, want %s", first["role"], RoleAnalystFundamental)
	}
	if reason, _ := first["reason"].(string); reason == "" {
		t.Error("skipped 的 reason 不能为空（要中文可读）")
	}

	// aggressive 被裁 → 应写进 join_risk
	jrNodes, err := es.NodesByIDs([]int64{res.NodeIDs["join_risk"]})
	if err != nil || len(jrNodes) == 0 {
		t.Fatalf("查 join_risk: %v", err)
	}
	var jrPayload map[string]any
	if err := json.Unmarshal(jrNodes[0].Payload, &jrPayload); err != nil {
		t.Fatalf("解析 join_risk payload: %v", err)
	}
	riskSkipped, ok := jrPayload["skipped_by_design"].([]any)
	if !ok || len(riskSkipped) == 0 {
		t.Fatalf("join_risk.payload.skipped_by_design 应为非空，实际 %v",
			jrPayload["skipped_by_design"])
	}
	if riskSkipped[0].(map[string]any)["role"] != RoleRiskAggressive {
		t.Errorf("join_risk 的 skipped role = %v, want %s",
			riskSkipped[0].(map[string]any)["role"], RoleRiskAggressive)
	}
}

// TestMaterializeRejectsDanglingRef 悬空 Ref 必须报错且**不留任何节点**。
func TestMaterializeRejectsDanglingRef(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { d.Close() })
	expID := mustExploration(t, d)

	g := &StageGraph{
		Nodes: []StageNode{{Ref: "a", Kind: KindIntent, Role: RoleAnalystMarket}},
		Edges: []StageEdge{{From: "a", To: "不存在"}},
	}
	err := withTx(t, d, func(tx *sql.Tx) error {
		_, e := MaterializeGraph(tx, expID, g)
		return e
	})
	if err == nil {
		t.Fatal("悬空 Ref 应报错")
	}

	// 事务回滚 → 库里一个节点都没有
	var n int
	if qErr := d.QueryRow(`SELECT count(*) FROM exploration_nodes
WHERE exploration_id=$1 AND kind IN ('intent','join')`, expID).Scan(&n); qErr != nil {
		t.Fatal(qErr)
	}
	if n != 0 {
		t.Errorf("失败后残留 %d 个节点 —— 事务必须回滚干净", n)
	}
}

// TestMaterializeRejectsNonStageKind 非 stage kind（fact/thesis/signal）必须报错。
func TestMaterializeRejectsNonStageKind(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { d.Close() })
	expID := mustExploration(t, d)

	for _, badKind := range []string{KindFact, KindThesis, KindSignal, KindConclusion} {
		g := &StageGraph{
			Nodes: []StageNode{{Ref: "x", Kind: badKind, Role: RoleResearchManager}},
		}
		err := withTx(t, d, func(tx *sql.Tx) error {
			_, e := MaterializeGraph(tx, expID, g)
			return e
		})
		if err == nil {
			t.Errorf("kind=%q 应被拒绝（产物类型不能当 stage 节点）", badKind)
		}
	}
}

// TestMaterializeRejectsDuplicateRef 重复 Ref 必须报错（Ref 是图内唯一标识）。
func TestMaterializeRejectsDuplicateRef(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { d.Close() })
	expID := mustExploration(t, d)

	g := &StageGraph{
		Nodes: []StageNode{
			{Ref: "dup", Kind: KindIntent, Role: RoleAnalystMarket},
			{Ref: "dup", Kind: KindIntent, Role: RoleAnalystNews},
		},
	}
	err := withTx(t, d, func(tx *sql.Tx) error {
		_, e := MaterializeGraph(tx, expID, g)
		return e
	})
	if err == nil {
		t.Fatal("重复 Ref 应报错")
	}
}

// TestMaterializeRollbackIsAtomic 图建到一半失败时，已插入的节点必须全部回滚。
//
// 构造方式：正常图 + 一条悬空边。节点会先全部插入，然后边校验失败 → 回滚。
// 这是最危险的场景：如果不回滚，库里会有一堆没有依赖关系的孤立节点。
func TestMaterializeRollbackIsAtomic(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { d.Close() })
	expID := mustExploration(t, d)

	g, err := Expand(AssetClassStock, ExpandOpts{InstrumentID: 1})
	if err != nil {
		t.Fatal(err)
	}
	// 加一条悬空边，让它在节点全部插入之后、边阶段失败
	g.Edges = append(g.Edges, StageEdge{From: "analyst_market", To: "ghost_ref"})

	err = withTx(t, d, func(tx *sql.Tx) error {
		_, e := MaterializeGraph(tx, expID, g)
		return e
	})
	if err == nil {
		t.Fatal("悬空边应报错")
	}

	var n, e int
	if qErr := d.QueryRow(`SELECT count(*) FROM exploration_nodes WHERE exploration_id=$1`, expID).Scan(&n); qErr != nil {
		t.Fatal(qErr)
	}
	if qErr := d.QueryRow(`SELECT count(*) FROM exploration_edges WHERE exploration_id=$1`, expID).Scan(&e); qErr != nil {
		t.Fatal(qErr)
	}
	if n != 0 || e != 0 {
		t.Errorf("回滚不干净：残留 %d 节点 / %d 边（应为 0/0）—— "+
			"半成的图比没有图危险：engine 会派发孤立节点", n, e)
	}
}

// TestMaterializeEmptyGraph 空图报错（不是静默成功）。
func TestMaterializeEmptyGraph(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { d.Close() })
	expID := mustExploration(t, d)

	for _, tc := range []struct {
		name string
		g    *StageGraph
	}{
		{"nil 图", nil},
		{"零节点", &StageGraph{}},
	} {
		err := withTx(t, d, func(tx *sql.Tx) error {
			_, e := MaterializeGraph(tx, expID, tc.g)
			return e
		})
		if err == nil {
			t.Errorf("%s 应报错", tc.name)
		}
	}
}

// TestMaterializeUnknownInstrumentRollsBackEverything 最关键的测试：
// instrument_id 指向不存在的标的时，task/exploration/nodes 三张表都必须无残留。
//
// 只断言「返回错误」不够 —— 「任务建了但图没建」这个中间态才是真正危险的：
// engine 会派发一个空图，任务卡住且看不出原因。
func TestMaterializeUnknownInstrumentRollsBackEverything(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { d.Close() })

	ghostID := int64(999999999) // 不存在的标的
	before := countAllRows(t, d)

	_, err := d.CreateTaskWithOptions("幽灵标的任务", "验证回滚", db.TaskCreateOptions{
		InstrumentID: &ghostID,
		MaterializeGraph: func(tx *sql.Tx, expID int64) error {
			// 真实路径里 server 层会先查标的、查不到就 400 返回，不会进到这里。
			// 但 db 层必须自己扛住「调用方传了不存在的标的」这种误用。
			return fmt.Errorf("标的 %d 不存在", ghostID)
		},
	})
	if err == nil {
		t.Fatal("不存在的标的应导致整体失败")
	}

	after := countAllRows(t, d)
	if after != before {
		t.Errorf("回滚不干净：\n  前: %s\n  后: %s\n"+
			"「任务建了但图没建」会让 engine 派发空图、任务卡住且无明确错误",
			before, after)
	}
}

// TestMaterializeNilFuncRejectedWithInstrument 传了 instrument_id 但没传
// MaterializeGraph 必须拒绝（不是静默建空图）。
func TestMaterializeNilFuncRejectedWithInstrument(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { d.Close() })

	instID := int64(1)
	before := countAllRows(t, d)
	_, err := d.CreateTaskWithOptions("缺 materializer", "应被拒绝", db.TaskCreateOptions{
		InstrumentID: &instID,
		// MaterializeGraph 故意不传
	})
	if err == nil {
		t.Fatal("传了 instrument_id 却没 materializer 应被拒绝")
	}
	if !strings.Contains(err.Error(), "MaterializeGraph") {
		t.Errorf("错误信息应指出缺 MaterializeGraph: %v", err)
	}
	if after := countAllRows(t, d); after != before {
		t.Errorf("应无残留：前后 %s vs %s", before, after)
	}
}

// TestCreateTaskWithoutInstrumentUnchanged 渗透任务（不传 instrument_id）行为不变。
func TestCreateTaskWithoutInstrumentUnchanged(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { d.Close() })

	task, err := d.CreateTaskWithOptions("渗透任务", "不建阶段图", db.TaskCreateOptions{})
	if err != nil {
		t.Fatalf("渗透任务应正常创建: %v", err)
	}
	t.Cleanup(func() {
		if err := d.DeleteTask(task.ID); err != nil {
			t.Logf("清理 task %d 失败: %v", task.ID, err)
		}
	})

	// 不该有 stage 节点（只有 origin fact）
	var stageNodes int
	if err := d.QueryRow(`SELECT count(*) FROM exploration_nodes
WHERE exploration_id=$1 AND kind IN ('intent','join')`, task.ExplorationID).Scan(&stageNodes); err != nil {
		t.Fatal(err)
	}
	if stageNodes != 0 {
		t.Errorf("渗透任务不该有 stage 节点，实际 %d 个", stageNodes)
	}
	// instrument_id 应为 NULL
	var instID sql.NullInt64
	if err := d.QueryRow(`SELECT instrument_id FROM tasks WHERE id=$1`, task.ID).Scan(&instID); err != nil {
		t.Fatal(err)
	}
	if instID.Valid {
		t.Errorf("渗透任务的 instrument_id 应为 NULL，实际 %d", instID.Int64)
	}
}

// countAllRows 统计三张关键表的行数（用于回滚断言）。
func countAllRows(t *testing.T, d *db.DB) string {
	t.Helper()
	var tasks, exps, nodes int
	if err := d.QueryRow(`SELECT
  (SELECT count(*) FROM tasks),
  (SELECT count(*) FROM explorations),
  (SELECT count(*) FROM exploration_nodes)`).Scan(&tasks, &exps, &nodes); err != nil {
		t.Fatalf("统计行数: %v", err)
	}
	return fmt.Sprintf("tasks=%d explorations=%d nodes=%d", tasks, exps, nodes)
}

// TestMaterializeWritesOwnerColumn MaterializeGraph 必须把 StageNode.Role 写进 owner 列。
//
// # 为什么这条必须存在（同一个 bug 的第二条写入路径）
//
// `owner` 有两条写入路径：
//
//	① `db.AddNode`          —— 已修（TestAddNodeWritesOwnerFromPayloadRole）
//	② `agent.MaterializeGraph` —— 本测试守护
//
// 修好 ① 不等于修好 ②：**「同一语义多处写入，只有一处写对」是我们反复踩的坑**。
//
// # 漏写的后果是静默的
//
// 阶段图建的节点 owner 恒为 NULL → 任何「按 owner 判断 role」的逻辑对它们
// 永远不成立 → 不报错，只是「本该发生的事没发生」（L16 的失败方式）。
func TestMaterializeWritesOwnerColumn(t *testing.T) {
	d := testDB(t)
	expID := createTestExploration(t, d)
	t.Cleanup(func() {
		if _, err := d.Exec("DELETE FROM explorations WHERE id=$1", expID); err != nil {
			t.Logf("清理: %v", err)
		}
	})

	g, err := Expand(AssetClassCrypto, ExpandOpts{InstrumentID: 1})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}

	tx, err := d.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := MaterializeGraph(tx, expID, g); err != nil {
		_ = tx.Rollback()
		t.Fatalf("MaterializeGraph: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// ① 有 role 的节点，owner 必须等于 role
	var mismatched int
	if err := d.QueryRow(`
SELECT count(*) FROM exploration_nodes
 WHERE exploration_id=$1
   AND payload->>'role' IS NOT NULL
   AND payload->>'role' <> ''
   AND (owner IS NULL OR owner <> payload->>'role')`, expID).Scan(&mismatched); err != nil {
		t.Fatalf("扫描 role/owner 不一致: %v", err)
	}
	if mismatched != 0 {
		t.Errorf("有 %d 个节点的 payload.role 与 owner 列不一致 —— "+
			"这些节点的「按 owner 判断 role」会静默失效", mismatched)
	}

	// ② 抽查一个已知有 role 的节点（analyst_market）
	var owner string
	err = d.QueryRow(`
SELECT COALESCE(owner,'') FROM exploration_nodes
 WHERE exploration_id=$1 AND payload->>'role'='analyst_market' LIMIT 1`, expID).Scan(&owner)
	if err != nil {
		t.Fatalf("查 analyst_market 节点: %v", err)
	}
	if owner != "analyst_market" {
		t.Errorf("analyst_market 节点的 owner = %q, want \"analyst_market\"", owner)
	}
}
