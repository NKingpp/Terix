package db

import (
	"context"
	"math/big"
	"strconv"
	"strings"
	"testing"
)

// 交易所 OCO/TP/SL 触发的平仓腿（clOrdId 形如 O4114…）在 trade_records 里没有行。
// 归属必须靠「逐任务逐方向净持仓 + 按 |open| 比例拆份额」，否则平仓腿被丢
// → 净持仓算成未平 → 人工验收永远 409。
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

// 两个任务的持仓被一段连续净额串在一起。生产实测（账户 4，任务 42/45）：
// 42 先开 1 张空，45 接着开 20 张空把净额堆到 -21，交易所 SL 一笔买 21 打回 0，
// 45 再重开 19 张空。旧「回合首笔归属」规则把整段算给 42（45 的 20 张开仓
// 全丢，realized 变 -891.6452）。按份额拆后 42 只占 SL 腿的 1/21。
func TestTaskFillsSplitsMixedEpisode(t *testing.T) {
	d := testOpen(t)
	ctx := context.Background()
	const sym = "EPISODE-MIXED-SWAP"
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

	// 台账：42 开 1 张空；45 分 4 笔开 20 张空，SL 走交易所 algo 单（无台账行），
	// 再重开 19 张空。
	for _, r := range []struct {
		task int64
		cid  string
		qty  string
	}{
		{42, "artexo-t42", "1"},
		{45, "artexo-t45a", "9"},
		{45, "artexo-t45b", "5"},
		{45, "artexo-t45c", "4"},
		{45, "artexo-t45d", "2"},
		{45, "artexo-t45e", "19"},
	} {
		if _, err := d.Exec(`INSERT INTO trade_records(task_id, account_id, instrument_id, inst_id, action, side, qty,
client_order_id, state, mode, operator)
VALUES ($1,4,$2,$3,'open','sell',$4,$5,'filled','live_auto','agent')`,
			r.task, instID, sym, r.qty, r.cid); err != nil {
			t.Fatal(err)
		}
	}
	mk := func(fid, ordID, cid, side, sz, px, at string) {
		if _, _, err := d.InsertFill(TradeFill{
			FillID: fid, AccountID: 4, InstID: sym, Side: side, PosSide: "short",
			FillPx: px, FillSz: sz, OrdID: ordID, ClOrdID: cid, FilledAt: at,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// 净额走一遍：-1 → -10 → -15 → -19 → -21 → 0 → -19。
	// 前 6 笔构成一个回合（第 6 笔 algo SL 把净额打回 0），第 7 笔开新回合。
	mk(sym+":1", "4115698143391629312", "artexo-t42", "sell", "1", "662.8252", "2026-11-20T07:29:22Z")
	mk(sym+":2", "4116635165232971776", "artexo-t45a", "sell", "9", "637.5200", "2026-11-20T15:14:48Z")
	mk(sym+":3", "4116640143066513408", "artexo-t45b", "sell", "5", "637.4100", "2026-11-20T15:17:16Z")
	mk(sym+":4", "4116640591521497088", "artexo-t45c", "sell", "4", "636.1302", "2026-11-20T15:17:30Z")
	mk(sym+":5", "4117845030240501760", "artexo-t45d", "sell", "2", "651.4200", "2026-11-21T01:15:45Z")
	mk(sym+":6", "4119146181791563776", "O4119146181668506624", "buy", "21", "682.2172", "2026-11-21T12:02:03Z")
	mk(sym+":7", "4119277642117005312", "artexo-t45e", "sell", "19", "619.6850", "2026-11-21T13:07:20Z")

	// 42 拿到 2 笔，不是 1 笔：① 自己那 1 张开仓腿；② algo SL 腿（整笔 21 张）
	// 里属于它的 1/21 份额（SplitSz=1，原 FillSz 仍是 21）。
	//
	// 「只拿 1 笔」是错的 —— 只拿开仓腿就等于那张空单没被平掉，与下面
	// OpenPositions==0 自相矛盾。整段回合归属（旧规则）才会把 45 的 20 张开仓
	// 一起塞给 42；按份额拆则只给 1/21。两条断言互为交叉验证，勿「修」回 1 笔。
	old, err := d.TaskFills(ctx, 42, 4, sym)
	if err != nil {
		t.Fatal(err)
	}
	if len(old) != 2 {
		t.Fatalf("任务 42 应拿到 2 笔（自己的开仓腿 + algo 平仓腿的 1/21 份额），得到 %d 笔: %+v",
			len(old), old)
	}
	var openLeg, algoLeg *TaskFill
	for i := range old {
		switch old[i].ClOrdID {
		case "artexo-t42":
			openLeg = &old[i]
		case "O4119146181668506624":
			algoLeg = &old[i]
		}
	}
	if openLeg == nil || openLeg.OrdID != "4115698143391629312" {
		t.Fatalf("任务 42 应拿到自己的开仓腿 ordId 4115698143391629312，得到 %+v", old)
	}
	if algoLeg == nil {
		t.Fatalf("任务 42 应拿到 algo 平仓腿 O4119146181668506624 的份额，得到 %+v", old)
	}
	// 拆份额的核心：整笔仍是 21 张，本任务只占 1 张。
	if algoLeg.FillSz != "21" {
		t.Fatalf("algo 腿的整笔数量应保留 21，得到 %q", algoLeg.FillSz)
	}
	if algoLeg.SplitSz != "1" {
		t.Fatalf("任务 42 应只占 algo 腿 21 张里的 1 张，得到 SplitSz=%q", algoLeg.SplitSz)
	}

	// 经济后果：整段回合算给 42 时汇总被灌成 21 张往返、realized -891.6452。
	// 真值是 1 张：662.8252 开空，682.2172 被 SL 平掉 → -(682.2172-662.8252) = -19.392。
	sum42, err := TaskTradeSummaryFor(ctx, d, 42, instID)
	if err != nil {
		t.Fatal(err)
	}
	if sum42.Fills != 2 {
		t.Fatalf("任务 42 的汇总应统计 2 笔（开仓腿 + 1/21 平仓腿），得到 %d —— 说明跨任务的整段回合被算给了它", sum42.Fills)
	}
	if sum42.OpenPositions != 0 {
		t.Fatalf("任务 42 的 1 张空单应已被 SL 平掉（未平仓数 0），得到 %d", sum42.OpenPositions)
	}
	if sum42.RealizedTotal != "-19.392" {
		t.Fatalf("任务 42 的已实现盈亏应为 -19.392，得到 %s", sum42.RealizedTotal)
	}

	// 45 侧才是这个 bug 的真正受害面：旧规则下它的 20 张开仓整段消失，
	// 亏损不可见（wait_for 曾据此报 +1001 USDT）。20 张全平（algo 腿占 20/21），
	// 之后再开 19 张仍是未平仓。
	sum45, err := TaskTradeSummaryFor(ctx, d, 45, instID)
	if err != nil {
		t.Fatal(err)
	}
	if sum45.Fills != 6 {
		t.Fatalf("任务 45 的汇总应统计 6 笔（4 笔开仓 + 20/21 平仓腿 + 19 张开仓），得到 %d", sum45.Fills)
	}
	if sum45.OpenPositions != 1 {
		t.Fatalf("任务 45 应剩 1 个未平标的（重开的 19 张空），得到 %d", sum45.OpenPositions)
	}
	if sum45.NetQty[instID] != "-19" {
		t.Fatalf("任务 45 的净持仓应为 -19，得到 %q", sum45.NetQty[instID])
	}
	if sum45.RealizedTotal != "-872.2532" {
		t.Fatalf("任务 45 的已实现盈亏应为 -872.2532，得到 %s", sum45.RealizedTotal)
	}
}

// 账户 4 是 long_short 模式（实测 291 笔 long + 855 笔 short），同一标的上
// 多个任务可以各持一条反向仓。拆分必须只看「这笔成交能平掉的桶」：
// 同向仓收到份额会把仓位越平分越大。
//
// 构造：A 多 2、B 空 1，一笔无主 algo sell 1。卖出只平多头 —— A 应减半到 1，
// B 那张空单必须原样不动。按任务单一净额分桶时 A 会变成 2-2/3=1.33、
// B 变成 -1-1/3=-1.33（方向全错）。
func TestTaskFillsSplitIgnoresOppositeDirection(t *testing.T) {
	d := testOpen(t)
	ctx := context.Background()
	const sym = "SPLIT-DIRECTION-SWAP"
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

	for _, r := range []struct {
		task int64
		cid  string
		qty  string
	}{{9301, "artexo-dir-long", "2"}, {9302, "artexo-dir-short", "1"}} {
		if _, err := d.Exec(`INSERT INTO trade_records(task_id, account_id, instrument_id, inst_id, action, side, qty,
client_order_id, state, mode, operator)
VALUES ($1,4,$2,$3,'open','sell',$4,$5,'filled','live_auto','agent')`,
			r.task, instID, sym, r.qty, r.cid); err != nil {
			t.Fatal(err)
		}
	}
	mk := func(fid, ordID, cid, side, posSide, sz, px, at string) {
		if _, _, err := d.InsertFill(TradeFill{
			FillID: fid, AccountID: 4, InstID: sym, Side: side, PosSide: posSide,
			FillPx: px, FillSz: sz, OrdID: ordID, ClOrdID: cid, FilledAt: at,
		}); err != nil {
			t.Fatal(err)
		}
	}
	mk(sym+":1", "4115000000000000001", "artexo-dir-long", "buy", "long", "2", "100", "2026-11-22T01:00:00Z")
	mk(sym+":2", "4115000000000000002", "artexo-dir-short", "sell", "short", "1", "100", "2026-11-22T01:00:01Z")
	mk(sym+":3", "4115000000000000003", "O4115000000000000010", "sell", "long", "1", "110", "2026-11-22T02:00:00Z")

	sumA, err := TaskTradeSummaryFor(ctx, d, 9301, instID)
	if err != nil {
		t.Fatal(err)
	}
	if sumA.NetQty[instID] != "1" {
		t.Fatalf("任务 9301（多 2）被 algo 卖出 1 张后应剩 1 张多，得到 %q —— 份额分给了不该分的桶", sumA.NetQty[instID])
	}

	sumB, err := TaskTradeSummaryFor(ctx, d, 9302, instID)
	if err != nil {
		t.Fatal(err)
	}
	if sumB.NetQty[instID] != "-1" {
		t.Fatalf("任务 9302 的空单不该被 algo 卖出腿碰到（仍 -1），得到 %q", sumB.NetQty[instID])
	}

	// A 应拿到整笔（只有一个可平掉的桶），B 不该拿到这一笔的任何份额。
	fillsA, err := d.TaskFills(ctx, 9301, 4, sym)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, f := range fillsA {
		if f.ClOrdID == "O4115000000000000010" {
			found = true
			if f.SplitSz != "1" {
				t.Fatalf("任务 9301 应占 algo 腿全部 1 张，得到 SplitSz=%q", f.SplitSz)
			}
		}
	}
	if !found {
		t.Fatalf("任务 9301 应拿到 algo 卖出腿，得到 %+v", fillsA)
	}
	fillsB, err := d.TaskFills(ctx, 9302, 4, sym)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fillsB {
		if f.ClOrdID == "O4115000000000000010" {
			t.Fatalf("同向仓不该收到份额（任务 9302 是空头，卖出腿平不掉它），得到 %+v", f)
		}
	}
}

// 三个任务各 1 张空，交易所连发三笔单张 algo buy。每笔的精确份额是 1/3。
// ratLiteral 只留 10 位小数，0.3333333333×3 = 0.9999999999 —— 下游重新累加后
// 净持仓被判非零 → 人工验收报「持仓未平」，正是本次要修的病。
// 最大余额法保证 ΣSplitSz 精确等于 FillSz。
func TestTaskFillsSplitSumsExactly(t *testing.T) {
	d := testOpen(t)
	ctx := context.Background()
	const sym = "SPLIT-SUM-SWAP"
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

	tasks := []int64{9401, 9402, 9403}
	for i, tid := range tasks {
		cid := "artexo-sum-" + string(rune('a'+i))
		if _, err := d.Exec(`INSERT INTO trade_records(task_id, account_id, instrument_id, inst_id, action, side, qty,
client_order_id, state, mode, operator)
VALUES ($1,4,$2,$3,'open','sell','1',$4,'filled','live_auto','agent')`,
			tid, instID, sym, cid); err != nil {
			t.Fatal(err)
		}
		if _, _, err := d.InsertFill(TradeFill{
			FillID: sym + ":open" + string(rune('a'+i)), AccountID: 4, InstID: sym,
			Side: "sell", PosSide: "short", FillPx: "100", FillSz: "1",
			OrdID: "411600000000000000" + string(rune('1'+i)), ClOrdID: cid,
			FilledAt: "2026-11-22T01:00:0" + string(rune('1'+i)) + "Z",
		}); err != nil {
			t.Fatal(err)
		}
	}
	// 三笔单张 algo buy：每笔都要在三个桶之间分 1/3。
	for i := 0; i < 3; i++ {
		if _, _, err := d.InsertFill(TradeFill{
			FillID: sym + ":algo" + string(rune('a'+i)), AccountID: 4, InstID: sym,
			Side: "buy", PosSide: "short", FillPx: "110", FillSz: "1",
			OrdID:    "411600000000001000" + string(rune('1'+i)),
			ClOrdID:  "O4116000000000020" + string(rune('0'+i)),
			FilledAt: "2026-11-22T02:00:0" + string(rune('1'+i)) + "Z",
		}); err != nil {
			t.Fatal(err)
		}
	}

	for _, tid := range tasks {
		sum, err := TaskTradeSummaryFor(ctx, d, tid, instID)
		if err != nil {
			t.Fatal(err)
		}
		if sum.OpenPositions != 0 {
			t.Fatalf("任务 %d 三笔平分后应完全平仓，得到 %+v（幻影持仓：份额未能精确加回）", tid, sum)
		}
		if len(sum.NetQty) != 0 {
			t.Fatalf("任务 %d 的 NetQty 应为空，得到 %v", tid, sum.NetQty)
		}
	}

	// ΣSplitSz 必须精确等于 FillSz（逐笔核）。
	for i := 0; i < 3; i++ {
		clOrd := "O4116000000000020" + string(rune('0'+i))
		total := new(big.Rat)
		var rows int
		for _, tid := range tasks {
			fills, err := d.TaskFills(ctx, tid, 4, sym)
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range fills {
				if f.ClOrdID != clOrd {
					continue
				}
				v, ok := new(big.Rat).SetString(strings.TrimSpace(f.SplitSz))
				if !ok {
					t.Fatalf("SplitSz 不是数值: %q", f.SplitSz)
				}
				total.Add(total, v)
				rows++
			}
		}
		if rows != 3 {
			t.Fatalf("algo 腿 %s 应拆给 3 个任务，得到 %d 行", clOrd, rows)
		}
		if total.Cmp(new(big.Rat).SetInt64(1)) != 0 {
			t.Fatalf("algo 腿 %s 的份额之和应为 1，得到 %s", clOrd, ratLiteral(total))
		}
	}
}

// 第一笔就是无主成交（此时谁都没持仓）—— 无从归属，必须跳过而不是随便塞给谁。
func TestTaskFillsSplitSkipsWhenNoOpenPosition(t *testing.T) {
	d := testOpen(t)
	ctx := context.Background()
	const sym = "SPLIT-NOOPEN-SWAP"
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
VALUES (9501,4,$1,$2,'open','sell','1','artexo-noopen','filled','live_auto','agent')`, instID, sym); err != nil {
		t.Fatal(err)
	}
	// 先来一笔无主 sell（谁都没持仓），再来本任务的开仓。
	if _, _, err := d.InsertFill(TradeFill{
		FillID: sym + ":algo", AccountID: 4, InstID: sym, Side: "sell", PosSide: "short",
		FillPx: "110", FillSz: "1", OrdID: "4117000000000000001",
		ClOrdID: "O4117000000000000020", FilledAt: "2026-11-22T01:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.InsertFill(TradeFill{
		FillID: sym + ":open", AccountID: 4, InstID: sym, Side: "sell", PosSide: "short",
		FillPx: "100", FillSz: "1", OrdID: "4117000000000000002",
		ClOrdID: "artexo-noopen", FilledAt: "2026-11-22T01:00:01Z",
	}); err != nil {
		t.Fatal(err)
	}

	fills, err := d.TaskFills(ctx, 9501, 4, sym)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fills {
		if f.ClOrdID == "O4117000000000000020" {
			t.Fatalf("无持仓时的无主成交无从归属，不该记到任何任务，得到 %+v", f)
		}
	}
	if len(fills) != 1 {
		t.Fatalf("只该拿到自己的开仓腿，得到 %d 笔: %+v", len(fills), fills)
	}
	sum, err := TaskTradeSummaryFor(ctx, d, 9501, instID)
	if err != nil {
		t.Fatal(err)
	}
	if sum.NetQty[instID] != "-1" {
		t.Fatalf("净持仓应为 -1（无主成交被跳过），得到 %q", sum.NetQty[instID])
	}
}

// 无主成交大于 Σ|open|（手工单等病态输入）：份额夹在各个桶的持仓上限，
// 多出来的部分丢弃不改派，任何桶都不许翻过零。
func TestTaskFillsSplitClampsToAvailable(t *testing.T) {
	d := testOpen(t)
	ctx := context.Background()
	const sym = "SPLIT-CLAMP-SWAP"
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
	for _, r := range []struct {
		task int64
		cid  string
	}{{9601, "artexo-clamp-a"}, {9602, "artexo-clamp-b"}} {
		if _, err := d.Exec(`INSERT INTO trade_records(task_id, account_id, instrument_id, inst_id, action, side, qty,
client_order_id, state, mode, operator)
VALUES ($1,4,$2,$3,'open','sell','1',$4,'filled','live_auto','agent')`, r.task, instID, sym, r.cid); err != nil {
			t.Fatal(err)
		}
		if _, _, err := d.InsertFill(TradeFill{
			FillID: sym + ":" + r.cid, AccountID: 4, InstID: sym, Side: "sell", PosSide: "short",
			FillPx: "100", FillSz: "1", ClOrdID: r.cid,
			FilledAt: "2026-11-22T01:00:" + strconv.Itoa(1+int(r.task-9601)) + "Z",
		}); err != nil {
			t.Fatal(err)
		}
	}
	// algo buy 10 张，而总共只有 2 张空可平。
	if _, _, err := d.InsertFill(TradeFill{
		FillID: sym + ":big", AccountID: 4, InstID: sym, Side: "buy", PosSide: "short",
		FillPx: "110", FillSz: "10", OrdID: "4118000000000000001",
		ClOrdID: "O4118000000000000020", FilledAt: "2026-11-22T02:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}

	for _, tid := range []int64{9601, 9602} {
		sum, err := TaskTradeSummaryFor(ctx, d, tid, instID)
		if err != nil {
			t.Fatal(err)
		}
		if len(sum.NetQty) != 0 {
			t.Fatalf("任务 %d 的 1 张空应被平掉且不许翻成多头，得到 %+v", tid, sum)
		}
	}
	var total float64
	for _, tid := range []int64{9601, 9602} {
		fills, err := d.TaskFills(ctx, tid, 4, sym)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range fills {
			if f.ClOrdID != "O4118000000000000020" {
				continue
			}
			v, _ := strconv.ParseFloat(f.SplitSz, 64)
			total += v
		}
	}
	// 每桶夹到 1 张，多出来的 8 张丢弃（不改派）。
	if total != 2 {
		t.Fatalf("夹仓后份额之和应为 2（多出的 8 张丢弃），得到 %v", total)
	}
}

// SplitSz 是 10 位小数字符串，逐笔重新解析累加会留下残差。真实已平仓的
// 任务因此会算出 1e-10 而非 0 → OpenPositions 误判为 1 → 验收报「持仓未平」。
//
// 实测任务 45：230 笔累加，真值 -19 得 -19.0000000008。这里用 3 个任务
// 各 1 张空 + 三条 algo 买入腿复现同类残差，断言全部判为已平仓。
//
// # 为什么不能只靠归属层「最大余额法」
//
// 最大余额法保证**单笔** ΣSplitSz == FillSz，但跨多笔累加仍有残差 ——
// 每笔的 10 位舍入方向不同，N 笔之后偏差可达 N×1e-10。
// 汇总层必须有自己的容差，这是它的责任边界。
func TestTaskTradeSummaryIgnoresFractionalResidual(t *testing.T) {
	d := testOpen(t)
	ctx := context.Background()
	const sym = "EPISODE-RESIDUAL-SWAP"
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

	// 3 个任务各开 1 张空（三笔独立开仓）
	for i, tid := range []int64{9401, 9402, 9403} {
		cid := "artexo-res-" + string(rune('a'+i))
		if _, err := d.Exec(`INSERT INTO trade_records(task_id, account_id, instrument_id, inst_id,
action, side, qty, client_order_id, state, mode, operator)
VALUES ($1,1,$2,$3,'open','sell','1',$4,'filled','live_auto','agent')`, tid, instID, sym, cid); err != nil {
			t.Fatal(err)
		}
		if _, _, err := d.InsertFill(TradeFill{
			FillID: sym + ":open" + string(rune('a'+i)), AccountID: 1, InstID: sym,
			Side: "sell", PosSide: "short", FillPx: "100", FillSz: "1",
			ClOrdID: cid, FilledAt: "2026-11-20T0" + string(rune('1'+i)) + ":00:00Z",
		}); err != nil {
			t.Fatal(err)
		}
	}
	// 三条无主 algo 买入腿，每条平 1 张 —— 每条按 1/3 分给三个任务
	for i := 0; i < 3; i++ {
		if _, _, err := d.InsertFill(TradeFill{
			FillID: sym + ":close" + string(rune('a'+i)), AccountID: 1, InstID: sym,
			Side: "buy", PosSide: "short", FillPx: "90", FillSz: "1",
			ClOrdID: "O9400000000000000" + string(rune('0'+i)), OrdID: "9400000000000000" + string(rune('0'+i)),
			FilledAt: "2026-11-21T0" + string(rune('1'+i)) + ":00:00Z",
		}); err != nil {
			t.Fatal(err)
		}
	}

	for _, tid := range []int64{9401, 9402, 9403} {
		sum, err := TaskTradeSummaryFor(ctx, d, tid, instID)
		if err != nil {
			t.Fatalf("task %d: %v", tid, err)
		}
		if sum.OpenPositions != 0 {
			t.Errorf("task %d: 真实已平仓，OpenPositions 应为 0，得到 %d（netqty=%v）—— 1e-10 级残差被当成持仓，验收会永远 409",
				tid, sum.OpenPositions, sum.NetQty)
		}
		if q, ok := sum.NetQty[instID]; ok {
			t.Errorf("task %d: 已平仓不该有 NetQty，得到 %q", tid, q)
		}
	}
}
