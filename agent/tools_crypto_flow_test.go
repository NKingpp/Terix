package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
)

// fakeFlowClient 是可控的 CryptoFlowClient。
type fakeFlowClient struct {
	fr     *FundingRate
	frErr  error
	oi     *OpenInterest
	oiErr  error
	frHits int
	oiHits int
}

func (f *fakeFlowClient) FetchFundingRate(ctx context.Context, symbol string) (*FundingRate, error) {
	f.frHits++
	return f.fr, f.frErr
}

func (f *fakeFlowClient) FetchOpenInterest(ctx context.Context, symbol string) (*OpenInterest, error) {
	f.oiHits++
	return f.oi, f.oiErr
}

// cryptoFlowTool 取 get_crypto_flow 工具实例。
func cryptoFlowTool(t *testing.T, flow CryptoFlowClient) actool.CoreTool {
	t.Helper()
	mts := NewMarketToolSetWithCryptoAndFlow(nil, nil, flow)
	for _, x := range mts.Tools() {
		if x.Name() == "get_crypto_flow" {
			return x
		}
	}
	t.Fatal("get_crypto_flow 未注册")
	return nil
}

// TestToSwapInstID 锁定 symbol → SWAP instID 的推导规则。
func TestToSwapInstID(t *testing.T) {
	ok := map[string]string{
		"BTC-USDT":      "BTC-USDT-SWAP",
		"BTC-USDT-SWAP": "BTC-USDT-SWAP", // 幂等
		"BTC":           "BTC-USDT-SWAP", // 先补计价币
		"btc-usdt":      "BTC-USDT-SWAP", // 大写化
		"ETH-USDT":      "ETH-USDT-SWAP",
	}
	for in, want := range ok {
		got, err := toSwapInstID(in)
		if err != nil {
			t.Errorf("toSwapInstID(%q) 报错: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("toSwapInstID(%q) = %q, want %q", in, got, want)
		}
	}

	// 交割合约必须拒绝（P0 只支持永续）
	if _, err := toSwapInstID("BTC-USDT-250926"); err == nil {
		t.Error("交割合约应报错")
	} else if !strings.Contains(err.Error(), "交割合约") {
		t.Errorf("错误信息应说明是交割合约: %v", err)
	}

	// 空 symbol
	if _, err := toSwapInstID(""); err == nil {
		t.Error("空 symbol 应报错")
	}
}

// TestGetCryptoFlowRejectsNonCrypto 非 crypto 必须明确报错，**不返回空结果**。
func TestGetCryptoFlowRejectsNonCrypto(t *testing.T) {
	for _, market := range []string{"cn", "hk", "us", ""} {
		f := &fakeFlowClient{
			fr: &FundingRate{Rate: "0.0001"},
			oi: &OpenInterest{OI: "1"},
		}
		tool := cryptoFlowTool(t, f)
		out, isErr := callTool(t, tool, map[string]any{"symbol": "600519", "market": market})
		if !isErr {
			t.Errorf("market=%q 应返回错误结果，实际: %s", market, out)
		}
		if !strings.Contains(out, "仅适用于 crypto") {
			t.Errorf("market=%q 错误信息应含「仅适用于 crypto」: %s", market, out)
		}
		// 关键：不能走到 client 调用
		if f.frHits != 0 || f.oiHits != 0 {
			t.Errorf("market=%q 不应调用 client，实际 fr=%d oi=%d", market, f.frHits, f.oiHits)
		}
	}
}

// TestGetCryptoFlowPartialFailure 一项成功一项失败 → 返回部分数据 + unavailable。
//
// 这是最容易写错的一条：很多人会整体返回 error，导致 LLM 拿不到已有的那半。
func TestGetCryptoFlowPartialFailure(t *testing.T) {
	t.Run("funding 成功 OI 失败", func(t *testing.T) {
		f := &fakeFlowClient{
			fr:    &FundingRate{InstID: "BTC-USDT-SWAP", Rate: "0.0001000000000000", FundingTime: "1793030400000"},
			oiErr: &MarketError{Code: "OKX_NO_DATA", Message: "空数组", Channel: ChannelOKX, Retryable: true},
		}
		tool := cryptoFlowTool(t, f)
		out, isErr := callTool(t, tool, map[string]any{"symbol": "BTC-USDT", "market": "crypto"})
		if isErr {
			t.Fatalf("部分成功不应整体失败: %s", out)
		}

		var got struct {
			FundingRate  *FundingRate  `json:"funding_rate"`
			OpenInterest *OpenInterest `json:"open_interest"`
			Unavailable  []struct {
				Field  string `json:"field"`
				Reason string `json:"reason"`
			} `json:"unavailable"`
		}
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("响应不是合法 JSON: %v\n%s", err, out)
		}
		if got.FundingRate == nil || got.FundingRate.Rate != "0.0001000000000000" {
			t.Errorf("funding_rate 应有值: %+v", got.FundingRate)
		}
		if got.OpenInterest != nil {
			t.Errorf("open_interest 应为 null: %+v", got.OpenInterest)
		}
		if len(got.Unavailable) != 1 {
			t.Fatalf("unavailable 应有 1 条，实际 %d", len(got.Unavailable))
		}
		if got.Unavailable[0].Field != "open_interest" {
			t.Errorf("unavailable.field = %q, want open_interest", got.Unavailable[0].Field)
		}
		if got.Unavailable[0].Reason != "OKX_NO_DATA" {
			t.Errorf("unavailable.reason = %q, want OKX_NO_DATA", got.Unavailable[0].Reason)
		}
	})

	t.Run("OI 成功 funding 失败", func(t *testing.T) {
		f := &fakeFlowClient{
			frErr: &MarketError{Code: "OKX_NO_DATA", Message: "无资金费率", Channel: ChannelOKX, Retryable: true},
			oi:    &OpenInterest{InstID: "BTC-USDT-SWAP", OI: "3115964.53", OIUsd: "2502811261.71"},
		}
		tool := cryptoFlowTool(t, f)
		out, isErr := callTool(t, tool, map[string]any{"symbol": "BTC-USDT", "market": "crypto"})
		if isErr {
			t.Fatalf("部分成功不应整体失败: %s", out)
		}
		var got struct {
			FundingRate  *FundingRate  `json:"funding_rate"`
			OpenInterest *OpenInterest `json:"open_interest"`
			Unavailable  []struct {
				Field string `json:"field"`
			} `json:"unavailable"`
		}
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if got.FundingRate != nil {
			t.Error("funding_rate 应为 null")
		}
		if got.OpenInterest == nil || got.OpenInterest.OIUsd == "" {
			t.Errorf("open_interest 应有值: %+v", got.OpenInterest)
		}
		if len(got.Unavailable) != 1 || got.Unavailable[0].Field != "funding_rate" {
			t.Errorf("unavailable 应指出 funding_rate: %+v", got.Unavailable)
		}
	})
}

