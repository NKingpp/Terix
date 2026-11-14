package okxrest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// 错误分类。调用方按 errors.Is 分支，不按文案匹配 —— 文案会改。
var (
	// ErrDuplicate 表示 clOrdId 已被使用（51603）。
	//
	// 这不是失败：上一轮可能已经下成了，只是响应丢了。调用方应转
	// QueryOrderByClOrdID 取真实状态。
	ErrDuplicate = errors.New("okx: 重复的 client order id")
	// ErrPermanent 表示重试无意义（参数错/余额不足/量过小）。
	ErrPermanent = errors.New("okx: 永久失败")
	// ErrRetryable 表示限流或瞬时故障，下一轮同 clOrdId 重试即可。
	ErrRetryable = errors.New("okx: 可重试失败")
)

// APIError 是 OKX 业务错误（HTTP 200 但 code != "0"，或 HTTP 非 200）。
type APIError struct {
	HTTP   int
	Code   string // 顶层 code
	SCode  string // data[0].sCode（下单类接口）
	Msg    string
	Detail string
	Kind   error // ErrDuplicate / ErrPermanent / ErrRetryable，可为 nil
}

func (e *APIError) Error() string {
	parts := make([]string, 0, 4)
	if e.HTTP != 0 && e.HTTP != http.StatusOK {
		parts = append(parts, "http="+strconv.Itoa(e.HTTP))
	}
	if e.Code != "" {
		parts = append(parts, "code="+e.Code)
	}
	if e.SCode != "" && e.SCode != "0" {
		parts = append(parts, "sCode="+e.SCode)
	}
	if e.Msg != "" {
		parts = append(parts, e.Msg)
	}
	if e.Detail != "" {
		parts = append(parts, e.Detail)
	}
	return "okx: " + strings.Join(parts, " ")
}

// Unwrap 让 errors.Is(err, ErrDuplicate) 能穿透。
func (e *APIError) Unwrap() error { return e.Kind }

// client 是内部实现；对外只暴露 Client 的方法集。
type client struct {
	creds  Credentials
	http   *http.Client
	base   string
	origin string // 用于测试断言
}

// Client 是 OKX REST 客户端。
type Client struct {
	c *client
}

// NewClient 构造客户端。baseURL 为空则用官方域名。
func NewClient(creds Credentials) *Client {
	base := strings.TrimRight(strings.TrimSpace(creds.BaseURL), "/")
	if base == "" {
		base = "https://www.okx.com"
	}
	return &Client{c: &client{
		creds: creds,
		base:  base,
		// 15s：下单/查单都在这个量级内完成。过长会让 tick 卡住。
		http: &http.Client{Timeout: 15 * time.Second},
	}}
}

