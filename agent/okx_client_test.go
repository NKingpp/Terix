package agent

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	actool "github.com/Autumn-27/norma/tool"
)

// TestOkxCandlesParsing 用固定 JSON 测解析逻辑，不依赖真 CLI。
//
// 为什么不用真 CLI：价格随时变，但**解析逻辑的正确性必须可重复验证**。
// 这份 JSON 是真实输出的结构（保留字段数与前 8 位含义，数值仅作示意）。
func TestOkxCandlesParsing(t *testing.T) {
	// 真实格式：位置参数数组，倒序（新→旧），[8] 是 confirm
	raw := []byte(`[
		["1792944000000","80637.1","80955.6","79377.7","80485.9","2674.21973258","214246125.115793966","0","0","0","0"],
		["1792857600000","79801.2","81070","79577.9","80637.1","2314.63281891","186303206.05512087","0","1","0","0"]
	]`)

	bars, err := parseOkxCandles(raw, ChannelOKX)
	if err != nil {
		t.Fatalf("parseOkxCandles: %v", err)
	}
	if len(bars) != 2 {
		t.Fatalf("期望 2 根，实际 %d", len(bars))
	}

	// ① 必须已反转成升序
	if got := tsToTimestamp("1793059200123"); got != "2026-10-27T00:00:00.123Z" {
		t.Errorf("quote timestamp = %q", got)
	}
	if got := tsToDate("1793059200123"); got != "2026-10-27" {
		t.Errorf("trading date = %q", got)
	}
	if got := tsToTimestamp("invalid"); got != "" {
		t.Errorf("invalid quote timestamp = %q, want empty", got)
	}
	if bars[0].TradingDate >= bars[1].TradingDate {
		t.Errorf("未反转成升序：bars[0].date=%s bars[1].date=%s", bars[0].TradingDate, bars[1].TradingDate)
	}

	// ② 字段映射（反转后第一根 = 原始第二行：o=79801.2 h=81070 l=79577.9 c=80637.1）
	if bars[0].Open != "79801.2" {
		t.Errorf("open = %q, want 79801.2", bars[0].Open)
	}
	if bars[0].High != "81070" {
		t.Errorf("high = %q, want 81070", bars[0].High)
	}
	if bars[0].Low != "79577.9" {
		t.Errorf("low = %q, want 79577.9", bars[0].Low)
	}
	if bars[0].Close != "80637.1" {
		t.Errorf("close = %q, want 80637.1", bars[0].Close)
	}
	if bars[0].Volume != "2314.63281891" {
		t.Errorf("volume = %q", bars[0].Volume)
	}
	if bars[0].Amount != "186303206.05512087" {
		t.Errorf("amount = %q", bars[0].Amount)
	}

	// ③ confirm 语义：原第二行 confirm=1（已收盘）→ isPartial=false
	if bars[0].IsPartial {
		t.Error("已收盘 bar 的 isPartial 应为 false（confirm=1）")
	}
	// 原第一行 confirm=0（未收盘）→ isPartial=true，反转后在末尾
	if !bars[1].IsPartial {
		t.Error("未收盘 bar 的 isPartial 应为 true（confirm=0）")
	}

	t.Logf("bars[0]=%s o=%s c=%s partial=%v | bars[1]=%s partial=%v",
		bars[0].TradingDate, bars[0].Open, bars[0].Close, bars[0].IsPartial,
		bars[1].TradingDate, bars[1].IsPartial)
}

// TestOkxCandlesParsingEdgeCases 覆盖畸形输入。
func TestOkxCandlesParsingEdgeCases(t *testing.T) {
	t.Run("空数组", func(t *testing.T) {
		_, err := parseOkxCandles([]byte(`[]`), ChannelOKX)
		if err == nil {
			t.Fatal("空数组应返回错误（不能静默返回空 bars）")
		}
		var me *MarketError
		if !errors.As(err, &me) || me.Code != "OKX_NO_DATA" {
			t.Errorf("错误码应为 OKX_NO_DATA，实际: %v", err)
		}
	})

	t.Run("不是二维数组", func(t *testing.T) {
		if _, err := parseOkxCandles([]byte(`{"foo":1}`), ChannelOKX); err == nil {
			t.Fatal("对象输入应报错")
		}
	})

	t.Run("字段不足 5 个", func(t *testing.T) {
		if _, err := parseOkxCandles([]byte(`[["1792944000000","1","2"]]`), ChannelOKX); err == nil {
			t.Fatal("字段不足应报错（缺 OHLC 无法构造 bar）")
		}
	})

	t.Run("缺 confirm 字段按已收盘处理", func(t *testing.T) {
		// 老版本 CLI 可能不返回 [8]，此时不该判成 partial
		raw := []byte(`[["1792944000000","80637.1","80955.6","79377.7","80485.9","2674.2","214246125.1"]]`)
		bars, err := parseOkxCandles(raw, ChannelOKX)
		if err != nil {
			t.Fatalf("不完整但合法的输入应能解析: %v", err)
		}
		if bars[0].IsPartial {
			t.Error("缺 confirm 时应默认按已收盘处理（isPartial=false）")
		}
	})
}

