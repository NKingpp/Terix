package server

import "testing"

// 改止盈止损的方向校验。
//
// # 为什么这条必须是单测而不是「交给交易所」
//
// 「多单止损设在现价上方」在 OKX 侧是**合法请求**，结果却是立刻触发平仓 ——
// 请求成功、仓位没了，而错误信息一个字都不会说。
// 所以本地必须拦住：它防的不是参数笔误，是「一改就平」这种静默事故。
func TestBracketSideInvalid(t *testing.T) {
	cases := []struct {
		name    string
		posSide string
		price   string
		tp      string
		sl      string
		wantBad bool
	}{
		// ---- 多单：sl < 现价 < tp ----
		{"多单正常", "long", "100", "110", "95", false},
		// 止损恰好等于现价 → 会立刻触发，按无效处理（fail-closed：
		// 「搬到成本价」在浮盈时自然成立，正好等于现价时应当显式改成市价平仓）。
		{"多单止损恰好等于现价", "long", "100", "", "100", true},
		{"多单止损高于现价", "long", "100", "110", "105", true},
		{"多单止盈低于现价", "long", "100", "95", "90", true},
		{"多单只改止盈", "long", "100", "120", "", false},

		// ---- 空单：tp < 现价 < sl ----
		{"空单正常", "short", "100", "90", "105", false},
		{"空单止损低于现价", "short", "100", "90", "95", true},
		{"空单止盈高于现价", "short", "100", "105", "110", true},
		{"空单只改止损", "short", "100", "", "108", false},

		// ---- 边界 ----
		{"价格缺失不做方向校验", "long", "", "110", "95", false},
		{"价格非法不做方向校验", "long", "abc", "110", "95", false},
		{"价格为零不校验", "long", "0", "110", "95", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := bracketSideInvalid(c.posSide, c.price, c.tp, c.sl)
			if c.wantBad && got == "" {
				t.Errorf("%s: 应判为无效（%s px=%s tp=%s sl=%s）", c.name, c.posSide, c.price, c.tp, c.sl)
			}
			if !c.wantBad && got != "" {
				t.Errorf("%s: 不该判无效，得到 %q", c.name, got)
			}
		})
	}
}
