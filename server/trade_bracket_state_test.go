package server

// 加仓重挂状态机的测试。
//
// # 为什么这些分支必须用 fake 覆盖
//
// settleBracket 的价值几乎全在失败分支上：
//
//	撤旧单失败 → 两组 OCO 并存，第一组触发就全平
//	挂新单失败 → 该仓裸奔
//	成交确认超时 → 按未知数量挂保护
//
// 这些如果不可测，只能上实盘验证「撤单失败时仓是不是真的裸的」——
// 要真钱、真行情，还要恰好赶上故障。用函数字段打桩的 fake 能逐条钉死
// 调用顺序与失败后的 protection 状态。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/exec"
	"github.com/Autumn-27/artex/exec/okxrest"
)

// fakeOkxTrader 是可编程的 okxTrader 替身。
//
// 每个方法一个函数字段：nil 时走默认成功行为，测试只覆盖关心的那一个。
// calls 记录调用顺序，用来断言「先下单后撤单」这类顺序不变量。
type fakeOkxTrader struct {
	calls []string

	positions []okxrest.Position
	algos     []okxrest.AlgoOrder

	placeErr     error
	cancelErr    error
	placeAlgoErr error
	listAlgoErr  error
	getPosErr    error
	orderState   string // 固定返回的订单状态，空表示 filled
	queryAlways  bool   // 为真时总是返回 orderState（模拟一直未成交）

	placedOrders []okxrest.OrderRequest
	placedAlgos  []okxrest.AlgoRequest
	canceledIDs  []string
	lastPrice    string
}

func (f *fakeOkxTrader) GetPositions(context.Context, string) ([]okxrest.Position, error) {
	f.calls = append(f.calls, "GetPositions")
	if f.getPosErr != nil {
		return nil, f.getPosErr
	}
	return f.positions, nil
}

func (f *fakeOkxTrader) PlaceOrder(_ context.Context, req okxrest.OrderRequest) (*okxrest.OrderAck, error) {
	f.calls = append(f.calls, "PlaceOrder")
	if f.placeErr != nil {
		return nil, f.placeErr
	}
	f.placedOrders = append(f.placedOrders, req)
	return &okxrest.OrderAck{OrdID: "ord-1", ClOrdID: req.ClOrdID}, nil
}

func (f *fakeOkxTrader) QueryOrderByClOrdID(context.Context, string, string) (*okxrest.OrderState, error) {
	f.calls = append(f.calls, "QueryOrder")
	st := f.orderState
	if st == "" {
		st = "filled"
	}
	return &okxrest.OrderState{OrdID: "ord-1", State: st, AccFillSz: "0.5", AvgPx: "102"}, nil
}

func (f *fakeOkxTrader) ListAlgoOrders(context.Context, string, string) ([]okxrest.AlgoOrder, error) {
	f.calls = append(f.calls, "ListAlgoOrders")
	if f.listAlgoErr != nil {
		return nil, f.listAlgoErr
	}
	return f.algos, nil
}

func (f *fakeOkxTrader) PlaceAlgoOCO(_ context.Context, req okxrest.AlgoRequest) (string, error) {
	f.calls = append(f.calls, "PlaceAlgoOCO")
	if f.placeAlgoErr != nil {
		return "", f.placeAlgoErr
	}
	f.placedAlgos = append(f.placedAlgos, req)
	return "algo-1", nil
}

func (f *fakeOkxTrader) CancelAlgo(_ context.Context, _, algoID string) error {
	f.calls = append(f.calls, "CancelAlgo")
	if f.cancelErr != nil {
		return f.cancelErr
	}
	f.canceledIDs = append(f.canceledIDs, algoID)
	return nil
}

func (f *fakeOkxTrader) AmendAlgoOCO(context.Context, string, string, string, string) error {
	f.calls = append(f.calls, "AmendAlgoOCO")
	return nil
}

func (f *fakeOkxTrader) SetLeverage(context.Context, string, string, string) error {
	f.calls = append(f.calls, "SetLeverage")
	return nil
}

func (f *fakeOkxTrader) LastPrice(context.Context, string) (string, error) {
	f.calls = append(f.calls, "LastPrice")
	if f.lastPrice == "" {
		return "100", nil
	}
	return f.lastPrice, nil
}

// orderOf 返回某方法在调用序列里的下标（-1 = 没调）。
func (f *fakeOkxTrader) orderOf(name string) int {
	for i, c := range f.calls {
		if c == name {
			return i
		}
	}
	return -1
}

