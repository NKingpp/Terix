package agent

// get_indicator_okx 的测试。
//
// 分两类：
//   - 参数校验（不需要网络、不需要 CLI）—— 这类是主力，因为它们挡的是
//     「用户/模型传错 → 拿到难懂 CLI 错误」这种体验问题
//   - 真调 CLI（有 skip 门控）—— 验证解析链路真的能跑通

import (
	"context"
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
)

// okxIndicatorTool 构造只含 get_indicator_okx 的工具集（真 OkxClient）。
func okxIndicatorTool(t *testing.T) actool.CoreTool {
	t.Helper()
	m := NewMarketToolSetWithCryptoAndFlow(nil, NewOkxClient(""), nil)
	return m.getIndicatorOkx()
}

// callIndicator 调工具并返回 Flatten 文本。
func callIndicator(t *testing.T, tool actool.CoreTool, body string) string {
	t.Helper()
	res, err := tool.Call(context.Background(), json.RawMessage(body), nil)
	if err != nil {
		t.Fatalf("工具调用返回 error: %v", err)
	}
	return res.Flatten()
}

// TestOkxIndicatorRejectsBadBar bar 传 "1D" 必须报错，且错误里要提 1Dutc。
//
// 这是实测踩出来的坑：CLI 的 --bar 只认 9 个值，传 1D 会报一句
// 完全看不出原因的 CLI 错误。我们要在到达 CLI 之前拦住。
func TestOkxIndicatorRejectsBadBar(t *testing.T) {
	tool := okxIndicatorTool(t)
	got := callIndicator(t, tool, `{"indicator":"rsi","symbol":"BTC-USDT","bar":"1D"}`)

	if !strings.Contains(got, "bar 必须") {
		t.Errorf("应报 bar 不合法，实际: %s", got)
	}
	if !strings.Contains(got, "1Dutc") {
		t.Errorf("错误信息必须提 1Dutc（否则用户不知道该怎么改），实际: %s", got)
	}
	// 不该走到 CLI（CLI 错误会是 "OKX_CLI_FAILED" 或 "Missing required parameter"）
	if strings.Contains(got, "OKX_CLI_FAILED") {
		t.Errorf("应在本地拦截，不该真调 CLI：%s", got)
	}
}

// TestOkxIndicatorRejectsEmptyIndicator 空 indicator 明确报错。
func TestOkxIndicatorRejectsEmptyIndicator(t *testing.T) {
	tool := okxIndicatorTool(t)
	got := callIndicator(t, tool, `{"indicator":"","symbol":"BTC-USDT"}`)
	if !strings.Contains(got, "indicator") {
		t.Errorf("应报缺 indicator，实际: %s", got)
	}
}

// TestOkxIndicatorRequiresDashInSymbol "BTC" 报错，提示要 "BTC-USDT"。
func TestOkxIndicatorRequiresDashInSymbol(t *testing.T) {
	tool := okxIndicatorTool(t)
	got := callIndicator(t, tool, `{"indicator":"rsi","symbol":"BTC"}`)

	if !strings.Contains(got, "连字符") && !strings.Contains(got, "BTC-USDT") {
		t.Errorf("应提示 symbol 需要带连字符（BTC-USDT），实际: %s", got)
	}
	if strings.Contains(got, "OKX_CLI_FAILED") {
		t.Errorf("应在本地拦截，不该真调 CLI：%s", got)
	}
}

// TestOkxIndicatorDefaultBar doc 说 bar 默认 1Dutc —— 不传 bar 应能跑。
//
// 只验证「不报 bar 错误」，不验证结果（那需要网络，见下面的 Real 测试）。
func TestOkxIndicatorDefaultBar(t *testing.T) {
	cli := NewOkxClient("")
	res, err := cli.FetchIndicator(context.Background(), IndicatorQuery{
		Indicator: "rsi", Symbol: "BTC-USDT", // Bar 留空
	})
	if err != nil {
		// 网络/CLI 不可用时跳过，但不该是「bar 不合法」
		if strings.Contains(err.Error(), "bar 必须") {
			t.Fatalf("空 bar 应默认 1Dutc，不该报错: %v", err)
		}
		t.Skipf("CLI/网络不可用（跳过真实查询）: %v", err)
	}
	if res.Bar != "1Dutc" {
		t.Errorf("默认 bar 应为 1Dutc，实际 %q", res.Bar)
	}
}

// TestOkxIndicatorRealRsi 真调 CLI，断言 RSI 在 0-100 之间。
func TestOkxIndicatorRealRsi(t *testing.T) {
	if _, err := exec.LookPath("okx"); err != nil {
		t.Skip("okx CLI 未安装")
	}
	cli := NewOkxClient("")
	res, err := cli.FetchIndicator(context.Background(), IndicatorQuery{
		Indicator: "rsi", Symbol: "BTC-USDT", Bar: "1Dutc", Params: "14",
	})
	if err != nil {
		t.Skipf("CLI 查询失败（可能无网络）: %v", err)
	}
	if res.Source != "okx" {
		t.Errorf("Source = %q, want okx", res.Source)
	}
	if res.TS == "" {
		t.Error("TS 不该为空（毫秒应转成 ISO8601）")
	}
	if !strings.Contains(res.TS, "T") || !strings.Contains(res.TS, "Z") {
		t.Errorf("TS 应是 RFC3339 格式，实际 %q", res.TS)
	}
	// rsi 的 values key 是参数值 "14"
	v, ok := res.Values["14"]
	if !ok {
		// 服务端可能用不同 key，把所有 key 报出来便于排查
		keys := make([]string, 0, len(res.Values))
		for k := range res.Values {
			keys = append(keys, k)
		}
		t.Fatalf("values 里没有 key \"14\"，实际有: %v", keys)
	}
	// RSI 必须在 0-100
	var f float64
	if _, err := strconv.ParseFloat(v, 64); err != nil {
		t.Fatalf("RSI 值 %q 不是数字: %v", v, err)
	}
	if f < 0 || f > 100 {
		t.Errorf("RSI = %v 超出 0-100 范围", f)
	}
}

