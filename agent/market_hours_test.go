package agent

import (
	"errors"
	"testing"
	"time"
)

func mustLoc(t *testing.T, n string) *time.Location {
	t.Helper()
	l, err := time.LoadLocation(n)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", n, err)
	}
	return l
}

// ---- 1. A 股 12:00 午休（两段式判定的核心用例） ----

func TestCNLunchBreak(t *testing.T) {
	sh := mustLoc(t, "Asia/Shanghai")
	day := time.Date(2026, 3, 6, 0, 0, 0, 0, sh) // 周五，非节假日

	// 12:00 午休 → SessionAt 必须 ok=false
	lunch := time.Date(2026, 3, 6, 12, 0, 0, 0, sh)
	if _, ok, err := SessionAt(MktCN, lunch); err != nil || ok {
		t.Fatalf("12:00 午休应 ok=false, got ok=%v err=%v", ok, err)
	}

	// 10:00 第一段内 → ok=true
	morning := time.Date(2026, 3, 6, 10, 0, 0, 0, sh)
	s, ok, err := SessionAt(MktCN, morning)
	if err != nil || !ok {
		t.Fatalf("10:00 应在时段内: ok=%v err=%v", ok, err)
	}
	if s.Open.Hour() != 9 || s.Open.Minute() != 30 || s.Close.Hour() != 11 || s.Close.Minute() != 30 {
		t.Fatalf("第一段应为 09:30-11:30, got %v-%v", s.Open, s.Close)
	}

	// 14:00 第二段内 → ok=true
	afternoon := time.Date(2026, 3, 6, 14, 0, 0, 0, sh)
	s2, ok2, err := SessionAt(MktCN, afternoon)
	if err != nil || !ok2 {
		t.Fatalf("14:00 应在时段内: ok=%v err=%v", ok2, err)
	}
	if s2.Open.Hour() != 13 || s2.Close.Hour() != 15 {
		t.Fatalf("第二段应为 13:00-15:00, got %v-%v", s2.Open, s2.Close)
	}

	// SessionOf 只返回第一段（这是刻意的，供不需要判断「当前段」的场景用）
	s3, ok3, err := SessionOf(MktCN, day)
	if err != nil || !ok3 {
		t.Fatalf("SessionOf 应 ok: %v %v", ok3, err)
	}
	if s3.Close.Hour() != 11 {
		t.Fatalf("SessionOf 应返回第一段（11:30 收），got %v", s3.Close)
	}
}

// ---- 2. 美 DST 起始日 ----

func TestUSDST(t *testing.T) {
	ny := mustLoc(t, "America/New_York")

	// 2026-03-08 是美 DST 起始日（3 月第 2 个周日）
	before := time.Date(2026, 3, 6, 10, 0, 0, 0, ny) // 周五，EST（-05:00）
	dstDay := time.Date(2026, 3, 9, 10, 0, 0, 0, ny) // 周一，EDT（-04:00）

	sBefore, ok, err := SessionOf(MktUS, before)
	if err != nil || !ok {
		t.Fatalf("DST 前应 ok: %v %v", ok, err)
	}
	_, offBefore := sBefore.Open.Zone()
	if offBefore != -5*3600 {
		t.Fatalf("DST 前偏移应为 -05:00, got %d", offBefore)
	}

	sAfter, ok, err := SessionOf(MktUS, dstDay)
	if err != nil || !ok {
		t.Fatalf("DST 后应 ok: %v %v", ok, err)
	}
	_, offAfter := sAfter.Open.Zone()
	if offAfter != -4*3600 {
		t.Fatalf("DST 后偏移应为 -04:00, got %d", offAfter)
	}

	// 两个日期的当地 09:30 都必须是 09:30（不是 UTC 计算出的偏移错位）
	if sBefore.Open.Hour() != 9 || sBefore.Open.Minute() != 30 {
		t.Fatalf("DST 前开盘应为当地 09:30, got %v", sBefore.Open)
	}
	if sAfter.Open.Hour() != 9 || sAfter.Open.Minute() != 30 {
		t.Fatalf("DST 后开盘应为当地 09:30, got %v", sAfter.Open)
	}

	// DataCutoffAt 的偏移也要跟着 DST 走
	cutBefore, err := DataCutoffAt(MktUS, before)
	if err != nil {
		t.Fatal(err)
	}
	_, cutOffBefore := cutBefore.Zone()
	if cutOffBefore != -5*3600 || cutBefore.Hour() != 16 {
		t.Fatalf("DST 前 cutoff 应为 16:00 -05:00, got %v (%d)", cutBefore, cutOffBefore)
	}

	cutAfter, err := DataCutoffAt(MktUS, dstDay)
	if err != nil {
		t.Fatal(err)
	}
	_, cutOffAfter := cutAfter.Zone()
	if cutOffAfter != -4*3600 || cutAfter.Hour() != 16 {
		t.Fatalf("DST 后 cutoff 应为 16:00 -04:00, got %v (%d)", cutAfter, cutOffAfter)
	}
}

