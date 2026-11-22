package db

import (
	"context"
	"testing"
)

// 交易所 OCO/TP/SL 触发的平仓腿（clOrdId 形如 O4114…）在 trade_records 里没有行。
// 归属必须靠「持仓回合」，否则平仓腿被丢 → 净持仓算成未平 → 人工验收永远 409。
func TestTaskFillsIncludesAlgoCloseLeg(t *testing.T) {
	d := testOpen(t)
	ctx := context.Background()
	const sym = "EPISODE-TEST-SWAP"
	if _, err := d.Exec(`INSERT INTO instruments(market, symbol, asset_class, currency)
VALUES ('crypto',$1,'future','USDT')`, sym); err != nil {
		t.Fatal(err)
	}
	var instID int64
	if err := d.QueryRow(`SELECT id FROM instruments WHERE market='crypto' AND symbol=$1`, sym).Scan(&instID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		d.Exec(`DELETE FROM trade_fills WHERE inst_id=$1`, sym)
		d.Exec(`DELETE FROM trade_records WHERE inst_id=$1`, sym)
		d.Exec(`DELETE FROM instruments WHERE id=$1`, instID)
	})

	const openCID, closeCID = "artexo-episode-open", "artexo-episode-close"
	// 本任务：开 40 空 @689.889（有台账）→ 交易所 TP 平 40 @649.76（无台账，O 前缀）
	if _, err := d.Exec(`INSERT INTO trade_records(task_id, account_id, instrument_id, inst_id, action, side, qty,
client_order_id, state, mode, operator)
VALUES (9101,1,$1,$2,'open','sell','40',$3,'filled','live_auto','agent')`, instID, sym, openCID); err != nil {
		t.Fatal(err)
	}
	mk := func(fid, cid, side, sz, px, at string) {
		if _, _, err := d.InsertFill(TradeFill{
			FillID: fid, AccountID: 1, InstID: sym, Side: side, PosSide: "short",
			FillPx: px, FillSz: sz, ClOrdID: cid, FilledAt: at,
		}); err != nil {
			t.Fatal(err)
		}
	}
	mk(sym+":1", openCID, "sell", "40", "689.889", "2026-11-19T16:06:09Z")
	mk(sym+":2", "O4114102004303924224", "buy", "40", "649.76", "2026-11-19T18:16:34Z")

	// 别的任务：同账户同标的的另一回合（开 5 多 @100，未平）
	if _, err := d.Exec(`INSERT INTO trade_records(task_id, account_id, instrument_id, inst_id, action, side, qty,
client_order_id, state, mode, operator)
VALUES (9102,1,$1,$2,'open','buy','5','artexo-episode-other','filled','live_auto','agent')`, instID, sym); err != nil {
		t.Fatal(err)
	}
	mk(sym+":3", "artexo-episode-other", "buy", "5", "100", "2026-11-20T01:00:00Z")

	fills, err := d.TaskFills(ctx, 9101, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(fills) != 2 {
		t.Fatalf("本任务应拿到开仓+algo平仓 2 笔，得到 %d 笔: %+v", len(fills), fills)
	}
	var sawAlgo bool
	for _, f := range fills {
		if f.ClOrdID == "O4114102004303924224" {
			sawAlgo = true
		}
	}
	if !sawAlgo {
		t.Fatal("algo 平仓腿必须归属到本任务（否则净持仓永远算作未平）")
	}

	sum, err := TaskTradeSummaryFor(ctx, d, 9101, instID)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Fills != 2 || sum.OpenPositions != 0 {
		t.Fatalf("应 2 笔成交且已平仓，得到 %+v", sum)
	}
	// (689.889 - 649.76) * 40
	if sum.RealizedTotal != "1605.16" {
		t.Fatalf("已实现盈亏应为 1605.16，得到 %s", sum.RealizedTotal)
	}

	// 另一个任务不能被这一回合污染，且它自己的仓仍未平。
	other, err := TaskTradeSummaryFor(ctx, d, 9102, instID)
	if err != nil {
		t.Fatal(err)
	}
	if other.Fills != 1 || other.OpenPositions != 1 {
		t.Fatalf("任务 9102 应 1 笔未平仓，得到 %+v", other)
	}

	// 交易记录页走的 ListFills 必须与汇总同口径。
	listed, err := d.ListFills(FillsFilter{TaskID: 9101, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 {
		t.Fatalf("ListFills 应返回 2 笔（含 algo 平仓腿），得到 %d", len(listed))
	}
}

// 未平仓的回合也必须归属（持仓中）。回归：修复前只认台账 clOrdId，
// 有台账的开仓腿能上来、无台账的 algo 平仓腿丢了 —— 半截账。
func TestTaskFillsOpenEpisodeAttributed(t *testing.T) {
	d := testOpen(t)
	ctx := context.Background()
	const sym = "EPISODE-OPEN-SWAP"
	if _, err := d.Exec(`INSERT INTO instruments(market, symbol, asset_class, currency)
VALUES ('crypto',$1,'future','USDT')`, sym); err != nil {
		t.Fatal(err)
	}
	var instID int64
	if err := d.QueryRow(`SELECT id FROM instruments WHERE market='crypto' AND symbol=$1`, sym).Scan(&instID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		d.Exec(`DELETE FROM trade_fills WHERE inst_id=$1`, sym)
		d.Exec(`DELETE FROM trade_records WHERE inst_id=$1`, sym)
		d.Exec(`DELETE FROM instruments WHERE id=$1`, instID)
	})
	if _, err := d.Exec(`INSERT INTO trade_records(task_id, account_id, instrument_id, inst_id, action, side, qty,
client_order_id, state, mode, operator)
VALUES (9201,1,$1,$2,'open','buy','2','artexo-episode-open2','filled','live_auto','agent')`, instID, sym); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.InsertFill(TradeFill{
		FillID: sym + ":1", AccountID: 1, InstID: sym, Side: "buy", PosSide: "long",
		FillPx: "50", FillSz: "2", ClOrdID: "artexo-episode-open2", FilledAt: "2026-11-20T02:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	sum, err := TaskTradeSummaryFor(ctx, d, 9201, instID)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Fills != 1 || sum.OpenPositions != 1 || sum.NetQty[instID] != "2" {
		t.Fatalf("未平仓回合应被归属（2 张多），得到 %+v", sum)
	}
}
