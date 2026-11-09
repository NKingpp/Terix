package db

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// mkJoin 建一个 join 节点。
func mkJoin(t *testing.T, es *ExplorationStore, payload map[string]any) int64 {
	t.Helper()
	if payload == nil {
		payload = map[string]any{}
	}
	id, err := es.AddNode("join", payload, 20, "open", "system", nil)
	if err != nil {
		t.Fatalf("建 join: %v", err)
	}
	return id
}

// mkRoleIntent 建一个带 role 的 intent，返回 id。
func mkRoleIntent(t *testing.T, es *ExplorationStore, role string, priority int) int64 {
	t.Helper()
	id, err := es.AddIntent(map[string]any{"role": role, "summary": role + " work"}, priority, nil, "system")
	if err != nil {
		t.Fatalf("建 intent %s: %v", role, err)
	}
	return id
}

// frontierIDs 取当前可 claim 的 intent id 集合。
func frontierIDs(t *testing.T, es *ExplorationStore) map[int64]bool {
	t.Helper()
	fr, err := es.Frontier(50)
	if err != nil {
		t.Fatalf("Frontier: %v", err)
	}
	out := make(map[int64]bool, len(fr))
	for _, n := range fr {
		out[n.ID] = true
	}
	return out
}

// TestGatesBlockDownstreamUntilUpstreamSettled 门控核心断言：
// 上游未结束时下游不出现在 frontier；上游全部结束后才出现。
func TestGatesBlockDownstreamUntilUpstreamSettled(t *testing.T) {
	d := testOpen(t)
	expID, err := d.CreateExploration("gates-basic", "验证门控")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := d.Exec("DELETE FROM explorations WHERE id=$1", expID); err != nil {
			t.Logf("清理 exploration %d 失败: %v", expID, err)
		}
	})
	es := d.Exploration(expID)

	// 两个分析师 → join → 一个辩论
	a1 := mkRoleIntent(t, es, "analyst_market", 10)
	a2 := mkRoleIntent(t, es, "analyst_news", 10)
	join := mkJoin(t, es, map[string]any{"stage": "analysis"})
	debate := mkRoleIntent(t, es, "debate_bull", 31)

	// 依赖：每个分析师都 gates join；join gates debate
	for _, a := range []int64{a1, a2} {
		if err := addGatesForTest(t, es, a, join); err != nil {
			t.Fatalf("AddGates(%d→join): %v", a, err)
		}
	}
	if err := addGatesForTest(t, es, join, debate); err != nil {
		t.Fatalf("AddGates(join→debate): %v", err)
	}

	// 初始：只有两个分析师可 claim，辩论被挡
	fr := frontierIDs(t, es)
	if !fr[a1] || !fr[a2] {
		t.Errorf("两个分析师应可 claim，实际 frontier=%v", fr)
	}
	if fr[debate] {
		t.Error("辩论不该在上游未完成时出现")
	}

	// a1 完成：join 仍未兑现（a2 未结束），辩论仍被挡
	if err := es.SetIntentState(a1, "done"); err != nil {
		t.Fatalf("a1 done: %v", err)
	}
	res, err := es.ReconcileJoins()
	if err != nil {
		t.Fatalf("ReconcileJoins: %v", err)
	}
	if res.Satisfied != 0 {
		t.Errorf("a2 未结束时 join 不应兑现，实际 Satisfied=%d", res.Satisfied)
	}
	if frontierIDs(t, es)[debate] {
		t.Error("辩论不该在 a2 未完成时出现")
	}

	// a2 完成：join 兑现，辩论出现
	if err := es.SetIntentState(a2, "done"); err != nil {
		t.Fatalf("a2 done: %v", err)
	}
	res, err = es.ReconcileJoins()
	if err != nil {
		t.Fatalf("ReconcileJoins: %v", err)
	}
	if res.Satisfied != 1 {
		t.Fatalf("join 应兑现 1 个，实际 %d", res.Satisfied)
	}
	if len(res.Details) != 1 || res.Details[0].JoinID != join {
		t.Errorf("Details 应指向 join %d: %+v", join, res.Details)
	}
	if len(res.Details[0].FailedUpstreams) != 0 {
		t.Errorf("两个上游都 done，不应有失败上游: %v", res.Details[0].FailedUpstreams)
	}
	if !frontierIDs(t, es)[debate] {
		t.Error("join 兑现后辩论应可 claim")
	}

	// join 状态确实是 done
	nodes, err := es.NodesByIDs([]int64{join})
	if err != nil || len(nodes) == 0 {
		t.Fatalf("查 join: %v", err)
	}
	if nodes[0].State != "done" {
		t.Errorf("join state = %q, want done", nodes[0].State)
	}
}

