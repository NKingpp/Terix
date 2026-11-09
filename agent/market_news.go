package agent

// 新闻与财报取数：*AkshareClient 的扩展方法。
//
// # 为什么不进 MarketClient 接口
//
// MarketClient（market_client.go:93）是「quote/kline/macro」三件套的抽象，
// okx 也要实现它。新闻与财报只有 akshare 有，塞进接口会让 OKX 客户端被迫
// 实现两个永远返回 CHANNEL_NOT_APPLICABLE 的方法 —— 那是接口污染。
// 所以新方法只挂在具体类型 *AkshareClient 上，调用方用类型断言拿。
// （同 CryptoFlowClient / IndicatorClient 的处理方式。）

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// 新增错误码：与 AKSHARE_* 系列同一层级，但语义是「查得到，只是没有」。
//
// 必须与 AKSHARE_UPSTREAM_BLOCKED / TIMEOUT 严格区分：
//   - 源故障 → 换通道/重试有意义，且应上报为系统异常；
//   - 无数据 → 「这只票最近没新闻」「这个市场财报没接」是**合法结论**，
//     重试一万次也一样，而且不该被算作健康度告警。
const (
	ErrNewsNoData        = "NEWS_NO_DATA"
	ErrFundamentalNoData = "FUNDAMENTAL_NO_DATA"
)

// NewsItem 是一条财经新闻。
//
// 字段来源说明（两个源的列结构完全不同，这里做了归一）：
//   - 财新 stock_news_main_cx 只有 tag/summary/url，没有标题、没有时间。
//     Title 取 summary 首句，Category 放 tag，Published 从 URL 里的日期抠。
//   - 财联社 stock_info_global_cls 有 标题/内容/发布日期/发布时间，没有 URL。
//
// 所以 **URL 和 Published 都可能为空** —— 这是上游限制，不是解析漏了。
// 调用方渲染时必须容忍空串，不能当成错误。
type NewsItem struct {
	Title     string `json:"title"`
	Summary   string `json:"summary"`
	URL       string `json:"url"`
	Source    string `json:"source"`    // caixin | cls
	Published string `json:"published"` // 可能为空
	Category  string `json:"category,omitempty"`
}

// Fundamental 是财报摘要。
//
// 为什么是「行 + 列名」而不是结构化字段：
// 三大报表科目上百个，且不同行业科目不同（银行没有「存货」）。用
// []map[string]string 让调用方按列名自取，比预定义 100 个字段现实。
//
// **值一律 string** —— 与 Quote/Bar 同一套精度铁律，禁 float。
type Fundamental struct {
	Symbol     string              `json:"symbol"`
	Market     string              `json:"market"`
	ReportType string              `json:"report_type"`
	Currency   string              `json:"currency"`
	Periods    []string            `json:"periods"` // 报告期，新→旧
	Columns    []string            `json:"columns"` // 首列是「指标」或「科目」
	Rows       []map[string]string `json:"rows"`
	Note       string              `json:"note"`
	Channel    Channel             `json:"-"`
}

// 市场常量（sidecar 只认这三个）。
var validNewsMarkets = map[string]bool{"cn": true, "us": true, "hk": true}

