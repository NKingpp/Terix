package db

import (
	"errors"
	"strings"
	"testing"
)

// paperOrdersSetup opens a DB and returns its PaperOrderStore. Skips if no PG.
func paperOrdersSetup(t *testing.T) (*DB, *PaperOrderStore) {
	t.Helper()
	d := testOpen(t)
	return d, d.PaperOrders()
}

// TestPaperOrderRoundtrip 验证写入 → 读回 → 走向完整合法路径 → 时间戳落库。
func TestPaperOrderRoundtrip(t *testing.T) {
	d, store := paperOrdersSetup(t)

	instID, err := d.Instruments().InsertInstrument(InstrumentInput{
		Market: MarketUS, Symbol: uniqueName(t, "PO"), AssetClass: AssetClassStock,
	})
	if err != nil {
		t.Fatalf("insert instrument: %v", err)
	}
	defer d.Exec(`DELETE FROM instruments WHERE id = $1`, instID) //nolint:errcheck

	taskID := int64(990005)
	id, err := store.InsertOrder(PaperOrderInput{
		TaskID:       &taskID,
		InstrumentID: &instID,
		Mode:         OrderModePaper,
		Side:         OrderSideBuy,
		Qty:          "0.1234567800",
		Price:        "42.5000000000",
	})
	if err != nil {
		t.Fatalf("insert order: %v", err)
	}
	defer d.Exec(`DELETE FROM paper_orders WHERE id = $1`, id) //nolint:errcheck

	got, err := store.GetOrder(id)
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if got.Status != OrderStatusDraft {
		t.Errorf("status = %q, want draft", got.Status)
	}
	if got.Qty != "0.1234567800" {
		t.Errorf("qty = %q", got.Qty)
	}
	if got.Price == nil || *got.Price != "42.5000000000" {
		t.Errorf("price = %v", got.Price)
	}
	// filled_qty 默认 0 而非 NULL,聚合时不必 COALESCE。
	if got.FilledQty != "0.0000000000" && got.FilledQty != "0" {
		t.Errorf("filled_qty = %q, want zero", got.FilledQty)
	}

	// 合法路径 draft → risk_passed → submitted → filled。
	for _, next := range []string{OrderStatusRiskPassed, OrderStatusSubmitted, OrderStatusFilled} {
		if err := store.TransitStatus(id, next); err != nil {
			t.Fatalf("transit to %s: %v", next, err)
		}
		cur, err := store.GetOrder(id)
		if err != nil {
			t.Fatalf("get after %s: %v", next, err)
		}
		if cur.Status != next {
			t.Errorf("status = %q, want %q", cur.Status, next)
		}
	}

	cur, err := store.GetOrder(id)
	if err != nil {
		t.Fatalf("final get: %v", err)
	}
	// 进 submitted / filled 时必须写时间戳。
	if cur.SubmittedAt == nil {
		t.Error("expected submitted_at to be set")
	}
	if cur.FilledAt == nil {
		t.Error("expected filled_at to be set")
	}

	// 幂等:重复推进到同一状态不算错误。
	if err := store.TransitStatus(id, OrderStatusFilled); err != nil {
		t.Errorf("idempotent transit should succeed, got %v", err)
	}
}

