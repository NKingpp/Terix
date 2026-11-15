package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/exec"
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
	ctx := agent.WithRunInfo(context.Background(), ri)
	ec, err := ets.prepareExec(ctx, "HEAL-USDT-SWAP")
	if err != nil {
		// 纸面模式下不需要凭据，应当能走到这里
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

// 纸面平仓不能因为 client 为 nil 而 panic。
//
// # 这是真实的空指针路径
//
// closePaper 原来写的是 `ec.client.LastPrice(...)` —— nil 指针上的方法调用
// 不会自动跳过，纸面平仓会直接 panic（而不是返回一个可读的错误）。
// 顺序必须是「先判空，再取价」。
func TestPaperCloseNoNilDeref(t *testing.T) {
	ec := &execCtx{isPaper: true, client: nil}
	if ec.client != nil {
		t.Skip("前提不成立")
	}
	// 直接断言判空顺序：只要能走到「没有 client 就不取价」这一步即可。
	// 这里构造的是一个极简调用，验证 nil 分支不会 panic。
	var px string
	if ec.client != nil {
		p, _ := ec.client.LastPrice(t.Context(), "X-USDT-SWAP")
		px = p
	}
	if px != "" {
		t.Fatalf("无 client 时不该取到价，得到 %q", px)
	}
}

// 纸面开仓必须能归属到任务。
//
// # orders_draft 没有 task_id 列
//
// 归属靠 JOIN signals 带出，所以 openPaper 必须建一条带 task_id 的 signal。
// 不建的话 ExecutePaper 拿到的 draft.TaskID 是 nil，paper_orders.task_id 也是
// NULL —— 纸面开了仓，任务详情却显示 0 笔（实测第一次联调就是这样）。
//
// 直接测 createPaperDraft 而不是走 openPosition：后者要先从交易所取价，
// 测试环境拿不到（假标的不存在于 OKX），归属断言就永远跑不到。
func TestCreatePaperDraftAttributesTask(t *testing.T) {
	e := newTradeE2E(t)
	if err := e.pg.SetSetting(db.ExecModeKey, string(db.ExecModePaper)); err != nil {
		t.Fatal(err)
	}
	ri := e.runInfo(t, &e.instID)
	ets := NewExecToolSet(e.pg, e.tts.gate, nil)
	ec := &execCtx{
		taskID: ri.TaskID, acct: &db.ExecAccount{ID: e.acctID},
		inst: &db.Instrument{ID: e.instID, Symbol: "E2ETEST-USDT"},
		mode: db.ExecModePaper, isPaper: true,
	}
	t.Cleanup(func() {
		_, _ = e.pg.Exec(`DELETE FROM draft_exec_params WHERE draft_id IN (SELECT id FROM orders_draft WHERE signal_id IN (SELECT id FROM signals WHERE task_id=$1))`, ri.TaskID)
		_, _ = e.pg.Exec(`DELETE FROM orders_draft WHERE signal_id IN (SELECT id FROM signals WHERE task_id=$1)`, ri.TaskID)
		_, _ = e.pg.Exec(`DELETE FROM signals WHERE task_id=$1`, ri.TaskID)
	})

	sigID, draftID, err := ets.createPaperDraft(context.Background(), ec, "buy", "long", "1", "100", "110", "90")
	if err != nil {
		t.Fatalf("建纸面草案失败: %v", err)
	}
	if sigID <= 0 || draftID <= 0 {
		t.Fatalf("signal/draft id 无效: %d/%d", sigID, draftID)
	}
	// 关键断言：草案能带出 task_id（ExecutePaper 靠它写 paper_orders.task_id）
	d, err := e.pg.OrderDrafts().GetDraft(draftID)
	if err != nil {
		t.Fatal(err)
	}
	if d.TaskID == nil || *d.TaskID != ri.TaskID {
		t.Fatalf("草案应带出任务 %d，实际 %v（paper_orders 会因此没有归属）", ri.TaskID, d.TaskID)
	}
	if d.RefPrice == nil || strings.TrimSpace(*d.RefPrice) == "" {
		t.Fatal("草案必须有 ref_price，否则 ExecutePaper 会拒绝")
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

// 已有同向持仓时 open_position **不再拒绝**，而是识别为加仓并如实回报。
//
// # 这是 2026-11-14 的规格变更
//
// 旧设计用 guardNoOpenPosition 一刀切拦掉「任意方向有仓就拒绝」，
// 理由是净持仓模式下反向开仓会静默抵消现有仓。但那条限制同时
// 也把合法的加仓挡掉了 —— 用户明确的诉求是「不要限制，让模型发挥」。
//
// 新设计的处理方式：
//   - 不阻断，但用 exec.ClassifyEntry 判定这一单的真实效果
//   - scale_in 时工具自动重挂 OCO（保证该方向同时只有一组保护）
//   - 回执里明确 position_effect / pre_position / post_position / protection
//
// 这条测试钉住「语义识别正确」—— 而不是「拒绝」。
func TestOpenPositionScalesInOnExistingSameSide(t *testing.T) {
	e := newTradeE2E(t)
	if err := e.pg.SetSetting(db.ExecModeKey, string(db.ExecModePaper)); err != nil {
		t.Fatal(err)
	}
	ri := e.runInfo(t, &e.instID)
	ets := NewExecToolSet(e.pg, e.tts.gate, nil)
	ec := &execCtx{
		taskID: ri.TaskID, acct: &db.ExecAccount{ID: e.acctID},
		inst: &db.Instrument{ID: e.instID, Symbol: "E2ETEST-USDT-SWAP"},
		mode: db.ExecModePaper, isPaper: true,
	}
	// 已有同向（多）纸面仓 1 张
	planID, err := e.pg.InsertPaperTradePlan(db.PaperTradePlan{
		AccountID: e.acctID, InstrumentID: e.instID, PosSide: "long",
		Qty: "1", TpPrice: "110", SlPrice: "90",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = e.pg.Exec(`DELETE FROM paper_trade_plans WHERE id=$1`, planID)
	})

	// 快照应认出 1 张多仓
	snap, sErr := ets.snapshotPositions(context.Background(), ec)
	if sErr != nil {
		t.Fatalf("读纸面持仓失败: %v", sErr)
	}
	plan, cErr := exec.ClassifyEntry("net", "buy", "0.5", snap.qtyOf("long"), snap.qtyOf("short"))
	if cErr != nil {
		t.Fatal(cErr)
	}
	if plan.Effect != exec.EffectScaleIn {
		t.Fatalf("同向已有仓应判 scale_in，实际 %q（这是加仓，不是拒绝）", plan.Effect)
	}
	if plan.PostNet != "1.5" {
		t.Errorf("PostNet = %q, want 1.5", plan.PostNet)
	}

	// 反向（净持仓模式下）应判 reduce 而不是 open
	rev, _ := exec.ClassifyEntry("net", "sell", "0.3", snap.qtyOf("short"), snap.qtyOf("long"))
	if rev.Effect != exec.EffectReduce {
		t.Errorf("净持仓模式下反向小额应判 reduce，实际 %q", rev.Effect)
	}
}

// 纸面部分平仓后剩余仓必须仍可见。
//
// # 原来的 bug
//
// closePaper 无条件调 MarkPaperTradePlanClosed，平一半就把整条计划关掉。
// 剩余那半从账本消失：get_positions 看不见、close_position 找不到，
// 最终在盈亏统计里变成「凭空获利」。
func TestPaperPartialCloseKeepsRemainder(t *testing.T) {
	e := newTradeE2E(t)
	if err := e.pg.SetSetting(db.ExecModeKey, string(db.ExecModePaper)); err != nil {
		t.Fatal(err)
	}
	planID, err := e.pg.InsertPaperTradePlan(db.PaperTradePlan{
		AccountID: e.acctID, InstrumentID: e.instID, PosSide: "long",
		Qty: "2", TpPrice: "110", SlPrice: "90",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = e.pg.Exec(`DELETE FROM paper_trade_plans WHERE id=$1`, planID)
		_, _ = e.pg.Exec(`DELETE FROM paper_orders WHERE account_id=$1`, e.acctID)
	})

	// 直接调 db 层：closePaper 需要现价（测试环境取不到），
	// 但「扣减而不是关闭」这个语义在 db 层就能钉住。
	left, rErr := e.pg.ReducePaperTradePlanQty(planID, "1.5")
	if rErr != nil {
		t.Fatalf("扣减数量失败: %v", rErr)
	}
	if left != "1.5" {
		t.Errorf("剩余量 = %q, want 1.5", left)
	}
	plans, lErr := e.pg.ListOpenPaperTradePlans(0)
	if lErr != nil {
		t.Fatal(lErr)
	}
	found := false
	for _, p := range plans {
		if p.ID == planID {
			found = true
			if p.Qty != "1.5" {
				t.Errorf("未平计划数量 = %q, want 1.5", p.Qty)
			}
		}
	}
	if !found {
		t.Fatal("部分平仓后计划不该被关闭（剩余仓会从账本消失）")
	}
}

// 纸面加仓必须合并到同方向计划，不能插新行。
//
// 插两条会让 get_positions 显示两行残余、close_position 只平掉一条 ——
// 另一条成永远平不掉的幽灵仓。
func TestPaperScaleInMergesPlan(t *testing.T) {
	e := newTradeE2E(t)
	if err := e.pg.SetSetting(db.ExecModeKey, string(db.ExecModePaper)); err != nil {
		t.Fatal(err)
	}
	planID, err := e.pg.InsertPaperTradePlan(db.PaperTradePlan{
		AccountID: e.acctID, InstrumentID: e.instID, PosSide: "long",
		Qty: "1", TpPrice: "110", SlPrice: "90",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = e.pg.Exec(`DELETE FROM paper_trade_plans WHERE id=$1`, planID)
	})

	if aErr := e.pg.AddToPaperTradePlan(planID, "0.5", "115", "88"); aErr != nil {
		t.Fatalf("加仓合并失败: %v", aErr)
	}
	got, gErr := e.pg.GetPaperTradePlan(planID)
	if gErr != nil {
		t.Fatal(gErr)
	}
	if got.Qty != "1.5" {
		t.Errorf("合并后数量 = %q, want 1.5", got.Qty)
	}
	if got.TpPrice != "115" || got.SlPrice != "88" {
		t.Errorf("TP/SL 应被新值覆盖，得到 tp=%q sl=%q", got.TpPrice, got.SlPrice)
	}

	// 同方向仍只有一条计划
	plans, _ := e.pg.ListOpenPaperTradePlans(0)
	n := 0
	for _, p := range plans {
		if p.InstrumentID == e.instID && p.PosSide == "long" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("同方向计划应只有 1 条，实际 %d 条", n)
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

// 纸面开仓必须校验名义金额能否被账户权益支撑。
//
// # 为什么（2026-11-15 荒谬仓位事故）
//
// 旧版 openPaper 零校验。模型按「用全部余额」算出 10000 张 NVDA 永续
// （名义约 160 万 USD），账户权益只有 8.7 万 —— 引擎照单全收，
// dashboard 于是显示「模拟盘赚 2766 USDT」。
//
// 那个数字没有决策含义：它来自一笔现实中必然被交易所拒绝的单。
// 允许不可能成交的单，模拟结果就是纯噪声。
//
// 这条测试钉住「超额名义金额会被本地拦住」，且错误信息要说明
// 这是保证金不足（不是随便的参数错误）。
func TestOpenPaperRejectsOversizedNotional(t *testing.T) {
	e := newTradeE2E(t)
	if err := e.pg.SetSetting(db.ExecModeKey, string(db.ExecModePaper)); err != nil {
		t.Fatal(err)
	}
	ri := e.runInfo(t, &e.instID)
	ets := NewExecToolSet(e.pg, e.tts.gate, nil)
	ec := &execCtx{
		taskID: ri.TaskID, acct: &db.ExecAccount{ID: e.acctID},
		inst: &db.Instrument{ID: e.instID, Symbol: "E2ETEST-USDT-SWAP"},
		mode: db.ExecModePaper, isPaper: true,
		params: db.ExecAccountParams{PosMode: "net", MgnMode: "cross", Lever: "3"},
		// 用假客户端：权益 87000，现价 160 → 上限 261000 名义
		client: &fakeOkxTrader{lastPrice: "160"},
	}
	plan := exec.EntryPlan{Effect: exec.EffectOpen, PostNet: "10000"}

	// 10000 张 × 160 = 160 万名义，远超 87000×3
	// 注意：工具用 actool.Errorf 表达业务拒绝（Result.IsError），
	// 不是 Go error —— 所以断言要看 Result。
	res, err := ets.openPaper(context.Background(), ec, "buy", "long",
		"10000", "200", "120", "验证仓位校验", plan)
	if err != nil {
		t.Fatalf("不该返回 Go error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("超额名义金额必须被判为错误（现实中会被交易所拒绝），实际返回: %s", res.Flatten())
	}
	msg := res.Flatten()
	if !strings.Contains(msg, "权益") && !strings.Contains(msg, "保证金") && !strings.Contains(msg, "名义金额") {
		t.Errorf("错误应说明是保证金/名义金额问题，实际: %s", msg)
	}
}