// TestGatesFailedUpstreamBlocksNotReconciles 失败态上游必须**阻塞** join（不是兑现）。
//
// # 为什么是「阻塞」而不是「兑现 + 记录」
//
// 文档 ARCH-W2-W5.md:899 给的判据：
//
//	取数失败的四种「软处理」（emit neutral / Hold / REVIEW / 复用旧快照）
//	全部被排除，理由是「会污染统计」。
//	而「join → done + 记录 FailedUpstreams」是第五种同类做法 ——
//	它让下游照样跑，只在 facts 里留一条「某维度缺失」。
//
// 文档 :4641 更直接：
//
//	join 的任一 await 处于 blocked/exhausted/stopped → join 自身转 blocked。
//
// # 这条测试原来写的是相反的断言，注释称「裁定 4 的核心」
//
// 「裁定 4」在任何文档里查不到（grep "裁定 4" = 0 命中）。
// 而它的担心（「一个分析师失败不能让整条链 deadlock」）已经被文档的方案解决：
// blocked 是终态 + 前端显式显示「数据源故障」+ 提供「重试」按钮。
// 所以「deadlock」这个前提不成立。
//
// 详见 ARCH-W2-W5.md:4641 附近的 2026-10-28 补充说明。
func TestGatesFailedUpstreamBlocksNotReconciles(t *testing.T) {
	d := testOpen(t)
	expID, err := d.CreateExploration("gates-failed-upstream", "验证失败上游阻塞")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := d.Exec("DELETE FROM explorations WHERE id=$1", expID); err != nil {
			t.Logf("清理 exploration %d 失败: %v", expID, err)
		}
	})
	es := d.Exploration(expID)

	ok1 := mkRoleIntent(t, es, "analyst_market", 10)
	bad := mkRoleIntent(t, es, "analyst_news", 10)
	join := mkJoin(t, es, map[string]any{"stage": "analysis"})
	debate := mkRoleIntent(t, es, "debate_bull", 31)

	if err := addGatesForTest(t, es, ok1, join); err != nil {
		t.Fatal(err)
	}
	if err := addGatesForTest(t, es, bad, join); err != nil {
		t.Fatal(err)
	}
	if err := addGatesForTest(t, es, join, debate); err != nil {
		t.Fatal(err)
	}

	// 一个成功一个失败
	if err := es.SetIntentState(ok1, "done"); err != nil {
		t.Fatalf("ok1 done: %v", err)
	}
	if err := es.SetIntentState(bad, "blocked"); err != nil {
		t.Fatalf("bad blocked: %v", err)
	}

	res, err := es.ReconcileJoins()
	if err != nil {
		t.Fatalf("ReconcileJoins: %v", err)
	}

	// ---- 关键断言 1：join 转 blocked，不是 done ----
	if res.Blocked != 1 || res.Satisfied != 0 {
		t.Fatalf("有失败上游时应 Blocked=1 Satisfied=0，实际 Blocked=%d Satisfied=%d",
			res.Blocked, res.Satisfied)
	}
	if len(res.BlockedDetails) != 1 {
		t.Fatalf("BlockedDetails 应有 1 条: %+v", res.BlockedDetails)
	}
	fa := res.BlockedDetails[0].FailedUpstreams
	if len(fa) != 1 || fa[0] != "analyst_news" {
		t.Errorf("FailedUpstreams = %v, want [analyst_news]", fa)
	}

	// ---- 关键断言 2：join 的 state 真的是 blocked ----
	nodes, err := es.NodesByIDs([]int64{join})
	if err != nil || len(nodes) == 0 {
		t.Fatalf("查 join: %v", err)
	}
	if nodes[0].State != "blocked" {
		t.Fatalf("join state = %q, want \"blocked\"（上游 analyst_news 是 blocked）", nodes[0].State)
	}

	// ---- 关键断言 3：blocked_reason 里能看到是哪个上游失败的 ----
	if !strings.Contains(nodes[0].BlockedReason, "analyst_news") {
		t.Errorf("blocked_reason 应指出失败的上游（analyst_news），实际 %q",
			nodes[0].BlockedReason)
	}

	// ---- 断言 4：下游被门控挡住（不在 Frontier 里）----
	//
	// 这是「阻塞」和「兑现」的实际差别：兑现时下游可 claim，阻塞时不可。
	//
	// # 这条曾经用 t.Log 而不是 t.Error
	//
	// 因为当时 Frontier 用 `upstreamSettledStates`（含 blocked）判断「上游可否放行下游」，
	// 所以 join 转 blocked 后下游仍被 claim —— L16。L16 修好后（Frontier 改用
	// `upstreamSucceededStates`），这里改回 t.Error。
	if frontierIDs(t, es)[debate] {
		t.Error("上游 blocked 时下游不该可 claim —— 那会拿不完整数据出结论")
	}

	// ---- 附带：payload 里仍应记录 failed_upstreams（供前端显示）----
	var p map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &p); err != nil {
		t.Fatalf("解析 join payload: %v", err)
	}
	arr, ok := p["failed_upstreams"].([]any)
	if !ok || len(arr) != 1 || arr[0] != "analyst_news" {
		t.Errorf("join.payload.failed_upstreams = %v, want [analyst_news]", p["failed_upstreams"])
	}
	if _, has := p["reconciled_at"]; !has {
		t.Error("join.payload 应记 reconciled_at")
	}
	// 原有 payload 字段应保留（JSONB merge 不是覆盖）
	if p["stage"] != "analysis" {
		t.Errorf("原有 payload 字段被覆盖了：stage = %v", p["stage"])
	}
}

