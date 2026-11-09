package agent

// OKX CLI 客户端。
//
// # 为什么是 CLI 而不是 HTTP
//
// OKX 有公开 REST API，但本机已装官方 CLI（`okx` 1.4.8，免鉴权查公开行情）。
// 用 CLI 的理由：
//  1. 装好了、实连验证过（`okx market ticker BTC-USDT` 返回真数据）
//  2. 官方维护，API 变更由 CLI 吸收
//  3. 公开行情不需要 API Key
//
// 不把 CLI 抽象成通用 transport 的理由（为什么不用 mcp_servers 的 cli transport）：
// 现在只有这一个工具族用 CLI。为一个通道加通用 transport 层是过度设计
// （与「1 个工具不值得加 abstraction」同一取舍）。等 crypto 相关工具有 3 个
// 以上再抽象。
//
// # 三条硬约束（踩过的坑）
//
//  1. **必须 `--json`**：默认输出是给人看的对齐表格，列宽/分隔符会变，解析必然碎。
//  2. **必须显式接 stderr**：CLI 启动时往 stderr 打印「Update available for
//     @okx_ai/okx-trade-cli」，不接会跑到终端或丢失真正的错误信息。
//  3. **错误必须带 stderr**：CLI 失败时原因在 stderr，不带就变成「静默失败」。
//
// # 二进制路径
//
// 用 `exec.LookPath("okx")`，解析失败说明「OKX 通道不可用」，返回明确的
// ChannelUnavailable 错误而不是 panic。**不硬编码路径** —— 不同机器上
// node 版本目录不同（本机是 .workbuddy/binaries/node/versions/22.22.2-6/okx）。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// OkxClient 通过 exec 调用 okx CLI。
type OkxClient struct {
	binPath string
	timeout time.Duration
	// lookErr 记录 LookPath 的失败原因。构造时不返回 error（保持 New 系列
	// 简单），而是在实际调用时返回明确错误。
	lookErr error
	once    sync.Once
	credMu  sync.RWMutex

	// cred 是注入给 CLI 的凭证。零值表示「不注入」——
	// 此时 CLI 会去读 ~/.okx/config.toml。
	//
	// # 为什么必须有这条路（2026-11-02 线上事故）
	//
	// 用户填了正确的模拟盘 key 并「验证通过」，但**模型交易时报
	// `API key doesn't exist`（50119）**。
	//
	// 根因：CREDENTIAL PAGE 和 CLI 是两套独立配置。
	//   - 凭证页 → market_credentials 表（server 的 REST 验证用它）
	//   - CLI    → ~/.okx/config.toml（模型交易实际用它）
	//
	// 两者可以是完全不同的 key。实测用户机器上：
	//   config.toml: fd8dd762-...  demo=false  → 50119 key 不存在
	//   凭证表     : 450d***6d7d    demo=true   → HTTP 200，余额 1 BTC
	//
	// 也就是说「验证通过」给出了虚假的安全感 —— 它验的是 A，交易用的是 B。
	//
	// 修法不是「让用户再手动填一遍 config.toml」（那只是把两处配置
	// 变成三处），而是让调用方把表里的 key 作为环境变量注入 CLI。
	// CLI 支持 OKX_API_KEY / OKX_SECRET_KEY / OKX_PASSPHRASE / OKX_DEMO，
	// 环境变量优先级高于 config.toml —— 于是凭证页成为唯一入口。
	cred *OkxCredential
}

// OkxCredential 是注入给 CLI 的凭证（对齐 CLI 的环境变量名）。
type OkxCredential struct {
	APIKey     string
	SecretKey  string
	Passphrase string
	// Demo=true 时设 OKX_DEMO=1，让 CLI 打到模拟盘。
	//
	// 必须与 key 的环境一致：实盘/模拟盘的 key 互不通用，
	// 错配的报错是 50101「APIKey does not match current environment」，
	// 看起来像「key 无效」而实际是环境错了。
	Demo bool
}

// SetCredential 注入凭证。传 nil 表示清除（回退到 config.toml）。
//
// 可在运行期调用（如用户在凭证页改了 key）—— 内部用 mutex 保护。
func (c *OkxClient) SetCredential(cred *OkxCredential) {
	c.credMu.Lock()
	defer c.credMu.Unlock()
	c.cred = cred
}

// CredentialForTest 暴露当前凭证供测试断言（生产代码不要用它）。
//
// # 为什么单独开一个而不是把 cred 导出
//
// 导出字段会被误写（`c.cred = xxx` 绕过 mutex）。测试需要的是**读**，
// 所以给一个只读访问器，名字带 ForTest 让 greppable。
func (c *OkxClient) CredentialForTest() *OkxCredential { return c.getCredential() }

// getCredential 取当前凭证（可能为 nil）。
func (c *OkxClient) getCredential() *OkxCredential {
	c.credMu.RLock()
	defer c.credMu.RUnlock()
	if c.cred == nil {
		return nil
	}
	cp := *c.cred
	return &cp
}

