package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	actool "github.com/Autumn-27/norma/tool"
)

// fakeMarketClient 是可控的 MarketClient，用来验证错误码映射与降级逻辑。
type fakeMarketClient struct {
	// quoteByChannel 每个通道返回什么。err 非 nil 时按错误处理。
	quoteByChannel map[Channel]struct {
		q   *Quote
		err error
	}
	klineByChannel map[Channel]struct {
		bars []Bar
		err  error
	}
	macro     *Macro
	macroErr  error
	warmup    bool
	healthErr error
	// calls 记录每个通道被调用了几次，用于验证降级顺序。
	calls []Channel
}

func (f *fakeMarketClient) Health(ctx context.Context) (bool, string, error) {
	return f.warmup, "fake", f.healthErr
}

func (f *fakeMarketClient) FetchQuote(ctx context.Context, symbol, market string, ch Channel) (*Quote, error) {
	f.calls = append(f.calls, ch)
	if r, ok := f.quoteByChannel[ch]; ok {
		return r.q, r.err
	}
	return nil, &MarketError{Code: "NO_ENTRY", Message: "fake 未配置该通道", Channel: ch, Retryable: true}
}

func (f *fakeMarketClient) FetchKline(ctx context.Context, symbol, market, period string, count int, ch Channel) ([]Bar, error) {
	f.calls = append(f.calls, ch)
	if r, ok := f.klineByChannel[ch]; ok {
		return r.bars, r.err
	}
	return nil, &MarketError{Code: "NO_ENTRY", Message: "fake 未配置该通道", Channel: ch, Retryable: true}
}

func (f *fakeMarketClient) FetchMacro(ctx context.Context, indicator string) (*Macro, error) {
	return f.macro, f.macroErr
}

func (f *fakeMarketClient) Timeout() time.Duration { return 5 * time.Second }

// callTool 调用一个工具并返回文本内容。
func callTool(t *testing.T, tool actool.CoreTool, in map[string]any) (string, bool) {
	t.Helper()
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal 入参: %v", err)
	}
	res, err := tool.Call(context.Background(), raw, nil)
	if err != nil {
		t.Fatalf("工具调用返回 error: %v", err)
	}
	var sb strings.Builder
	for _, c := range res.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}
	return sb.String(), res.IsError
}

// TestMarketToolMetasCoverAllTools 保证 marketToolMetas 与实际注册的工具一致。
//
// 两边任意一侧增删工具而忘了另一侧，这个测试立刻失败 —— 否则会出现
// 「工具注册了但 meta 里没有」（超时为 0，实际无超时）或
// 「meta 里有但工具没注册」（死配置）。
func TestMarketToolMetasCoverAllTools(t *testing.T) {
	mts := NewMarketToolSet(nil)
	tools := mts.Tools()

	if len(tools) != len(marketToolMetas) {
		t.Fatalf("工具数 %d != marketToolMetas 条目数 %d", len(tools), len(marketToolMetas))
	}
	for _, tool := range tools {
		name := tool.Name()
		meta, ok := marketToolMetas[name]
		if !ok {
			t.Errorf("工具 %q 注册了但 marketToolMetas 里没有（超时将无效）", name)
			continue
		}
		if meta.Timeout <= 0 {
			t.Errorf("工具 %q 的 Timeout 必须为正，当前 %v", name, meta.Timeout)
		}
		if meta.SourceKind == "" {
			t.Errorf("工具 %q 的 SourceKind 不能为空", name)
		}
		if len(meta.AssetClasses) == 0 {
			t.Errorf("工具 %q 的 AssetClasses 不能为空", name)
		}
		// 有通道的工具必须有超时；纯计算的工具（Channels 为空）也应有超时保护
	}
	// 反向：meta 里有但工具没注册
	registered := map[string]bool{}
	for _, tool := range tools {
		registered[tool.Name()] = true
	}
	for name := range marketToolMetas {
		if !registered[name] {
			t.Errorf("marketToolMetas 有 %q 但没有对应工具（死配置）", name)
		}
	}
}