// ---- 3. AddTradingDays 跨国庆 ----

func TestAddTradingDaysCrossHoliday(t *testing.T) {
	sh := mustLoc(t, "Asia/Shanghai")
	start := time.Date(2026, 9, 30, 0, 0, 0, 0, sh) // 周三，国庆前最后交易日

	// 2026 国庆假期：10/01, 10/02, 10/05, 10/06, 10/07（10/03-04 周末）
	// 交易日序列：9/30 → 10/08 → 10/09 → 10/12 → 10/13 → 10/14
	got, err := AddTradingDays(MktCN, start, 5)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 10, 14, 0, 0, 0, 0, sh)
	if !got.Equal(want) {
		t.Fatalf("跨国庆 T+5 应为 %s（10/01-10/07 休市），got %s",
			want.Format("2006-01-02"), got.Format("2006-01-02"))
	}

	// 关键反例：如果实现用「自然日 +5」，会得到 10/05（仍在假期内）
	naive := start.AddDate(0, 0, 5)
	if got.Equal(naive) {
		t.Fatalf("实现退化成了自然日 +5（%s），未跳过假期", naive.Format("2006-01-02"))
	}

	// T+1 应为 10/08（假期后首个交易日）
	got1, err := AddTradingDays(MktCN, start, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got1.Format("2006-01-02") != "2026-10-08" {
		t.Fatalf("T+1 应为 2026-10-08, got %s", got1.Format("2006-01-02"))
	}

	// T+0：起算日是交易日 → 返回自身
	got0, err := AddTradingDays(MktCN, start, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !got0.Equal(start) {
		t.Fatalf("T+0 应返回自身, got %s", got0.Format("2006-01-02"))
	}

	// 负向：从 10/08 往前 1 个交易日 → 9/30
	back, err := AddTradingDays(MktCN, time.Date(2026, 10, 8, 0, 0, 0, 0, sh), -1)
	if err != nil {
		t.Fatal(err)
	}
	if back.Format("2006-01-02") != "2026-09-30" {
		t.Fatalf("往前 1 交易日应为 2026-09-30, got %s", back.Format("2006-01-02"))
	}
}

// ---- 4. crypto 周六正常开市 ----

func TestCryptoWeekend(t *testing.T) {
	// 2026-03-07 是周六
	sat := time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)

	ok, err := IsTradingDay(MktCrypto, sat)
	if err != nil || !ok {
		t.Fatalf("crypto 周六应开市: ok=%v err=%v", ok, err)
	}

	if _, ok, err := SessionAt(MktCrypto, sat); err != nil || !ok {
		t.Fatalf("crypto 周六应在交易时段内: ok=%v err=%v", ok, err)
	}

	// 任一时刻都命中（7×24）
	for _, h := range []int{0, 6, 12, 18, 23} {
		tm := time.Date(2026, 3, 7, h, 30, 0, 0, time.UTC)
		if _, ok, err := SessionAt(MktCrypto, tm); err != nil || !ok {
			t.Fatalf("crypto %02d:30 应命中: ok=%v err=%v", h, ok, err)
		}
	}

	// 对照：美股周六休市
	ny := mustLoc(t, "America/New_York")
	usSat := time.Date(2026, 3, 7, 12, 0, 0, 0, ny)
	if ok, err := IsTradingDay(MktUS, usSat); err != nil || ok {
		t.Fatalf("美股周六应休市: ok=%v err=%v", ok, err)
	}
}

