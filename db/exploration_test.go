package db

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestExplorationFlow(t *testing.T) {
	d := testOpen(t)

	expID, err := d.CreateExploration("test", "拿下测试目标")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID) // cascades nodes/edges/activity
	es := d.Exploration(expID)

	// goal node + two intents
	goal, err := es.AddGoal(map[string]any{"text": "getadmin", "vulnclass": "authz"}, "human")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := es.AddIntent(map[string]any{"summary": "enumerate endpoints"}, 5, nil, "planner"); err != nil {
		t.Fatal(err)
	}
	i2, err := es.AddIntent(map[string]any{"summary": "test idor"}, 8, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}

	// frontier ordered by priority desc → i2(8) before i1(5)
	fr, err := es.Frontier(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(fr) != 2 || fr[0].ID != i2 {
		t.Fatalf("frontier order wrong: %+v", fr)
	}

	// atomic claim: first wins, second on same id fails
	ok, err := es.ClaimIntent(i2, "worker-1")
	if err != nil || !ok {
		t.Fatalf("claim i2: ok=%v err=%v", ok, err)
	}
	ok2, _ := es.ClaimIntent(i2, "worker-2")
	if ok2 {
		t.Fatalf("double-claim should fail")
	}

	// finding yields from intent, proves goal
	find, err := es.AddNode("finding", map[string]any{"vulnclass": "idor", "severity": "high"}, 9, "confirmed", "worker-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := es.Link(i2, "yields", find); err != nil {
		t.Fatal(err)
	}
	if err := es.Link(find, "proves", goal); err != nil {
		t.Fatal(err)
	}
	if err := es.SetNodeState(goal, "met"); err != nil {
		t.Fatal(err)
	}

	// lineage: ancestors of the finding traced backward — here {i2, find} joined by
	// the yields edge. The proves→goal edge is DOWNSTREAM (goal must be excluded),
	// and the unrelated intent i1 is not on any path to the finding (excluded too).
	lnNodes, lnEdges, err := es.FindingLineage(find)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]bool{}
	for _, n := range lnNodes {
		got[n.ID] = true
	}
	if len(lnNodes) != 2 || !got[find] || !got[i2] {
		t.Fatalf("lineage nodes: want {i2,find}, got %+v", lnNodes)
	}
	if got[goal] {
		t.Fatalf("lineage must exclude the proved goal (it is downstream of the finding)")
	}
	if len(lnEdges) != 1 || lnEdges[0].From != i2 || lnEdges[0].To != find || lnEdges[0].Rel != "yields" {
		t.Fatalf("lineage edges: want i2-yields->find, got %+v", lnEdges)
	}

	// activity poll by id cursor
	id1, err := es.AppendActivity(Activity{Worker: "worker-1", Kind: "tool_use", Tool: "Bash", Summary: "ran curl", Detail: "full output"})
	if err != nil {
		t.Fatal(err)
	}
	items, cursor, err := es.ActivityList(nil, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || cursor != id1 {
		t.Fatalf("activity list: items=%d cursor=%d", len(items), cursor)
	}
	det, _ := es.ActivityDetail(id1)
	if det != "full output" {
		t.Fatalf("detail want 'full output', got %q", det)
	}
	// incremental: nothing new after cursor
	items2, _, _ := es.ActivityList(nil, cursor, 100)
	if len(items2) != 0 {
		t.Fatalf("incremental poll should be empty, got %d", len(items2))
	}

	// stats
	st, _ := es.Stats()
	if st["intent"] != 2 || st["goal"] != 1 || st["finding"] != 1 {
		t.Fatalf("stats: %+v", st)
	}
}