// NewOkxClient 解析 okx 二进制。binPath 为空时用 exec.LookPath("okx")。
func NewOkxClient(binPath string) *OkxClient {
	c := &OkxClient{timeout: 15 * time.Second}
	if binPath != "" {
		c.binPath = binPath
		return c
	}
	return c
}

// resolve 惰性解析二进制路径（只解析一次）。
func (c *OkxClient) resolve() error {
	c.once.Do(func() {
		if c.binPath != "" {
			return
		}
		// 注意：这里拿到的 p 在 MSYS2/Git Bash 下是 `/c/Users/.../okx`
		// （MSYS 虚拟路径），**不能直接传给 exec.CommandContext 当绝对路径** ——
		// Windows CreateProcess 不认识 `/c/...` 形式。
		//
		// 务实做法：run() 里用的是 c.binPath，而实测传这个值仍能成功，
		// 因为 exec 会对裸名/相对名重新解析。但若将来需要「日志里记录完整
		// 命令」或「传给子进程环境变量」，这个路径就是不可用的。
		//
		// 详见 KNOWN-LIMITATIONS.md L6。
		p, err := exec.LookPath("okx")
		if err != nil {
			// PATH 里找不到时，按已知安装位置兜底探测（见 findOkxFallback）。
			//
			// 2026-11-05 事故：用户系统 PATH 里**有** okx 目录，但后端进程
			// 没继承到 → OKX_CLI_NOT_FOUND → planner 判定「CLI 未安装」→
			// 派意图去装 CLI → 那意图没 role → 拿到 Bash → 到处翻环境变量。
			// 起点只是 PATH 没传进来。
			if alt := findOkxFallback(); alt != "" {
				c.binPath = alt
				return
			}
			c.lookErr = &MarketError{
				Code:      "OKX_CLI_NOT_FOUND",
				Message:   "找不到 okx 命令（PATH 缺失且常见安装位置也没有）",
				Channel:   ChannelOKX,
				Retryable: false,
			}
			return
		}
		c.binPath = p
	})
	return c.lookErr
}

func (c *OkxClient) Timeout() time.Duration { return c.timeout }

// run 执行一条 okx 命令并返回 stdout。
//
// 参数顺序约定：模块、动作、位置参数，然后**选项**，`--json` 放最后。
// （实测 CLI 对手写顺序不敏感，但固定在末尾便于阅读与 grep。）
func (c *OkxClient) run(ctx context.Context, args ...string) ([]byte, error) {
	if err := c.resolve(); err != nil {
		return nil, err
	}
	full := append([]string{}, args...)
	full = append(full, "--json")

	cmd := exec.CommandContext(ctx, c.binPath, full...)
	// 注入凭证：环境变量优先级高于 ~/.okx/config.toml，
	// 所以凭证页填的 key 会覆盖 CLI 自己那份（见 OkxClient.cred 的注释）。
	//
	// # 为什么用 append 而不是 cmd.Env = map
	//
	// exec.Cmd.Env 是「完整环境」，设了它就必须自己带上 PATH 等所有变量 ——
	// 漏掉 PATH 会让 CLI 找不到 okx-auth 等配套二进制。
	// os.Environ() 复制当前环境再追加，是唯一不会误伤的做法。
	if cred := c.getCredential(); cred != nil && cred.APIKey != "" {
		cmd.Env = append(os.Environ(),
			"OKX_API_KEY="+cred.APIKey,
			"OKX_SECRET_KEY="+cred.SecretKey,
			"OKX_PASSPHRASE="+cred.Passphrase,
		)
		if cred.Demo {
			cmd.Env = append(cmd.Env, "OKX_DEMO=1")
		} else {
			// 显式清空：父进程若恰好有 OKX_DEMO=1（比如我们自己 export 过），
			// 不覆盖会让实盘 key 打到模拟盘。空串让 CLI 视为未设置。
			cmd.Env = append(cmd.Env, "OKX_DEMO=")
		}
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr // 必须显式接：升级提示与错误都走这里

	out, err := cmd.Output() // 只读 stdout → 纯净 JSON
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		// 把 CLI 的 stderr 原样带出来。丢掉它等于把「为什么失败」扔掉。
		return nil, &MarketError{
			Code:      "OKX_CLI_FAILED",
			Message:   fmt.Sprintf("%v（stderr: %s）", err, msg),
			Channel:   ChannelOKX,
			Retryable: true,
		}
	}
	return out, nil
}

// Health 检查 okx CLI 是否可用。
//
// 没有专门的 health 子命令，用 `--version` 探活（快，不联网）。
func (c *OkxClient) Health(ctx context.Context) (bool, string, error) {
	if err := c.resolve(); err != nil {
		return false, "", err
	}
	ver, err := c.run(ctx, "--version")
	if err != nil {
		return false, "", err
	}
	return true, "okx " + strings.TrimSpace(string(ver)), nil
}

