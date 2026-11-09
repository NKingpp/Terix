package db

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// watchlistSetup opens a DB and returns its WatchlistStore. Skips if no PG.
func watchlistSetup(t *testing.T) (*DB, *WatchlistStore) {
	t.Helper()
	d := testOpen(t)
	return d, d.Watchlists()
}

// uniqueName 生成每次运行都不冲突的名字,避免残留数据干扰。
func uniqueName(t *testing.T, prefix string) string {
	t.Helper()
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// TestWatchlistRoundtrip 验证建清单 → 加标的 → 列举(带 instruments join)→ 移除。
func TestWatchlistRoundtrip(t *testing.T) {
	d, store := watchlistSetup(t)

	instID, err := d.Instruments().InsertInstrument(InstrumentInput{
		Market: MarketUS, Symbol: uniqueName(t, "WL"), AssetClass: AssetClassStock,
		Name: "Watchlist Roundtrip Corp",
	})
	if err != nil {
		t.Fatalf("insert instrument: %v", err)
	}
	defer d.Exec(`DELETE FROM instruments WHERE id = $1`, instID) //nolint:errcheck

	name := uniqueName(t, "wl")
	wlID, err := store.CreateWatchlist(name, "测试清单")
	if err != nil {
		t.Fatalf("create watchlist: %v", err)
	}
	defer d.Exec(`DELETE FROM watchlists WHERE id = $1`, wlID) //nolint:errcheck

	got, err := store.GetWatchlist(name)
	if err != nil {
		t.Fatalf("get watchlist: %v", err)
	}
	if got.ID != wlID || got.Name != name {
		t.Errorf("watchlist mismatch: %+v", got)
	}
	if got.Note == nil || *got.Note != "测试清单" {
		t.Errorf("note = %v", got.Note)
	}

	if _, err := store.AddWatchlistItem(wlID, instID, "关注中"); err != nil {
		t.Fatalf("add item: %v", err)
	}

	items, err := store.ListWatchlistItems(wlID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	it := items[0]
	if it.InstrumentID != instID {
		t.Errorf("instrument_id = %d", it.InstrumentID)
	}
	// join 出来的标的信息必须带上。
	if it.Symbol == "" || it.Market != MarketUS || it.AssetClass != AssetClassStock {
		t.Errorf("instrument join missing: %+v", it)
	}
	if it.Name == nil || *it.Name != "Watchlist Roundtrip Corp" {
		t.Errorf("name join = %v", it.Name)
	}

	if err := store.RemoveWatchlistItem(wlID, instID); err != nil {
		t.Fatalf("remove item: %v", err)
	}
	items, err = store.ListWatchlistItems(wlID)
	if err != nil {
		t.Fatalf("list after remove: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("expected empty list, got %d", len(items))
	}
}

// TestWatchlistErrors 覆盖重名清单、重复加标的、移除不存在的项。
func TestWatchlistErrors(t *testing.T) {
	d, store := watchlistSetup(t)

	name := uniqueName(t, "wl-err")
	wlID, err := store.CreateWatchlist(name, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer d.Exec(`DELETE FROM watchlists WHERE id = $1`, wlID) //nolint:errcheck

	if _, err := store.CreateWatchlist(name, ""); !errors.Is(err, ErrWatchlistNameConflict) {
		t.Errorf("expected ErrWatchlistNameConflict, got %v", err)
	}
	if _, err := store.CreateWatchlist("   ", ""); !errors.Is(err, ErrWatchlistNameConflict) {
		t.Errorf("expected empty-name rejection, got %v", err)
	}
	if _, err := store.GetWatchlist(uniqueName(t, "no-such")); !errors.Is(err, ErrWatchlistNotFound) {
		t.Errorf("expected ErrWatchlistNotFound, got %v", err)
	}

	instID, err := d.Instruments().InsertInstrument(InstrumentInput{
		Market: MarketHK, Symbol: uniqueName(t, "WLERR"), AssetClass: AssetClassETF,
	})
	if err != nil {
		t.Fatalf("insert instrument: %v", err)
	}
	defer d.Exec(`DELETE FROM instruments WHERE id = $1`, instID) //nolint:errcheck

	if _, err := store.AddWatchlistItem(wlID, instID, ""); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if _, err := store.AddWatchlistItem(wlID, instID, ""); !errors.Is(err, ErrWatchlistItemExists) {
		t.Errorf("expected ErrWatchlistItemExists, got %v", err)
	}
	if err := store.RemoveWatchlistItem(wlID, instID+999999); !errors.Is(err, ErrWatchlistItemNotFound) {
		t.Errorf("expected ErrWatchlistItemNotFound, got %v", err)
	}
}
