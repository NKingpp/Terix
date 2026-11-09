package vaultd

// vaultd 的 HTTP 服务（WE-5 骨架）。
//
// # 这个进程的职责边界
//
// `vaultd` 是**唯一持有券商凭证**的进程。它与决策进程（`artex`）的关系：
//
//	artex（决策 + HTTP + 前端）  ←127.0.0.1 + Bearer token→  vaultd（凭证 + 通道）
//
// 隔离是**物理的**（独立二进制、独立进程），不是约定 ——
// 因为 `vaultd` 不 import `agent`/`server`/`llmpool`，
// 编译器层面就无法把凭证泄进决策路径。
//
// # 为什么只暴露 3 个接口
//
// | 接口 | 作用 |
// |---|---|
// | POST /v1/sign   | 只签名不外发（P0 可 501） |
// | POST /v1/submit | 唯一能真实下单的入口 |
// | POST /v1/query  | 拉回执，对账专用 |
//
// **明确的非目标**：不提供 `GET /credentials`、不提供「取余额」、不提供通用代理。
// 每多一个接口，就多一条凭证可能外流的路径。
//
// # token 的生命周期
//
// 每次启动生成 32 字节随机 token，打印到 stdout 首行。
// `artex` 由父进程管道读取后写内存 —— **不落库、不落日志、不进环境变量**。
// 这三条是「凭证不落地」的具体要求。

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/Autumn-27/artex/redact"
)

// 通道名白名单。与 exec_authorizations 的 channel 取值一致。
const (
	ChannelFosun = "fosunwealth"
	ChannelOKX   = "okx"
)

var validChannels = map[string]bool{
	ChannelFosun: true,
	ChannelOKX:   true,
}

// 错误码。调用方（exec.Client）按码判断，不靠字符串匹配。
const (
	CodeUnauthorized       = "VAULT_UNAUTHORIZED"
	CodeBadRequest         = "VAULT_BAD_REQUEST"
	CodeChannelInvalid     = "VAULT_CHANNEL_INVALID"
	CodeNotImplemented     = "VAULT_NOT_IMPLEMENTED"
	CodeNotFound           = "VAULT_NOT_FOUND"
	CodeChannelUnavailable = "VAULT_CHANNEL_UNAVAILABLE"
)

// Server 是 vaultd 的 HTTP 服务。
type Server struct {
	token    string
	channels map[string]Channel
}

// Channel 是「券商通道」的抽象。
//
// # 为什么是接口而 P0 只有 fake 实现
//
// 真实实现（fosun sidecar / okx CLI）是 WE-6/WE-7。
// 但 WE-14 的泄漏测试需要一条「会打印凭证到日志」的假通道 ——
// 用它验证 redact 真的拦住了。
//
// 所以：接口先定下来，P0 给一个 `fakeChannel`（测试用），
// 真实通道后补，不改接口。
type Channel interface {
	// Submit 提交一笔委托。
	//
	// # 幂等要求
	//
	// 同一个 clientOrderID 两次调用必须不重复下单。
	// 实现方负责去重（内存 map 或券商侧的 clientOrderId 幂等键）。
	Submit(req SubmitRequest) (SubmitResponse, error)

	// Query 查回执。
	Query(req QueryRequest) (QueryResponse, error)

	// Health 检查通道可用性。
	Health() error
}

// SubmitRequest 是 POST /v1/submit 的请求体。
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
	// TgtCcy 指定现货下单的 sz 是「币的数量」还是「计价币的金额」。
	//
	//   base_ccy  —— sz 是标的数量（如 BTC 0.001）
	//   quote_ccy —— sz 是计价币金额（如 USDT 10）
	//
	// # 为什么必须支持（2026-11-06 实测）
	//
	// 模拟盘按币量下小额单会被拒：
	//
	//	sz=0.0001（BTC）→ 500 / "All operations failed"
	//
	// 因为 OKX 现货最小下单额是 1 USDT 等值。改按金额下单就成功：
	//
	//	sz=10 & tgtCcy=quote_ccy → 订单 4074517127092490240 OK
	//
	// 留空时用 OKX 默认（base_ccy）—— 与改动前行为一致，不影响既有调用。
	TgtCcy string `json:"tgt_ccy,omitempty"`
}

// SubmitResponse 是 POST /v1/submit 的响应。
type SubmitResponse struct {
	ChannelOrderID string          `json:"channel_order_id"`
	AcceptedAt     string          `json:"accepted_at"`
	RawAck         json.RawMessage `json:"raw_ack"`
}