// TestOkxBarMapping 锁定 period → OKX --bar 的映射。
func TestOkxBarMapping(t *testing.T) {
	cases := map[string]string{
		"daily": "1D", "1d": "1D", "": "1D",
		"weekly": "1W", "monthly": "1M",
		"1h": "1H", "4h": "4H",
	}
	for in, want := range cases {
		got, err := okxBar(in)
		if err != nil {
			t.Errorf("okxBar(%q) 报错: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("okxBar(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := okxBar("bogus"); err == nil {
		t.Error("非法 period 应报错")
	}
}

// TestInstIDForMarket 锁定 symbol → instId 的补全规则。
func TestInstIDForMarket(t *testing.T) {
	cases := map[string]string{
		"BTC":      "BTC-USDT", // 补计价币
		"btc":      "BTC-USDT", // 大写化
		"BTC-USDT": "BTC-USDT", // 已是完整 instId，不动
		"ETH-USDT": "ETH-USDT",
		"":         "",
	}
	for in, want := range cases {
		if got := instIDForMarket(in, ""); got != want {
			t.Errorf("instIDForMarket(%q) = %q, want %q", in, got, want)
		}
	}
	if got := instIDForMarket("BTC", "-SWAP"); got != "BTC-USDT-SWAP" {
		t.Errorf("swap 后缀: got %q", got)
	}
	if got := instIDForMarket("BTC-USDT-SWAP", "-SWAP"); got != "BTC-USDT-SWAP" {
		t.Errorf("已是完整 swap instId 不应重复加后缀: got %q", got)
	}
}

// TestOkxClientChannelGuard 非 okx 通道应被拒绝（防止误用）。
func TestOkxClientChannelGuard(t *testing.T) {
	c := NewOkxClient("/nonexistent/okx")
	ctx := context.Background()
	if _, err := c.FetchQuote(ctx, "BTC", "crypto", ChannelAkshareCN); err == nil {
		t.Error("非 okx 通道应返回 CHANNEL_NOT_APPLICABLE")
	}
	if _, err := c.FetchKline(ctx, "BTC", "crypto", "daily", 10, ChannelAkshareUS); err == nil {
		t.Error("非 okx 通道应返回 CHANNEL_NOT_APPLICABLE")
	}
	if _, err := c.FetchMacro(ctx, "cpi"); err == nil {
		t.Error("OKX 不应提供宏观数据")
	}
}

// TestOkxMissingBinary 二进制不存在时应返回明确错误而非 panic。
func TestOkxMissingBinary(t *testing.T) {
	c := NewOkxClient("/definitely/not/here/okx")
	done, _, err := c.Health(context.Background())
	if err == nil {
		t.Fatal("二进制不存在时 Health 应报错")
	}
	if done {
		t.Error("二进制不存在时不应报告 ready")
	}
	var me *MarketError
	if !errors.As(err, &me) {
		t.Fatalf("错误应是 *MarketError，实际 %T", err)
	}
	if me.Code != "OKX_CLI_NOT_FOUND" && me.Code != "OKX_CLI_FAILED" {
		t.Errorf("错误码应表明 CLI 不可用，实际 %q", me.Code)
	}
}

// TestMarketToolSetCryptoRouting 验证 crypto 走 crypto client，其余走默认 client。
func TestMarketToolSetCryptoRouting(t *testing.T) {
	fakeMarket := &fakeMarketClient{quoteByChannel: map[Channel]struct {
		q   *Quote
		err error
	}{
		ChannelAkshareCN: {q: &Quote{Symbol: "600519", Market: "cn", PriceKind: "daily_close", Last: "1349.98", Channel: ChannelAkshareCN}},
	}}
	fakeCrypto := &fakeMarketClient{quoteByChannel: map[Channel]struct {
		q   *Quote
		err error
	}{
		ChannelOKX: {q: &Quote{Symbol: "BTC", Market: "crypto", PriceKind: "realtime", Last: "80493.9", Channel: ChannelOKX}},
	}}
	mts := NewMarketToolSetWithCrypto(fakeMarket, fakeCrypto)

	var quoteTool actool.CoreTool
	for _, x := range mts.Tools() {
		if x.Name() == "get_quote" {
			quoteTool = x
		}
	}

	// crypto → crypto client（不应碰 fakeMarket）
	out, isErr := callTool(t, quoteTool, map[string]any{"symbol": "BTC", "market": "crypto"})
	if isErr {
		t.Fatalf("crypto 查询失败: %s", out)
	}
	if !strings.Contains(out, "realtime") {
		t.Errorf("crypto 应返回 price_kind=realtime: %s", out)
	}
	if len(fakeMarket.calls) != 0 {
		t.Errorf("crypto 不应调用默认 client，实际: %v", fakeMarket.calls)
	}

	// cn → 默认 client
	cryptoCallsBefore := len(fakeCrypto.calls)
	if _, isErr := callTool(t, quoteTool, map[string]any{"symbol": "600519", "market": "cn"}); isErr {
		t.Fatal("cn 查询失败")
	}
	if len(fakeCrypto.calls) != cryptoCallsBefore {
		t.Errorf("cn 不应调用 crypto client，新增调用: %v", fakeCrypto.calls[cryptoCallsBefore:])
	}

	// crypto 但未注入 crypto client → 明确报错，不 panic
	mtsNoCrypto := NewMarketToolSet(fakeMarket)
	var q2 actool.CoreTool
	for _, x := range mtsNoCrypto.Tools() {
		if x.Name() == "get_quote" {
			q2 = x
		}
	}
	out2, isErr2 := callTool(t, q2, map[string]any{"symbol": "BTC", "market": "crypto"})
	if !isErr2 {
		t.Error("未注入 crypto client 时应返回错误")
	}
	if !strings.Contains(out2, "crypto 通道未配置") {
		t.Errorf("错误信息应说明 crypto 通道未配置: %s", out2)
	}
}

// TestOkxClientIntegration 真调 CLI 的集成测试。
//
// 没有 okx 二进制或没有网络时 skip（不是 fail）。
func TestOkxClientIntegration(t *testing.T) {
	if os.Getenv("OKX_INTEGRATION") != "1" {
		t.Skip("设 OKX_INTEGRATION=1 后运行（需已安装 okx CLI 与网络）")
	}
	c := NewOkxClient("")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ok, detail, err := c.Health(ctx)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !ok {
		t.Fatalf("okx CLI 不可用: %s", detail)
	}
	t.Logf("health: %s", detail)

	t.Run("quote BTC", func(t *testing.T) {
		q, err := c.FetchQuote(ctx, "BTC", "crypto", ChannelOKX)
		if err != nil {
			t.Fatalf("FetchQuote: %v", err)
		}
		if q.Last == "" {
			t.Error("last 不应为空")
		}
		if q.PriceKind != "realtime" {
			t.Errorf("price_kind = %q, want realtime（OKX ticker 是实时价）", q.PriceKind)
		}
		t.Logf("BTC last=%s high=%s low=%s change_pct=%s channel=%s",
			q.Last, q.High, q.Low, q.ChangePct, q.Channel)
	})

	t.Run("kline BTC 升序+confirm", func(t *testing.T) {
		bars, err := c.FetchKline(ctx, "BTC", "crypto", "daily", 3, ChannelOKX)
		if err != nil {
			t.Fatalf("FetchKline: %v", err)
		}
		if len(bars) == 0 {
			t.Fatal("bars 为空")
		}
		// 升序校验（OKX 原生倒序，我们必须反转）
		for i := 1; i < len(bars); i++ {
			if bars[i-1].TradingDate > bars[i].TradingDate {
				t.Errorf("未升序: %s > %s", bars[i-1].TradingDate, bars[i].TradingDate)
			}
		}
		// 最后一根若未收盘，应标 is_partial
		last := bars[len(bars)-1]
		t.Logf("返回 %d 根，最后 %s close=%s is_partial=%v",
			len(bars), last.TradingDate, last.Close, last.IsPartial)
	})
}