// FetchNews 取财经新闻（财新 + 财联社合并去重）。
//
// symbol 非空时只保留 title/summary 里含该代码的条目。
//
// # 空结果不是错误，但仍返回 error —— 这是刻意的设计冲突，解释如下
//
// 「这只票没新闻」对分析师是有效信息，所以 sidecar 返回 ok=true + 空 items。
// 但若这里也返回 (nil, nil)，调用方无法区分「没新闻」和「实现忘了填」，
// 且会把空切片直接喂给 prompt 生成「无新闻」小结，看起来像成功。
// 因此返回 (nil, *MarketError{Code: NEWS_NO_DATA, Retryable: false})：
//   - 上层用 IsNoDataError 判定，可以渲染成「该标的近期无相关新闻」；
//   - 不会被降级链当作源故障去重试别的通道（Retryable=false）。
func (c *AkshareClient) FetchNews(ctx context.Context, symbol, market string, limit int) ([]NewsItem, error) {
	market = strings.ToLower(strings.TrimSpace(market))
	if market == "" {
		market = "cn"
	}
	if !validNewsMarkets[market] {
		return nil, &MarketError{
			Code:      "NEWS_BAD_MARKET",
			Message:   fmt.Sprintf("market 非法: %q（应为 cn/us/hk）", market),
			Retryable: false,
		}
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 50 {
		limit = 50 // 与 sidecar 上限一致：每条新闻进 prompt 都花 token
	}

	q := url.Values{"market": {market}, "limit": {strconv.Itoa(limit)}}
	if s := strings.TrimSpace(symbol); s != "" {
		q.Set("symbol", s)
	}

	resp, err := c.do(ctx, "/news", q)
	if err != nil {
		return nil, err
	}
	var d struct {
		Symbol  string     `json:"symbol"`
		Market  string     `json:"market"`
		Matched int        `json:"matched"`
		Items   []NewsItem `json:"items"`
	}
	if err := json.Unmarshal(resp.Data, &d); err != nil {
		return nil, fmt.Errorf("解析 /news 失败: %w", err)
	}
	if len(d.Items) == 0 {
		return nil, &MarketError{
			Code: ErrNewsNoData,
			Message: fmt.Sprintf("%s 近期无相关新闻（market=%s，已检索财新+财联社）",
				orDash(symbol), market),
			Channel:   ChannelAkshareCN,
			Retryable: false, // 换通道也找不到「不存在的新闻」
		}
	}
	return d.Items, nil
}

// FetchFundamental 取财报数据（最近 4 期关键科目）。
//
// reportType: 资产负债表 / 利润表 / 现金流量表 / abstract（摘要）。
//
// 美股/港股当前返回 FUNDAMENTAL_NO_DATA —— sidecar 侧刻意不接东财 _em 接口，
// 理由见 server.py 里 ep_fundamental 上方那段注释（核心：_em 会静默失效）。
// 这是**能力缺口**而不是故障，所以 Retryable=false，上层应提示「暂不支持」。
func (c *AkshareClient) FetchFundamental(ctx context.Context, symbol, market, reportType string) (*Fundamental, error) {
	symbol = strings.TrimSpace(symbol)
	if symbol == "" {
		return nil, &MarketError{
			Code:      "FUNDAMENTAL_BAD_REQUEST",
			Message:   "缺少 symbol",
			Retryable: false,
		}
	}
	market = strings.ToLower(strings.TrimSpace(market))
	if market == "" {
		market = "cn"
	}
	if !validNewsMarkets[market] {
		return nil, &MarketError{
			Code:      "FUNDAMENTAL_BAD_REQUEST",
			Message:   fmt.Sprintf("market 非法: %q（应为 cn/us/hk）", market),
			Retryable: false,
		}
	}
	if reportType == "" {
		reportType = "abstract"
	}

	q := url.Values{
		"symbol":      {symbol},
		"market":      {market},
		"report_type": {reportType},
	}
	resp, err := c.do(ctx, "/fundamental", q)
	if err != nil {
		return nil, err
	}
	var d struct {
		Symbol     string              `json:"symbol"`
		Market     string              `json:"market"`
		ReportType string              `json:"report_type"`
		Currency   string              `json:"currency"`
		Periods    []string            `json:"periods"`
		Columns    []string            `json:"columns"`
		Rows       []map[string]string `json:"rows"`
		Note       string              `json:"note"`
	}
	if err := json.Unmarshal(resp.Data, &d); err != nil {
		return nil, fmt.Errorf("解析 /fundamental 失败: %w", err)
	}
	if len(d.Rows) == 0 {
		// 与 FetchNews 同理：ok=true 但零行 = 该标的无财报数据，不是源挂了。
		return nil, &MarketError{
			Code:      ErrFundamentalNoData,
			Message:   fmt.Sprintf("%s 无 %s 数据（market=%s）", symbol, reportType, market),
			Channel:   ChannelAkshareCN,
			Retryable: false,
		}
	}
	return &Fundamental{
		Symbol:     d.Symbol,
		Market:     d.Market,
		ReportType: d.ReportType,
		Currency:   d.Currency,
		Periods:    d.Periods,
		Columns:    d.Columns,
		Rows:       d.Rows,
		Note:       d.Note,
		Channel:    ChannelAkshareCN,
	}, nil
}

// IsNoDataError 判断错误是否是「无数据」而非「源故障」。
//
// 上层用它决定提示语：「该标的近期无相关新闻」vs「数据源暂时不可用，请稍后重试」。
// 两者的运维含义完全不同 —— 前者不该触发告警和重试，后者该。
//
// 覆盖三类来源：
//   - Go 侧转换出的 NEWS_NO_DATA / FUNDAMENTAL_NO_DATA / MX_NO_DATA
//   - sidecar 直接返回的 AKSHARE_NO_DATA 与 AKSHARE_SYMBOL_NOT_FOUND
//     （标的代码写错，本质也是「查不到，重试无用」）
//
// MX_NO_DATA 归进来的理由与前面一致：妙想问句没命中标的/该指标不提供，
// 换问句或换通道可能有用，但**重试同一个问句一万次都一样**，
// 而且它不该被算作健康度告警（源是好的）。
func IsNoDataError(err error) bool {
	me, ok := err.(*MarketError)
	if !ok {
		return false
	}
	switch me.Code {
	case ErrNewsNoData, ErrFundamentalNoData, ErrMXNoData,
		"AKSHARE_NO_DATA", "AKSHARE_SYMBOL_NOT_FOUND":
		return true
	}
	return false
}

// Compile-time guard: 新方法只挂在具体类型上，接口保持不变。
var _ MarketClient = (*AkshareClient)(nil)

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "全市场"
	}
	return s
}
