package db

// 十进制精确算术（金额与持仓的唯一实现）。
//
// # 为什么不能用 float64
//
// 切到 SQLite 后 numeric 列是 TEXT，SQL 里做算术只能 CAST(x AS REAL)，
// 也就是 float64。实测（见 decimal_test.go 的手算基准）：
//
//	10 次累加 0.1            → 0.99999999999999988898   (≠ 1.0)
//	0.1*0.1 + 0.2*0.2        → 0.05000000000000000278   (精确值 0.05)
//
// 这两个偏差在「成本基数 vs 持仓市值」的比较里会真的判错方向 ——
// 金融产品里算错钱是事故，不是精度瑕疵。所以凡是要累进/乘除的金额，
// 一律回到 Go 层用 big.Rat。
//
// # 为什么是 big.Rat 而不是定点整数
//
// big.Rat 是精确有理数：加减乘除全无误差，且直接接受 "1.5" 这种字面量。
// 定点整数（×1e10 存 INTEGER）要额外记住每列的 scale，而 schema 里同时
// 存在 28,10 / 20,8 / 12,6 / 5,4 四种精度 —— 取最小公倍数等于把
// 「这一列是什么精度」的知识复制到每一个运算点，改一处漏三处。
//
// 精度只在**输出**时收敛：按目标列精度四舍五入，规则与 PG 的 numeric 一致
// （四舍五入、远离零）。中间量全程精确。
//
// # 与 PG 唯一的语义差异（且是改善）
//
// PG 的 `numeric / numeric` 结果自己有 scale（至少 16 位十进制），所以
// `prev_buy_amt / prev_qty` 在 PG 里是被截断过的中间量。这里用精确有理数，
// 只在最后落库时舍入 —— 结果只会更准，不会更偏。

import (
	"fmt"
	"math/big"
	"strings"
)

const (
	// decimalScaleAmount 对应 numeric(28,10)：金额、数量、成本价、已实现盈亏。
	decimalScaleAmount = 10
	// decimalScaleNotional 对应 numeric(20,8)：成交额合计。
	decimalScaleNotional = 8
)

// ParseDecimal 把十进制字面量解析成精确有理数。
//
// 只接受「数字 + 小数点 + 负号」这种形态（与 validDecimalLiteral 同一套
// 语法），刻意**不接受**分数 "1/2"、指数 "1e3"、十六进制 —— big.Rat.SetString
// 都支持，但金额列里出现那些形态说明上游有 bug，应该在边界炸掉而不是猜。
func ParseDecimal(s string) (*big.Rat, error) {
	s = strings.TrimSpace(s)
	if !validDecimalLiteral(s) {
		return nil, fmt.Errorf("非法十进制字面量 %q", s)
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, fmt.Errorf("非法十进制字面量 %q", s)
	}
	return r, nil
}

// DecimalString 把有理数按固定小数位输出，四舍五入（远离零）。
//
// scale 是输出精度：金额用 decimalScaleAmount，成交额用 decimalScaleNotional。
// nil 视为 0，返回对应精度的零串（"0.0000000000"），便于调用方直接比较。
func DecimalString(r *big.Rat, scale int) string {
	if scale < 0 {
		scale = 0
	}
	if r == nil {
		r = new(big.Rat)
	}
	q := decimalRound(r, scale)
	neg := q.Sign() < 0
	digits := new(big.Int).Abs(q).String()
	if scale > 0 {
		if len(digits) <= scale {
			digits = strings.Repeat("0", scale-len(digits)+1) + digits
		}
		digits = digits[:len(digits)-scale] + "." + digits[len(digits)-scale:]
	}
	if neg {
		return "-" + digits
	}
	return digits
}

// decimalRound 返回 r×10^scale 四舍五入后的整数（远离零）。
func decimalRound(r *big.Rat, scale int) *big.Int {
	pow := decimalPow10(scale)
	scaled := new(big.Rat).Mul(r, new(big.Rat).SetInt(pow))
	num, den := scaled.Num(), scaled.Denom()
	q, rem := new(big.Int).QuoRem(num, den, new(big.Int))
	// 半个单位以上就进位；den 恒为正（big.Rat 的不变量），符号由 num 承担。
	twice := new(big.Int).Lsh(new(big.Int).Abs(rem), 1)
	if twice.Cmp(den) >= 0 {
		if num.Sign() < 0 {
			q.Sub(q, big.NewInt(1))
		} else {
			q.Add(q, big.NewInt(1))
		}
	}
	return q
}

