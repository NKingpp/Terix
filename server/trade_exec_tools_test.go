package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// callToolCtx 带上下文调用工具（callTool 用的是 Background，拿不到 RunInfo）。
func callToolCtx(t *testing.T, ctx context.Context, tl actool.CoreTool, args map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("序列化入参: %v", err)
	}
	res, err := tl.Call(ctx, raw, nil)
	if err != nil {
		t.Fatalf("工具调用返回 error: %v", err)
	}
	return res.Flatten()
}

// 工具级台账必须能按任务归属，且 ListFills 的归属要能通过它命中。
//
// # 为什么这条是核心
//
// 旧归属是 fills → cl_ord_id → orders_execution → draft → signal → task，五跳。
// 工具下单不产生 draft/signal，所以只靠旧路径任务盈亏永远是 0。
// 这条测试钉住「台账(task_id) 直连」这一跳。
func TestTradeRecordsTaskAttribution(t *testing.T) {
	e := newTradeE2E(t)
	ri := e.runInfo(t, &e.instID) // 每次调用都会新建任务，只能取一次
	recID, err := e.pg.InsertTradeRecord(db.TradeRecord{
		TaskID:    ri.TaskID,
		AccountID: e.acctID, InstrumentID: e.instID,
		InstID: "E2ETEST-USDT", Action: "open", Side: "buy", PosSide: "long",
		Qty: "1", OrdType: "market", ClientOrderID: "artextestattr01",
		State: db.TradeRecordSubmitted, Mode: "live_auto",
	})
	if err != nil {
		t.Fatalf("写台账失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = e.pg.Exec(`DELETE FROM trade_fills WHERE cl_ord_id='artextestattr01'`)
		_, _ = e.pg.Exec(`DELETE FROM trade_records WHERE id=$1`, recID)
	})

	taskID := ri.TaskID
	if _, _, err := e.pg.InsertFill(db.TradeFill{
		FillID: "ATTR-FILL-1", AccountID: e.acctID, InstID: "E2ETEST-USDT",
		Side: "buy", PosSide: "long", FillPx: "100", FillSz: "1", Fee: "-0.1",
		ClOrdID: "artextestattr01", FilledAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("写成交失败: %v", err)
	}

	fills, err := e.pg.ListFills(db.FillsFilter{TaskID: taskID, Limit: 50})
	if err != nil {
		t.Fatalf("按任务查成交失败: %v", err)
	}
	found := false
	for _, f := range fills {
		if f.FillID == "ATTR-FILL-1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("通过 trade_records.task_id 应能归属到任务 %d，实际查到 %d 笔: %+v",
			taskID, len(fills), fills)
	}
}

// 开仓必须带 TP/SL —— 没有保护的仓不允许开。
//
// 这是「裸仓」的唯一防线：交易所不会替你拒绝一张没有 OCO 的市价单，
// 而一张裸仓在极端行情里可以亏掉全部保证金。
func TestOpenPositionRequiresBracket(t *testing.T) {
	e := newTradeE2E(t)
	ets := NewExecToolSet(e.pg, e.tts.gate, nil)
	ets.clientOverride = &fakeOkxTrader{lastPrice: "100"}
	ctx := agent.WithRunInfo(context.Background(), e.runInfo(t, &e.instID))

	out := callToolCtx(t, ctx, ets.openPosition(), map[string]any{
		"symbol": "E2ETEST-USDT", "side": "buy", "qty": "1", "reason": "测试",
	})
	if !strings.Contains(out, "tp_price") || !strings.Contains(out, "sl_price") {
		t.Fatalf("缺 TP/SL 应拒绝，实际: %s", out)
	}
}

