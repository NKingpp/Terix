package exec

import "testing"

// 持仓归一化：net 与 long_short 的唯一分歧点。
//
// # 为什么这些断言值得写
//
// 同一段「net 下方向看符号」的判断原来散在三个文件里。三份实现
// 只要有一份写错，症状是「close_position 能平、modify_bracket 报没有该持仓」——
// 而且只在 long_short 模式出现，纸面测试永远看不见。
func TestSideQtyOf(t *testing.T) {
	cases := []struct {
		name    string
		posMode string
		posSide string
		pos     string
		wantSi  string
		wantQty string
		wantOK  bool
	}{
		{"net 正数=多", "net", "net", "1.5", "long", "1.5", true},
		{"net 负数=空", "net", "net", "-0.75", "short", "0.75", true},
		{"net posSide 缺失也认符号", "", "", "-2", "short", "2", true},
		{"net 零仓不算持仓", "net", "net", "0", "", "", false},
		{"net 0.0 也不算", "net", "net", "0.0", "", "", false},
		{"net 空串", "net", "net", "  ", "", "", false},
		{"long_short 用 posSide", "long_short", "short", "3", "short", "3", true},
		{"long_short long", "long_short", "long", "3", "long", "3", true},
		// 双向模式下 posSide 非法 → 不猜方向。猜了会让后续撤错 OCO。
		{"long_short posSide=net 视为不一致", "long_short", "net", "3", "", "", false},
		{"long_short posSide 空", "long_short", "", "3", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			si, qty, ok := SideQtyOf(c.posMode, c.posSide, c.pos)
			if ok != c.wantOK || si != c.wantSi || qty != c.wantQty {
				t.Errorf("SideQtyOf(%q,%q,%q) = (%q,%q,%v), want (%q,%q,%v)",
					c.posMode, c.posSide, c.pos, si, qty, ok, c.wantSi, c.wantQty, c.wantOK)
			}
		})
	}
}

// 可平量优先用 availPos —— long_short 下 pos 恒正，只有 availPos 反映挂单占用。
func TestAvailOf(t *testing.T) {
	cases := []struct{ pos, avail, want string }{
		{"1", "0.8", "0.8"},
		{"-1", "0.8", "0.8"},
		{"1", "", "1"},
		{"-1.5", "0", "1.5"},    // availPos 非正 → 回退
		{"2", "abc", "2"},       // 非法 → 回退
		{"", "", ""},            // 都没有 → 空
		{"-0.25", "  ", "0.25"}, // 空白 → 回退
	}
	for _, c := range cases {
		if got := AvailOf(c.pos, c.avail); got != c.want {
			t.Errorf("AvailOf(%q,%q) = %q, want %q", c.pos, c.avail, got, c.want)
		}
	}
}

// net 下算法单不区分方向；long_short 下必须精确匹配，否则会撤掉反向仓的保护。
func TestAlgoSideMatches(t *testing.T) {
	// net：任何算法单都属于该标的的净持仓
	if !AlgoSideMatches("net", "net", "long") {
		t.Error("net 模式 algoPosSide=net 应匹配 long")
	}
	if !AlgoSideMatches("net", "", "short") {
		t.Error("net 模式 algoPosSide 空应匹配 short")
	}
	// long_short：精确匹配
	if !AlgoSideMatches("long_short", "long", "long") {
		t.Error("long_short 应匹配同向")
	}
	if AlgoSideMatches("long_short", "short", "long") {
		t.Error("long_short 绝不能匹配反向 —— 撤掉会让另一条仓裸奔")
	}
	if AlgoSideMatches("long_short", "net", "long") {
		t.Error("long_short 下 algoPosSide=net 是不一致数据，应判不匹配")
	}
	// 非法 holdSide 一律不匹配（防止调用方传空串时撤光所有单）
	if AlgoSideMatches("net", "net", "") {
		t.Error("holdSide 为空应判不匹配")
	}
}
