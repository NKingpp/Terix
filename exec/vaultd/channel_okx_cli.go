package vaultd

// OKX 通道的 CLI 实现：通过官方 `okx` 命令行工具下单，而不是直连 REST API。
//
// # 为什么要有这条实现（2026-11-06 实测）
//
// 直连 `www.okx.com` 在本机网络下不通，官方 CLI 靠内置的 okx-pilot
// 解析出可用镜像节点（www.ynhf1jp.com / okexweb.qqhrss.com）再连接。
// 而 Go 直连这些镜像会被拒：
//
//	直连 www.okx.com        → dial: no such host（DNS 解析不到）
//	直连镜像（钉 IP 后）     → HTTP 403
//	  已排除：UA（试了 3 种）、tdMode/tgtCcy 组合（4 种）、节点轮换（2 个）
//	  剩余差异：Node 与 Go 的 TLS 指纹不同，镜像按指纹拦截
//
// 而同一条命令用 CLI 执行就成功：
//
//	okx --demo spot place --instId BTC-USDT --side buy --ordType market \
//	    --sz 10 --tgtCcy quote_ccy --tdMode cash --clOrdId ex123
//	→ {"ordId":"4074551391804706816","sCode":"0","sMsg":"Order placed"}
//
// 所以让 vaultd 走 CLI —— 复用它已验证可用的网络路径。
//
// # 安全边界不变
//
// 凭证仍只从环境变量来（vaultd 不连数据库），只是从「HTTP header」
// 变成「子进程环境变量」。凭证不落盘、不进日志。
//
// # 与 API 实现的取舍
//
// CLI 版慢一些（每次 fork 一个 Node 进程，约 300ms 启动开销），
// 但可用。API 版留着 —— 网络环境正常时它更快、无外部依赖。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// okxCLITimeout 是单次 CLI 调用的上限。
//
// 比 API 版（15s）宽松：CLI 要先启动 Node 进程，冷启动约 300-500ms；
// 网络抖动时 pilot 还会尝试多个节点。
const okxCLITimeout = 25 * time.Second

// OKXCLIChannel 通过 `okx` 命令行工具执行下单与查单。
type OKXCLIChannel struct {
	apiKey     string
	secretKey  string
	passphrase string
	demo       bool

	// binPath 是 okx 可执行文件路径。空表示按 PATH 查找。
	binPath string

	// idem 是进程内幂等表（与 API 版同语义：同一 clientOrderID 只发一次）。
	idem sync.Map // map[string]*okxCLIEntry
}

type okxCLIEntry struct {
	mu   sync.Mutex
	done bool
	resp SubmitResponse
}

// OKXCLIConfig 是 CLI 通道的配置。
type OKXCLIConfig struct {
	APIKey     string
	SecretKey  string
	Passphrase string
	Demo       bool
	// BinPath 为空时按 PATH 查找 `okx`。
	BinPath string
}

// NewOKXCLIChannel 构造 CLI 通道。凭证缺失直接报错（与 API 版一致：
// vaultd 启动时就该知道通道能不能用）。
func NewOKXCLIChannel(cfg OKXCLIConfig) (*OKXCLIChannel, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, fmt.Errorf("okx-cli: api_key 为空")
	}
	if strings.TrimSpace(cfg.SecretKey) == "" {
		return nil, fmt.Errorf("okx-cli: api_secret 为空")
	}
	if strings.TrimSpace(cfg.Passphrase) == "" {
		return nil, fmt.Errorf("okx-cli: passphrase 为空")
	}
	return &OKXCLIChannel{
		apiKey:     cfg.APIKey,
		secretKey:  cfg.SecretKey,
		passphrase: cfg.Passphrase,
		demo:       cfg.Demo,
		binPath:    strings.TrimSpace(cfg.BinPath),
	}, nil
}

// Health 检查 CLI 可用：能执行 `okx --version` 即可。
//
// 不做「查账户余额」那种带鉴权的健康检查 —— 那会消耗 API 限频，
// 而 vaultd 的健康检查是给编排层轮询用的，频率不可控。
func (c *OKXCLIChannel) Health() error {
	if _, err := c.run(context.Background(), "--version"); err != nil {
		return fmt.Errorf("okx-cli 不可用: %w", err)
	}
	return nil
}

func (c *OKXCLIChannel) entry(key string) *okxCLIEntry {
	v, _ := c.idem.LoadOrStore(key, &okxCLIEntry{})
	return v.(*okxCLIEntry)
}

