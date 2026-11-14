package db

import (
	"context"
	"testing"
)

// 任务级成交汇总必须按 task_id 隔离：别的任务同标的成交不得污染本任务是否已平仓。
func TestTaskTradeSummaryForIsolatesByTask(t *testing.T) {
	d := testOpen(t)
	ctx := context.Background()
	res, err := d.Exec(`INSERT INTO instruments(market, symbol, asset_class, currency) VALUES ('us','TTS-ACCEPT-TEST','stock','USD')`)
	if err != nil {
		t.Fatal(err)
	}
	instID, _ := res.LastInsertId()
	t.Cleanup(func() {
		d.Exec(`DELETE FROM paper_orders WHERE instrument_id=$1`, instID)
		d.Exec(`DELETE FROM instruments WHERE id=$1`, instID)
	})

	insert := func(taskID int64, side, qty, px string) {
		if _, err := d.Exec(`INSERT INTO paper_orders(task_id, instrument_id, mode, side, qty, filled_qty, avg_fill_price, status, filled_at)
			VALUES ($1,$2,'live',$3,$4,$4,$5,'filled','2026-11-13T10:00:00Z')`, taskID, instID, side, qty, px); err != nil {
			t.Fatal(err)
		}
	}
	// 本任务：买 10@100 再卖 10@101 → 平仓 + 已实现 10
	insert(9001, "buy", "10", "100")
	insert(9001, "sell", "10", "101")
	// 别的任务：买 5@100 未平 —— 不得影响 9001 的 flat 判定
	insert(9002, "buy", "5", "100")

	sum, err := TaskTradeSummaryFor(ctx, d.DB, 9001, "live", instID)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Fills != 2 || sum.OpenPositions != 0 {
		t.Fatalf("本任务应 2 笔成交且已平仓，得到 %+v", sum)
	}
	if sum.RealizedTotal != "10.0000000000" {
		t.Fatalf("已实现盈亏应为 10.0000000000，得到 %s", sum.RealizedTotal)
	}
	other, err := TaskTradeSummaryFor(ctx, d.DB, 9002, "live", instID)
	if err != nil {
		t.Fatal(err)
	}
	if other.OpenPositions != 1 {
		t.Fatalf("别的任务仍有 1 个未平仓标的，得到 %+v", other)
	}
	empty, err := TaskTradeSummaryFor(ctx, d.DB, 9003, "live", instID)
	if err != nil || empty.Fills != 0 {
		t.Fatalf("无成交任务应返回空汇总: %+v err=%v", empty, err)
	}
}

// 归档/删除任务的残留 pass 草案不得出现在自动执行清单里 ——
// 否则任务归档之后仍会有真实下单（任务 19 就有 4 条历史 pass 草案）。
func TestListAutoExecutableExcludesArchivedTask(t *testing.T) {
	d := testOpen(t)
	res, err := d.Exec(`INSERT INTO instruments(market, symbol, asset_class, currency)
VALUES ('crypto','EXCL-ARCHIVE-SWAP','future','USDT')`)
	if err != nil {
		t.Fatal(err)
	}
	instID, _ := res.LastInsertId()
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM orders_draft WHERE instrument_id=$1`, instID)
		_, _ = d.Exec(`DELETE FROM signals WHERE instrument_id=$1`, instID)
		_, _ = d.Exec(`DELETE FROM tasks WHERE INSTR(goal,'excl-archive-test')>0`)
		_, _ = d.Exec(`DELETE FROM explorations WHERE goal='excl-archive-test'`)
		_, _ = d.Exec(`DELETE FROM instruments WHERE id=$1`, instID)
	})

	expID, err := d.CreateExploration("归档过滤", "excl-archive-test")
	if err != nil {
		t.Fatal(err)
	}
	var taskID int64
	if err := d.QueryRow(`INSERT INTO tasks(description,goal,exploration_id,archived_at)
VALUES ('x','excl-archive-test',$1,now()) RETURNING id`, expID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	var sigID int64
	if err := d.QueryRow(`INSERT INTO signals(task_id,instrument_id,signal_type,direction)
VALUES ($1,$2,'technical','long') RETURNING id`, taskID, instID).Scan(&sigID); err != nil {
		t.Fatal(err)
	}
	var draftID int64
	if err := d.QueryRow(`INSERT INTO orders_draft(signal_id,instrument_id,side,qty,ref_price,risk_verdict)
VALUES ($1,$2,'buy','1','100','pass') RETURNING id`, sigID, instID).Scan(&draftID); err != nil {
		t.Fatal(err)
	}

	list, err := d.OrderDrafts().ListAutoExecutable(50)
	if err != nil {
		t.Fatal(err)
	}
	for _, dr := range list {
		if dr.ID == draftID {
			t.Fatal("归档任务的草案不应出现在自动执行清单")
		}
	}
}
