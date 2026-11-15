package db

import (
	"fmt"
	"testing"
)

// 成交明细的幂等：同一个 fill_id 重复写入只算一次。
// 这条不成立会让盈亏翻倍（每轮同步都把同一笔成交再记一遍），
// 而且表现是「看起来赚多了」—— 最难被发现的错误方向。
func TestInsertFillIdempotent(t *testing.T) {
	d := testOpen(t)
	f := TradeFill{
		FillID: "TESTFILL-1", AccountID: 1, InstID: "IDEM-USDT-SWAP",
		Side: "buy", PosSide: "net", FillPx: "100", FillSz: "1", Fee: "-0.05",
		FeeCcy: "USDT", OrdID: "ord1", ClOrdID: "artex1", FilledAt: "2026-11-14T01:00:00Z",
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM trade_fills WHERE fill_id LIKE 'TESTFILL-%'`) })

	id1, ins1, err := d.InsertFill(f)
	if err != nil || !ins1 || id1 <= 0 {
		t.Fatalf("首次写入应成功: id=%d inserted=%v err=%v", id1, ins1, err)
	}
	id2, ins2, err := d.InsertFill(f)
	if err != nil {
		t.Fatal(err)
	}
	if ins2 || id2 != 0 {
		t.Fatalf("重复写入应被忽略（inserted=false），得到 id=%d inserted=%v", id2, ins2)
	}
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM trade_fills WHERE fill_id=$1`, f.FillID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应只有 1 行，得到 %d", n)
	}
}

