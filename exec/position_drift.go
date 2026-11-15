package exec

// 持仓漂移检测：本地成交推算的净持仓 vs 交易所真实持仓。
//
// # 为什么必须显式检查（2026-11-15 事故）
//
// 2026-11-15 发现 trade_pnl 算出 NVDA 净持仓 -68.95，而交易所有 0 仓。
// 表层看像「系统乱下单」，实际是成交明细**漏同步**（分页 bug）：
// 一笔 150 张的卖单拆成 150+ 笔成交，我们只取到前 100 笔。
//
// 两边数字都不报错，报表看起来也正常（只是偏小），所以这个不一致
// 存活了一整天。等它被发现时，已经基于错误的净持仓做了若干决策。
//
// 所以这里把它变成一个**可查询的结论**：每次同步成交后对比一次，
// 不一致就说出来，带上双方数字。让人在它影响决策之前看到。

import (
	"context"
	"fmt"
	"math/big"
	"strings"

	"github.com/Autumn-27/artex/exec/okxrest"
)

// PositionDrift 是一个标的的持仓漂移。
type PositionDrift struct {
	InstID string `json:"inst_id"`
	// LocalNet 是本地成交推算的带符号净持仓（正=多）。
	LocalNet string `json:"local_net"`
	// ExchangeNet 是交易所真实净持仓（带符号）。
	ExchangeNet string `json:"exchange_net"`
	// Diff = LocalNet - ExchangeNet。
	Diff string `json:"diff"`
	// Reason 是给人看的一句话（漂移原因的可能解释）。
	Reason string `json:"reason,omitempty"`
}

// DetectPositionDrift 对比本地与交易所的净持仓，返回不一致的标的。
//
// tolerance 是允许的绝对误差（十进制字符串，如 "0.0001"）。手数很小的
// 品种（如 0.0001 BTC）用固定容差会误报，所以由调用方按标的给。
// 传空表示零容差。
// PositionReader 是对账需要的最小交易所能力。
//
// 刻意只要求一个方法：对账不该顺带拿到下单权限。
// *okxrest.Client 天然满足，server 侧的 okxTrader 也满足。
type PositionReader interface {
	GetPositions(ctx context.Context, instType string) ([]okxrest.Position, error)
}

func DetectPositionDrift(ctx context.Context, c PositionReader, localNet map[string]string, tolerance string) ([]PositionDrift, error) {
	if c == nil {
		return nil, fmt.Errorf("交易客户端未装配")
	}
	rows, err := c.GetPositions(ctx, "SWAP")
	if err != nil {
		return nil, fmt.Errorf("读交易所持仓失败: %v", err)
	}

	// 交易所侧净持仓：net 模式下 pos 带符号；long_short 模式下按方向汇总。
	// 这里不区分模式 —— 我们要的是「净敞口」，两个方向的仓相减即可。
	exNet := map[string]*big.Rat{}
	for _, p := range rows {
		inst := strings.ToUpper(strings.TrimSpace(p.InstID))
		if inst == "" {
			continue
		}
		q, ok := new(big.Rat).SetString(strings.TrimSpace(p.Pos))
		if !ok {
			continue
		}
		// long_short 模式下 pos 恒正，方向由 posSide 决定。
		if strings.EqualFold(strings.TrimSpace(p.PosSide), "short") && q.Sign() > 0 {
			q.Neg(q)
		}
		if cur, exists := exNet[inst]; exists {
			cur.Add(cur, q)
		} else {
			exNet[inst] = q
		}
	}

	// 隐式噪声下限：1e-8。
	//
	// # 为什么不能是零容差（2026-11-15 首次上线就踩到）
	//
	// 交易所返回的是十进制字符串，本地净持仓来自 SQL SUM(REAL)，
	// 「一买一卖正好抵消」能算出 -1.4e-17。零容差下这就是一条
	// 漂移告警，而两边其实完全一致。
	//
	// 假阳性比漏报更糟：它让「漂移」这个信号失去可信度，
	// 下次真的不一致时没人当回事。
	//
	// 1e-8 远低于任何可交易手数（OKX 最细精度就是 1e-8 量级），
	// 所以不会掩盖真实差异。
	tol := new(big.Rat).SetFrac64(1, 100000000)
	if t := strings.TrimSpace(tolerance); t != "" {
		if v, ok := new(big.Rat).SetString(t); ok {
			v.Abs(v)
			if v.Cmp(tol) > 0 {
				tol = v // 调用方给的容差更宽时放宽
			}
		}
	}

	// 两边的标的并集：本地有、交易所无（或反之）都是漂移。
	seen := map[string]struct{}{}
	var insts []string
	for inst := range localNet {
		k := strings.ToUpper(strings.TrimSpace(inst))
		if k == "" {
			continue
		}
		if _, dup := seen[k]; !dup {
			seen[k] = struct{}{}
			insts = append(insts, k)
		}
	}
	for inst := range exNet {
		if _, dup := seen[inst]; !dup {
			seen[inst] = struct{}{}
			insts = append(insts, inst)
		}
	}

	out := make([]PositionDrift, 0, 2)
	for _, inst := range insts {
		local := new(big.Rat)
		if v, ok := new(big.Rat).SetString(strings.TrimSpace(localNet[inst])); ok {
			local.Set(v)
		}
		ex := new(big.Rat)
		if v, exists := exNet[inst]; exists {
			ex.Set(v)
		}
		diff := new(big.Rat).Sub(local, ex)
		if diff.Sign() == 0 {
			continue
		}
		if new(big.Rat).Abs(diff).Cmp(tol) <= 0 {
			continue
		}
		d := PositionDrift{
			InstID:      inst,
			LocalNet:    ratText(local),
			ExchangeNet: ratText(ex),
			Diff:        ratText(diff),
		}
		// 常见成因：成交明细漏同步（分页）、手工用交易所 APP/CLI 操作、
		// 或同步窗口没覆盖到更早的成交。
		if local.Sign() != 0 && ex.Sign() == 0 {
			d.Reason = "本地有净持仓而交易所已平 —— 检查成交明细是否漏同步（大单拆成上百笔成交时分页不足会漏）"
		} else if ex.Sign() != 0 && local.Sign() == 0 {
			d.Reason = "交易所有持仓而本地无记录 —— 可能是手工下单或同步窗口未覆盖"
		}
		out = append(out, d)
	}
	return out, nil
}

// ratText 把 big.Rat 渲染成简洁十进制串（保留足够精度用于对账比对）。
func ratText(r *big.Rat) string {
	if r == nil {
		return "0"
	}
	if r.IsInt() {
		return r.Num().String()
	}
	s := strings.TrimRight(r.FloatString(10), "0")
	s = strings.TrimRight(s, ".")
	// "-0" 与 "0" 数值相同但字符串不同。对账是字符串比对，
	// 留着 "-0" 会产生假阳性（2026-11-15 首次上线就踩到）。
	if s == "" || s == "-" || s == "-0" {
		return "0"
	}
	return s
}
