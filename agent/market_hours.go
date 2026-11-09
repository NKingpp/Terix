// Package agent — 交易日历（四市场）。
//
// 设计取向：P0 只有 4 个市场，节假日表硬编码两年（2026–2027）放 Go 常量，
// **不建 DB 表、不引第三方日历库**。理由见《ARCH-W2-W5》§2.9.4（docs 仓库：quantex/ARCH-W2-W5.md）：
//  1. 交易日历的权威源是交易所公告；任何库都是二手转抄且会滞后。
//  2. Go 常量可 code review、可单测、不依赖 PG —— 与「阶段图模板放代码常量」同一原则。
//  3. 硬编码的代价（年度维护）在单用户本机系统上极低；库带来的代价（库说开市、
//     交易所说休市，你无从判断该信谁）无法承受。
//
// 表过期一律返回 error（ErrCalendarOutOfRange），**不返回 false**。
// 返回 false 会让系统静默地「今天休市，不生成任务」，而真相是「日历表过期了」——
// 与 SOURCE_UNAVAILABLE / DATA_SNAPSHOT_MISSING 的区分是同一原则。
package agent

import (
	"errors"
	"fmt"
	"time"

	// Windows 上若无 %GOROOT%/lib/time/zoneinfo.zip，LoadLocation 会失败。
	// 内嵌 IANA 时区库（stdlib，约 450KB）保证任何机器上都能解析 America/New_York。
	// 代价：二进制 +450KB。收益：不依赖运行时 tzdata。
	_ "time/tzdata"
)

// 四个市场标识。与 instruments.market 的取值一致。
const (
	MktUS     = "us"
	MktHK     = "hk"
	MktCN     = "cn"
	MktCrypto = "crypto"
)

var (
	// ErrUnknownMarket 未知市场。不返回「默认市场」—— 错市场比无市场更危险
	// （会用一个市场的日历去判另一个市场的交易时段）。
	ErrUnknownMarket = errors.New("market_hours: unknown market")
	// ErrCalendarOutOfRange 查询日期超出节假日表覆盖年份。
	// 刻意与 ErrUnknownMarket 分开：前者是「表该更新了」，后者是「参数错了」。
	ErrCalendarOutOfRange = errors.New("market_hours: calendar out of range, holiday table needs update")
	// ErrNotTradingDay 非交易日。仅在需要「某交易日的收盘时刻」时返回。
	ErrNotTradingDay = errors.New("market_hours: not a trading day")
)

// Session 是一个交易时段，左闭右开 [Open, Close)。
// 左闭右开的取舍：收盘瞬间（16:00:00.000）不能再下单，开盘瞬间可以。
// 这个语义必须固定，否则「收盘前一秒」的订单在两次求值中结论会不同。
type Session struct {
	Open  time.Time
	Close time.Time
}

// clockSpan 是当地时钟上的时段（不含日期），用于构造 Session。
type clockSpan struct{ oh, om, ch, cm int }