// TestGatesDistinctFromYields 区分 gates 与 yields：
// 只有 yields 边时下游不会被阻塞（yields 是归属不是依赖）。
func TestGatesDistinctFromYields(t *testing.T) {
	d := testOpen(t)
	expID, err := d.CreateExploration("gates-vs-yields", "yields 不阻塞")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := d.Exec("DELETE FROM explorations WHERE id=$1", expID); err != nil {
			t.Logf("清理 exploration %d 失败: %v", expID, err)
		}
	})
	es := d.Exploration(expID)

	a1 := mkRoleIntent(t, es, "analyst_market", 10)
	a2 := mkRoleIntent(t, es, "analyst_news", 10)

	// 只用 yields（归属关系），不是依赖
	factID, err := es.AddNode(KindFact, map[string]any{"claim": "x"}, 5, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := es.Link(a1, RelYields, factID); err != nil {
		t.Fatalf("Link yields: %v", err)
	}

	// a2 不该被 a1 的 yields 阻塞
	fr := frontierIDs(t, es)
	if !fr[a2] {
		t.Error("yields 是归属关系不是依赖，a2 不该被阻塞")
	}
}

// TestGatesIdempotent 重复建同一条 gates 边不报错（PK 兜底）。
func TestGatesIdempotent(t *testing.T) {
	d := testOpen(t)
	expID, err := d.CreateExploration("gates-idem", "幂等")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := d.Exec("DELETE FROM explorations WHERE id=$1", expID); err != nil {
			t.Logf("清理 exploration %d 失败: %v", expID, err)
		}
	})
	es := d.Exploration(expID)
	a := mkRoleIntent(t, es, "analyst_market", 10)
	j := mkJoin(t, es, nil)

	for i := 0; i < 3; i++ {
		if err := addGatesForTest(t, es, a, j); err != nil {
			t.Fatalf("第 %d 次 AddGates 报错: %v", i+1, err)
		}
	}
	var cnt int
	if err := d.QueryRow(`SELECT count(*) FROM exploration_edges
WHERE exploration_id=$1 AND src_id=$2 AND dst_id=$3 AND rel='gates'`, expID, a, j).Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 1 {
		t.Errorf("gates 边应只有 1 条，实际 %d", cnt)
	}
}

// TestAddGatesRejectsSelf 自环必须拒绝（ck_edge_noself 也兜底，但应用层先拦）。
func TestAddGatesRejectsSelf(t *testing.T) {
	d := testOpen(t)
	expID, err := d.CreateExploration("gates-self", "自环")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := d.Exec("DELETE FROM explorations WHERE id=$1", expID); err != nil {
			t.Logf("清理 exploration %d 失败: %v", expID, err)
		}
	})
	es := d.Exploration(expID)
	a := mkRoleIntent(t, es, "analyst_market", 10)
	if err := addGatesForTest(t, es, a, a); err == nil {
		t.Error("自环应被拒绝")
	}
}

// TestReconcileJoinsNoJoins 没有 join 时应快速返回，不做多余查询。
func TestReconcileJoinsNoJoins(t *testing.T) {
	d := testOpen(t)
	expID, err := d.CreateExploration("no-joins", "无 join")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := d.Exec("DELETE FROM explorations WHERE id=$1", expID); err != nil {
			t.Logf("清理 exploration %d 失败: %v", expID, err)
		}
	})
	es := d.Exploration(expID)
	mkRoleIntent(t, es, "analyst_market", 10)

	res, err := es.ReconcileJoins()
	if err != nil {
		t.Fatalf("ReconcileJoins: %v", err)
	}
	if res.Satisfied != 0 {
		t.Errorf("没有 join 时应 Satisfied=0，实际 %d", res.Satisfied)
	}
}

