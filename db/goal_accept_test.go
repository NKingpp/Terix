package db

import (
	"database/sql"
	"strings"
	"testing"
)

func TestAcceptGoalAuditAndIdempotent(t *testing.T) {
	d := testOpen(t)
	expID, err := d.CreateExploration("人工验收", "净盈利 10 USDT")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	es := d.Exploration(expID)
	goal, err := es.AddGoal(map[string]any{"text": "净盈利 10 USDT"}, "human")
	if err != nil {
		t.Fatal(err)
	}
	acc := &GoalAcceptance{Kind: "pnl_ge", Source: "ledger", Mode: "live", Threshold: "10"}
	res, err := es.AcceptGoalWithAudit(goal, "实盘 round-trip 净 +11.004", 0, acc, map[string]string{"realized_task": "11.0041880000"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed || res.Previous != "open" {
		t.Fatalf("首次验收应 changed=true/previous=open，得到 %+v", res)
	}
	var state string
	var completed, payloadAcceptance string
	if err := d.QueryRow(`SELECT state, COALESCE(completed_at,''), COALESCE(json_extract(payload,'$.acceptance.kind'),'')
		FROM exploration_nodes WHERE id=$1`, goal).Scan(&state, &completed, &payloadAcceptance); err != nil {
		t.Fatal(err)
	}
	if state != "met" || completed == "" || payloadAcceptance != "pnl_ge" {
		t.Fatalf("置 met 失败：state=%q completed=%q acceptance=%q", state, completed, payloadAcceptance)
	}
	var n int
	var metadata string
	if err := d.QueryRow(`SELECT COUNT(*), COALESCE(MAX(metadata),'') FROM activity
		WHERE exploration_id=$1 AND kind='goal_accepted'`, expID).Scan(&n, &metadata); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("审计行数应为 1，得到 %d", n)
	}
	for _, want := range []string{`"reason"`, `"observation"`, `"acceptance"`, `previous_state`} {
		if !strings.Contains(metadata, want) {
			t.Fatalf("审计 metadata 缺 %s: %s", want, metadata)
		}
	}

	// 幂等：第二次不得再写审计、不得报错。
	res2, err := es.AcceptGoalWithAudit(goal, "重复点击", 0, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Changed {
		t.Fatal("重复验收不应 changed")
	}
	if err := d.QueryRow(`SELECT COUNT(*) FROM activity WHERE exploration_id=$1 AND kind='goal_accepted'`, expID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("重复验收不得新增审计，得到 %d 条", n)
	}
}

func TestAcceptGoalAuditRollback(t *testing.T) {
	d := testOpen(t)
	expID, err := d.CreateExploration("人工验收回滚", "已平仓")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	es := d.Exploration(expID)
	goal, err := es.AddGoal(map[string]any{"text": "已平仓"}, "human")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`CREATE TRIGGER test_accept_audit_fail BEFORE INSERT ON activity
		WHEN NEW.kind='goal_accepted' BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DROP TRIGGER IF EXISTS test_accept_audit_fail`)

	if _, err := es.AcceptGoalWithAudit(goal, "审计失败必须回滚", 0, &GoalAcceptance{Kind: "flat", Source: "ledger"}, nil); err == nil {
		t.Fatal("审计写入失败时应返回错误")
	}
	var state string
	var completed sql.NullString
	if err := d.QueryRow(`SELECT state, completed_at FROM exploration_nodes WHERE id=$1`, goal).Scan(&state, &completed); err != nil {
		t.Fatal(err)
	}
	if state != "open" || completed.Valid {
		t.Fatalf("审计失败必须整体回滚：state=%q completed=%v", state, completed)
	}
}

func TestAcceptGoalEvidenceScope(t *testing.T) {
	d := testOpen(t)
	expID, err := d.CreateExploration("人工验收证据", "需外部证据")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	es := d.Exploration(expID)
	goal, err := es.AddGoal(map[string]any{"text": "需外部证据"}, "human")
	if err != nil {
		t.Fatal(err)
	}
	fact, err := es.AddNode(KindFact, map[string]any{"text": "结算证据"}, 0, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}

	// 无口径且无证据 → 拒绝（老目标不能裸盖章）。
	if _, err := es.AcceptGoalWithAudit(goal, "无证据", 0, nil, nil); err == nil {
		t.Fatal("无验收口径且无证据应被拒绝")
	}
	// 有口径 + 本任务事实证据 → 通过，且写入 proves 边。
	res, err := es.AcceptGoalWithAudit(goal, "附结算证据", fact, &GoalAcceptance{Kind: "flat", Source: "evidence"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Fatal("应置 met")
	}
	var edges int
	if err := d.QueryRow(`SELECT COUNT(*) FROM exploration_edges
		WHERE exploration_id=$1 AND rel='proves' AND src_id=$2 AND dst_id=$3`, expID, fact, goal).Scan(&edges); err != nil {
		t.Fatal(err)
	}
	if edges != 1 {
		t.Fatalf("应写入 1 条 proves 边，得到 %d", edges)
	}

	// 别的探索的事实不能当证据。
	otherExp, err := d.CreateExploration("别的任务", "无关")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, otherExp)
	otherFact, err := d.Exploration(otherExp).AddNode(KindFact, map[string]any{"text": "越界证据"}, 0, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	goal2, err := es.AddGoal(map[string]any{"text": "第二条"}, "human")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := es.AcceptGoalWithAudit(goal2, "越界证据", otherFact, &GoalAcceptance{Kind: "flat", Source: "evidence"}, nil); err == nil {
		t.Fatal("跨任务证据应被拒绝")
	}
}

func TestGoalCountsAndAllGoalsMet(t *testing.T) {
	d := testOpen(t)
	expID, err := d.CreateExploration("目标计数", "两条目标")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	es := d.Exploration(expID)
	g1, err := es.AddGoal(map[string]any{"text": "A"}, "human")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := es.AddGoal(map[string]any{"text": "B"}, "human"); err != nil {
		t.Fatal(err)
	}
	gc, err := es.GoalCounts()
	if err != nil {
		t.Fatal(err)
	}
	if gc.Total != 2 || gc.Met != 0 {
		t.Fatalf("初始计数应为 2/0，得到 %+v", gc)
	}
	if all, _ := es.AllGoalsMet(); all {
		t.Fatal("未全部达成时 AllGoalsMet 应为 false")
	}
	if _, err := es.AcceptGoalWithAudit(g1, "A 达成", 0, &GoalAcceptance{Kind: "flat", Source: "ledger"}, nil); err != nil {
		t.Fatal(err)
	}
	gc, _ = es.GoalCounts()
	if gc.Met != 1 {
		t.Fatalf("一条达成后 met 应为 1，得到 %+v", gc)
	}
	if all, _ := es.AllGoalsMet(); all {
		t.Fatal("还剩一条未达成")
	}
}
