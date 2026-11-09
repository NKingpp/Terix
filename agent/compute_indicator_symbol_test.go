package agent

// compute_indicator 的 symbol 模式（2026-11-05）。
//
// # 为什么加这个模式
//
// 原实现只接受显式 OHLCV 数组。模型取 250 根 K 线后**手动抄写**数组，
// 实测抄出 high=248 / low=249 / close=249 这种不齐的长度：
//
//	计算失败: 指标输入序列长度不一致: high=248 low=249 close=249
//
// 它又重试 6 次仍失败（每次试图「修数字」而不是重新取数）。
//
// 根因是接口设计：让 LLM 搬运 750+ 个价格数字必然出错。
// 给 symbol 让工具自己取，从根上消除这个错误类型。

import (
	"strconv"
	"strings"
	"testing"
)

// TestComputeIndicatorRequiresSomeData 完全不给数据必须报明确错误。
//
// # 为什么要这条
//
// 报错文案必须告诉模型**怎么修**。只说「缺少 closes」会让它以为
// 只是漏传了一个字段，于是手动补数组 —— 而那正是问题所在。
// 新文案必须点出「传 symbol 让工具自己取」这条路。
func TestComputeIndicatorRequiresSomeData(t *testing.T) {
	m := NewMarketToolSet(nil)
	out, isErr := callTool(t, m.computeIndicator(), map[string]any{"indicator": "rsi"})
	if !isErr {
		t.Fatalf("缺数据应返回错误结果")
	}
	if !strings.Contains(out, "symbol") {
		t.Errorf("错误信息应提示用 symbol 模式，实际: %s", out)
	}
}

// TestComputeIndicatorSymbolModeFailsLoudlyWhenNoClient 无 client 时给出明确原因。
//
// symbol 模式要取数，需要 MarketClient。nil client 时必须说「取数未启用」，
// 而不是 panic 或返回空指标（空指标会被当成「数据就是空的」）。
func TestComputeIndicatorSymbolModeFailsLoudlyWhenNoClient(t *testing.T) {
	m := NewMarketToolSet(nil)
	out, isErr := callTool(t, m.computeIndicator(), map[string]any{
		"indicator": "rsi", "symbol": "BTC-USDT", "market": "crypto",
	})
	if !isErr {
		t.Fatalf("无 client 应返回错误结果")
	}
	if !strings.Contains(out, "未启用") && !strings.Contains(out, "取数") {
		t.Errorf("无 client 时应说明取数未启用，实际: %s", out)
	}
}

// TestComputeIndicatorSymbolModeUsesFetchedBars 有 client 时按 symbol 自动取数。
//
// 用 fakeMarketClient 提供固定的 bar 序列，验证工具自己去取 K 线
// （而不是要求调用方传数组），且结果里带 symbol 便于下游写 fact。
func TestComputeIndicatorSymbolModeUsesFetchedBars(t *testing.T) {
	f := &fakeMarketClient{
		klineByChannel: map[Channel]struct {
			bars []Bar
			err  error
		}{ChannelOKX: {bars: makeTestBars(30)}},
	}
	m := NewMarketToolSetWithCrypto(f, f)

	out, isErr := callTool(t, m.computeIndicator(), map[string]any{
		"indicator": "rsi", "symbol": "TEST-USDT", "market": "crypto",
	})
	if isErr {
		t.Fatalf("symbol 模式应成功，实际报错: %s", out)
	}
	if !strings.Contains(out, "TEST-USDT") {
		t.Errorf("结果应含 symbol（便于事实溯源），实际: %s", out)
	}
}

// makeTestBars 造 n 根已收盘 bar，价格递增（RSI 需要非恒定序列）。
func makeTestBars(n int) []Bar {
	out := make([]Bar, 0, n)
	for i := 0; i < n; i++ {
		v := strconv.Itoa(100 + i)
		out = append(out, Bar{
			TradingDate: "2026-01-01",
			Open:        v,
			High:        strconv.Itoa(102 + i),
			Low:         strconv.Itoa(98 + i),
			Close:       v,
			Volume:      "1000",
		})
	}
	return out
}

// TestIndicatorLengthMismatchMessageIsActionable 长度不一致的报错要能指向修法。
//
// 现行为是硬报错（正确）。这条锁住「错误信息里带各序列长度」，
// 否则模型看不出是哪个数组短了。
func TestIndicatorLengthMismatchMessageIsActionable(t *testing.T) {
	// 用 atr：它同时读 highs/lows/closes，才能暴露长度不一致。
	// rsi 只读 closes，缺 highs/lows 也不报错 —— 用它测不出这个问题。
	_, err := runIndicator("atr",
		[]string{"1", "2"},      // highs: 2
		[]string{"1", "2", "3"}, // lows: 3
		[]string{"1", "2", "3"}, // closes: 3
		nil, 0)
	if err == nil {
		t.Fatal("长度不一致应报错")
	}
	msg := err.Error()
	for _, want := range []string{"high", "low", "close"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息应含 %q（指出哪个序列不齐），实际: %s", want, msg)
		}
	}
}