// 基线参数：多单加仓，旧 OCO 未穿价。
func scaleInParam() settleParam {
	return settleParam{
		InstID: "TEST-USDT-SWAP", TdMode: "cross", PosMode: "net", PosSide: "long",
		Side: "buy", Qty: "0.5", Tp: "120", Sl: "90", MarkPx: "100",
		Effect: exec.EffectScaleIn, ClOrdID: "cid-scalein",
	}
}

func TestScaleInCancelsAfterFill(t *testing.T) {
	f := &fakeOkxTrader{
		algos: []okxrest.AlgoOrder{{
			AlgoID: "old-algo", InstID: "TEST-USDT-SWAP", PosSide: "net",
			TpTriggerPx: "130", SlTriggerPx: "80", State: "live",
		}},
		positions: []okxrest.Position{{
			InstID: "TEST-USDT-SWAP", PosSide: "net", Pos: "1.5", AvgPx: "102", MarkPx: "100",
		}},
	}
	ets := &ExecToolSet{}
	out, err := ets.settleBracket(context.Background(), f, scaleInParam())
	if err != nil {
		t.Fatalf("不该失败: %v", err)
	}
	// 先下单，后撤旧单 —— 撤在前会让「下单被拒」变成整仓裸奔。
	pi, ci := f.orderOf("PlaceOrder"), f.orderOf("CancelAlgo")
	if pi < 0 || ci < 0 || pi > ci {
		t.Fatalf("顺序应为 PlaceOrder 先于 CancelAlgo，实际 calls=%v", f.calls)
	}
	if len(f.placedOrders) != 1 || len(f.placedOrders[0].AttachAlgos) != 0 {
		t.Error("加仓单不能带 attachAlgoOrds（数量变了，保护要按新持仓重挂）")
	}
	if len(f.placedAlgos) != 1 {
		t.Fatalf("应挂 1 组新 OCO，实际 %d", len(f.placedAlgos))
	}
	if f.placedAlgos[0].CloseFraction != "1" {
		t.Errorf("closeFraction 应为 1（整仓保护），实际 %q", f.placedAlgos[0].CloseFraction)
	}
	if out.Protection != ProtectionProtected {
		t.Errorf("protection = %q, want protected", out.Protection)
	}
}

func TestScaleInOldBracketCrossedCancelsFirst(t *testing.T) {
	// 多单 tp=95 已被现价 100 穿过 → 必须先撤
	f := &fakeOkxTrader{
		algos: []okxrest.AlgoOrder{{
			AlgoID: "old-algo", InstID: "TEST-USDT-SWAP", PosSide: "net",
			TpTriggerPx: "95", SlTriggerPx: "80", State: "live",
		}},
		positions: []okxrest.Position{{
			InstID: "TEST-USDT-SWAP", PosSide: "net", Pos: "1.5", AvgPx: "102", MarkPx: "100",
		}},
	}
	ets := &ExecToolSet{}
	if _, err := ets.settleBracket(context.Background(), f, scaleInParam()); err != nil {
		t.Fatalf("不该失败: %v", err)
	}
	// 穿价的旧单要在下单前撤掉：留着它，加仓一成交它就立刻平掉全部仓。
	ci, pi := f.orderOf("CancelAlgo"), f.orderOf("PlaceOrder")
	if ci < 0 || pi < 0 || ci > pi {
		t.Fatalf("旧 TP 已穿价时应先撤后下单，实际 calls=%v", f.calls)
	}
}

func TestScaleInCancelFailsReportsDouble(t *testing.T) {
	f := &fakeOkxTrader{
		algos: []okxrest.AlgoOrder{{
			AlgoID: "old-algo", InstID: "TEST-USDT-SWAP", PosSide: "net",
			TpTriggerPx: "130", SlTriggerPx: "80", State: "live",
		}},
		cancelErr: errors.New("51000 cancel failed"),
		positions: []okxrest.Position{{
			InstID: "TEST-USDT-SWAP", PosSide: "net", Pos: "1.5", AvgPx: "102", MarkPx: "100",
		}},
	}
	ets := &ExecToolSet{}
	out, err := ets.settleBracket(context.Background(), f, scaleInParam())
	if err != nil {
		t.Fatalf("撤单失败不该让下单失败: %v", err)
	}
	if out.Protection != ProtectionDouble {
		t.Fatalf("protection = %q, want double", out.Protection)
	}
	if !strings.Contains(out.Note, "旧") {
		t.Errorf("必须说清旧单仍在，实际: %s", out.Note)
	}
}

