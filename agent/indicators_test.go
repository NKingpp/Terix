package agent

import (
	"bufio"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// goldenFile 对应 testdata/indicators_golden.json 的结构。
// 只声明测试用到的字段。
type goldenFile struct {
	Meta struct {
		TalibVersion string `json:"talib_version"`
		Symbol       string `json:"symbol"`
		Range        string `json:"range"`
		BarCount     int    `json:"bar_count"`
		BarsSHA256   string `json:"bars_sha256"`
		SampleIdx    []int  `json:"sample_indices"`
	} `json:"_meta"`
	MACD struct {
		FirstValid map[string]int `json:"first_valid"`
		Samples    struct {
			MACD   map[string]*string `json:"macd"`
			Signal map[string]*string `json:"signal"`
			Hist   map[string]*string `json:"hist"`
		} `json:"samples"`
	} `json:"MACD"`
	RSI struct {
		FirstValid int                `json:"first_valid"`
		Samples    map[string]*string `json:"samples"`
	} `json:"RSI"`
	ATR struct {
		FirstValid int                `json:"first_valid"`
		Samples    map[string]*string `json:"samples"`
	} `json:"ATR"`
	BOLL struct {
		FirstValid map[string]int `json:"first_valid"`
		Samples    struct {
			Upper map[string]*string `json:"upper"`
			Mid   map[string]*string `json:"mid"`
			Lower map[string]*string `json:"lower"`
		} `json:"samples"`
	} `json:"BOLL"`
	KDJCN933 struct {
		FirstValid map[string]int `json:"first_valid"`
		Samples    struct {
			K map[string]*string `json:"k"`
			D map[string]*string `json:"d"`
			J map[string]*string `json:"j"`
		} `json:"samples"`
	} `json:"KDJ_CN_933"`
	KDJDefault533 struct {
		FirstValid map[string]int `json:"first_valid"`
		Samples    struct {
			K map[string]*string `json:"k"`
			D map[string]*string `json:"d"`
			J map[string]*string `json:"j"`
		} `json:"samples"`
	} `json:"KDJ_TALibDefault_533"`
	VWAP struct {
		FirstValid int                `json:"first_valid"`
		Samples    map[string]*string `json:"samples"`
	} `json:"VWAP"`
}

// loadGolden 读黄金值文件并加载对应的输入数据。
func loadGolden(t *testing.T) (*goldenFile, *barSeries) {
	t.Helper()
	path := filepath.Join("..", "testdata", "indicators_golden.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读黄金值失败（先跑 testdata/gen_indicators_golden.py）: %v", err)
	}
	var g goldenFile
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatalf("解析黄金值失败: %v", err)
	}
	bs := loadBars(t, filepath.Join("..", "testdata", "qqq_2020_2021.psv"))
	if bs.n != g.Meta.BarCount {
		t.Fatalf("输入数据 %d 根，黄金值基于 %d 根 —— 数据文件与黄金值不匹配", bs.n, g.Meta.BarCount)
	}
	return &g, bs
}

// barSeries 是测试用的 OHLCV 序列（字符串形式，与生产函数签名一致）。
type barSeries struct {
	dates                       []string
	open, high, low, close, vol []string
	n                           int
}

func loadBars(t *testing.T, path string) *barSeries {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("打开 %s 失败: %v", path, err)
	}
	defer f.Close()

	bs := &barSeries{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) != 6 {
			t.Fatalf("PSV 格式错误（需 6 段）: %q", line)
		}
		bs.dates = append(bs.dates, parts[0])
		bs.open = append(bs.open, parts[1])
		bs.high = append(bs.high, parts[2])
		bs.low = append(bs.low, parts[3])
		bs.close = append(bs.close, parts[4])
		bs.vol = append(bs.vol, parts[5])
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("读 %s 失败: %v", path, err)
	}
	bs.n = len(bs.close)
	if bs.n == 0 {
		t.Fatalf("%s 无数据", path)
	}
	return bs
}

// assertWarmupEmpty 断言 [0, firstValid) 全是空串。
//
// 这条断言是黄金值测试的核心价值之一：如果实现把 warmup 算错（比如
// 用第一个值当 EMA 种子），前段就不是空的，但稳态值可能仍然接近 ——
// 只比稳态值会漏掉这类错误。
func assertWarmupEmpty(t *testing.T, name string, got []string, firstValid int) {
	t.Helper()
	if firstValid < 0 {
		return
	}
	if firstValid > len(got) {
		t.Fatalf("%s: first_valid=%d 超出序列长度 %d", name, firstValid, len(got))
	}
	for i := 0; i < firstValid; i++ {
		if got[i] != "" {
			t.Fatalf("%s warmup[%d] = %q，期望空串（该位置数据不足，不应有值）", name, i, got[i])
		}
	}
}

