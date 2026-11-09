package intercept

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"
)

// =====================================================================
// Subject:规则求值的统一输入
// =====================================================================
//
// 三条路径(tool_name / tool_input / draft_field)各自只填自己那部分,
// matcher 只读自己关心的字段。
//
// 为什么不为每条路径拆一个求值函数:拆了就有三套求值循环,而
// 「规则优先级 + 三态聚合(任一 deny = deny;有 ask 无 deny = ask;全 pass = pass)
// + 超时兜底」必须永远一致。三份实现必然漂移。
type Subject struct {
	// ---- 工具路径(现有 Match 使用) ----
	ToolName  string
	ToolInput []byte

	// ---- 草案路径(EvaluateDraft 使用) ----
	Draft *DraftSubject

	// ---- 两条路径共用 ----
	// Now 由 clock 注入,禁止 matcher 内直接 time.Now()。
	Now time.Time
	// Loc 由调用方按 Draft.Market 解析后传入。
	// 为 nil 时 time_window / daily_count 必须返回错误,禁止默认 UTC ——
	// crypto 用 UTC 是显式配置,不是缺失时的兜底。
	Loc *time.Location
	// Count 是 daily_count 的计数来源。为 nil 时 daily_count 返回 ErrNoCount。
	// 用函数注入而不是直接传 *db.DB:intercept 包不 import db 的 DB 方法,
	// 否则单测必须起 PG。
	Count CountFunc
	// Session 是 time_window(source='market_session') 的时段来源。
	// 同样用函数注入,让 intercept 包不依赖 agent 包(避免 import 环:
	// agent 已经 import intercept,反向 import 必然成环)。
	Session SessionFunc
}

// DraftSubject 是订单草案在求值时的快照。字段全部是「matcher 会读到的」,
// 不多不少。不在这里塞 *db.OrderDraft —— 那会让 intercept 包绑死在 db 的表结构上。
type DraftSubject struct {
	DraftID      int64  // orders_draft.id,用于审计回填与 daily_count 排除自身
	InstrumentID int64  // daily_count 按此分组
	Symbol       string // 白名单判定;大小写归一在 matcher 里做
	Market       string // us / hk / cn / crypto;决定时区与时段来源
	AssetClass   string // stock / etf / index / crypto
	Side         string // buy / sell
	Qty          string // NUMERIC(20,8) 的字符串形式,来自 orders_draft.qty
	RefPrice     string // NUMERIC(20,8) 的字符串形式;可为空(市价单)
	Notional     string // = Qty × RefPrice,由调用方算好;空串时 matcher 兜底自算
	Mode         string // paper / live_whitelist / live_manual
}

// CountQuery 是 daily_count 的查询条件。matcher 只负责算边界(「今日」按交易所时区),
// 真正的 SQL 在 db 层(避免 intercept 包写 SQL)。
type CountQuery struct {
	InstrumentID int64
	Symbol       string
	Market       string
	SinceIncl    time.Time // 含:当地 00:00:00
	UntilExcl    time.Time // 不含:次日 00:00:00(DST 日可能是 23 或 25 小时后)
	ExcludeSuper bool      // true = 不含 superseded_by IS NOT NULL 的行
}

// CountFunc 是 daily_count 的计数注入点。
type CountFunc func(ctx context.Context, q CountQuery) (int64, error)

// Session 是 [Open, Close) 左闭右开。cn/hk 返回两段(午休)。
type Session struct{ Open, Close time.Time }

// SessionFunc 由 agent/market_hours.go 的 SessionAt 提供。
type SessionFunc func(market string, d time.Time, loc *time.Location) ([]Session, error)

// =====================================================================
// 结构化 matcher 分派
// =====================================================================
//
// 背景(G12):改造前 ruleMatches 是
//
//	if r.MatchType == "regex" { ... }
//	return strings.Contains(subject, r.Pattern)   // ← 所有未知类型落这里
//
// 于是新增 numeric_lte / list / time_window / daily_count 后,它们会被静默
// 按 "string" 处理:编译通过、不报错、单测测 matcher 函数也全绿,但集成路径
// 根本没被调用。这是最危险的一类缺陷 —— 风控"看起来在工作"。
//
// 现在改为显式 switch,未知类型必须进 matchStructured,而 matchStructured
// 对未实现的类型返回 error(不是 false)。error 的语义是「无法判定」,
// 由上层决定 fail-open 还是 fail-closed:
//   - 工具路径(Match):fail-open,当作未命中(存量行为,不能改)
//   - 草案路径(EvaluateDraft):fail-closed,一律 deny