// decimalPow10 返回 10^scale。
func decimalPow10(scale int) *big.Int {
	if scale <= 0 {
		return big.NewInt(1)
	}
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil)
}

// DecimalAdd 返回 a+b（nil 视为 0），结果总是新的有理数，不复用入参。
func DecimalAdd(a, b *big.Rat) *big.Rat {
	return new(big.Rat).Add(decimalZero(a), decimalZero(b))
}

// DecimalSub 返回 a-b（nil 视为 0）。
func DecimalSub(a, b *big.Rat) *big.Rat {
	return new(big.Rat).Sub(decimalZero(a), decimalZero(b))
}

// DecimalMul 返回 a×b（nil 视为 0）。
func DecimalMul(a, b *big.Rat) *big.Rat {
	return new(big.Rat).Mul(decimalZero(a), decimalZero(b))
}

// DecimalQuo 返回 a÷b。b 为 0 时返回 nil,false ——
// 调用方必须显式处理「除零」，静默得到 0 会让成本价看起来像免费。
func DecimalQuo(a, b *big.Rat) (*big.Rat, bool) {
	b = decimalZero(b)
	if b.Sign() == 0 {
		return nil, false
	}
	return new(big.Rat).Quo(decimalZero(a), b), true
}

// DecimalSign 返回 -1 / 0 / 1。nil 视为 0。
func DecimalSign(r *big.Rat) int { return decimalZero(r).Sign() }

// decimalZero 把 nil 归一成 0，避免每个调用点都判空。
func decimalZero(r *big.Rat) *big.Rat {
	if r == nil {
		return new(big.Rat)
	}
	return r
}

// NormalizeDecimal 把十进制字面量按列精度规范化（四舍五入，与 PG numeric 一致）。
//
// # 为什么写路径要规范化
//
// numeric(28,10) 列在 PG 里会把 "123.45678901" 存成 "123.4567890100" ——
// 读出时标签化、位数固定。改成 TEXT 后如果不规范化，同一个金额可能以
// "1.5" / "1.50" / "1.5000000000" 三种形态出现在库里：
// 字典序比较会错（虽然数值比较用 big.Rat 不受影响），前端也会显示得不一致。
//
// # 空串与 nil 的语义
//
// 空串返回空串（调用方按「未设置」处理，写 NULL）；非法字面量原样返回 ——
// 校验是 validDecimalLiteral 的职责，这里不重复判断也不吞错误。
func NormalizeDecimal(s string, scale int) string {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return ""
	}
	r, err := ParseDecimal(trimmed)
	if err != nil {
		return s
	}
	return DecimalString(r, scale)
}

// normalizeDecimalInput 是写路径的统一入口：按列精度规范化十进制字面量。
//
// 空串保持空串（调用方按 NULL 处理），非法字面量原样返回（校验在别处）。
func normalizeDecimalInput(s string, scale int) string {
	return NormalizeDecimal(s, scale)
}

// normalizeOptDecimal 是「可空十进制」的写路径入口：空串折成 NULL。
func normalizeOptDecimal(s string, scale int) any {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return NormalizeDecimal(s, scale)
}

// nullableNormalizedDecimal 把 *string 形态的十进制按列精度规范化，nil 保持 nil。
//
// 用于「这次应答没带这个值」的 COALESCE 参数：nil 让 SQL 保留旧值。
func nullableNormalizedDecimal(p *string, scale int) any {
	if p == nil {
		return nil
	}
	return NormalizeDecimal(*p, scale)
}

// decimalScaleConfidence 对应 numeric(5,4)：评级/信号的置信度。
const decimalScaleConfidence = 4

// optNormalizedDecimal 是 optDecimal 的别名，供 signals/ratings 的调用点使用
// （那两处的字段名是 Confidence，读起来更顺）。
func optNormalizedDecimal(s string, scale int) any { return normalizeOptDecimal(s, scale) }
