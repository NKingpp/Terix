package exec

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/Autumn-27/artex/exec/okxrest"
)

// 代码级的止盈止损合理性护栏。
//
// # 为什么护栏在这里而不在 prompt 里
//
// 止损是唯一的物理防线。让 LLM 自己决定「止损该多远」，它给出的值可能
// 远到等于没有止损（例如 ref=200、sl=20）。prompt 会被忽略，代码不会。
//
// 这两个阈值故意硬编码、不可配置：能配置的安全边界等于没有边界。
const (
	maxStopLossPct = 0.20 // 止损距离不得超过参考价的 20%
	minRewardRisk  = 1.2  // 止盈距离至少是止损距离的 1.2 倍（R:R ≥ 1.2）
)

// BracketInput 是下单前的完整校验输入。
type BracketInput struct {
	Side     string // buy | sell
	Action   string // entry | exit | scale_in | scale_out | hold | ""
	Qty      string
	RefPrice string
	TpPrice  string
	SlPrice  string
	// PosSide / PosQty 只在平仓（reduce）场景需要：当前持仓的方向与绝对数量。
	PosSide string // long | short
	PosQty  string
}

// ValidateBracket 校验一张草案/一笔下单是否可执行。
//
// 返回的错误是给 LLM 看的：它必须能据此改参数重试，而不是看到「参数错」。
func ValidateBracket(in BracketInput) error {
	side := strings.ToLower(strings.TrimSpace(in.Side))
	if side != "buy" && side != "sell" {
		return fmt.Errorf("side 必须是 buy 或 sell，收到 %q", in.Side)
	}
	qty, ok := parseDecimal(in.Qty)
	if !ok || qty.Sign() <= 0 {
		return fmt.Errorf("qty 必须是正数，收到 %q", in.Qty)
	}

	action := strings.ToLower(strings.TrimSpace(in.Action))
	reduce := action == "exit" || action == "scale_out"

	if reduce {
		posSide := strings.ToLower(strings.TrimSpace(in.PosSide))
		if posSide != "long" && posSide != "short" {
			return fmt.Errorf("平仓需要知道被平的方向（PosSide 为 long 或 short），收到 %q", in.PosSide)
		}
		// 平多要卖，平空要买。
		want := "sell"
		if posSide == "short" {
			want = "buy"
		}
		if side != want {
			return fmt.Errorf("平 %s 仓只能 %s，收到 %s", posSide, want, side)
		}
		posQty, ok := parseDecimal(in.PosQty)
		if !ok || posQty.Sign() <= 0 {
			return fmt.Errorf("当前无该标的%s仓（持仓量 %q），不能 %s", posSide, in.PosQty, action)
		}
		if qty.Cmp(new(big.Rat).Abs(posQty)) > 0 {
			return fmt.Errorf("平仓量 %s 超过当前持仓 %s", in.Qty, in.PosQty)
		}
		return nil
	}

	// 开仓/加仓：必须给出可执行的止盈止损。
	ref, ok := parseDecimal(in.RefPrice)
	if !ok || ref.Sign() <= 0 {
		return fmt.Errorf("开仓必须有正的参考价 ref_price，收到 %q", in.RefPrice)
	}
	tp, okTp := parseDecimal(in.TpPrice)
	sl, okSl := parseDecimal(in.SlPrice)
	if !okTp || !okSl || tp.Sign() <= 0 || sl.Sign() <= 0 {
		return fmt.Errorf("开仓必须同时给 tp_price 与 sl_price（多单 sl<ref<tp，空单 tp<ref<sl）")
	}

	long := side == "buy"
	if long {
		if !(sl.Cmp(ref) < 0 && ref.Cmp(tp) < 0) {
			return fmt.Errorf("多单必须 sl_price < ref_price < tp_price（当前 sl=%s ref=%s tp=%s）", in.SlPrice, in.RefPrice, in.TpPrice)
		}
	} else {
		if !(tp.Cmp(ref) < 0 && ref.Cmp(sl) < 0) {
			return fmt.Errorf("空单必须 tp_price < ref_price < sl_price（当前 tp=%s ref=%s sl=%s）", in.TpPrice, in.RefPrice, in.SlPrice)
		}
	}

	// 风险距离与盈利距离都按「不利/有利方向」取正：
	//   多单 risk=ref-sl  reward=tp-ref
	//   空单 risk=sl-ref  reward=ref-tp
	// 前面的不等号校验已经保证两者为正，这里不再取 Abs ——
	// 取 Abs 会掩盖「校验写错但后面仍通过」的情况。
	risk := new(big.Rat).Sub(ref, sl)
	reward := new(big.Rat).Sub(tp, ref)
	if !long {
		risk = new(big.Rat).Sub(sl, ref)
		reward = new(big.Rat).Sub(ref, tp)
	}
	// 止损距离上限：|ref-sl| / ref ≤ 20%
	if pct := new(big.Rat).Quo(risk, ref); pct.Cmp(big.NewRat(maxStopLossPct*100, 100)) > 0 {
		return fmt.Errorf("止损距离过大（%.2f%% > %.0f%%），会等于没有止损",
			ratPct(pct), maxStopLossPct*100)
	}
	if cmp := new(big.Rat).Quo(reward, risk); cmp.Cmp(big.NewRat(minRewardRisk*100, 100)) < 0 {
		return fmt.Errorf("盈亏比过低（%.2f < %.1fR），止盈至少要覆盖 1.2 倍止损距离", ratFloat(cmp), minRewardRisk)
	}
	return nil
}

