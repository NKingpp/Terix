package db

import (
	"errors"
	"testing"
)

// signalsSetup opens a DB and returns its SignalStore. Skips if no PG.
func signalsSetup(t *testing.T) (*DB, *SignalStore) {
	t.Helper()
	d := testOpen(t)
	return d, d.Signals()
}

// TestSignalRoundtrip 验证写入 → 按 id 读 → confidence 精度无损 → 改状态。
func TestSignalRoundtrip(t *testing.T) {
	d, store := signalsSetup(t)

	instID, err := d.Instruments().InsertInstrument(InstrumentInput{
		Market: MarketUS, Symbol: uniqueName(t, "SIG"), AssetClass: AssetClassStock,
	})
	if err != nil {
		t.Fatalf("insert instrument: %v", err)
	}
	defer d.Exec(`DELETE FROM instruments WHERE id = $1`, instID) //nolint:errcheck

	taskID := int64(990001)
	id, err := store.InsertSignal(SignalInput{
		TaskID:       &taskID,
		InstrumentID: &instID,
		NodeID:       nil,
		SignalType:   "fundamental",
		Direction:    SignalDirectionLong,
		Confidence:   "0.8500",
		Horizon:      "3m",
		Rationale:    "毛利率改善",
	})
	if err != nil {
		t.Fatalf("insert signal: %v", err)
	}
	defer d.Exec(`DELETE FROM signals WHERE id = $1`, id) //nolint:errcheck

	got, err := store.GetSignal(id)
	if err != nil {
		t.Fatalf("get signal: %v", err)
	}
	if got.Direction != SignalDirectionLong || got.SignalType != "fundamental" {
		t.Errorf("enum mismatch: %+v", got)
	}
	if got.TaskID == nil || *got.TaskID != taskID {
		t.Errorf("task_id = %v", got.TaskID)
	}
	if got.InstrumentID == nil || *got.InstrumentID != instID {
		t.Errorf("instrument_id = %v", got.InstrumentID)
	}
	if got.NodeID != nil {
		t.Errorf("expected nil node_id, got %v", *got.NodeID)
	}
	// NUMERIC(5,4) 必须原样回来,不能因 float64 化成 0.85。
	if got.Confidence == nil || *got.Confidence != "0.8500" {
		t.Errorf("confidence = %v (want \"0.8500\")", got.Confidence)
	}
	if got.Status != SignalStatusOpen {
		t.Errorf("status = %q, want open", got.Status)
	}
	if got.CreatedAt == "" {
		t.Error("expected created_at")
	}

	if err := store.UpdateSignalStatus(id, SignalStatusSettled); err != nil {
		t.Fatalf("update status: %v", err)
	}
	got, err = store.GetSignal(id)
	if err != nil {
		t.Fatalf("re-get signal: %v", err)
	}
	if got.Status != SignalStatusSettled {
		t.Errorf("status = %q, want settled", got.Status)
	}
}

// TestSignalListAndErrors 覆盖按任务列举与各类错误路径。
func TestSignalListAndErrors(t *testing.T) {
	d, store := signalsSetup(t)

	taskID := int64(990002)
	var ids []int64
	for _, dir := range []string{SignalDirectionLong, SignalDirectionShort} {
		id, err := store.InsertSignal(SignalInput{
			TaskID:     &taskID,
			SignalType: "technical",
			Direction:  dir,
		})
		if err != nil {
			t.Fatalf("insert %s: %v", dir, err)
		}
		ids = append(ids, id)
	}
	defer func() {
		for _, id := range ids {
			d.Exec(`DELETE FROM signals WHERE id = $1`, id) //nolint:errcheck
		}
	}()

	list, err := store.ListSignalsByTask(taskID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 signals, got %d", len(list))
	}
	for _, s := range list {
		if s.TaskID == nil || *s.TaskID != taskID {
			t.Errorf("task filter leaked: %v", s.TaskID)
		}
		// confidence 未填必须是 nil,而不是空串或 0。
		if s.Confidence != nil {
			t.Errorf("expected nil confidence, got %v", *s.Confidence)
		}
	}

	// 非法方向必须在落库前被拒。
	if _, err := store.InsertSignal(SignalInput{
		SignalType: "x", Direction: "sideways",
	}); !errors.Is(err, ErrSignalInvalid) {
		t.Errorf("expected ErrSignalInvalid for bad direction, got %v", err)
	}
	// 空 signal_type 也要拒。
	if _, err := store.InsertSignal(SignalInput{Direction: SignalDirectionLong}); !errors.Is(err, ErrSignalInvalid) {
		t.Errorf("expected ErrSignalInvalid for empty signal_type, got %v", err)
	}
	// 非法 confidence 形状。
	if _, err := store.InsertSignal(SignalInput{
		SignalType: "x", Direction: SignalDirectionLong, Confidence: "abc",
	}); !errors.Is(err, ErrSignalInvalid) {
		t.Errorf("expected ErrSignalInvalid for bad confidence, got %v", err)
	}
	// 不存在的信号。
	if _, err := store.GetSignal(-1); !errors.Is(err, ErrSignalNotFound) {
		t.Errorf("expected ErrSignalNotFound, got %v", err)
	}
	if err := store.UpdateSignalStatus(-1, SignalStatusSettled); !errors.Is(err, ErrSignalNotFound) {
		t.Errorf("expected ErrSignalNotFound from update, got %v", err)
	}
	if err := store.UpdateSignalStatus(ids[0], "  "); !errors.Is(err, ErrSignalInvalid) {
		t.Errorf("expected ErrSignalInvalid for empty status, got %v", err)
	}
}