// marketSpec 是一个市场的完整日历定义。
type marketSpec struct {
	locName string
	loc     *time.Location

	regular []clockSpan // 常规交易日的时段；cn/hk 为两段
	halfDay []clockSpan // 半日市的时段；空表示该市场无半日市

	// weekend[w] == true 表示该 weekday 休市。crypto 全 false（7×24）。
	//
	// ⚠ 已知偏差（W3-2 待修）：调休补班日会被误判为休市。
	//
	// 现状：周六周日一律判休市，没有「周末补班开市」的表。
	//
	// 受影响的市场：**只有 cn**。hk / us 无调休制度，周末补班不存在；
	// crypto 无周末。所以这个偏差不影响 hk/us/crypto 三路。
	//
	// 具体错法（以 A 股为例）：
	//   1. 春节 / 国庆前，国务院常把某个周六调成工作日补班。本实现判该日为
	//      休市 → **这个真实交易日被静默吞掉**。
	//   2. 连带影响 AddTradingDays：少算一天会让 signal_settlements 的
	//      settle_date 整体错位，PnL 归因随之偏移。这比「少一个交易日」
	//      本身严重 —— 错的是结算基准日。
	//   3. 反向情形（工作日被临时调成休息，如 2020 年疫情延期开市）同样错：
	//      那天既非周末也不在 holidays 里 → 被判成开市，需要 specialClosedDays 才能修。
	//
	// 修正方式：给 marketSpec 增加
	//   specialOpenDays   map[string]struct{}  // 周末补班 → 开市
	//   specialClosedDays map[string]struct{}  // 工作日临时休市
	// 并让 IsTradingDay 按此优先级判定：
	//   specialOpenDays > specialClosedDays > weekend > holidays
	//
	// 为什么现在不改：补班日期由国务院办公厅逐年公告，无法凭空填入；
	// 只加空字段等于假装修好。登记为 W3-2，拿到 2026/2027 具体补班日后
	// 一次性加字段 + 填表 + 测试（TestCNMakeupWorkday）。
	weekend [7]bool

	holidays map[string]struct{} // "2006-01-02" → 休市
	halfDays map[string]struct{} // "2006-01-02" → 半日市

	// 节假日表覆盖年份闭区间。yearFrom == 0 表示无表（crypto），不做范围校验。
	yearFrom, yearTo int
}

func mk(y, m, d int) string { return fmt.Sprintf("%04d-%02d-%02d", y, m, d) }

func set(dst map[string]struct{}, days ...string) {
	for _, d := range days {
		dst[d] = struct{}{}
	}
}

// specs 是市场的注册表，init 时构建（LoadLocation 失败则 panic —— 启动期失败
// 好过运行期静默降级到 UTC）。
var specs = map[string]*marketSpec{}