// matchResult 是一次 matcher 求值的结果。
// 之所以不用裸 bool:needle 型 matcher 将来要带出"命中了哪一项"用于风控解释。
// matchResult 是一次 matcher 求值的结果。
//
// 返回 `(matchResult{Matched:true}, nil)` 与 `(_, error)` 的分界(必须严格遵守):
//
//   - **方向明确是「拦」** → 用 `Matched:true` + 中文 Reason。
//     例:名义金额算不出、白名单为空、计数查询失败。这些场景判定结论唯一,
//     带 reason 能让审计看到「为什么拦」,比 error 有用得多。
//   - **不知道是该拦还是该放** → 必须返 error,由上层 fail-closed。
//     例:max 缺失/非法、field 不支持、Loc 为 nil、Session 为 nil。
//
// 错用第一种会把「求不出结论」伪装成「已判定」;错用第二种会让审计只剩
// 「求值失败」四个字,事后复盘无从下手。
type matchResult struct {
	Matched bool
	// Reason 供风控语料/审计展示,如 "名义金额 12000 超过上限 10000"。
	// 空字符串表示无需解释。
	Reason string
}

// errMatcherNotImplemented 表示该 match_type 的分派已知但实现未接上。
// 这是「无法判定」,不是「未命中」—— 上层必须按 fail-closed 处理草案路径。
var errMatcherNotImplemented = errors.New("matcher 尚未实现")

// errUnknownMatchTarget 表示规则的 match_target 不在已知集合内。
//
// 这是「无法判定」而非「未命中」。改造前 ruleMatches 的 matchTarget switch
// 兜底是 `return matchResult{}, nil`,于是未知目标 = 静默放行。将来加
// match_target='draft_field' 之类的新目标时若忘了加 case,该规则会静默失效 ——
// 不报错、不命中,等于这条风控规则不存在。风控规则被设计来拦某种单,
// 静默失效比报错危险得多。
var errUnknownMatchTarget = errors.New("未知 match_target")

// errNoCount 表示 daily_count 缺少计数来源。
// 返回 false 会被上层理解为「未命中 = 放行」,所以必须报错。
var errNoCount = errors.New("daily_count 缺少计数来源(Count 为 nil)")

// errNoLocation 表示 time_window / daily_count 缺少时区。
// 禁止默认 UTC:静默用 UTC 会让 A 股 12:00(CST)被判为 04:00(UTC) → 误放行。
var errNoLocation = errors.New("缺少时区(Loc 为 nil),禁止默认 UTC")

// structuredMatchTypes 是走 matchStructured 的结构化类型集合。
// 单独列出来是为了让分派意图可测:任何新增类型必须加入此集合,
// 否则 ruleMatches 的 default 分支会把它送进来但这里会报"未知类型"。
var structuredMatchTypes = map[string]bool{
	"numeric_lte": true,
	"list":        true,
	"time_window": true,
	"daily_count": true,
}

// matchStructured 是 4 种结构化 matcher 的唯一分发点(G12 的落点)。
//
// 返回值语义(务必区分,这是 G11/G12 的核心):
//   - (matchResult{Matched:...}, nil)  → 判定完成
//   - (matchResult{}, err)             → **无法判定**,上层按场景决定 fail-open/closed
//
// 绝不允许把"无法判定"写成 (matchResult{Matched:false}, nil) —— 那是把
// 未知当成"未命中=放行",又一个 fail-open 入口。
func matchStructured(ctx context.Context, mt string, params MatcherParams, subj *Subject) (matchResult, error) {
	switch mt {
	case "numeric_lte":
		return matchNumericLTE(params, subj)
	case "list":
		return matchList(params, subj)
	case "time_window":
		return matchTimeWindow(params, subj)
	case "daily_count":
		return matchDailyCount(ctx, params, subj)
	default:
		return matchResult{}, fmt.Errorf("%w: 未知 match_type %q", errMatcherNotImplemented, mt)
	}
}

// =====================================================================
// pattern_params 与四个结构化 matcher
// =====================================================================