// ---- 5. 港股半日市 ----

func TestHKHalfDay(t *testing.T) {
	hkt := mustLoc(t, "Asia/Hong_Kong")

	// 2026-12-24 平安夜：半日市，12:00 收，无下午段
	half := time.Date(2026, 12, 24, 0, 0, 0, 0, hkt)

	ok, err := IsTradingDay(MktHK, half)
	if err != nil || !ok {
		t.Fatalf("平安夜应是交易日（半日市）: ok=%v err=%v", ok, err)
	}

	cut, err := DataCutoffAt(MktHK, half)
	if err != nil {
		t.Fatal(err)
	}
	if cut.Hour() != 12 || cut.Minute() != 0 {
		t.Fatalf("半日市 cutoff 应为 12:00, got %v", cut)
	}

	// 11:00 在半日市时段内
	if _, ok, err := SessionAt(MktHK, time.Date(2026, 12, 24, 11, 0, 0, 0, hkt)); err != nil || !ok {
		t.Fatalf("半日市 11:00 应在时段内: ok=%v err=%v", ok, err)
	}

	// 14:00 在半日市应已收盘（常规日这个点还在交易）
	if _, ok, err := SessionAt(MktHK, time.Date(2026, 12, 24, 14, 0, 0, 0, hkt)); err != nil || ok {
		t.Fatalf("半日市 14:00 应已收盘（无下午段）: ok=%v err=%v", ok, err)
	}

	// 对照：常规日 14:00 在交易
	regular := time.Date(2026, 3, 6, 14, 0, 0, 0, hkt)
	if _, ok, err := SessionAt(MktHK, regular); err != nil || !ok {
		t.Fatalf("常规日 14:00 应在时段内: ok=%v err=%v", ok, err)
	}

	// 港股午休 12:30（常规日）
	if _, ok, err := SessionAt(MktHK, time.Date(2026, 3, 6, 12, 30, 0, 0, hkt)); err != nil || ok {
		t.Fatalf("港股 12:30 午休应 ok=false: ok=%v err=%v", ok, err)
	}
}

// ---- 6. 节假日表过期返回 error 而非 false ----

func TestCalendarOutOfRange(t *testing.T) {
	sh := mustLoc(t, "Asia/Shanghai")

	// 2030 超出 2026-2027 覆盖范围 → 必须 error
	far := time.Date(2030, 1, 2, 10, 0, 0, 0, sh)
	ok, err := IsTradingDay(MktCN, far)
	if !errors.Is(err, ErrCalendarOutOfRange) {
		t.Fatalf("超出覆盖年份应返回 ErrCalendarOutOfRange, got ok=%v err=%v", ok, err)
	}
	// 关键：不能伪装成「休市」
	if !ok {
		// ok 应该是 false，但必须是「因 error 而 false」，不是「因休市而 false」。
		// 测试点在于 err 非 nil —— 上面已断言。
		_ = ok
	}

	// 同理 SessionOf / DataCutoffAt / AddTradingDays 都要传播这个 error
	if _, _, err := SessionOf(MktCN, far); !errors.Is(err, ErrCalendarOutOfRange) {
		t.Fatalf("SessionOf 应传播 ErrCalendarOutOfRange, got %v", err)
	}
	if _, err := DataCutoffAt(MktCN, far); !errors.Is(err, ErrCalendarOutOfRange) {
		t.Fatalf("DataCutoffAt 应传播 ErrCalendarOutOfRange, got %v", err)
	}
	if _, err := AddTradingDays(MktCN, far, 1); !errors.Is(err, ErrCalendarOutOfRange) {
		t.Fatalf("AddTradingDays 应传播 ErrCalendarOutOfRange, got %v", err)
	}

	// crypto 无表 → 2030 也正常
	if ok, err := IsTradingDay(MktCrypto, time.Date(2030, 1, 2, 0, 0, 0, 0, time.UTC)); err != nil || !ok {
		t.Fatalf("crypto 不应有范围限制: ok=%v err=%v", ok, err)
	}

	// 未知市场 → ErrUnknownMarket（不是 ErrCalendarOutOfRange）
	if _, err := IsTradingDay("jp", far); !errors.Is(err, ErrUnknownMarket) {
		t.Fatalf("未知市场应返回 ErrUnknownMarket, got %v", err)
	}
}

