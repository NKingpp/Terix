package agent

// AkShare sidecar 的 HTTP 客户端。
//
// sidecar 的协议与错误码见 sidecars/akshare/README.md。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// 环境变量名与 sidecar 侧读的同一个 —— 两处都改才能生效，
// 但至少不会出现「Go 读 A、Python 读 B」的静默漂移。
const (
	envAkshareAddr = "AKSHARE_SIDECAR_ADDR"
	defaultAkshare = "127.0.0.1:8791"
)

// AkshareClient 通过 HTTP 调本地 sidecar。
type AkshareClient struct {
	baseURL string
	http    *http.Client
	timeout time.Duration
}

// NewAkshareClient 按环境变量构造。addr 为空时用 AKSHARE_SIDECAR_ADDR，
// 再为空用 127.0.0.1:8791。
func NewAkshareClient(addr string) *AkshareClient {
	if addr == "" {
		addr = os.Getenv(envAkshareAddr)
	}
	if addr == "" {
		addr = defaultAkshare
	}
	// 客户端的 HTTP 超时留得比工具超时宽：真正的截止时间由 handler 的
	// context 控制，这里的超时是兜底（防止 sidecar 无响应时永久挂起）。
	return &AkshareClient{
		baseURL: "http://" + addr,
		http:    &http.Client{Timeout: 30 * time.Second},
		timeout: 15 * time.Second,
	}
}

func (c *AkshareClient) Timeout() time.Duration { return c.timeout }

// sidecarResp 是 sidecar 的统一响应信封。
type sidecarResp struct {
	OK   bool            `json:"ok"`
	Data json.RawMessage `json:"data"`
	Meta struct {
		Source     string `json:"source"`
		ElapsedMS  int    `json:"elapsed_ms"`
		FetchedAt  string `json:"fetched_at"`
		WarmupDone *bool  `json:"warmup_done"`
		AKShareVer string `json:"akshare_version"`
	} `json:"meta"`
	Error *struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Detail  map[string]any `json:"detail"`
	} `json:"error"`
}

// do 发一次请求并解析信封。
func (c *AkshareClient) do(ctx context.Context, path string, q url.Values) (*sidecarResp, error) {
	u := c.baseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// 连不上 sidecar（进程没起 / 端口错）—— 这是通道不可用，
		// 换个通道可能成功，所以 Retryable=true。
		return nil, &MarketError{
			Code:      "SIDECAR_UNREACHABLE",
			Message:   err.Error(),
			Channel:   ChannelAkshareCN, // 具体渠道由调用方覆盖
			Retryable: true,
		}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("读取 sidecar 响应失败: %w", err)
	}
	// 反爬页面检测（sidecar 已做，这里兜一道：万一 sidecar 前面又挂了代理）
	trimmed := bytes.TrimSpace(body)
	if bytes.HasPrefix(trimmed, []byte("<!DOCTYPE")) || bytes.HasPrefix(trimmed, []byte("<html")) {
		return nil, &MarketError{
			Code:      "AKSHARE_HTML_CHALLENGE",
			Message:   "响应是 HTML 而非 JSON",
			Retryable: true,
		}
	}
	var out sidecarResp
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("解析 sidecar 响应失败: %w (body=%s)", err, snippet(body))
	}
	if !out.OK {
		if out.Error == nil {
			return nil, fmt.Errorf("sidecar 返回 ok=false 但没有 error 字段")
		}
		return nil, &MarketError{
			Code:      out.Error.Code,
			Message:   out.Error.Message,
			Retryable: retryableCode(out.Error.Code),
		}
	}
	return &out, nil
}

// retryableCode 判断某个 sidecar 错误码是否值得换通道重试。
//
// 这是降级决策的唯一依据。判定原则：
//   - 「这个标的不存在」「你传的参数错了」→ 换通道也没用，不重试
//   - 「源不可用」「超时」「被反爬」「这个源没这条数据」→ 换通道有意义，重试
//
// 特别注意 NO_DATA：某源对该区间没数据，另一个源可能有（例如新浪缺某天、
// 腾讯有）。所以 NO_DATA 是 retryable。
func retryableCode(code string) bool {
	switch code {
	case "AKSHARE_BAD_REQUEST", "AKSHARE_SYMBOL_NOT_FOUND":
		return false
	case "AKSHARE_NO_DATA", "AKSHARE_HTML_CHALLENGE",
		"AKSHARE_UPSTREAM_TIMEOUT", "AKSHARE_UPSTREAM_BLOCKED",
		"AKSHARE_PARSE_FAILED", "AKSHARE_INTERNAL":
		return true
	default:
		// 未知错误码：倾向于重试，因为「没见过的错误」更可能是源侧问题。
		// 但要在日志里记录，便于发现新的错误码。
		return true
	}
}