// TestGetCryptoFlowBothFail 两个都失败 → 整体错误。
func TestGetCryptoFlowBothFail(t *testing.T) {
	f := &fakeFlowClient{
		frErr: &MarketError{Code: "OKX_NO_DATA", Message: "a", Channel: ChannelOKX},
		oiErr: &MarketError{Code: "OKX_NO_DATA", Message: "b", Channel: ChannelOKX},
	}
	tool := cryptoFlowTool(t, f)
	out, isErr := callTool(t, tool, map[string]any{"symbol": "BTC-USDT", "market": "crypto"})
	if !isErr {
		t.Fatalf("两项都失败应返回错误结果，实际: %s", out)
	}
	if !strings.Contains(out, "全部失败") {
		t.Errorf("错误信息应说明全部失败: %s", out)
	}
}

// TestCryptoFlowNilClient 通道未配置 → 明确错误，不 panic。
func TestCryptoFlowNilClient(t *testing.T) {
	tool := cryptoFlowTool(t, nil)
	out, isErr := callTool(t, tool, map[string]any{"symbol": "BTC-USDT", "market": "crypto"})
	if !isErr {
		t.Fatalf("nil client 应返回错误: %s", out)
	}
	if !strings.Contains(out, "OKX 通道未配置") {
		t.Errorf("错误信息应说明通道未配置: %s", out)
	}
}

