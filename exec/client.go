package exec

// Client 是 artex 主进程调 vaultd 的 HTTP 客户端。
//
// # 为什么不直接用 net/http
//
// ① token 从 vaultd 的 stdout 首行读（进程启动时）—— 不是配置文件、不是环境变量。
//    这个「怎么拿到 token」的知识属于本客户端。
// ② 所有请求带 Bearer，且 **token 不能出现在任何错误信息里**（见 redactErr）。
// ③ 错误要映射成结构化 code，让调用方按码分支而不是匹配字符串。
//
// # 本机进程为什么要短超时
//
// vaultd 在 127.0.0.1 上，正常响应是毫秒级。
// 30s 的默认超时会让「vaultd 挂了」表现为「下单卡 30 秒」——
// 而用户会以为「系统在思考」，实际是「在等一个永远不会来的响应」。
//
// 2s 足够覆盖冷启动后的首次请求，且让故障立刻暴露。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Autumn-27/artex/redact"
)

// Client 默认超时。
//
// # 为什么是 2s
//
// 见包注释：本机回环地址，正常响应毫秒级。
// 超时的意义不是「等久一点」，是「尽早知道等不到」。
const defaultClientTimeout = 2 * time.Second

// vaultd 返回的错误码（与 exec/vaultd/server.go 的常量一一对应）。
//
// # 为什么在 client 侧再定义一遍
//
// exec 包不能 import exec/vaultd（那会把 vaultd 的通道实现拖进决策进程，
// 破坏「编译期隔离」——见 scripts/check-vaultd-isolation.sh）。
//
// 所以这里是**协议常量**，两边各自定义 —— 它们是 HTTP 契约的一部分，
// 不是代码依赖。
const (
	VaultCodeUnauthorized       = "VAULT_UNAUTHORIZED"
	VaultCodeBadRequest         = "VAULT_BAD_REQUEST"
	VaultCodeChannelInvalid     = "VAULT_CHANNEL_INVALID"
	VaultCodeNotImplemented     = "VAULT_NOT_IMPLEMENTED"
	VaultCodeNotFound           = "VAULT_NOT_FOUND"
	VaultCodeChannelUnavailable = "VAULT_CHANNEL_UNAVAILABLE"
)

// 客户端侧的哨兵错误。
var (
	// ErrVaultUnauthorized 表示 vaultd 拒绝了 token。
	//
	// # 为什么单独一个哨兵
	//
	// 「token 错了」和「vaultd 挂了」的应对完全不同：
	// 前者要检查启动时的 token 传递；后者要检查进程是否活着。
	// 都返回「调用失败」会让排查方向丢失。
	ErrVaultUnauthorized = errors.New("vaultd: token 被拒绝")

	// ErrVaultUnavailable 表示 vaultd 不可达（连接失败 / 超时 / 5xx）。
	ErrVaultUnavailable = errors.New("vaultd: 不可达")

	// ErrVaultChannelUnavailable 表示通道未配置（503）。
	//
	// 这与「vaultd 挂了」不同 —— 前者是「vaultd 活着但没接通道」，
	// 那是预期的部署状态（WE-6/7 未做）。
	ErrVaultChannelUnavailable = errors.New("vaultd: 通道未配置")
)

// Client 是 vaultd 的 HTTP 客户端。
type Client struct {
	base  string
	token string
	http  *http.Client
}

// NewClient 构造客户端。
//
// base 形如 "http://127.0.0.1:12345"。
func NewClient(base, token string) *Client {
	return &Client{
		base:  strings.TrimRight(strings.TrimSpace(base), "/"),
		token: strings.TrimSpace(token),
		http:  &http.Client{Timeout: defaultClientTimeout},
	}
}

// SubmitRequest 是 POST /v1/submit 的请求体。
//
// # ClientOrderID 必须由调用方提供
//
// 如果 Client 自己生成，那「重试」会生成新的 id —— 幂等失效。
// 幂等的前提是「同一个业务意图用同一个 id」，
// 而「是不是同一个业务意图」只有调用方知道。
type SubmitRequest struct {
	Channel       string `json:"channel"`
	AccountRef    string `json:"account_ref"`
	ClientOrderID string `json:"client_order_id"`
	Symbol        string `json:"symbol"`
	Side          string `json:"side"`
	Qty           string `json:"qty"`
	OrderType     string `json:"order_type"`
	Price         string `json:"price,omitempty"`
	TIF           string `json:"tif,omitempty"`
	// TgtCcy 见 vaultd.SubmitRequest.TgtCcy：base_ccy（按币量）| quote_ccy（按金额）。
	// 现货小额单必须用 quote_ccy —— OKX 现货最小下单额是 1 USDT 等值。
	TgtCcy string `json:"tgt_ccy,omitempty"`
}

