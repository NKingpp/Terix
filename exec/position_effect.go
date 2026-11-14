package exec

// 开仓动作的真实语义判定。
//
// # 为什么这件事值得单独一个文件
//
// 「open_position」这个名字会骗人：在 net 模式下，对已有空仓的标的
// 下 buy 单，交易所做的是**减仓**，不是开多。
//
// 实测 2026-11-14 任务 30 就栽在这里：模型以为自己在「开多 0.1」，
// 实际是把 578 的空仓减到 577.9；而工具按「新开多单」算 TP/SL
// （多单 sl < 现价 < tp），挂上去的止损在现价下方 —— 对空仓永远不会
// 触发，止盈却在价格一涨时把盈利的空单平掉。全程无错误码。
//
// 所以下单前必须先回答：这一单到底在干什么。答案只有四种：
//
//	open      无仓，纯开新仓
//	scale_in  同向有仓，加仓（均价被摊平）
//	reduce    反向有仓且数量 ≤ 持仓，实际是减仓
//	reverse   反向有仓且数量 > 持仓，先平后反手
//
// # long_short 模式的关键差异
//
// 双向持仓模式下 long 与 short 是**两条独立仓**，可以同时存在。
// 所以「有反向持仓时开仓」在那里是真开新仓（open），不是减仓。
// 这一条判错，双向模式下每次反向开仓都会：
//   - 不挂保护（因为判成 reduce 就不动 OCO）
//   - 回报「平仓成功」而实际开了一个裸的新仓
//
// 这是本文件最容易错、也最必须逐条固化的地方。

import (
	"fmt"
	"math/big"
	"strings"
)

// EntryPlan 是一次「开仓方向的下单」的真实效果。
type EntryPlan struct {
	// Effect 是 open | scale_in | reduce | reverse。
	Effect string
	// PreNet 是动作前该标的的**带符号净量**（正 = 净多，负 = 净空）。
	// long_short 模式下两条仓并存时按净额算。
	PreNet string
	// PostNet 是动作后**预计**的带符号净量。
	PostNet string
	// HeldSame / HeldOpp 是动作前同向 / 反向的绝对数量。
	HeldSame string
	HeldOpp  string
}

// Effect 取值常量（写入台账 position_effect 列）。
const (
	EffectOpen    = "open"
	EffectScaleIn = "scale_in"
	EffectReduce  = "reduce"
	EffectReverse = "reverse"
)

// ClassifyEntry 判定一次「按 side 方向下单 qty」对现有持仓的真实效果。
//
// heldSame / heldOpp 传入向 / 反向的**绝对**持仓量（空串或 "0" 表示无仓）。
// 调用方从 SideQtyOf 拿到方向与绝对量，按 side 分到这两栏。
func ClassifyEntry(posMode, side, qty, heldSame, heldOpp string) (EntryPlan, error) {
	s := strings.ToLower(strings.TrimSpace(side))
	if s != "buy" && s != "sell" {
		return EntryPlan{}, fmt.Errorf("side 必须是 buy 或 sell，收到 %q", side)
	}
	q, ok := new(big.Rat).SetString(strings.TrimSpace(qty))
	if !ok || q.Sign() <= 0 {
		return EntryPlan{}, fmt.Errorf("qty 必须是正的十进制数，收到 %q", qty)
	}
	same := ratOrZero(heldSame)
	opp := ratOrZero(heldOpp)

	newSide := "long"
	if s == "sell" {
		newSide = "short"
	}
	// 带符号的净额：同向为正（按新仓方向），反向为负。
	signedSame := new(big.Rat).Set(same)
	signedOpp := new(big.Rat).Neg(opp)
	preNet := new(big.Rat).Add(signedSame, signedOpp)

	plan := EntryPlan{
		HeldSame: ratString(same),
		HeldOpp:  ratString(opp),
		PreNet:   signedNetString(newSide, preNet),
	}

	// ① 同向已有仓 → 加仓。两种模式一致。
	if same.Sign() > 0 {
		plan.Effect = EffectScaleIn
		plan.PostNet = signedNetString(newSide, new(big.Rat).Add(same, q))
		return plan, nil
	}

	// ② 反向有仓 → 分模式处理。
	if opp.Sign() > 0 {
		if posMode == "long_short" {
			// 双向模式：反方向是另一条独立仓，这就是纯开新仓。
			//
			// 这一分支是整个判定的分水岭 —— 漏了它，双向模式下反向开仓
			// 会被判成 reduce：工具不挂保护、回报「平仓」，而实际开了裸仓。
			plan.Effect = EffectOpen
			plan.PostNet = signedNetString(newSide, q)
			return plan, nil
		}
		// net 模式：反向单先在交易所内部抵消现有仓。
		if q.Cmp(opp) <= 0 {
			plan.Effect = EffectReduce
			plan.PostNet = signedNetString(newSide, new(big.Rat).Sub(q, opp))
			return plan, nil
		}
		plan.Effect = EffectReverse
		// 反手后净量 = qty - heldOpp，方向与新仓方向一致。
		plan.PostNet = signedNetString(newSide, new(big.Rat).Sub(q, opp))
		return plan, nil
	}

	// ③ 无仓 → 纯开仓。
	plan.Effect = EffectOpen
	plan.PostNet = signedNetString(newSide, q)
	return plan, nil
}

// signedNetString 把「以 newSide 为正方向」的净额渲染成带符号字符串。
//
// 约定：净额为正表示持有 newSide 方向，为负表示反向。
// 直接返回带符号的十进制数 —— 调用方需要知道符号时不做二次推断。
func signedNetString(newSide string, net *big.Rat) string {
	if net == nil || net.Sign() == 0 {
		return "0"
	}
	// 统一成「正数 = net 多」的全局口径，与 Position.Pos 的符号一致
	// （net 模式下负数就是空头）。这样回执里 pre_position 的符号
	// 与 get_positions 的 pos 可以直接对比。
	abs := new(big.Rat).Abs(net)
	s := ratString(abs)
	netLong := net.Sign() > 0
	if newSide == "short" {
		netLong = !netLong
	}
	if netLong {
		return s
	}
	return "-" + s
}

// ratOrZero 解析十进制字面量，失败或空返回 0（持仓量缺失等价于无仓）。
func ratOrZero(s string) *big.Rat {
	v := strings.TrimSpace(s)
	if v == "" {
		return new(big.Rat)
	}
	r, ok := new(big.Rat).SetString(v)
	if !ok {
		return new(big.Rat)
	}
	return new(big.Rat).Abs(r)
}