// Submit 提交一笔委托。
func (c *OKXCLIChannel) Submit(req SubmitRequest) (SubmitResponse, error) {
	key := strings.TrimSpace(req.ClientOrderID)
	if key == "" {
		return SubmitResponse{}, fmt.Errorf("okx-cli: client_order_id 不能为空（它是幂等键）")
	}
	if err := validateOKXClientOrderID(key); err != nil {
		return SubmitResponse{}, err
	}

	e := c.entry(key)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.done {
		return e.resp, nil // 已成功提交过：回放，不再发包
	}

	instID, err := okxInstID(req.Symbol)
	if err != nil {
		return SubmitResponse{}, err
	}
	side := strings.ToLower(strings.TrimSpace(req.Side))
	if side != "buy" && side != "sell" {
		return SubmitResponse{}, fmt.Errorf("okx-cli: side 必须是 buy 或 sell，实际 %q", req.Side)
	}
	ordType := strings.ToLower(strings.TrimSpace(req.OrderType))
	if ordType == "" {
		ordType = "market"
	}
	if ordType != "market" && ordType != "limit" {
		return SubmitResponse{}, fmt.Errorf("okx-cli: ordType 只支持 market/limit，实际 %q", req.OrderType)
	}
	sz := strings.TrimSpace(req.Qty)
	if sz == "" {
		return SubmitResponse{}, fmt.Errorf("okx-cli: qty 不能为空")
	}

	args := []string{"spot", "place",
		"--instId", instID,
		"--side", side,
		"--ordType", ordType,
		"--sz", sz,
		"--tdMode", "cash",
		"--clOrdId", key,
		"--json",
	}
	if tc := strings.TrimSpace(req.TgtCcy); tc != "" {
		// CLI 的参数名是 --tgtCcy（与 REST 的 tgtCcy 同义）。
		args = append(args, "--tgtCcy", tc)
	}
	if ordType == "limit" {
		px := strings.TrimSpace(req.Price)
		if px == "" {
			return SubmitResponse{}, fmt.Errorf("okx-cli: 限价单必须提供 price")
		}
		args = append(args, "--px", px)
	}

	ctx, cancel := context.WithTimeout(context.Background(), okxCLITimeout)
	defer cancel()
	raw, err := c.run(ctx, args...)
	if err != nil {
		return SubmitResponse{}, err
	}

	// CLI 输出是数组：[{clOrdId, ordId, sCode, sMsg, ts}]
	var out []struct {
		ClOrdID string `json:"clOrdId"`
		OrdID   string `json:"ordId"`
		SCode   string `json:"sCode"`
		SMsg    string `json:"sMsg"`
		TS      string `json:"ts"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return SubmitResponse{}, fmt.Errorf("okx-cli: 下单响应不是合法 JSON: %w（原文: %s）",
			err, truncateForErr(raw))
	}
	if len(out) == 0 {
		return SubmitResponse{}, fmt.Errorf("okx-cli: 下单响应为空数组")
	}
	o := out[0]
	if o.SCode != "0" {
		return SubmitResponse{}, fmt.Errorf("okx-cli: 下单被拒 sCode=%s sMsg=%s", o.SCode, o.SMsg)
	}
	if strings.TrimSpace(o.OrdID) == "" {
		return SubmitResponse{}, fmt.Errorf("okx-cli: 下单返回 ordId 为空（sCode=0 但无订单号）")
	}

	resp := SubmitResponse{
		ChannelOrderID: o.OrdID,
		AcceptedAt:     time.Now().UTC().Format(time.RFC3339),
		RawAck:         json.RawMessage(raw),
	}
	e.resp, e.done = resp, true
	return resp, nil
}

// Query 查回执。按 clientOrderID 或（from/to）区间查。
func (c *OKXCLIChannel) Query(req QueryRequest) (QueryResponse, error) {
	args := []string{"spot", "orders", "--json"}
	if req.ClientOrderID != "" {
		// orders 子命令不支持按 clOrdId 过滤，用 get 查单笔。
		args = []string{"spot", "get", "--clOrdId", req.ClientOrderID, "--json"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), okxCLITimeout)
	defer cancel()
	raw, err := c.run(ctx, args...)
	if err != nil {
		return QueryResponse{}, err
	}
	// 字段名随子命令不同（get 返回对象、orders 返回数组），用宽松解析。
	var asArr []map[string]any
	if err := json.Unmarshal(raw, &asArr); err != nil {
		var one map[string]any
		if err2 := json.Unmarshal(raw, &one); err2 != nil {
			return QueryResponse{}, fmt.Errorf("okx-cli: 查单响应不是合法 JSON: %w", err)
		}
		asArr = []map[string]any{one}
	}
	out := QueryResponse{Orders: make([]ChannelOrder, 0, len(asArr))}
	for _, m := range asArr {
		out.Orders = append(out.Orders, ChannelOrder{
			ClientOrderID:  str(m["clOrdId"]),
			ChannelOrderID: str(m["ordId"]),
			State:          str(m["state"]),
			FilledQty:      str(m["accFillSz"]),
			AvgPrice:       str(m["avgPx"]),
			UpdatedAt:      str(m["uTime"]),
		})
	}
	return out, nil
}

// run 执行一次 okx 命令并返回 stdout（已去除非 JSON 前缀行）。
//
// # 为什么凭证走子进程环境变量而不是命令行参数
//
// 命令行参数在 Windows 上会出现在进程列表（任务管理器可见），
// 凭证会泄漏。环境变量只对子进程可见。这与 agent 包 okx_client 的做法一致。
func (c *OKXCLIChannel) run(ctx context.Context, args ...string) ([]byte, error) {
	bin := c.binPath
	if bin == "" {
		p, err := exec.LookPath("okx")
		if err != nil {
			return nil, fmt.Errorf("okx-cli: 找不到 okx 命令（未安装或不在 PATH）")
		}
		bin = p
	}
	full := append([]string{}, args...)
	if c.demo {
		// --demo 必须在子命令之前：它是全局选项。
		if len(full) > 0 && !strings.HasPrefix(full[0], "-") {
			full = append([]string{"--demo"}, full...)
		}
	}

	cmd := exec.CommandContext(ctx, bin, full...)
	// 保留父进程环境（PATH 等），再追加凭证。
	cmd.Env = append(os.Environ(),
		"OKX_API_KEY="+c.apiKey,
		"OKX_SECRET_KEY="+c.secretKey,
		"OKX_PASSPHRASE="+c.passphrase,
	)
	if c.demo {
		cmd.Env = append(cmd.Env, "OKX_DEMO=1")
	} else {
		// 显式清空：父进程若恰好导出过 OKX_DEMO=1，不清会让实盘 key 打到模拟盘。
		cmd.Env = append(cmd.Env, "OKX_DEMO=")
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("okx-cli 执行失败: %s", msg)
	}
	return trimToJSONOKX(out), nil
}

// validateOKXClientOrderID 校验 OKX 对 clOrdId 的格式要求。
//
// # 规则（实测 2026-11-06）
//
// 只允许**字母与数字**，长度 1-32。带连字符会被拒：
//
//	artex-probe-1793933483 → sCode=51000 "Parameter clOrdId error"
//	artex1793933492        → sCode=0 "Order placed"
//
// 这条约束很关键：本项目原来的幂等键是 `artex-<hex16>`（带连字符），
// 走 API 时 OKX 接受，但 CLI 路径下统一被拒。所以在通道入口就挡掉，
// 报错信息说明格式，避免调用方以为是网络问题。
func validateOKXClientOrderID(id string) error {
	if len(id) > 32 {
		return fmt.Errorf("okx-cli: client_order_id 超过 32 字符（OKX 上限），实际 %d: %q", len(id), id)
	}
	for i, r := range id {
		isDigit := r >= '0' && r <= '9'
		isUpper := r >= 'A' && r <= 'Z'
		isLower := r >= 'a' && r <= 'z'
		if !isDigit && !isUpper && !isLower {
			return fmt.Errorf(
				"okx-cli: client_order_id 含非法字符 %q（位置 %d）。OKX 只允许字母数字，"+
					"且长度 ≤32。当前值: %q", string(r), i, id)
		}
	}
	return nil
}

// trimToJSONOKX 去掉 CLI 可能的前置非 JSON 行（升级提示等）。
func trimToJSONOKX(raw []byte) []byte {
	s := bytes.TrimSpace(raw)
	for i := 0; i < len(s); i++ {
		if s[i] == '[' || s[i] == '{' {
			return s[i:]
		}
	}
	return s
}

// str 从 map 里安全取字符串。
func str(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		// 大数字（如 ts）在 JSON 里是 float64，直接格式化会变科学计数法。
		return fmt.Sprintf("%.0f", t)
	default:
		return ""
	}
}

// truncateForErr 截断错误信息里的原始响应，避免日志被长文本淹没。
func truncateForErr(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > 300 {
		return s[:300] + "...(截断)"
	}
	return s
}