// ---- 7. benchmark 四映射 ----

func TestBenchmarks(t *testing.T) {
	cases := []struct{ market, sym, cls string }{
		{MktUS, "SPY", "etf"},
		{MktHK, "HSI", "index"},
		{MktCN, "000300.SH", "index"}, // 必须带 .SH 后缀
		{MktCrypto, "BTC-USDT", "crypto"},
	}
	for _, c := range cases {
		sym, cls, err := BenchmarkOf(c.market)
		if err != nil {
			t.Fatalf("%s: %v", c.market, err)
		}
		if sym != c.sym || cls != c.cls {
			t.Fatalf("%s → (%s,%s), want (%s,%s)", c.market, sym, cls, c.sym, c.cls)
		}
	}
	if _, _, err := BenchmarkOf("jp"); !errors.Is(err, ErrUnknownMarket) {
		t.Fatalf("未知市场应 error, got %v", err)
	}
}

// ---- 8. 非交易日与 LastClosedTradingDay ----

func TestNonTradingDay(t *testing.T) {
	sh := mustLoc(t, "Asia/Shanghai")

	// 2026-10-01 国庆 → 休市
	ok, err := IsTradingDay(MktCN, time.Date(2026, 10, 1, 10, 0, 0, 0, sh))
	if err != nil || ok {
		t.Fatalf("国庆应休市: ok=%v err=%v", ok, err)
	}
	// 非交易日 DataCutoffAt → ErrNotTradingDay
	if _, err := DataCutoffAt(MktCN, time.Date(2026, 10, 1, 10, 0, 0, 0, sh)); !errors.Is(err, ErrNotTradingDay) {
		t.Fatalf("非交易日应返回 ErrNotTradingDay, got %v", err)
	}

	// LastClosedTradingDay：10/03（周六，假期内）→ 9/30
	got, err := LastClosedTradingDay(MktCN, time.Date(2026, 10, 3, 12, 0, 0, 0, sh))
	if err != nil {
		t.Fatal(err)
	}
	if got.Format("2006-01-02") != "2026-09-30" {
		t.Fatalf("10/03 的最近已收盘交易日应为 2026-09-30, got %s", got.Format("2006-01-02"))
	}

	// 交易日但未收盘：10/09 10:00（盘中）→ 10/08
	got2, err := LastClosedTradingDay(MktCN, time.Date(2026, 10, 9, 10, 0, 0, 0, sh))
	if err != nil {
		t.Fatal(err)
	}
	if got2.Format("2006-01-02") != "2026-10-08" {
		t.Fatalf("盘中应回溯到上一交易日 2026-10-08, got %s", got2.Format("2006-01-02"))
	}

	// 交易日已收盘：10/09 16:00 → 10/09 当天
	got3, err := LastClosedTradingDay(MktCN, time.Date(2026, 10, 9, 16, 0, 0, 0, sh))
	if err != nil {
		t.Fatal(err)
	}
	if got3.Format("2006-01-02") != "2026-10-09" {
		t.Fatalf("收盘后应返回当天, got %s", got3.Format("2006-01-02"))
	}
}

// ---- 9. Session 左闭右开 ----