// 任务级查询：成交必须能通过 cl_ord_id → orders_execution → orders_draft → signals 关联到任务。
// 这条链断了，交易记录页会永远显示空。
func TestListFillsByTask(t *testing.T) {
	d := testOpen(t)
	// 账户最先建、cleanup 最先注册 —— t.Cleanup 是 LIFO，
	// 后注册的先跑；账户被 orders_execution 外键引用，必须最后删。
	acct, err := d.ExecAccounts().UpsertAccount(ExecAccountInput{
		Channel: "okx", ChannelAcctID: "tfills-test-" + t.Name(),
		Label: "成交归属测试", Market: "crypto",
		MarketSupport: []string{"crypto"}, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM orders_execution WHERE account_id=$1`, acct.ID)
		_, _ = d.Exec(`DELETE FROM exec_accounts WHERE id=$1`, acct.ID)
	})

	expID, err := d.CreateExploration("成交归属", "trade-fills-task-link")
	if err != nil {
		t.Fatal(err)
	}
	var taskID int64
	if err := d.QueryRow(`INSERT INTO tasks(description,goal,exploration_id)
VALUES ('x','trade-fills-task-link',$1) RETURNING id`, expID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	res, err := d.Exec(`INSERT INTO instruments(market, symbol, asset_class, currency)
VALUES ('crypto','TFILL-USDT-SWAP','future','USDT')`)
	if err != nil {
		t.Fatal(err)
	}
	instID, _ := res.LastInsertId()
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
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM trade_fills WHERE fill_id LIKE 'TFILL-%'`)
		_, _ = d.Exec(`DELETE FROM orders_execution WHERE draft_id=$1`, draftID)
		_, _ = d.Exec(`DELETE FROM orders_draft WHERE id=$1`, draftID)
		_, _ = d.Exec(`DELETE FROM signals WHERE id=$1`, sigID)
		_, _ = d.Exec(`DELETE FROM tasks WHERE id=$1`, taskID)
		_, _ = d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
		_, _ = d.Exec(`DELETE FROM instruments WHERE id=$1`, instID)
	})

	// 一笔订单执行记录（cl_ord_id 是枢纽）。
	if _, err := d.Exec(`INSERT INTO orders_execution(draft_id,account_id,mode,state,client_order_id,qty)
VALUES ($1,$2,'live_auto','submitted','TFILLARTEX1','1')`, draftID, acct.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.InsertFill(TradeFill{
		FillID: "TFILL-1", AccountID: acct.ID, InstID: "TFILL-USDT-SWAP", Side: "buy",
		PosSide: "net", FillPx: "100", FillSz: "1", Fee: "-0.05", FeeCcy: "USDT",
		ClOrdID: "TFILLARTEX1", FilledAt: "2026-11-14T01:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}

	fills, err := d.ListFills(FillsFilter{TaskID: taskID})
	if err != nil {
		t.Fatal(err)
	}
	if len(fills) != 1 || fills[0].FillID != "TFILL-1" {
		t.Fatalf("应查到 1 笔本任务成交，得到 %+v", fills)
	}

	// 别的任务查不到
	otherExp, err := d.CreateExploration("别的任务", "other-task")
	if err != nil {
		t.Fatal(err)
	}
	var otherTask int64
	if err := d.QueryRow(`INSERT INTO tasks(description,goal,exploration_id)
VALUES ('y','other-task',$1) RETURNING id`, otherExp).Scan(&otherTask); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM tasks WHERE id=$1`, otherTask)
		_, _ = d.Exec(`DELETE FROM explorations WHERE id=$1`, otherExp)
	})
	other, err := d.ListFills(FillsFilter{TaskID: otherTask})
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 0 {
		t.Fatalf("别的任务不该看到这笔成交，得到 %d 笔", len(other))
	}
}

// clOrdId 为空时必须仍能通过 ordId 归属到任务。
//
// 2026-11-14 实测：OKX 对部分成交的 clOrdId 返回空串（手工 CLI 下的单）。
// 只靠 cl_ord_id 关联会让交易记录页在该情形下永远空白 ——
// 那是「功能看起来做了但从不出数据」的典型失败。
func TestListFillsFallsBackToOrdID(t *testing.T) {
	d := testOpen(t)
	acct, err := d.ExecAccounts().UpsertAccount(ExecAccountInput{
		Channel: "okx", ChannelAcctID: "tfillordid-" + t.Name(),
		Label: "ordId 兜底", Market: "crypto", MarketSupport: []string{"crypto"}, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	expID, _ := d.CreateExploration("ordId 兜底", "tfill-ordid")
	var taskID int64
	if err := d.QueryRow(`INSERT INTO tasks(description,goal,exploration_id)
VALUES ('x','tfill-ordid',$1) RETURNING id`, expID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	res, _ := d.Exec(`INSERT INTO instruments(market, symbol, asset_class, currency)
VALUES ('crypto','ORDID-USDT-SWAP','future','USDT')`)
	instID, _ := res.LastInsertId()
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
	// 清理顺序必须按外键依赖：execution → draft → signal → task → exploration → instrument。
	// 反了会被 FK RESTRICT 挡住，而 Delete 的错误被吞掉 —— 表现为「残留 1 行」。
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM trade_fills WHERE fill_id LIKE 'ORDID-FILL-%'`)
		_, _ = d.Exec(`DELETE FROM orders_execution WHERE account_id=$1`, acct.ID)
		_, _ = d.Exec(`DELETE FROM orders_draft WHERE id=$1`, draftID)
		_, _ = d.Exec(`DELETE FROM signals WHERE id=$1`, sigID)
		_, _ = d.Exec(`DELETE FROM tasks WHERE id=$1`, taskID)
		_, _ = d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
		_, _ = d.Exec(`DELETE FROM instruments WHERE id=$1`, instID)
		_, _ = d.Exec(`DELETE FROM exec_accounts WHERE id=$1`, acct.ID)
	})
	// execution 只有 channel_order_id，没有 client_order_id 的对应关系
	if _, err := d.Exec(`INSERT INTO orders_execution(draft_id,account_id,mode,state,client_order_id,channel_order_id,qty)
VALUES ($1,$2,'live_auto','filled','SOMETHINGELSE','ORDID-CH-1','1')`, draftID, acct.ID); err != nil {
		t.Fatal(err)
	}
	// 成交的 cl_ord_id 为空，但有 ord_id
	if _, _, err := d.InsertFill(TradeFill{
		FillID: "ORDID-FILL-1", AccountID: acct.ID, InstID: "ORDID-USDT-SWAP", Side: "buy",
		PosSide: "net", FillPx: "100", FillSz: "1", Fee: "-0.05", FeeCcy: "USDT",
		OrdID: "ORDID-CH-1", ClOrdID: "", FilledAt: "2026-11-14T01:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}

	fills, err := d.ListFills(FillsFilter{TaskID: taskID})
	if err != nil {
		t.Fatal(err)
	}
	if len(fills) != 1 {
		t.Fatalf("clOrdId 为空时应通过 ordId 归属到任务，得到 %d 笔", len(fills))
	}
}

// 交易所 OCO 触发的平仓成交无法靠 cl_ord_id 归属（那是交易所的 clOrdId），
// 必须靠「同账户同标的最近一条持仓跟踪」兜底。
//
// 不兜底的后果：任务盈亏里只有开仓腿，持仓永远显示未平（实测任务 23）。
func TestDraftIDByInstRecently(t *testing.T) {
	d := testOpen(t)
	acct, err := d.ExecAccounts().UpsertAccount(ExecAccountInput{
		Channel: "okx", ChannelAcctID: "instrec-" + t.Name(),
		Label: "归属兜底", Market: "crypto", MarketSupport: []string{"crypto"}, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM live_positions WHERE account_id=$1`, acct.ID)
		_, _ = d.Exec(`DELETE FROM exec_accounts WHERE id=$1`, acct.ID)
	})
	if _, err := d.OpenLivePosition(LivePosition{
		AccountID: acct.ID, InstID: "RECHK-USDT-SWAP", PosSide: "long", Qty: "1", DraftID: 777,
	}); err != nil {
		t.Fatal(err)
	}
	if got := d.DraftIDByInstRecently(acct.ID, "RECHK-USDT-SWAP"); got != 777 {
		t.Fatalf("应反查到 777，得到 %d", got)
	}
	if got := d.DraftIDByInstRecently(acct.ID, "NOPE-USDT-SWAP"); got != 0 {
		t.Fatalf("不存在的标的应返回 0，得到 %d", got)
	}
	if got := d.DraftIDByInstRecently(0, "RECHK-USDT-SWAP"); got != 0 {
		t.Fatalf("account=0 应返回 0，得到 %d", got)
	}
}

// 归属必须按持仓时间窗，不能只按 instId。
//
// 同一标的会反复开平。只按 instId 会把历史上所有无人认领的成交都算到
// 最新草案上 —— 实测 open_qty 从 0.1 变成 -0.2，比不归属更糟。
func TestBackfillFillDraftIDByWindow(t *testing.T) {
	d := testOpen(t)
	acct, err := d.ExecAccounts().UpsertAccount(ExecAccountInput{
		Channel: "okx", ChannelAcctID: "win-" + t.Name(),
		Label: "窗口归属", Market: "crypto", MarketSupport: []string{"crypto"}, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM trade_fills WHERE fill_id LIKE 'WIN-%'`)
		_, _ = d.Exec(`DELETE FROM live_positions WHERE account_id=$1`, acct.ID)
		_, _ = d.Exec(`DELETE FROM exec_accounts WHERE id=$1`, acct.ID)
	})

	inst := "WIN-USDT-SWAP"
	// 持仓窗口：08:00 ~ 09:00
	if _, err := d.OpenLivePosition(LivePosition{
		AccountID: acct.ID, InstID: inst, PosSide: "long", Qty: "1", DraftID: 555,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE live_positions SET opened_at='2026-11-14T08:00:00Z', closed_at='2026-11-14T09:00:00Z'
WHERE account_id=$1 AND inst_id=$2`, acct.ID, inst); err != nil {
		t.Fatal(err)
	}

	// 窗口内（应归属）与窗口外（不该归属）各一笔，cl_ord_id 都是交易所生成的
	for _, f := range []struct {
		id, ts string
	}{
		{"WIN-IN", "2026-11-14T08:30:00Z"},
		{"WIN-OUT", "2026-11-14T07:00:00Z"},
	} {
		if _, _, err := d.InsertFill(TradeFill{
			FillID: f.id, AccountID: acct.ID, InstID: inst, Side: "sell",
			PosSide: "net", FillPx: "100", FillSz: "1", Fee: "-0.01",
			ClOrdID: "O4098EXCHANGE", FilledAt: f.ts,
		}); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := d.BackfillFillDraftID(acct.ID, inst, 555); err != nil {
		t.Fatal(err)
	}

	var inDraft, outDraft *int64
	if err := d.QueryRow(`SELECT draft_id FROM trade_fills WHERE fill_id='WIN-IN'`).Scan(&inDraft); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT draft_id FROM trade_fills WHERE fill_id='WIN-OUT'`).Scan(&outDraft); err != nil {
		t.Fatal(err)
	}
	if inDraft == nil || *inDraft != 555 {
		t.Errorf("窗口内的成交应归属 555，得到 %v", inDraft)
	}
	if outDraft != nil {
		t.Errorf("窗口外的成交不该归属，得到 %v", *outDraft)
	}
}

// 同一标的的多次持仓必须各自归属，互不覆盖。
//
// 早先的版本复位全标的的行，只有最后一次回填生效 —— 任务 21/22 的
// 平仓腿永远补不上（持仓永远显示未平）。
func TestBackfillMultiplePositionsSameInst(t *testing.T) {
	d := testOpen(t)
	acct, err := d.ExecAccounts().UpsertAccount(ExecAccountInput{
		Channel: "okx", ChannelAcctID: "mul-" + t.Name(),
		Label: "多仓归属", Market: "crypto", MarketSupport: []string{"crypto"}, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM trade_fills WHERE fill_id LIKE 'MUL-%'`)
		_, _ = d.Exec(`DELETE FROM live_positions WHERE account_id=$1`, acct.ID)
		_, _ = d.Exec(`DELETE FROM exec_accounts WHERE id=$1`, acct.ID)
	})

	inst := "MUL-USDT-SWAP"
	// 两次持仓：08:00-09:00（草案 601）、10:00-11:00（草案 602）
	for i, spec := range []struct {
		draft, opened, closed string
	}{
		{"601", "2026-11-14T08:00:00Z", "2026-11-14T09:00:00Z"},
		{"602", "2026-11-14T10:00:00Z", "2026-11-14T11:00:00Z"},
	} {
		_ = i
		var did int64
		_, _ = fmt.Sscanf(spec.draft, "%d", &did)
		if _, err := d.OpenLivePosition(LivePosition{
			AccountID: acct.ID, InstID: inst, PosSide: "long", Qty: "1", DraftID: did,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Exec(`UPDATE live_positions SET opened_at=$1, closed_at=$2
WHERE account_id=$3 AND inst_id=$4 AND draft_id=$5`, spec.opened, spec.closed, acct.ID, inst, did); err != nil {
			t.Fatal(err)
		}
	}

	for _, f := range []struct{ id, ts string }{
		{"MUL-A", "2026-11-14T08:30:00Z"},
		{"MUL-B", "2026-11-14T10:30:00Z"},
	} {
		if _, _, err := d.InsertFill(TradeFill{
			FillID: f.id, AccountID: acct.ID, InstID: inst, Side: "sell",
			PosSide: "net", FillPx: "100", FillSz: "1", Fee: "0",
			ClOrdID: "OEXCH1", FilledAt: f.ts,
		}); err != nil {
			t.Fatal(err)
		}
	}

	for _, did := range []int64{601, 602} {
		if _, err := d.BackfillFillDraftID(acct.ID, inst, did); err != nil {
			t.Fatal(err)
		}
	}

	var a, b *int64
	_ = d.QueryRow(`SELECT draft_id FROM trade_fills WHERE fill_id='MUL-A'`).Scan(&a)
	_ = d.QueryRow(`SELECT draft_id FROM trade_fills WHERE fill_id='MUL-B'`).Scan(&b)
	if a == nil || *a != 601 {
		t.Errorf("MUL-A 应归属 601，得到 %v", a)
	}
	if b == nil || *b != 602 {
		t.Errorf("MUL-B 应归属 602，得到 %v", b)
	}
}

// AccountIDsForTask 必须覆盖工具下单（trade_records）。
//
// 只查 orders_execution 会让工具下的单查不到账户，同步入口直接 400：
// 「本任务还没有与任何执行账户关联的下单记录」—— 而那些单其实就挂在
// 这个账户上，只是没走草案链路。
func TestAccountIDsForTaskIncludesTradeRecords(t *testing.T) {
	d := testOpen(t)
	acct, err := d.ExecAccounts().UpsertAccount(ExecAccountInput{
		Channel: "okx", ChannelAcctID: "tr-acct-" + t.Name(),
		Label: "工具台账账户", Market: "crypto", MarketSupport: []string{"crypto"}, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM trade_records WHERE account_id=$1`, acct.ID)
		_, _ = d.Exec(`DELETE FROM exec_accounts WHERE id=$1`, acct.ID)
	})
	if _, err := d.InsertTradeRecord(TradeRecord{
		TaskID: 987654, AccountID: acct.ID, InstID: "TASKCHK-USDT-SWAP",
		Action: "open", Side: "buy", Qty: "1", ClientOrderID: "artextaskchk01",
		State: TradeRecordSubmitted, Mode: "live_auto",
	}); err != nil {
		t.Fatal(err)
	}
	ids, err := d.AccountIDsForTask(987654)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, id := range ids {
		if id == acct.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("应查到账户 %d，实际 %v（工具下单没被算进账户归属）", acct.ID, ids)
	}
}

