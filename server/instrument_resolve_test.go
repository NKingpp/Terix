package server

// 标的文本解析的验收测试。
//
// # 这组测试守的是什么
//
// 用户删掉了标的选器，改为在「分析背景/分析目标」里写代码。而后端
// instrument_id 是三条金融链的唯一判据（goals prompt / 阶段图 / 行情取数）。
//
// 解析错了的后果比「没解析出来」更严重：
//   - 把 "the" 认成美股代码 → 建一条脏标的，并且整个任务围绕它跑
//   - 「比较 MSFT 和 AAPL」里取了后出现的那个 → 分析错标的，全程无人报错
//
// 所以这里守的是「优先级与边界」。

import (
	"testing"
)

// TestFindTokenRequiresBoundary 代码匹配要求词边界。
//
// # 为什么这条最要紧
//
// "SPYDER" 里含 "SPY"。没有边界判断时，用户写「像 spyder 一样」会被
// 识别成 SPY 标的，然后整条投研流程对着一只没提过的 ETF 跑。
func TestFindTokenRequiresBoundary(t *testing.T) {
	cases := []struct {
		blob, sym string
		want      bool
	}{
		{"给出 msft 交易策略", "MSFT", true},
		{"给出 MSFT 交易策略", "MSFT", true},
		{"像 spyder 一样", "SPY", false},  // 前缀粘连
		{"SPYDER 是个词", "SPY", false},   // 同上，大写
		{"分析SPY走势", "SPY", true},       // 中文紧邻：中文不是 word 字节
		{"BTC-USDT 怎么看", "BTC", false}, // 应留给更长的 BTC-USDT
		{"BTC-USDT 怎么看", "BTC-USDT", true},
		{"就看 SPY", "SPY", true}, // 句尾
	}
	for _, c := range cases {
		if _, got := findToken(c.blob, c.sym); got != c.want {
			t.Errorf("findToken(%q, %q) = %v, want %v", c.blob, c.sym, got, c.want)
		}
	}
}

// TestInferMarket 代码形态推断市场。
//
// # 为什么可以推断 market 而 asset_class 不行
//
// market 的形态特征足够强（6 位数字=A 股、带连字符=加密、其余=美股），
// 而 asset_class 决定阶段图模板，猜错会让整条链问错问题 —— 所以只推 market。
func TestInferMarket(t *testing.T) {
	cases := map[string]string{
		"MSFT":     "us",
		"AAPL":     "us",
		"600519":   "cn",
		"000300":   "cn",
		"00700":    "hk",
		"BTC-USDT": "crypto",
	}
	for sym, want := range cases {
		if got := inferMarket(sym); got != want {
			t.Errorf("inferMarket(%q) = %q, want %q", sym, got, want)
		}
	}
}

// TestCandidateSymbolsOrderAndDedup 候选按出现顺序去重。
//
// # 为什么顺序重要
//
// createTask 只绑定**第一个**验证通过的候选。顺序错了（比如按长度排）
// 就会绑定错误的标的 ——「比较 MSFT 和 AAPL」必须主看 MSFT。
func TestCandidateSymbolsOrderAndDedup(t *testing.T) {
	got := CandidateSymbols("比较 MSFT 和 AAPL 的走势，MSFT 为主")
	want := []string{"MSFT", "AAPL"}
	if len(got) < len(want) {
		t.Fatalf("候选数 %d 少于期望 %d: %v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("第 %d 个候选 = %q, want %q（完整: %v）", i, got[i], w, got)
		}
	}
	// 去重：MSFT 出现两次，只应留一个
	n := 0
	for _, s := range got {
		if s == "MSFT" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("MSFT 应去重为 1 个，实际 %d 个（完整: %v）", n, got)
	}
}

// TestCandidateSymbolsRejectsLowercase 虚词不算候选。
//
// # 为什么不是「拒绝所有小写」
//
// 用户实际就写「给出 msft 交易策略」。只认全大写会让最常见的写法静默失效。
// 所以小写也算候选 —— 但会先砍掉 the/and/for 这类虚词（它们不可能命中，
// 问了也是白问行情源）。真正的脏数据防线是**行情验证**：验证不过就不登记。
func TestCandidateSymbolsRejectsLowercase(t *testing.T) {
	got := CandidateSymbols("给出 msft 交易策略，看看 the market and you")
	for _, s := range got {
		switch s {
		case "THE", "AND", "YOU":
			t.Errorf("虚词 %q 不该成为候选（完整: %v）", s, got)
		}
	}
	// msft 必须成为候选，且已上抛大写（行情源按大写查）。
	found := false
	for _, s := range got {
		if s == "MSFT" {
			found = true
		}
	}
	if !found {
		t.Errorf("小写 msft 应被切成候选 MSFT（完整: %v）", got)
	}
}

// TestCandidateSymbolsAcceptsCrypto 加密代码带连字符，大小写均可。
func TestCandidateSymbolsAcceptsCrypto(t *testing.T) {
	for _, text := range []string{"分析 BTC-USDT 的资金费率", "分析 btc-usdt 的资金费率"} {
		got := CandidateSymbols(text)
		found := false
		for _, s := range got {
			if s == "BTC-USDT" {
				found = true
			}
		}
		if !found {
			t.Errorf("%q 的 BTC-USDT 应成为候选（完整: %v）", text, got)
		}
	}
}