// 现货写法必须**升级**成对应永续，而不是拒绝。
//
// # 为什么是升级而不是拒绝（2026-11-14 任务 29）
//
// 用户/模型写 BTC-USDT 时，本系统只能理解为「要交易 BTC」——因为现货
// 在 OKX 是余额、不在 positions 接口里，无法巡检、无法挂 OCO、无法平仓。
// 若直接拒绝，模型会以为是代码写错，然后反复重试别的写法（实测就是这样）。
//
// 真正常见的错是「交割合约」（BTC-USDT-250926）—— 那个确实不支持，
// 必须明确拒绝而不是悄悄换成永续。
func TestOpenPositionUpgradesSpotToPerp(t *testing.T) {
	e := newTradeE2E(t)
	spotID, _, err := e.pg.Instruments().UpsertInstrument(db.InstrumentInput{
		Market: "crypto", Symbol: "UPG-USDT", AssetClass: "crypto",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = e.pg.Exec(`DELETE FROM instruments WHERE symbol IN ('UPG-USDT','UPG-USDT-SWAP')`)
	})
	ri := e.runInfo(t, &spotID)
	ets := NewExecToolSet(e.pg, e.tts.gate, nil)
	ctx := agent.WithRunInfo(context.Background(), ri)

	out := callToolCtx(t, ctx, ets.openPosition(), map[string]any{
		"symbol": "UPG-USDT", "side": "buy", "qty": "1",
		"tp_price": "110", "sl_price": "90", "reason": "测试",
	})
	if strings.Contains(out, "不是永续") {
		t.Fatalf("现货写法应升级为永续，不该被拒: %s", out)
	}
	// 应自动登记了永续行
	if _, err := e.pg.Instruments().GetInstrumentByMarketSymbol("crypto", "UPG-USDT-SWAP"); err != nil {
		t.Errorf("应自动登记 UPG-USDT-SWAP: %v", err)
	}
}

// 交割合约必须拒绝（三段式非 SWAP 无法映射）。
func TestOpenPositionRejectsDeliveryContract(t *testing.T) {
	e := newTradeE2E(t)
	ets := NewExecToolSet(e.pg, e.tts.gate, nil)
	ctx := agent.WithRunInfo(context.Background(), e.runInfo(t, &e.instID))
	out := callToolCtx(t, ctx, ets.openPosition(), map[string]any{
		"symbol": "BTC-USDT-250926", "side": "buy", "qty": "1",
		"tp_price": "110000", "sl_price": "90000", "reason": "测试",
	})
	if !strings.Contains(out, "永续") {
		t.Fatalf("交割合约应被拒，实际: %s", out)
	}
}

// 不在任务上下文里的调用必须拒绝（不能凭空下单）。
func TestExecToolsRequireTaskContext(t *testing.T) {
	e := newTradeE2E(t)
	ets := NewExecToolSet(e.pg, e.tts.gate, nil)
	for _, tl := range ets.Tools() {
		out := callToolCtx(t, context.Background(), tl, map[string]any{
			"symbol": "E2ETEST-USDT", "side": "buy", "qty": "1",
			"tp_price": "110", "sl_price": "90", "leverage": "3",
			"pos_side": "long", "reason": "测试",
		})
		if !strings.Contains(out, "上下文") {
			t.Errorf("%s 无任务上下文应拒绝，实际: %s", tl.Name(), out)
		}
	}
}

// 台账的 client_order_id 唯一约束要生效（防重复记账）。
func TestTradeRecordClOrdIDUnique(t *testing.T) {
	e := newTradeE2E(t)
	t.Cleanup(func() { _, _ = e.pg.Exec(`DELETE FROM trade_records WHERE client_order_id='artexdup0001'`) })
	mk := func() error {
		_, err := e.pg.InsertTradeRecord(db.TradeRecord{
			AccountID: e.acctID, InstID: "E2ETEST-USDT", Action: "open",
			Side: "buy", Qty: "1", ClientOrderID: "artexdup0001",
			State: db.TradeRecordSubmitted, Mode: "live_auto",
		})
		return err
	}
	if err := mk(); err != nil {
		t.Fatalf("首次写入应成功: %v", err)
	}
	if err := mk(); err == nil {
		t.Fatal("重复 client_order_id 必须报错 —— 否则同一笔单会记两次账")
	}
}