func TestScaleInPlaceFailsReportsNaked(t *testing.T) {
	f := &fakeOkxTrader{
		algos: []okxrest.AlgoOrder{{
			AlgoID: "old-algo", InstID: "TEST-USDT-SWAP", PosSide: "net",
			TpTriggerPx: "130", SlTriggerPx: "80", State: "live",
		}},
		placeAlgoErr: errors.New("51008 insufficient"),
		positions: []okxrest.Position{{
			InstID: "TEST-USDT-SWAP", PosSide: "net", Pos: "1.5", AvgPx: "102", MarkPx: "100",
		}},
	}
	ets := &ExecToolSet{}
	out, err := ets.settleBracket(context.Background(), f, scaleInParam())
	if err != nil {
		t.Fatalf("挂保护失败不该让下单失败: %v", err)
	}
	if out.Protection != ProtectionNaked {
		t.Fatalf("protection = %q, want naked", out.Protection)
	}
	if !out.naked() {
		t.Error("naked() 应为真")
	}
	if !strings.Contains(out.Note, "没有保护") {
		t.Errorf("必须明确说「没有保护」，实际: %s", out.Note)
	}
}

func TestScaleInFillTimeoutStillPlacesBracket(t *testing.T) {
	f := &fakeOkxTrader{
		orderState: "live", // 一直未成交
		positions: []okxrest.Position{{
			InstID: "TEST-USDT-SWAP", PosSide: "net", Pos: "1.5", AvgPx: "102", MarkPx: "100",
		}},
	}
	ets := &ExecToolSet{}
	out, err := ets.settleBracket(context.Background(), f, scaleInParam())
	if err != nil {
		t.Fatalf("不该失败: %v", err)
	}
	// 超时也必须挂上保护：closeFraction=100% 不依赖具体数量。
	if len(f.placedAlgos) != 1 {
		t.Fatalf("成交未确认时仍应挂保护，实际 %d 组", len(f.placedAlgos))
	}
	if out.FillState != "unknown" {
		t.Errorf("fill_state = %q, want unknown", out.FillState)
	}
}

func TestReverseCancelsOldBracketAndSkipsNew(t *testing.T) {
	f := &fakeOkxTrader{
		algos: []okxrest.AlgoOrder{{
			AlgoID: "old-algo", InstID: "TEST-USDT-SWAP", PosSide: "net",
			TpTriggerPx: "80", SlTriggerPx: "120", State: "live",
		}},
		positions: []okxrest.Position{{
			InstID: "TEST-USDT-SWAP", PosSide: "net", Pos: "-0.5", AvgPx: "100", MarkPx: "100",
		}},
	}
	p := scaleInParam()
	p.Effect = exec.EffectReverse
	p.Tp, p.Sl = "", "" // 反手允许不给保护
	ets := &ExecToolSet{}
	out, err := ets.settleBracket(context.Background(), f, p)
	if err != nil {
		t.Fatalf("不该失败: %v", err)
	}
	if f.orderOf("CancelAlgo") < 0 {
		t.Fatal("反手必须撤旧 OCO —— 否则它按旧方向把新仓平掉")
	}
	if len(f.placedAlgos) != 0 {
		t.Error("反手不自动挂保护（方向刚翻转，价位由模型决定）")
	}
	if out.Protection != ProtectionNaked {
		t.Errorf("protection = %q, want naked", out.Protection)
	}
	if !strings.Contains(out.Note, "无保护") {
		t.Errorf("必须提示无保护，实际: %s", out.Note)
	}
}

func TestReduceLeavesOldBracket(t *testing.T) {
	f := &fakeOkxTrader{
		algos: []okxrest.AlgoOrder{{
			AlgoID: "old-algo", InstID: "TEST-USDT-SWAP", PosSide: "net",
			TpTriggerPx: "130", SlTriggerPx: "80", State: "live",
		}},
		positions: []okxrest.Position{{
			InstID: "TEST-USDT-SWAP", PosSide: "net", Pos: "0.7", AvgPx: "100", MarkPx: "100",
		}},
	}
	p := scaleInParam()
	p.Effect = exec.EffectReduce
	p.Side = "sell"
	p.Qty = "0.3"
	ets := &ExecToolSet{}
	out, err := ets.settleBracket(context.Background(), f, p)
	if err != nil {
		t.Fatalf("不该失败: %v", err)
	}
	if f.orderOf("CancelAlgo") >= 0 {
		t.Error("减仓不能撤 OCO —— closeFraction 是相对当前持仓的，撤了剩余仓就裸奔")
	}
	if len(f.placedAlgos) != 0 {
		t.Error("减仓不挂新 OCO")
	}
	if len(f.placedOrders) != 1 || !f.placedOrders[0].ReduceOnly {
		t.Error("减仓单必须带 reduceOnly")
	}
	if out.Protection != ProtectionProtected {
		t.Errorf("protection = %q, want protected", out.Protection)
	}
}

