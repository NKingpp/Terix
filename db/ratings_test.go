package db

import (
	"errors"
	"testing"
)

// ratingsSetup opens a DB and returns its RatingStore. Skips if no PG.
func ratingsSetup(t *testing.T) (*DB, *RatingStore) {
	t.Helper()
	d := testOpen(t)
	return d, d.Ratings()
}

// TestRatingRoundtrip 验证写入 → 读回 → evidence_node_ids 数组完整 → 精度无损。
func TestRatingRoundtrip(t *testing.T) {
	d, store := ratingsSetup(t)

	instID, err := d.Instruments().InsertInstrument(InstrumentInput{
		Market: MarketUS, Symbol: uniqueName(t, "RAT"), AssetClass: AssetClassStock,
	})
	if err != nil {
		t.Fatalf("insert instrument: %v", err)
	}
	defer d.Exec(`DELETE FROM instruments WHERE id = $1`, instID) //nolint:errcheck

	taskID := int64(990003)
	id, err := store.InsertRating(RatingInput{
		TaskID:          &taskID,
		InstrumentID:    &instID,
		Rating:          RatingOverweight,
		Confidence:      "0.7500",
		DecidedAt:       "2026-03-01T09:30:00Z",
		EvidenceNodeIDs: []int64{101, 202, 303},
	})
	if err != nil {
		t.Fatalf("insert rating: %v", err)
	}
	defer d.Exec(`DELETE FROM ratings WHERE id = $1`, id) //nolint:errcheck

	got, err := store.GetRating(id)
	if err != nil {
		t.Fatalf("get rating: %v", err)
	}
	if got.Rating != RatingOverweight {
		t.Errorf("rating = %q", got.Rating)
	}
	if got.Confidence == nil || *got.Confidence != "0.7500" {
		t.Errorf("confidence = %v (want \"0.7500\")", got.Confidence)
	}
	if got.DecidedAt == nil {
		t.Error("expected decided_at")
	}
	// BIGINT[] 必须是完整有序的 3 个元素 — 这是 array 走 to_json 的关键验证点。
	if len(got.EvidenceNodeIDs) != 3 {
		t.Fatalf("expected 3 evidence ids, got %v", got.EvidenceNodeIDs)
	}
	want := []int64{101, 202, 303}
	for i, w := range want {
		if got.EvidenceNodeIDs[i] != w {
			t.Errorf("evidence[%d] = %d, want %d", i, got.EvidenceNodeIDs[i], w)
		}
	}
	if got.TaskID == nil || *got.TaskID != taskID {
		t.Errorf("task_id = %v", got.TaskID)
	}
}

// TestRatingListAndErrors 覆盖空证据数组、按标的列举、非法评级。
func TestRatingListAndErrors(t *testing.T) {
	d, store := ratingsSetup(t)

	instID, err := d.Instruments().InsertInstrument(InstrumentInput{
		Market: MarketHK, Symbol: uniqueName(t, "RATL"), AssetClass: AssetClassETF,
	})
	if err != nil {
		t.Fatalf("insert instrument: %v", err)
	}
	defer d.Exec(`DELETE FROM instruments WHERE id = $1`, instID) //nolint:errcheck

	// 不传证据 → 数组为空切片而非 nil,调用方不必判空。
	id1, err := store.InsertRating(RatingInput{
		InstrumentID: &instID, Rating: RatingHold,
	})
	if err != nil {
		t.Fatalf("insert no-evidence rating: %v", err)
	}
	defer d.Exec(`DELETE FROM ratings WHERE id = $1`, id1) //nolint:errcheck

	got, err := store.GetRating(id1)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.EvidenceNodeIDs == nil {
		t.Error("expected non-nil empty slice for missing evidence")
	}
	if len(got.EvidenceNodeIDs) != 0 {
		t.Errorf("expected empty evidence, got %v", got.EvidenceNodeIDs)
	}
	if got.Confidence != nil {
		t.Errorf("expected nil confidence, got %v", *got.Confidence)
	}
	if got.DecidedAt != nil {
		t.Errorf("expected nil decided_at, got %v", *got.DecidedAt)
	}

	id2, err := store.InsertRating(RatingInput{
		InstrumentID: &instID, Rating: RatingSell, EvidenceNodeIDs: []int64{7},
	})
	if err != nil {
		t.Fatalf("insert sell rating: %v", err)
	}
	defer d.Exec(`DELETE FROM ratings WHERE id = $1`, id2) //nolint:errcheck

	list, err := store.ListRatingsByInstrument(instID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 ratings, got %d", len(list))
	}
	for _, r := range list {
		if r.InstrumentID == nil || *r.InstrumentID != instID {
			t.Errorf("instrument filter leaked: %v", r.InstrumentID)
		}
	}

	// 非法评级取值必须在落库前被拒。
	if _, err := store.InsertRating(RatingInput{
		InstrumentID: &instID, Rating: "strong_buy",
	}); !errors.Is(err, ErrRatingInvalid) {
		t.Errorf("expected ErrRatingInvalid, got %v", err)
	}
	// 非法 confidence 形状。
	if _, err := store.InsertRating(RatingInput{
		InstrumentID: &instID, Rating: RatingBuy, Confidence: "high",
	}); !errors.Is(err, ErrRatingInvalid) {
		t.Errorf("expected ErrRatingInvalid for bad confidence, got %v", err)
	}
	// 不存在的记录。
	if _, err := store.GetRating(-1); !errors.Is(err, ErrRatingNotFound) {
		t.Errorf("expected ErrRatingNotFound, got %v", err)
	}
}
