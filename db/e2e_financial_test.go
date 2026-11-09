package db

// 端到端验证：建带 instrument_id 的金融任务，确认「回调在事务内被调用」且
// tasks.instrument_id 落库。
//
// # 为什么这个测试不 import agent
//
// db 包不能 import agent（agent 已 import db，会成环）。生产代码用
// `TaskCreateOptions.MaterializeGraph` 注入回调解耦，测试也一样 ——
// 这里注入一个假回调，验证 db 侧契约：
//   1. 传了 InstrumentID 但没注入回调 → 拒绝创建
//   2. 注入了回调 → 回调在事务内被调用（拿到非 nil tx）
//   3. 回调报错 → 整个任务创建回滚（不留半成品）
//   4. tasks.instrument_id 落库
//
// 图本身的落库（16/12 节点 + gates 边）由 agent/materialize_test.go 覆盖。
// 两者合起来才是完整的端到端 —— 分开是因为包依赖方向决定的。

import (
	"database/sql"
	"errors"
	"testing"
)

// TestCreateFinancialTaskRequiresMaterializer 传了 instrument_id 但没注入回调必须拒绝。
func TestCreateFinancialTaskRequiresMaterializer(t *testing.T) {
	d, _ := instrumentSetup(t)

	instID, err := d.Instruments().InsertInstrument(InstrumentInput{
		Market: "us", Symbol: uniqueInstrumentSymbol(t), AssetClass: "etf",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM instruments WHERE id=$1`, instID) })

	_, err = d.CreateTaskWithOptions("fin task", "", TaskCreateOptions{InstrumentID: &instID})
	if err == nil {
		t.Fatal("传了 instrument_id 但没注入 MaterializeGraph，应当拒绝 —— 否则任务建了图没建，engine 派发空图会卡住")
	}
	t.Logf("✓ 正确拒绝: %v", err)

	// 拒绝后库里不该有任务
	var n int
	d.QueryRow(`SELECT count(*) FROM tasks WHERE description='fin task'`).Scan(&n)
	if n != 0 {
		t.Errorf("拒绝后仍留下 %d 个任务", n)
	}
}

// TestCreateFinancialTaskInvokesMaterializerInTx 回调必须在事务内被调用。
func TestCreateFinancialTaskInvokesMaterializerInTx(t *testing.T) {
	d, _ := instrumentSetup(t)

	sym := uniqueInstrumentSymbol(t)
	instID, err := d.Instruments().InsertInstrument(InstrumentInput{
		Market: "us", Symbol: sym, AssetClass: "etf",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM instruments WHERE id=$1`, instID) })

	var (
		called  bool
		gotExp  int64
		txInUse bool
	)
	task, err := d.CreateTaskWithOptions("fin task tx", "goal", TaskCreateOptions{
		InstrumentID: &instID,
		MaterializeGraph: func(tx *sql.Tx, expID int64) error {
			called = true
			gotExp = expID
			txInUse = tx != nil
			// 在事务内写一个节点，验证 tx 真的可用
			_, err := tx.Exec(`INSERT INTO exploration_nodes(exploration_id,kind,payload,priority,state,origin)
VALUES ($1,'intent','{"role":"test_probe"}',1,'open','system')`, expID)
			return err
		},
	})
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })

	if !called {
		t.Fatal("MaterializeGraph 回调没被调用")
	}
	if !txInUse {
		t.Fatal("回调拿到的 tx 是 nil —— 不是同事务")
	}
	if gotExp <= 0 {
		t.Errorf("回调拿到的 expID=%d，应当 > 0", gotExp)
	}
	t.Logf("✓ 回调在事务内被调用: expID=%d", gotExp)

	// tx 内写的节点应当可见（说明真的提交了）
	var probe int
	if err := d.QueryRow(`SELECT count(*) FROM exploration_nodes
		WHERE exploration_id=$1 AND payload->>'role'='test_probe'`, gotExp).Scan(&probe); err != nil {
		t.Fatal(err)
	}
	if probe != 1 {
		t.Errorf("探针节点数=%d，期望 1（回调写的节点应已随事务提交）", probe)
	}

	// instrument_id 落库
	var taskInst sql.NullInt64
	if err := d.QueryRow(`SELECT instrument_id FROM tasks WHERE id=$1`, task.ID).Scan(&taskInst); err != nil {
		t.Fatal(err)
	}
	if !taskInst.Valid || taskInst.Int64 != instID {
		t.Errorf("tasks.instrument_id=%v，期望 %d", taskInst, instID)
	}
	t.Logf("✓ tasks.instrument_id=%d", taskInst.Int64)
}

// TestCreateFinancialTaskRollsBackOnMaterializerError 回调报错必须整体回滚。
func TestCreateFinancialTaskRollsBackOnMaterializerError(t *testing.T) {
	d, _ := instrumentSetup(t)

	sym := uniqueInstrumentSymbol(t)
	instID, err := d.Instruments().InsertInstrument(InstrumentInput{
		Market: "us", Symbol: sym, AssetClass: "etf",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM instruments WHERE id=$1`, instID) })

	beforeTasks := countAll(t, d)
	beforeExp := countTable(t, d, "explorations")
	beforeNodes := countTable(t, d, "exploration_nodes")

	boom := errors.New("模拟阶段图落库失败")
	_, err = d.CreateTaskWithOptions("fin rollback", "goal", TaskCreateOptions{
		InstrumentID: &instID,
		MaterializeGraph: func(tx *sql.Tx, expID int64) error {
			// 先写一半，再报错 —— 验证「建到一半失败也整体回滚」
			if _, e := tx.Exec(`INSERT INTO exploration_nodes(exploration_id,kind,payload,priority,state,origin)
VALUES ($1,'intent','{}',1,'open','system')`, expID); e != nil {
				return e
			}
			return boom
		},
	})
	if err == nil {
		t.Fatal("回调报错时应当创建失败")
	}
	if !errors.Is(err, boom) {
		t.Logf("错误被包装（可接受）: %v", err)
	}

	// 三张表都必须回到 before 状态
	afterTasks := countAll(t, d)
	afterExp := countTable(t, d, "explorations")
	afterNodes := countTable(t, d, "exploration_nodes")

	if afterTasks != beforeTasks || afterExp != beforeExp || afterNodes != beforeNodes {
		t.Errorf("回滚不干净：tasks %d→%d / explorations %d→%d / nodes %d→%d\n"+
			"「任务建了但图没建」会让 engine 派发空图、任务卡住且无明确错误",
			beforeTasks, afterTasks, beforeExp, afterExp, beforeNodes, afterNodes)
	}
	t.Logf("✓ 回滚干净: tasks=%d explorations=%d nodes=%d", afterTasks, afterExp, afterNodes)
}

func countAll(t *testing.T, d *DB) int {
	t.Helper()
	return countTable(t, d, "tasks")
}

func countTable(t *testing.T, d *DB, table string) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}