// assertSamples 对抽样点逐个比对，容差 1e-5。
//
// 为什么不是精确相等：黄金值是 TA-Lib 的 float64 输出经 repr() 后再
// FormatFloat(6) 格式化，两侧的舍入路径不同，末位可能差 1 ulp。
// 1e-5 的绝对容差远小于任何指标的业务含义（RSI 的 1e-5 无意义），
// 但足以抓出实现错误（少一个系数、平滑方式错、ddof 用错都会差几个数量级）。
func assertSamples(t *testing.T, name string, got []string, want map[string]*string) {
	t.Helper()
	const tol = 1e-5
	if len(want) == 0 {
		t.Fatalf("%s: 黄金值里没有抽样点", name)
	}
	for idxStr, wantPtr := range want {
		idx, err := strconv.Atoi(idxStr)
		if err != nil {
			t.Fatalf("%s: 抽样下标 %q 不是数字", name, idxStr)
		}
		if idx >= len(got) {
			t.Fatalf("%s: 抽样下标 %d 超出序列长度 %d", name, idx, len(got))
		}
		gotStr := got[idx]

		if wantPtr == nil {
			// 黄金值是 null（NaN）→ 我们必须是空串
			if gotStr != "" {
				t.Errorf("%s[%d] = %q，黄金值为 null（warmup），期望空串", name, idx, gotStr)
			}
			continue
		}
		if gotStr == "" {
			t.Errorf("%s[%d] 为空，黄金值 %s", name, idx, *wantPtr)
			continue
		}
		gotF, err := strconv.ParseFloat(gotStr, 64)
		if err != nil {
			t.Errorf("%s[%d] = %q 无法解析", name, idx, gotStr)
			continue
		}
		wantF, err := strconv.ParseFloat(*wantPtr, 64)
		if err != nil {
			t.Fatalf("%s[%d] 黄金值 %q 无法解析", name, idx, *wantPtr)
		}
		if math.Abs(gotF-wantF) > tol {
			t.Errorf("%s[%d] = %.10f，黄金值 %.10f，差 %.3e 超过容差 %g",
				name, idx, gotF, wantF, math.Abs(gotF-wantF), tol)
		}
	}
}