// SubmitResponse 是 POST /v1/submit 的响应。
type SubmitResponse struct {
	ChannelOrderID string          `json:"channel_order_id"`
	AcceptedAt     string          `json:"accepted_at"`
	RawAck         json.RawMessage `json:"raw_ack"`
}

// QueryRequest 是 POST /v1/query 的请求体。
type QueryRequest struct {
	Channel       string `json:"channel"`
	AccountRef    string `json:"account_ref"`
	ClientOrderID string `json:"client_order_id,omitempty"`
	From          string `json:"from,omitempty"`
	To            string `json:"to,omitempty"`
}

// VaultOrder 是通道侧的一笔订单（对账输入）。
//
// # 为什么不叫 ChannelAck
//
// 本包已有 `ChannelAck`（reconcile.go 里定义），那是「对账用的通道回执抽象」，
// 含 Found/State/FilledQty/AvgPrice/Raw 六个字段。
//
// 而这里是「vaultd HTTP 协议里的订单结构」—— 字段是 JSON 标签驱动的。
// 两者语义相近但契约不同（一个是内部抽象，一个是线协议），
// 共用名字会让「我该用哪个」变成每次都要想的问题。
type VaultOrder struct {
	ClientOrderID  string `json:"client_order_id"`
	ChannelOrderID string `json:"channel_order_id"`
	State          string `json:"state"`
	FilledQty      string `json:"filled_qty"`
	AvgPrice       string `json:"avg_price"`
	UpdatedAt      string `json:"updated_at"`
}

// Submit 提交订单。
//
// 幂等：同 client_order_id 两次不重复下单（由 vaultd 侧保证）。
func (c *Client) Submit(ctx context.Context, req SubmitRequest) (*SubmitResponse, error) {
	// 本地先校验幂等键 —— 不发出去就知道错了。
	//
	// # 为什么在 Client 侧也校验一遍（vaultd 也会校验）
	//
	// 一次网络往返的成本远大于一次字符串检查。
	// 而「漏了 client_order_id」是明显的调用方 bug，早失败早暴露。
	if strings.TrimSpace(req.ClientOrderID) == "" {
		return nil, fmt.Errorf("%w: client_order_id 不能为空（它是幂等键，缺了会导致重试重复下单）",
			ErrVaultBadRequest)
	}

	var out SubmitResponse
	if err := c.post(ctx, "/v1/submit", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Query 查回执。
func (c *Client) Query(ctx context.Context, req QueryRequest) ([]VaultOrder, error) {
	// 本地校验：两种查询方式至少要有一个。
	//
	// 与 vaultd 侧的校验一致 —— 「要么给 client_order_id，要么给 from+to」。
	if strings.TrimSpace(req.ClientOrderID) == "" &&
		(strings.TrimSpace(req.From) == "" || strings.TrimSpace(req.To) == "") {
		return nil, fmt.Errorf("%w: 必须提供 client_order_id，或同时提供 from + to",
			ErrVaultBadRequest)
	}

	var out struct {
		Orders []VaultOrder `json:"orders"`
	}
	if err := c.post(ctx, "/v1/query", req, &out); err != nil {
		return nil, err
	}
	if out.Orders == nil {
		out.Orders = []VaultOrder{}
	}
	return out.Orders, nil
}

// Health 探活。
//
// # 怎么探
//
// vaultd 没有 /health 接口（对外只有 3 个 POST，且明确不许加）。
// 所以用「一个必然 400 的 POST」来探 —— 能返回 400 说明进程活着且 token 有效。
//
// # 为什么不用 /v1/query 探
//
// query 需要真实参数；而「发送一个空请求体」能得到 400，
// 那已经证明了「TCP 通 + token 通过 + handler 在跑」三件事。
func (c *Client) Health(ctx context.Context) error {
	// 空 body 的 submit：会被 vaultd 判 400（缺字段）——
	// 但那正是「活着」的证据。
	body := bytes.NewBufferString("{}")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/submit", body)
	if err != nil {
		return fmt.Errorf("构造探活请求失败: %w", redactErr(err, c.token))
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrVaultUnavailable, redactErr(err, c.token))
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return ErrVaultUnauthorized
	case resp.StatusCode == http.StatusBadRequest:
		// 400 = 活着 + token 对 + 只是参数不全。这正是我们要的。
		return nil
	case resp.StatusCode >= 500:
		return fmt.Errorf("%w: status %d", ErrVaultUnavailable, resp.StatusCode)
	default:
		// 其它状态码（404/501 等）说明进程活着，但我们的探活方式不对。
		// 仍返回 nil（活着），但带上状态码让调用方知道。
		return nil
	}
}

// post 是统一的 POST + JSON 解码 + 错误映射。
func (c *Client) post(ctx context.Context, path string, in, out any) error {
	raw, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("序列化请求失败: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("构造请求失败: %w", redactErr(err, c.token))
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		// # 这里是 token 最可能泄漏的地方
		//
		// http.Client 的 *url.Error 会带上完整 URL；如果将来有人把 token
		// 放进 query string，错误信息就会带出 token。
		//
		// 所以所有 error 在返回前必须过 redactErr —— 它把 token 替换掉。
		return fmt.Errorf("%w: %v", ErrVaultUnavailable, redactErr(err, c.token))
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("%w: 读响应失败: %v", ErrVaultUnavailable, redactErr(err, c.token))
	}

	if resp.StatusCode == http.StatusOK {
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("解析响应失败: %w", redactErr(err, c.token))
		}
		return nil
	}

	// ---- 错误映射 ----
	return c.mapHTTPError(resp.StatusCode, respBody)
}