// 成交同步必须覆盖工具下单的标的。
//
// 只从 orders_execution 推导标的列表时，工具下的单永远不会被同步 ——
// 交易所已有仓、台账已落，但任务详情里的实盘成交与盈亏恒为 0
// （实测任务 29：BTC-USDT-SWAP 有仓，live fills=0）。
func TestDistinctInstIDsIncludesTradeRecords(t *testing.T) {
	d := testOpen(t)
	acct, err := d.ExecAccounts().UpsertAccount(ExecAccountInput{
		Channel: "okx", ChannelAcctID: "instids-" + t.Name(),
		Label: "标的来源", Market: "crypto", MarketSupport: []string{"crypto"}, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM trade_records WHERE account_id=$1`, acct.ID)
		_, _ = d.Exec(`DELETE FROM exec_accounts WHERE id=$1`, acct.ID)
	})
	if _, err := d.InsertTradeRecord(TradeRecord{
		TaskID: 123456, AccountID: acct.ID, InstID: "PERPONLY-USDT-SWAP",
		Action: "open", Side: "buy", Qty: "1", ClientOrderID: "artexperponly1",
		State: TradeRecordSubmitted, Mode: "live_auto",
	}); err != nil {
		t.Fatal(err)
	}
	ids, err := d.DistinctInstIDsForAccount(acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range ids {
		if s == "PERPONLY-USDT-SWAP" {
			found = true
		}
	}
	if !found {
		t.Fatalf("应包含工具下单的标的，实际 %v（成交永远不会被同步）", ids)
	}
}

// 本地净持仓汇总：对账的基础。
//
// 2026-11-15 的不一致就是「这个函数算出来 -68.95，交易所是 0」。
// 所以它必须只做纯粹的买卖相减，不含成本/盈亏口径
// （那些是派生值，不能用来互相校验）。
func TestLocalNetPositions(t *testing.T) {
	d := testOpen(t)
	acct, err := d.ExecAccounts().UpsertAccount(ExecAccountInput{
		Channel: "okx", ChannelAcctID: "netpos-" + t.Name(),
		Label: "净持仓", Market: "crypto", MarketSupport: []string{"crypto"}, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM trade_fills WHERE account_id=$1`, acct.ID)
		_, _ = d.Exec(`DELETE FROM exec_accounts WHERE id=$1`, acct.ID)
	})

	ins := []TradeFill{
		{FillID: "np1", AccountID: acct.ID, InstID: "NPA-USDT-SWAP", Side: "buy", FillPx: "100", FillSz: "0.3", FilledAt: "2026-11-15T01:00:00Z"},
		{FillID: "np2", AccountID: acct.ID, InstID: "NPA-USDT-SWAP", Side: "buy", FillPx: "100", FillSz: "0.2", FilledAt: "2026-11-15T01:01:00Z"},
		{FillID: "np3", AccountID: acct.ID, InstID: "NPA-USDT-SWAP", Side: "sell", FillPx: "110", FillSz: "0.1", FilledAt: "2026-11-15T01:02:00Z"},
		// 另一个标的：纯空仓
		{FillID: "np4", AccountID: acct.ID, InstID: "NPB-USDT-SWAP", Side: "sell", FillPx: "50", FillSz: "2", FilledAt: "2026-11-15T01:03:00Z"},
	}
	for _, f := range ins {
		if _, _, err := d.InsertFill(f); err != nil {
			t.Fatal(err)
		}
	}

	got, err := d.LocalNetPositions(acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 0.3 + 0.2 - 0.1 = 0.4
	if got["NPA-USDT-SWAP"] != "0.4" {
		t.Errorf("NPA 净持仓 = %q, want 0.4", got["NPA-USDT-SWAP"])
	}
	if got["NPB-USDT-SWAP"] != "-2" {
		t.Errorf("NPB 净持仓 = %q, want -2", got["NPB-USDT-SWAP"])
	}
}