func snippet(b []byte) string {
	const max = 200
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "..."
}

// Health 检查 sidecar 是否可用以及是否预热完成。
//
// WarmupDone=false 时调用方应等待而非发真实请求 —— 预热期间首个请求会
// 撞工具超时，把「还没热身」误判成「通道不可用」。
func (c *AkshareClient) Health(ctx context.Context) (bool, string, error) {
	resp, err := c.do(ctx, "/health", nil)
	if err != nil {
		return false, "", err
	}
	var h struct {
		Status     string   `json:"status"`
		WarmupDone bool     `json:"warmup_done"`
		WarmupErr  *string  `json:"warmup_error"`
		WarmupMS   *int     `json:"warmup_elapsed_ms"`
		SourcesOK  []string `json:"sources_ok"`
		QuoteKind  string   `json:"quote_kind"`
		AKShareVer string   `json:"akshare_version"`
	}
	if err := json.Unmarshal(resp.Data, &h); err != nil {
		return false, "", fmt.Errorf("解析 /health 失败: %w", err)
	}
	detail := fmt.Sprintf("status=%s akshare=%s sources=%v quote_kind=%s",
		h.Status, h.AKShareVer, h.SourcesOK, h.QuoteKind)
	if h.WarmupErr != nil && *h.WarmupErr != "" {
		detail += " warmup_error=" + *h.WarmupErr
	}
	return h.WarmupDone, detail, nil
}

// WaitWarmup 轮询 /health 直到 warmup_done 或超时。
//
// 为什么需要它：sidecar 启动后要预热（本机 ~800ms，慢机器可能 20s+）。
// 预热完成前发的真实请求会 8s 超时，表现为「服务起来了但第一次调用总失败」。
func (c *AkshareClient) WaitWarmup(ctx context.Context, poll time.Duration) error {
	if poll <= 0 {
		poll = 200 * time.Millisecond
	}
	for {
		done, detail, err := c.Health(ctx)
		if err != nil {
			return fmt.Errorf("等待 sidecar 预热时健康检查失败: %w", err)
		}
		if done {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("等待 sidecar 预热超时（最后状态: %s）: %w", detail, ctx.Err())
		case <-time.After(poll):
		}
	}
}

// channelToMarket 把 Channel 映射成 sidecar 的 market 参数。
func channelToMarket(ch Channel) string {
	switch ch {
	case ChannelAkshareCN:
		return "cn"
	case ChannelAkshareHK:
		return "hk"
	case ChannelAkshareUS:
		return "us"
	default:
		return ""
	}
}

// FetchQuote 取行情快照。
//
// ch 决定市场：ChannelAkshareCN/HK/US 分别对应 cn/hk/us。
// 若 ch 是 ChannelOKX（OKX 不走 akshare），返回不可重试错误 —— 调用方应
// 在降级链里跳过它，而不是把它当 akshare 的失败。
func (c *AkshareClient) FetchQuote(ctx context.Context, symbol, market string, ch Channel) (*Quote, error) {
	if ch == ChannelOKX {
		return nil, &MarketError{
			Code:      "CHANNEL_NOT_APPLICABLE",
			Message:   "OKX 不支持通过 akshare sidecar 查询",
			Channel:   ch,
			Retryable: false,
		}
	}
	if m := channelToMarket(ch); m != "" {
		market = m
	}
	if market == "" {
		market = "cn"
	}
	q := url.Values{"symbol": {symbol}, "market": {market}}
	resp, err := c.do(ctx, "/quote", q)
	if err != nil {
		if me, ok := err.(*MarketError); ok && me.Channel == "" {
			me.Channel = ch
		}
		return nil, err
	}
	var d struct {
		Symbol      string `json:"symbol"`
		Market      string `json:"market"`
		PriceKind   string `json:"price_kind"`
		TradingDate string `json:"trading_date"`
		Last        string `json:"last"`
		Open        string `json:"open"`
		High        string `json:"high"`
		Low         string `json:"low"`
		PrevClose   string `json:"prev_close"`
		ChangePct   string `json:"change_pct"`
		Volume      string `json:"volume"`
		Amount      string `json:"amount"`
		IsPartial   bool   `json:"is_partial"`
	}
	if err := json.Unmarshal(resp.Data, &d); err != nil {
		return nil, fmt.Errorf("解析 /quote 失败: %w", err)
	}
	return &Quote{
		Symbol: d.Symbol, Market: d.Market, PriceKind: d.PriceKind,
		TradingDate: d.TradingDate, Last: d.Last, Open: d.Open, High: d.High,
		Low: d.Low, PrevClose: d.PrevClose, ChangePct: d.ChangePct,
		Volume: d.Volume, Amount: d.Amount, IsPartial: d.IsPartial, Channel: ch,
	}, nil
}