func TestIntentPauseResumeAndCancelCleanup(t *testing.T) {
	d := testOpen(t)

	expID, err := d.CreateExploration("test", "worker control cleanup")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	es := d.Exploration(expID)

	assetID, err := d.Assets().UpsertRootDomain(UpsertRootDomainReq{
		Domain: fmt.Sprintf("cancel-intent-%d.invalid", expID),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer deleteAsset(d, assetID)

	otherIntent, err := es.AddIntent(map[string]any{"summary": "keep intent"}, 1, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	intentID, err := es.AddIntent(map[string]any{"summary": "cancel intent"}, 10, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := es.ClaimIntent(intentID, "worker-control-test"); err != nil || !claimed {
		t.Fatalf("initial claim: claimed=%v err=%v", claimed, err)
	}
	if err := es.SetIntentState(intentID, "paused"); err != nil {
		t.Fatal(err)
	}

	frontier, err := es.Frontier(10)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range frontier {
		if node.ID == intentID {
			t.Fatalf("paused intent %d must not enter frontier", intentID)
		}
	}
	if claimed, err := es.ClaimIntent(intentID, "worker-while-paused"); err != nil || claimed {
		t.Fatalf("paused intent claim: claimed=%v err=%v", claimed, err)
	}
	if err := es.SetIntentState(intentID, "open"); err != nil {
		t.Fatal(err)
	}
	if claimed, err := es.ClaimIntent(intentID, "worker-after-resume"); err != nil || !claimed {
		t.Fatalf("resumed intent claim: claimed=%v err=%v", claimed, err)
	}
	if err := es.SetIntentState(intentID, "paused"); err != nil {
		t.Fatal(err)
	}

	directFact, err := es.AddNode("fact", map[string]any{"summary": "remove fact"}, 0, "confirmed", "worker", []int64{assetID})
	if err != nil {
		t.Fatal(err)
	}
	directFinding, err := es.AddNode("finding", map[string]any{"summary": "remove finding"}, 0, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	keptFact, err := es.AddNode("fact", map[string]any{"summary": "keep fact"}, 0, "confirmed", "other-worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := es.Link(intentID, RelYields, directFact); err != nil {
		t.Fatal(err)
	}
	if err := es.Link(intentID, RelYields, directFinding); err != nil {
		t.Fatal(err)
	}
	findingRowID, err := es.AddStandaloneFinding(0, directFinding, "test", "cancelled finding", SeverityHigh, "summary", "evidence", "worker", []int64{assetID})
	if err != nil {
		t.Fatal(err)
	}
	activityID, err := es.AppendActivity(Activity{NodeID: &intentID, Worker: "worker", Kind: "result", Summary: "remove activity"})
	if err != nil {
		t.Fatal(err)
	}
	keptActivityID, err := es.AppendActivity(Activity{NodeID: &otherIntent, Worker: "other-worker", Kind: "result", Summary: "keep activity"})
	if err != nil {
		t.Fatal(err)
	}

	cleanup, err := es.CancelIntent(intentID)
	if err != nil {
		t.Fatal(err)
	}
	if cleanup.Intents != 1 || cleanup.Facts != 1 || cleanup.Findings != 1 || cleanup.Activities != 1 {
		t.Fatalf("unexpected cleanup counts: %+v", cleanup)
	}
	for _, nodeID := range []int64{intentID, directFact, directFinding} {
		node, err := es.GetNode(nodeID)
		if err != nil {
			t.Fatal(err)
		}
		if node != nil {
			t.Fatalf("node %d survived intent cancellation", nodeID)
		}
	}
	for _, nodeID := range []int64{otherIntent, keptFact} {
		node, err := es.GetNode(nodeID)
		if err != nil || node == nil {
			t.Fatalf("unrelated node %d removed: node=%v err=%v", nodeID, node, err)
		}
	}

	assertCount := func(query string, want int, args ...any) {
		t.Helper()
		var got int
		if err := d.QueryRow(query, args...).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("query count=%d, want %d: %s", got, want, query)
		}
	}
	assertCount(`SELECT COUNT(*) FROM findings WHERE id=$1`, 0, findingRowID)
	assertCount(`SELECT COUNT(*) FROM activity WHERE id=$1`, 0, activityID)
	assertCount(`SELECT COUNT(*) FROM activity WHERE id=$1`, 1, keptActivityID)
	assertCount(`SELECT COUNT(*) FROM exploration_edges WHERE exploration_id=$1 AND (src_id=$2 OR dst_id=$2)`, 0, expID, intentID)
	assertCount(`SELECT COUNT(*) FROM exploration_anchors WHERE node_id=$1`, 0, directFact)
	assertCount(`SELECT COUNT(*) FROM assets WHERE id=$1`, 1, assetID)
}

// TestNodesPageQueryMatchesID verifies the 播报板 search filters on node id (both
// the bare number and the「#id」form the UI shows) in addition to payload/origin.
func TestNodesPageQueryMatchesID(t *testing.T) {
	d := testOpen(t)

	expID, err := d.CreateExploration("test", "id 搜索")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	es := d.Exploration(expID)

	target, err := es.AddNode("fact", map[string]any{"summary": "needle-alpha"}, 0, "confirmed", "worker-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	other, err := es.AddNode("fact", map[string]any{"summary": "unrelated-beta"}, 0, "confirmed", "worker-b", nil)
	if err != nil {
		t.Fatal(err)
	}

	onlyTarget := func(label, q string) {
		t.Helper()
		nodes, total, err := es.NodesPage(NodeFilter{Query: q}, 1, 50)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if total != 1 || len(nodes) != 1 || nodes[0].ID != target {
			t.Fatalf("%s: q=%q total=%d nodes=%+v, want single node %d", label, q, total, nodes, target)
		}
	}

	onlyTarget("bare id", fmt.Sprint(target))
	onlyTarget("hash id", "#"+fmt.Sprint(target))
	onlyTarget("payload still works", "needle-alpha")

	// A non-matching numeric id returns nothing (and does not accidentally match other).
	if nodes, total, err := es.NodesPage(NodeFilter{Query: fmt.Sprint(target + other + 1000)}, 1, 50); err != nil {
		t.Fatal(err)
	} else if total != 0 || len(nodes) != 0 {
		t.Fatalf("non-existent id: total=%d nodes=%+v, want empty", total, nodes)
	}
}

// TestAddNodeWritesOwnerFromPayloadRole AddNode 必须把 payload.role 写进 owner 列。
//
// # 这是一个真 bug 的回归测试
//
// `LatestSibling` / `LatestSiblings` 按 `owner = ANY($n)` 过滤。而 AddNode
// 早期不写 owner —— 导致：
//
//	PM 读三方风控结论   → 永远 found=false
//	debate 读对手论点   → 永远 found=false
//	research_manager 裁决全部辩论轮次 → 永远 found=false
//
// 全部静默失效（返回 found=false，不报错），而 agent/pipeline.go 的节点链
// 正依赖这个能力。
func TestAddNodeWritesOwnerFromPayloadRole(t *testing.T) {
	d := testOpen(t)

	expID, err := d.CreateExploration("test", "owner 回归测试")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	es := d.Exploration(expID)

	// 带 role 的节点 → owner 必须等于 role
	id, err := es.AddNode(KindThesis, map[string]any{
		"role":   "debate_bull",
		"thesis": "第一轮论点",
	}, 10, "standing", "agent", nil)
	if err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	var owner string
	var isNull bool
	if err := d.QueryRow(`SELECT COALESCE(owner,''), owner IS NULL FROM exploration_nodes WHERE id=$1`, id).
		Scan(&owner, &isNull); err != nil {
		t.Fatalf("读 owner: %v", err)
	}
	if isNull || owner != "debate_bull" {
		t.Errorf("owner 应等于 payload.role（debate_bull），实际 owner=%q isNull=%v", owner, isNull)
	}

	// 不带 role 的节点 → owner 应为 NULL（不是空串）
	id2, err := es.AddNode(KindFact, map[string]any{
		"summary": "系统写入的 fact，没有 role",
	}, 10, "confirmed", "system", nil) // fact 的合法状态是 confirmed/dismissed/origin
	if err != nil {
		t.Fatalf("AddNode(fact): %v", err)
	}
	var isNull2 bool
	if err := d.QueryRow(`SELECT owner IS NULL FROM exploration_nodes WHERE id=$1`, id2).Scan(&isNull2); err != nil {
		t.Fatalf("读 fact owner: %v", err)
	}
	if !isNull2 {
		t.Error("没有 role 的节点 owner 应为 NULL（不是空串）")
	}
}

// TestNoNodeMissingOwnerWhenPayloadHasRole 全局一致性断言。
//
// # 这条守的是「将来某次改动把 owner 写入删掉」
//
// 单条 AddNode 的测试能发现「这次改坏了」，但发现不了「有人加了另一条
// 建节点的路径却忘了写 owner」。
//
// 这条扫描全表：凡是 payload 里有 role 的节点，owner 都不能是 NULL。
func TestNoNodeMissingOwnerWhenPayloadHasRole(t *testing.T) {
	d := testOpen(t)

	// 先建几个带 role 的节点（保证表里至少有数据可查）
	expID, err := d.CreateExploration("test", "owner 一致性")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	es := d.Exploration(expID)

	for _, role := range []string{"analyst_market", "debate_bear", "risk_neutral"} {
		if _, err := es.AddNode(KindThesis, map[string]any{"role": role}, 10, "standing", "agent", nil); err != nil {
			t.Fatalf("AddNode(%s): %v", role, err)
		}
	}

	var bad int
	if err := d.QueryRow(`
SELECT count(*) FROM exploration_nodes
WHERE owner IS NULL AND payload->>'role' IS NOT NULL`).Scan(&bad); err != nil {
		t.Fatalf("扫描: %v", err)
	}
	if bad != 0 {
		t.Errorf("有 %d 个节点的 payload 带 role 但 owner 为 NULL —— "+
			"这些节点的 sibling 读取会永远返回空（静默失效）", bad)
	}
}

// TestClaimIntentKeepsSemanticOwner 认领不得抹掉 intent 的产出角色。
//
// # 这条守的是什么
//
// owner 列承载两件事：节点**产出角色**（触发器从 payload.role 派生，金融阶段图
// intent 靠它定位 portfolio_manager 等）与「谁在跑」（work#N 槽位）。
// ClaimIntent 早期无条件 `SET owner=$1`，语义角色在 claim 那一刻永久丢失 ——
// server/engine.go 的 `intent.Owner == agent.RolePortfolioManager` 判据
// 因此在真实运行中永不成立，PM 评级 → 交易草案整条链路静默失效。
//
// 所以断言两件事：有角色时保留角色，无角色时仍落槽名（渗透侧不回归）。
func TestClaimIntentKeepsSemanticOwner(t *testing.T) {
	d := testOpen(t)

	expID, err := d.CreateExploration("test", "claim 保留语义角色")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	es := d.Exploration(expID)

	// 金融阶段图 intent：payload 带 role，触发器派生 owner。
	withRole, err := es.AddIntent(map[string]any{
		"summary": "综合三方风控出评级", "role": "portfolio_manager",
	}, 5, nil, "system")
	if err != nil {
		t.Fatalf("AddIntent(带 role): %v", err)
	}
	// 渗透 intent：payload 只有 summary —— owner 为 NULL。
	plain, err := es.AddIntent(map[string]any{"summary": "test idor"}, 5, nil, "planner")
	if err != nil {
		t.Fatalf("AddIntent(无 role): %v", err)
	}

	ok, err := es.ClaimIntent(withRole, "work#1")
	if err != nil || !ok {
		t.Fatalf("claim 带 role 的意图: ok=%v err=%v", ok, err)
	}
	n, err := es.GetNode(withRole)
	if err != nil || n == nil {
		t.Fatalf("读回意图 %d: %v", withRole, err)
	}
	if n.Owner != "portfolio_manager" {
		t.Errorf("claim 后 owner = %q，期望 portfolio_manager —— "+
			"语义角色被 work#N 槽名覆写了，engine.go 的 PM 判据会永久失效", n.Owner)
	}

	ok, err = es.ClaimIntent(plain, "work#2")
	if err != nil || !ok {
		t.Fatalf("claim 无 role 的意图: ok=%v err=%v", ok, err)
	}
	n, err = es.GetNode(plain)
	if err != nil || n == nil {
		t.Fatalf("读回意图 %d: %v", plain, err)
	}
	if n.Owner != "work#2" {
		t.Errorf("无 role 的意图 claim 后 owner = %q，期望 work#2（槽名兜底）", n.Owner)
	}

	// 认领守卫未受影响：同一 id 二次 claim 仍失败（state 已 running）。
	if again, _ := es.ClaimIntent(withRole, "work#3"); again {
		t.Error("同一意图被二次认领 —— state='open' 守卫失效")
	}
}

// TestJoinBecomesBlockedWhenUpstreamBlocked 上游失败时 join 必须转 blocked（不是 done）。
//
// # 这条守的是 ARCH-W2-W5.md:4641
//
//	「join 的任一 await 处于 blocked/exhausted/stopped → join 自身转 blocked」
//
// 以及 :899 的完整路径：
//
//	「SOURCE_UNAVAILABLE → intent blocked → join 节点因上游非 done 而转 blocked
//	 → 任务终态 blocked → 前端显式显示数据源故障」
//
// # 为什么这个语义重要
//
// 如果 join 置 done，下游看到 join=done 会**继续执行** ——
// 一个分析师跑挂了，下游辩论照样开跑，只是从 payload 里读到「某维度缺失」。
//
// 而设计意图是「上游失败 → 整条分支不跑」：数据不全时不该产出结论。
func TestJoinBecomesBlockedWhenUpstreamBlocked(t *testing.T) {
	d := testOpen(t)

	expID, err := d.CreateExploration("test", "join blocked 传播")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	es := d.Exploration(expID)

	// 两个上游：一个 done，一个 blocked。
	up1, err := es.AddNode(KindIntent, map[string]any{"role": "analyst_market"}, 10, "done", "agent", nil)
	if err != nil {
		t.Fatalf("AddNode up1: %v", err)
	}
	up2, err := es.AddNode(KindIntent, map[string]any{"role": "analyst_news"}, 10, "blocked", "agent", nil)
	if err != nil {
		t.Fatalf("AddNode up2: %v", err)
	}

	join, err := es.AddNode("join", map[string]any{}, 20, "open", "system", nil)
	if err != nil {
		t.Fatalf("AddNode join: %v", err)
	}
	if err := es.Link(up1, "gates", join); err != nil {
		t.Fatalf("Link up1: %v", err)
	}
	if err := es.Link(up2, "gates", join); err != nil {
		t.Fatalf("Link up2: %v", err)
	}

	res, err := es.ReconcileJoins()
	if err != nil {
		t.Fatalf("ReconcileJoins: %v", err)
	}
	if res.Blocked != 1 {
		t.Errorf("应有 1 个 join 转 blocked，实际 Blocked=%d（Satisfied=%d）", res.Blocked, res.Satisfied)
	}
	if res.Satisfied != 0 {
		t.Errorf("有失败上游时不该有 join 被置 done，实际 Satisfied=%d", res.Satisfied)
	}

	// 关键断言：join 的状态是 blocked，不是 done。
	var state, reason string
	if err := d.QueryRow(`SELECT state, COALESCE(blocked_reason,'') FROM exploration_nodes WHERE id=$1`, join).
		Scan(&state, &reason); err != nil {
		t.Fatalf("读 join 状态: %v", err)
	}
	if state != "blocked" {
		t.Fatalf("join 状态 = %q, want \"blocked\"（上游 analyst_news 是 blocked）", state)
	}
	// blocked_reason 必须指出是哪个上游
	if !strings.Contains(reason, "analyst_news") {
		t.Errorf("blocked_reason 应指出失败的 role（analyst_news），实际 %q", reason)
	}
}

// TestJoinDoneWhenAllUpstreamsDone 全部上游 done 时正常兑现。
//
// 与上一条对照：同样是「全部上游已结束」，结果取决于「有没有失败的」。
func TestJoinDoneWhenAllUpstreamsDone(t *testing.T) {
	d := testOpen(t)

	expID, err := d.CreateExploration("test", "join done 正常路径")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	es := d.Exploration(expID)

	up1, _ := es.AddNode(KindIntent, map[string]any{"role": "analyst_market"}, 10, "done", "agent", nil)
	up2, _ := es.AddNode(KindIntent, map[string]any{"role": "analyst_sentiment_crypto"}, 10, "done", "agent", nil)
	join, _ := es.AddNode("join", map[string]any{}, 20, "open", "system", nil)
	_ = es.Link(up1, "gates", join)
	_ = es.Link(up2, "gates", join)

	res, err := es.ReconcileJoins()
	if err != nil {
		t.Fatalf("ReconcileJoins: %v", err)
	}
	if res.Satisfied != 1 || res.Blocked != 0 {
		t.Errorf("应 Satisfied=1 Blocked=0，实际 Satisfied=%d Blocked=%d", res.Satisfied, res.Blocked)
	}

	var state string
	_ = d.QueryRow(`SELECT state FROM exploration_nodes WHERE id=$1`, join).Scan(&state)
	if state != "done" {
		t.Errorf("join 状态 = %q, want \"done\"", state)
	}
}

// TestJoinStaysOpenWhenUpstreamStillRunning 上游未结束时 join 不动。
//
// 这是「门控」的基本语义：还没跑完就不该判定。
func TestJoinStaysOpenWhenUpstreamStillRunning(t *testing.T) {
	d := testOpen(t)

	expID, err := d.CreateExploration("test", "join 门控")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	es := d.Exploration(expID)

	up1, _ := es.AddNode(KindIntent, map[string]any{"role": "analyst_market"}, 10, "done", "agent", nil)
	up2, _ := es.AddNode(KindIntent, map[string]any{"role": "analyst_news"}, 10, "running", "agent", nil)
	join, _ := es.AddNode("join", map[string]any{}, 20, "open", "system", nil)
	_ = es.Link(up1, "gates", join)
	_ = es.Link(up2, "gates", join)

	res, err := es.ReconcileJoins()
	if err != nil {
		t.Fatalf("ReconcileJoins: %v", err)
	}
	if res.Satisfied != 0 || res.Blocked != 0 {
		t.Errorf("上游 running 时不该动 join，实际 Satisfied=%d Blocked=%d", res.Satisfied, res.Blocked)
	}

	var state string
	_ = d.QueryRow(`SELECT state FROM exploration_nodes WHERE id=$1`, join).Scan(&state)
	if state != "open" {
		t.Errorf("join 状态 = %q, want \"open\"（上游还在跑）", state)
	}
}

// TestOwnerTriggerDerivesFromPayloadRole 触发器必须从 payload.role 派生 owner。
//
// # 这条是触发器唯一有效的验收方式
//
// 它**绕过** `AddNode` 和 `MaterializeGraph`，直接 INSERT 不带 owner ——
// 模拟「将来新增的第 6 条写入路径」。
//
// # 为什么不能用「跑一遍 AddNode 看 owner 有没有值」验证
//
// 因为 `AddNode` 自己显式写了 owner。那条路径即使在触发器缺失时也能通过 ——
// 它证明不了「触发器存在」。
//
// 而全仓有 5 处 INSERT，历史上只有 1 处写 owner。要验证「第 6 处也安全」，
// 只能用一条「不写 owner 的 INSERT」。
//
// # 为什么不覆盖已有值
//
// 显式传 owner 的调用方（AddNode）不被推翻 —— 由第 2 段断言守。
func TestOwnerTriggerDerivesFromPayloadRole(t *testing.T) {
	d := testOpen(t)

	expID, err := d.CreateExploration("owner-trigger", "触发器派生 owner")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)

	// ---- ① 不写 owner 的 INSERT（模拟第 6 条写入路径）----
	// # 为什么不用 RETURNING owner
	//
	// PG 版这条是 `INSERT ... RETURNING owner`，靠 AFTER 触发器在同一语句里
	// 把 owner 填上。**SQLite 下不成立**：RETURNING 返回的是 INSERT 时的行镜像，
	// AFTER 触发器（trg_expnodes_owner_ins）随后做的 UPDATE 不会反映到
	// RETURNING 里 —— 读回来是 NULL，看起来像「触发器没生效」。
	// ARCH-SQLite-Migration.md §2.1 #16 记的就是这个坑。
	// 所以先 RETURNING id，再单独 SELECT owner。
	var insID int64
	if err := d.QueryRow(`
INSERT INTO exploration_nodes(exploration_id, kind, payload, priority, state, origin)
VALUES ($1, 'intent', '{"role":"portfolio_manager"}', 10, 'open', 'system')
RETURNING id`, expID).Scan(&insID); err != nil {
		t.Fatalf("INSERT（不带 owner）: %v", err)
	}
	var owner sql.NullString
	if err := d.QueryRow(`SELECT owner FROM exploration_nodes WHERE id=$1`, insID).Scan(&owner); err != nil {
		t.Fatalf("SELECT owner: %v", err)
	}
	if !owner.Valid || owner.String != "portfolio_manager" {
		t.Fatalf("触发器未生效：owner = %v, want \"portfolio_manager\"\n"+
			"（如果为 NULL，说明 schema_sqlite.sql 的 trg_expnodes_owner_ins 没被创建）", owner)
	}

	// ---- ② payload 没有 role → owner 必须是 NULL（不是空串）----
	var insID2 int64
	if err := d.QueryRow(`
INSERT INTO exploration_nodes(exploration_id, kind, payload, priority, state, origin)
VALUES ($1, 'fact', '{"text":"没有 role 的 fact"}', 10, 'origin', 'system')
RETURNING id`, expID).Scan(&insID2); err != nil {
		t.Fatalf("INSERT（fact）: %v", err)
	}
	var owner2 sql.NullString
	if err := d.QueryRow(`SELECT owner FROM exploration_nodes WHERE id=$1`, insID2).Scan(&owner2); err != nil {
		t.Fatalf("SELECT owner (fact): %v", err)
	}
	if owner2.Valid {
		t.Errorf("没有 role 时 owner 应为 NULL，实际 %q（'没有 owner' 只能有一种表示）", owner2.String)
	}

	// ---- ③ role 是空串 → owner 必须是 NULL（NULLIF 生效）----
	var insID3 int64
	if err := d.QueryRow(`
INSERT INTO exploration_nodes(exploration_id, kind, payload, priority, state, origin)
VALUES ($1, 'intent', '{"role":"   "}', 10, 'open', 'system')
RETURNING id`, expID).Scan(&insID3); err != nil {
		t.Fatalf("INSERT（空白 role）: %v", err)
	}
	var owner3 sql.NullString
	if err := d.QueryRow(`SELECT owner FROM exploration_nodes WHERE id=$1`, insID3).Scan(&owner3); err != nil {
		t.Fatalf("SELECT owner (blank): %v", err)
	}
	if owner3.Valid {
		t.Errorf("空白 role 应落成 NULL，实际 %q（NULLIF(TRIM(...),'') 没生效？）", owner3.String)
	}

	// ---- ④ 显式传 owner → 触发器不覆盖 ----
	var insID4 int64
	if err := d.QueryRow(`
INSERT INTO exploration_nodes(exploration_id, kind, payload, priority, state, origin, owner)
VALUES ($1, 'intent', '{"role":"analyst_market"}', 10, 'open', 'system', 'explicit_owner')
RETURNING id`, expID).Scan(&insID4); err != nil {
		t.Fatalf("INSERT（显式 owner）: %v", err)
	}
	var owner4 sql.NullString
	if err := d.QueryRow(`SELECT owner FROM exploration_nodes WHERE id=$1`, insID4).Scan(&owner4); err != nil {
		t.Fatalf("SELECT owner (explicit): %v", err)
	}
	if !owner4.Valid || owner4.String != "explicit_owner" {
		t.Errorf("显式传的 owner 被触发器覆盖了：%v, want \"explicit_owner\"\n"+
			"（触发器只在 owner IS NULL 时填）", owner4)
	}

	// ---- ⑤ UPDATE OF payload 也能补（先建节点、后补 role）----
	var id5 int64
	if err := d.QueryRow(`
INSERT INTO exploration_nodes(exploration_id, kind, payload, priority, state, origin)
VALUES ($1, 'intent', '{}', 10, 'open', 'system')
RETURNING id`, expID).Scan(&id5); err != nil {
		t.Fatalf("INSERT（空 payload）: %v", err)
	}
	if _, err := d.Exec(`
UPDATE exploration_nodes SET payload = json_patch(payload, '{"role":"trader"}') WHERE id=$1`, id5); err != nil {
		t.Fatalf("UPDATE payload: %v", err)
	}
	var owner5 sql.NullString
	if err := d.QueryRow(`SELECT owner FROM exploration_nodes WHERE id=$1`, id5).Scan(&owner5); err != nil {
		t.Fatalf("读 owner: %v", err)
	}
	if !owner5.Valid || owner5.String != "trader" {
		t.Errorf("UPDATE OF payload 未触发 owner 派生：%v, want \"trader\"\n"+
			"（如果为 NULL，说明触发器缺 UPDATE OF payload）", owner5)
	}
}