// 台账 state 回填：成交齐了就不能再显示 submitted。
//
// 2026-11-15 的误判正是「看到 submitted 就断定 0 成交，而交易所早已
// 全额成交」。回填让台账能自证。
func TestBackfillTradeRecordStates(t *testing.T) {
	d := testOpen(t)
	acct, err := d.ExecAccounts().UpsertAccount(ExecAccountInput{
		Channel: "okx", ChannelAcctID: "bftr-" + t.Name(),
		Label: "回填", Market: "crypto", MarketSupport: []string{"crypto"}, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM trade_fills WHERE account_id=$1`, acct.ID)
		_, _ = d.Exec(`DELETE FROM trade_records WHERE account_id=$1`, acct.ID)
		_, _ = d.Exec(`DELETE FROM exec_accounts WHERE id=$1`, acct.ID)
	})

	mk := func(cid, qty string) int64 {
		id, err := d.InsertTradeRecord(TradeRecord{
			TaskID: 999001, AccountID: acct.ID, InstID: "BF-USDT-SWAP",
			Action: "open", Side: "buy", Qty: qty, ClientOrderID: cid,
			State: TradeRecordSubmitted, Mode: "live_auto",
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	fullID := mk("artexbf-full", "2")  // 成交满量 → filled
	partID := mk("artexbf-part", "10") // 只成交 1 → 保持 submitted
	noneID := mk("artexbf-none", "5")  // 无成交 → 不动

	fills := []TradeFill{
		{FillID: "bf1", AccountID: acct.ID, InstID: "BF-USDT-SWAP", Side: "buy", FillPx: "100", FillSz: "1", ClOrdID: "artexbf-full", FilledAt: "2026-11-15T02:00:00Z"},
		{FillID: "bf2", AccountID: acct.ID, InstID: "BF-USDT-SWAP", Side: "buy", FillPx: "101", FillSz: "1", ClOrdID: "artexbf-full", FilledAt: "2026-11-15T02:00:01Z"},
		{FillID: "bf3", AccountID: acct.ID, InstID: "BF-USDT-SWAP", Side: "buy", FillPx: "100", FillSz: "1", ClOrdID: "artexbf-part", FilledAt: "2026-11-15T02:00:02Z"},
	}
	for _, f := range fills {
		if _, _, err := d.InsertFill(f); err != nil {
			t.Fatal(err)
		}
	}

	n, err := d.BackfillTradeRecordStates(acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("应只回填 1 条（满量那条），实际 %d", n)
	}
	check := func(id int64, want, why string) {
		got, err := d.GetTradeRecord(id)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != want {
			t.Errorf("%s: state = %q, want %q", why, got.State, want)
		}
	}
	check(fullID, TradeRecordFilled, "成交满量")
	check(partID, TradeRecordSubmitted, "部分成交仍应 submitted")
	check(noneID, TradeRecordSubmitted, "无成交应保持不动")
}