// MatcherParams 是 pattern_params JSONB 的 Go 形状。
// 与 pattern 正交:pattern 继续给 string/regex 用,语义不变;
// 结构化 matcher 只读 pattern_params。
type MatcherParams struct {
	// numeric_lte
	Max *string `json:"max,omitempty"` // 上限,十进制字符串。指针 → 缺省与 "0" 可区分
	Min *string `json:"min,omitempty"` // 预留(P1 下限规则)

	// numeric_lte / list / daily_count 共用:判定哪个字段
	Field string `json:"field,omitempty"` // notional | symbol

	// list
	Values        []string `json:"values,omitempty"`
	Mode          string   `json:"mode,omitempty"`           // include(默认)| exclude
	CaseSensitive *bool    `json:"case_sensitive,omitempty"` // 默认 false

	// time_window
	Source   string `json:"source,omitempty"` // market_session | window(默认)
	Window   string `json:"window,omitempty"` // "09:30-16:00" 或 "09:30-11:30,13:00-15:00"
	Timezone string `json:"tz,omitempty"`     // IANA 名;source=window 时必填
	Days     []int  `json:"days,omitempty"`   // ISO weekday,1=周一..7=周日;空 = 不限

	// daily_count
	Scope        string `json:"scope,omitempty"`                // instrument(默认)| portfolio(P1)
	ExcludeSuper *bool  `json:"exclude_superseded,omitempty"`   // 默认 true

	// 规则 5 的强制披露文案(PRD:deny/ask 理由必须含「基于草案数近似」)
	Disclosure string `json:"disclosure,omitempty"`
}

// ParseParams 解析 pattern_params。空输入视为空对象(不支持参数的类型会用不到)。
func ParseParams(raw json.RawMessage) (MatcherParams, error) {
	var p MatcherParams
	if len(raw) == 0 {
		return p, nil
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, fmt.Errorf("pattern_params 不是合法 JSON: %w", err)
	}
	return p, nil
}

// 结构化 matcher 的配置类错误哨兵。一律由上层按 fail-closed 处理。
var (
	ErrEmptyWin    = fmt.Errorf("%w: window 为空", errBadMatcherConfig)
	ErrBadSource   = fmt.Errorf("%w: source 非法", errBadMatcherConfig)
	ErrNoSession   = fmt.Errorf("%w: Session 为 nil", errBadMatcherConfig)
	ErrBadField    = fmt.Errorf("%w: field 非法", errBadMatcherConfig)
	ErrBadMax      = fmt.Errorf("%w: max 缺失或非法", errBadMatcherConfig)
	ErrEmptyValues = fmt.Errorf("%w: values 为空", errBadMatcherConfig)
	ErrBadMode     = fmt.Errorf("%w: mode 非法", errBadMatcherConfig)
)

// errBadMatcherConfig 是配置类错误的根哨兵,便于上层统一识别。
var errBadMatcherConfig = fmt.Errorf("matcher 配置错误")

// ratFromDecimal 把十进制字符串解析成 big.Rat。
//
// 两层校验是刻意的:
//  1. validDecimalLiteralStr 过形状 —— 拒绝科学计数法(1e3)、十六进制、下划线。
//     big.Rat.SetString 是接受 "1e3" 的,风控不该接受。
//  2. 再交给 big.Rat 做精确比较,全程不经过 float64。
func ratFromDecimal(s string) (*big.Rat, bool) {
	s = strings.TrimSpace(s)
	if !validDecimalLiteralStr(s) {
		return nil, false
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, false
	}
	return r, true
}

// validDecimalLiteralStr 校验十进制字面量形状:可选负号 + 数字 + 可选小数点。
// 与 db.validDecimalLiteral 同语义,但 intercept 包不 import db(避免依赖),
// 且 db 里那个不接受空格裁剪,这里先 TrimSpace。
func validDecimalLiteralStr(s string) bool {
	if s == "" {
		return false
	}
	// 科学计数法显式拒绝,给出可读原因比让 SetString 静默接受更好。
	if strings.ContainsAny(s, "eE") {
		return false
	}
	dotSeen := false
	digits := 0
	for i, r := range s {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case r == '-' && i == 0:
			// 允许负号,但 $-x$ 由调用方决定是否合法(金额上限负数已在校验层拒)
		case r == '.' && !dotSeen:
			dotSeen = true
		default:
			return false
		}
	}
	return digits > 0
}

