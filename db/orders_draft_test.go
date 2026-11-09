package db

import (
	"errors"
	"testing"
)

// draftsSetup opens a DB and returns its OrderDraftStore. Skips if no PG.
func draftsSetup(t *testing.T) (*DB, *OrderDraftStore) {
	t.Helper()
	d := testOpen(t)
	return d, d.OrderDrafts()
}

// TestOrderDraftRoundtrip 验证写入 → 读回 → 数量精度 → 风控结论更新。
func TestOrderDraftRoundtrip(t *testing.T) {
	d, store := draftsSetup(t)

	instID, err := d.Instruments().InsertInstrument(InstrumentInput{
		Market: MarketUS, Symbol: uniqueName(t, "DRF"), AssetClass: AssetClassStock,
	})
	if err != nil {
		t.Fatalf("insert instrument: %v", err)
	}
	defer d.Exec(`DELETE FROM instruments WHERE id = $1`, instID) //nolint:errcheck

	id, err := store.InsertDraft(OrderDraftInput{
		InstrumentID: &instID,
		Side:         OrderSideBuy,
		Qty:          "123.45678901",
		RefPrice:     "987.6543210000",
		ExecutedNote: "打算分两笔",
	})
	if err != nil {
		t.Fatalf("insert draft: %v", err)
	}
	defer d.Exec(`DELETE FROM orders_draft WHERE id = $1`, id) //nolint:errcheck

	got, err := store.GetDraft(id)
	if err != nil {
		t.Fatalf("get draft: %v", err)
	}
	if got.Side != OrderSideBuy {
		t.Errorf("side = %q", got.Side)
	}
	// NUMERIC(28,10) 精确到 10 位小数,不能因 float64 变成 123.45678901 的近似值。
	if got.Qty != "123.4567890100" {
		t.Errorf("qty = %q, want \"123.4567890100\"", got.Qty)
	}
	if got.RefPrice == nil || *got.RefPrice != "987.6543210000" {
		t.Errorf("ref_price = %v", got.RefPrice)
	}
	// 未风控时 risk_verdict 必须是 NULL。
	if got.RiskVerdict != nil {
		t.Errorf("expected nil risk_verdict, got %v", *got.RiskVerdict)
	}
	if got.ExportedAt != nil {
		t.Errorf("expected nil exported_at, got %v", *got.ExportedAt)
	}
	if got.ExecutedNote == nil || *got.ExecutedNote != "打算分两笔" {
		t.Errorf("executed_note = %v", got.ExecutedNote)
	}

	if err := store.UpdateRiskVerdict(id, RiskVerdictPass); err != nil {
		t.Fatalf("update verdict: %v", err)
	}
	got, err = store.GetDraft(id)
	if err != nil {
		t.Fatalf("re-get draft: %v", err)
	}
	if got.RiskVerdict == nil || *got.RiskVerdict != RiskVerdictPass {
		t.Errorf("risk_verdict = %v", got.RiskVerdict)
	}
}

// TestOrderDraftListAndErrors 覆盖按任务列举、已导出不可改、非法入参。
func TestOrderDraftListAndErrors(t *testing.T) {
	d, store := draftsSetup(t)

	instID, err := d.Instruments().InsertInstrument(InstrumentInput{
		Market: MarketCN, Symbol: uniqueName(t, "DRFL"), AssetClass: AssetClassStock,
	})
	if err != nil {
		t.Fatalf("insert instrument: %v", err)
	}
	defer d.Exec(`DELETE FROM instruments WHERE id = $1`, instID) //nolint:errcheck

	taskID := int64(990004)
	sigID, err := d.Signals().InsertSignal(SignalInput{
		TaskID: &taskID, InstrumentID: &instID,
		SignalType: "fundamental", Direction: SignalDirectionLong,
	})
	if err != nil {
		t.Fatalf("insert signal: %v", err)
	}
	defer d.Exec(`DELETE FROM signals WHERE id = $1`, sigID) //nolint:errcheck

	draftID, err := store.InsertDraft(OrderDraftInput{
		SignalID: &sigID, InstrumentID: &instID,
		Side: OrderSideBuy, Qty: "10",
	})
	if err != nil {
		t.Fatalf("insert draft: %v", err)
	}
	defer d.Exec(`DELETE FROM orders_draft WHERE id = $1`, draftID) //nolint:errcheck

	list, err := store.ListDraftsByTask(taskID)
	if err != nil {
		t.Fatalf("list by task: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 draft, got %d", len(list))
	}
	// task_id 由 JOIN signals 带出,必须非空且正确。
	if list[0].TaskID == nil || *list[0].TaskID != taskID {
		t.Errorf("task_id join = %v", list[0].TaskID)
	}

	// 已导出的草稿不允许再改风控结论。
	if _, err := d.Exec(`UPDATE orders_draft SET exported_at = now() WHERE id = $1`, draftID); err != nil {
		t.Fatalf("mark exported: %v", err)
	}
	if err := store.UpdateRiskVerdict(draftID, RiskVerdictPass); !errors.Is(err, ErrDraftInvalid) {
		t.Errorf("expected ErrDraftInvalid for exported draft, got %v", err)
	}
	// 未传 signal_id 的草稿不出现在任务列表里(LEFT JOIN 语义上属于别的任务)。
	orphanID, err := store.InsertDraft(OrderDraftInput{
		InstrumentID: &instID, Side: OrderSideSell, Qty: "5",
	})
	if err != nil {
		t.Fatalf("insert orphan draft: %v", err)
	}
	defer d.Exec(`DELETE FROM orders_draft WHERE id = $1`, orphanID) //nolint:errcheck

	list, err = store.ListDraftsByTask(taskID)
	if err != nil {
		t.Fatalf("re-list by task: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("orphan draft leaked into task list: %d rows", len(list))
	}

	// 非法入参。
	if _, err := store.InsertDraft(OrderDraftInput{Side: "hold", Qty: "1"}); !errors.Is(err, ErrDraftInvalid) {
		t.Errorf("expected ErrDraftInvalid for bad side, got %v", err)
	}
	if _, err := store.InsertDraft(OrderDraftInput{Side: OrderSideBuy, Qty: "abc"}); !errors.Is(err, ErrDraftInvalid) {
		t.Errorf("expected ErrDraftInvalid for bad qty, got %v", err)
	}
	if _, err := store.InsertDraft(OrderDraftInput{Side: OrderSideBuy, Qty: "0"}); !errors.Is(err, ErrDraftInvalid) {
		t.Errorf("expected ErrDraftInvalid for zero qty, got %v", err)
	}
	if _, err := store.InsertDraft(OrderDraftInput{
		Side: OrderSideBuy, Qty: "1", RiskVerdict: "maybe",
	}); !errors.Is(err, ErrDraftInvalid) {
		t.Errorf("expected ErrDraftInvalid for bad verdict, got %v", err)
	}
	if _, err := store.GetDraft(-1); !errors.Is(err, ErrDraftNotFound) {
		t.Errorf("expected ErrDraftNotFound, got %v", err)
	}
	if err := store.UpdateRiskVerdict(-1, RiskVerdictPass); !errors.Is(err, ErrDraftNotFound) {
		t.Errorf("expected ErrDraftNotFound from verdict update, got %v", err)
	}
	if err := store.UpdateRiskVerdict(draftID, "nope"); !errors.Is(err, ErrDraftInvalid) {
		t.Errorf("expected ErrDraftInvalid for bad verdict arg, got %v", err)
	}
}