// 交易工具的标的解析必须容忍三种写法，并在表里没有该永续时自动登记。
//
// # 这是任务 29 的第一层卡点
//
// instruments 表只有 BTC-USDT（选品阶段写的现货），于是
// `create_order_draft {symbol:"BTC-USDT-SWAP"}` 直接 "找不到标的"。
// 而 `open_position` 要的正是 -SWAP 写法 —— 表里没有就永远开不了仓。
func TestResolveTradeInstrument(t *testing.T) {
	e := newTradeE2E(t)
	t.Cleanup(func() {
		_, _ = e.pg.Exec(`DELETE FROM instruments WHERE symbol IN ('AUTOPERP-USDT-SWAP','AUTOPERP-USDT')`)
	})

	// ① 自动登记：表里没有，但形如 X-USDT-SWAP → 建出来
	inst, err := resolveTradeInstrument(e.pg, "AUTOPERP-USDT-SWAP")
	if err != nil {
		t.Fatalf("应自动登记永续: %v", err)
	}
	if inst.Symbol != "AUTOPERP-USDT-SWAP" {
		t.Fatalf("symbol = %q", inst.Symbol)
	}

	// ② 现货写法补 -SWAP
	got, err := resolveTradeInstrument(e.pg, "AUTOPERP-USDT")
	if err != nil {
		t.Fatalf("现货写法应映射到永续: %v", err)
	}
	if got.Symbol != "AUTOPERP-USDT-SWAP" {
		t.Errorf("AUTOPERP-USDT 应解析到永续，得到 %q", got.Symbol)
	}

	// ③ 裸代码补 -USDT-SWAP
	got2, err := resolveTradeInstrument(e.pg, "AUTOPERP")
	if err != nil {
		t.Fatalf("裸代码应映射到永续: %v", err)
	}
	if got2.Symbol != "AUTOPERP-USDT-SWAP" {
		t.Errorf("AUTOPERP 应解析到永续，得到 %q", got2.Symbol)
	}

	// ④ 无法映射成永续的必须报错，不能悄悄登记假标的
	if _, err := resolveTradeInstrument(e.pg, "NO-SUCH-PAIR-X"); err == nil {
		t.Error("非永续写法应报错")
	}
}

// 任务绑了现货、要交易对应永续时必须自动改绑（存量任务自愈）。
//
// 不改绑的后果：`set_task_instrument` 禁止改标的，任务从此永久卡死，
// 只能新建任务重跑整条链。
func TestPrepareExecHealsSpotBinding(t *testing.T) {
	e := newTradeE2E(t)
	// 现货标的 + 任务绑它（复刻任务 29 的状态）
	spotID, _, err := e.pg.Instruments().UpsertInstrument(db.InstrumentInput{
		Market: "crypto", Symbol: "HEAL-USDT", AssetClass: "crypto",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = e.pg.Exec(`DELETE FROM instruments WHERE symbol IN ('HEAL-USDT','HEAL-USDT-SWAP')`)
	})
	ri := e.runInfo(t, &spotID)
	if _, err := e.pg.Exec(`UPDATE tasks SET instrument_id=$1 WHERE id=$2`, spotID, ri.TaskID); err != nil {
		t.Fatal(err)
	}

	ets := NewExecToolSet(e.pg, e.tts.gate, nil)
	ets.clientOverride = &fakeOkxTrader{lastPrice: "100"}
	ctx := agent.WithRunInfo(context.Background(), ri)
	ec, err := ets.prepareExec(ctx, "HEAL-USDT-SWAP")
	if err != nil {
		t.Fatalf("应自动改绑到永续: %v", err)
	}
	if ec.inst.Symbol != "HEAL-USDT-SWAP" {
		t.Fatalf("inst = %q", ec.inst.Symbol)
	}
	var bound int64
	if err := e.pg.QueryRow(`SELECT COALESCE(instrument_id,0) FROM tasks WHERE id=$1`, ri.TaskID).Scan(&bound); err != nil {
		t.Fatal(err)
	}
	if bound != ec.inst.ID {
		t.Errorf("任务标的应已改绑到 %d，实际 %d", ec.inst.ID, bound)
	}
}

