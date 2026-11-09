package agent

// 技术指标计算（纯函数，零 IO）。
//
// # 精度策略
//
// 输入输出一律用 string（NUMERIC 精度，项目铁律），但**内部用 float64**：
//
//  1. 指标是统计量不是金额，精度要求低于撮合。RSI 是 0..100 的比值、
//     ATR 是波动幅度、BOLL 是均线加减标准差 —— 它们的有效位数远低于金额。
//  2. TA-Lib 本身就是 float64 实现。用 big.Rat 计算会导致**无法与黄金值对撞**
//     （黄金值来自 TA-Lib），失去唯一的独立验证手段。
//  3. 输出时格式化到固定 6 位小数，避免 `38.779998779296875` 这种 float64
//     长尾污染下游比较。
//
// 这不是「为了省事放弃精度」：inputs 的字符串精度在 parse 阶段就转成
// float64 了，但指标结果的业务含义（相对强弱、波动率、通道位置）对 1e-6
// 的误差不敏感。金额类计算（下单、风控名义金额）仍然走 big.Rat，见
// intercept/matchers.go。
//
// # warmup 语义
//
// 所有指标在数据不足时返回**空字符串 ""**，不是 "0" 也不是 NaN（Go 的
// float64 能表示 NaN，但字符串不行；用 "" 明确表示「无值」）。
// 测试断言前 N 个是空串 —— 否则实现把 warmup 算错（比如用第一个值当 EMA
// 种子）也能"通过"。
//
// # 平滑方式（与 TA-Lib 对齐，见 testdata/gen_indicators_golden.py 的注释）
//
//   - EMA：SMA 种子 + α = 2/(period+1)
//   - RSI/ATR：Wilder 平滑，α = 1/period（**不是** 2/(period+1)）
//   - BOLL：SMA + 总体标准差（ddof=0，**不是** ddof=1）
//   - KDJ：RSV → SMA 平滑 K → SMA 平滑 D，J = 3K - 2D

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// indicatorDecimals 是输出的小数位数。
// 6 位足以表达所有指标的业务含义，又能吃掉 float64 的长尾噪声。
const indicatorDecimals = 6

var (
	// ErrEmptySeries 表示输入为空。
	ErrEmptySeries = errors.New("指标输入序列为空")
	// ErrLengthMismatch 表示多序列输入长度不一致。
	ErrLengthMismatch = errors.New("指标输入序列长度不一致")
	// ErrBadPeriod 表示周期参数非法。
	ErrBadPeriod = errors.New("指标周期参数非法")
)

// parseSeries 把字符串序列转成 float64 序列。
//
// 空字符串视为「缺失」，转成 NaN（与 TA-Lib 的缺失语义一致），
// 让指标函数内部能统一处理而不必到处判空。
func parseSeries(name string, xs []string) ([]float64, error) {
	if len(xs) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrEmptySeries, name)
	}
	out := make([]float64, len(xs))
	for i, s := range xs {
		s = strings.TrimSpace(s)
		if s == "" {
			out[i] = math.NaN()
			continue
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil, fmt.Errorf("%s[%d]=%q 无法解析为数字: %w", name, i, s, err)
		}
		out[i] = f
	}
	return out, nil
}

// formatSeries 把 float64 序列转回字符串序列，NaN 写成 ""。
func formatSeries(xs []float64) []string {
	out := make([]string, len(xs))
	for i, f := range xs {
		if math.IsNaN(f) || math.IsInf(f, 0) {
			out[i] = ""
			continue
		}
		// -0 归一为 0，避免出现 "-0.000000"
		if f == 0 {
			f = 0
		}
		out[i] = strconv.FormatFloat(f, 'f', indicatorDecimals, 64)
	}
	return out
}

// allFinite 报告 [0,n) 是否都是有限值（非 NaN 非 Inf）。
// 用于判断某段窗口能否参与计算。
func allFinite(xs []float64, n int) bool {
	if n > len(xs) {
		return false
	}
	for i := 0; i < n; i++ {
		if math.IsNaN(xs[i]) || math.IsInf(xs[i], 0) {
			return false
		}
	}
	return true
}