// TestIndicatorsGolden 用真实 QQQ 数据对撞 TA-Lib 黄金值。
//
// 覆盖 6 个指标 + warmup 边界 + 抽样点。数据是 2020-01-02..2021-12-31
// 共 505 根日线，含 2020-03 熔断的极端行情。
func TestIndicatorsGolden(t *testing.T) {
	g, bs := loadGolden(t)

	t.Run("MACD", func(t *testing.T) {
		macd, signal, hist, err := MACD(bs.close)
		if err != nil {
			t.Fatalf("MACD: %v", err)
		}
		if len(macd) != bs.n || len(signal) != bs.n || len(hist) != bs.n {
			t.Fatalf("MACD 返回长度 %d/%d/%d，期望 %d", len(macd), len(signal), len(hist), bs.n)
		}
		assertWarmupEmpty(t, "MACD.macd", macd, g.MACD.FirstValid["macd"])
		assertWarmupEmpty(t, "MACD.signal", signal, g.MACD.FirstValid["signal"])
		assertWarmupEmpty(t, "MACD.hist", hist, g.MACD.FirstValid["hist"])
		assertSamples(t, "MACD.macd", macd, g.MACD.Samples.MACD)
		assertSamples(t, "MACD.signal", signal, g.MACD.Samples.Signal)
		assertSamples(t, "MACD.hist", hist, g.MACD.Samples.Hist)
	})

	t.Run("RSI", func(t *testing.T) {
		rsi, err := RSI(bs.close, 14)
		if err != nil {
			t.Fatalf("RSI: %v", err)
		}
		assertWarmupEmpty(t, "RSI", rsi, g.RSI.FirstValid)
		assertSamples(t, "RSI", rsi, g.RSI.Samples)
	})

	t.Run("ATR", func(t *testing.T) {
		atr, err := ATR(bs.high, bs.low, bs.close, 14)
		if err != nil {
			t.Fatalf("ATR: %v", err)
		}
		assertWarmupEmpty(t, "ATR", atr, g.ATR.FirstValid)
		assertSamples(t, "ATR", atr, g.ATR.Samples)
	})

	t.Run("BOLL", func(t *testing.T) {
		up, mid, lo, err := BOLL(bs.close, 20, 2.0)
		if err != nil {
			t.Fatalf("BOLL: %v", err)
		}
		assertWarmupEmpty(t, "BOLL.upper", up, g.BOLL.FirstValid["upper"])
		assertWarmupEmpty(t, "BOLL.mid", mid, g.BOLL.FirstValid["mid"])
		assertWarmupEmpty(t, "BOLL.lower", lo, g.BOLL.FirstValid["lower"])
		assertSamples(t, "BOLL.upper", up, g.BOLL.Samples.Upper)
		assertSamples(t, "BOLL.mid", mid, g.BOLL.Samples.Mid)
		assertSamples(t, "BOLL.lower", lo, g.BOLL.Samples.Lower)
	})

	// 国内 A 股惯例 (9,3,3) —— 主测试
	t.Run("KDJ_CN_933", func(t *testing.T) {
		k, d, j, err := KDJ(bs.high, bs.low, bs.close, 9, 3, 3)
		if err != nil {
			t.Fatalf("KDJ(9,3,3): %v", err)
		}
		assertWarmupEmpty(t, "KDJ.k", k, g.KDJCN933.FirstValid["k"])
		assertWarmupEmpty(t, "KDJ.d", d, g.KDJCN933.FirstValid["d"])
		assertWarmupEmpty(t, "KDJ.j", j, g.KDJCN933.FirstValid["j"])
		assertSamples(t, "KDJ.k", k, g.KDJCN933.Samples.K)
		assertSamples(t, "KDJ.d", d, g.KDJCN933.Samples.D)
		assertSamples(t, "KDJ.j", j, g.KDJCN933.Samples.J)
	})

	// TA-Lib 默认 (5,3,3) —— 辅助测试。
	// 价值：证明实现框架正确，只是参数不同。
	// 若只测 (9,3,3)，无法区分「实现正确」与「参数凑巧」。
	t.Run("KDJ_TALibDefault_533", func(t *testing.T) {
		k, d, j, err := KDJ(bs.high, bs.low, bs.close, 5, 3, 3)
		if err != nil {
			t.Fatalf("KDJ(5,3,3): %v", err)
		}
		assertWarmupEmpty(t, "KDJ533.k", k, g.KDJDefault533.FirstValid["k"])
		assertWarmupEmpty(t, "KDJ533.d", d, g.KDJDefault533.FirstValid["d"])
		assertWarmupEmpty(t, "KDJ533.j", j, g.KDJDefault533.FirstValid["j"])
		assertSamples(t, "KDJ533.k", k, g.KDJDefault533.Samples.K)
		assertSamples(t, "KDJ533.d", d, g.KDJDefault533.Samples.D)
		assertSamples(t, "KDJ533.j", j, g.KDJDefault533.Samples.J)
	})

	t.Run("VWAP", func(t *testing.T) {
		vwap, err := VWAP(bs.high, bs.low, bs.close, bs.vol)
		if err != nil {
			t.Fatalf("VWAP: %v", err)
		}
		assertWarmupEmpty(t, "VWAP", vwap, g.VWAP.FirstValid)
		assertSamples(t, "VWAP", vwap, g.VWAP.Samples)
	})
}

