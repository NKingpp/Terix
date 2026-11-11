package server

// 阶段图建图路径的验收测试。
//
// # 为什么测 stageGraphMaterializer 而不是直接跑 spawn_task
//
// spawn_task 会真的启动探索引擎（后台 goroutine + LLM 调用），在单测里既慢又
// 不稳定。而它建图那部分的风险全在**建图函数本身**：
//   - 无标任务是否只建 screener（多建会产出 13 个 instrument_id=0 的空节点）
//   - 有标任务是否建出完整图
//   - 事务内是否幂等（ApplyStageGraph 的版本标记）
//
// 这三条都能用 stageGraphMaterializer 直接验证，不需要起引擎。
// spawn_task 侧的改动只是「调用这个函数」，逻辑风险已经在这里覆盖。

import (
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// countStageNodes 数阶段图节点（intent + join，不含根的 origin fact）。
func countStageNodes(t *testing.T, d *db.DB, expID int64) (int, int) {
	t.Helper()
	var joins, total int
	if err := d.QueryRow(`SELECT
    count(*) FILTER (WHERE kind='join'),
    count(*) FILTER (WHERE kind IN ('intent','join'))
 FROM exploration_nodes WHERE exploration_id=$1`, expID).Scan(&joins, &total); err != nil {
		t.Fatalf("数节点: %v", err)
	}
	return joins, total
}

// TestStageGraphMaterializerWithInstrument 有标的 → 建出完整图。
func TestStageGraphMaterializerWithInstrument(t *testing.T) {
	s, _, _ := newAuthServer(t)

	instID := upsertTestInstrument(t, s.m.pg, db.InstrumentInput{
		Market: "us", Symbol: "TINSTR-SPAWN", AssetClass: "stock",
	})
	inst, err := s.m.pg.Instruments().GetInstrumentByID(instID)
	if err != nil || inst == nil {
		t.Fatalf("取标的: %v", err)
	}

	expID := createTestExploration(t, s.m.pg)

	tx, err := s.m.pg.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := s.stageGraphMaterializer(inst)(tx, expID); err != nil {
		tx.Rollback()
		t.Fatalf("materialize: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	joins, total := countStageNodes(t, s.m.pg, expID)
	if joins == 0 {
		t.Fatal("没有 join 节点 —— 完整图必然含 join_analysts / join_risk")
	}
	// 与 Expand 的输出对比，不硬编码数字（模板演进时数字会变）
	g, err := agent.Expand("stock", agent.ExpandOpts{InstrumentID: instID, AsOf: time.Now().UTC()})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if total != len(g.Nodes) {
		t.Errorf("阶段节点数 %d != Expand 输出 %d", total, len(g.Nodes))
	}

	// 有标的的图不该有 screener（那是无标任务的第一阶段）
	var screenerCount int
	if err := s.m.pg.QueryRow(
		`SELECT count(*) FROM exploration_nodes
		  WHERE exploration_id=$1 AND json_extract(payload,'$.role')='screener'`,
		expID).Scan(&screenerCount); err != nil {
		t.Fatalf("数 screener: %v", err)
	}
	if screenerCount != 0 {
		t.Errorf("有标的的任务不该建 screener 节点，实际 %d 个", screenerCount)
	}
}

// TestStageGraphMaterializerWithoutInstrument 无标的 → 只建 screener。
//
// # 这是本设计的核心约束
//
// 下游 13 个节点全都要 instrument_id。无标时先建出来只会是空标的节点，
// 分析师拿到 instrument_id=0 后全部判「标的无法分析」（实测浪费一整轮）。
// 所以无标时必须 ScreenerOnly，等 set_task_instrument 再展开。
func TestStageGraphMaterializerWithoutInstrument(t *testing.T) {
	s, _, _ := newAuthServer(t)
	expID := createTestExploration(t, s.m.pg)

	tx, err := s.m.pg.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := s.stageGraphMaterializer(nil)(tx, expID); err != nil {
		tx.Rollback()
		t.Fatalf("materialize: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	_, total := countStageNodes(t, s.m.pg, expID)
	if total != 1 {
		t.Errorf("无标任务应只建 1 个节点（screener），实际 %d", total)
	}
	var role string
	if err := s.m.pg.QueryRow(
		`SELECT COALESCE(json_extract(payload,'$.role'),'') FROM exploration_nodes
		  WHERE exploration_id=$1 AND kind='intent'`, expID).Scan(&role); err != nil {
		t.Fatalf("查节点 role: %v", err)
	}
	if role != agent.RoleScreener {
		t.Errorf("唯一的节点应是 screener，实际 role=%q", role)
	}
}

// TestStageGraphMaterializerIsIdempotent 重复调用不重复建图。
//
// ApplyStageGraph 的版本标记保证这一点。幂等很重要：spawn_task 与 HTTP createTask
// 共用同一个回调，将来若出现「任务重跑」也不会建出两套节点。
func TestStageGraphMaterializerIsIdempotent(t *testing.T) {
	s, _, _ := newAuthServer(t)
	instID := upsertTestInstrument(t, s.m.pg, db.InstrumentInput{
		Market: "us", Symbol: "TINSTR-IDEM", AssetClass: "stock",
	})
	inst, _ := s.m.pg.Instruments().GetInstrumentByID(instID)
	expID := createTestExploration(t, s.m.pg)

	apply := func() error {
		tx, err := s.m.pg.Begin()
		if err != nil {
			return err
		}
		if err := s.stageGraphMaterializer(inst)(tx, expID); err != nil {
			tx.Rollback()
			return err
		}
		return tx.Commit()
	}

	if err := apply(); err != nil {
		t.Fatalf("首次: %v", err)
	}
	_, first := countStageNodes(t, s.m.pg, expID)

	if err := apply(); err != nil {
		t.Fatalf("二次: %v", err)
	}
	_, second := countStageNodes(t, s.m.pg, expID)

	if first != second {
		t.Errorf("重复调用建了重复节点：%d → %d（ApplyStageGraph 的幂等失效）", first, second)
	}
}

// TestStageGraphMaterializerUnknownAssetClass 未知资产类别要让建任务失败，而不是静默跳过。
//
// 静默跳过会建出一个空 exploration：任务列表里看着正常，实际没有任何阶段图，
// 全是通用 worker —— 与本次要修的问题一模一样。
func TestStageGraphMaterializerUnknownAssetClass(t *testing.T) {
	s, _, _ := newAuthServer(t)
	instID := upsertTestInstrument(t, s.m.pg, db.InstrumentInput{
		Market: "us", Symbol: "TINSTR-FUT", AssetClass: "future",
	})
	inst, _ := s.m.pg.Instruments().GetInstrumentByID(instID)
	expID := createTestExploration(t, s.m.pg)

	tx, err := s.m.pg.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	if err := s.stageGraphMaterializer(inst)(tx, expID); err == nil {
		t.Error("future 资产类别应报错（Expand 不支持），不该静默建空图")
	}
}

// createTestExploration 建一个测试用 exploration（含根 fact），结束自动清理。
//
// # 为什么必须建根 fact
//
// ApplyStageGraph 把「阶段图已应用」的版本记在根 fact 的 payload 上
// （readStageGraphVersion 找 kind='fact' AND state='origin'）。
// 没有根节点时 rootID==0 → 无法记版本 → **幂等失效**，每次调用都重新建图。
//
// 生产路径上 db.CreateTaskWithOptions 会建这个根 fact，所以测试里也要建，
// 否则测的不是生产形态。这一点是我第一版漏掉的：幂等测试从 15 变成 30 才发现。
func createTestExploration(t *testing.T, d *db.DB) int64 {
	t.Helper()
	id, err := d.CreateExploration("stage-graph 测试", "验证建图路径")
	if err != nil {
		t.Fatalf("CreateExploration: %v", err)
	}
	t.Cleanup(func() {
		// 节点由外键级联删；失败只记日志（避免掩盖真正的断言失败）
		if _, err := d.Exec(`DELETE FROM explorations WHERE id=$1`, id); err != nil {
			t.Logf("清理 exploration %d: %v", id, err)
		}
	})
	// 根 fact —— ApplyStageGraph 把版本标记记在它上面。
	if _, err := d.Exploration(id).AddNode(db.KindFact, map[string]any{
		"text": "任务根节点",
	}, 0, db.StateOrigin, "system", nil); err != nil {
		t.Fatalf("建根 fact: %v", err)
	}
	return id
}