// sma 计算简单移动平均，返回长度与输入相同的序列（前 period-1 个为 NaN）。
func sma(xs []float64, period int) []float64 {
	out := make([]float64, len(xs))
	for i := range out {
		out[i] = math.NaN()
	}
	if period <= 0 || len(xs) < period {
		return out
	}
	sum := 0.0
	for i := 0; i < len(xs); i++ {
		if math.IsNaN(xs[i]) {
			// 窗口内有缺失，重置累加器，后续需重新累计 period 个有效值。
			sum = 0
			continue
		}
		sum += xs[i]
		if i >= period {
			sum -= xs[i-period]
		}
		if i >= period-1 && allFinite(xs[i-period+1:i+1], period) {
			out[i] = sum / float64(period)
		}
	}
	return out
}

// smaOfNaNSeries 对已经含 NaN 的序列做 SMA，**跳过**那些窗口内含 NaN 的位置。
// 与 sma 的区别：sma 用于原始价格序列（无 NaN），这个用于指标序列（前段是 NaN）。
func smaOfNaNSeries(xs []float64, period int) []float64 {
	out := make([]float64, len(xs))
	for i := range out {
		out[i] = math.NaN()
	}
	if period <= 0 || len(xs) < period {
		return out
	}
	for i := period - 1; i < len(xs); i++ {
		var sum float64
		ok := true
		for j := i - period + 1; j <= i; j++ {
			if math.IsNaN(xs[j]) || math.IsInf(xs[j], 0) {
				ok = false
				break
			}
			sum += xs[j]
		}
		if ok {
			out[i] = sum / float64(period)
		}
	}
	return out
}

// ema 计算指数移动平均，**用 SMA 作为种子**（TA-Lib 行为）。
//
// 这是关键：很多实现把第一个值直接当 EMA 种子，导致整条序列偏移。
// TA-Lib 的做法是：
//
//	EMA[period-1] = SMA(xs[0..period-1])
//	EMA[i]        = EMA[i-1] + α*(xs[i] - EMA[i-1]),  α = 2/(period+1)
//
// 前 period-1 个位置为 NaN。
func ema(xs []float64, period int) []float64 {
	out := make([]float64, len(xs))
	for i := range out {
		out[i] = math.NaN()
	}
	if period <= 0 || len(xs) < period {
		return out
	}
	// 种子：前 period 个值的简单平均。若窗口内有 NaN，往后顺延。
	seedEnd := -1
	for start := 0; start+period <= len(xs); start++ {
		if allFinite(xs[start:start+period], period) {
			var sum float64
			for _, v := range xs[start : start+period] {
				sum += v
			}
			seedEnd = start + period - 1
			out[seedEnd] = sum / float64(period)
			break
		}
	}
	if seedEnd < 0 {
		return out
	}
	alpha := 2.0 / float64(period+1)
	prev := out[seedEnd]
	for i := seedEnd + 1; i < len(xs); i++ {
		if math.IsNaN(xs[i]) || math.IsInf(xs[i], 0) {
			out[i] = math.NaN()
			continue
		}
		prev = prev + alpha*(xs[i]-prev)
		out[i] = prev
	}
	return out
}

// wilder 计算 Wilder 平滑（α = 1/period），**用 SMA 作为种子**。
//
// RSI 和 ATR 都用它。这是最常被写错的地方：很多人拿 EMA 的 α=2/(n+1)
// 套在 RSI 上，结果整条序列数值全错。
//
//	seed = SMA(xs[0..period-1])
//	W[i] = W[i-1] + (xs[i] - W[i-1]) / period
func wilder(xs []float64, period int) []float64 {
	out := make([]float64, len(xs))
	for i := range out {
		out[i] = math.NaN()
	}
	if period <= 0 || len(xs) < period {
		return out
	}
	seedEnd := -1
	for start := 0; start+period <= len(xs); start++ {
		if allFinite(xs[start:start+period], period) {
			var sum float64
			for _, v := range xs[start : start+period] {
				sum += v
			}
			seedEnd = start + period - 1
			out[seedEnd] = sum / float64(period)
			break
		}
	}
	if seedEnd < 0 {
		return out
	}
	prev := out[seedEnd]
	for i := seedEnd + 1; i < len(xs); i++ {
		if math.IsNaN(xs[i]) || math.IsInf(xs[i], 0) {
			out[i] = math.NaN()
			continue
		}
		prev = prev + (xs[i]-prev)/float64(period)
		out[i] = prev
	}
	return out
}