// InstTypeOf 从 instId 推导 OKX 的 instType。
//
//	"BTC-USDT-SWAP"  → "SWAP"
//	"BTC-USDT"       → "SPOT"
//	"BTC-USDT-250926"→ ""（交割合约，不支持）
//
// # 为什么必须显式区分
//
// 交易循环的整套逻辑（读持仓 → 巡检 TP/SL → 强平）建立在
// GET /api/v5/account/positions 上，而该接口的 instType 只支持
// MARGIN / SWAP / FUTURES / OPTION —— **不含 SPOT**。
// 现货的「持仓」在 OKX 里是余额（account/balance），不是 position，
// 且下单要求 tdMode=cash 且不支持 posSide / reduceOnly。
//
// 所以现货不能靠「tdMode 猜错」失败：那条路径的错误码是 51000
// （参数错），看不出是「本系统不支持现货」，排查成本极高。
func InstTypeOf(instID string) string {
	s := strings.ToUpper(strings.TrimSpace(instID))
	if s == "" {
		return ""
	}
	const swapSuffix = "-SWAP"
	if strings.HasSuffix(s, swapSuffix) {
		return "SWAP"
	}
	parts := strings.Split(s, "-")
	// 形如 BTC-USDT-250926：第三段是 6 位日期 → 交割合约
	if len(parts) >= 3 {
		last := parts[len(parts)-1]
		if len(last) == 6 && isAllDigits(last) {
			return ""
		}
	}
	if len(parts) == 2 {
		return "SPOT"
	}
	return ""
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// derivePosSide 按账户持仓模式推导 posSide。
//
// net_mode → 恒 "net"（方向由 side 表达）；long_short_mode → 买开多、卖开空。
// 填错会被 51000 拒，而错误文案只说「Parameter posSide error」。
// DerivePosSideForAlgo 把「被保护的方向」映射成算法单的 posSide。
//
// 与 derivePosSide 的差异：算法单带的是**被保护仓位**的方向（long/short），
// 不是下单方向（buy/sell）。net 模式下交易所只认 "net"。
func DerivePosSideForAlgo(posMode, holdSide string) string {
	if posMode != "long_short" {
		return "net"
	}
	if holdSide == "short" {
		return "short"
	}
	return "long"
}

// DerivePosSide 是 derivePosSide 的导出版本（server 侧直接下单要用）。
func DerivePosSide(posMode, side string, reduceOnly bool, posSideHint string) string {
	return derivePosSide(posMode, side, reduceOnly, posSideHint)
}

func derivePosSide(posMode, side string, reduceOnly bool, posSideHint string) string {
	if posMode != "long_short" {
		return "net"
	}
	// 平仓才采信 hint（「被平的方向」只有平仓时才有意义）。
	// 开仓时采信 hint 会让「buy + posSide=short」这种自相矛盾的组合
	// 流到交易所，被 51000 拒且文案只有「Parameter posSide error」。
	if reduceOnly {
		if h := strings.ToLower(strings.TrimSpace(posSideHint)); h == "long" || h == "short" {
			return h
		}
		// 平仓方向与被平方向相反：卖平多、买平空。
		if strings.EqualFold(side, "sell") {
			return "long"
		}
		return "short"
	}
	if strings.EqualFold(side, "buy") {
		return "long"
	}
	return "short"
}

// DraftClOrdID 是草案单的幂等键（≤32 位纯字母数字）。
//
// 不能带连字符：OKX 对 clOrdId 只允许字母数字，`artex-xxxx` 会被 51000 拒。
func DraftClOrdID(draftID int64) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("draft:%d", draftID)))
	return "artex" + hex.EncodeToString(h[:8])
}

// CloseClOrdID 是兜底强平的幂等键。
//
// 按**持仓身份**（instId+posSide+来源草案）派生，不按时间 —— 同一持仓被反复
// 尝试强平只产生同一个键，交易所侧的 51603 会兜住重复。
func CloseClOrdID(instID, posSide string, draftID int64) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("close:%s:%s:%d", instID, posSide, draftID)))
	return "artexc" + hex.EncodeToString(h[:8])
}

func parseDecimal(s string) (*big.Rat, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, false
	}
	r, ok := new(big.Rat).SetString(s)
	return r, ok
}

func ratFloat(r *big.Rat) float64 {
	f, _ := r.Float64()
	return f
}

func ratPct(r *big.Rat) float64 {
	return ratFloat(r) * 100
}

// IsDuplicateErr 判断是否是「同一个 clOrdId 已经下过单」。
//
// 工具直接下单时用它做幂等：超时重发可能其实已经成功，
// 此时应当查回原单而不是再下一张。
func IsDuplicateErr(err error) bool { return errors.Is(err, okxrest.ErrDuplicate) }
