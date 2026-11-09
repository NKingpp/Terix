package db

// T03 探索 / 发现 / 拦截域的 SQLite 方言自检。
//
// 覆盖四处最容易改错的方言映射（都是本轮实测过的）：
//  1. payload 查询：PG 的 `payload->>'k'` / `payload::text` → json_extract / 直接 LIKE
//  2. findings 的数组包含：PG 的 `asset_ids @> ANY(jsonb[])` → json_each 求交
//  3. intercept 规则匹配 + audit 的 json_patch 叠加 / json_extract 条件
//  4. evidence 锁替代方案：WithEvidenceTx 并发写不冲突、不丢更新
//
// 断言都对着「PG 版本的语义」写，不是对着实现写 —— 这样换实现时测试仍然有效。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
)

// ---------------------------------------------------------------------------
// 1. exploration.go：payload 查询
// ---------------------------------------------------------------------------

// TestExplorationPayloadFilter 验证 payload 的三种查询路径。
//
// 这三条对应 PG 版的：
//
//	payload->>'summary' ILIKE $n            （列表页筛选）
//	payload::text ILIKE $n                  （NodesPage 全文筛选）
//	json_extract 缺 key 时不能报错           （PG 的 ->> 返回 NULL）
func TestExplorationPayloadFilter(t *testing.T) {
	d := testOpen(t)

	expID, err := d.CreateExploration("payload-filter", "payload filter")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID) //nolint:errcheck // cascades
	es := d.Exploration(expID)

	cases := []struct {
		summary string
		origin  string
	}{
		{"Pick IDOR on /admin", "planner"},
		{"enumerate subdomains via DNS", "planner"},
		{"中文摘要：验证越权", "human"},
	}
	ids := make([]int64, 0, len(cases))
	for _, c := range cases {
		id, err := es.AddIntent(map[string]any{"summary": c.summary}, 5, nil, c.origin)
		if err != nil {
			t.Fatalf("AddIntent(%q): %v", c.summary, err)
		}
		ids = append(ids, id)
	}

	// (a) listByKindPageFiltered：json_extract + LIKE，ASCII 大小写不敏感
	got, err := es.listByKindPageFiltered(KindIntent, 0, 10, "idor")
	if err != nil {
		t.Fatalf("filter idor: %v", err)
	}
	if len(got) != 1 || got[0].ID != ids[0] {
		t.Fatalf("filter \"idor\" 命中 %d 条，want 1 (id=%d)", len(got), ids[0])
	}

	// (b) 同一个筛选换大写：SQLite 的 LIKE 对 ASCII 不区分大小写
	got, err = es.listByKindPageFiltered(KindIntent, 0, 10, "IDOR")
	if err != nil {
		t.Fatalf("filter IDOR: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("filter \"IDOR\" 命中 %d 条，want 1", len(got))
	}

	// (c) 中文摘要必须能命中（CJK 没有大小写，LIKE 直接可用）
	got, err = es.listByKindPageFiltered(KindIntent, 0, 10, "越权")
	if err != nil {
		t.Fatalf("filter 越权: %v", err)
	}
	if len(got) != 1 || got[0].ID != ids[2] {
		t.Fatalf("filter \"越权\" 命中 %d 条，want 1", len(got))
	}

	// (d) countByKindFiltered 必须与列表同口径
	n, err := es.countByKindFiltered(KindIntent, "idor")
	if err != nil {
		t.Fatalf("count idor: %v", err)
	}
	if n != 1 {
		t.Fatalf("count \"idor\" = %d, want 1", n)
	}

	// (e) NodesPage 的全文筛选会同时看 payload 与 origin
	nodes, total, err := es.NodesPage(NodeFilter{Query: "planner"}, 1, 10)
	if err != nil {
		t.Fatalf("NodesPage planner: %v", err)
	}
	if total != 2 || len(nodes) != 2 {
		t.Fatalf("NodesPage \"planner\" total=%d len=%d, want 2/2（两条 origin=planner）", total, len(nodes))
	}

	// (f) 没有任何一个 payload 带这段文字：json_extract 返回 NULL，
	//     LIKE 得到 NULL → 该行不入选。不能报错。
	nodes, total, err = es.NodesPage(NodeFilter{Query: "这条不可能出现在任何字段里"}, 1, 10)
	if err != nil {
		t.Fatalf("NodesPage 无命中: %v", err)
	}
	if total != 0 || len(nodes) != 0 {
		t.Fatalf("无命中时 total=%d len=%d, want 0/0", total, len(nodes))
	}
}