// mapHTTPError 把 vaultd 的错误响应映射成哨兵错误。
//
// # 为什么用哨兵而不是 fmt.Errorf 带状态码
//
// 调用方需要区分「token 错」和「vaultd 挂了」——
// 前者要检查启动时的 token 传递，后者要检查进程是否活着。
// 而字符串匹配状态码会让这个区分依赖文案，文案一改就坏。
func (c *Client) mapHTTPError(status int, body []byte) error {
	var payload struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	_ = json.Unmarshal(body, &payload)

	msg := strings.TrimSpace(payload.Error)
	if msg == "" {
		msg = http.StatusText(status)
	}
	msg = redact.Mask(msg)

	switch {
	case status == http.StatusUnauthorized:
		return fmt.Errorf("%w: %s", ErrVaultUnauthorized, msg)
	case status == http.StatusServiceUnavailable:
		// 503 = vaultd 活着但通道没接（WE-6/7 未做的预期状态）
		return fmt.Errorf("%w: %s", ErrVaultChannelUnavailable, msg)
	case status == http.StatusBadRequest:
		return fmt.Errorf("%w: %s", ErrVaultBadRequest, msg)
	case status == http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrVaultNotFound, msg)
	case status >= 500:
		return fmt.Errorf("%w: status %d: %s", ErrVaultUnavailable, status, msg)
	default:
		return fmt.Errorf("vaultd 返回 %d: %s", status, msg)
	}
}

// ---- 额外的哨兵 ----

// ErrVaultBadRequest 表示请求参数问题（vaultd 判 400）。
var ErrVaultBadRequest = errors.New("vaultd: 请求参数错误")

// ErrVaultNotFound 表示接口不存在（vaultd 判 404）。
var ErrVaultNotFound = errors.New("vaultd: 接口不存在")

// redactErr 把 error 里的 token 抹掉。
//
// # 为什么必须是「抹掉」而不是「不打印」
//
// 因为 http.Client 返回的 *url.Error 会带上 URL 和底层错误，
// 而我们无法控制它内部拼了什么。
//
// 唯一可靠的做法是：**拿到最终字符串，把已知的 token 子串替换掉**。
//
// 这不是「防御性编程」，是「token 可能的泄漏路径只有一条，就是错误信息」——
// 把它堵住，就不需要担心别的。
func redactErr(err error, token string) error {
	if err == nil {
		return nil
	}
	s := err.Error()
	if token != "" {
		s = strings.ReplaceAll(s, token, "***")
	}
	// 再跑一遍通用脱敏（错误信息里可能带别的凭证形态）。
	s = redact.Mask(s)
	return errors.New(s)
}
