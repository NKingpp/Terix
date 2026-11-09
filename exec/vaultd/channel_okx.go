package vaultd

// OKX 通道实现（OKX v5 REST API）。
//
// # 只碰公开文档确认过的部分
//
// 签名算法、下单/查单路径、错误码结构都来自 OKX v5 官方文档的公开约定：
//
//	prehash = timestamp + METHOD + requestPath + body
//	sign    = base64(HMAC-SHA256(secretKey, prehash))
//	header  = OK-ACCESS-KEY / OK-ACCESS-SIGN / OK-ACCESS-TIMESTAMP / OK-ACCESS-PASSPHRASE
//
// requestPath **包含 query string**（查单接口必需），这点容易写错。
//
// # 凭证不进日志（硬要求）
//
// 本文件任何日志/错误字符串都不得出现 api key / secret / passphrase 的明文。
// 错误信息只带「哪一步失败」，不带值。

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// okxDefaultBaseURL 是生产 base url。测试用 httptest 覆盖。
const okxDefaultBaseURL = "https://www.okx.com"

// okxTimeout 是单次 HTTP 请求超时。
//
// # 为什么要超时
//
// 没有超时的 HTTP 客户端在下单路径上等于「可能永久阻塞」——
// 调用方（artex）会误判为「提交中」，而券商侧可能已经成交。
// 超时后返回错误，让调用方走 /v1/query 对账，而不是盲目重试。
const okxTimeout = 15 * time.Second

// OKXConfig 是 OKX 通道的配置。
//
// 凭证来自环境变量（见 cmd/vaultd/main.go 的说明：vaultd 不连数据库）。
type OKXConfig struct {
	APIKey     string
	SecretKey  string
	Passphrase string

	// BaseURL 为空时用 okxDefaultBaseURL。
	BaseURL string

	// ResolveHost / ResolveIP 把某个域名强制解析到指定 IP。
	//
	// # 为什么需要（2026-11-06 实测）
	//
	// OKX 的镜像域名（如 www.ynhf1jp.com）在本机 DNS 解析不到，
	// 而官方 CLI 靠内置的 okx-pilot 解析出 IP 后直连。vaultd 是纯 Go，
	// 没有那套逻辑，于是报：
	//
	//	dial tcp: lookup www.ynhf1jp.com: no such host
	//
	// 改系统 hosts 影响全局且有权限问题，所以在 HTTP Transport 层做
	// DialContext 重写：只影响本进程，且只影响这**一个**域名。
	//
	// 留空则完全走系统 DNS（默认行为，不影响正常网络环境）。
	ResolveHost string
	ResolveIP   string

	// Demo 为 true 时带上 `x-simulated-trading: 1`，把请求打到模拟盘。
	//
	// # 为什么必须显式区分
	//
	// 模拟盘 key 与实盘 key 是两套凭证，混用会得到 401 或更糟 ——
	// 用模拟盘 key 打实盘地址会得到鉴权失败（安全），
	// 但用实盘 key 忘了带 header 就是**真实资金**在动。
	// 所以这个开关只由配置决定，绝不由「key 长什么样」去猜。
	Demo bool
}

// OKXChannel 是 OKX 的 Channel 实现。
type OKXChannel struct {
	cfg    OKXConfig
	base   string
	client *http.Client

	// idem 是进程内幂等表。
	//
	// # 为什么需要两层幂等
	//
	//  1. 内存表（本字段）：同一进程内同一个 clientOrderID 只发一次 HTTP。
	//     好处：重试不发包，省一次往返，也避免 OKX 侧限频。
	//     局限：**只在单进程有效**。vaultd 重启后表清空。
	//  2. OKX 侧的 clOrdId 幂等：跨重启/跨进程的最终保证。
	//     OKX 对同一 clOrdId 的重复下单返回 51603（重复 clOrdId）而不是新开一笔。
	//
	// 结论：内存表是优化 + 防手抖，clOrdId 才是权威。两者都不能省。
	idem sync.Map // map[string]*okxIdemEntry
}