// emaAt 计算 EMA，但**从指定起点开始**取种子。
//
// 这是 TA-Lib 的内部细节，非常反直觉：TA-Lib 的 MACD 计算 fast EMA 时，
// 不是从序列第一个值开始取种子，而是从 “slowperiod - fastperiod“ 处开始 ——
// 目的是让快慢两条 EMA **在同一个位置有定义**，这样两者相减才有意义。
//
// 实测验证（QQQ 2020-2021）：
//
//	fast EMA 从 offset=0 开始 → MACD[33]=4.6604613
//	fast EMA 从 offset=14 开始 → MACD[33]=4.4226492  ← 与 TA-Lib 完全一致
//
// 若用 offset=0（直觉做法），整条 MACD 线会偏移 0.2 左右，且永不对齐。
func emaAt(xs []float64, period, offset int) []float64 {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(xs) {
		out := make([]float64, len(xs))
		for i := range out {
			out[i] = math.NaN()
		}
		return out
	}
	tail := ema(xs[offset:], period)
	out := make([]float64, len(xs))
	for i := range out {
		out[i] = math.NaN()
	}
	for i := offset; i < len(xs); i++ {
		out[i] = tail[i-offset]
	}
	return out
}

// MACD 计算 MACD(12,26,9)。
//
// 返回三条等长序列：macd（快慢 EMA 之差）、signal（MACD 的 9 期 EMA）、
// hist（macd - signal）。
//
// warmup：由于 signal 需要对 macd 再做一次 EMA(9)，而 macd 本身要到
// 第 26 个值才有定义，故首个有效位置是 slow+signal-2 = 33（0-indexed）。
func MACD(closes []string) (macd, signal, hist []string, err error) {
	const fast, slow, sig = 12, 26, 9
	c, err := parseSeries("closes", closes)
	if err != nil {
		return nil, nil, nil, err
	}
	// 快线从 slow-fast 处开始取种子 —— TA-Lib 行为，见 emaAt 的注释。
	emaFast := emaAt(c, fast, slow-fast)
	emaSlow := ema(c, slow)

	macdLine := make([]float64, len(c))
	for i := range c {
		if math.IsNaN(emaFast[i]) || math.IsNaN(emaSlow[i]) {
			macdLine[i] = math.NaN()
			continue
		}
		macdLine[i] = emaFast[i] - emaSlow[i]
	}
	signalLine := ema(macdLine, sig)

	histLine := make([]float64, len(c))
	for i := range c {
		if math.IsNaN(macdLine[i]) || math.IsNaN(signalLine[i]) {
			histLine[i] = math.NaN()
			continue
		}
		histLine[i] = macdLine[i] - signalLine[i]
	}
	// TA-Lib 把 MACD 的三条输出**对齐到同一个 first_valid**（= signal 的起点，
	// 因为 signal 是最后一个才能算出的分量）。macd 线本身在第 26 个值就有定义，
	// 但 TA-Lib 从 33 才开始输出。不对齐会让 warmup 断言失败，也会让下游
	// 「三条线同时可用」的判断出错。
	alignTo := firstValidIndex(signalLine)
	if alignTo > 0 {
		for i := 0; i < alignTo && i < len(macdLine); i++ {
			macdLine[i] = math.NaN()
			histLine[i] = math.NaN()
		}
	}
	return formatSeries(macdLine), formatSeries(signalLine), formatSeries(histLine), nil
}

// firstValidIndex 返回首个非 NaN 的下标；没有则返回 -1。
func firstValidIndex(xs []float64) int {
	for i, v := range xs {
		if !math.IsNaN(v) && !math.IsInf(v, 0) {
			return i
		}
	}
	return -1
}