func init() {
	loc := func(name string) *time.Location {
		l, err := time.LoadLocation(name)
		if err != nil {
			// 内嵌了 time/tzdata，这里不该失败。失败说明二进制缺 tzdata 或被篡改。
			panic("market_hours: LoadLocation(" + name + "): " + err.Error())
		}
		return l
	}

	// ---------------- us：09:30–16:00 ET，DST 由 time.Location 自动处理 ----------------
	us := &marketSpec{
		locName: "America/New_York",
		loc:     loc("America/New_York"),
		regular: []clockSpan{{9, 30, 16, 0}},
		// NYSE 半日市：13:00 收（感恩节次日、平安夜、7/3 等）
		halfDay:  []clockSpan{{9, 30, 13, 0}},
		weekend:  [7]bool{time.Sunday: true, time.Saturday: true},
		holidays: map[string]struct{}{},
		halfDays: map[string]struct{}{},
		yearFrom: 2026, yearTo: 2027,
	}
	// 2026（NYSE）。⚠️ 上线前必须对照交易所公告核实。
	set(us.holidays,
		"2026-01-01", // New Year's Day
		"2026-01-19", // MLK Day
		"2026-02-16", // Presidents' Day
		"2026-04-03", // Good Friday
		"2026-05-25", // Memorial Day
		"2026-06-19", // Juneteenth
		"2026-07-03", // Independence Day observed（7/4 为周六）
		"2026-09-07", // Labor Day
		"2026-11-26", // Thanksgiving
		"2026-12-25", // Christmas
	)
	set(us.halfDays, "2026-11-27", "2026-12-24")
	// 2027
	set(us.holidays,
		"2027-01-01", // New Year's Day
		"2027-01-18", // MLK Day
		"2027-02-15", // Presidents' Day
		"2027-03-26", // Good Friday
		"2027-05-31", // Memorial Day
		"2027-06-18", // Juneteenth observed（6/19 为周六）
		"2027-07-05", // Independence Day observed（7/4 为周日）
		"2027-09-06", // Labor Day
		"2027-11-25", // Thanksgiving
		"2027-12-24", // Christmas observed（12/25 为周六）
	)
	set(us.halfDays, "2027-11-26")
	specs[MktUS] = us

	// ---------------- hk：09:30–12:00 + 13:00–16:00 HKT，有半日市 ----------------
	hk := &marketSpec{
		locName: "Asia/Hong_Kong",
		loc:     loc("Asia/Hong_Kong"),
		regular: []clockSpan{{9, 30, 12, 0}, {13, 0, 16, 0}},
		// 港股半日市：平安夜 / 除夕 / 农历新年前夕，12:00 收，无下午段
		halfDay:  []clockSpan{{9, 30, 12, 0}},
		weekend:  [7]bool{time.Sunday: true, time.Saturday: true},
		holidays: map[string]struct{}{},
		halfDays: map[string]struct{}{},
		yearFrom: 2026, yearTo: 2027,
	}
	// 2026（HKEX）。⚠️ 农历节日与佛诞/重阳等按港交所公告核实。
	set(hk.holidays,
		"2026-01-01", // 元旦
		"2026-02-17", // 农历年初一
		"2026-02-18", // 农历年初二
		"2026-02-19", // 农历年初三
		"2026-04-03", // 耶稣受难节
		"2026-04-04", // 耶稣受难节翌日
		"2026-04-06", // 复活节星期一
		"2026-05-01", // 劳动节
		"2026-05-25", // 佛诞翌日（5/24 为周日）
		"2026-06-19", // 端午节
		"2026-07-01", // 香港特别行政区成立纪念日
		"2026-10-01", // 国庆日
		"2026-10-19", // 重阳节翌日（10/18 为周日）
		"2026-12-25", // 圣诞节
		"2026-12-26", // 圣诞节后第一个周日（周六）
	)
	set(hk.halfDays, "2026-12-24", "2026-12-31")
	// 2027
	set(hk.holidays,
		"2027-01-01", // 元旦
		"2027-02-06", // 农历年初一
		"2027-02-08", // 农历年初三（2/7 为周日，年初二顺延）
		"2027-03-26", // 耶稣受难节
		"2027-03-27", // 耶稣受难节翌日
		"2027-03-29", // 复活节星期一
		"2027-05-01", // 劳动节
		"2027-05-13", // 佛诞
		"2027-06-09", // 端午节
		"2027-07-01", // 特区成立纪念日
		"2027-10-01", // 国庆日
		"2027-10-08", // 重阳节
		"2027-12-25", // 圣诞节
	)
	set(hk.halfDays, "2027-12-24", "2027-12-31")
	specs[MktHK] = hk

	// ---------------- cn：09:30–11:30 + 13:00–15:00 CST，无半日市 ----------------
	cn := &marketSpec{
		locName: "Asia/Shanghai",
		loc:     loc("Asia/Shanghai"),
		regular: []clockSpan{{9, 30, 11, 30}, {13, 0, 15, 0}},
		// A 股无半日市（除夕若开市也是全日，实际近年除夕均休市）
		halfDay:  nil,
		weekend:  [7]bool{time.Sunday: true, time.Saturday: true},
		holidays: map[string]struct{}{},
		halfDays: map[string]struct{}{},
		yearFrom: 2026, yearTo: 2027,
	}
	// 2026（SSE/SZSE）。⚠️ 春节/国庆的调休安排每年由国务院办公厅发布，必须核。
	set(cn.holidays,
		"2026-01-01", "2026-01-02", // 元旦
		"2026-02-16", "2026-02-17", "2026-02-18", "2026-02-19", "2026-02-20",
		"2026-02-23", "2026-02-24", // 春节
		"2026-04-06",                             // 清明
		"2026-05-01", "2026-05-04", "2026-05-05", // 劳动节
		"2026-06-19",                                                         // 端午
		"2026-09-25",                                                         // 中秋
		"2026-10-01", "2026-10-02", "2026-10-05", "2026-10-06", "2026-10-07", // 国庆
	)
	// 2027
	set(cn.holidays,
		"2027-01-01", // 元旦
		"2027-02-05", "2027-02-08", "2027-02-09", "2027-02-10", "2027-02-11",
		"2027-02-12",                             // 春节
		"2027-04-05",                             // 清明
		"2027-05-03", "2027-05-04", "2027-05-05", // 劳动节
		"2027-06-09",                                                         // 端午
		"2027-09-15",                                                         // 中秋
		"2027-10-01", "2027-10-04", "2027-10-05", "2027-10-06", "2027-10-07", // 国庆
	)
	specs[MktCN] = cn

	// ---------------- crypto：7×24 UTC，无假日表 ----------------
	specs[MktCrypto] = &marketSpec{
		locName: "UTC",
		loc:     time.UTC,
		// 24 小时窗口。刻意用 0→24 而不是 0→0：跨午夜窗口在 inWindow 里
		// 会被展开成次日 00:00，语义等价且更好读。
		regular:  []clockSpan{{0, 0, 24, 0}},
		halfDay:  nil,
		weekend:  [7]bool{}, // 全 false：无周末
		holidays: map[string]struct{}{},
		halfDays: map[string]struct{}{},
		yearFrom: 0, yearTo: 0, // 无表，不做范围校验
	}
}

