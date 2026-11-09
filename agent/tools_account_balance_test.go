package agent

// get_account_balance 的验收测试。
//
// # 这组测试守的是什么
//
// 2026-11-09 任务 #4：目标是「使用全部账户余额」，但系统里没有余额工具，
// agent 只能开 shell 摸 okx CLI，看到 env 里 OKX_API_KEY 为空就误判
// 「交易账户未登录」。同期另一个意图用 quote_okx（公开行情、不需要凭证）
// 就「验证通过」了凭据 —— 结论完全相反，都是错的。
//
// 所以这里钉住三件事：
//  1. 余额解析正确，且只保留非零币种（OKX 会返回全部 200+ 币种）；
//  2. 未装配 client 时给出明确错误，不是空结果；
//  3. 凭证类错误带排查指引（不能用 okx-auth status 判断 API key）。

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeAccount 是注入用的假 AccountClient。
type fakeAccount struct {
	bal *AccountBalance
	err error
}

func (f *fakeAccount) FetchBalance(context.Context) (*AccountBalance, error) {
	return f.bal, f.err
}

// callBalanceTool 调 get_account_balance 并返回输出文本 + 是否错误。
// 复用 tools_market_test.go 里的 callTool（签名含 map 入参）。
func callBalanceTool(t *testing.T, m *MarketToolSet) (string, bool) {
	t.Helper()
	return callTool(t, m.getAccountBalance(), map[string]any{})
}

// TestAccountBalanceOnlyNonZeroCurrencies 只保留非零币种。
//
// OKX 的 details[] 会列出账户支持的**全部**币种，绝大多数余额为 0。
// 全返回会让模型以为「持有 200 种币」，进而算出荒谬的仓位。
func TestAccountBalanceOnlyNonZeroCurrencies(t *testing.T) {
	k := NewOkxClient("")
	// 直接测解析：构造一段真实形状的 CLI 输出（含零余额币种）。
	raw := []byte(`[{
		"totalEq":"30.072844369604777",
		"adjEq":"",
		"upl":"",
		"uTime":"1794215592699",
		"details":[
			{"ccy":"USDT","eq":"30.10724968117349","cashBal":"30.10724968117349","availBal":"30.10724968117349","frozenBal":"0","eqUsd":"30.068110256587964"},
			{"ccy":"BTC","eq":"0","cashBal":"0","availBal":"0","frozenBal":"0","eqUsd":"0"},
			{"ccy":"ETH","eq":"0.00000000","cashBal":"0.00000000","availBal":"0.00000000","frozenBal":"0","eqUsd":"0"}
		]
	}]`)

	bal, err := parseAccountBalance(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if bal.TotalEq != "30.072844369604777" {
		t.Errorf("TotalEq = %q", bal.TotalEq)
	}
	if len(bal.Currencies) != 1 {
		t.Fatalf("只应保留 1 个非零币种，实际 %d: %+v", len(bal.Currencies), bal.Currencies)
	}
	if bal.Currencies[0].Ccy != "USDT" {
		t.Errorf("保留的币种 = %q, want USDT", bal.Currencies[0].Ccy)
	}
	if bal.Currencies[0].AvailBal != "30.10724968117349" {
		t.Errorf("AvailBal = %q", bal.Currencies[0].AvailBal)
	}
	_ = k
}

// TestIsZeroAmount 金额字符串的零判定（不做 float 解析）。
func TestIsZeroAmount(t *testing.T) {
	cases := map[string]bool{
		"":                  true,
		"0":                 true,
		"0.0":               true,
		"0.00000000":        true,
		"-0.0":              true,
		"30.1":              false,
		"0.00000001":        false, // 极小但非零，不能被吞掉
		"1":                 false,
		"-5":                false,
		" 0.00 ":            true,
	}
	for in, want := range cases {
		if got := isZeroAmount(in); got != want {
			t.Errorf("isZeroAmount(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestAccountBalanceToolUnconfigured 未装配 client 时必须明确报错。
//
// 返回空结果会被模型读成「账户余额为 0」，从而得出「没有本金」的错误结论。
func TestAccountBalanceToolUnconfigured(t *testing.T) {
	m := NewMarketToolSet(nil)
	out, _ := callBalanceTool(t, m)
	if !strings.Contains(out, "未配置") {
		t.Errorf("未装配时应报「未配置」，实际: %s", out)
	}
}

// TestAccountBalanceToolUnauthorizedHint 凭证类错误要带排查指引。
//
// agent 拿到 unauthorized 的第一反应是「跑 okx-auth login」，而那是 OAuth
// 登录态，与 API key 凭证完全无关 —— 这条提示就是为了打断那个错误联想。
func TestAccountBalanceToolUnauthorizedHint(t *testing.T) {
	m := NewMarketToolSet(nil)
	m.SetAccountClient(&fakeAccount{err: &MarketError{
		Code:    "UNAUTHORIZED",
		Message: "okx-auth rejected the caller (unauthorized)",
	}})
	out, _ := callBalanceTool(t, m)
	for _, want := range []string{"凭证", "okx-auth", "无关"} {
		if !strings.Contains(out, want) {
			t.Errorf("凭证错误提示应含 %q，实际: %s", want, out)
		}
	}
}

// TestAccountBalanceToolEmptyIsNotError 零余额是合法结果，要显式说明。
func TestAccountBalanceToolEmptyIsNotError(t *testing.T) {
	m := NewMarketToolSet(nil)
	m.SetAccountClient(&fakeAccount{bal: &AccountBalance{TotalEq: "0"}})
	out, isErr := callBalanceTool(t, m)
	if isErr {
		t.Errorf("零余额不该被标成错误结果: %s", out)
	}
	if !strings.Contains(out, "余额均为 0") {
		t.Errorf("应显式说明「不是查询失败」，实际: %s", out)
	}
}

// TestAccountBalanceToolPropagatesGenericError 非凭证错误原样带出。
func TestAccountBalanceToolPropagatesGenericError(t *testing.T) {
	m := NewMarketToolSet(nil)
	m.SetAccountClient(&fakeAccount{err: errors.New("boom")})
	out, _ := callBalanceTool(t, m)
	if !strings.Contains(out, "boom") {
		t.Errorf("应带出原始错误，实际: %s", out)
	}
}