// draftNotional 取草案的名义金额字符串:优先用调用方算好的 Notional,
// 否则用 Qty × RefPrice 兜底自算。
//
// 返回 (值, 是否可计算)。不可计算时调用方必须按「拦」处理 ——
// 缺失数据不能放行,这是 numeric_lte 的硬边界。
func draftNotional(d *DraftSubject) (string, bool) {
	if d == nil {
		return "", false
	}
	if s := strings.TrimSpace(d.Notional); s != "" {
		if _, ok := ratFromDecimal(s); ok {
			return s, true
		}
		return "", false
	}
	q, ok := ratFromDecimal(d.Qty)
	if !ok {
		return "", false
	}
	p, ok := ratFromDecimal(d.RefPrice)
	if !ok {
		// RefPrice 缺失 = 市价单 = 名义金额无法计算。
		return "", false
	}
	return new(big.Rat).Mul(q, p).FloatString(18), true
}

// matchNumericLTE 判定名义金额是否超过上限。
//
// 判定方向:`notional <= max` → Matched=false(放行);`notional > max` → true(命中,拦)。
// 恰好等于阈值算放行(上位已定 <= 不是 <)。
func matchNumericLTE(p MatcherParams, subj *Subject) (matchResult, error) {
	field := strings.TrimSpace(p.Field)
	if field == "" {
		field = "notional"
	}
	if field != "notional" {
		return matchResult{}, fmt.Errorf("%w: numeric_lte 只支持 field=notional,收到 %q", ErrBadField, field)
	}
	if p.Max == nil || strings.TrimSpace(*p.Max) == "" {
		return matchResult{}, fmt.Errorf("%w: numeric_lte 缺少 max", ErrBadMax)
	}
	maxRat, ok := ratFromDecimal(*p.Max)
	if !ok {
		return matchResult{}, fmt.Errorf("%w: max=%q 不是合法十进制数", ErrBadMax, *p.Max)
	}

	notionalStr, ok := draftNotional(subj.Draft)
	if !ok {
		// 名义金额算不出来 —— 不能放行。
		// 文案刻意区分「漏传字段」与「市价单被禁止」:前者是调用方要修的 bug,
		// 后者会被误读成系统不支持市价单。市价单的正确路径是由执行层用实时
		// 行情算出 Notional 再传进来,风控层不做取价(否则「风控失败」与
		// 「行情失败」两类错误会混在一起)。
		return matchResult{
			Matched: true,
			Reason: "名义金额无法计算(调用方未提供 notional,且 qty/ref_price 不足)," +
				"按拦截处理。市价单请由执行层按实时行情填充 notional 字段。",
		}, nil
	}
	notionalRat, ok := ratFromDecimal(notionalStr)
	if !ok {
		return matchResult{Matched: true, Reason: "名义金额格式非法,按拦截处理"}, nil
	}
	if notionalRat.Cmp(maxRat) > 0 {
		return matchResult{
			Matched: true,
			Reason:  fmt.Sprintf("名义金额 %s 超过上限 %s", notionalStr, *p.Max),
		}, nil
	}
	return matchResult{Matched: false}, nil
}