// TestRetryableCode 锁定「哪些错误码该降级」的判定表。
//
// 这是降级策略的唯一依据，改错会导致「标的不存在时反复打上游」或
// 「源故障时不换源直接失败」。
func TestRetryableCode(t *testing.T) {
	cases := map[string]bool{
		// 不可重试：换源也没用
		"AKSHARE_BAD_REQUEST":      false,
		"AKSHARE_SYMBOL_NOT_FOUND": false,
		// 可重试：换源有意义
		"AKSHARE_NO_DATA":          true,
		"AKSHARE_HTML_CHALLENGE":   true,
		"AKSHARE_UPSTREAM_TIMEOUT": true,
		"AKSHARE_UPSTREAM_BLOCKED": true,
		"AKSHARE_PARSE_FAILED":     true,
		"AKSHARE_INTERNAL":         true,
		// 未知错误码：倾向重试（可能是新错误码，记日志）
		"SOME_FUTURE_CODE": true,
	}
	for code, want := range cases {
		if got := retryableCode(code); got != want {
			t.Errorf("retryableCode(%q) = %v, want %v", code, got, want)
		}
	}
}

// TestGetQuoteFallback 验证降级链：可重试错误换下一个通道，不可重试立刻停。
func TestGetQuoteFallback(t *testing.T) {
	t.Run("首个通道不可重试错误 → 不尝试后续通道", func(t *testing.T) {
		f := &fakeMarketClient{quoteByChannel: map[Channel]struct {
			q   *Quote
			err error
		}{
			ChannelAkshareCN: {err: &MarketError{
				Code: "AKSHARE_SYMBOL_NOT_FOUND", Message: "无此标的",
				Channel: ChannelAkshareCN, Retryable: false,
			}},
			ChannelAkshareHK: {q: &Quote{Symbol: "X", Last: "1"}},
		}}
		mts := NewMarketToolSet(f)
		var quoteTool actool.CoreTool
		for _, tool := range mts.Tools() {
			if tool.Name() == "get_quote" {
				quoteTool = tool
			}
		}
		out, isErr := callTool(t, quoteTool, map[string]any{"symbol": "NOPE", "market": "cn"})
		if !isErr {
			t.Fatalf("不可重试错误应返回错误结果，实际: %s", out)
		}
		if len(f.calls) != 1 {
			t.Errorf("不可重试错误不应尝试后续通道，实际调用了 %d 个: %v", len(f.calls), f.calls)
		}
	})

	t.Run("首个通道可重试错误 → 尝试下一个", func(t *testing.T) {
		f := &fakeMarketClient{quoteByChannel: map[Channel]struct {
			q   *Quote
			err error
		}{
			ChannelAkshareCN: {err: &MarketError{
				Code: "AKSHARE_UPSTREAM_BLOCKED", Message: "被反爬",
				Channel: ChannelAkshareCN, Retryable: true,
			}},
		}}
		mts := NewMarketToolSet(f)
		var quoteTool actool.CoreTool
		for _, tool := range mts.Tools() {
			if tool.Name() == "get_quote" {
				quoteTool = tool
			}
		}
		out, isErr := callTool(t, quoteTool, map[string]any{"symbol": "600519", "market": "cn"})
		if !isErr {
			t.Fatalf("全部通道失败应返回错误结果，实际: %s", out)
		}
		// cn 市场的通道链只有 akshare:cn（腾讯是 sidecar 内部的降级，
		// 在 Go 侧看来仍是同一个 Channel），所以只应调用 1 次。
		if len(f.calls) != 1 {
			t.Errorf("cn 通道链应只有 1 个 Channel，实际: %v", f.calls)
		}
	})

	t.Run("成功返回带 channel 与 note", func(t *testing.T) {
		f := &fakeMarketClient{quoteByChannel: map[Channel]struct {
			q   *Quote
			err error
		}{
			ChannelAkshareCN: {q: &Quote{
				Symbol: "600519", Market: "cn", PriceKind: "daily_close",
				TradingDate: "2026-10-26", Last: "1349.98", Channel: ChannelAkshareCN,
			}},
		}}
		mts := NewMarketToolSet(f)
		var quoteTool actool.CoreTool
		for _, tool := range mts.Tools() {
			if tool.Name() == "get_quote" {
				quoteTool = tool
			}
		}
		out, isErr := callTool(t, quoteTool, map[string]any{"symbol": "600519", "market": "cn"})
		if isErr {
			t.Fatalf("应成功，实际错误: %s", out)
		}
		if !strings.Contains(out, "daily_close") {
			t.Errorf("响应应含 price_kind=daily_close: %s", out)
		}
		if !strings.Contains(out, "不是实时价") {
			t.Errorf("响应应含语义提示「不是实时价」: %s", out)
		}
		if !strings.Contains(out, "akshare:cn") {
			t.Errorf("响应应含实际通道: %s", out)
		}
	})
}