// WaitWarmup CLI 无预热需求（每次调用是独立进程），直接返回。
func (c *OkxClient) WaitWarmup(ctx context.Context, poll time.Duration) error { return nil }

// instIDForMarket 把用户给的 symbol 规范成 OKX 的 instId。
//
// 约定：
//   - SPOT 计价/基础对：`BTC-USDT`（get_quote / get_kline 用这个）
//   - SWAP 永续：`BTC-USDT-SWAP`（funding-rate / open-interest 用）
//
// 用户可能只传 `BTC`，这里补成 `BTC-USDT`（USDT 是加密最通用的计价币）。
func instIDForMarket(symbol, suffix string) string {
	s := strings.ToUpper(strings.TrimSpace(symbol))
	if s == "" {
		return ""
	}
	if strings.Contains(s, "-") {
		return s // 已是完整 instId
	}
	return s + "-USDT" + suffix
}

// FetchQuote 用 `okx market ticker <instId>` 取报价。
//
// ticker 返回**对象数组**（不是位置参数），取第一个元素。
func (c *OkxClient) FetchQuote(ctx context.Context, symbol, market string, ch Channel) (*Quote, error) {
	if ch != ChannelOKX {
		return nil, &MarketError{
			Code:      "CHANNEL_NOT_APPLICABLE",
			Message:   fmt.Sprintf("OkxClient 只处理 okx 通道，收到 %s", ch),
			Channel:   ch,
			Retryable: false,
		}
	}
	instID := instIDForMarket(symbol, "")
	if instID == "" {
		return nil, &MarketError{Code: "BAD_REQUEST", Message: "缺少 symbol", Channel: ch}
	}

	raw, err := c.run(ctx, "market", "ticker", instID)
	if err != nil {
		return nil, err
	}

	// ticker 是对象数组：[{instId,last,open24h,high24h,low24h,vol24h,ts,...}]
	var rows []struct {
		InstID  string `json:"instId"`
		Last    string `json:"last"`
		Open24h string `json:"open24h"`
		High24h string `json:"high24h"`
		Low24h  string `json:"low24h"`
		Vol24h  string `json:"vol24h"`
		VolCcy  string `json:"volCcy24h"`
		TS      string `json:"ts"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, &MarketError{
			Code:      "OKX_PARSE_FAILED",
			Message:   fmt.Sprintf("解析 ticker 失败: %v (raw=%s)", err, snippet(raw)),
			Channel:   ch,
			Retryable: true,
		}
	}
	if len(rows) == 0 {
		return nil, &MarketError{
			Code:      "OKX_NO_DATA",
			Message:   fmt.Sprintf("ticker 返回空数组（instId=%s 可能不存在）", instID),
			Channel:   ch,
			Retryable: true,
		}
	}
	r := rows[0]

	// OKX 的 `open24h` 是 24 小时前的开价，不是「昨收」。
	// 我们在 Quote.PrevClose 里放 open24h，语义是「24h 前的参考价」——
	// 计算 change_pct 用它，与 A 股的「昨收」不同但都是「比较基准」。
	changePct := ""
	if r.Open24h != "" && r.Last != "" {
		if o, err1 := strconv.ParseFloat(r.Open24h, 64); err1 == nil && o != 0 {
			if l, err2 := strconv.ParseFloat(r.Last, 64); err2 == nil {
				changePct = fmt.Sprintf("%.4f", (l-o)/o*100)
			}
		}
	}

	return &Quote{
		Symbol: strings.ToUpper(strings.TrimSpace(symbol)),
		Market: "crypto",
		// OKX ticker 是**实时**报价，不是日线收盘价 —— 与 akshare 通道不同。
		PriceKind: "realtime",
		// TradingDate 用 ts 的日期部分（毫秒时间戳转 UTC 日期）。
		TradingDate: tsToDate(r.TS),
		Last:        r.Last,
		High:        r.High24h,
		Low:         r.Low24h,
		Open:        r.Open24h,
		PrevClose:   r.Open24h,
		ChangePct:   changePct,
		Volume:      r.Vol24h,
		Amount:      r.VolCcy,
		Channel:     ChannelOKX,
	}, nil
}

// FetchKline 用 `okx market candles <instId> --bar <bar> --limit <n>` 取 K 线。
//
// # 返回格式（与 akshare 完全不同）
//
// OKX candles 返回**位置参数数组**（没有字段名），且是**倒序**（新→旧）：
//
//	[["1792944000000","80637.1","80955.6","79377.7","80485.9","2674.21","214246125.11","0","0","0","0"], ...]
//
// 下标含义（OKX 官方 REST 文档 /v5/market/candles）：
//
//	[0] ts          时间戳(ms)
//	[1] o           开盘价
//	[2] h           最高价
//	[3] l           最低价
//	[4] c           收盘价
//	[5] vol         成交量（张/币）
//	[6] volCcy      成交量（计价币）
//	[7] volCcyQuote 成交量（USDT）
//	[8] confirm     1=已收盘, 0=未收盘（当前正在形成的 bar）
//
// **必须反转成升序**才能与 akshare / bars 表的约定一致（旧→新）。
// **confirm=0 → is_partial=true**，对应 akshare 侧的 is_partial 语义：
// 这根 bar 还没定稿，其 OHLC 会继续变化，计算指标时应跳过或标记。
func (c *OkxClient) FetchKline(ctx context.Context, symbol, market, period string, count int, ch Channel) ([]Bar, error) {
	if ch != ChannelOKX {
		return nil, &MarketError{
			Code:      "CHANNEL_NOT_APPLICABLE",
			Message:   fmt.Sprintf("OkxClient 只处理 okx 通道，收到 %s", ch),
			Channel:   ch,
			Retryable: false,
		}
	}
	instID := instIDForMarket(symbol, "")
	if instID == "" {
		return nil, &MarketError{Code: "BAD_REQUEST", Message: "缺少 symbol", Channel: ch}
	}
	bar, err := okxBar(period)
	if err != nil {
		return nil, &MarketError{Code: "BAD_REQUEST", Message: err.Error(), Channel: ch, Retryable: false}
	}
	if count <= 0 {
		count = 250
	}
	if count > 300 { // OKX limit 上限按官方文档是 300（部分接口 100）
		count = 300
	}

	args := []string{"market", "candles", instID, "--bar", bar, "--limit", strconv.Itoa(count)}
	raw, err := c.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	return parseOkxCandles(raw, ch)
}

// parseOkxCandles 解析 OKX candles 的「位置参数数组」。
//
// 独立成函数是为了可单测：真 CLI 的输出价格随时变，但解析逻辑必须可重复
// 验证（见 TestOkxCandlesParsing）。
func parseOkxCandles(raw []byte, ch Channel) ([]Bar, error) {
	// 每行是 []string，但要容忍数字被解析成 float（理论上 CLI 都给字符串，
	// 这里用 json.RawMessage 二次解析更稳）。
	var rows [][]json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, &MarketError{
			Code:      "OKX_PARSE_FAILED",
			Message:   fmt.Sprintf("candles 不是二维数组: %v (raw=%s)", err, snippet(raw)),
			Channel:   ch,
			Retryable: true,
		}
	}
	if len(rows) == 0 {
		return nil, &MarketError{
			Code:      "OKX_NO_DATA",
			Message:   "candles 返回空数组",
			Channel:   ch,
			Retryable: true,
		}
	}

	// 逐行转成字符串切片
	str := func(rm json.RawMessage) string {
		var s string
		if err := json.Unmarshal(rm, &s); err == nil {
			return s
		}
		return string(rm) // 数字等原样
	}

	bars := make([]Bar, 0, len(rows))
	for i, row := range rows {
		if len(row) < 5 {
			return nil, &MarketError{
				Code:      "OKX_PARSE_FAILED",
				Message:   fmt.Sprintf("第 %d 行只有 %d 个字段，至少需要 5 个(ts/o/h/l/c)", i, len(row)),
				Channel:   ch,
				Retryable: true,
			}
		}
		b := Bar{
			TradingDate: tsToDate(str(row[0])),
			Open:        str(row[1]),
			High:        str(row[2]),
			Low:         str(row[3]),
			Close:       str(row[4]),
		}
		if len(row) > 5 {
			b.Volume = str(row[5])
		}
		if len(row) > 6 {
			b.Amount = str(row[6])
		}
		// [8] confirm：1=已收盘，0=未收盘。缺失时按「已收盘」处理（老版本可能不返回）。
		if len(row) > 8 {
			// 注意：confirm=="0" 表示**未收盘** → is_partial=true
			b.IsPartial = str(row[8]) == "0"
		}
		bars = append(bars, b)
	}

	// 反转成升序（OKX 返回新→旧，我们约定旧→新）
	for i, j := 0, len(bars)-1; i < j; i, j = i+1, j-1 {
		bars[i], bars[j] = bars[j], bars[i]
	}
	return bars, nil
}

// okxBar 把我们的 period 映射成 OKX 的 --bar 值。
//
// OKX bar 取值：1m/3m/5m/15m/30m/1H/2H/4H/6H/12H/1D/2D/3D/1W/1M/3M
func okxBar(period string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(period)) {
	case "daily", "1d", "":
		return "1D", nil
	case "weekly", "1w":
		return "1W", nil
	case "monthly", "1m":
		return "1M", nil
	case "1h":
		return "1H", nil
	case "4h":
		return "4H", nil
	default:
		return "", fmt.Errorf("okx 不支持的 period %q（daily|weekly|monthly|1h|4h）", period)
	}
}

// tsToDate 把毫秒时间戳转成 YYYY-MM-DD（UTC）。
func tsToDate(ts string) string {
	ms, err := strconv.ParseInt(strings.TrimSpace(ts), 10, 64)
	if err != nil || ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format("2006-01-02")
}

// FetchMacro OKX 无宏观数据。
func (c *OkxClient) FetchMacro(ctx context.Context, indicator string) (*Macro, error) {
	return nil, &MarketError{
		Code:      "CHANNEL_NOT_APPLICABLE",
		Message:   "OKX 不提供宏观数据（宏观走 akshare）",
		Channel:   ChannelOKX,
		Retryable: false,
	}
}

// 编译期确认 *OkxClient 实现了 MarketClient。
var _ MarketClient = (*OkxClient)(nil)

// errOkxNotApplicable 供测试断言用。
var errOkxNotApplicable = errors.New("okx 通道不适用")

// ---------------------------------------------------------------------
// 加密衍生品资金流（CryptoFlowClient 实现）
// ---------------------------------------------------------------------

// toSwapInstID 把 symbol 推导成 SWAP instID。
//
//	"BTC-USDT"           → "BTC-USDT-SWAP"
//	"BTC-USDT-SWAP"      → "BTC-USDT-SWAP"（已带后缀，幂等）
//	"BTC"                → "BTC-USDT-SWAP"（先补计价币再补后缀）
//	"BTC-USDT-250926"    → error（交割合约，P0 只支持永续）
//	""                   → error
//
// 为什么必须转：OKX 的 ticker / candles 用 SPOT（BTC-USDT），但
// funding-rate / open-interest **只对 SWAP 有效**。混用会让这两条命令
// 返回空数组或报错 —— 而不是给出一个明显的「参数错误」。
//
// 交割合约（形如 BTC-USDT-250926，末段是 6 位日期）不能用 SWAP 命令查，
// 也没有资金费率概念，所以显式拒绝而不是静默当成永续。
func toSwapInstID(symbol string) (string, error) {
	s := instIDForMarket(symbol, "")
	if s == "" {
		return "", fmt.Errorf("缺少 symbol")
	}
	const swapSuffix = "-SWAP"
	if strings.HasSuffix(s, swapSuffix) {
		return s, nil // 幂等
	}
	parts := strings.Split(s, "-")
	if len(parts) >= 3 {
		// 形如 BTC-USDT-250926：第三段是日期 → 交割合约
		last := parts[len(parts)-1]
		if len(last) == 6 && isAllDigits(last) {
			return "", fmt.Errorf("%q 是交割合约，P0 只支持永续(SWAP)；请用 %s-%s-SWAP",
				symbol, parts[0], parts[1])
		}
		return "", fmt.Errorf("%q 不是可识别的永续合约代码（期望 BASE-QUOTE，如 BTC-USDT）", symbol)
	}
	if len(parts) != 2 {
		return "", fmt.Errorf("%q 不是合法的 instID（期望 BASE-QUOTE，如 BTC-USDT）", symbol)
	}
	return s + swapSuffix, nil
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// FetchFundingRate 用 `okx market funding-rate <swapInstID>` 取资金费率。
//
// 返回是**对象数组**（不是 candles 的位置参数），取 [0]。
func (c *OkxClient) FetchFundingRate(ctx context.Context, symbol string) (*FundingRate, error) {
	swapID, err := toSwapInstID(symbol)
	if err != nil {
		return nil, &MarketError{Code: "BAD_REQUEST", Message: err.Error(), Channel: ChannelOKX, Retryable: false}
	}
	raw, err := c.run(ctx, "market", "funding-rate", swapID)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		InstID          string `json:"instId"`
		FundingRate     string `json:"fundingRate"`
		FundingTime     string `json:"fundingTime"`
		NextFundingTime string `json:"nextFundingTime"`
		InterestRate    string `json:"interestRate"`
		MaxFundingRate  string `json:"maxFundingRate"`
		MinFundingRate  string `json:"minFundingRate"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, &MarketError{
			Code:      "OKX_PARSE_FAILED",
			Message:   fmt.Sprintf("解析 funding-rate 失败: %v (raw=%s)", err, snippet(raw)),
			Channel:   ChannelOKX,
			Retryable: true,
		}
	}
	if len(rows) == 0 {
		return nil, &MarketError{
			Code:      "OKX_NO_DATA",
			Message:   fmt.Sprintf("funding-rate 返回空数组（%s 可能不是永续合约或无资金费率）", swapID),
			Channel:   ChannelOKX,
			Retryable: true,
		}
	}
	r := rows[0]
	return &FundingRate{
		InstID:          r.InstID,
		Rate:            r.FundingRate,
		FundingTime:     r.FundingTime,
		NextFundingTime: r.NextFundingTime,
		InterestRate:    r.InterestRate,
		MaxFundingRate:  r.MaxFundingRate,
		MinFundingRate:  r.MinFundingRate,
	}, nil
}