// TestLatestSiblingsRolesFilter 验证 `owner = ANY($n)` → json_each。
//
// PG 版直接绑 []string；SQLite 不支持，改成 JSON 数组 + json_each。
// 这里额外钉住两个边界：空 roles 提前返回、role 不存在时结果为空。
func TestLatestSiblingsRolesFilter(t *testing.T) {
	d := testOpen(t)
	expID, err := d.CreateExploration("siblings", "siblings")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID) //nolint:errcheck // cascades
	es := d.Exploration(expID)

	for _, role := range []string{"analyst_market", "analyst_news", "debate_bull"} {
		if _, err := es.AddNode(KindFact, map[string]any{"role": role, "claim": role}, 1, "confirmed", role, nil); err != nil {
			t.Fatalf("AddNode(%s): %v", role, err)
		}
	}

	got, err := es.LatestSiblings(KindFact, []string{"analyst_market", "debate_bull"}, 0)
	if err != nil {
		t.Fatalf("LatestSiblings: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("两个 role 各取全部 = %d 条，want 2（analyst_news 不该入选）", len(got))
	}
	for _, n := range got {
		if n.Owner == "analyst_news" {
			t.Fatalf("analyst_news 不在 roles 里却被选出: %+v", n)
		}
	}

	// 空 roles → nil, nil（不打库）
	got, err = es.LatestSiblings(KindFact, nil, 0)
	if err != nil || got != nil {
		t.Fatalf("空 roles 应返回 nil,nil；实际 %v,%v", got, err)
	}

	// role 全部不存在 → 空结果（空 JSON 数组在 json_each 下是空集合，不是语法错）
	got, err = es.LatestSiblings(KindFact, []string{"no_such_role"}, 0)
	if err != nil {
		t.Fatalf("不存在的 role: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("不存在的 role 应返回 0 条，实际 %d", len(got))
	}
}

// ---------------------------------------------------------------------------
// 2. findings.go / finding_assets.go：数组包含查询
// ---------------------------------------------------------------------------

// TestFindingAssetIDsFilter 验证 `asset_ids @> ANY(jsonb[])` 的替代实现。
//
// PG 语义：finding.asset_ids 里**含有**候选集合中的任意一个 id。
// 替代实现：json_each(asset_ids) 与 json_each($n) 求交。
//
// 同时验证资产树的「未关联桶」口径：asset_ids 为空、或里面的 id 在 assets
// 表里已不存在，两种都算未关联 —— 否则桶上的计数会大于点开后查到的条数。
func TestFindingAssetIDsFilter(t *testing.T) {
	d := testOpen(t)

	if _, err := d.Exec(`INSERT INTO assets(type,domain) VALUES('root_domain','a.t03.test'),('root_domain','b.t03.test')`); err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM assets WHERE domain LIKE '%.t03.test'`) //nolint:errcheck
	var aidA, aidB int64
	if err := d.QueryRow(`SELECT id FROM assets WHERE domain='a.t03.test'`).Scan(&aidA); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT id FROM assets WHERE domain='b.t03.test'`).Scan(&aidB); err != nil {
		t.Fatal(err)
	}

	mk := func(name string, ids []int64) int64 {
		t.Helper()
		id, err := d.AddFinding(0, 0, "t03-class", name, SeverityHigh, "summary", "evidence", "w", ids)
		if err != nil {
			t.Fatalf("AddFinding(%s): %v", name, err)
		}
		return id
	}
	fA := mk("only-a-t03", []int64{aidA})
	fB := mk("only-b-t03", []int64{aidB})
	fGhost := mk("ghost-t03", []int64{99999999})
	fEmpty := mk("empty-t03", nil)
	defer d.Exec(`DELETE FROM findings WHERE vulnclass='t03-class'`) //nolint:errcheck

	// (a) where() 层：候选集合 = {A, B} 时必须同时命中两条挂载的 finding，
	//     且不能把 empty / ghost 混进来。这一层是 @> ANY(jsonb[]) 的直接替代，
	//     与资产树的子树展开无关，所以直接调 where() 断言。
	cond, cargs := FindingFilter{VulnClass: "t03-class", assetIDs: []int64{aidA, aidB}}.where()
	var nAB int
	if err := d.QueryRow(`SELECT COUNT(*) FROM findings f LEFT JOIN tasks t ON f.task_id=t.id`+cond, cargs...).Scan(&nAB); err != nil {
		t.Fatalf("where() assetIDs A+B: %v", err)
	}
	if nAB != 2 {
		t.Fatalf("where() assetIDs={A,B} 命中 %d 条，want 2", nAB)
	}

	// (b) 只按 A 筛（走完整 ListFindingsPage 路径）：只命中 only-a-t03
	page, total, err := d.ListFindingsPage(FindingFilter{VulnClass: "t03-class", AssetScope: assetScopeOf(aidA)}, 1, 50)
	if err != nil {
		t.Fatalf("ListFindingsPage asset A: %v", err)
	}
	if total != 1 || len(page) != 1 || page[0].Name != "only-a-t03" {
		t.Fatalf("asset A 应只命中 only-a-t03；实际 total=%d names=%v", total, namesOf(page))
	}

	// (c) 未关联桶：空 asset_ids + ghost（资产已不存在）都收下
	page, total, err = d.ListFindingsPage(FindingFilter{VulnClass: "t03-class", AssetScope: FindingUnassignedAsset}, 1, 50)
	if err != nil {
		t.Fatalf("ListFindingsPage assetNone: %v", err)
	}
	got := map[string]bool{}
	for _, f := range page {
		got[f.Name] = true
	}
	if !got["empty-t03"] {
		t.Fatalf("未关联桶应含 empty-t03，实际 %v", got)
	}
	if got["only-a-t03"] || got["only-b-t03"] {
		t.Fatalf("未关联桶不该含已挂载资产的 finding，实际 %v", got)
	}

	// (d) 资产树的「未关联资产」节点计数必须与 assetNone 筛选同口径。
	//
	// buildFindingAssetTree 会主动清空 AssetScope（否则树会随选中节点塌缩成
	// 一条链），所以它的 FindingTotal 是「当前筛选下全部 finding」，不能用它
	// 断言子树。这里断言的是未关联桶：树里的该节点应存在，且其 finding 计数
	// 等于 assetNone 查出来的条数。
	tree, err := d.BuildFindingAssetTree(FindingFilter{VulnClass: "t03-class"})
	if err != nil {
		t.Fatalf("BuildFindingAssetTree: %v", err)
	}
	var unassignedNode *FindingAssetNode
	for i := range tree.Nodes {
		if tree.Nodes[i].Key == FindingUnassignedAsset {
			unassignedNode = &tree.Nodes[i]
			break
		}
	}
	if unassignedNode == nil {
		t.Fatalf("资产树里没有未关联资产节点: %+v", tree.Nodes)
	}
	if unassignedNode.Self != total {
		t.Fatalf("树的未关联桶计数=%d，assetNone 查出 %d —— 口径不一致（桶计数大于能查到的条数就是 bug）",
			unassignedNode.Self, total)
	}

	_ = fA
	_ = fB
	_ = fGhost
	_ = fEmpty
}