// matchList 判定标的是否在白名单/黑名单内。
//
//	include(默认):不在列表 → Matched=true(拦)
//	exclude      :在列表   → Matched=true(拦)
//
// 空 values 一律 Matched=true:[] 的字面意思是「谁都不允许」,当成
// 「没有限制」会直接变成 fail-open。
func matchList(p MatcherParams, subj *Subject) (matchResult, error) {
	mode := strings.TrimSpace(p.Mode)
	if mode == "" {
		mode = "include"
	}
	if mode != "include" && mode != "exclude" {
		return matchResult{}, fmt.Errorf("%w: mode=%q,只接受 include/exclude", ErrBadMode, p.Mode)
	}
	if len(p.Values) == 0 {
		// 空白名单必须拦。这是配置错误,但错误的方向是「更严格」,不是放行。
		return matchResult{Matched: true, Reason: "白名单 values 为空(配置错误),按拦截处理"}, nil
	}
	caseSensitive := p.CaseSensitive != nil && *p.CaseSensitive
	norm := func(s string) string {
		if caseSensitive {
			return strings.TrimSpace(s)
		}
		return strings.ToUpper(strings.TrimSpace(s))
	}

	symbol := ""
	if subj.Draft != nil {
		symbol = subj.Draft.Symbol
	}
	if strings.TrimSpace(symbol) == "" {
		return matchResult{Matched: true, Reason: "标为空,无法判定是否在名单内,按拦截处理"}, nil
	}
	want := norm(symbol)
	inList := false
	for _, v := range p.Values {
		if norm(v) == want {
			inList = true
			break
		}
	}

	if mode == "include" && !inList {
		return matchResult{
			Matched: true,
			Reason:  fmt.Sprintf("标的 %s 不在白名单内", symbol),
		}, nil
	}
	if mode == "exclude" && inList {
		return matchResult{
			Matched: true,
			Reason:  fmt.Sprintf("标的 %s 在黑名单内", symbol),
		}, nil
	}
	return matchResult{Matched: false}, nil
}

// matchTimeWindow 判定「当前时刻」是否在允许下单的时段内。
//
// 在时段内 → Matched=false(放行);不在 → Matched=true(拦)。
//
// 区间一律左闭右开 [Open, Close):开盘瞬间可下单,收盘瞬间不可。
func matchTimeWindow(p MatcherParams, subj *Subject) (matchResult, error) {
	if subj == nil || subj.Now.IsZero() {
		return matchResult{}, fmt.Errorf("%w: Now 未注入,无法判定交易时段", errBadMatcherConfig)
	}
	source := strings.TrimSpace(p.Source)
	if source == "" {
		source = "window"
	}

	var sessions []Session
	switch source {
	case "market_session":
		if subj.Session == nil {
			return matchResult{}, ErrNoSession
		}
		if subj.Loc == nil {
			return matchResult{}, errNoLocation
		}
		market := ""
		if subj.Draft != nil {
			market = subj.Draft.Market
		}
		if strings.TrimSpace(market) == "" {
			return matchResult{}, fmt.Errorf("%w: market 为空,无法取交易时段", errBadMatcherConfig)
		}
		ss, err := subj.Session(market, subj.Now, subj.Loc)
		if err != nil {
			return matchResult{}, fmt.Errorf("取交易时段失败(market=%s): %w", market, err)
		}
		sessions = ss
	case "window":
		if strings.TrimSpace(p.Window) == "" {
			return matchResult{}, ErrEmptyWin
		}
		if subj.Loc == nil {
			return matchResult{}, errNoLocation
		}
		ss, err := parseWindowSpec(p.Window, subj.Now, subj.Loc)
		if err != nil {
			// window 字符串解析不出来 = 配置错误。按拦处理而非报错:
			// 报错会让整条草案被 fail-closed deny,而这里还能给出可读理由。
			return matchResult{Matched: true, Reason: "window 配置无法解析,按拦截处理"}, nil
		}
		sessions = ss
	default:
		return matchResult{}, fmt.Errorf("%w: source=%q", ErrBadSource, p.Source)
	}

	if len(p.Days) > 0 {
		wd := int(subj.Now.In(subj.Loc).Weekday()) // Sunday=0
		iso := wd
		if iso == 0 {
			iso = 7
		}
		allowed := false
		for _, d := range p.Days {
			if d == iso {
				allowed = true
				break
			}
		}
		if !allowed {
			return matchResult{Matched: true, Reason: "当前星期不在允许的 days 内"}, nil
		}
	}

	now := subj.Now.In(subj.Loc)
	for _, s := range sessions {
		// 左闭右开:now >= Open && now < Close
		if !now.Before(s.Open) && now.Before(s.Close) {
			return matchResult{Matched: false}, nil
		}
	}
	return matchResult{
		Matched: true,
		Reason:  fmt.Sprintf("当前 %s 不在可交易时段内", now.Format("15:04:05")),
	}, nil
}

