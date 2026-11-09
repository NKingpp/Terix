package vaultd

// 星财富（fosunwealth）通道。
//
// # 当前状态：**未接入验证**
//
// 本项目里**没有** `openapi-sdk/`，也没有星财富的官方 API 文档。
// 所以这个文件分两部分，边界画得很明确：
//
//   - **可信部分**：PEM 私钥解析 + RSA-SHA256 签名。这是标准加密操作，
//     用 Go 标准库 `crypto/rsa` + `crypto/x509` 实现，能验证（有单测）。
//   - **推测部分**：endpoint 路径、签名 header 名称、请求/响应 JSON 字段名。
//     全部来自「通用 REST+签名」的惯例，**未经官方文档校验**。
//
// 推测部分一律标 `ponytail:` 注释。拿到官方文档后按注释里的清单校正。
//
// # 为什么不做成「假装能用」
//
// 一个看起来能跑、实际签名/字段全错的通道，比一个明确报错的通道危险得多：
// 前者会让人以为「已经接好了」，把真实资金押在一次 400 或者更糟的误下单上。

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// fosunSigner 抽象「如何给一个请求签名」。
//
// # 为什么要这个接口
//
// 签名规范一旦拿到官方文档，只需要换一个实现，Submit/Query 的调用代码不动。
// 也让测试能注入一个确定性 signer，不用真造 RSA 密钥就能测请求组装。
type fosunSigner interface {
	// Sign 返回要附加到请求上的 header 集合。
	//
	// body 为 nil 表示无 body（GET）。
	Sign(method, path string, body []byte) (map[string]string, error)
}

// fosunDefaultBaseURL 是**推测**的 base url。
//
// ponytail: fosun 的 base url 未经官方文档确认（openapi-sdk/ 缺失），
// 这里用星财富官网域名作为占位。拿到文档后改这里 + channel_fosun_test.go。
const fosunDefaultBaseURL = "https://openapi.fosunwealth.com"

// fosunTimeout 同 OKX：下单路径不允许无界阻塞。
const fosunTimeout = 15 * time.Second

// FosunConfig 是星财富通道的配置。
type FosunConfig struct {
	// ServerPublicKey 是券商给的 server_public_key（PEM）。
	//
	// # 为什么保存 public key 但当前不用它验签
	//
	// 星财富应该会用这把公钥对**响应**签名，我方需要验签才能确认响应未被篡改。
	// 但响应签名的字段名/覆盖范围未知（见文件头），所以当前只保存不使用，
	// 留给拿到文档后实现 `verifyResponse`。
	ServerPublicKey string

	// ClientPrivateKey 是我方私钥（PEM）。用于给请求签名。
	ClientPrivateKey string

	// AccountID 是 apply_account_id，星财富侧的账户标识。
	AccountID string

	// BaseURL 为空时用 fosunDefaultBaseURL。
	BaseURL string
}

// FosunChannel 是星财富的 Channel 实现。
//
// 未验证状态：Submit/Query 会真的发 HTTP（按推测的协议），
// 但 Health 直接返回「未验证」错误 —— 让运维一眼看到这不是可用通道。
type FosunChannel struct {
	cfg    FosunConfig
	base   string
	signer fosunSigner
	client *http.Client

	// idem 与 OKX 同理：进程内幂等表。
	//
	// # 注意
	//
	// 星财富侧是否有 clientOrderId 幂等键**未知**（文档缺失）。
	// 所以这里的幂等**只有一层**（内存），跨重启不保证 ——
	// 这是一个必须让调用方知道的风险，写在 Health 的错误里不合适，
	// 写在包注释里：见文件头。
	idem sync.Map // map[string]*fosunIdemEntry
}

type fosunIdemEntry struct {
	mu   sync.Mutex
	resp SubmitResponse
	done bool
}

// NewFosunChannel 构造星财富通道。
//
// # 缺凭证返回 error，不 panic
//
// 启动期的问题应该在启动期以「一条错误日志 + 不加载该通道」的形式解决，
// 而不是让进程崩掉 —— 崩掉会连带把 OKX 通道一起搞下线。
func NewFosunChannel(cfg FosunConfig) (*FosunChannel, error) {
	if strings.TrimSpace(cfg.ClientPrivateKey) == "" {
		return nil, errors.New("fosun: client_private_key 为空")
	}
	if strings.TrimSpace(cfg.AccountID) == "" {
		return nil, errors.New("fosun: account_id 为空")
	}

	// 提前解析私钥：配错了在启动期就报，不要等到第一次下单。
	signer, err := newFosunPEMSigner(cfg.ClientPrivateKey)
	if err != nil {
		return nil, err
	}

	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		base = fosunDefaultBaseURL
	}

	return &FosunChannel{
		cfg:    cfg,
		base:   base,
		signer: signer,
		client: &http.Client{Timeout: fosunTimeout},
	}, nil
}