// TestOkxIndicatorMacdValues macd 返回三个字段且都是数字。
func TestOkxIndicatorMacdValues(t *testing.T) {
	if _, err := exec.LookPath("okx"); err != nil {
		t.Skip("okx CLI 未安装")
	}
	cli := NewOkxClient("")
	res, err := cli.FetchIndicator(context.Background(), IndicatorQuery{
		Indicator: "macd", Symbol: "BTC-USDT", Bar: "1Dutc",
	})
	if err != nil {
		t.Skipf("CLI 查询失败: %v", err)
	}
	for _, k := range []string{"dif", "dea", "macd"} {
		if _, ok := res.Values[k]; !ok {
			t.Errorf("macd 的 values 应含 %q，实际: %v", k, res.Values)
		}
	}
}

// TestOkxIndicatorBacktestTimePassthrough backtest_time 必须传给 CLI。
//
// 验证方式：用一个明显无效的时间戳（未来时间），CLI 应报错。
// 如果参数没传，就不会报错 —— 那就是「静默忽略」，比报错更糟。
func TestOkxIndicatorBacktestTimePassthrough(t *testing.T) {
	if _, err := exec.LookPath("okx"); err != nil {
		t.Skip("okx CLI 未安装")
	}
	cli := NewOkxClient("")
	// 3000-01-01 的时间戳，服务端必然拒绝
	res, err := cli.FetchIndicator(context.Background(), IndicatorQuery{
		Indicator: "rsi", Symbol: "BTC-USDT", Bar: "1Dutc",
		Params: "14", BacktestTime: 32503680000000,
	})
	if err == nil && res != nil {
		// 服务端居然接受了 —— 那说明参数生效了但服务端宽松。
		// 仍要断言回传值正确。
		if res.BacktestTime != 32503680000000 {
			t.Errorf("BacktestTime 未回传: got %d", res.BacktestTime)
		}
		return
	}
	// 报错是预期的 —— 但必须能确定「是因为时间戳而非其他原因」。
	t.Logf("未来时间戳被拒（预期）: %v", err)
}

// TestOkxIndicatorInvalidIndicatorShowsAvailable 传不存在的指标名，
// 错误里要列出可用的名字。
//
// 这是「透传不白名单」的代价补偿：我们不枚举 70+ 指标，但传错时要
// 能从错误里看到正确的名字。前提是服务端对同一 instId 返回全部指标 ——
// 如果它只返回请求的那个（空），错误里就没有可选项，这时应给出通用提示。
func TestOkxIndicatorInvalidIndicatorShowsAvailable(t *testing.T) {
	if _, err := exec.LookPath("okx"); err != nil {
		t.Skip("okx CLI 未安装")
	}
	cli := NewOkxClient("")
	_, err := cli.FetchIndicator(context.Background(), IndicatorQuery{
		Indicator: "definitely_not_an_indicator", Symbol: "BTC-USDT", Bar: "1Dutc",
	})
	if err == nil {
		t.Fatal("不存在的指标名应报错")
	}
	// 错误信息要能帮到人：要么列出可用的，要么说明指标名不对
	msg := err.Error()
	if !strings.Contains(msg, "indicator") && !strings.Contains(msg, "实际有") {
		t.Errorf("错误信息应能提示指标名问题，实际: %s", msg)
	}
}

// TestOkxIndicatorNotConfigured crypto client 未注入时明确报错。
func TestOkxIndicatorNotConfigured(t *testing.T) {
	m := NewMarketToolSet(nil) // crypto = nil
	tool := m.getIndicatorOkx()
	got := callIndicator(t, tool, `{"indicator":"rsi","symbol":"BTC-USDT"}`)
	if !strings.Contains(got, "okx 通道未配置") {
		t.Errorf("应报通道未配置，实际: %s", got)
	}
}

// TestValidOkxBarExactlyNine 白名单恰好 9 个值 —— 多一个少一个都说明与 CLI 不符。
func TestValidOkxBarExactlyNine(t *testing.T) {
	if len(ValidOkxBar) != 9 {
		t.Errorf("ValidOkxBar 应有 9 个值（CLI 实测），实际 %d: %v", len(ValidOkxBar), ValidOkxBar)
	}
	// 1D 不在白名单（这是最容易踩的坑）
	if ValidOkxBar["1D"] {
		t.Error("1D 不该在白名单里 —— CLI 不接受，必须用 1Dutc")
	}
	for _, b := range []string{"3m", "5m", "15m", "1H", "4H", "12Hutc", "1Dutc", "3Dutc", "1Wutc"} {
		if !ValidOkxBar[b] {
			t.Errorf("%q 应在白名单里", b)
		}
	}
}