// QueryRequest 是 POST /v1/query 的请求体。
//
// 两种模式：按 client_order_id 查单笔，或按时间区间查多笔。
type QueryRequest struct {
	Channel       string `json:"channel"`
	AccountRef    string `json:"account_ref"`
	ClientOrderID string `json:"client_order_id,omitempty"`
	From          string `json:"from,omitempty"`
	To            string `json:"to,omitempty"`
}

// QueryResponse 是 POST /v1/query 的响应。
type QueryResponse struct {
	Orders []ChannelOrder `json:"orders"`
}

// ChannelOrder 是通道侧的一笔订单（脱敏后）。
type ChannelOrder struct {
	ClientOrderID  string `json:"client_order_id"`
	ChannelOrderID string `json:"channel_order_id"`
	State          string `json:"state"`
	FilledQty      string `json:"filled_qty"`
	AvgPrice       string `json:"avg_price"`
	UpdatedAt      string `json:"updated_at"`
}

// NewServer 构造 vaultd 服务。
//
// token 必须非空 —— 空 token 等于「不设防」。
//
// # 为什么这里没有「是否允许真实下单」开关了
//
// WE-5 骨架阶段 Server 上有一个 `submitOK bool`，恒 false。
// 当时的意图是「P0 不真下单，用一个开关硬挡」。问题是：它**没有任何地方读**——
// 下单的唯一闸门是 `channels[req.Channel]` 是否存在。也就是说，
// 只要装配了通道，这个开关拦不住任何东西；真加了读取逻辑，
// 反而会变成「装了通道还是 503」的假故障。
//
// 结论：删掉这个恒 false 的字段，让「有没有通道」成为唯一、且代码里真实生效的判断。
// 真实下单的闸门是：main 有没有用环境变量装配出通道。
func NewServer(token string, channels map[string]Channel) (*Server, error) {
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("token 不能为空（空 token 等于不设防）")
	}
	if channels == nil {
		channels = map[string]Channel{}
	}
	return &Server{token: token, channels: channels}, nil
}

// Handler 返回 vaultd 的 http.Handler。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1/sign", s.handleSign)
	mux.HandleFunc("POST /v1/submit", s.handleSubmit)
	mux.HandleFunc("POST /v1/query", s.handleQuery)

	// 明确的非目标：任何 GET（含 /v1/credentials）一律 404。
	//
	// # 为什么要显式拦
	//
	// 默认的 http.ServeMux 对未注册路径就返回 404 —— 但这里显式注册一个
	// 兜底 handler，是为了让「不提供 GET」成为**代码里的决定**，
	// 而不是「碰巧没注册」。
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeErr(w, http.StatusNotFound, "vaultd 不提供任何 GET 接口", CodeNotFound)
			return
		}
		writeErr(w, http.StatusNotFound, "未知接口", CodeNotFound)
	})

	return s.requireToken(mux)
}

// requireToken 是 Bearer token 鉴权中间件。
//
// # 为什么用 subtle.ConstantTimeCompare
//
// 普通的 `==` 会在第一个不同字节处短路，攻击者能通过响应时间逐字节猜出 token。
// 常量时间比较让每次比较耗时与内容无关。
func (s *Server) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(h, prefix) {
			writeErr(w, http.StatusUnauthorized, "缺少 Authorization: Bearer <token>", CodeUnauthorized)
			return
		}
		got := strings.TrimPrefix(h, prefix)
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			writeErr(w, http.StatusUnauthorized, "token 不匹配", CodeUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleSign(w http.ResponseWriter, _ *http.Request) {
	// P0 为 501：复星/OKX 走 SDK/CLI，不走「我方签名」这条路。
	// 这个接口是给「未来自研 HTTP 通道」留的位置。
	writeErr(w, http.StatusNotImplemented,
		"/v1/sign 在 P0 未实现（复星/OKX 走 SDK/CLI，不需要我方签名）", CodeNotImplemented)
}

func (s *Server) handleSubmit(w http.ResponseWriter, r *http.Request) {
	var req SubmitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("请求体格式错误：%v", err), CodeBadRequest)
		return
	}

	if msg, code := validateSubmit(&req); code != "" {
		writeErr(w, http.StatusBadRequest, msg, code)
		return
	}

	ch, ok := s.channels[req.Channel]
	if !ok {
		writeErr(w, http.StatusServiceUnavailable,
			fmt.Sprintf("通道 %q 未配置", req.Channel), CodeChannelUnavailable)
		return
	}

	resp, err := ch.Submit(req)
	if err != nil {
		writeErr(w, http.StatusBadGateway,
			fmt.Sprintf("通道提交失败：%v", err), CodeChannelUnavailable)
		return
	}

	// 出参前跑一遍 redact —— 通道的 raw_ack 可能带凭证。
	//
	// # 为什么在这里而不是在通道实现里
	//
	// 通道实现可能有多个（fosun / okx / 未来的），而「出参脱敏」是
	// vaultd 对外的统一承诺。放在边界上，一个地方保证所有通道都不泄漏。
	resp.RawAck = redactRaw(resp.RawAck)

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	var req QueryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("请求体格式错误：%v", err), CodeBadRequest)
		return
	}

	if !validChannels[req.Channel] {
		writeErr(w, http.StatusBadRequest,
			fmt.Sprintf("channel 必须是 %q 或 %q", ChannelFosun, ChannelOKX), CodeChannelInvalid)
		return
	}
	if strings.TrimSpace(req.AccountRef) == "" {
		writeErr(w, http.StatusBadRequest, "account_ref 不能为空", CodeBadRequest)
		return
	}
	if strings.TrimSpace(req.ClientOrderID) == "" &&
		(strings.TrimSpace(req.From) == "" || strings.TrimSpace(req.To) == "") {
		writeErr(w, http.StatusBadRequest,
			"必须提供 client_order_id，或同时提供 from + to", CodeBadRequest)
		return
	}

	ch, ok := s.channels[req.Channel]
	if !ok {
		writeErr(w, http.StatusServiceUnavailable,
			fmt.Sprintf("通道 %q 未配置", req.Channel), CodeChannelUnavailable)
		return
	}

	resp, err := ch.Query(req)
	if err != nil {
		writeErr(w, http.StatusBadGateway,
			fmt.Sprintf("通道查询失败：%v", err), CodeChannelUnavailable)
		return
	}

	writeJSON(w, http.StatusOK, resp)
}