func TestSessionHalfOpen(t *testing.T) {
	sh := mustLoc(t, "Asia/Shanghai")

	// 09:30:00 整 → 在时段内（左闭）
	if _, ok, err := SessionAt(MktCN, time.Date(2026, 3, 6, 9, 30, 0, 0, sh)); err != nil || !ok {
		t.Fatalf("09:30:00 应命中（左闭）: ok=%v err=%v", ok, err)
	}
	// 09:29:59 → 不在
	if _, ok, err := SessionAt(MktCN, time.Date(2026, 3, 6, 9, 29, 59, 0, sh)); err != nil || ok {
		t.Fatalf("09:29:59 不应命中: ok=%v err=%v", ok, err)
	}
	// 11:30:00 整 → 不在（右开）
	if _, ok, err := SessionAt(MktCN, time.Date(2026, 3, 6, 11, 30, 0, 0, sh)); err != nil || ok {
		t.Fatalf("11:30:00 不应命中（右开）: ok=%v err=%v", ok, err)
	}
	// 15:00:00 整 → 不在（右开，收盘后）
	if _, ok, err := SessionAt(MktCN, time.Date(2026, 3, 6, 15, 0, 0, 0, sh)); err != nil || ok {
		t.Fatalf("15:00:00 不应命中（右开）: ok=%v err=%v", ok, err)
	}
}

func TestExtraUSHalfDayAndDSTEnd(t *testing.T) {
	ny := mustLoc(t, "America/New_York")
	// 2026-11-27 感恩节次日：半日市 13:00 收
	d := time.Date(2026, 11, 27, 0, 0, 0, 0, ny)
	cut, err := DataCutoffAt(MktUS, d)
	if err != nil {
		t.Fatal(err)
	}
	if cut.Hour() != 13 {
		t.Fatalf("感恩节次日半日市 cutoff 应为 13:00, got %v", cut)
	}
	// 2026-11-02 是 DST 结束后（11月第1个周日是 11/01）
	dst := time.Date(2026, 11, 2, 10, 0, 0, 0, ny)
	s, ok, err := SessionOf(MktUS, dst)
	if err != nil || !ok {
		t.Fatalf("DST 回拨后应 ok: %v %v", ok, err)
	}
	_, off := s.Open.Zone()
	if off != -5*3600 {
		t.Fatalf("回拨后应为 -05:00, got %d", off)
	}
}

func TestExtraHKTwoSegments(t *testing.T) {
	hkt := mustLoc(t, "Asia/Hong_Kong")
	// 16:00 整收盘后（右开）
	if _, ok, err := SessionAt(MktHK, time.Date(2026, 3, 6, 16, 0, 0, 0, hkt)); err != nil || ok {
		t.Fatalf("16:00 应已收盘: ok=%v err=%v", ok, err)
	}
	// 12:00 整（午休开始，右开）
	if _, ok, err := SessionAt(MktHK, time.Date(2026, 3, 6, 12, 0, 0, 0, hkt)); err != nil || ok {
		t.Fatalf("12:00 应午休: ok=%v err=%v", ok, err)
	}
	// 13:00 整（下午开盘，左闭）
	if _, ok, err := SessionAt(MktHK, time.Date(2026, 3, 6, 13, 0, 0, 0, hkt)); err != nil || !ok {
		t.Fatalf("13:00 应在时段内: ok=%v err=%v", ok, err)
	}
}

func TestExtraCryptoAddTradingDays(t *testing.T) {
	// crypto T+5 = 5 个交易日 = 5 天（无假日）
	base := time.Date(2026, 3, 6, 0, 0, 0, 0, time.UTC)
	got, err := AddTradingDays(MktCrypto, base, 5)
	if err != nil {
		t.Fatal(err)
	}
	if got.Format("2006-01-02") != "2026-03-11" {
		t.Fatalf("crypto T+5 应为 2026-03-11, got %s", got.Format("2006-01-02"))
	}
}

func TestExtraUSTradingDayAfterDST(t *testing.T) {
	ny := mustLoc(t, "America/New_York")
	// 2026-03-09（周一，DST 后第一天）应为交易日
	ok, err := IsTradingDay(MktUS, time.Date(2026, 3, 9, 10, 0, 0, 0, ny))
	if err != nil || !ok {
		t.Fatalf("2026-03-09 应为交易日: ok=%v err=%v", ok, err)
	}
	// 2026-04-03 Good Friday 休市
	ok2, err := IsTradingDay(MktUS, time.Date(2026, 4, 3, 10, 0, 0, 0, ny))
	if err != nil || ok2 {
		t.Fatalf("Good Friday 应休市: ok=%v err=%v", ok2, err)
	}
}