func TestLongShortOnlyCancelsMatchingAlgo(t *testing.T) {
	f := &fakeOkxTrader{
		algos: []okxrest.AlgoOrder{
			{AlgoID: "algo-long", InstID: "TEST-USDT-SWAP", PosSide: "long",
				TpTriggerPx: "130", SlTriggerPx: "80", State: "live"},
			{AlgoID: "algo-short", InstID: "TEST-USDT-SWAP", PosSide: "short",
				TpTriggerPx: "70", SlTriggerPx: "130", State: "live"},
		},
		positions: []okxrest.Position{
			{InstID: "TEST-USDT-SWAP", PosSide: "long", Pos: "1.5", AvgPx: "102", MarkPx: "100"},
			{InstID: "TEST-USDT-SWAP", PosSide: "short", Pos: "2", AvgPx: "110", MarkPx: "100"},
		},
	}
	p := scaleInParam()
	p.PosMode = "long_short"
	p.PosSide = "long"
	ets := &ExecToolSet{}
	if _, err := ets.settleBracket(context.Background(), f, p); err != nil {
		t.Fatalf("不该失败: %v", err)
	}
	for _, id := range f.canceledIDs {
		if id == "algo-short" {
			t.Fatal("双向模式下绝不能撤反向仓的 OCO —— 那条仓会裸奔且用户看不出原因")
		}
	}
}

func TestOpenAttachesAlgosOnce(t *testing.T) {
	f := &fakeOkxTrader{}
	p := scaleInParam()
	p.Effect = exec.EffectOpen
	ets := &ExecToolSet{}
	out, err := ets.settleBracket(context.Background(), f, p)
	if err != nil {
		t.Fatalf("不该失败: %v", err)
	}
	if len(f.placedOrders) != 1 {
		t.Fatalf("纯开仓只下 1 单，实际 %d", len(f.placedOrders))
	}
	if len(f.placedOrders[0].AttachAlgos) != 1 {
		t.Error("纯开仓应随主单附带 OCO（一次调用搞定，无裸仓窗口）")
	}
	if len(f.placedAlgos) != 0 {
		t.Error("纯开仓不该再单独调 PlaceAlgoOCO")
	}
	if f.orderOf("ListAlgoOrders") >= 0 {
		t.Error("纯开仓不需要读现有算法单")
	}
	if out.Protection != ProtectionProtected {
		t.Errorf("protection = %q, want protected", out.Protection)
	}
}

func TestScaleInListAlgoFailsRefusesToTrade(t *testing.T) {
	f := &fakeOkxTrader{listAlgoErr: errors.New("network down")}
	ets := &ExecToolSet{}
	_, err := ets.settleBracket(context.Background(), f, scaleInParam())
	if err == nil {
		t.Fatal("读不到现有算法单时加仓必须拒绝 —— 否则会留下两组 OCO")
	}
	if len(f.placedOrders) != 0 {
		t.Error("拒绝时不该已经下单")
	}
}

func TestReverseCancelFailsRefusesToTrade(t *testing.T) {
	f := &fakeOkxTrader{
		algos: []okxrest.AlgoOrder{{
			AlgoID: "old-algo", InstID: "TEST-USDT-SWAP", PosSide: "net",
			TpTriggerPx: "80", SlTriggerPx: "120", State: "live",
		}},
		cancelErr: errors.New("cancel rejected"),
	}
	p := scaleInParam()
	p.Effect = exec.EffectReverse
	ets := &ExecToolSet{}
	_, err := ets.settleBracket(context.Background(), f, p)
	if err == nil {
		t.Fatal("反手撤不掉旧 OCO 时必须拒绝下单 —— 留着它会把新仓按旧方向平掉")
	}
	if len(f.placedOrders) != 0 {
		t.Error("拒绝时不该已经下单")
	}
}

// long_short 双向模式下，反向持仓不参与「同向/反向」配对 ——
// 这一层由 ClassifyEntry 保证，这里补一条端到端断言防止将来改坏。
func TestLongShortOppositeIsOpenNotReduce(t *testing.T) {
	plan, err := exec.ClassifyEntry("long_short", "buy", "0.5", "0", "10")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Effect != exec.EffectOpen {
		t.Fatalf("双向模式下反向开仓是 open（另一条独立仓），得到 %q", plan.Effect)
	}
	if plan.PostNet != "0.5" {
		t.Errorf("PostNet = %q, want 0.5", plan.PostNet)
	}
}
