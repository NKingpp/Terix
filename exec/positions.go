package exec

// 持仓归一化：把交易所的一条持仓（或一条算法单）翻译成「方向 + 绝对数量」。
//
// # 为什么必须集中在一处
//
// 同一段「net 模式下 posSide 是 "net"，方向看 pos 符号」的判断，
// 原来在三个地方各写了一遍：
//
//	server/trade_exec_tools.go  开仓护栏
//	server/trade_exec_tools.go  平仓定方向
//	server/trade_tools.go       modify_bracket 找保护单
//
// 三份实现里只要有一份写错，就会出现「close_position 能平、
// modify_bracket 报没有该持仓」这种自相矛盾 —— 而且只在 long_short
// 模式下出现，纸面测试永远看不见（纸面一律按 net 处理）。
//
// 所以这里把语义固化：net 与 long_short 的差异只在这一个文件里出现。

import (
	"math/big"
	"strings"
)

// SideQtyOf 把一条交易所持仓归一化成「方向 + 绝对数量」。
//
//	net 模式：posSide 恒 "net"，方向由 pos 的符号决定（负 = 空头）
//	long_short 模式：方向就是 posSide，pos 恒为正
//
// ok=false 表示这条记录无法判定方向（posSide 非法且 pos 也不像数）。
// 调用方必须跳过 ok=false 的行 —— 猜方向比跳过危险得多。
func SideQtyOf(posMode, posSide, pos string) (side, qty string, ok bool) {
	q := strings.TrimSpace(pos)
	if q == "" {
		return "", "", false
	}
	// 用 big.Rat 判零与符号：字符串比较会被 "0.0" / "-0" / "1e-8" 骗。
	r, parsed := new(big.Rat).SetString(q)
	if !parsed {
		return "", "", false
	}
	if r.Sign() == 0 {
		return "", "", false
	}

	abs := new(big.Rat).Abs(r)
	qty = ratString(abs)

	if posMode == "long_short" {
		s := strings.ToLower(strings.TrimSpace(posSide))
		switch s {
		case "long", "short":
			return s, qty, true
		}
		// long_short 模式下 posSide 非法就是不可信数据，不按符号猜：
		// 交易所若返回 net 而账户是双向模式，说明配置与账户不一致，
		// 猜方向只会掩盖问题。
		return "", "", false
	}

	// net 模式（含 posMode 为空的历史账户）
	if r.Sign() > 0 {
		return "long", qty, true
	}
	return "short", qty, true
}

// AvailOf 返回可平量。
//
// long_short 模式下 pos 恒为正（是该方向的总持仓），只有 availPos
// 反映了「扣掉挂单占用后真正能平的量」。net 模式下两者通常相等，
// 但挂单占用一样会体现在 availPos 上。
//
// availPos 为空或非正时回退 |pos| —— 老数据/纸面可能不带这个字段。
func AvailOf(pos, availPos string) string {
	if v := strings.TrimSpace(availPos); v != "" {
		if r, ok := new(big.Rat).SetString(v); ok && r.Sign() > 0 {
			return ratString(r)
		}
	}
	p := strings.TrimSpace(pos)
	if p == "" {
		return ""
	}
	r, ok := new(big.Rat).SetString(p)
	if !ok {
		return ""
	}
	return ratString(new(big.Rat).Abs(r))
}

// AlgoSideMatches 判断一条算法单（OCO）是否属于 holdSide 这个仓。
//
// # 为什么不能直接用字符串比较
//
// net 模式下所有算法单的 posSide 都是 "net"（或空），它保护的是
// 该标的唯一的那条净持仓 —— 必须匹配任何 holdSide。
// long_short 模式下同一个 instId 可能有 long 和 short 两条仓，
// 各挂各的 OCO，撤单/改价时必须精确匹配，**绝不能顺手撤掉反向的**
// （那会让另一个仓裸奔，而且用户看不出原因）。
func AlgoSideMatches(posMode, algoPosSide, holdSide string) bool {
	hold := strings.ToLower(strings.TrimSpace(holdSide))
	if hold != "long" && hold != "short" {
		return false
	}
	if posMode != "long_short" {
		// net 模式：算法单不区分方向，任何一条都属于这个标的的净持仓。
		return true
	}
	a := strings.ToLower(strings.TrimSpace(algoPosSide))
	if a == "" || a == "net" {
		// 双向模式下返回 net 的算法单是不一致数据。
		// 这里保守地判为「不匹配」：宁可一条保护单找不到（会报出来），
		// 也不要错撤掉。
		return false
	}
	return a == hold
}

// ratString 把 big.Rat 渲染成不带多余尾零的十进制字符串。
//
// 用 Rat.FloatString 会引入固定小数位（丢精度或补零），而 OKX 对
// sz/px 的字符串原样接受，尾零无害但会让日志与台账难读。
// 这里保持「整数时不带小数点」的简洁形式。
func ratString(r *big.Rat) string {
	if r == nil {
		return ""
	}
	if r.IsInt() {
		return r.Num().String()
	}
	// 用足够位数保证 round-trip：Rat 的分母可能含 2/5 以外的因子，
	// 那种情况 FloatString 必然截断，但交易所数量/价格都是有限小数，
	// 所以这里取 16 位（OKX 最细的 tick 是 1e-8 级别）足够。
	s := r.FloatString(16)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	if s == "" || s == "-" {
		return "0"
	}
	return s
}
