package agent

// 金融数据客户端的抽象。
//
// # 为什么用 interface 而不是直接依赖 HTTP 实现
//
// 测试是主要原因：MarketToolSet 的工具 handler 需要覆盖「错误码映射」与
// 「多通道降级」这两块逻辑，而这两块的真实验证需要构造各种失败响应
// （SYMBOL_NOT_FOUND / UPSTREAM_BLOCKED / NO_DATA …）。如果 handler 直接
// 依赖 HTTP client，每个用例都要起一个假 sidecar 服务。
//
// 有了这个 interface，测试可以注入一个返回预设错误码的假 client，
// 同时真 sidecar 的集成测试用一个 build tag 或 skip 条件单独跑。

import (
	"context"
	"time"
)

// MarketError 是通道返回的结构化错误。
//
// Code 直接对应 sidecar 的错误码（AKSHARE_*）。Go 侧据此决定是否降级 ——
// 这是「错误分类驱动降级」的关键数据结构：光有 error 字符串无法可靠判断
// 「该换源还是该直接失败」。
type MarketError struct {
	// Code 是 sidecar 错误码，如 "AKSHARE_SYMBOL_NOT_FOUND"。
	Code string
	// Message 是人可读描述。
	Message string
	// Channel 是产生该错误的通道（用于日志与降级决策）。
	Channel Channel
	// Retryable 表示换一个通道是否可能成功。
	// SYMBOL_NOT_FOUND / BAD_REQUEST → false（换源也找不到）
	// UPSTREAM_BLOCKED / TIMEOUT / CHALLENGE / NO_DATA → true
	Retryable bool
}

func (e *MarketError) Error() string {
	return string(e.Channel) + ": " + e.Code + ": " + e.Message
}

// Quote 是一次行情查询的结果。
//
// 注意 PriceKind 与 TradingDate 一起表达语义边界：返回的是「某一天的收盘价」，
// 不是「此刻的价」。见 sidecar README 的「/quote 的语义边界」。
type Quote struct {
	Symbol      string
	Market      string
	PriceKind   string // "daily_close"
	TradingDate string // 该价格所属的交易日
	Last        string
	Open        string
	High        string
	Low         string
	PrevClose   string
	ChangePct   string
	Volume      string
	Amount      string
	IsPartial   bool
	Channel     Channel
}

// Bar 是一根 K 线。价格全为字符串（NUMERIC 精度，禁 float）。
type Bar struct {
	TradingDate string
	Open        string
	High        string
	Low         string
	Close       string
	Volume      string
	Amount      string
	IsPartial   bool
}

// MacroPoint 是宏观序列的一个点。
type MacroPoint struct {
	Period string
	Value  string
}

// Macro 是一个宏观指标序列。
type Macro struct {
	SeriesName string
	Unit       string
	Points     []MacroPoint
	Channel    Channel
}

// MarketClient 是金融数据的统一取数接口。
//
// 实现方负责：HTTP 调用、错误码解析、超时、重试。调用方（工具 handler）
// 只关心「拿到了数据」或「拿到了一个带 Code 的错误」。
type MarketClient interface {
	// Health 报告客户端可用性。WarmupDone=false 表示 sidecar 还在预热，
	// 此时不应发真实请求（否则首个请求会超时而误判为通道不可用）。
	Health(ctx context.Context) (warmupDone bool, detail string, err error)

	// FetchQuote 取行情快照（日线收盘价语义）。
	FetchQuote(ctx context.Context, symbol, market string, ch Channel) (*Quote, error)

	// FetchKline 取 K 线。
	FetchKline(ctx context.Context, symbol, market, period string, count int, ch Channel) ([]Bar, error)

	// FetchMacro 取宏观指标。
	FetchMacro(ctx context.Context, indicator string) (*Macro, error)

	// Timeout 返回该客户端的建议超时（供 handler 建 context）。
	Timeout() time.Duration
}

// ---------------------------------------------------------------------
// 加密衍生品资金流
// ---------------------------------------------------------------------

// CryptoFlowClient 是加密衍生品资金流数据的抽象。
//
// **刻意与 MarketClient 分开**：MarketClient 的方法（quote/kline/macro）
// akshare 和 okx 都能实现；而资金费率与持仓量只有 OKX 有。若塞进
// MarketClient，AkshareClient 就得实现两个永远返回 CHANNEL_NOT_APPLICABLE
// 的方法 —— 那是接口污染（一个类型被迫实现它做不到的方法，说明接口切错了）。
type CryptoFlowClient interface {
	// FetchFundingRate 返回当前资金费率。symbol 可以是 SPOT 或 SWAP 形式，
	// 实现方负责推导成 SWAP instID。
	FetchFundingRate(ctx context.Context, symbol string) (*FundingRate, error)

	// FetchOpenInterest 返回当前持仓量。同上。
	FetchOpenInterest(ctx context.Context, symbol string) (*OpenInterest, error)
}

