package agent

import (
	"strings"
	"testing"
	"time"

	actool "github.com/Autumn-27/norma/tool"

	"github.com/Autumn-27/artex/db"
)

// TestSnapshotParamsHashDeterministic 同参数必须算出同一个 hash，
// 且键顺序不影响结果（字典序）。
func TestSnapshotParamsHashDeterministic(t *testing.T) {
	a := snapshotParamsHash("kline", map[string]string{
		"symbol": "600519", "market": "cn", "period": "daily", "count": "250", "channel": "akshare:cn",
	})
	// 故意用不同插入顺序构造同一个 map
	b := snapshotParamsHash("kline", map[string]string{
		"channel": "akshare:cn", "count": "250", "period": "daily",
		"market": "cn", "symbol": "600519",
	})
	if a != b {
		t.Errorf("同参数不同构造顺序算出不同 hash:\n  %s\n  %s", a, b)
	}
	if len(a) != 64 {
		t.Errorf("hash 应为 64 位十六进制，实际 %d", len(a))
	}
}

// TestSnapshotParamsHashDistinguishes 不同参数必须算出不同 hash。
//
// 特别是 channel：akshare:cn 与 okx 对同一 symbol 返回的数据不同，
// 不进 hash 会错误去重。
func TestSnapshotParamsHashDistinguishes(t *testing.T) {
	base := map[string]string{
		"symbol": "600519", "market": "cn", "period": "daily", "count": "250", "channel": "akshare:cn",
	}
	baseHash := snapshotParamsHash("kline", base)

	cases := map[string]map[string]string{
		"不同 symbol":  {"symbol": "000001", "market": "cn", "period": "daily", "count": "250", "channel": "akshare:cn"},
		"不同 market":  {"symbol": "600519", "market": "hk", "period": "daily", "count": "250", "channel": "akshare:cn"},
		"不同 period":  {"symbol": "600519", "market": "cn", "period": "weekly", "count": "250", "channel": "akshare:cn"},
		"不同 count":   {"symbol": "600519", "market": "cn", "period": "daily", "count": "10", "channel": "akshare:cn"},
		"不同 channel": {"symbol": "600519", "market": "cn", "period": "daily", "count": "250", "channel": "okx"},
	}
	for name, p := range cases {
		if got := snapshotParamsHash("kline", p); got == baseHash {
			t.Errorf("%s 应算出不同 hash，实际相同 %s", name, got)
		}
	}

	// 不同 endpoint 也要区分
	other := snapshotParamsHash("quote", base)
	if other == baseHash {
		t.Error("不同 endpoint 应算出不同 hash")
	}
}

// TestSnapshotParamsHashEscapesValues 值里含分隔符时不应破坏结构。
func TestSnapshotParamsHashEscapesValues(t *testing.T) {
	a := snapshotParamsHash("kline", map[string]string{"a": "1|b=2"})
	b := snapshotParamsHash("kline", map[string]string{"a": "1", "b": "2"})
	if a == b {
		t.Error("值里的 | 和 = 未被转义，不同的参数组合算出同一 hash")
	}
}

// TestSnapshotChecksumSeparator 分隔符必须让「字段边界」明确。
//
// 用 \x1f 而不是 |：值里可能含 |（如某些 symbol），不会含控制字符。
func TestSnapshotChecksumSeparator(t *testing.T) {
	a := snapshotChecksum("ab", "c")
	b := snapshotChecksum("a", "bc")
	if a == b {
		t.Error("字段边界不清：[\"ab\",\"c\"] 与 [\"a\",\"bc\"] 算出同一 checksum")
	}
}

// TestBarsChecksumOrderMatters bar 顺序变了 checksum 必须变。
func TestBarsChecksumOrderMatters(t *testing.T) {
	b1 := Bar{TradingDate: "2026-10-25", Open: "1", Close: "2"}
	b2 := Bar{TradingDate: "2026-10-26", Open: "3", Close: "4"}
	a := barsChecksum([]Bar{b1, b2})
	b := barsChecksum([]Bar{b2, b1})
	if a == b {
		t.Error("bar 顺序不同应算出不同 checksum")
	}
	if a != barsChecksum([]Bar{b1, b2}) {
		t.Error("同内容应算出同 checksum（不具确定性）")
	}
}

// TestQuoteChecksumDetectsChange 内容变化必须反映到 checksum。
func TestQuoteChecksumDetectsChange(t *testing.T) {
	base := Quote{
		TradingDate: "2026-10-26", Last: "1349.98", Open: "1325.40",
		High: "1350.00", Low: "1324.79", PrevClose: "1335.00",
		ChangePct: "1.1221", Volume: "4159916", Amount: "5574287630",
	}
	h := quoteChecksum(&base)

	changed := base
	changed.Last = "1350.00"
	if quoteChecksum(&changed) == h {
		t.Error("Last 变化应反映到 checksum")
	}

	changed2 := base
	changed2.IsPartial = true
	if quoteChecksum(&changed2) == h {
		t.Error("IsPartial 变化应反映到 checksum")
	}

	if quoteChecksum(&base) != h {
		t.Error("同内容应算出同 checksum")
	}
}

