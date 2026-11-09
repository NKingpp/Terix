package agent

// 按源行情工具的「返回值契约」测试（2026-11-03）。
//
// # 为什么单独成文件
//
// tools_market_source_test.go 测的是「工具是否存在、是否按可用性注册」。
// 这里测的是「模型看到的文案与字段名」—— 契约层，不是装配层。
//
// 起因：用户指出 quote_mx 的 open/high/low/volume 恒为空串，
// 需要明确写进 agent 上下文，否则会被误读为「0（跌停/停牌）」。

import (
	"strings"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
)

// TestSourceToolsPromptExplainsEmptyOHLC 工具描述必须说明空值语义。
//
// # 这条守的是「agent 不会误读空字符串」
//
// quote_mx 的 open/high/low/volume 恒为空串。若描述里不写清楚，
// agent 可能解读为「0（跌停/停牌）」或「数据坏了要重试」。
//
// norma 的 Registry.Schemas() 会把 Spec.Prompt 追加到 Description 末尾
// （tool/registry.go:50-53），所以这里把两者拼起来断言 —— 那才是模型看到的全文。
func TestSourceToolsPromptExplainsEmptyOHLC(t *testing.T) {
	seed := NewMarketToolSet(nil)
	var mxTool actool.CoreTool
	for _, tl := range seed.Tools() {
		if tl.Name() == "quote_mx" {
			mxTool = tl
			break
		}
	}
	if mxTool == nil {
		t.Fatal("种子壳缺 quote_mx")
	}

	// 模型实际看到的是 Description + Prompt 拼接
	full := mxTool.Description() + "\n\n" + mxTool.Prompt()

	for _, want := range []string{
		"只有收盘价",         // 能力边界
		"空字符串",          // 空值形态
		"不是 0",          // 否定「值为 0」
		"停牌",            // 否定「停牌」
		"quote_akshare", // 指向替代工具
	} {
		if !strings.Contains(full, want) {
			t.Errorf("quote_mx 描述缺 %q —— agent 可能误读空 OHLC 字段", want)
		}
	}
}

// TestFullOHLCToolsDeclareCompleteness 返回完整 OHLC 的工具也要声明完整性。
//
// # 为什么两边都要写
//
// 只说「mx 字段不全」不够：agent 需要知道**另一个**工具是完整的，
// 才会在需要振幅/量比时主动换源。若 quote_akshare 的描述里没这句，
// agent 可能以为所有工具都只给收盘价。
func TestFullOHLCToolsDeclareCompleteness(t *testing.T) {
	m := NewMarketToolSet(&fakeQuoteClient{})
	m2 := NewMarketToolSetWithCrypto(&fakeQuoteClient{}, &fakeQuoteClient{})

	for _, ts := range []*MarketToolSet{m, m2} {
		for _, tl := range ts.sourceQuoteTools() {
			if tl.Name() == "quote_mx" {
				continue // 它本来就不完整，另有断言
			}
			full := tl.Description() + "\n\n" + tl.Prompt()
			if !strings.Contains(full, "完整 OHLC") {
				t.Errorf("%s 应说明返回完整 OHLC，实际描述未提及", tl.Name())
			}
		}
	}
}

// TestQuoteToMapUsesLowercaseKeys 返回字段必须全是小写（与 get_quote 一致）。
//
// # 为什么这条重要
//
// Quote 没有 json tag。若直接 json.Marshal，键是 "Symbol"/"Last"/"Open"。
// 而 get_quote 手写小写键（tools_market.go:377-393）。
// 同类工具返回不同大小写，agent 必须在两个工具间切换字段名，极易读错。
func TestQuoteToMapUsesLowercaseKeys(t *testing.T) {
	q := &Quote{
		Symbol: "600519", Market: "cn", PriceKind: "daily_close",
		TradingDate: "2026-11-02", Last: "1355.9", Open: "1300",
		High: "1400", Low: "1290", Volume: "100",
		Channel: ChannelMX,
	}
	m := quoteToMap(q)

	for k := range m {
		if k != strings.ToLower(k) {
			t.Errorf("字段名 %q 不是小写 —— 与 get_quote 不一致", k)
		}
	}

	for k, want := range map[string]string{
		"symbol": "600519", "market": "cn", "last": "1355.9",
		"open": "1300", "high": "1400", "low": "1290", "volume": "100",
		"trading_date": "2026-11-02", "price_kind": "daily_close",
	} {
		if got, _ := m[k].(string); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if m["channel"] != "mx" {
		t.Errorf("channel = %v, want mx", m["channel"])
	}
}

// TestSeedAndRuntimeMxToolSharePrompt 种子壳与运行实例的 Prompt 必须一致。
//
// # 为什么
//
// 种子壳写 tools 表（UI 显示、模型看到的描述都来自那里），
// 运行实例真正执行。两处 Prompt 不一致 = 表里说的与实际行为脱节，
// 而这种漂移不会报错，只会让 agent 拿到过时的契约。
func TestSeedAndRuntimeMxToolSharePrompt(t *testing.T) {
	seed := NewMarketToolSet(nil)
	var seedPrompt string
	for _, tl := range seed.Tools() {
		if tl.Name() == "quote_mx" {
			seedPrompt = tl.Prompt()
		}
	}
	if seedPrompt == "" {
		t.Fatal("种子壳 quote_mx 的 Prompt 为空")
	}

	// 运行实例需要 mx client；用 NewMXClient("") + 环境变量可能拿不到，
	// 所以直接比对常量 —— 两个构造点都引用 mxQuotePrompt。
	if seedPrompt != mxQuotePrompt {
		t.Error("种子壳 Prompt 与 mxQuotePrompt 常量不一致")
	}
}