// FetchKline 取 K 线。
func (c *AkshareClient) FetchKline(ctx context.Context, symbol, market, period string, count int, ch Channel) ([]Bar, error) {
	if ch == ChannelOKX {
		return nil, &MarketError{
			Code:      "CHANNEL_NOT_APPLICABLE",
			Message:   "OKX 不支持通过 akshare sidecar 查询",
			Channel:   ch,
			Retryable: false,
		}
	}
	if m := channelToMarket(ch); m != "" {
		market = m
	}
	if market == "" {
		market = "cn"
	}
	if period == "" {
		period = "daily"
	}
	if count <= 0 {
		count = 250
	}
	q := url.Values{
		"symbol": {symbol}, "market": {market},
		"period": {period}, "count": {strconv.Itoa(count)},
	}
	resp, err := c.do(ctx, "/kline", q)
	if err != nil {
		if me, ok := err.(*MarketError); ok && me.Channel == "" {
			me.Channel = ch
		}
		return nil, err
	}
	var d struct {
		Bars []struct {
			TradingDate string `json:"trading_date"`
			Open        string `json:"open"`
			High        string `json:"high"`
			Low         string `json:"low"`
			Close       string `json:"close"`
			Volume      string `json:"volume"`
			Amount      string `json:"amount"`
			IsPartial   bool   `json:"is_partial"`
		} `json:"bars"`
	}
	if err := json.Unmarshal(resp.Data, &d); err != nil {
		return nil, fmt.Errorf("解析 /kline 失败: %w", err)
	}
	out := make([]Bar, len(d.Bars))
	for i, b := range d.Bars {
		out[i] = Bar{
			TradingDate: b.TradingDate, Open: b.Open, High: b.High, Low: b.Low,
			Close: b.Close, Volume: b.Volume, Amount: b.Amount, IsPartial: b.IsPartial,
		}
	}
	return out, nil
}

// FetchMacro 取宏观指标。
func (c *AkshareClient) FetchMacro(ctx context.Context, indicator string) (*Macro, error) {
	if strings.TrimSpace(indicator) == "" {
		indicator = "cpi"
	}
	resp, err := c.do(ctx, "/macro", url.Values{"indicator": {indicator}})
	if err != nil {
		return nil, err
	}
	var d struct {
		SeriesName string `json:"series_name"`
		Unit       string `json:"unit"`
		Points     []struct {
			Period string `json:"period"`
			Value  string `json:"value"`
		} `json:"points"`
	}
	if err := json.Unmarshal(resp.Data, &d); err != nil {
		return nil, fmt.Errorf("解析 /macro 失败: %w", err)
	}
	m := &Macro{SeriesName: d.SeriesName, Unit: d.Unit, Channel: ChannelAkshareCN}
	for _, p := range d.Points {
		m.Points = append(m.Points, MacroPoint{Period: p.Period, Value: p.Value})
	}
	return m, nil
}

// 编译期确认 *AkshareClient 实现了 MarketClient。
var _ MarketClient = (*AkshareClient)(nil)

// 避免 net 包未使用（保留 import 以便未来加自定义 Dialer 超时）。
var _ = net.DialTimeout
