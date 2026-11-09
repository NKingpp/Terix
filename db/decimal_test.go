package db

// 十进制算术的手算基准测试。
//
// # 为什么基准值必须手算
//
// 用 float64 的结果当基准等于「拿病人自己的体温计校准」—— 本文件要防的
// 正是 float64 的漂移，所以期望值全部写成**精确十进制字面量**，
// 并在注释里写明这套数的解析解（可以拿笔算验证）。
//
// 每条用例都附带同一算式在 float64 下的实际结果（放在注释里），
// 用来说明「不换成精确算术就会错」这件事不是假想。

import (
	"math/big"
	"testing"
)

func mustRat(t *testing.T, s string) *big.Rat {
	t.Helper()
	r, err := ParseDecimal(s)
	if err != nil {
		t.Fatalf("ParseDecimal(%q): %v", s, err)
	}
	return r
}

// TestDecimalAccumulationTenths 验证 10 次累加 0.1 精确等于 1.0。
//
// float64 下同一算式得 0.99999999999999988898（≠ 1.0）。
func TestDecimalAccumulationTenths(t *testing.T) {
	sum := new(big.Rat)
	for i := 0; i < 10; i++ {
		sum = DecimalAdd(sum, mustRat(t, "0.1"))
	}
	if got := DecimalString(sum, decimalScaleAmount); got != "1.0000000000" {
		t.Fatalf("10 × 0.1 = %q, want 1.0000000000", got)
	}
	if got := DecimalString(sum, 0); got != "1" {
		t.Fatalf("10 × 0.1 (scale 0) = %q, want 1", got)
	}
	// 精确相等，不是「差得很小」。
	if sum.Cmp(mustRat(t, "1")) != 0 {
		t.Fatalf("10 × 0.1 != 1 精确比较")
	}
}

// TestDecimalProductsExact 验证 0.1×0.1 + 0.2×0.2 精确等于 0.05。
//
// 解析解：0.01 + 0.04 = 0.05。
// float64 下同一算式得 0.05000000000000000278。
func TestDecimalProductsExact(t *testing.T) {
	a := DecimalAdd(
		DecimalMul(mustRat(t, "0.1"), mustRat(t, "0.1")),
		DecimalMul(mustRat(t, "0.2"), mustRat(t, "0.2")),
	)
	if got := DecimalString(a, decimalScaleAmount); got != "0.0500000000" {
		t.Fatalf("0.1*0.1 + 0.2*0.2 = %q, want 0.0500000000", got)
	}
	if a.Cmp(mustRat(t, "0.05")) != 0 {
		t.Fatalf("精确值 != 1/20")
	}
}

// TestDecimalLargeAccumulationNoDrift 验证 1000 笔不同金额累进后仍精确。
//
// 手算基准：每笔 0.01，1000 笔 → 恰为 10。
// 再加 1000 笔 0.001 → 11。再减 1000 笔 0.0001 → 10.9。
func TestDecimalLargeAccumulationNoDrift(t *testing.T) {
	sum := new(big.Rat)
	penny := mustRat(t, "0.01")
	for i := 0; i < 1000; i++ {
		sum = DecimalAdd(sum, penny)
	}
	if got := DecimalString(sum, decimalScaleAmount); got != "10.0000000000" {
		t.Fatalf("1000 × 0.01 = %q, want 10.0000000000", got)
	}

	milli := mustRat(t, "0.001")
	for i := 0; i < 1000; i++ {
		sum = DecimalAdd(sum, milli)
	}
	if got := DecimalString(sum, decimalScaleAmount); got != "11.0000000000" {
		t.Fatalf("+1000 × 0.001 = %q, want 11.0000000000", got)
	}

	tenthMilli := mustRat(t, "0.0001")
	for i := 0; i < 1000; i++ {
		sum = DecimalSub(sum, tenthMilli)
	}
	if got := DecimalString(sum, decimalScaleAmount); got != "10.9000000000" {
		t.Fatalf("-1000 × 0.0001 = %q, want 10.9000000000", got)
	}
}

