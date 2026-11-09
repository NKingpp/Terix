package db

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// instrumentSetup opens a DB and returns its InstrumentStore. Skips if no PG.
func instrumentSetup(t *testing.T) (*DB, *InstrumentStore) {
	t.Helper()
	d := testOpen(t)
	return d, d.Instruments()
}

// uniqueInstrumentSymbol 生成每次运行都不冲突的代码,避免残留数据干扰。
func uniqueInstrumentSymbol(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("TST%d", time.Now().UnixNano())
}

// TestInstrumentInsertGetRoundtrip 验证写入后按 (market, symbol) 能原样读回。
func TestInstrumentInsertGetRoundtrip(t *testing.T) {
	d, store := instrumentSetup(t)

	active := true
	in := InstrumentInput{
		Market:     MarketUS,
		Symbol:     uniqueInstrumentSymbol(t),
		Name:       "Roundtrip Test Corp",
		AssetClass: AssetClassStock,
		Currency:   "USD",
		Exchange:   "NASDAQ",
		Sector:     "Technology",
		Industry:   "Software",
		Active:     &active,
	}
	id, err := store.InsertInstrument(in)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero id")
	}
	defer d.Exec(`DELETE FROM instruments WHERE id = $1`, id) //nolint:errcheck

	got, err := store.GetInstrumentByMarketSymbol(in.Market, in.Symbol)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != id {
		t.Errorf("id mismatch: got %d want %d", got.ID, id)
	}
	if got.Market != in.Market || got.Symbol != in.Symbol {
		t.Errorf("identity mismatch: %+v", got)
	}
	if got.AssetClass != AssetClassStock {
		t.Errorf("asset_class = %q", got.AssetClass)
	}
	if got.Name == nil || *got.Name != in.Name {
		t.Errorf("name = %v", got.Name)
	}
	if got.Sector == nil || *got.Sector != in.Sector {
		t.Errorf("sector = %v", got.Sector)
	}
	if !got.Active {
		t.Error("expected active = true")
	}
	if got.CreatedAt == "" {
		t.Error("expected created_at to be populated")
	}

	// 可选字段留空应落成 NULL,读回为 nil。
	id2, err := store.InsertInstrument(InstrumentInput{
		Market:     MarketHK,
		Symbol:     in.Symbol,
		AssetClass: AssetClassETF,
	})
	if err != nil {
		t.Fatalf("insert minimal: %v", err)
	}
	defer d.Exec(`DELETE FROM instruments WHERE id = $1`, id2) //nolint:errcheck

	minimal, err := store.GetInstrumentByMarketSymbol(MarketHK, in.Symbol)
	if err != nil {
		t.Fatalf("get minimal: %v", err)
	}
	if minimal.Name != nil || minimal.Sector != nil || minimal.Currency != nil {
		t.Errorf("expected NULL optional columns, got %+v", minimal)
	}
	if !minimal.Active {
		t.Error("expected default active = true")
	}

	// 未命中必须返回 ErrInstrumentNotFound。
	if _, err := store.GetInstrumentByMarketSymbol(MarketCN, "NO_SUCH_SYMBOL_X"); !errors.Is(err, ErrInstrumentNotFound) {
		t.Errorf("expected ErrInstrumentNotFound, got %v", err)
	}
}

// TestInstrumentDuplicateMarketSymbol 验证同一 (market, symbol) 重复插入被拒。
func TestInstrumentDuplicateMarketSymbol(t *testing.T) {
	d, store := instrumentSetup(t)

	symbol := uniqueInstrumentSymbol(t)
	in := InstrumentInput{
		Market:     MarketCN,
		Symbol:     symbol,
		AssetClass: AssetClassStock,
	}
	id, err := store.InsertInstrument(in)
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}
	defer d.Exec(`DELETE FROM instruments WHERE id = $1`, id) //nolint:errcheck

	_, err = store.InsertInstrument(in)
	if !errors.Is(err, ErrInstrumentExists) {
		t.Fatalf("expected ErrInstrumentExists, got %v", err)
	}

	// 不同 market、相同 symbol 应允许 — 唯一键是 (market, symbol) 组合。
	id2, err := store.InsertInstrument(InstrumentInput{
		Market:     MarketUS,
		Symbol:     symbol,
		AssetClass: AssetClassStock,
	})
	if err != nil {
		t.Fatalf("cross-market insert: %v", err)
	}
	defer d.Exec(`DELETE FROM instruments WHERE id = $1`, id2) //nolint:errcheck
}

// TestInstrumentListFilters 验证 ListInstruments 的 market / active 过滤。
func TestInstrumentListFilters(t *testing.T) {
	d, store := instrumentSetup(t)

	symbol := uniqueInstrumentSymbol(t)
	activeID, err := store.InsertInstrument(InstrumentInput{
		Market: MarketUS, Symbol: symbol + "A", AssetClass: AssetClassStock,
	})
	if err != nil {
		t.Fatalf("insert active: %v", err)
	}
	defer d.Exec(`DELETE FROM instruments WHERE id = $1`, activeID) //nolint:errcheck

	inactive := false
	inactiveID, err := store.InsertInstrument(InstrumentInput{
		Market: MarketUS, Symbol: symbol + "B", AssetClass: AssetClassStock, Active: &inactive,
	})
	if err != nil {
		t.Fatalf("insert inactive: %v", err)
	}
	defer d.Exec(`DELETE FROM instruments WHERE id = $1`, inactiveID) //nolint:errcheck

	onlyActive := true
	rows, err := store.ListInstruments(InstrumentFilter{Market: MarketUS, Active: &onlyActive})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("expected at least one active us instrument")
	}
	for _, r := range rows {
		if r.Market != MarketUS {
			t.Errorf("market filter leaked: %q", r.Market)
		}
		if !r.Active {
			t.Errorf("active filter leaked: id %d", r.ID)
		}
	}

	// 非法 market 应在查库前被拒。
	if _, err := store.ListInstruments(InstrumentFilter{Market: "mars"}); !errors.Is(err, ErrInstrumentInvalid) {
		t.Errorf("expected ErrInstrumentInvalid, got %v", err)
	}
}

// TestInstrumentValidation 验证字段校验先于 SQL 执行。
func TestInstrumentValidation(t *testing.T) {
	_, store := instrumentSetup(t)

	cases := []struct {
		name string
		in   InstrumentInput
	}{
		{"bad market", InstrumentInput{Market: "jp", Symbol: "X", AssetClass: AssetClassStock}},
		{"empty symbol", InstrumentInput{Market: MarketUS, Symbol: "  ", AssetClass: AssetClassStock}},
		{"bad asset class", InstrumentInput{Market: MarketUS, Symbol: "X", AssetClass: "bond"}},
	}
	for _, c := range cases {
		if _, err := store.InsertInstrument(c.in); !errors.Is(err, ErrInstrumentInvalid) {
			t.Errorf("%s: expected ErrInstrumentInvalid, got %v", c.name, err)
		}
	}
}