// TestGetCryptoFlowSuccess 两项都成功 → 完整数据 + 多空比显式标注不支持。
func TestGetCryptoFlowSuccess(t *testing.T) {
	f := &fakeFlowClient{
		fr: &FundingRate{
			InstID: "BTC-USDT-SWAP", Rate: "0.0001000000000000",
			FundingTime: "1793030400000", NextFundingTime: "1793059200000",
		},
		oi: &OpenInterest{
			InstID: "BTC-USDT-SWAP", OI: "3115964.53",
			OICcy: "31159.6453", OIUsd: "2502811261.715670843497", TS: "1793019389282",
		},
	}
	tool := cryptoFlowTool(t, f)
	out, isErr := callTool(t, tool, map[string]any{"symbol": "BTC-USDT", "market": "crypto"})
	if isErr {
		t.Fatalf("应成功: %s", out)
	}

	var got struct {
		Symbol           string        `json:"symbol"`
		PriceKind        string        `json:"price_kind"`
		FundingRate      *FundingRate  `json:"funding_rate"`
		OpenInterest     *OpenInterest `json:"open_interest"`
		Unavailable      []any         `json:"unavailable"`
		LongShortRatio   *string       `json:"long_short_ratio"`
		UnsupportedField []string      `json:"unsupported_fields"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("解析失败: %v\n%s", err, out)
	}
	if got.PriceKind != "realtime" {
		t.Errorf("price_kind = %q, want realtime", got.PriceKind)
	}
	if got.FundingRate == nil || got.FundingRate.Rate == "" {
		t.Error("funding_rate 应有值")
	}
	if got.OpenInterest == nil || got.OpenInterest.OIUsd == "" {
		t.Error("open_interest 应有值")
	}
	if len(got.Unavailable) != 0 {
		t.Errorf("全部成功时 unavailable 应为空，实际: %+v", got.Unavailable)
	}
	// 多空比必须显式标注不支持，不能静默缺失
	if got.LongShortRatio != nil {
		t.Error("long_short_ratio 应为 null（OKX 无此数据）")
	}
	if len(got.UnsupportedField) == 0 {
		t.Error("unsupported_fields 应说明多空比不支持")
	} else if !strings.Contains(strings.Join(got.UnsupportedField, " "), "long_short_ratio") {
		t.Errorf("unsupported_fields 应提及 long_short_ratio: %v", got.UnsupportedField)
	}

	t.Logf("响应: %s", out)
}

// TestMarketErrCode 提取错误码。
func TestMarketErrCode(t *testing.T) {
	if got := marketErrCode(&MarketError{Code: "OKX_NO_DATA"}); got != "OKX_NO_DATA" {
		t.Errorf("got %q", got)
	}
	if got := marketErrCode(errors.New("plain")); got != "UNKNOWN" {
		t.Errorf("非 MarketError 应返回 UNKNOWN，got %q", got)
	}
	if got := marketErrCode(nil); got != "UNKNOWN" {
		t.Errorf("nil 应返回 UNKNOWN，got %q", got)
	}
}

// TestOkxCryptoFlowIntegration 真调 CLI 的集成测试。
func TestOkxCryptoFlowIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过集成测试")
	}
	c := NewOkxClient("")
	ctx, cancel := context.WithTimeout(context.Background(), 60*1e9)
	defer cancel()

	if _, _, err := c.Health(ctx); err != nil {
		t.Skipf("okx CLI 不可用，跳过: %v", err)
	}

	fr, err := c.FetchFundingRate(ctx, "BTC-USDT")
	if err != nil {
		t.Fatalf("FetchFundingRate: %v", err)
	}
	if fr.Rate == "" {
		t.Error("funding rate 不应为空")
	}
	if fr.InstID != "BTC-USDT-SWAP" {
		t.Errorf("instId = %q, want BTC-USDT-SWAP", fr.InstID)
	}
	t.Logf("funding: instId=%s rate=%s fundingTime=%s nextFundingTime=%s",
		fr.InstID, fr.Rate, fr.FundingTime, fr.NextFundingTime)

	oi, err := c.FetchOpenInterest(ctx, "BTC-USDT")
	if err != nil {
		t.Fatalf("FetchOpenInterest: %v", err)
	}
	if oi.OIUsd == "" {
		t.Error("oiUsd 不应为空")
	}
	if oi.InstID != "BTC-USDT-SWAP" {
		t.Errorf("instId = %q, want BTC-USDT-SWAP", oi.InstID)
	}
	t.Logf("openInterest: instId=%s oi=%s oiCcy=%s oiUsd=%s",
		oi.InstID, oi.OI, oi.OICcy, oi.OIUsd)
}