// TestDecimalMixedScale 验证 28,10 与 5,4 两种精度混算。
//
// 解析解（手算）：
//
//	数量      = 100.0000000001 + 0.5   = 100.5000000001
//	金额      = 100.0000000001 × 10.5 = 1050.00000000105
//	成本价    = 1050.00000000105 ÷ 100.5000000001
//
// 最后一步用 5,4 精度输出 = 10.4478（10.447761… 四舍五入到 4 位）。
func TestDecimalMixedScale(t *testing.T) {
	qty := mustRat(t, "100.0000000001")
	half := mustRat(t, "0.5")
	totalQty := DecimalAdd(qty, half)
	if got := DecimalString(totalQty, decimalScaleAmount); got != "100.5000000001" {
		t.Fatalf("qty = %q, want 100.5000000001", got)
	}

	price := mustRat(t, "10.5")
	amount := DecimalMul(qty, price)
	if got := DecimalString(amount, decimalScaleAmount); got != "1050.0000000011" {
		// 1050.00000000105 在 10 位小数下四舍五入 → .0000000011（远离零进位）。
		t.Fatalf("amount = %q, want 1050.0000000011", got)
	}
	if got := DecimalString(amount, 5); got != "1050.00000" {
		t.Fatalf("amount (scale 5) = %q, want 1050.00000", got)
	}

	cost, ok := DecimalQuo(amount, totalQty)
	if !ok {
		t.Fatal("DecimalQuo 意外的除零")
	}
	if got := DecimalString(cost, 4); got != "10.4478" {
		t.Fatalf("cost (5,4) = %q, want 10.4478", got)
	}
}

// TestDecimalRoundingHalfAwayFromZero 验证舍入规则与 PG numeric 一致。
func TestDecimalRoundingHalfAwayFromZero(t *testing.T) {
	cases := []struct {
		in    string
		scale int
		want  string
	}{
		{"0.15", 1, "0.2"},
		{"-0.15", 1, "-0.2"}, // 远离零，不是 -0.1
		{"2.5", 0, "3"},      // 远离零，不是银行家舍入的 2
		{"-2.5", 0, "-3"},    //
		{"0.0000000001", 10, "0.0000000001"},
		{"0.00000000004", 10, "0.0000000000"},
		{"0.00000000005", 10, "0.0000000001"},
		{"1", 4, "1.0000"},
		{"0", 10, "0.0000000000"},
		{"-0.00000000005", 10, "-0.0000000001"},
	}
	for _, c := range cases {
		if got := DecimalString(mustRat(t, c.in), c.scale); got != c.want {
			t.Errorf("DecimalString(%q, %d) = %q, want %q", c.in, c.scale, got, c.want)
		}
	}
}

// TestDecimalQuoByZero 验证除零返回 ok=false 而不是 0。
//
// 静默返回 0 会让「成本价未知」看起来像「成本价为零」，是很危险的骗人。
func TestDecimalQuoByZero(t *testing.T) {
	if _, ok := DecimalQuo(mustRat(t, "1"), mustRat(t, "0")); ok {
		t.Fatal("除以 0 应当返回 ok=false")
	}
	if _, ok := DecimalQuo(mustRat(t, "1"), nil); ok {
		t.Fatal("除以 nil 应当返回 ok=false")
	}
}

// TestParseDecimalRejectsNonDecimal 验证只接受纯十进制字面量。
//
// big.Rat.SetString 能解析 "1/2"、"1e3"、"0x10"，金额列里出现这些形态
// 说明上游有 bug，必须在边界拒绝而不是猜。
func TestParseDecimalRejectsNonDecimal(t *testing.T) {
	for _, s := range []string{"1/2", "1e3", "0x10", "", "  ", "abc", "+1.5", "1,5"} {
		if r, err := ParseDecimal(s); err == nil {
			t.Errorf("ParseDecimal(%q) 应当失败，得到 %v", s, r)
		}
	}
}

// TestDecimalNilIsZero 验证 nil 入参按 0 处理（调用点不必判空）。
func TestDecimalNilIsZero(t *testing.T) {
	if got := DecimalString(nil, decimalScaleAmount); got != "0.0000000000" {
		t.Fatalf("nil → %q, want 0.0000000000", got)
	}
	if got := DecimalString(DecimalAdd(nil, mustRat(t, "1.5")), 1); got != "1.5" {
		t.Fatalf("nil + 1.5 = %q", got)
	}
	if DecimalSign(nil) != 0 {
		t.Fatal("nil 的符号应为 0")
	}
}

// TestDecimalNoMutation 验证运算不复用入参（避免别名 bug）。
func TestDecimalNoMutation(t *testing.T) {
	a := mustRat(t, "2")
	b := mustRat(t, "3")
	_ = DecimalAdd(a, b)
	if a.Cmp(mustRat(t, "2")) != 0 || b.Cmp(mustRat(t, "3")) != 0 {
		t.Fatal("DecimalAdd 修改了入参")
	}
}
