package server

// 阶段图落库编排的验收测试（W3-10）。
//
// # 测的是「编排层」，不是「建图」
//
// `agent.MaterializeGraph` 的 10 个测试已覆盖「建 nodes+edges」本身
// （含原子性 TestMaterializeRollbackIsAtomic）。
// 本文件测的是它之上的**幂等判断**与**版本标记**。
//
// # 为什么幂等值得单独测
//
// 因为幂等的判据不在建图逻辑里 —— 它是「这个 exploration 是否已应用过该版本」。
// 而那个判据错了，表现为「重复建了第二套图」（节点翻倍），
// 或者「模板升级后永远不重建」（版本判断写错）。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// stageGraphEnv 建一个带根 fact 的 exploration，用于落图。
type stageGraphEnv struct {
	t     *testing.T
	d     *db.DB
	expID int64
	es    *db.ExplorationStore
}

// newStageGraphEnv 建 exploration + 根 fact（origin），这是 ApplyStageGraph 的幂等锚点。
func newStageGraphEnv(t *testing.T, tag string) *stageGraphEnv {
	t.Helper()
	d, err := db.Open(mustDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	t.Cleanup(func() { d.Close() })

	expID, err := d.CreateExploration(tag, "阶段图落库测试")
	if err != nil {
		t.Fatalf("CreateExploration: %v", err)
	}
	t.Cleanup(func() {
		if _, e := d.Exec(`DELETE FROM explorations WHERE id=$1`, expID); e != nil {
			t.Errorf("清理 exploration %d: %v", expID, e)
		}
	})

	es := d.Exploration(expID)

	// 根 fact（origin）—— ApplyStageGraph 把版本记在它上面。
	if _, err := es.AddNode(db.KindFact, map[string]any{
		"text": "任务根节点",
	}, 0, db.StateOrigin, "system", nil); err != nil {
		t.Fatalf("建根 fact: %v", err)
	}

	return &stageGraphEnv{t: t, d: d, expID: expID, es: es}
}

// inTx 在一个事务里执行 fn，出错则回滚。
func (e *stageGraphEnv) inTx(fn func(tx *sql.Tx) error) error {
	e.t.Helper()
	tx, err := e.d.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// nodeCount 数该 exploration 的节点数。
func (e *stageGraphEnv) nodeCount() int {
	e.t.Helper()
	var n int
	if err := e.d.QueryRow(`SELECT count(*) FROM exploration_nodes WHERE exploration_id=$1`,
		e.expID).Scan(&n); err != nil {
		e.t.Fatalf("数节点: %v", err)
	}
	return n
}

// edgeCount 数 gates 边。
func (e *stageGraphEnv) edgeCount() int {
	e.t.Helper()
	var n int
	if err := e.d.QueryRow(`SELECT count(*) FROM exploration_edges WHERE exploration_id=$1 AND rel='gates'`,
		e.expID).Scan(&n); err != nil {
		e.t.Fatalf("数边: %v", err)
	}
	return n
}

// appliedVersion 读根 fact 上的版本标记。
func (e *stageGraphEnv) appliedVersion() string {
	e.t.Helper()
	var v string
	if err := e.d.QueryRow(`
SELECT COALESCE(json_extract(payload,'$.stage_graph_applied'),'') FROM exploration_nodes
 WHERE exploration_id=$1 AND kind='fact' AND state='origin' LIMIT 1`, e.expID).Scan(&v); err != nil {
		e.t.Fatalf("读版本: %v", err)
	}
	return v
}

// ---- ① 建出 stock 图，节点/边/映射都对 ----

func TestApplyStageGraphStock(t *testing.T) {
	e := newStageGraphEnv(t, "apply-stock")

	g, err := agent.Expand(agent.AssetClassStock, agent.ExpandOpts{InstrumentID: 1})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}

	var refs map[string]int64
	var applied bool
	if err := e.inTx(func(tx *sql.Tx) error {
		var aErr error
		refs, applied, aErr = ApplyStageGraph(tx, e.expID, g)
		return aErr
	}); err != nil {
		t.Fatalf("ApplyStageGraph: %v", err)
	}

	if !applied {
		t.Error("首次应用应 applied=true")
	}
	// Ref 映射覆盖全部节点
	if len(refs) != len(g.Nodes) {
		t.Errorf("refs 数 = %d, want %d（每个 StageNode 都要有映射）", len(refs), len(g.Nodes))
	}
	// 库里的节点数 = 1（根 fact）+ len(g.Nodes)
	if got, want := e.nodeCount(), 1+len(g.Nodes); got != want {
		t.Errorf("节点数 = %d, want %d（1 根 fact + %d 阶段节点）", got, want, len(g.Nodes))
	}
	if got, want := e.edgeCount(), len(g.Edges); got != want {
		t.Errorf("gates 边数 = %d, want %d", got, want)
	}
	// 版本标记已写
	if v := e.appliedVersion(); v != StageGraphVersion {
		t.Errorf("applied 版本 = %q, want %q", v, StageGraphVersion)
	}
}

// ---- ② 幂等：连续两次 → 第二次 applied=false，节点数不变 ----

func TestApplyStageGraphIdempotent(t *testing.T) {
	e := newStageGraphEnv(t, "apply-idempotent")

	g, _ := agent.Expand(agent.AssetClassStock, agent.ExpandOpts{InstrumentID: 1})

	// 第一次
	if err := e.inTx(func(tx *sql.Tx) error {
		_, _, err := ApplyStageGraph(tx, e.expID, g)
		return err
	}); err != nil {
		t.Fatalf("第一次: %v", err)
	}
	nodesAfterFirst := e.nodeCount()
	edgesAfterFirst := e.edgeCount()

	// 第二次
	var applied bool
	var refs map[string]int64
	if err := e.inTx(func(tx *sql.Tx) error {
		var aErr error
		refs, applied, aErr = ApplyStageGraph(tx, e.expID, g)
		return aErr
	}); err != nil {
		t.Fatalf("第二次: %v", err)
	}

	if applied {
		t.Error("第二次应 applied=false（已建过同版本）")
	}
	if refs != nil {
		t.Errorf("no-op 时 refs 应为 nil，实际 len=%d", len(refs))
	}
	if got := e.nodeCount(); got != nodesAfterFirst {
		t.Errorf("no-op 后节点数变了：%d → %d（幂等失效，重复建了图）", nodesAfterFirst, got)
	}
	if got := e.edgeCount(); got != edgesAfterFirst {
		t.Errorf("no-op 后边数变了：%d → %d", edgesAfterFirst, got)
	}
}

// ---- ③ 幂等键在版本上：改版本后重建 ----

func TestApplyStageGraphRebuildsOnVersionChange(t *testing.T) {
	e := newStageGraphEnv(t, "apply-version")

	g, _ := agent.Expand(agent.AssetClassCrypto, agent.ExpandOpts{InstrumentID: 1})

	if err := e.inTx(func(tx *sql.Tx) error {
		_, _, err := ApplyStageGraph(tx, e.expID, g)
		return err
	}); err != nil {
		t.Fatalf("首次: %v", err)
	}

	// 手工把版本改成 v0（模拟「旧版本建的图」）
	if _, err := e.d.Exec(`
UPDATE exploration_nodes
   SET payload = json_patch(COALESCE(payload,'{}'), '{"stage_graph_applied":"v0"}')
 WHERE exploration_id=$1 AND kind='fact' AND state='origin'`, e.expID); err != nil {
		t.Fatalf("改版本: %v", err)
	}

	var applied bool
	if err := e.inTx(func(tx *sql.Tx) error {
		var aErr error
		_, applied, aErr = ApplyStageGraph(tx, e.expID, g)
		return aErr
	}); err != nil {
		t.Fatalf("v0 时重建: %v", err)
	}
	if !applied {
		t.Error("版本不同时应 applied=true（重建）—— 否则模板升级后永远用旧图")
	}
}

// ---- ④ refs 可用于后续 Link（From / To 都能查到）----

func TestApplyStageGraphRefsUsable(t *testing.T) {
	e := newStageGraphEnv(t, "apply-refs")

	g, _ := agent.Expand(agent.AssetClassStock, agent.ExpandOpts{InstrumentID: 1})

	var refs map[string]int64
	if err := e.inTx(func(tx *sql.Tx) error {
		var aErr error
		refs, _, aErr = ApplyStageGraph(tx, e.expID, g)
		return aErr
	}); err != nil {
		t.Fatalf("ApplyStageGraph: %v", err)
	}

	// 每条边的两端都必须能在 refs 里查到
	for _, edge := range g.Edges {
		if _, ok := refs[edge.From]; !ok {
			t.Errorf("边 %s→%s 的 From ref 不在映射里", edge.From, edge.To)
		}
		if _, ok := refs[edge.To]; !ok {
			t.Errorf("边 %s→%s 的 To ref 不在映射里", edge.From, edge.To)
		}
	}

	// 抽查：join_analysts 存在且 id > 0
	if id, ok := refs["join_analysts"]; !ok || id <= 0 {
		t.Errorf("refs[join_analysts] = %d, ok=%v", id, ok)
	}
}

// ---- ⑤ 空图 → 报错（不是「成功但建 0 个」）----

func TestApplyStageGraphRejectsEmpty(t *testing.T) {
	e := newStageGraphEnv(t, "apply-empty")

	for _, g := range []*agent.StageGraph{
		nil,
		{Nodes: nil, Edges: nil},
		{Nodes: []agent.StageNode{}, Edges: []agent.StageEdge{}},
	} {
		err := e.inTx(func(tx *sql.Tx) error {
			_, _, aErr := ApplyStageGraph(tx, e.expID, g)
			return aErr
		})
		if err == nil {
			t.Errorf("空图应报错（g=%+v），否则会留下「applied 了但图是空的」状态", g)
			continue
		}
		if !errors.Is(err, ErrEmptyStageGraph) {
			t.Errorf("错误应为 ErrEmptyStageGraph，实际 %v", err)
		}
	}

	// 且没有写版本标记（失败后不该有 applied）
	if v := e.appliedVersion(); v != "" {
		t.Errorf("空图失败后不该写版本标记，实际 %q", v)
	}
}

// ---- ⑥ 原子性：建图失败 → 一个节点都不留 ----

func TestApplyStageGraphAtomicOnFailure(t *testing.T) {
	e := newStageGraphEnv(t, "apply-atomic")

	before := e.nodeCount()

	// 造一个「边指向不存在的 Ref」的图 —— MaterializeGraph 会在建完节点后校验边时失败。
	g := &agent.StageGraph{
		Nodes: []agent.StageNode{
			{Ref: "a", Kind: db.KindIntent, Role: "analyst_market", Priority: 10},
		},
		Edges: []agent.StageEdge{{From: "a", To: "ghost_ref"}},
	}

	err := e.inTx(func(tx *sql.Tx) error {
		_, _, aErr := ApplyStageGraph(tx, e.expID, g)
		return aErr
	})
	if err == nil {
		t.Fatal("悬空 Ref 应报错")
	}

	// 关键：节点 'a' 也不该存在（整个事务回滚）
	if got := e.nodeCount(); got != before {
		t.Errorf("失败后节点数 = %d, want %d（事务没回滚，留下了半成品）", got, before)
	}
	if v := e.appliedVersion(); v != "" {
		t.Errorf("失败后不该写版本标记，实际 %q", v)
	}
}

// ---- ⑦ edges 全部 rel='gates' ----

func TestApplyStageGraphEdgesAreGates(t *testing.T) {
	e := newStageGraphEnv(t, "apply-gates")

	g, _ := agent.Expand(agent.AssetClassCrypto, agent.ExpandOpts{InstrumentID: 1})
	if err := e.inTx(func(tx *sql.Tx) error {
		_, _, err := ApplyStageGraph(tx, e.expID, g)
		return err
	}); err != nil {
		t.Fatalf("ApplyStageGraph: %v", err)
	}

	// 非 gates 的边数必须为 0
	var others int
	if err := e.d.QueryRow(`
SELECT count(*) FROM exploration_edges
 WHERE exploration_id=$1 AND rel <> 'gates'`, e.expID).Scan(&others); err != nil {
		t.Fatalf("数非 gates 边: %v", err)
	}
	if others != 0 {
		t.Errorf("阶段图不该产生非 gates 边，实际 %d 条", others)
	}
}

// ---- ⑧ 失败后 exploration 仍可用（能再次成功建图）----

func TestApplyStageGraphRecoverableAfterFailure(t *testing.T) {
	e := newStageGraphEnv(t, "apply-recover")

	// 先失败一次
	bad := &agent.StageGraph{
		Nodes: []agent.StageNode{{Ref: "x", Kind: db.KindIntent, Role: "analyst_market", Priority: 10}},
		Edges: []agent.StageEdge{{From: "x", To: "ghost"}},
	}
	if err := e.inTx(func(tx *sql.Tx) error {
		_, _, err := ApplyStageGraph(tx, e.expID, bad)
		return err
	}); err == nil {
		t.Fatal("应先失败")
	}

	// 再用正常图建一次 —— 必须成功
	g, _ := agent.Expand(agent.AssetClassStock, agent.ExpandOpts{InstrumentID: 1})
	if err := e.inTx(func(tx *sql.Tx) error {
		_, _, err := ApplyStageGraph(tx, e.expID, g)
		return err
	}); err != nil {
		t.Fatalf("失败后应能重新建图: %v", err)
	}

	if got, want := e.nodeCount(), 1+len(g.Nodes); got != want {
		t.Errorf("重建后节点数 = %d, want %d", got, want)
	}
}

// ---- 辅助 ----

// mustDSN 取测试库路径（与其他 server 测试一致）。
//
// SQLite 没有连接串；db.Open 接受文件路径。TestMain 保证已有库，
// 所以这里不再有「没配置就 skip」。
func mustDSN(t *testing.T) string {
	t.Helper()
	return testDBPath(t)
}

// TestMaterializeDownstreamWritesRole 锁住 2026-11-08 修的回归：
// 选品后补建下游阶段时，role 必须同时进 payload 和 owner 列。
// 漏了它，worker 会把所有金融阶段当通用 worker 跑（角色 prompt/工具全丢，
// 表现为 list_facts / emit_fact 报 unknown tool）。
func TestMaterializeDownstreamWritesRole(t *testing.T) {
	e := newStageGraphEnv(t, "downstream-role")
	// 比 newStageGraphEnv 的 cleanup 晚注册 → LIFO 先执行，先删子节点再删父。
	t.Cleanup(func() {
		_, _ = e.d.Exec(`DELETE FROM exploration_edges WHERE exploration_id=$1`, e.expID)
		_, _ = e.d.Exec(`DELETE FROM exploration_nodes WHERE exploration_id=$1`, e.expID)
	})

	created, err := MaterializeDownstream(context.Background(), e.d, e.expID, 1, "stock")
	if err != nil {
		t.Fatalf("补建下游阶段失败: %v", err)
	}
	if created == 0 {
		t.Fatal("应补建下游意图")
	}

	rows, err := e.d.Query(`SELECT COALESCE(json_extract(payload,'$.role'),''), COALESCE(owner,'')
FROM exploration_nodes WHERE exploration_id=$1 AND kind='intent'`, e.expID)
	if err != nil {
		t.Fatalf("查意图: %v", err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var role, owner string
		if err := rows.Scan(&role, &owner); err != nil {
			t.Fatalf("扫描: %v", err)
		}
		if role == "" {
			t.Error("补建的意图缺 payload.role")
			continue
		}
		if role != owner {
			t.Errorf("role=%q 与 owner=%q 不一致", role, owner)
		}
		seen[role] = true
	}
	for _, want := range []string{
		"analyst_market", "analyst_news", "analyst_fundamental",
		"trader", "portfolio_manager",
	} {
		if !seen[want] {
			t.Errorf("缺少角色 %q 的意图", want)
		}
	}

	// gates 边 + join 节点必须一起落库，否则所有阶段同时 open、顺序失效。
	wantGraph, _ := agent.Expand(agent.AssetClassStock, agent.ExpandOpts{InstrumentID: 1})
	var gates, joinCount int
	if err := e.d.QueryRow(`SELECT COUNT(*) FROM exploration_edges
WHERE exploration_id=$1 AND rel='gates'`, e.expID).Scan(&gates); err != nil {
		t.Fatalf("数 gates 边: %v", err)
	}
	if err := e.d.QueryRow(`SELECT COUNT(*) FROM exploration_nodes
WHERE exploration_id=$1 AND kind='join'`, e.expID).Scan(&joinCount); err != nil {
		t.Fatalf("数 join 节点: %v", err)
	}
	if gates != len(wantGraph.Edges) {
		t.Errorf("gates 边 = %d, want %d", gates, len(wantGraph.Edges))
	}
	if joinCount != 2 {
		t.Errorf("join 节点 = %d, want 2", joinCount)
	}

	// Frontier 在上游未完成时只能放行 3 个分析师，下游必须被 gates 挡住。
	frontier, err := e.es.Frontier(20)
	if err != nil {
		t.Fatalf("Frontier: %v", err)
	}
	if len(frontier) != 3 {
		t.Errorf("首轮可领取意图 = %d, want 3（仅分析师）", len(frontier))
	}
	for _, n := range frontier {
		var p struct {
			Role string `json:"role"`
		}
		_ = json.Unmarshal(n.Payload, &p)
		if !strings.HasPrefix(p.Role, "analyst_") {
			t.Errorf("上游未完成时 %q 不该进入 frontier", p.Role)
		}
	}
}