// TestPaperOrderIllegalTransitions 显式验证状态机拒绝回退与跨级跳跃。
func TestPaperOrderIllegalTransitions(t *testing.T) {
	d, store := paperOrdersSetup(t)

	instID, err := d.Instruments().InsertInstrument(InstrumentInput{
		Market: MarketHK, Symbol: uniqueName(t, "POI"), AssetClass: AssetClassETF,
	})
	if err != nil {
		t.Fatalf("insert instrument: %v", err)
	}
	defer d.Exec(`DELETE FROM instruments WHERE id = $1`, instID) //nolint:errcheck

	id, err := store.InsertOrder(PaperOrderInput{
		InstrumentID: &instID, Mode: OrderModePaper, Side: OrderSideSell, Qty: "1",
	})
	if err != nil {
		t.Fatalf("insert order: %v", err)
	}
	defer d.Exec(`DELETE FROM paper_orders WHERE id = $1`, id) //nolint:errcheck

	// draft 不能直接跳到 submitted / filled / settled。
	for _, illegal := range []string{OrderStatusSubmitted, OrderStatusFilled, OrderStatusSettled} {
		err := store.TransitStatus(id, illegal)
		if err == nil {
			t.Fatalf("draft→%s should be rejected", illegal)
		}
		if !strings.Contains(err.Error(), "非法状态转换") {
			t.Errorf("error should name the illegal transition, got %v", err)
		}
		// 拒绝后状态不能被改动。
		cur, gerr := store.GetOrder(id)
		if gerr != nil {
			t.Fatalf("get after reject: %v", gerr)
		}
		if cur.Status != OrderStatusDraft {
			t.Errorf("status mutated by rejected transit: %q", cur.Status)
		}
	}

	// 推到 rejected 后是终点,任何出边都不合法。
	if err := store.TransitStatus(id, OrderStatusRejected); err != nil {
		t.Fatalf("draft→rejected: %v", err)
	}
	for _, illegal := range []string{OrderStatusRiskPassed, OrderStatusSubmitted, OrderStatusCancelled} {
		if err := store.TransitStatus(id, illegal); err == nil {
			t.Errorf("rejected→%s should be rejected", illegal)
		}
	}

	// 未知状态名必须在查库前被拒。
	if err := store.TransitStatus(id, "flying"); !errors.Is(err, ErrOrderInvalid) {
		t.Errorf("expected ErrOrderInvalid for unknown state, got %v", err)
	}

	// 新建订单只能从 draft / risk_passed 起。
	if _, err := store.InsertOrder(PaperOrderInput{
		InstrumentID: &instID, Mode: OrderModePaper, Side: OrderSideBuy,
		Qty: "1", Status: OrderStatusFilled,
	}); !errors.Is(err, ErrOrderInvalid) {
		t.Errorf("expected ErrOrderInvalid for filled initial status, got %v", err)
	}
	// 非法 side / mode / qty。
	if _, err := store.InsertOrder(PaperOrderInput{
		InstrumentID: &instID, Mode: "margin", Side: OrderSideBuy, Qty: "1",
	}); !errors.Is(err, ErrOrderInvalid) {
		t.Errorf("expected ErrOrderInvalid for bad mode, got %v", err)
	}
	if _, err := store.InsertOrder(PaperOrderInput{
		InstrumentID: &instID, Mode: OrderModePaper, Side: "short", Qty: "1",
	}); !errors.Is(err, ErrOrderInvalid) {
		t.Errorf("expected ErrOrderInvalid for bad side, got %v", err)
	}
	if _, err := store.InsertOrder(PaperOrderInput{
		InstrumentID: &instID, Mode: OrderModePaper, Side: OrderSideBuy, Qty: "lots",
	}); !errors.Is(err, ErrOrderInvalid) {
		t.Errorf("expected ErrOrderInvalid for bad qty, got %v", err)
	}
	// 不存在的订单。
	if err := store.TransitStatus(-1, OrderStatusRiskPassed); !errors.Is(err, ErrOrderNotFound) {
		t.Errorf("expected ErrOrderNotFound, got %v", err)
	}
}

// TestPaperOrderListAndEquityCurve 验证按任务列举与成交曲线聚合。
func TestPaperOrderListAndEquityCurve(t *testing.T) {
	d, store := paperOrdersSetup(t)

	instID, err := d.Instruments().InsertInstrument(InstrumentInput{
		Market: MarketCN, Symbol: uniqueName(t, "POE"), AssetClass: AssetClassStock,
	})
	if err != nil {
		t.Fatalf("insert instrument: %v", err)
	}
	defer d.Exec(`DELETE FROM instruments WHERE id = $1`, instID) //nolint:errcheck

	taskID := int64(990006)
	id, err := store.InsertOrder(PaperOrderInput{
		TaskID: &taskID, InstrumentID: &instID,
		Mode: OrderModePaper, Side: OrderSideBuy, Qty: "10", Price: "100",
	})
	if err != nil {
		t.Fatalf("insert order: %v", err)
	}
	defer d.Exec(`DELETE FROM paper_orders WHERE id = $1`, id) //nolint:errcheck

	list, err := store.ListOrdersByTask(taskID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 order, got %d", len(list))
	}
	if list[0].TaskID == nil || *list[0].TaskID != taskID {
		t.Errorf("task_id = %v", list[0].TaskID)
	}

	// 推到 filled 并写入成交量/成交价,曲线里应出现这一笔。
	for _, next := range []string{OrderStatusRiskPassed, OrderStatusSubmitted, OrderStatusFilled} {
		if err := store.TransitStatus(id, next); err != nil {
			t.Fatalf("transit %s: %v", next, err)
		}
	}
	if _, err := d.Exec(`
UPDATE paper_orders SET filled_qty = 10, avg_fill_price = 101.25 WHERE id = $1`, id); err != nil {
		t.Fatalf("fill order: %v", err)
	}

	curve, err := store.GetEquityCurve(OrderModePaper)
	if err != nil {
		t.Fatalf("equity curve: %v", err)
	}
	if len(curve) == 0 {
		t.Fatal("expected at least one curve point")
	}
	// 全库可能有其它 paper 单,只断言这一天包含本笔的成交额(10*101.25=1012.5)。
	var found bool
	for _, p := range curve {
		if p.Day == "" {
			t.Error("empty day in curve point")
		}
		if p.OrderCount <= 0 {
			t.Errorf("non-positive order_count: %d", p.OrderCount)
		}
		if p.FilledAmount == "1012.50000000" || p.FilledAmount == "1012.5" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected filled amount 1012.5 in curve, got %+v", curve)
	}

	// 非法 mode。
	if _, err := store.GetEquityCurve("margin"); !errors.Is(err, ErrOrderInvalid) {
		t.Errorf("expected ErrOrderInvalid, got %v", err)
	}
	if _, err := store.ListOrdersByTask(0); !errors.Is(err, ErrOrderInvalid) {
		t.Errorf("expected ErrOrderInvalid for zero task, got %v", err)
	}
}
