package exec

import (
	"math/big"
	"sort"
	"strings"

	"github.com/Autumn-27/artex/db"
)

// PnL 是某个标的的盈亏汇总。
//
// # 为什么用带符号净持仓 + 加权平均成本
//
// 逐笔「卖出价 - 买入价」在三种情况下会算错：
//   - 分批建仓后一次平（先进先出 vs 加权平均，结果不同）
//   - 部分平仓后又加仓
//   - 反手（多 → 空 一次成交完成）
//
// 加权平均成本法对这三种都给出同一个可解释的答案，且与 paper 侧
// （db/paper_positions.go）口径一致 —— 两处口径不同会让「模拟盘赚了、
// 实盘亏了」这种对比失去意义。
type PnL struct {
	InstID string `json:"inst_id"`
	// PosSide 是这条盈亏属于哪个方向（long / short）。
	//
	// # 为什么需要它（2026-11-14）
	//
	// long_short 模式下同一个 instId 可以同时有多、空两条独立仓。
	// 只按 instId 分组会把两者的成交混成一条净持仓 —— 一边大赚、
	// 一边大亏时，报表显示「没赚没亏」，而两边都已平仓落袋。
	//
	// net 模式（或纸面账本）下留空：那里的 pos_side 是 net / 空，
	// 本来就该合并成一条。
	PosSide string `json:"pos_side,omitempty"`
	// Realized 已实现盈亏（不含手续费，USDT 计价）。
	Realized string `json:"realized"`
	// FeeTotal 手续费合计（**负数**表示支出）。
	FeeTotal string `json:"fee_total"`
	// NetRealized = Realized + FeeTotal，这才是「平仓后到手多少」。
	NetRealized string `json:"net_realized"`
	// OpenQty 期末净持仓（带符号，正=多，负=空）。
	OpenQty string `json:"open_qty"`
	// OpenAvgCost 期末持仓的加权平均成本（无持仓时为空）。
	OpenAvgCost string `json:"open_avg_cost,omitempty"`
	// BuyAmount / SellAmount 是成交额合计，供核对（手算验证用）。
	BuyAmount  string `json:"buy_amount"`
	SellAmount string `json:"sell_amount"`
	// FillCount 参与计算的成交笔数。
	FillCount int `json:"fill_count"`
}

// ComputePnLByInst 按标的计算盈亏。
//
// 输入顺序无关（内部按 filled_at + id 排序）—— 交易所返回顺序不保证，
// 而顺序错了会让「先平后开」被算成「先开后平」，盈亏符号直接反。
func ComputePnLByInst(fills []db.TradeFill) []PnL {
	type key struct{ inst, side string }
	groups := map[key][]db.TradeFill{}
	order := make([]key, 0, 8)
	for _, f := range fills {
		k := key{f.InstID, groupSideOf(f.PosSide)}
		if _, seen := groups[k]; !seen {
			order = append(order, k)
		}
		groups[k] = append(groups[k], f)
	}
	out := make([]PnL, 0, len(groups))
	for _, k := range order {
		out = append(out, computeOne(k.inst, k.side, groups[k]))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].InstID != out[j].InstID {
			return out[i].InstID < out[j].InstID
		}
		return out[i].PosSide < out[j].PosSide
	})
	return out
}

// groupSideOf 决定一笔成交归到哪个方向组。
//
// long / short 原样保留（long_short 模式：两条仓各自算账）；
// net / 空 / 其它一律合并成 ""（net 模式与纸面账本只有一条净持仓）。
//
// 判据刻意是「白名单」而不是「排除 net」：交易所或历史数据里出现
// 未预期的 pos_side 值时，合并比分裂安全 —— 分裂会产生两条都算不全的
// 盈亏，合并至少总数是对的。
func groupSideOf(posSide string) string {
	switch strings.ToLower(strings.TrimSpace(posSide)) {
	case "long":
		return "long"
	case "short":
		return "short"
	}
	return ""
}