// TestGatesMultipleJoinsChain 多级 join 链：join1 → join2。
func TestGatesMultipleJoinsChain(t *testing.T) {
	d := testOpen(t)
	expID, err := d.CreateExploration("gates-chain", "多级 join")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := d.Exec("DELETE FROM explorations WHERE id=$1", expID); err != nil {
			t.Logf("清理 exploration %d 失败: %v", expID, err)
		}
	})
	es := d.Exploration(expID)

	a1 := mkRoleIntent(t, es, "analyst_market", 10)
	j1 := mkJoin(t, es, map[string]any{"stage": "analysis"})
	mid := mkRoleIntent(t, es, "research_manager", 50)
	j2 := mkJoin(t, es, map[string]any{"stage": "research"})
	final := mkRoleIntent(t, es, "portfolio_manager", 90)

	// a1 → j1 → mid → j2 → final
	for _, pair := range [][2]int64{{a1, j1}, {j1, mid}, {mid, j2}, {j2, final}} {
		if err := addGatesForTest(t, es, pair[0], pair[1]); err != nil {
			t.Fatal(err)
		}
	}

	fr := frontierIDs(t, es)
	if !fr[a1] || fr[mid] || fr[final] {
		t.Errorf("初始应只有 a1 可 claim，实际 %v", fr)
	}

	// a1 完成 → j1 兑现 → mid 出现（j2 仍未兑现）
	if err := es.SetIntentState(a1, "done"); err != nil {
		t.Fatal(err)
	}
	if _, err := es.ReconcileJoins(); err != nil {
		t.Fatal(err)
	}
	fr = frontierIDs(t, es)
	if !fr[mid] {
		t.Error("j1 兑现后 mid 应可 claim")
	}
	if fr[final] {
		t.Error("final 不该在 mid 未完成时出现")
	}

	// mid 完成 → j2 兑现 → final 出现
	if err := es.SetIntentState(mid, "done"); err != nil {
		t.Fatal(err)
	}
	if _, err := es.ReconcileJoins(); err != nil {
		t.Fatal(err)
	}
	if !frontierIDs(t, es)[final] {
		t.Error("j2 兑现后 final 应可 claim")
	}
}

// TestFrontierUnaffectedWithoutGates 回归：没有 gates 边时 Frontier 行为不变。
//
// 这是对渗透流程零影响的核心断言 —— 渗透只用 spawns/derived_from/yields/proves。
func TestFrontierUnaffectedWithoutGates(t *testing.T) {
	d := testOpen(t)
	expID, err := d.CreateExploration("frontier-regression", "无 gates 时行为不变")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := d.Exec("DELETE FROM explorations WHERE id=$1", expID); err != nil {
			t.Logf("清理 exploration %d 失败: %v", expID, err)
		}
	})
	es := d.Exploration(expID)

	i1 := mkRoleIntent(t, es, "recon", 10)
	i2 := mkRoleIntent(t, es, "exploit", 5)
	// 加一条 spawns（渗透最常用的关系）
	if err := es.Link(i1, RelSpawns, i2); err != nil {
		t.Fatalf("Link spawns: %v", err)
	}

	fr := frontierIDs(t, es)
	if !fr[i1] || !fr[i2] {
		t.Errorf("无 gates 边时两条 intent 都应可 claim，实际 %v", fr)
	}
}

// addGatesForTest 直接建一条 gates 边（仅测试使用）。
//
// 生产代码没有这个能力：图的边必须经由 agent.MaterializeGraph 建，
// 那里有 kind 校验、Ref 存在性校验、事务保证（见 joins.go 末尾的注释）。
// 门控测试需要直接构造边来测 Frontier 的过滤行为，不需要完整 16 节点图。
func addGatesForTest(t *testing.T, es *ExplorationStore, srcID, dstID int64) error {
	t.Helper()
	if srcID == dstID {
		return fmt.Errorf("gates 边的两端不能是同一节点（%d）", srcID)
	}
	_, err := es.db.Exec(`
INSERT INTO exploration_edges(exploration_id, src_id, dst_id, rel)
VALUES ($1,$2,$3,'gates') ON CONFLICT DO NOTHING`, es.expID, srcID, dstID)
	return err
}