// assetScopeOf 拼出资产树里「某个资产节点」的 key（见 finding_assets.go 的 key 规则）。
func assetScopeOf(assetID int64) string { return fmt.Sprintf("a:%d", assetID) }

func namesOf(fs []*DBFinding) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.Name)
	}
	return out
}

// ---------------------------------------------------------------------------
// 3. intercept*.go：规则匹配 + audit 叠加
// ---------------------------------------------------------------------------

// TestInterceptRuleMatchAndAudit 覆盖拦截域的三条方言改动：
//
//	(a) CreateInterceptRule 的 RETURNING + pattern_params 存取（json 列）
//	(b) ResolveIntercept 的 audit 叠加：PG 的 `audit || patch || 条件对象`
//	    改成 json_patch 嵌套 + json_extract 条件判断
//	(c) CompleteIntercept 的 `audit->>'k'` 改成 json_extract
func TestInterceptRuleMatchAndAudit(t *testing.T) {
	d := testOpen(t)

	rule, err := d.CreateInterceptRule("t03-rule", "tool_name", "string", "PlaceOrder", "ask", "需要人工确认", 10, true, false, 0, "deny")
	if err != nil {
		t.Fatalf("CreateInterceptRule: %v", err)
	}
	defer d.Exec(`DELETE FROM intercept_rules WHERE id=$1`, rule.ID)        //nolint:errcheck
	defer d.Exec(`DELETE FROM intercept_pending WHERE rule_id=$1`, rule.ID) //nolint:errcheck

	if rule.ID == 0 || rule.MatchTarget != "tool_name" || rule.Pattern != "PlaceOrder" {
		t.Fatalf("规则回读不一致: %+v", rule)
	}
	if len(rule.PatternParams) == 0 || !json.Valid(rule.PatternParams) {
		t.Fatalf("pattern_params 不是合法 JSON: %q", string(rule.PatternParams))
	}

	rules, err := d.ListInterceptRules()
	if err != nil {
		t.Fatalf("ListInterceptRules: %v", err)
	}
	found := false
	for _, r := range rules {
		if r.ID == rule.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("ListInterceptRules 里找不到刚建的规则 %d", rule.ID)
	}

	// (b) 建一条 pending，然后 allow 决议：audit 里应出现 execution_status
	audit := &InterceptAudit{
		Correlation: "legacy", RunID: "run-t03", ToolUseID: "tool-t03",
	}
	pendingID, err := d.CreateInterceptPending(rule.ID, 0, "", "worker", "PlaceOrder",
		[]byte(`{"symbol":"QQQ"}`), "需要人工确认", audit)
	if err != nil {
		t.Fatalf("CreateInterceptPending: %v", err)
	}

	ok, err := d.ResolveIntercept(pendingID, "allowed", "allow", "人工放行")
	if err != nil {
		t.Fatalf("ResolveIntercept: %v", err)
	}
	if !ok {
		t.Fatal("ResolveIntercept 应更新一行")
	}

	detail, err := d.GetInterceptDetail(pendingID)
	if err != nil {
		t.Fatalf("GetInterceptDetail: %v", err)
	}
	if detail == nil || detail.Audit == nil {
		t.Fatalf("决议后应有 audit，实际 %+v", detail)
	}
	// correlation 不是 exact 且 action=allow → execution_status 必须被改写成 unknown
	if got := detail.Audit.ExecutionStatus; got != "unknown" {
		t.Fatalf("legacy correlation 下 execution_status = %q，want \"unknown\"", got)
	}
	// 原有字段不能被 json_patch 冲掉
	if detail.Audit.RunID != "run-t03" || detail.Audit.ToolUseID != "tool-t03" {
		t.Fatalf("json_patch 丢字段: run_id=%q tool_use_id=%q", detail.Audit.RunID, detail.Audit.ToolUseID)
	}

	// 重复决议必须无效（status 已不是 pending）
	ok, err = d.ResolveIntercept(pendingID, "denied", "deny", "再改一次")
	if err != nil {
		t.Fatalf("重复 ResolveIntercept: %v", err)
	}
	if ok {
		t.Fatal("重复决议不该成功（WHERE status='pending'）")
	}

	// (c) CompleteIntercept 的 json_extract 条件：
	//     effective_action=allow 且 execution_status=awaiting_result 才更新。
	//     刚走过 decision 的行 execution_status 是 unknown，所以不该命中。
	if err := d.CompleteIntercept(pendingID, "run-t03", "tool-t03", "succeeded", "done", false); err != nil {
		t.Fatalf("CompleteIntercept: %v", err)
	}
	detail, err = d.GetInterceptDetail(pendingID)
	if err != nil {
		t.Fatalf("GetInterceptDetail after complete: %v", err)
	}
	if got := detail.Audit.ExecutionStatus; got != "unknown" {
		t.Fatalf("execution_status 不该被改写（前置条件不满足）；实际 %q", got)
	}
}