// TestClosedBarsFilters 未收盘 bar 必须被过滤掉。
func TestClosedBarsFilters(t *testing.T) {
	bars := []Bar{
		{TradingDate: "2026-10-24", IsPartial: false},
		{TradingDate: "2026-10-25", IsPartial: false},
		{TradingDate: "2026-10-26", IsPartial: true}, // 当日未收盘
	}
	got := closedBars(bars)
	if len(got) != 2 {
		t.Fatalf("应过滤掉未收盘 bar，剩 2 根，实际 %d", len(got))
	}
	for _, b := range got {
		if b.IsPartial {
			t.Errorf("未收盘 bar 未被过滤: %+v", b)
		}
	}

	// 全部未收盘 → 空
	allPartial := []Bar{{IsPartial: true}, {IsPartial: true}}
	if len(closedBars(allPartial)) != 0 {
		t.Error("全部未收盘时应返回空")
	}
}

// TestCaptureKlineSnapshotDedup 同日同参数重复落库应命中去重。
func TestCaptureKlineSnapshotDedup(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { d.Close() })

	bars := []Bar{
		{TradingDate: "2026-10-25", Open: "1", High: "2", Low: "0.5", Close: "1.5"},
		{TradingDate: "2026-10-26", Open: "2", High: "3", Low: "1.5", Close: "2.5"},
	}
	id1, err := captureKlineSnapshot(d, "600519", "cn", "daily", 2, ChannelAkshareCN, bars)
	if err != nil {
		t.Fatalf("第一次落库: %v", err)
	}
	if id1 == 0 {
		t.Fatal("应返回非零 snapshot id")
	}

	id2, err := captureKlineSnapshot(d, "600519", "cn", "daily", 2, ChannelAkshareCN, bars)
	if err != nil {
		t.Fatalf("第二次落库: %v", err)
	}
	if id2 != id1 {
		t.Errorf("同内容重复落库应命中去重返回同一 id: %d vs %d", id1, id2)
	}
	t.Cleanup(func() { _, _ = d.Exec("DELETE FROM snapshots WHERE id=$1", id1) })
}

// TestCaptureKlineSnapshotSkipsAllPartial 全部未收盘时不落快照。
func TestCaptureKlineSnapshotSkipsAllPartial(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { d.Close() })

	bars := []Bar{
		{TradingDate: "2026-10-26", Open: "1", Close: "2", IsPartial: true},
	}
	id, err := captureKlineSnapshot(d, "600519", "cn", "daily", 1, ChannelAkshareCN, bars)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if id != 0 {
		t.Errorf("全部未收盘时应返回 0（不落快照），实际 %d", id)
	}
}

// TestCaptureQuoteSnapshotSkipsPartial is_partial 的 quote 不落快照。
func TestCaptureQuoteSnapshotSkipsPartial(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { d.Close() })

	q := &Quote{
		Symbol: "600519", Market: "cn", TradingDate: "2026-10-26",
		Last: "1349.98", IsPartial: true, Channel: ChannelAkshareCN,
	}
	id, err := captureQuoteSnapshot(d, q)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if id != 0 {
		t.Errorf("is_partial 的 quote 不落快照，应返回 0，实际 %d", id)
	}
}

// TestCaptureQuoteSnapshotCryptoUsesOKXSource crypto 的快照 source 应为 okx。
func TestCaptureQuoteSnapshotCryptoUsesOKXSource(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { d.Close() })

	q := &Quote{
		Symbol: "BTC-USDT", Market: "crypto", TradingDate: "2026-10-26",
		Last: "80412.1", PriceKind: "realtime", Channel: ChannelOKX,
	}
	id, err := captureQuoteSnapshot(d, q)
	if err != nil {
		t.Fatalf("落库: %v", err)
	}
	if id == 0 {
		t.Fatal("应返回非零 id")
	}
	t.Cleanup(func() { _, _ = d.Exec("DELETE FROM snapshots WHERE id=$1", id) })

	snap, err := d.Snapshots().GetSnapshotByDedupKey(
		"okx", "/quote",
		snapshotParamsHash("quote", map[string]string{
			"symbol": "BTC-USDT", "market": "crypto", "channel": "okx",
		}),
		"2026-10-26",
	)
	if err != nil {
		t.Fatalf("查快照: %v", err)
	}
	if snap.Source != "okx" {
		t.Errorf("source = %q, want okx", snap.Source)
	}
}