// TestJoinStillReconcilesWhenUpstreamBlocked 守住「两个集合」的边界。
//
// # 这条测什么
//
// Frontier 用 succeeded（blocked 挡住下游）
// ReconcileJoins 用 settled（blocked 不阻止 join 判断）
//
// # 如果这里红了，说明有人把 ReconcileJoins 也改成了 succeeded
//
// 那 join 会永远等不到「全部成功」，任务永久卡住 —— 比「下游拿不完整数据」更糟。
func TestJoinStillReconcilesWhenUpstreamBlocked(t *testing.T) {
	d := testOpen(t)

	expID, err := d.CreateExploration("join-settled-boundary", "两个集合的边界")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	es := d.Exploration(expID)

	ok1, _ := es.AddNode(KindIntent, map[string]any{"role": "analyst_market"}, 10, "done", "agent", nil)
	bad, _ := es.AddNode(KindIntent, map[string]any{"role": "analyst_news"}, 10, "blocked", "agent", nil)
	join, _ := es.AddNode("join", map[string]any{}, 20, "open", "system", nil)
	_ = es.Link(ok1, "gates", join)
	_ = es.Link(bad, "gates", join)

	res, err := es.ReconcileJoins()
	if err != nil {
		t.Fatalf("ReconcileJoins 必须能处理 blocked 上游（不能永久等待）：%v", err)
	}
	if res.Blocked != 1 {
		t.Fatalf("join 必须能判断出「有上游失败」并转 blocked，实际 Blocked=%d（"+
			"如果为 0，说明 ReconcileJoins 也用了 succeeded 集合，join 永远等不到全部 done）",
			res.Blocked)
	}
}

// TestFrontierBlocksAllNonDoneUpstreamStates 参数化：blocked / exhausted / stopped 都挡住下游。
//
// # 为什么要参数化
//
// 如果只测 blocked，将来有人把集合改成 []string{} 或 {"done","blocked"} 会漏掉另外两个。
// 这三个都是「结束但不成功」—— 都不该让下游拿不完整数据出结论。
func TestFrontierBlocksAllNonDoneUpstreamStates(t *testing.T) {
	for _, upstreamState := range []string{"blocked", "exhausted", "stopped"} {
		t.Run(upstreamState, func(t *testing.T) {
			d := testOpen(t)

			expID, err := d.CreateExploration("frontier-gate-"+upstreamState, "上游未成功挡住下游")
			if err != nil {
				t.Fatal(err)
			}
			defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
			es := d.Exploration(expID)

			up, _ := es.AddNode(KindIntent, map[string]any{"role": "analyst_market"}, 10, upstreamState, "agent", nil)
			downstream, _ := es.AddNode(KindIntent, map[string]any{"role": "debate_bull"}, 30, "open", "agent", nil)
			_ = es.Link(up, "gates", downstream)

			if frontierIDs(t, es)[downstream] {
				t.Errorf("上游 state=%q 时下游不该可 claim —— 它是「结束但不成功」，"+
					"下游拿不到可用产出", upstreamState)
			}
		})
	}
}

// TestFrontierAllowsWhenUpstreamDone 「防过度修正」：done 上游必须放行。
//
// # 为什么需要这条
//
// 上一条测「挡住非 done」。如果只测那个，会让人以为「挡得越多越好」——
// 而把 done 也挡了会让整个系统停摆（所有下游都永远不可 claim）。
func TestFrontierAllowsWhenUpstreamDone(t *testing.T) {
	d := testOpen(t)

	expID, err := d.CreateExploration("frontier-gate-done", "上游成功放行")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	es := d.Exploration(expID)

	up, _ := es.AddNode(KindIntent, map[string]any{"role": "analyst_market"}, 10, "done", "agent", nil)
	downstream, _ := es.AddNode(KindIntent, map[string]any{"role": "debate_bull"}, 30, "open", "agent", nil)
	_ = es.Link(up, "gates", downstream)

	if !frontierIDs(t, es)[downstream] {
		t.Error("上游 done 时下游必须可 claim —— 否则整个流程停摆")
	}
}

// TestFrontierAllowsWhenNoGates 没有 gates 上游的 intent 直接可 claim。
//
// 防止「加了 gates 检查后，把无依赖的 intent 也挡住了」。
func TestFrontierAllowsWhenNoGates(t *testing.T) {
	d := testOpen(t)

	expID, err := d.CreateExploration("frontier-nogates", "无 gates 上游")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	es := d.Exploration(expID)

	solo, _ := es.AddNode(KindIntent, map[string]any{"role": "analyst_market"}, 10, "open", "agent", nil)

	if !frontierIDs(t, es)[solo] {
		t.Error("没有 gates 上游的 intent 必须可 claim")
	}
}