// Submit 提交一笔委托。
//
// ponytail: fosun 的下单路径 /openapi/order 与请求体字段名来自推测（openapi-sdk/ 缺失），
// 未经官方文档校验。拿到文档后需要校正路径、字段名与响应解析。
func (c *FosunChannel) Submit(req SubmitRequest) (SubmitResponse, error) {
	key := strings.TrimSpace(req.ClientOrderID)
	if key == "" {
		return SubmitResponse{}, errors.New("fosun: client_order_id 不能为空（它是幂等键）")
	}

	e := c.entry(key)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.done {
		return e.resp, nil
	}

	body, err := json.Marshal(map[string]string{
		"accountId":     c.cfg.AccountID,
		"clientOrderId": key,
		"symbol":        strings.TrimSpace(req.Symbol),
		"side":          strings.ToLower(strings.TrimSpace(req.Side)),
		"qty":           strings.TrimSpace(req.Qty),
		"orderType":     strings.ToLower(strings.TrimSpace(req.OrderType)),
		"price":         strings.TrimSpace(req.Price),
		"tif":           strings.ToLower(strings.TrimSpace(req.TIF)),
	})
	if err != nil {
		return SubmitResponse{}, fmt.Errorf("fosun: 序列化下单请求失败: %w", err)
	}

	const path = "/openapi/order"
	raw, err := c.do(http.MethodPost, path, body)
	if err != nil {
		return SubmitResponse{}, err
	}

	// ponytail: 响应字段名（orderId）来自推测。
	var ack struct {
		OrderID string `json:"orderId"`
		Msg     string `json:"message"`
	}
	if err := json.Unmarshal(raw, &ack); err != nil {
		return SubmitResponse{}, fmt.Errorf("fosun: 下单响应不是合法 JSON: %w", err)
	}
	if ack.OrderID == "" {
		return SubmitResponse{}, fmt.Errorf("fosun: 下单响应缺少 orderId（msg=%s）", ack.Msg)
	}

	resp := SubmitResponse{
		ChannelOrderID: ack.OrderID,
		AcceptedAt:     time.Now().UTC().Format(time.RFC3339),
		RawAck:         json.RawMessage(raw),
	}
	e.resp = resp
	e.done = true
	return resp, nil
}

// Query 查回执。
//
// ponytail: 查单路径 /openapi/order 与字段名来自推测，未经官方文档校验。
func (c *FosunChannel) Query(req QueryRequest) (QueryResponse, error) {
	const path = "/openapi/order"
	var body []byte
	if strings.TrimSpace(req.ClientOrderID) != "" {
		body = []byte(fmt.Sprintf(`{"accountId":%q,"clientOrderId":%q}`,
			c.cfg.AccountID, strings.TrimSpace(req.ClientOrderID)))
	} else {
		body = []byte(fmt.Sprintf(`{"accountId":%q,"from":%q,"to":%q}`,
			c.cfg.AccountID, strings.TrimSpace(req.From), strings.TrimSpace(req.To)))
	}

	raw, err := c.do(http.MethodPost, path, body)
	if err != nil {
		return QueryResponse{}, err
	}

	// ponytail: 响应结构来自推测。
	var ack struct {
		Orders []struct {
			ClientOrderID string `json:"clientOrderId"`
			OrderID       string `json:"orderId"`
			State         string `json:"state"`
			FilledQty     string `json:"filledQty"`
			AvgPrice      string `json:"avgPrice"`
			UpdatedAt     string `json:"updatedAt"`
		} `json:"orders"`
	}
	if err := json.Unmarshal(raw, &ack); err != nil {
		return QueryResponse{}, fmt.Errorf("fosun: 查单响应不是合法 JSON: %w", err)
	}

	orders := make([]ChannelOrder, 0, len(ack.Orders))
	for _, o := range ack.Orders {
		orders = append(orders, ChannelOrder{
			ClientOrderID:  o.ClientOrderID,
			ChannelOrderID: o.OrderID,
			State:          strings.ToLower(o.State),
			FilledQty:      o.FilledQty,
			AvgPrice:       o.AvgPrice,
			UpdatedAt:      o.UpdatedAt,
		})
	}
	return QueryResponse{Orders: orders}, nil
}