// TestVWAPDailyReset 验证「按日重置」的做法：分组后每组独立算累积 VWAP。
//
// 指标函数本身不知道「交易日」（纯函数原则），按日重置由调用方分组完成。
// 这里用合成的两日数据验证分组语义：每组的 VWAP 只受本组数据影响。
func TestVWAPDailyReset(t *testing.T) {
	// 第 1 天：3 根；第 2 天：2 根。第 2 天价格有巨大跳变，
	// 如果 VWAP 累积不过界，第 2 天的值会被第 1 天污染。
	high := []string{"10", "11", "12", "100", "101"}
	low := []string{"9", "10", "11", "99", "100"}
	close := []string{"9.5", "10.5", "11.5", "99.5", "100.5"}
	vol := []string{"100", "100", "100", "100", "100"}

	// 累积（不分日）：第 4 个值会被前 3 天的高价拖低很多
	cum, err := VWAP(high, low, close, vol)
	if err != nil {
		t.Fatalf("累积 VWAP: %v", err)
	}

	// 按日分组
	day1, err := VWAP(high[:3], low[:3], close[:3], vol[:3])
	if err != nil {
		t.Fatalf("day1 VWAP: %v", err)
	}
	day2, err := VWAP(high[3:], low[3:], close[3:], vol[3:])
	if err != nil {
		t.Fatalf("day2 VWAP: %v", err)
	}

	// day2[0] 是第 2 天第一根，按日重置时它应等于当根的 typical price
	tp := (99.0 + 100.0 + 99.5) / 3.0
	got, _ := strconv.ParseFloat(day2[0], 64)
	if math.Abs(got-tp) > 1e-5 {
		t.Errorf("day2[0] = %v，期望当根 typical price %v（按日重置）", got, tp)
	}

	// 而累积 VWAP 在同位置必然不等于当根 typical price（被前几天拖累）
	cumAt3, _ := strconv.ParseFloat(cum[3], 64)
	if math.Abs(cumAt3-tp) < 1e-3 {
		t.Errorf("累积 VWAP[3] = %v，与当根 typical price %v 几乎相同 —— "+
			"说明累积逻辑没生效，测试数据没有区分度", cumAt3, tp)
	}

	// day1 内部应单调受本组影响，且末值与累积的前 3 个末值相同
	d1last, _ := strconv.ParseFloat(day1[2], 64)
	cum2, _ := strconv.ParseFloat(cum[2], 64)
	if math.Abs(d1last-cum2) > 1e-5 {
		t.Errorf("day1 末值 = %v，累积[2] = %v，第一天两者应相同", d1last, cum2)
	}
}

// TestIndicatorsEdgeCases 覆盖边界条件。
func TestIndicatorsEdgeCases(t *testing.T) {
	t.Run("空输入报错", func(t *testing.T) {
		if _, _, _, err := MACD(nil); err == nil {
			t.Error("MACD(nil) 应报错")
		}
		if _, err := RSI(nil, 14); err == nil {
			t.Error("RSI(nil) 应报错")
		}
	})
	t.Run("周期非法报错", func(t *testing.T) {
		c := []string{"1", "2", "3"}
		if _, err := RSI(c, 0); err == nil {
			t.Error("RSI period=0 应报错")
		}
		if _, err := RSI(c, -1); err == nil {
			t.Error("RSI period=-1 应报错")
		}
	})
	t.Run("长度不一致报错", func(t *testing.T) {
		if _, err := ATR([]string{"1", "2"}, []string{"1"}, []string{"1", "2"}, 14); err == nil {
			t.Error("ATR 长度不一致应报错")
		}
	})
	t.Run("数据不足返回全空", func(t *testing.T) {
		short := make([]string, 5)
		for i := range short {
			short[i] = "100"
		}
		rsi, err := RSI(short, 14)
		if err != nil {
			t.Fatalf("数据不足不应报错: %v", err)
		}
		for i, v := range rsi {
			if v != "" {
				t.Errorf("数据不足时 rsi[%d] = %q，期望空串", i, v)
			}
		}
	})
	t.Run("价格完全不动 RSI=0", func(t *testing.T) {
		// 单边不变：avgGain=avgLoss=0 → TA-Lib 返回 0
		flat := make([]string, 30)
		for i := range flat {
			flat[i] = "100"
		}
		rsi, err := RSI(flat, 14)
		if err != nil {
			t.Fatalf("RSI: %v", err)
		}
		last := rsi[len(rsi)-1]
		if last == "" {
			t.Fatal("最后一位不应为空")
		}
		f, _ := strconv.ParseFloat(last, 64)
		if math.Abs(f-0) > 1e-5 {
			t.Errorf("价格不变时 RSI = %v，期望 0", f)
		}
	})
	t.Run("单边上涨 RSI=100", func(t *testing.T) {
		up := make([]string, 30)
		for i := range up {
			up[i] = strconv.Itoa(100 + i)
		}
		rsi, err := RSI(up, 14)
		if err != nil {
			t.Fatalf("RSI: %v", err)
		}
		f, _ := strconv.ParseFloat(rsi[len(rsi)-1], 64)
		if math.Abs(f-100) > 1e-5 {
			t.Errorf("单边上涨 RSI = %v，期望 100", f)
		}
	})
}