// okxIdemEntry 是单个 clientOrderID 的幂等状态。
//
// 每个 key 一把锁：不同订单可并行提交，同一订单严格串行 ——
// 避免「两个并发请求都判为未提交、都发一次 HTTP」。
type okxIdemEntry struct {
	mu   sync.Mutex
	resp SubmitResponse
	done bool // true 表示已成功提交，后续调用直接回放
}

// NewOKXChannel 构造 OKX 通道。缺任一凭证即报错（不 panic、不降级）。
//
// # 为什么缺凭证要报错而不是「先构造、下单时再报」
//
// vaultd 启动时就该知道「这个通道能不能用」。带着残缺配置启动，
// 会在第一次真实下单时才失败 —— 而那一刻调用方以为通道是好的。
func NewOKXChannel(cfg OKXConfig) (*OKXChannel, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("okx: api_key 为空")
	}
	if strings.TrimSpace(cfg.SecretKey) == "" {
		return nil, errors.New("okx: api_secret 为空")
	}
	if strings.TrimSpace(cfg.Passphrase) == "" {
		return nil, errors.New("okx: passphrase 为空")
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		base = okxDefaultBaseURL
	}
	client := &http.Client{Timeout: okxTimeout}
	// 可选：把指定域名钉到固定 IP（见 OKXConfig.ResolveHost 注释）。
	if h := strings.TrimSpace(cfg.ResolveHost); h != "" {
		ip := strings.TrimSpace(cfg.ResolveIP)
		if ip == "" {
			return nil, errors.New("okx: 配置了 resolve_host 但 resolve_ip 为空")
		}
		dialer := &net.Dialer{Timeout: 10 * time.Second}
		client.Transport = &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				if host, port, err := net.SplitHostPort(addr); err == nil && host == h {
					addr = net.JoinHostPort(ip, port)
				}
				return dialer.DialContext(ctx, network, addr)
			},
			TLSHandshakeTimeout: 10 * time.Second,
		}
	}
	return &OKXChannel{
		cfg:    cfg,
		base:   base,
		client: client,
	}, nil
}

// Submit 提交一笔委托。
//
// # 幂等
//
// 见 OKXChannel.idem 的注释：内存表挡单进程重复，clOrdId 挡跨重启重复。
func (c *OKXChannel) Submit(req SubmitRequest) (SubmitResponse, error) {
	key := strings.TrimSpace(req.ClientOrderID)
	if key == "" {
		return SubmitResponse{}, errors.New("okx: client_order_id 不能为空（它是幂等键）")
	}

	e := c.entry(key)
	e.mu.Lock()
	defer e.mu.Unlock()

	// 已成功提交过：直接回放，不再发包。
	if e.done {
		return e.resp, nil
	}

	instID, err := okxInstID(req.Symbol)
	if err != nil {
		return SubmitResponse{}, err
	}
	body, err := okxOrderBody(req, instID)
	if err != nil {
		return SubmitResponse{}, err
	}

	const path = "/api/v5/trade/order"
	raw, err := c.do(http.MethodPost, path, body)
	if err != nil {
		return SubmitResponse{}, err
	}

	var out okxOrderResp
	if err := json.Unmarshal(raw, &out); err != nil {
		return SubmitResponse{}, fmt.Errorf("okx: 下单响应不是合法 JSON: %w", err)
	}
	if out.Code != "0" {
		return SubmitResponse{}, fmt.Errorf("okx: 下单被拒 code=%s msg=%s", out.Code, out.Msg)
	}
	if len(out.Data) == 0 {
		return SubmitResponse{}, errors.New("okx: 下单响应 data 为空")
	}
	// 单笔下单 data 只有一条；sCode 非 0 表示该笔被拒（HTTP 仍可能是 200）。
	if sc := out.Data[0].SCode; sc != "" && sc != "0" {
		return SubmitResponse{}, fmt.Errorf("okx: 下单被拒 sCode=%s sMsg=%s", sc, out.Data[0].SMsg)
	}

	resp := SubmitResponse{
		ChannelOrderID: out.Data[0].OrdID,
		AcceptedAt:     time.Now().UTC().Format(time.RFC3339),
		RawAck:         json.RawMessage(raw),
	}
	e.resp = resp
	e.done = true
	return resp, nil
}