// TestComputeIndicatorEndToEnd 验证纯计算工具（不需要 sidecar）。
//
// 这是装配链路最先可验证的一环：如果它能通，说明
// readTool → schema → handler → indicators.go 全链路正确。
func TestComputeIndicatorEndToEnd(t *testing.T) {
	mts := NewMarketToolSet(nil) // nil client：纯计算不需要
	var tool actool.CoreTool
	for _, x := range mts.Tools() {
		if x.Name() == "compute_indicator" {
			tool = x
		}
	}
	if tool == nil {
		t.Fatal("compute_indicator 未注册")
	}

	// 用 testdata 的真实 QQQ 收盘价（10 个点算 RSI 不够，用足够长的）
	bs := loadBars(t, "../testdata/qqq_2020_2021.psv")
	closes := bs.close[:60]

	out, isErr := callTool(t, tool, map[string]any{
		"indicator": "rsi",
		"closes":    closes,
		"period":    14,
	})
	if isErr {
		t.Fatalf("RSI 计算失败: %s", out)
	}
	var got struct {
		Indicator string   `json:"indicator"`
		Values    []string `json:"values"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\n%s", err, out)
	}
	if got.Indicator != "rsi" {
		t.Errorf("indicator = %q, want rsi", got.Indicator)
	}
	if len(got.Values) != len(closes) {
		t.Errorf("values 长度 %d != 输入长度 %d", len(got.Values), len(closes))
	}
	// 前 14 个应为空（warmup）
	for i := 0; i < 14; i++ {
		if got.Values[i] != "" {
			t.Errorf("warmup[%d] = %q，期望空串", i, got.Values[i])
		}
	}
	if got.Values[14] == "" {
		t.Error("index 14 应有 RSI 值")
	}

	// 未知指标应报错（不是静默返回空）
	out2, isErr2 := callTool(t, tool, map[string]any{"indicator": "bogus", "closes": closes})
	if !isErr2 {
		t.Errorf("未知指标应返回错误结果，实际: %s", out2)
	}
}

// TestMarketToolSetNilClient 验证 nil client 时工具仍可枚举，
// 但 Call 返回明确的「未启用」而不是 panic。
func TestMarketToolSetNilClient(t *testing.T) {
	mts := NewMarketToolSet(nil)
	tools := mts.Tools()
	if len(tools) == 0 {
		t.Fatal("nil client 时工具应仍可枚举（供 toolcatalog 读 Name/Desc/Schema）")
	}
	for _, tool := range tools {
		if tool.Name() == "" || tool.Description() == "" {
			t.Errorf("工具 %q 的 Name/Description 不应为空", tool.Name())
		}
		if tool.InputSchema() == nil {
			t.Errorf("工具 %q 的 InputSchema 不应为 nil", tool.Name())
		}
	}
	// 需要 client 的工具在 nil 时应报错而非 panic
	for _, tool := range tools {
		if tool.Name() != "get_quote" {
			continue
		}
		out, isErr := callTool(t, tool, map[string]any{"symbol": "600519", "market": "cn"})
		if !isErr {
			t.Errorf("nil client 时 get_quote 应返回错误，实际: %s", out)
		}
		if !strings.Contains(out, "未启用") {
			t.Errorf("错误信息应说明未启用: %s", out)
		}
	}
}

// TestAkshareClientIntegration 真连 sidecar 的集成测试。
//
// **没有 sidecar 时 skip**（不是 fail）—— 因为 CI 不一定起 sidecar。
// 手工验证时先启动 sidecar：
//
//	.venv-akshare/Scripts/python sidecars/akshare/server.py &
func TestAkshareClientIntegration(t *testing.T) {
	if os.Getenv("AKSHARE_INTEGRATION") != "1" {
		t.Skip("设 AKSHARE_INTEGRATION=1 并启动 sidecar 后运行（见 sidecars/akshare/README.md）")
	}
	c := NewAkshareClient("")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// 等预热完成（见 sidecars/akshare/README.md「冷启动与预热」）
	if err := c.WaitWarmup(ctx, 300*time.Millisecond); err != nil {
		t.Fatalf("等待预热失败: %v", err)
	}
	done, detail, err := c.Health(ctx)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !done {
		t.Fatalf("预热未完成: %s", detail)
	}

	t.Run("quote cn", func(t *testing.T) {
		q, err := c.FetchQuote(ctx, "600519", "cn", ChannelAkshareCN)
		if err != nil {
			t.Fatalf("FetchQuote: %v", err)
		}
		if q.PriceKind != "daily_close" {
			t.Errorf("price_kind = %q, want daily_close", q.PriceKind)
		}
		if q.Last == "" {
			t.Error("last 不应为空")
		}
		if q.TradingDate == "" {
			t.Error("trading_date 不应为空（语义边界依赖它）")
		}
		t.Logf("600519 last=%s date=%s channel=%s", q.Last, q.TradingDate, q.Channel)
	})

	t.Run("kline cn", func(t *testing.T) {
		bars, err := c.FetchKline(ctx, "600519", "cn", "daily", 5, ChannelAkshareCN)
		if err != nil {
			t.Fatalf("FetchKline: %v", err)
		}
		if len(bars) == 0 {
			t.Fatal("bars 为空")
		}
		if len(bars) > 5 {
			t.Errorf("count=5 但返回 %d 根", len(bars))
		}
		// 升序校验
		for i := 1; i < len(bars); i++ {
			if bars[i-1].TradingDate > bars[i].TradingDate {
				t.Errorf("bar 未按时间升序: %s > %s", bars[i-1].TradingDate, bars[i].TradingDate)
			}
		}
		t.Logf("600519 返回 %d 根，最后 %s close=%s", len(bars), bars[len(bars)-1].TradingDate, bars[len(bars)-1].Close)
	})

	t.Run("不存在的标的 → SYMBOL_NOT_FOUND 且不可重试", func(t *testing.T) {
		_, err := c.FetchKline(ctx, "INVALID999", "cn", "daily", 5, ChannelAkshareCN)
		if err == nil {
			t.Fatal("不存在的标的应返回错误")
		}
		var me *MarketError
		if !errors.As(err, &me) {
			t.Fatalf("错误应是 *MarketError，实际 %T: %v", err, err)
		}
		if me.Code != "AKSHARE_SYMBOL_NOT_FOUND" {
			t.Errorf("code = %q, want AKSHARE_SYMBOL_NOT_FOUND", me.Code)
		}
		if me.Retryable {
			t.Error("SYMBOL_NOT_FOUND 不应可重试（换源也没用）")
		}
	})
}