// Health 明确返回「未接入验证」。
//
// # 为什么不发一个探测请求
//
// 因为没有可信的探测目标：endpoint 本身是推测的，探测失败说明不了任何事，
// 探测成功反而更糟（可能是撞上了某个无关的 200）。
// 与其给一个假的绿灯，不如给一个明确的红灯。
func (c *FosunChannel) Health() error {
	return errors.New(
		"fosun: 通道未接入验证 —— 缺少星财富官方 API 文档与 openapi-sdk，" +
			"签名 header 名、endpoint 路径、请求/响应字段均为推测，不可用于真实下单")
}

// ---- 内部 ----

func (c *FosunChannel) entry(key string) *fosunIdemEntry {
	if v, ok := c.idem.Load(key); ok {
		return v.(*fosunIdemEntry)
	}
	v, _ := c.idem.LoadOrStore(key, &fosunIdemEntry{})
	return v.(*fosunIdemEntry)
}

// do 签名并发请求。错误信息不含凭证。
func (c *FosunChannel) do(method, path string, body []byte) ([]byte, error) {
	headers, err := c.signer.Sign(method, path, body)
	if err != nil {
		return nil, fmt.Errorf("fosun: 签名失败: %w", err)
	}

	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return nil, fmt.Errorf("fosun: 构造请求失败: %w", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fosun: 请求 %s 失败: %w", path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("fosun: 读取响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fosun: 请求 %s 返回 HTTP %d", path, resp.StatusCode)
	}
	return raw, nil
}

// ---- PEM 签名实现（这部分是可信的）----

// fosunPEMSigner 用 PEM 私钥做 RSA-SHA256 签名。
//
// # 签什么
//
// 签名原文：method + "\n" + path + "\n" + body
//
// ponytail: 星财富的实际签名原文拼接规则**未知**（文档缺失），
// 这里用最常见的 `method\npath\nbody` 形式。拿到文档后只需改
// fosunPEMSigner.Sign 里的 prehash 拼接和 header 名，调用方不用动。
type fosunPEMSigner struct {
	key *rsa.PrivateKey
}

// newFosunPEMSigner 解析 PEM 私钥。
//
// 支持 PKCS#1（"RSA PRIVATE KEY"）与 PKCS#8（"PRIVATE KEY"）两种，
// 因为券商给的 PEM 是哪种无法预设。
func newFosunPEMSigner(pemText string) (*fosunPEMSigner, error) {
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return nil, errors.New("fosun: client_private_key 不是合法 PEM（缺少 BEGIN/END 行）")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		k, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("fosun: 解析 PKCS#1 私钥失败: %w", err)
		}
		return &fosunPEMSigner{key: k}, nil
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("fosun: 解析 PKCS#8 私钥失败: %w", err)
		}
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("fosun: PKCS#8 私钥不是 RSA（本实现只支持 RSA）")
		}
		return &fosunPEMSigner{key: rk}, nil
	default:
		return nil, fmt.Errorf("fosun: 不支持的 PEM 类型 %q（期望 RSA PRIVATE KEY 或 PRIVATE KEY）", block.Type)
	}
}

// Sign 生成签名 header。
//
// ponytail: header 名（X-FOSUN-*）与参数名来自推测，未经官方文档校验。
// 拿到文档后改这里即可，Submit/Query 不用动。
func (s *fosunPEMSigner) Sign(method, path string, body []byte) (map[string]string, error) {
	ts := time.Now().UTC().Format(time.RFC3339)
	prehash := method + "\n" + path + "\n" + string(body)

	sum := sha256.Sum256([]byte(prehash))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, sum[:])
	if err != nil {
		return nil, fmt.Errorf("fosun: RSA 签名失败: %w", err)
	}

	// 注意：官方协议很可能还要求一个「应用标识 / appKey」header。
	// 但我们手上没有这个值（凭证里只有 server_public_key 与 client_private_key），
	// 所以**不伪造**一个空 header —— 发空值反而会让对端的错误信息更难懂。
	return map[string]string{
		"X-FOSUN-TIMESTAMP":   ts,
		"X-FOSUN-SIGN":        base64.StdEncoding.EncodeToString(sig),
		"X-FOSUN-SIGN-METHOD": "RSA-SHA256",
	}, nil
}