func specOf(market string) (*marketSpec, error) {
	s, ok := specs[market]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownMarket, market)
	}
	return s, nil
}

// LocationOf 返回该市场的交易时区。调用方需要自己构造当地时钟时使用。
func LocationOf(market string) (*time.Location, error) {
	s, err := specOf(market)
	if err != nil {
		return nil, err
	}
	return s.loc, nil
}

// checkRange 校验查询日期是否在节假日表覆盖年份内。
// crypto（yearFrom == 0）跳过校验；其余市场越界返回 ErrCalendarOutOfRange。
func (s *marketSpec) checkRange(d time.Time) error {
	if s.yearFrom == 0 {
		return nil
	}
	// 用当地日期的年份，不是 UTC 年份 —— 跨年边界（如 12/31 20:00 ET）
	// 用 UTC 年份会判错一天。
	y := d.In(s.loc).Year()
	if y < s.yearFrom || y > s.yearTo {
		return fmt.Errorf("%w: market=%s date=%s covered=%d..%d",
			ErrCalendarOutOfRange, s.locName, d.In(s.loc).Format("2006-01-02"), s.yearFrom, s.yearTo)
	}
	return nil
}

// spansFor 返回该当地日期使用的时段。isHalf 表示是否半日市。
func (s *marketSpec) spansFor(d time.Time) ([]clockSpan, bool) {
	key := d.In(s.loc).Format("2006-01-02")
	if _, ok := s.halfDays[key]; ok && len(s.halfDay) > 0 {
		return s.halfDay, true
	}
	return s.regular, false
}

// sessionsFor 把 clockSpan 展开成带日期的 Session 列表。不判断是否交易日。
func (s *marketSpec) sessionsFor(d time.Time) []Session {
	local := d.In(s.loc)
	y, m, dd := local.Date()
	spans, _ := s.spansFor(d)
	out := make([]Session, 0, len(spans))
	for _, sp := range spans {
		open := time.Date(y, m, dd, sp.oh, sp.om, 0, 0, s.loc)
		// Close 用 AddDate 而非 Add(time.Hour) —— crypto 的 24:00 若写 0,0 会变成
		// 当天 00:00（close == open，区间为空）；用小时数偏移更直观。
		close := open.Add(time.Duration((sp.ch-sp.oh)*60+(sp.cm-sp.om)) * time.Minute)
		out = append(out, Session{Open: open, Close: close})
	}
	return out
}

