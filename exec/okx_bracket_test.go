package exec

import (
	"testing"
)

func TestValidateBracketLongDirection(t *testing.T) {
	base := BracketInput{Side: "buy", Action: "entry", Qty: "1", RefPrice: "200"}
	// tp 低于 ref：方向错
	bad := base
	bad.TpPrice, bad.SlPrice = "190", "180"
	if err := ValidateBracket(bad); err == nil {
		t.Fatal("多单 tp<ref 应被拒")
	}
	good := base
	good.TpPrice, good.SlPrice = "215", "190"
	if err := ValidateBracket(good); err != nil {
		t.Fatalf("合理参数应通过，得到 %v", err)
	}
}

func TestValidateBracketRequiresBoth(t *testing.T) {
	in := BracketInput{Side: "buy", Action: "entry", Qty: "1", RefPrice: "200", TpPrice: "215"}
	if err := ValidateBracket(in); err == nil {
		t.Fatal("缺 sl 应被拒")
	}
}

func TestValidateBracketStopTooWide(t *testing.T) {
	in := BracketInput{Side: "buy", Action: "entry", Qty: "1", RefPrice: "200", TpPrice: "260", SlPrice: "100"}
	err := ValidateBracket(in)
	if err == nil {
		t.Fatal("止损 50% 距离应被拒")
	}
}

func TestValidateBracketLowRewardRisk(t *testing.T) {
	// risk=10, reward=5 → 0.5R
	in := BracketInput{Side: "buy", Action: "entry", Qty: "1", RefPrice: "200", TpPrice: "205", SlPrice: "190"}
	if err := ValidateBracket(in); err == nil {
		t.Fatal("R:R < 1.2 应被拒")
	}
}

func TestValidateBracketExitNeedsPosition(t *testing.T) {
	in := BracketInput{Side: "sell", Action: "exit", Qty: "1", PosSide: "long", PosQty: "0"}
	if err := ValidateBracket(in); err == nil {
		t.Fatal("无持仓的平仓应被拒")
	}
	ok := BracketInput{Side: "sell", Action: "exit", Qty: "1", PosSide: "long", PosQty: "2"}
	if err := ValidateBracket(ok); err != nil {
		t.Fatalf("持多 2 平 1 应通过，得到 %v", err)
	}
	over := BracketInput{Side: "sell", Action: "exit", Qty: "3", PosSide: "long", PosQty: "2"}
	if err := ValidateBracket(over); err == nil {
		t.Fatal("平仓量超过持仓应被拒")
	}
}

func TestValidateBracketExitSideMustBeOpposite(t *testing.T) {
	in := BracketInput{Side: "buy", Action: "exit", Qty: "1", PosSide: "long", PosQty: "2"}
	if err := ValidateBracket(in); err == nil {
		t.Fatal("平多必须 sell，buy 应被拒")
	}
}

func TestValidateBracketShortDirection(t *testing.T) {
	in := BracketInput{Side: "sell", Action: "entry", Qty: "1", RefPrice: "200", TpPrice: "185", SlPrice: "210"}
	if err := ValidateBracket(in); err != nil {
		t.Fatalf("合理空单应通过，得到 %v", err)
	}
	bad := in
	bad.TpPrice, bad.SlPrice = "215", "190"
	if err := ValidateBracket(bad); err == nil {
		t.Fatal("空单 tp>ref 应被拒")
	}
}

func TestDerivePosSide(t *testing.T) {
	if got := derivePosSide("net", "buy", false, ""); got != "net" {
		t.Fatalf("net 模式应恒 net，得到 %q", got)
	}
	if got := derivePosSide("long_short", "buy", false, ""); got != "long" {
		t.Fatalf("双向模式买开应为 long，得到 %q", got)
	}
	if got := derivePosSide("long_short", "sell", true, ""); got != "long" {
		t.Fatalf("双向模式卖平应为 long，得到 %q", got)
	}
}