// RSI 计算相对强弱指数（Wilder 平滑）。
//
// RSI = 100 - 100/(1+RS)，RS = 平均涨幅/平均跌幅，两者都用 Wilder 平滑。
//
// 边界情况（必须与 TA-Lib 一致）：
//   - 平均跌幅为 0（单边上涨）→ RSI = 100
//   - 平均涨幅为 0（单边下跌）→ RSI = 0
//   - 两者都为 0（价格完全不动）→ TA-Lib 返回 0
func RSI(closes []string, period int) ([]string, error) {
	if period <= 0 {
		return nil, fmt.Errorf("%w: period=%d", ErrBadPeriod, period)
	}
	c, err := parseSeries("closes", closes)
	if err != nil {
		return nil, err
	}
	n := len(c)
	out := make([]float64, n)
	for i := range out {
		out[i] = math.NaN()
	}
	if n <= period {
		return formatSeries(out), nil
	}

	// 先算逐期涨跌。
	gains := make([]float64, n)
	losses := make([]float64, n)
	for i := 1; i < n; i++ {
		if math.IsNaN(c[i]) || math.IsNaN(c[i-1]) {
			gains[i] = math.NaN()
			losses[i] = math.NaN()
			continue
		}
		diff := c[i] - c[i-1]
		if diff >= 0 {
			gains[i] = diff
			losses[i] = 0
		} else {
			gains[i] = 0
			losses[i] = -diff
		}
	}
	// gains[0] 无定义，用 NaN 表示；Wilder 种子从 index 1 开始取 period 个。
	gains[0] = math.NaN()
	losses[0] = math.NaN()

	avgGain := wilder(gains[1:], period)
	avgLoss := wilder(losses[1:], period)

	for i := 0; i < len(avgGain); i++ {
		if math.IsNaN(avgGain[i]) || math.IsNaN(avgLoss[i]) {
			continue
		}
		var rsi float64
		switch {
		case avgLoss[i] == 0 && avgGain[i] == 0:
			rsi = 0 // 完全不动 —— TA-Lib 返回 0
		case avgLoss[i] == 0:
			rsi = 100
		default:
			rs := avgGain[i] / avgLoss[i]
			rsi = 100 - 100/(1+rs)
		}
		out[i+1] = rsi
	}
	return formatSeries(out), nil
}

// ATR 计算平均真实波幅（Wilder 平滑）。
//
// TR = max(high-low, |high-prevClose|, |low-prevClose|)
// ATR = Wilder(TR, period)
//
// 第一个 TR 没有 prevClose，TA-Lib 用 high-low。
func ATR(highs, lows, closes []string, period int) ([]string, error) {
	if period <= 0 {
		return nil, fmt.Errorf("%w: period=%d", ErrBadPeriod, period)
	}
	h, err := parseSeries("highs", highs)
	if err != nil {
		return nil, err
	}
	l, err := parseSeries("lows", lows)
	if err != nil {
		return nil, err
	}
	c, err := parseSeries("closes", closes)
	if err != nil {
		return nil, err
	}
	if len(h) != len(l) || len(h) != len(c) {
		return nil, fmt.Errorf("%w: high=%d low=%d close=%d", ErrLengthMismatch, len(h), len(l), len(c))
	}
	n := len(h)
	out := make([]float64, n)
	for i := range out {
		out[i] = math.NaN()
	}
	if n == 0 {
		return formatSeries(out), nil
	}

	tr := make([]float64, n)
	for i := 0; i < n; i++ {
		if math.IsNaN(h[i]) || math.IsNaN(l[i]) {
			tr[i] = math.NaN()
			continue
		}
		hl := h[i] - l[i]
		if i == 0 || math.IsNaN(c[i-1]) {
			// TR[0] 没有 prevClose。TA-Lib 把它当**未定义**，种子的 14 个值
			// 从 TR[1] 开始取。若这里用 high-low 顶替，种子会偏移
			// （实测 ATR[14] 偏 6.8e-3）。
			tr[i] = math.NaN()
			continue
		}
		hc := math.Abs(h[i] - c[i-1])
		lc := math.Abs(l[i] - c[i-1])
		tr[i] = math.Max(hl, math.Max(hc, lc))
	}
	// wilder 的种子窗口会跳过 NaN，自然从 TR[1] 开始取 14 个值，
	// 首个有效位置因此落在 index=period（与 TA-Lib 一致）。
	out = wilder(tr, period)
	return formatSeries(out), nil
}