// IsTradingDay 判断当地日期是否交易日。
//
// 注意签名与 §2.1 草案的差异：草案写的是 `bool`，这里加了 `error`。
// 理由：表过期必须能被调用方区分出来（返回 false 会让「日历过期」伪装成「休市」）。
// 已知会返回 error 的情况：未知市场、表过期。
func IsTradingDay(market string, d time.Time) (bool, error) {
	s, err := specOf(market)
	if err != nil {
		return false, err
	}
	if err := s.checkRange(d); err != nil {
		return false, err
	}
	local := d.In(s.loc)
	if s.weekend[local.Weekday()] {
		return false, nil
	}
	key := local.Format("2006-01-02")
	if _, off := s.holidays[key]; off {
		return false, nil
	}
	return true, nil
}

// SessionOf 返回该日首个时段。非交易日 ok=false。
// cn/hk 有两个时段，这里只返回第一段 —— 需要「当前处于哪一段」用 SessionAt。
func SessionOf(market string, d time.Time) (Session, bool, error) {
	s, err := specOf(market)
	if err != nil {
		return Session{}, false, err
	}
	ok, err := IsTradingDay(market, d)
	if err != nil {
		return Session{}, false, err
	}
	if !ok {
		return Session{}, false, nil
	}
	sess := s.sessionsFor(d)
	if len(sess) == 0 {
		return Session{}, false, nil
	}
	return sess[0], true, nil
}

// SessionAt 返回 t 所处的那一段时段。t 落在午休/收盘后/非交易日 → ok=false。
//
// 这是风控规则 3（交易时段）必须用的函数。用 SessionOf 判定会让 A 股 12:00
// （午休）被判为「在 09:30–15:00 内 → 可交易」—— 这是两段式市场的核心陷阱。
func SessionAt(market string, t time.Time) (Session, bool, error) {
	s, err := specOf(market)
	if err != nil {
		return Session{}, false, err
	}
	ok, err := IsTradingDay(market, t)
	if err != nil {
		return Session{}, false, err
	}
	if !ok {
		return Session{}, false, nil
	}
	// crypto 的时段可能是 24h，t 在任何时刻都应命中；但跨午夜窗口的 Close
	// 落在次日，需要同时检查「当天的段」与「昨天的段」。
	for _, cand := range []time.Time{t, t.AddDate(0, 0, -1)} {
		for _, sess := range s.sessionsFor(cand) {
			if !t.Before(sess.Open) && t.Before(sess.Close) {
				return sess, true, nil
			}
		}
	}
	return Session{}, false, nil
}

// DataCutoffAt 返回某交易日的数据截止时刻（= 该日最后一个时段的收盘）。
//
// 这是 point-in-time 校验与结算取价的唯一口径：日线 trading_date=D 的
// data_cutoff_at 是 D 日交易所收盘时刻，不是 D 日 00:00。
// 用 D 日 00:00 会让「当天开盘前就拿到了当天收盘价」成为可能（look-ahead）。
func DataCutoffAt(market string, tradingDate time.Time) (time.Time, error) {
	s, err := specOf(market)
	if err != nil {
		return time.Time{}, err
	}
	ok, err := IsTradingDay(market, tradingDate)
	if err != nil {
		return time.Time{}, err
	}
	if !ok {
		return time.Time{}, fmt.Errorf("%w: market=%s date=%s",
			ErrNotTradingDay, market, tradingDate.In(s.loc).Format("2006-01-02"))
	}
	sess := s.sessionsFor(tradingDate)
	if len(sess) == 0 {
		return time.Time{}, fmt.Errorf("%w: market=%s no session", ErrNotTradingDay, market)
	}
	return sess[len(sess)-1].Close, nil
}