// TestCaptureQuoteSnapshotNoTradingDate 无业务时间时不落快照（不用采集时间冒充）。
func TestCaptureQuoteSnapshotNoTradingDate(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { d.Close() })

	q := &Quote{Symbol: "600519", Market: "cn", TradingDate: "", Last: "1"}
	id, err := captureQuoteSnapshot(d, q)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if id != 0 {
		t.Errorf("无 trading_date 时不应落快照，实际 %d", id)
	}
}

// TestSnapshotNilDB 无 DB 时落快照返回错误（由调用方忽略并记日志）。
func TestSnapshotNilDB(t *testing.T) {
	if _, err := captureKlineSnapshot(nil, "600519", "cn", "daily", 1, ChannelAkshareCN,
		[]Bar{{TradingDate: "2026-10-26"}}); err == nil {
		t.Error("nil DB 应返回错误")
	}
	if _, err := captureQuoteSnapshot(nil, &Quote{TradingDate: "2026-10-26"}); err == nil {
		t.Error("nil DB 应返回错误")
	}
}

// TestSnapshotChecksumConflictWarns 同 dedup key 但 checksum 不同时应命中已有记录
// （created=false），内容不覆盖。warn 日志由 db 层打印，此处验证数据语义。
func TestSnapshotChecksumConflictKeepsExisting(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { d.Close() })

	base := db.SnapshotInput{
		Source:      "test-conflict",
		Endpoint:    "/conflict",
		ParamsHash:  "ph-conflict-" + time.Now().Format("150405.000000000"),
		CollectedAt: "2026-10-26",
	}
	first := base
	first.Checksum = "checksum-A"
	snap1, created1, err := d.Snapshots().InsertSnapshot(first)
	if err != nil {
		t.Fatalf("第一次: %v", err)
	}
	if !created1 {
		t.Fatal("第一次应创建")
	}
	t.Cleanup(func() { _, _ = d.Exec("DELETE FROM snapshots WHERE id=$1", snap1.ID) })

	// 同 dedup key，不同 checksum
	second := base
	second.Checksum = "checksum-B"
	snap2, created2, err := d.Snapshots().InsertSnapshot(second)
	if err != nil {
		t.Fatalf("第二次: %v", err)
	}
	if created2 {
		t.Error("同 dedup key 应命中去重（created=false）")
	}
	if snap2.ID != snap1.ID {
		t.Errorf("应返回已有记录的 id: %d vs %d", snap1.ID, snap2.ID)
	}
	// 内容不被覆盖（L3 语义：丢弃新内容而非覆盖）
	if snap2.Checksum != "checksum-A" {
		t.Errorf("内容被覆盖了（checksum=%q），L3 语义是丢弃新内容", snap2.Checksum)
	}
}

// TestHandlersOmitSnapshotIDWhenNoDB 未注入 DB 时两个 handler 都不应输出 snapshot_id。
func TestHandlersOmitSnapshotIDWhenNoDB(t *testing.T) {
	f := &fakeMarketClient{
		quoteByChannel: map[Channel]struct {
			q   *Quote
			err error
		}{
			ChannelAkshareCN: {q: &Quote{
				Symbol: "600519", Market: "cn", PriceKind: "daily_close",
				TradingDate: "2026-10-26", Last: "1349.98", Channel: ChannelAkshareCN,
			}},
		},
		klineByChannel: map[Channel]struct {
			bars []Bar
			err  error
		}{
			ChannelAkshareCN: {bars: []Bar{
				{TradingDate: "2026-10-26", Open: "1", High: "2", Low: "0.5", Close: "1.5"},
			}},
		},
	}
	mts := NewMarketToolSet(f) // 不调 SetDB
	for _, name := range []string{"get_quote", "get_kline"} {
		tool := findMarketTool(t, mts, name)
		in := map[string]any{"symbol": "600519", "market": "cn"}
		if name == "get_kline" {
			in["period"] = "daily"
			in["count"] = 1
		}
		out, isErr := callTool(t, tool, in)
		if isErr {
			t.Fatalf("%s 应成功: %s", name, out)
		}
		if strings.Contains(out, "snapshot_id") {
			t.Errorf("%s 未注入 DB 时不应输出 snapshot_id: %s", name, out)
		}
	}
}

// findMarketTool 从 MarketToolSet 里按名取工具。
func findMarketTool(t *testing.T, mts *MarketToolSet, name string) actool.CoreTool {
	t.Helper()
	for _, x := range mts.Tools() {
		if x.Name() == name {
			return x
		}
	}
	t.Fatalf("工具 %q 未注册", name)
	return nil
}