// BOLL 计算布林带。
//
// mid = SMA(closes, period)
// upper = mid + k*σ,  lower = mid - k*σ
//
// σ 是**总体标准差（ddof=0）**，不是样本标准差（ddof=1）。
// numpy 的 np.std() 默认 ddof=0，pandas 的 .std() 默认 ddof=1 —— 用错差
// 一个 sqrt(n/(n-1)) 的系数。TA-Lib 用 ddof=0。
func BOLL(closes []string, period int, k float64) (upper, mid, lower []string, err error) {
	if period <= 0 {
		return nil, nil, nil, fmt.Errorf("%w: period=%d", ErrBadPeriod, period)
	}
	if k <= 0 {
		return nil, nil, nil, fmt.Errorf("%w: k=%v", ErrBadPeriod, k)
	}
	c, err := parseSeries("closes", closes)
	if err != nil {
		return nil, nil, nil, err
	}
	n := len(c)
	midLine := sma(c, period)
	upLine := make([]float64, n)
	loLine := make([]float64, n)
	for i := range upLine {
		upLine[i] = math.NaN()
		loLine[i] = math.NaN()
	}
	for i := period - 1; i < n; i++ {
		if math.IsNaN(midLine[i]) {
			continue
		}
		if !allFinite(c[i-period+1:i+1], period) {
			continue
		}
		var sqSum float64
		m := midLine[i]
		for j := i - period + 1; j <= i; j++ {
			d := c[j] - m
			sqSum += d * d
		}
		sd := math.Sqrt(sqSum / float64(period)) // ddof=0
		upLine[i] = m + k*sd
		loLine[i] = m - k*sd
	}
	return formatSeries(upLine), formatSeries(midLine), formatSeries(loLine), nil
}

// KDJ 计算随机指标。
//
// RSV[i] = (close[i] - LLV(low, kPeriod)) / (HHV(high, kPeriod) - LLV(low, kPeriod)) * 100
// K      = SMA(RSV, smooth)     ← 国内惯例用 SMA，不是 EMA
// D      = SMA(K, dPeriod)
// J      = 3K - 2D
//
// 参数：
//   - 国内 A 股惯例 (9, 3, 3)
//   - TA-Lib 默认 (5, 3, 3)
//
// 分母为 0（kPeriod 内价格完全不动）时 RSV 记为 50（中位），避免除零。
func KDJ(highs, lows, closes []string, kPeriod, dPeriod, smooth int) (k, d, j []string, err error) {
	if kPeriod <= 0 || dPeriod <= 0 || smooth <= 0 {
		return nil, nil, nil, fmt.Errorf("%w: kPeriod=%d dPeriod=%d smooth=%d", ErrBadPeriod, kPeriod, dPeriod, smooth)
	}
	h, err := parseSeries("highs", highs)
	if err != nil {
		return nil, nil, nil, err
	}
	l, err := parseSeries("lows", lows)
	if err != nil {
		return nil, nil, nil, err
	}
	c, err := parseSeries("closes", closes)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(h) != len(l) || len(h) != len(c) {
		return nil, nil, nil, fmt.Errorf("%w: high=%d low=%d close=%d", ErrLengthMismatch, len(h), len(l), len(c))
	}
	n := len(h)
	rsv := make([]float64, n)
	for i := range rsv {
		rsv[i] = math.NaN()
	}
	for i := kPeriod - 1; i < n; i++ {
		if !allFinite(h[i-kPeriod+1:i+1], kPeriod) || !allFinite(l[i-kPeriod+1:i+1], kPeriod) || math.IsNaN(c[i]) {
			continue
		}
		hh, ll := h[i-kPeriod+1], l[i-kPeriod+1]
		for _, v := range h[i-kPeriod+1 : i+1] {
			if v > hh {
				hh = v
			}
		}
		for _, v := range l[i-kPeriod+1 : i+1] {
			if v < ll {
				ll = v
			}
		}
		if hh == ll {
			rsv[i] = 50 // 分母为 0 —— 取中位，避免除零
			continue
		}
		rsv[i] = (c[i] - ll) / (hh - ll) * 100
	}

	kLine := smaOfNaNSeries(rsv, smooth)
	dLine := smaOfNaNSeries(kLine, dPeriod)

	// TA-Lib 把 K 和 D **对齐到同一个 first_valid**（= D 的起点）。
	// 公式：kPeriod-1(RSV) + smooth-1(K) + dPeriod-1(D)
	// 实测 (9,3,3) → 12、(5,3,3) → 8，与 TA-Lib 输出一致。
	// K 本身在 kPeriod-1+smooth-1 就有定义，但 TA-Lib 从 D 的起点才开始输出 K。
	alignTo := (kPeriod - 1) + (smooth - 1) + (dPeriod - 1)
	for i := 0; i < alignTo && i < n; i++ {
		kLine[i] = math.NaN()
		dLine[i] = math.NaN()
	}

	jLine := make([]float64, n)
	for i := range jLine {
		if math.IsNaN(kLine[i]) || math.IsNaN(dLine[i]) {
			jLine[i] = math.NaN()
			continue
		}
		jLine[i] = 3*kLine[i] - 2*dLine[i]
	}
	return formatSeries(kLine), formatSeries(dLine), formatSeries(jLine), nil
}