// AddTradingDays 从 from 起数 n 个交易日之后那一天。
//
// 语义：
//   - n == 0：返回 from 若是交易日，否则返回下一个交易日。
//   - n > 0：往后数 n 个交易日（不含 from 本身）。
//   - n < 0：往前数（P1 结算回看用）。
//
// 返回的日期时间为该日当地 00:00（日期语义，不是时刻语义）。
// 越出节假日表覆盖范围返回 ErrCalendarOutOfRange —— 不静默返回零值。
func AddTradingDays(market string, from time.Time, n int) (time.Time, error) {
	s, err := specOf(market)
	if err != nil {
		return time.Time{}, err
	}
	local := from.In(s.loc)
	cur := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, s.loc)

	step := 1
	remaining := n
	if n < 0 {
		step = -1
		remaining = -n
	} else if n == 0 {
		// n=0：若 from 是交易日就返回它，否则找下一个交易日
		for i := 0; i < 400; i++ {
			ok, err := IsTradingDay(market, cur)
			if err != nil {
				return time.Time{}, err
			}
			if ok {
				return cur, nil
			}
			cur = cur.AddDate(0, 0, 1)
		}
		return time.Time{}, fmt.Errorf("%w: market=%s no trading day found within 400 days", ErrCalendarOutOfRange, market)
	}

	for i := 0; i < 400; i++ {
		cur = cur.AddDate(0, 0, step)
		ok, err := IsTradingDay(market, cur)
		if err != nil {
			return time.Time{}, err
		}
		if !ok {
			continue
		}
		remaining--
		if remaining == 0 {
			return cur, nil
		}
	}
	return time.Time{}, fmt.Errorf("%w: market=%s walk exceeded 400 days", ErrCalendarOutOfRange, market)
}

// LastClosedTradingDay 返回 asOf 时刻「已收盘」的最近交易日。
//
// 用途：手动创建任务时 as_of 落在非交易日或当日未收盘，需要规整到一个确定的
// 「已知数据上界」的交易日（§2.3.1）。
//   - asOf 是交易日且已过收盘 → 返回当天
//   - asOf 是交易日但未收盘   → 返回上一个交易日
//   - asOf 非交易日           → 返回上一个交易日
func LastClosedTradingDay(market string, asOf time.Time) (time.Time, error) {
	s, err := specOf(market)
	if err != nil {
		return time.Time{}, err
	}
	// 先看当天能不能收盘
	cut, err := DataCutoffAt(market, asOf)
	if err == nil && !asOf.Before(cut) {
		local := asOf.In(s.loc)
		return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, s.loc), nil
	}
	// 非交易日或未收盘：从当天往前找
	local := asOf.In(s.loc)
	cur := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, s.loc)
	for i := 0; i < 400; i++ {
		cur = cur.AddDate(0, 0, -1)
		ok, err := IsTradingDay(market, cur)
		if err != nil {
			return time.Time{}, err
		}
		if ok {
			return cur, nil
		}
	}
	return time.Time{}, fmt.Errorf("%w: market=%s no closed trading day within 400 days", ErrCalendarOutOfRange, market)
}

// ---------------- benchmark 四映射 ----------------

// Benchmarks：市场 → 基准 symbol。
// cn 必须是 000300.SH（不是 000300）—— 各数据通道对指数后缀要求不同，
// 后缀写错的行为是「查不到」，会静默落 DATA_SNAPSHOT_MISSING。
// 通道侧的格式转换放 symbol_norm.go，这里只给规范形式。
var Benchmarks = map[string]struct {
	Symbol     string
	AssetClass string
}{
	MktUS:     {"SPY", "etf"},
	MktHK:     {"HSI", "index"},
	MktCN:     {"000300.SH", "index"},
	MktCrypto: {"BTC-USDT", "crypto"},
}

// BenchmarkOf 返回该市场的基准 symbol 与 benchmark 自身的 asset_class。
// 未知 market 返回 error，**不返回默认值** —— 错基准会让 alpha 系统性偏移。
func BenchmarkOf(market string) (symbol string, assetClass string, err error) {
	b, ok := Benchmarks[market]
	if !ok {
		return "", "", fmt.Errorf("benchmark: %w: %q", ErrUnknownMarket, market)
	}
	return b.Symbol, b.AssetClass, nil
}