// 跨标的（不同资产）仍然必须拒绝 —— 自愈不能变成「随便改绑」。
func TestPrepareExecRejectsDifferentAsset(t *testing.T) {
	e := newTradeE2E(t)
	otherID, _, err := e.pg.Instruments().UpsertInstrument(db.InstrumentInput{
		Market: "crypto", Symbol: "OTHER-USDT-SWAP", AssetClass: "crypto",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = e.pg.Exec(`DELETE FROM instruments WHERE symbol IN ('OTHER-USDT-SWAP','INSTRDIFF-USDT-SWAP')`)
	})
	ri := e.runInfo(t, &otherID)

	ets := NewExecToolSet(e.pg, e.tts.gate, nil)
	ctx := agent.WithRunInfo(context.Background(), ri)
	// 用另一个永续（不经 resolver 造新行，直接已存在的）
	if _, err := ets.prepareExec(ctx, "INSTRDIFF-USDT-SWAP"); err == nil {
		t.Fatal("不同资产必须拒绝")
	} else if !strings.Contains(err.Error(), "不一致") {
		t.Errorf("错误应说明标的与任务不一致，得到: %v", err)
	}
}

// sameUnderlying 必须把「裸股票代码」与「该股票的永续」判为同一资产。
//
// # 任务 30 的卡点
//
// 任务绑的是 NVDA（market=us 的裸代码），要交易的是 NVDA-USDT-SWAP。
// 只剥 -SWAP 得到 "NVDA" vs "NVDA-USDT" → 判不同资产 → 自愈失效 →
// 任务永久卡死，模型只能报「绑定不一致，需人工介入」。
func TestSameUnderlying(t *testing.T) {
	yes := [][2]string{
		{"NVDA", "NVDA-USDT-SWAP"},
		{"BTC", "BTC-USDT-SWAP"},
		{"BTC-USDT", "BTC-USDT-SWAP"},
		{"btc-usdt-swap", "BTC-USDT-SWAP"},
	}
	for _, c := range yes {
		if !sameUnderlying(c[0], c[1]) {
			t.Errorf("%q 与 %q 应判为同一资产", c[0], c[1])
		}
	}
	no := [][2]string{
		{"BTC", "ETH-USDT-SWAP"},
		{"NVDA", "AAPL-USDT-SWAP"},
		{"", "BTC-USDT-SWAP"},
	}
	for _, c := range no {
		if sameUnderlying(c[0], c[1]) {
			t.Errorf("%q 与 %q 不该判为同一资产", c[0], c[1])
		}
	}
}

// position_effect 必须能落库并读回；非法值要被拒。
func TestTradeRecordPositionEffect(t *testing.T) {
	e := newTradeE2E(t)
	t.Cleanup(func() {
		_, _ = e.pg.Exec(`DELETE FROM trade_records WHERE client_order_id LIKE 'artexeff%'`)
	})
	rec := db.TradeRecord{
		AccountID: e.acctID, InstID: "EFF-USDT-SWAP", Action: "open",
		Side: "buy", Qty: "1", ClientOrderID: "artexeff01",
		State: db.TradeRecordSubmitted, Mode: "live_auto",
		PositionEffect: db.TradeEffectScaleIn,
	}
	id, err := e.pg.InsertTradeRecord(rec)
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	got, gErr := e.pg.GetTradeRecord(id)
	if gErr != nil {
		t.Fatal(gErr)
	}
	if got.PositionEffect != db.TradeEffectScaleIn {
		t.Errorf("读回 position_effect = %q, want scale_in", got.PositionEffect)
	}

	// 非法 effect
	rec.ClientOrderID = "artexeff02"
	rec.PositionEffect = "bogus"
	if _, err := e.pg.InsertTradeRecord(rec); err == nil {
		t.Error("非法 position_effect 应被拒")
	}

	// close 动作不能带 effect（否则报表把平仓与加仓混成一类）
	rec.ClientOrderID = "artexeff03"
	rec.PositionEffect = db.TradeEffectScaleIn
	rec.Action = "close"
	if _, err := e.pg.InsertTradeRecord(rec); err == nil {
		t.Error("action=close 带 position_effect 应被拒")
	}
}