func TestClOrdIDShape(t *testing.T) {
	for _, id := range []string{DraftClOrdID(9), CloseClOrdID("AAPL-USDT-SWAP", "net", 9)} {
		if len(id) > 32 {
			t.Fatalf("clOrdId 超长: %q", id)
		}
		for _, r := range id {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
				t.Fatalf("clOrdId 含非法字符 %q: %q", r, id)
			}
		}
	}
	// 同一持仓身份必须稳定（否则重复强平会被交易所当成新单）
	if CloseClOrdID("A", "net", 1) != CloseClOrdID("A", "net", 1) {
		t.Fatal("close clOrdId 必须确定性")
	}
}

// 多空边界：两侧的「刚好在限内 / 刚好超限」都要能区分。
// 这两个数是硬编码安全边界，算错的后果是「以为有止损其实没有」。
func TestValidateBracketBoundary(t *testing.T) {
	// 多单：ref=100，sl=80 恰好 20%（含），tp=124 → reward=24/risk=20=1.2R（含）
	if err := ValidateBracket(BracketInput{
		Side: "buy", Action: "entry", Qty: "1", RefPrice: "100", TpPrice: "124", SlPrice: "80",
	}); err != nil {
		t.Fatalf("恰好 20%% 止损 + 1.2R 应通过，得到 %v", err)
	}
	// 多单 sl=79 → 21% > 20%
	if err := ValidateBracket(BracketInput{
		Side: "buy", Action: "entry", Qty: "1", RefPrice: "100", TpPrice: "130", SlPrice: "79",
	}); err == nil {
		t.Fatal("21% 止损应被拒")
	}
	// 空单：ref=100，sl=120 恰好 20%，tp=76 → reward=24/risk=20=1.2R
	if err := ValidateBracket(BracketInput{
		Side: "sell", Action: "entry", Qty: "1", RefPrice: "100", TpPrice: "76", SlPrice: "120",
	}); err != nil {
		t.Fatalf("空单恰好边界应通过，得到 %v", err)
	}
	// 空单 sl=121 → 21%
	if err := ValidateBracket(BracketInput{
		Side: "sell", Action: "entry", Qty: "1", RefPrice: "100", TpPrice: "70", SlPrice: "121",
	}); err == nil {
		t.Fatal("空单 21%% 止损应被拒")
	}
}

// 平衡点：多单与空单镜像参数必须给出相同判定（同一套 R:R 公式）。
func TestValidateBracketLongShortSymmetry(t *testing.T) {
	longErr := ValidateBracket(BracketInput{
		Side: "buy", Action: "entry", Qty: "1", RefPrice: "100", TpPrice: "115", SlPrice: "90",
	})
	shortErr := ValidateBracket(BracketInput{
		Side: "sell", Action: "entry", Qty: "1", RefPrice: "100", TpPrice: "85", SlPrice: "110",
	})
	if (longErr == nil) != (shortErr == nil) {
		t.Fatalf("镜像参数判定应一致：long=%v short=%v", longErr, shortErr)
	}
}

// 开仓时 hint 不得覆盖 side（buy+short 这种组合会被 51000 拒）。
func TestDerivePosSideIgnoresHintOnEntry(t *testing.T) {
	if got := derivePosSide("long_short", "buy", false, "short"); got != "long" {
		t.Fatalf("开仓应忽略 hint，buy→long，得到 %q", got)
	}
	if got := derivePosSide("long_short", "sell", false, "long"); got != "short" {
		t.Fatalf("开仓应忽略 hint，sell→short，得到 %q", got)
	}
	// 平仓仍采信 hint
	if got := derivePosSide("long_short", "buy", true, "short"); got != "short" {
		t.Fatalf("平仓应采信 hint，得到 %q", got)
	}
}

func TestInstTypeOf(t *testing.T) {
	cases := map[string]string{
		"BTC-USDT-SWAP":   "SWAP",
		"AAPL-USDT-SWAP":  "SWAP",
		"btc-usdt-swap":   "SWAP", // 大小写不敏感
		"BTC-USDT":        "SPOT",
		"ETH-USDT":        "SPOT",
		"BTC-USDT-250926": "", // 交割合约
		"":                "",
		"BTC":             "",
	}
	for in, want := range cases {
		if got := InstTypeOf(in); got != want {
			t.Errorf("InstTypeOf(%q) = %q，期望 %q", in, got, want)
		}
	}
}