// validateSubmit 校验下单请求。
func validateSubmit(req *SubmitRequest) (msg, code string) {
	if !validChannels[req.Channel] {
		return fmt.Sprintf("channel 必须是 %q 或 %q", ChannelFosun, ChannelOKX), CodeChannelInvalid
	}
	if strings.TrimSpace(req.AccountRef) == "" {
		return "account_ref 不能为空", CodeBadRequest
	}
	if strings.TrimSpace(req.ClientOrderID) == "" {
		// 幂等键必须有 —— 没有它，重试会重复下单。
		return "client_order_id 不能为空（它是幂等键，缺了会导致重试重复下单）", CodeBadRequest
	}
	if strings.TrimSpace(req.Symbol) == "" {
		return "symbol 不能为空", CodeBadRequest
	}
	side := strings.ToLower(strings.TrimSpace(req.Side))
	if side != "buy" && side != "sell" {
		return `side 必须是 "buy" 或 "sell"`, CodeBadRequest
	}
	if strings.TrimSpace(req.Qty) == "" {
		return "qty 不能为空", CodeBadRequest
	}
	return "", ""
}

// ---- 辅助 ----

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg, errCode string) {
	writeJSON(w, code, map[string]any{"error": msg, "code": errCode})
}

// ListenAndServe 在 127.0.0.1 上起服务，返回实际端口。
//
// # 为什么只监听 127.0.0.1
//
// vaultd 持有凭证。监听 0.0.0.0 等于把它暴露给整个局域网 ——
// 而它设计上只服务本机的 artex 进程。
func (s *Server) ListenAndServe(addr string) (*http.Server, net.Listener, error) {
	// 强制回环地址：即使调用方传了 0.0.0.0 也改回来。
	//
	// # 为什么要强制
	//
	// 「不暴露到网络」这个约束不该依赖调用方传对参数。
	// 传错一次就是一个安全事件，而强制改写的成本是一行。
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, nil, fmt.Errorf("地址 %q 不合法: %w", addr, err)
	}
	if host != "127.0.0.1" && host != "localhost" && host != "" {
		slog.Warn("vaultd 地址被强制改为回环地址（凭证进程不得监听外部接口）",
			"requested", host, "forced", "127.0.0.1")
		host = "127.0.0.1"
	}
	if host == "" {
		host = "127.0.0.1"
	}

	ln, err := net.Listen("tcp", net.JoinHostPort(host, port))
	if err != nil {
		return nil, nil, err
	}

	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	return srv, ln, nil
}

// redactRaw 对通道的 raw_ack 做脱敏。
//
// # 为什么在 vaultd 的出口做，而不是在通道实现里
//
// 通道实现可能有多个（fosun / okx / 未来的）。而「出参不泄漏凭证」
// 是 vaultd 对外的**统一承诺** —— 放在边界上，一个地方保证所有通道。
//
// 这也符合 ARCH §3.2.2 ① 的「工具返回体」要求：
// `raw_ack` 在 vaultd 内已做 redact.Raw()，json.Marshal 前再跑一遍 redact.Mask()。
func redactRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	return redact.MaskJSON(raw)
}