// parseWindowSpec 解析 "09:30-16:00" 或 "09:30-11:30,13:00-15:00"。
// 跨午夜窗口(close <= open)时把 close 推到次日。
func parseWindowSpec(spec string, now time.Time, loc *time.Location) ([]Session, error) {
	var out []Session
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		openStr, closeStr, ok := strings.Cut(part, "-")
		if !ok {
			return nil, fmt.Errorf("window 段 %q 缺少 '-'", part)
		}
		oh, om, err := parseClock(strings.TrimSpace(openStr))
		if err != nil {
			return nil, err
		}
		ch, cm, err := parseClock(strings.TrimSpace(closeStr))
		if err != nil {
			return nil, err
		}
		d := now.In(loc)
		open := time.Date(d.Year(), d.Month(), d.Day(), oh, om, 0, 0, loc)
		closeT := time.Date(d.Year(), d.Month(), d.Day(), ch, cm, 0, 0, loc)
		if !closeT.After(open) {
			// 跨午夜(如 22:00-02:00)。用 AddDate 而不是 +24h,
			// 让 DST 日的实际跨度由时区规则决定。
			closeT = closeT.AddDate(0, 0, 1)
		}
		out = append(out, Session{Open: open, Close: closeT})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("window %q 未解析出任何时段", spec)
	}
	return out, nil
}

// parseClock 解析 "HH:MM" 为时、分。
func parseClock(s string) (hour, minute int, err error) {
	hh, mm, ok := strings.Cut(s, ":")
	if !ok {
		return 0, 0, fmt.Errorf("时刻 %q 缺少 ':'", s)
	}
	hour, err = strconv.Atoi(hh)
	if err != nil || hour < 0 || hour > 24 {
		return 0, 0, fmt.Errorf("时刻 %q 的小时非法", s)
	}
	minute, err = strconv.Atoi(mm)
	if err != nil || minute < 0 || minute > 59 {
		return 0, 0, fmt.Errorf("时刻 %q 的分钟非法", s)
	}
	return hour, minute, nil
}

// matchDailyCount 判定「今日已下单次数」是否达到上限。
//
//	count < max  → Matched=false(放行)
//	count >= max → Matched=true(拦)   ← 是 >= 不是 >,第 max 次之后就该拦
func matchDailyCount(ctx context.Context, p MatcherParams, subj *Subject) (matchResult, error) {
	if subj == nil || subj.Count == nil {
		return matchResult{}, errNoCount
	}
	if subj.Loc == nil {
		return matchResult{}, errNoLocation
	}
	if p.Max == nil || strings.TrimSpace(*p.Max) == "" {
		return matchResult{}, fmt.Errorf("%w: daily_count 缺少 max", ErrBadMax)
	}
	maxStr := strings.TrimSpace(*p.Max)
	// 次数必须是整数:先过十进制形状(拒科学计数法),再拒带小数点的。
	if !validDecimalLiteralStr(maxStr) || strings.Contains(maxStr, ".") {
		return matchResult{}, fmt.Errorf("%w: daily_count 的 max=%q 必须是正整数", ErrBadMax, maxStr)
	}
	maxN, err := strconv.ParseInt(maxStr, 10, 64)
	if err != nil || maxN <= 0 {
		return matchResult{}, fmt.Errorf("%w: daily_count 的 max=%q 必须是正整数", ErrBadMax, maxStr)
	}

	// 「今日」按交易所时区:当地 00:00 → 次日 00:00。
	// 必须用 AddDate 而不是 +24h:DST 日可能是 23 或 25 小时。
	d := subj.Now.In(subj.Loc)
	since := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, subj.Loc)
	until := since.AddDate(0, 0, 1)

	q := CountQuery{SinceIncl: since, UntilExcl: until}
	if subj.Draft != nil {
		q.InstrumentID = subj.Draft.InstrumentID
		q.Symbol = subj.Draft.Symbol
		q.Market = subj.Draft.Market
	}
	// 默认排除被取代的草案:否则反复改稿会耗尽配额。
	q.ExcludeSuper = p.ExcludeSuper == nil || *p.ExcludeSuper

	n, err := subj.Count(ctx, q)
	if err != nil {
		// 「不知道下了几单」→ 不放行。
		return matchResult{
			Matched: true,
			Reason:  fmt.Sprintf("无法查询今日下单次数(%v),按拦截处理", err),
		}, nil
	}
	if n >= maxN {
		return matchResult{
			Matched: true,
			Reason:  fmt.Sprintf("今日已下单 %d 次,达到上限 %d 次", n, maxN),
		}, nil
	}
	return matchResult{Matched: false}, nil
}