// VWAP 计算**累积**成交量加权平均价。
//
// VWAP[i] = Σ(typicalPrice[j] * vol[j]) / Σ vol[j],  j = 0..i
// typicalPrice = (high + low + close) / 3
//
// typical price 用 (h+l+c)/3 而**不是** (h+l+2c)/4：
//   - (h+l+c)/3 与 TA-Lib 的 VWAP 一致，可对撞黄金值验证。
//   - 部分教材用 (h+l+2c)/4（更重视收盘价），但那样**无法与 TA-Lib 对撞**，
//     而 VWAP 没有其他独立验证手段 —— 等于失去唯一的正确性保证。
//   - 因此选定 (h+l+c)/3。若要改成 (h+l+2c)/4，必须同时换一个可对撞的
//     参考实现，否则就是「凭感觉改」。
//
// 这是累积型（与 TA-Lib 一致），从序列起点累加，**不是**滚动窗口。
// 按交易日重置需要调用方先用 trading_date 分组，每组独立调用本函数 ——
// 指标函数不该知道「交易日」这个概念（纯函数原则）。
func VWAP(highs, lows, closes, volumes []string) ([]string, error) {
	h, err := parseSeries("highs", highs)
	if err != nil {
		return nil, err
	}
	l, err := parseSeries("lows", lows)
	if err != nil {
		return nil, err
	}
	c, err := parseSeries("closes", closes)
	if err != nil {
		return nil, err
	}
	v, err := parseSeries("volumes", volumes)
	if err != nil {
		return nil, err
	}
	if len(h) != len(l) || len(h) != len(c) || len(h) != len(v) {
		return nil, fmt.Errorf("%w: high=%d low=%d close=%d volume=%d",
			ErrLengthMismatch, len(h), len(l), len(c), len(v))
	}
	n := len(h)
	out := make([]float64, n)
	for i := range out {
		out[i] = math.NaN()
	}
	var cumPV, cumV float64
	for i := 0; i < n; i++ {
		if math.IsNaN(h[i]) || math.IsNaN(l[i]) || math.IsNaN(c[i]) || math.IsNaN(v[i]) {
			continue
		}
		tp := (h[i] + l[i] + c[i]) / 3
		cumPV += tp * v[i]
		cumV += v[i]
		if cumV == 0 {
			continue // 累计成交量为 0，无法得到有意义的价格
		}
		out[i] = cumPV / cumV
	}
	return formatSeries(out), nil
}