// Query 查回执。
//
// # 为什么只走 orders-history，不走 GET /api/v5/trade/order
//
// OKX 的按单查询 `/api/v5/trade/order` **必须同时给 instId**，
// 而 QueryRequest 里没有 symbol 字段（上层不打算在这里传品种）。
// 猜一个品种的后果是「订单不存在」——把真实订单误判成丢失，比查不到更糟。
//
// 所以统一走 `/api/v5/trade/orders-history`（只按 instType + 时间区间），
// 再在本地按 client_order_id 过滤。多一次本地过滤，换来「不会误判订单丢失」。
func (c *OKXChannel) Query(req QueryRequest) (QueryResponse, error) {
	resp, err := c.queryHistory(req)
	if err != nil {
		return QueryResponse{}, err
	}
	cid := strings.TrimSpace(req.ClientOrderID)
	if cid == "" {
		return resp, nil
	}
	filtered := make([]ChannelOrder, 0, 1)
	for _, o := range resp.Orders {
		if o.ClientOrderID == cid {
			filtered = append(filtered, o)
		}
	}
	return QueryResponse{Orders: filtered}, nil
}

// queryHistory 按时间区间查历史订单。
//
// # 为什么 from/to 缺省时不报错
//
// 上层用 client_order_id 查单时不传时间区间（QueryRequest 允许二选一）。
// 报错会让「按幂等键对账」这条最常用的路径直接不可用。
// 缺省时回退到「最近 7 天」——OKX 的 orders-history 单次最多查 7 天，
// 这个默认值既在接口限制内，也覆盖了「刚下完单来对账」的场景。
func (c *OKXChannel) queryHistory(req QueryRequest) (QueryResponse, error) {
	now := time.Now().UTC()
	beginRaw, endRaw := strings.TrimSpace(req.From), strings.TrimSpace(req.To)
	if beginRaw == "" {
		beginRaw = now.Add(-7 * 24 * time.Hour).Format(time.RFC3339)
	}
	if endRaw == "" {
		endRaw = now.Format(time.RFC3339)
	}

	begin, err := toMillis(beginRaw)
	if err != nil {
		return QueryResponse{}, fmt.Errorf("okx: from 不合法: %w", err)
	}
	end, err := toMillis(endRaw)
	if err != nil {
		return QueryResponse{}, fmt.Errorf("okx: to 不合法: %w", err)
	}
	path := fmt.Sprintf("/api/v5/trade/orders-history?instType=SPOT&begin=%d&end=%d&limit=100", begin, end)
	raw, err := c.do(http.MethodGet, path, nil)
	if err != nil {
		return QueryResponse{}, err
	}
	var out okxOrderResp
	if err := json.Unmarshal(raw, &out); err != nil {
		return QueryResponse{}, fmt.Errorf("okx: 历史查单响应不是合法 JSON: %w", err)
	}
	if out.Code != "0" {
		return QueryResponse{}, fmt.Errorf("okx: 历史查单被拒 code=%s msg=%s", out.Code, out.Msg)
	}
	orders := make([]ChannelOrder, 0, len(out.Data))
	for _, d := range out.Data {
		orders = append(orders, ChannelOrder{
			ClientOrderID:  d.ClOrdID,
			ChannelOrderID: d.OrdID,
			State:          okxState(d.State),
			FilledQty:      d.AccFillSz,
			AvgPrice:       d.AvgPx,
			UpdatedAt:      msToRFC3339(d.UTime),
		})
	}
	return QueryResponse{Orders: orders}, nil
}