// FetchOpenInterest 用 `okx market open-interest --instType SWAP --instId <id>` 取持仓量。
func (c *OkxClient) FetchOpenInterest(ctx context.Context, symbol string) (*OpenInterest, error) {
	swapID, err := toSwapInstID(symbol)
	if err != nil {
		return nil, &MarketError{Code: "BAD_REQUEST", Message: err.Error(), Channel: ChannelOKX, Retryable: false}
	}
	raw, err := c.run(ctx, "market", "open-interest", "--instType", "SWAP", "--instId", swapID)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		InstID string `json:"instId"`
		OI     string `json:"oi"`
		OICcy  string `json:"oiCcy"`
		OIUsd  string `json:"oiUsd"`
		TS     string `json:"ts"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, &MarketError{
			Code:      "OKX_PARSE_FAILED",
			Message:   fmt.Sprintf("解析 open-interest 失败: %v (raw=%s)", err, snippet(raw)),
			Channel:   ChannelOKX,
			Retryable: true,
		}
	}
	if len(rows) == 0 {
		return nil, &MarketError{
			Code:      "OKX_NO_DATA",
			Message:   fmt.Sprintf("open-interest 返回空数组（%s 可能不是永续合约）", swapID),
			Channel:   ChannelOKX,
			Retryable: true,
		}
	}
	r := rows[0]
	return &OpenInterest{InstID: r.InstID, OI: r.OI, OICcy: r.OICcy, OIUsd: r.OIUsd, TS: r.TS}, nil
}

// 编译期确认 *OkxClient 同时实现了两个接口。
var _ CryptoFlowClient = (*OkxClient)(nil)

// FetchIndicator 用 `okx market indicator <name> <instId> --bar <bar>` 查服务端指标。
//
// # 三个实测踩出来的约束（都在这里挡住，不让它们变成难懂的 CLI 错误）
//
//  1. **参数顺序是 `<indicator> <instId>`** —— 写反会报
//     `Missing required parameter "instId"`。
//  2. **--bar 只接受 9 个值**，`1D` 不被接受（必须 `1Dutc`）。见 ValidOkxBar。
//  3. **stdout 前两行是升级提示** —— 实际只在有新版时出现，且走 stderr；
//     `run()` 已经只读 stdout 并接住 stderr，所以这里是纯 JSON。
//     但为稳妥，解析前仍用首字符定位 `[`/`{`（见 trimToJSON）。
func (c *OkxClient) FetchIndicator(ctx context.Context, in IndicatorQuery) (*IndicatorResult, error) {
	indicator := strings.ToLower(strings.TrimSpace(in.Indicator))
	if indicator == "" {
		return nil, &MarketError{Code: "BAD_REQUEST",
			Message: "缺少 indicator（如 rsi / macd / boll）", Channel: ChannelOKX, Retryable: false}
	}
	symbol := strings.TrimSpace(in.Symbol)
	if !strings.Contains(symbol, "-") {
		return nil, &MarketError{Code: "BAD_REQUEST",
			Message: fmt.Sprintf("symbol=%q 需要带连字符的 instId（如 BTC-USDT）", symbol),
			Channel: ChannelOKX, Retryable: false}
	}
	bar := strings.TrimSpace(in.Bar)
	if bar == "" {
		bar = "1Dutc"
	}
	if !ValidOkxBar[bar] {
		return nil, &MarketError{Code: "BAD_REQUEST",
			Message: "bar 必须是 3m/5m/15m/1H/4H/12Hutc/1Dutc/3Dutc/1Wutc 之一" +
				"（注意是 1Dutc 不是 1D）",
			Channel: ChannelOKX, Retryable: false}
	}

	// 注意参数顺序：indicator 在前，instId 在后。
	args := []string{"market", "indicator", indicator, symbol, "--bar", bar}
	if p := strings.TrimSpace(in.Params); p != "" {
		args = append(args, "--params", p)
	}
	if in.BacktestTime > 0 {
		args = append(args, "--backtest-time", strconv.FormatInt(in.BacktestTime, 10))
	}

	raw, err := c.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	return parseOkxIndicator(raw, indicator, symbol, bar, in.BacktestTime)
}

// FetchBalance 用 `okx account balance --json` 取交易账户资产。
//
// # 这是**唯一**能证明交易凭证有效的调用
//
// 行情接口（`okx market ticker`）不需要任何凭证 —— 实测无凭证也能返回实时价。
// 所以「行情通了」绝不能当成「凭证有效」。而 `account balance` 必须带
// 有效 API key/secret/passphrase 签名，无凭证直接
// `okx-auth rejected the caller (unauthorized)`。
//
// 凭证来自 OkxClient 注入的 cred（凭证页 → market_credentials 表），
// 由 run() 作为 OKX_API_KEY / OKX_SECRET_KEY / OKX_PASSPHRASE 传给 CLI 子进程。
//
// 响应形状（实测 2026-11）：数组，只取第一个账户。每个 details[] 是一个币种。
func (c *OkxClient) FetchBalance(ctx context.Context) (*AccountBalance, error) {
	raw, err := c.run(ctx, "account", "balance")
	if err != nil {
		return nil, err
	}
	return parseAccountBalance(raw)
}

// parseAccountBalance 解析 `okx account balance --json` 的输出。
//
// 与 FetchBalance 分开是为了可测：没有真凭证时也能验证解析与
// 「只留非零币种」的过滤逻辑（见 tools_account_balance_test.go）。
func parseAccountBalance(raw []byte) (*AccountBalance, error) {
	body := trimToJSON(raw)
	var outer []struct {
		TotalEq string `json:"totalEq"`
		AdjEq   string `json:"adjEq"`
		Upl     string `json:"upl"`
		UTime   string `json:"uTime"`
		Details []struct {
			Ccy       string `json:"ccy"`
			Eq        string `json:"eq"`
			CashBal   string `json:"cashBal"`
			AvailBal  string `json:"availBal"`
			FrozenBal string `json:"frozenBal"`
			EqUsd     string `json:"eqUsd"`
		} `json:"details"`
	}
	if err := json.Unmarshal(body, &outer); err != nil {
		return nil, &MarketError{
			Code:      "OKX_PARSE_FAILED",
			Message:   fmt.Sprintf("解析 account balance 失败: %v (raw=%s)", err, snippet(body)),
			Channel:   ChannelOKX,
			Retryable: true,
		}
	}
	if len(outer) == 0 {
		return nil, &MarketError{
			Code:      "OKX_NO_DATA",
			Message:   "account balance 返回空数组（账户无资产或凭证无该账户权限）",
			Channel:   ChannelOKX,
			Retryable: false,
		}
	}
	r := outer[0]
	out := &AccountBalance{
		TotalEq:    r.TotalEq,
		AdjustedEq: r.AdjEq,
		Upl:        r.Upl,
		UpdateTime: r.UTime,
	}
	// 只保留真正有余额的币种：OKX 会把账户支持的所有币种都列出来，
	// 绝大多数是 0。全返回会让模型以为「持有 200 种币」。
	for _, d := range r.Details {
		if isZeroAmount(d.Eq) && isZeroAmount(d.CashBal) && isZeroAmount(d.AvailBal) {
			continue
		}
		out.Currencies = append(out.Currencies, CurrencyBalance{
			Ccy:       d.Ccy,
			Eq:        d.Eq,
			CashBal:   d.CashBal,
			AvailBal:  d.AvailBal,
			FrozenBal: d.FrozenBal,
			EqUsd:     d.EqUsd,
		})
	}
	return out, nil
}

// isZeroAmount 判断金额字符串是否可视为 0（空串或 "0" / "0.0" / "0.000"）。
//
// 不用 float 解析：金额精度是铁律（见 FundingRate 的注释），
// 这里只需要「是不是全 0」，去掉小数点与前后空格后全是 '0' 即可。
func isZeroAmount(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return true
	}
	neg := false
	if s[0] == '-' || s[0] == '+' {
		neg, s = s[0] == '-', s[1:]
	}
	for _, r := range s {
		if r != '0' && r != '.' {
			// 任意一个非 0 数字 → 非零。-0.0 仍算 0。
			return false
		}
	}
	_ = neg
	return true
}

// parseOkxIndicator 解析 OKX indicator 的嵌套响应。
//
// 响应形状（实测，2026-10）：
//
//	[{"data":[{"instId":"BTC-USDT",
//	           "timeframes":{"1Dutc":{"indicators":{"RSI":[{"ts":"1793059200000",
//	                                                         "values":{"14":"39.47"}}]}}}}],
//	  "mode":"live","summary":{...},"timestamp":1793090646154}]
//
// 三层嵌套都要剥：数组 → data 数组 → timeframes[bar] → indicators[NAME] → 最后一条。
//
// **为什么取最后一条而不是第一条**：返回可能含多条历史（配合 --limit），
// 最新的一定在最后（服务端按时间升序）。取第一条会拿到最旧的值。
func parseOkxIndicator(raw []byte, indicator, symbol, bar string, backtestTime int64) (*IndicatorResult, error) {
	body := trimToJSON(raw)
	var outer []struct {
		Data []struct {
			InstID     string `json:"instId"`
			Timeframes map[string]struct {
				Indicators map[string][]struct {
					TS     string            `json:"ts"`
					Values map[string]string `json:"values"`
				} `json:"indicators"`
			} `json:"timeframes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &outer); err != nil {
		return nil, &MarketError{
			Code:      "OKX_PARSE_FAILED",
			Message:   fmt.Sprintf("解析 indicator 失败: %v (raw=%s)", err, snippet(body)),
			Channel:   ChannelOKX,
			Retryable: true,
		}
	}
	if len(outer) == 0 || len(outer[0].Data) == 0 {
		return nil, &MarketError{Code: "OKX_NO_DATA",
			Message: fmt.Sprintf("indicator 返回空（%s %s）", indicator, symbol),
			Channel: ChannelOKX, Retryable: true}
	}
	tfs := outer[0].Data[0].Timeframes
	tf, ok := tfs[bar]
	if !ok {
		// 服务端对不认识的 bar 可能静默返回别的 key —— 明确报出来，不要懵。
		keys := make([]string, 0, len(tfs))
		for k := range tfs {
			keys = append(keys, k)
		}
		return nil, &MarketError{Code: "OKX_NO_DATA",
			Message: fmt.Sprintf("响应里没有 bar=%q（实际有: %v）", bar, keys),
			Channel: ChannelOKX, Retryable: true}
	}
	// indicators 的 key 是大写（"RSI"/"MACD"），而我们的 indicator 是小写。
	var series []struct {
		TS     string            `json:"ts"`
		Values map[string]string `json:"values"`
	}
	for name, s := range tf.Indicators {
		if strings.EqualFold(name, indicator) {
			series = s
			break
		}
	}
	if len(series) == 0 {
		keys := make([]string, 0, len(tf.Indicators))
		for k := range tf.Indicators {
			keys = append(keys, k)
		}
		return nil, &MarketError{Code: "OKX_NO_DATA",
			Message: fmt.Sprintf("响应里没有 indicator=%q（实际有: %v）", indicator, keys),
			Channel: ChannelOKX, Retryable: true}
	}
	last := series[len(series)-1]
	ts := last.TS
	if ms, err := strconv.ParseInt(ts, 10, 64); err == nil && ms > 0 {
		ts = time.UnixMilli(ms).UTC().Format(time.RFC3339)
	}
	return &IndicatorResult{
		Indicator:    indicator,
		Symbol:       symbol,
		Bar:          bar,
		TS:           ts,
		Values:       last.Values,
		BacktestTime: backtestTime,
		Source:       "okx",
	}, nil
}

// trimToJSON 去掉可能的前置非 JSON 行（升级提示等）。
//
// 实测升级提示走 stderr，所以这通常是 no-op —— 但 CLI 行为会随版本变
// （1.4.8 → 1.4.9 就在改这块），所以不假设它一定干净。
func trimToJSON(raw []byte) []byte {
	s := bytes.TrimSpace(raw)
	for i := 0; i < len(s); i++ {
		if s[i] == '[' || s[i] == '{' {
			return s[i:]
		}
	}
	return s
}

// findOkxFallback 在 PATH 查找失败时，按已知安装位置探测 okx 可执行文件。
//
// # 为什么需要兜底
//
// 2026-11-05 实测：okx 装在
// %USERPROFILE%/.workbuddy/binaries/node/versions/<ver>/okx，
// 用户系统 PATH 里有这一项，但从 PowerShell 的 Start-Process 启动的
// 后端进程没有继承到 —— exec.LookPath 失败，工具报 OKX_CLI_NOT_FOUND。
//
// 后果不只是「报个错」：planner 看到该错误会判定「OKX CLI 未安装」，
// 派一个意图去安装/定位 CLI。而那个意图由 planner 自建、没有 role，
// 于是被当成普通 worker（拿到 Bash），进一步跑 CLI、写脚本、翻环境变量。
// 整条链的起点只是 PATH 没传进来。
//
// # 为什么只探测固定几个位置
//
// 递归搜全盘在 Windows 上会让首次取数卡十几秒（且可能命中多个版本）。
// 这里只查两类确定存在过的位置：WorkBuddy 自带的 node 环境，以及
// npm 全局目录（手动 npm i -g 装的情况）。找不到就返回空串，让上层如实报错。
func findOkxFallback() string {
	var candidates []string
	if home, err := os.UserHomeDir(); err == nil {
		// WorkBuddy 的 node 版本目录：只列一层，不递归。
		base := filepath.Join(home, ".workbuddy", "binaries", "node", "versions")
		if entries, err := os.ReadDir(base); err == nil {
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				candidates = append(candidates, filepath.Join(base, e.Name(), "okx"))
			}
		}
		// npm 全局（Windows 在 AppData/Roaming/npm，类 Unix 在 ~/.npm-global）。
		candidates = append(candidates,
			filepath.Join(home, "AppData", "Roaming", "npm", "okx.cmd"),
			filepath.Join(home, "AppData", "Roaming", "npm", "okx"),
			filepath.Join(home, ".npm-global", "bin", "okx"),
		)
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return ""
}