// do 执行一次签名请求并解码 OKX 信封。
//
// OKX 的业务错误也走 HTTP 200，所以必须看 body 里的 code，
// 不能只看状态码。
func (cl *client) do(ctx context.Context, method, path string, body []byte, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, cl.base+path, rd)
	if err != nil {
		return fmt.Errorf("okx: 构造请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	setAuthHeaders(req, cl.creds, body)

	resp, err := cl.http.Do(req)
	if err != nil {
		return &APIError{Msg: "请求失败", Detail: err.Error(), Kind: ErrRetryable}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return &APIError{HTTP: resp.StatusCode, Msg: "读取响应失败", Detail: err.Error(), Kind: ErrRetryable}
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return &APIError{HTTP: resp.StatusCode, Msg: "服务端限流或故障", Detail: truncate(string(raw), 300), Kind: ErrRetryable}
	}
	if resp.StatusCode != http.StatusOK {
		return &APIError{HTTP: resp.StatusCode, Msg: "非 200", Detail: truncate(string(raw), 300), Kind: ErrPermanent}
	}

	var env struct {
		Code string          `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return &APIError{Msg: "响应不是合法 JSON", Detail: truncate(string(raw), 300), Kind: ErrRetryable}
	}
	if env.Code != "0" && env.Code != "" {
		return &APIError{Code: env.Code, Msg: env.Msg, Detail: truncate(string(env.Data), 300), Kind: classifyCode(env.Code)}
	}
	if out != nil && len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return &APIError{Msg: "解析 data 失败", Detail: err.Error(), Kind: ErrRetryable}
		}
	}
	return nil
}

// classifyOrderSCode 把**单笔订单的 sCode** 映射成三类。
//
// # 为什么必须单独一个函数（2026-11-14 实测事故）
//
// 下单类接口的失败在 OKX 侧是「顶层 code=1 + data[0].sCode=<具体原因>」。
// classifyCode 只认顶层 code，"1" 不在它的表里 → 落到 ErrRetryable →
// TradeLoop 每 15 秒重试同一张注定失败的草案，永不停止。
//
// 实测：sCode=51050「Your TP price should be higher than the primary order
// price」，一张草案被重试了 4 次以上，日志里全是同一条 WARN。
//
// 判据：sCode 是**参数/规则校验**类 → 永久失败（同参数重试无意义）；
// 只有限流与系统繁忙才值得重试；51603（clOrdId 重复）单独识别。
func classifyOrderSCode(sCode string) error {
	switch sCode {
	case "51603":
		return ErrDuplicate
	case "50011", "50004", "50061", "50013":
		return ErrRetryable
	}
	if sCode == "" || sCode == "0" {
		return nil
	}
	// 其余 sCode 一律永久失败：它们是「这张单的参数不被接受」。
	// 宁可少重试，也不要无限重试一张必然失败的单 —— 后者会掩盖真正的问题。
	return ErrPermanent
}

// classifyCode 把 OKX 顶层 code 映射成三类。
//
// 只覆盖会造成实际资金后果的几种；其余一律当可重试（宁可多试一次，
// 也不要因为一个没见过的码就把订单永久放弃）。
func classifyCode(code string) error {
	switch code {
	case "51603": // clOrdId 重复
		return ErrDuplicate
	case "51000", "51008", "51020", "51004", "51121": // 参数错 / 余额不足 / 量过小 / 标的不可用 / 无持仓
		return ErrPermanent
	case "50011", "50004", "50061": // 限流 / 超时 / 系统繁忙
		return ErrRetryable
	}
	return ErrRetryable
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// doPublic 执行一次**不签名**的公共请求。
//
// # 为什么要单独一条路径
//
// do 无条件加 OK-ACCESS-* 头。公共接口（行情）虽然通常容忍多余的头，
// 但凭证失效时带上无效签名会被 50113/50111 拒 —— 那时连价格都读不到，
// 而读价格本来根本不需要身份。
func (cl *client) doPublic(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cl.base+path, nil)
	if err != nil {
		return fmt.Errorf("okx: 构造请求失败: %w", err)
	}
	resp, err := cl.http.Do(req)
	if err != nil {
		return &APIError{Msg: "请求失败", Detail: err.Error(), Kind: ErrRetryable}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return &APIError{HTTP: resp.StatusCode, Msg: "读取响应失败", Detail: err.Error(), Kind: ErrRetryable}
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return &APIError{HTTP: resp.StatusCode, Msg: "限流或故障", Detail: truncate(string(raw), 300), Kind: ErrRetryable}
	}
	if resp.StatusCode != http.StatusOK {
		return &APIError{HTTP: resp.StatusCode, Msg: "非 200", Detail: truncate(string(raw), 300), Kind: ErrPermanent}
	}
	var env struct {
		Code string          `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return &APIError{Msg: "响应不是合法 JSON", Detail: truncate(string(raw), 300), Kind: ErrRetryable}
	}
	if env.Code != "0" && env.Code != "" {
		return &APIError{Code: env.Code, Msg: env.Msg, Detail: truncate(string(env.Data), 300), Kind: classifyCode(env.Code)}
	}
	if out != nil && len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return &APIError{Msg: "解析 data 失败", Detail: err.Error(), Kind: ErrRetryable}
		}
	}
	return nil
}