// ---------------------------------------------------------------------------
// 4. finding_traffic.go：evidence 锁替代方案
// ---------------------------------------------------------------------------

// TestEvidenceTxSerializesWriters 证明 WithEvidenceTx 在去掉 advisory lock 后
// 仍是互斥的，并且并发「读-判-写」不丢更新。
//
// 为什么这个测试能证明锁替代有效：PG 版靠 pg_advisory_xact_lock 把并发事务串起来，
// 现在只剩 BEGIN IMMEDIATE。SQLite 的 BEGIN IMMEDIATE 在事务开始就拿写锁，
// 所以 N 个 goroutine 各自的 +1 必须一个不少地累加 —— 只要丢一次就说明互斥失效。
func TestEvidenceTxSerializesWriters(t *testing.T) {
	d := testOpen(t)

	if _, err := d.Exec(`INSERT INTO assets(type,domain) VALUES('root_domain','evidence-t03b.test')`); err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM assets WHERE domain='evidence-t03b.test'`) //nolint:errcheck
	var aid int64
	if err := d.QueryRow(`SELECT id FROM assets WHERE domain='evidence-t03b.test'`).Scan(&aid); err != nil {
		t.Fatal(err)
	}

	// 在 findings.evidence_version 上做并发自增：它就是 bumpEvidenceVersionTx 的列。
	findingID, err := d.AddFinding(0, 0, "idor", "evidence-counter-t03", SeverityHigh, "s", "e", "w", []int64{aid})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM findings WHERE id=$1`, findingID) //nolint:errcheck

	const workers = 8
	const perWorker = 10
	var wg sync.WaitGroup
	errs := make(chan error, workers*perWorker)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				err := d.WithEvidenceTx(context.Background(), func(tx *sql.Tx) error {
					// 读-判-写：如果互斥失效，两个事务会读到同一个旧值再各自 +1
					var cur int64
					if err := tx.QueryRow(`SELECT evidence_version FROM findings WHERE id=$1`, findingID).Scan(&cur); err != nil {
						return err
					}
					_, err := tx.Exec(`UPDATE findings SET evidence_version=$2 WHERE id=$1`, findingID, cur+1)
					return err
				})
				if err != nil {
					errs <- fmt.Errorf("worker %d iter %d: %w", w, i, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("WithEvidenceTx 并发写失败: %v", err)
	}

	var final int64
	if err := d.QueryRow(`SELECT evidence_version FROM findings WHERE id=$1`, findingID).Scan(&final); err != nil {
		t.Fatal(err)
	}
	if want := int64(workers * perWorker); final != want {
		t.Fatalf("evidence_version = %d, want %d —— 丢更新说明事务互斥失效", final, want)
	}
}