// FundingRate 是资金费率快照。金额一律 string（NUMERIC 精度铁律）。
type FundingRate struct {
	InstID          string `json:"inst_id"`                     // BTC-USDT-SWAP
	Rate            string `json:"rate"`                        // fundingRate，如 "0.0001000000000000"
	FundingTime     string `json:"funding_time"`                // 下次结算时间，毫秒时间戳
	NextFundingTime string `json:"next_funding_time,omitempty"` // OKX 也返回 nextFundingTime
	InterestRate    string `json:"interest_rate,omitempty"`     // 诊断用
	MaxFundingRate  string `json:"max_funding_rate,omitempty"`  // 诊断用
	MinFundingRate  string `json:"min_funding_rate,omitempty"`  // 诊断用
}

// OpenInterest 是持仓量快照。
type OpenInterest struct {
	InstID string `json:"inst_id"`
	OI     string `json:"oi"`     // 张数
	OICcy  string `json:"oi_ccy"` // 币数
	OIUsd  string `json:"oi_usd"` // 美元价值
	TS     string `json:"ts"`     // 毫秒时间戳
}

// ---------------------------------------------------------------------
// OKX 服务端技术指标
// ---------------------------------------------------------------------

// IndicatorClient 是「服务端算指标」能力的抽象。
//
// # 为什么与 MarketClient 分开
//
// 与 CryptoFlowClient 同一理由：MarketClient 的三个方法（quote/kline/macro）
// akshare 与 okx 都能实现；而服务端指标只有 OKX 有（70+ 指标 + backtest-time
// 历史回溯）。塞进 MarketClient 会让 AkshareClient 多一个永远返回
// CHANNEL_NOT_APPLICABLE 的方法。
//
// # 与 compute_indicator 的分工
//
//   - compute_indicator：本地算 6 种（MACD/RSI/ATR/BOLL/KDJ/VWAP）。
//     离线可用、无网络依赖、用于 bars 预处理。
//   - IndicatorClient：查 OKX 服务端 70+ 种，支持 BacktestTime 历史时点回溯。
//
// 本地实现不了历史回溯（要用当时可见的 bars，而我们现在只有最新快照），
// 服务端有 --backtest-time。但服务端要网络，离线场景仍需本地算。
type IndicatorClient interface {
	// FetchIndicator 查询服务端技术指标。
	//
	// bar 必须已通过 ValidOkxBar 校验（CLI 只认 9 个值）。
	// backtestTime 为 0 表示实时。
	FetchIndicator(ctx context.Context, in IndicatorQuery) (*IndicatorResult, error)
}

// IndicatorQuery 是一次指标查询的参数。
type IndicatorQuery struct {
	Indicator    string // rsi / macd / boll ...（统一小写）
	Symbol       string // BTC-USDT（必须含连字符）
	Bar          string // 3m/5m/15m/1H/4H/12Hutc/1Dutc/3Dutc/1Wutc
	Params       string // "14" 或 "12,26,9"；空=用服务端默认
	BacktestTime int64  // unix 毫秒；0=实时
}

// IndicatorResult 是服务端算出的指标值。
//
// Values 是 map[string]string 而不是具体结构：不同指标的字段完全不同
// （rsi 返回 {"14": "39.59"}，macd 返回 {"dif":..,"dea":..,"macd":..}），
// 而且 OKX 的 70+ 指标字段名我们不可能穷举。
//
// **金额/数值一律 string** —— 与 FundingRate/OI 同一套精度铁律。
type IndicatorResult struct {
	Indicator    string            `json:"indicator"`
	Symbol       string            `json:"symbol"`
	Bar          string            `json:"bar"`
	TS           string            `json:"ts"` // ISO8601（服务端返回毫秒则转成 ISO）
	Values       map[string]string `json:"values"`
	BacktestTime int64             `json:"backtest_time"`
	Source       string            `json:"source"` // "okx"
}

// ValidOkxBar 判断 bar 是否是 OKX CLI 接受的值。
//
// # 为什么必须白名单
//
// CLI 的 --bar 只接受这 9 个，传 "1D" 会报错（**必须是 "1Dutc"**）。
// 不校验的话，用户/模型会拿到一句难懂的 CLI 错误，而不是「你该传 1Dutc」。
var ValidOkxBar = func() map[string]bool {
	m := make(map[string]bool, 9)
	for _, b := range []string{
		"3m", "5m", "15m", "1H", "4H",
		"12Hutc", "1Dutc", "3Dutc", "1Wutc",
	} {
		m[b] = true
	}
	return m
}()