// Health 打公开的时间接口。
//
// # 为什么用 /api/v5/public/time 而不是签名接口
//
// 健康检查只回答「OKX 的 REST 还活着吗」。用签名接口会把「凭证配错了」
// 和「OKX 挂了」混成同一个错误，运维分不清该修什么。
// 凭证问题应该在启动时（NewOKXChannel）暴露，不在健康检查里。
func (c *OKXChannel) Health() error {
	raw, err := c.do(http.MethodGet, "/api/v5/public/time", nil)
	if err != nil {
		return err
	}
	var out struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("okx: 健康检查响应不是合法 JSON: %w", err)
	}
	if out.Code != "0" {
		return fmt.Errorf("okx: 健康检查失败 code=%s msg=%s", out.Code, out.Msg)
	}
	return nil
}

// ---- 内部 ----

// entry 取（或建）某个 clientOrderID 的幂等条目。
func (c *OKXChannel) entry(key string) *okxIdemEntry {
	if v, ok := c.idem.Load(key); ok {
		return v.(*okxIdemEntry)
	}
	v, _ := c.idem.LoadOrStore(key, &okxIdemEntry{})
	return v.(*okxIdemEntry)
}

// do 发一个请求并返回响应体。
//
// body 为 nil 表示 GET（body 为空串参与签名）。
// 错误信息里**不含**任何 header 值。
//
// # 公开接口不签名
//
// `/api/v5/public/*` 不需要鉴权。给它带上 OK-ACCESS-KEY/SIGN 是**多余的凭证外发**：
// 每多一个收到凭证的端点，就多一个可能记录它的服务。
// 所以签名只在真正需要它的路径上发生。
func (c *OKXChannel) do(method, path string, body []byte) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return nil, fmt.Errorf("okx: 构造请求失败: %w", err)
	}

	if !strings.HasPrefix(path, "/api/v5/public/") {
		ts := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
		prehash := ts + method + path + string(body)
		mac := hmac.New(sha256.New, []byte(c.cfg.SecretKey))
		mac.Write([]byte(prehash))
		req.Header.Set("OK-ACCESS-KEY", c.cfg.APIKey)
		req.Header.Set("OK-ACCESS-SIGN", base64.StdEncoding.EncodeToString(mac.Sum(nil)))
		req.Header.Set("OK-ACCESS-TIMESTAMP", ts)
		req.Header.Set("OK-ACCESS-PASSPHRASE", c.cfg.Passphrase)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.cfg.Demo {
		req.Header.Set("x-simulated-trading", "1")
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("okx: 请求 %s 失败: %w", path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("okx: 读取响应失败: %w", err)
	}
	// OKX 的业务错误也走 HTTP 200，所以这里不能只看状态码 ——
	// 但非 200 至少要留下状态码，否则「为什么失败」无从判断。
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("okx: 请求 %s 返回 HTTP %d", path, resp.StatusCode)
	}
	return raw, nil
}

// okxInstID 把内部 symbol 转成 OKX 的 instId（BTC/USDT → BTC-USDT）。
func okxInstID(symbol string) (string, error) {
	s := strings.ToUpper(strings.TrimSpace(symbol))
	s = strings.ReplaceAll(s, "/", "-")
	s = strings.ReplaceAll(s, "_", "-")
	if s == "" {
		return "", errors.New("okx: symbol 不能为空")
	}
	if !strings.Contains(s, "-") {
		return "", fmt.Errorf("okx: symbol %q 不是 instId 形式（期望 BTC-USDT 或 BTC/USDT）", symbol)
	}
	return s, nil
}

