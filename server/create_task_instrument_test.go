package server

// 「带标的建任务」的端到端测试。
//
// # 为什么这条测试重要
//
// 第 4 批做了「按 instrumentID 分叉金融/渗透 prompt」（agent/goals.go 的
// goalsBodyFor / goalsPromptKeyFor），但前端 api.createTask 从不传 instrument_id，
// 所以 tasks.instrument_id 恒为 NULL → instrumentID 恒为 0 → 永远走渗透 prompt。
//
// 那条分叉在**生产路径上从未被触发过**，只有单元测试（goals_domain_test.go）验证过它。
//
// 本条是第一个「真的传了 instrument_id」的端到端验证。
//
// # 为什么不断言「用了哪个 prompt」
//
// prompt key 不落库 —— goal 节点的 payload 只有 {"text": "..."}，没有 prompt 标记。
// 而「用了哪个 prompt」只能靠拦截 agent.PromptOverride（包级变量），
// 那在并行测试下会串（server 包有 3 处 t.Parallel）。
//
// 所以本条验证的是「instrument_id 真的写进了 tasks」+「阶段图真的建了」——
// 那证明 instrumentID 会传下去，而 goalsBodyFor/goalsPromptKeyFor 的分叉
// 已由 agent/goals_domain_test.go 的纯函数单测覆盖。

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// TestCreateTaskWithInstrumentWritesInstrumentID 验证：
//  1. 带 instrument_id 建任务不报「阶段图不能为空」
//  2. tasks.instrument_id 是**那个值**（不只是非空）
//  3. 阶段图节点数与 agent.Expand("stock") 一致
func TestCreateTaskWithInstrumentWritesInstrumentID(t *testing.T) {
	s, _, request := newAuthServer(t)

	instID := upsertTestInstrument(t, s.m.pg, db.InstrumentInput{
		Market: "us", Symbol: "TINSTR-CREATE", AssetClass: "stock",
	})

	w := request("POST", "/api/tasks", fmt.Sprintf(
		`{"name":"inst test","description":"d","goal":"判断该标的方向","instrument_id":%d}`, instID))
	if w.Code != 201 && w.Code != 200 {
		t.Fatalf("createTask: %d %s", w.Code, w.Body.String())
	}

	// 本测试自己调 handler 建了一个 task（fixture 的 task 是另一条）——
	// 必须显式删，否则 TestMain 的残留检查会报 explorations/nodes 泄漏。
	// DeleteTask 会级联删 exploration subgraph（nodes/edges/activity）。
	var taskID, expID int64
	if err := s.m.pg.QueryRow(
		`SELECT id, exploration_id FROM tasks WHERE instrument_id=$1`, instID,
	).Scan(&taskID, &expID); err != nil {
		t.Fatalf("建完任务但查不到 instrument_id=%d 的行: %v", instID, err)
	}
	t.Cleanup(func() { _ = s.m.pg.DeleteTask(taskID) })

	// ② 值是「那一个」，不只是「非空」——
	// 「非空」在「写错成另一个标的」时也成立。
	var gotInstID sql.NullInt64
	if err := s.m.pg.QueryRow(`SELECT instrument_id FROM tasks WHERE id=$1`, taskID).Scan(&gotInstID); err != nil {
		t.Fatalf("查 tasks.instrument_id: %v", err)
	}
	if !gotInstID.Valid {
		t.Fatal("instrument_id 为 NULL —— 这正是修复前的状态（前端从不传 → 恒 NULL）")
	}
	if gotInstID.Int64 != instID {
		t.Fatalf("instrument_id 写错：期望 %d，实际 %d", instID, gotInstID.Int64)
	}

	// ③ 阶段图节点数 == agent.Expand("stock").Nodes 的长度。
	//
	// 不硬编码数字：L5 恢复（分析师 1→4）会改变节点数，那时硬编码的断言会红，
	// 而红的理由会和「真的没建图」混在一起。对比 Expand 的输出则只验证
	// 「建图路径用的就是 Expand 那个函数」，与具体数字无关。
	// ⚠️ 只数阶段图节点（intent + join），不数根 fact 节点 ——
	// fact 是 CreateExploration 建的「探索根」，不属于阶段图。
	var joinCount, stageNodes int
	if err := s.m.pg.QueryRow(`SELECT
    count(*) FILTER (WHERE kind='join'),
    count(*) FILTER (WHERE kind IN ('intent','join'))
 FROM exploration_nodes WHERE exploration_id=$1`, expID,
	).Scan(&joinCount, &stageNodes); err != nil {
		t.Fatal(err)
	}
	if joinCount == 0 {
		t.Fatal("阶段图未建出 join 节点 —— MaterializeGraph 没执行")
	}
	graph, err := agent.Expand("stock", agent.ExpandOpts{InstrumentID: instID, AsOf: time.Now().UTC()})
	if err != nil {
		t.Fatalf("agent.Expand(stock): %v", err)
	}
	if stageNodes != len(graph.Nodes) {
		// 失败时打出实际分布 —— 否则只看到「14 ≠ 13」不知差在哪。
		rows, _ := s.m.pg.Query(`SELECT kind, count(*) FROM exploration_nodes WHERE exploration_id=$1 GROUP BY kind ORDER BY 2 DESC`, expID)
		var kinds []string
		if rows != nil {
			for rows.Next() {
				var k string
				var c int
				if rows.Scan(&k, &c) == nil {
					kinds = append(kinds, fmt.Sprintf("%s=%d", k, c))
				}
			}
			rows.Close()
		}
		t.Fatalf("阶段图节点数 %d ≠ Expand 展开的 %d；实际分布：%v", stageNodes, len(graph.Nodes), kinds)
	}
	t.Logf("阶段图：%d 个节点（intent+join），其中 join %d 个", stageNodes, joinCount)
}