func computeOne(inst, groupSide string, fills []db.TradeFill) PnL {
	sorted := make([]db.TradeFill, len(fills))
	copy(sorted, fills)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].FilledAt != sorted[j].FilledAt {
			return sorted[i].FilledAt < sorted[j].FilledAt
		}
		return sorted[i].ID < sorted[j].ID
	})

	qty := new(big.Rat)      // 带符号净持仓
	cost := new(big.Rat)     // 带符号成本总额（同号于 qty）
	realized := new(big.Rat) // 已实现盈亏
	feeTotal := new(big.Rat)
	buyAmt := new(big.Rat)
	sellAmt := new(big.Rat)
	used := 0

	for _, f := range sorted {
		px, okPx := new(big.Rat).SetString(strings.TrimSpace(f.FillPx))
		sz, okSz := new(big.Rat).SetString(strings.TrimSpace(f.FillSz))
		if !okPx || !okSz {
			continue // 脏数据跳过，不参与计算（宁可少算也不要把符号搞反）
		}
		if sz.Sign() <= 0 {
			continue
		}
		used++
		if fee, ok := new(big.Rat).SetString(strings.TrimSpace(defaultZero(f.Fee))); ok {
			feeTotal.Add(feeTotal, fee)
		}

		side := strings.ToLower(strings.TrimSpace(f.Side))
		signed := new(big.Rat).Set(sz)
		notional := new(big.Rat).Mul(px, sz)
		if side == "sell" {
			signed.Neg(signed)
			sellAmt.Add(sellAmt, notional)
		} else {
			buyAmt.Add(buyAmt, notional)
		}

		// 同号（或空仓）→ 纯开仓/加仓；异号 → 先平后（可能）反手。
		if qty.Sign() == 0 || (qty.Sign() > 0) == (signed.Sign() > 0) {
			qty.Add(qty, signed)
			cost.Add(cost, new(big.Rat).Mul(signed, px))
			continue
		}

		// 平仓部分：数量取「本次成交量」与「现有持仓量」的较小者。
		closeSz := new(big.Rat).Abs(new(big.Rat).Set(qty))
		if signedAbs := new(big.Rat).Abs(new(big.Rat).Set(signed)); signedAbs.Cmp(closeSz) < 0 {
			closeSz = signedAbs
		}
		avgCost := new(big.Rat).Quo(cost, qty) // 带符号 → 成本均价为正
		if qty.Sign() > 0 {
			// 平多：赚 (卖出价 - 成本价)
			realized.Add(realized, new(big.Rat).Mul(closeSz, new(big.Rat).Sub(px, avgCost)))
			qty.Sub(qty, closeSz)
			cost.Sub(cost, new(big.Rat).Mul(closeSz, avgCost))
		} else {
			// 平空：赚 (成本价 - 买入价)
			realized.Add(realized, new(big.Rat).Mul(closeSz, new(big.Rat).Sub(avgCost, px)))
			qty.Add(qty, closeSz)
			cost.Add(cost, new(big.Rat).Mul(closeSz, avgCost))
		}
		if qty.Sign() == 0 {
			cost.SetInt64(0) // 归零后清成本，避免浮点/舍入残渣累积
		}

		// 剩余量 → 反手开仓
		if remain := new(big.Rat).Sub(new(big.Rat).Abs(new(big.Rat).Set(signed)), closeSz); remain.Sign() > 0 {
			reverse := new(big.Rat).Mul(signed, new(big.Rat).Quo(remain, new(big.Rat).Abs(new(big.Rat).Set(signed))))
			qty.Add(qty, reverse)
			cost.Add(cost, new(big.Rat).Mul(reverse, px))
		}
	}

	p := PnL{
		InstID:      inst,
		PosSide:     groupSide,
		Realized:    decimalOrZero(realized),
		FeeTotal:    decimalOrZero(feeTotal),
		NetRealized: decimalOrZero(new(big.Rat).Add(realized, feeTotal)),
		OpenQty:     decimalOrZero(qty),
		BuyAmount:   decimalOrZero(buyAmt),
		SellAmount:  decimalOrZero(sellAmt),
		FillCount:   used,
	}
	if qty.Sign() != 0 {
		p.OpenAvgCost = decimalOrZero(new(big.Rat).Quo(cost, qty))
	}
	return p
}

func defaultZero(s string) string {
	if strings.TrimSpace(s) == "" {
		return "0"
	}
	return s
}

// decimalOrZero 输出 10 位小数的十进制串（与金额列的精度约定一致）。
func decimalOrZero(r *big.Rat) string {
	if r == nil {
		return "0"
	}
	return ratToDecimal(r)
}

// DecimalAddStr 是两个十进制字符串相加（精确，不经过 float）。
//
// 导出版本供 server 层做合计：那里拿到的是已格式化好的字符串，
// 再用 float 累加会把 3.33+3.33+3.33 变成 9.989999999999998。
func DecimalAddStr(a, b string) string {
	x, ok1 := new(big.Rat).SetString(defaultZero(a))
	y, ok2 := new(big.Rat).SetString(defaultZero(b))
	if !ok1 {
		x = new(big.Rat)
	}
	if !ok2 {
		y = new(big.Rat)
	}
	return ratToDecimal(new(big.Rat).Add(x, y))
}