// okxOrderBody 组装下单请求体。
func okxOrderBody(req SubmitRequest, instID string) ([]byte, error) {
	side := strings.ToLower(strings.TrimSpace(req.Side))
	if side != "buy" && side != "sell" {
		return nil, fmt.Errorf("okx: side 必须是 buy 或 sell，实际 %q", req.Side)
	}
	ordType := strings.ToLower(strings.TrimSpace(req.OrderType))
	if ordType == "" {
		ordType = "market"
	}
	if ordType != "market" && ordType != "limit" {
		return nil, fmt.Errorf("okx: ordType 只支持 market/limit，实际 %q", req.OrderType)
	}

	m := map[string]string{
		"instId":  instID,
		"tdMode":  "cash",
		"side":    side,
		"ordType": ordType,
		"sz":      strings.TrimSpace(req.Qty),
		"clOrdId": strings.TrimSpace(req.ClientOrderID),
	}
	// 现货 sz 的语义：base_ccy=币量（OKX 默认），quote_ccy=金额。
	//
	// # 为什么要把这个字段透传下去
	//
	// OKX 现货最小下单额是 **1 USDT 等值**。按币量下小额单会被拒，
	// 而错误码只有笼统的 "All operations failed"，看不出原因：
	//
	//	sz=0.0001 BTC → 500 / All operations failed
	//	sz=10 + quote_ccy → 成功
	//
	// 模型算仓位时习惯用「金额」（如「买入 10 USDT」），所以这条通道
	// 必须能把金额语义传下去，否则每笔小额单都会被 OKX 拒。
	//
	// 只对现货（tdMode=cash）有意义，而本函数固定 cash —— 无需额外判断。
	if tc := strings.TrimSpace(req.TgtCcy); tc != "" {
		switch tc {
		case "base_ccy", "quote_ccy":
			m["tgtCcy"] = tc
		default:
			return nil, fmt.Errorf("okx: tgt_ccy 只支持 base_ccy/quote_ccy，实际 %q", tc)
		}
	}

	// 限价单必须有价格；市价单带了价格反而会被 OKX 拒。
	if ordType == "limit" {
		px := strings.TrimSpace(req.Price)
		if px == "" {
			return nil, errors.New("okx: 限价单必须提供 price")
		}
		m["px"] = px
	}
	// 说明：OKX v5 的下单接口没有独立的 TIF 字段，时效性通过 ordType
	// 表达（ioc/fok/post_only）。这里不透传 SubmitRequest.TIF，
	// 需要 IOC/FOK 时由调用方用 order_type 指定。

	body, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("okx: 序列化下单请求失败: %w", err)
	}
	return body, nil
}

// okxOrderResp 是下单/查单的共用响应结构（OKX 两者字段高度重合）。
type okxOrderResp struct {
	Code string `json:"code"`
	Msg  string `json:"msg"`
	Data []struct {
		OrdID     string `json:"ordId"`
		ClOrdID   string `json:"clOrdId"`
		SCode     string `json:"sCode"`
		SMsg      string `json:"sMsg"`
		State     string `json:"state"`
		AccFillSz string `json:"accFillSz"`
		AvgPx     string `json:"avgPx"`
		UTime     string `json:"uTime"`
	} `json:"data"`
}

// okxState 把 OKX 的 state 映射成内部状态名。
//
// 不直接透传 OKX 原始值，是为了让上层（exec/对账）只依赖我方词表。
func okxState(s string) string {
	switch strings.ToLower(s) {
	case "live":
		return "pending"
	case "partially_filled":
		return "partially_filled"
	case "filled":
		return "filled"
	case "canceled", "cancelled":
		return "canceled"
	case "mmp_canceled":
		return "canceled"
	default:
		if s == "" {
			return "unknown"
		}
		return strings.ToLower(s)
	}
}

// toMillis 把 RFC3339 或纯毫秒时间戳转成毫秒。
func toMillis(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("为空")
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UnixMilli(), nil
	}
	var ms int64
	if _, err := fmt.Sscanf(s, "%d", &ms); err == nil {
		return ms, nil
	}
	return 0, fmt.Errorf("%q 既不是 RFC3339 也不是毫秒时间戳", s)
}

// msToRFC3339 把 OKX 的毫秒字符串转成 RFC3339（失败返回原样）。
func msToRFC3339(s string) string {
	var ms int64
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &ms); err != nil || ms <= 0 {
		return s
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}
