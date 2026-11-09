package agent

// 「按数据源拆分的行情工具」的验收测试。
//
// # 这组测试守的是什么（2026-11-02 用户需求）
//
// 用户原话：「行情获取，查询，也都通过工具进行。多个行情数据源分别作为
// 不同的工具，并在可用时提供。」
//
// 两件事必须同时成立：
//  1. 每个可用的源有自己的工具（不能只有 get_quote 一个）
//  2. 不可用的源**不出现**在工具列表里（"在可用时提供"）
//
// 第 2 条最容易做错：无条件注册会让 agent 看到一个永远失败的工具，
// 它会在规划里反复尝试，且无法解释为什么不行。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	actool "github.com/Autumn-27/norma/tool"
)

// fakeQuoteClient 是满足 MarketClient 的最小桩。
//
// 记录被调用时的 channel，用于断言「工具只用本源通道、不降级」。
type fakeQuoteClient struct {
	quote     *Quote
	quoteErr  error
	lastChan  Channel
	callCount int
}

func (f *fakeQuoteClient) Health(context.Context) (bool, string, error) {
	return true, "fake", nil
}

func (f *fakeQuoteClient) FetchQuote(_ context.Context, _, _ string, ch Channel) (*Quote, error) {
	f.lastChan = ch
	f.callCount++
	if f.quoteErr != nil {
		return nil, f.quoteErr
	}
	return f.quote, nil
}

func (f *fakeQuoteClient) FetchKline(context.Context, string, string, string, int, Channel) ([]Bar, error) {
	return nil, nil
}

func (f *fakeQuoteClient) FetchMacro(context.Context, string) (*Macro, error) { return nil, nil }

// Timeout 满足 MarketClient（handler 用它建 context）。
func (f *fakeQuoteClient) Timeout() time.Duration { return 5 * time.Second }

// toolNamesOf 提取工具名集合。
func toolNamesOf(ts []actool.CoreTool) map[string]bool {
	out := make(map[string]bool, len(ts))
	for _, t := range ts {
		out[t.Name()] = true
	}
	return out
}

// callSourceTool 调用工具并返回 flatten 文本。
func callSourceTool(t *testing.T, tl actool.CoreTool, args map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("序列化入参: %v", err)
	}
	res, err := tl.Call(context.Background(), raw, nil)
	if err != nil {
		t.Fatalf("工具返回 error: %v", err)
	}
	return res.Flatten()
}

// TestSourceToolsOnlyAvailableSources 只有可用的源出现在工具列表里。
func TestSourceToolsOnlyAvailableSources(t *testing.T) {
	t.Run("只有 akshare", func(t *testing.T) {
		m := NewMarketToolSet(&fakeQuoteClient{})
		got := toolNamesOf(m.sourceQuoteTools())
		if !got["quote_akshare"] {
			t.Error("akshare 可用时应提供 quote_akshare")
		}
		if got["quote_okx"] {
			t.Error("okx 不可用时不该提供 quote_okx —— " +
				"那会让 agent 反复调用一个必然失败的工具")
		}
	})

	t.Run("akshare + okx", func(t *testing.T) {
		m := NewMarketToolSetWithCrypto(&fakeQuoteClient{}, &fakeQuoteClient{})
		got := toolNamesOf(m.sourceQuoteTools())
		if !got["quote_akshare"] || !got["quote_okx"] {
			t.Errorf("两个源都可用时应都提供，实际: %v", got)
		}
	})

	t.Run("都没有", func(t *testing.T) {
		// ⚠️ mxSource() 在 m.mx==nil 时会回退到环境变量 MX_APIKEY
		// （见 tools_mx.go 的实现）—— 那是有效配置来源，不是 bug。
		//
		// 所以「都没有」这个场景必须在**确定没有 MX_APIKEY** 的前提下验证，
		// 否则在配了环境变量的机器上会误报。
		// 这里显式清空环境变量来构造「真空」状态。
		t.Setenv("MX_APIKEY", "")
		m := NewMarketToolSet(nil)
		if n := len(m.sourceQuoteTools()); n != 0 {
			names := make([]string, 0, n)
			for _, tl := range m.sourceQuoteTools() {
				names = append(names, tl.Name())
			}
			t.Errorf("无可用源时不该有按源工具，实际 %d 个: %v", n, names)
		}
	})
}

// TestSeedShellHasAllSourceTools 种子壳必须提供**全部**按源工具名。
//
// # 这条与下一条的区别（关键）
//
// Tools() 挂在两个实例上，语义不同：
//
//	种子壳 NewMarketToolSet(nil) → 写 tools 表
//	运行实例                      → 真正执行
//
// 种子壳 client 全 nil。若它只返回可用源，则没有任何按源工具能写进
// tools 表 —— ToolResolve 按表过滤，agent 永远拿不到，且无任何报错。
func TestSeedShellHasAllSourceTools(t *testing.T) {
	seed := NewMarketToolSet(nil) // 种子壳：client 全 nil
	all := toolNamesOf(seed.Tools())
	for _, want := range SourceQuoteToolNames {
		if !all[want] {
			t.Errorf("种子壳缺 %q —— 该工具不会有 tools 表行，agent 永远拿不到", want)
		}
	}
}

// TestToolsIncludesSourceTools Tools() 必须真的把按源工具并进去。
//
// # 为什么单独测这一条
//
// sourceQuoteTools() 返回了工具，但忘记 append 到 Tools() 是很容易犯的错 ——
// 那样工具定义存在却永远不会给到 agent，且没有任何报错。
func TestToolsIncludesSourceTools(t *testing.T) {
	m := NewMarketToolSet(&fakeQuoteClient{})
	all := toolNamesOf(m.Tools())
	if !all["quote_akshare"] {
		t.Error("Tools() 没包含 quote_akshare —— 工具定义了但没给到 agent")
	}
	// 既有的 get_quote 必须还在（按源工具是新增，不是替换）
	if !all["get_quote"] {
		t.Error("get_quote 不该被移除（按源工具是新增能力）")
	}
}

// TestOkxToolHiddenWithoutCredential OKX 未注入凭证时不该提供 quote_okx。
//
// # 为什么这条重要
//
// NewOkxClient("") 只要二进制在 PATH 就构造成功 —— 即使一条凭证都没配。
// 那时若仍注册 quote_okx，agent 每次调用都拿到
// 「Not logged in — run okx-auth login first」，
// 而这句话完全指不出「你该去凭证页填 key」。
func TestOkxToolHiddenWithoutCredential(t *testing.T) {
	okx := NewOkxClient("") // 无凭证
	m := NewMarketToolSetWithCrypto(&fakeQuoteClient{}, okx)

	got := toolNamesOf(m.sourceQuoteTools())
	if got["quote_okx"] {
		t.Error("OKX 未注入凭证时不该提供 quote_okx —— " +
			"调用只会得到误导性的「Not logged in」")
	}

	// 注入后应出现
	okx.SetCredential(&OkxCredential{APIKey: "k", SecretKey: "s", Passphrase: "p"})
	got2 := toolNamesOf(m.sourceQuoteTools())
	if !got2["quote_okx"] {
		t.Error("注入凭证后应提供 quote_okx")
	}
}

// TestSourceToolUsesOwnChannel 按源工具必须只用本源通道。
//
// # 为什么这条是核心
//
// 拆分工具的全部意义就是「来源明确」。如果 quote_akshare 内部降级到了
// OKX，用户会拿到标着 akshare 但实际来自 OKX 的价格 —— 比不拆更糟。
func TestSourceToolUsesOwnChannel(t *testing.T) {
	fake := &fakeQuoteClient{
		quote: &Quote{Symbol: "600519", Market: "cn", Last: "1500"},
	}
	m := NewMarketToolSet(fake)

	var target actool.CoreTool
	for _, tl := range m.sourceQuoteTools() {
		if tl.Name() == "quote_akshare" {
			target = tl
			break
		}
	}
	if target == nil {
		t.Fatal("找不到 quote_akshare")
	}

	callSourceTool(t, target, map[string]any{"symbol": "600519", "market": "cn"})

	if fake.callCount != 1 {
		t.Errorf("请求次数 = %d, want 1", fake.callCount)
	}
	if fake.lastChan != ChannelAkshareCN {
		t.Errorf("使用通道 = %q, want %q（不该降级到别的源）",
			fake.lastChan, ChannelAkshareCN)
	}
}

// TestSourceToolRejectsWrongMarket 市场不属于该源时明确拒绝，且不发请求。
//
// 例如 quote_okx 收到 market=cn —— 应当说「不支持 cn」，
// 而不是转去 akshare 拿 A 股数据（那会让来源标签再次失真）。
func TestSourceToolRejectsWrongMarket(t *testing.T) {
	aks := &fakeQuoteClient{}
	okx := &fakeQuoteClient{}
	m := NewMarketToolSetWithCrypto(aks, okx)

	var target actool.CoreTool
	for _, tl := range m.sourceQuoteTools() {
		if tl.Name() == "quote_okx" {
			target = tl
			break
		}
	}
	if target == nil {
		t.Fatal("找不到 quote_okx")
	}

	out := callSourceTool(t, target, map[string]any{"symbol": "600519", "market": "cn"})
	if !strings.Contains(out, "不支持") {
		t.Errorf("quote_okx 收到 cn 应明确拒绝，实际: %s", out)
	}
	if okx.callCount != 0 {
		t.Error("拒绝前不该真正发起请求")
	}
	if aks.callCount != 0 {
		t.Error("拒绝后不该转去 akshare（那就退化成降级了）")
	}
}

// TestSourceToolErrorMentionsSource 失败时错误必须说明是哪个源。
//
// # 为什么不降级时这点尤其重要
//
// 降级模式下错误是「所有通道失败」，看不出谁挂了。
// 按源模式下只有一个源，错误必须直接说「Akshare 查询失败」——
// 否则 agent 无法判断该换源还是该修配置。
func TestSourceToolErrorMentionsSource(t *testing.T) {
	fake := &fakeQuoteClient{
		quoteErr: &MarketError{Code: "AKSHARE_DOWN", Message: "sidecar 未响应", Retryable: true},
	}
	m := NewMarketToolSet(fake)

	var target actool.CoreTool
	for _, tl := range m.sourceQuoteTools() {
		if tl.Name() == "quote_akshare" {
			target = tl
			break
		}
	}
	if target == nil {
		t.Fatal("找不到 quote_akshare")
	}

	out := callSourceTool(t, target, map[string]any{"symbol": "600519", "market": "cn"})
	if !strings.Contains(out, "AkShare") {
		t.Errorf("错误应指明是 AkShare 失败，实际: %s", out)
	}
	if !strings.Contains(out, "未降级") {
		t.Errorf("错误应说明未降级（避免用户以为是全局故障），实际: %s", out)
	}
}

// TestPickLatestQuoteFromMX 妙想表格 → Quote 的解析规则。
//
// # 为什么要单独测这个纯函数
//
// quote_mx 的价值全在这个提取里：把「若干张中文表」变成「一个价格」。
// 取错列（比如把成交量当价格）会给出一个看似合理但完全错误的数字，
// 比查不到危险得多。所以规则必须逐条锁住。
func TestPickLatestQuoteFromMX(t *testing.T) {
	t.Run("时间序列表取最后一行收盘价", func(t *testing.T) {
		res := &MXQueryResult{
			TableCount: 1,
			Tables: []MXTable{{
				Name: "贵州茅台(600519.SH)的收盘价", Code: "600519.SH",
				HeadLabel: "日期",
				Columns:   []string{"日期", "收盘价", "成交量"},
				Rows: []map[string]string{
					{"日期": "2026-10-30", "收盘价": "1500.00", "成交量": "1000"},
					{"日期": "2026-11-02", "收盘价": "1520.50", "成交量": "1200"},
				},
			}},
		}
		q, err := pickLatestQuoteFromMX(res, "600519", "cn")
		if err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if q.Last != "1520.50" {
			t.Errorf("Last = %q, want 1520.50（应取最后一行，不是第一行）", q.Last)
		}
		if q.TradingDate != "2026-11-02" {
			t.Errorf("TradingDate = %q, want 2026-11-02", q.TradingDate)
		}
		if q.Channel != ChannelMX {
			t.Errorf("Channel = %q, want mx", q.Channel)
		}
	})

	t.Run("没有收盘列时退到含价格的列", func(t *testing.T) {
		res := &MXQueryResult{
			TableCount: 1,
			Tables: []MXTable{{
				HeadLabel: "日期",
				Columns:   []string{"日期", "最新价"},
				Rows: []map[string]string{
					{"日期": "2026-11-02", "最新价": "88.8"},
				},
			}},
		}
		q, err := pickLatestQuoteFromMX(res, "00700", "hk")
		if err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if q.Last != "88.8" {
			t.Errorf("Last = %q, want 88.8", q.Last)
		}
	})

	t.Run("只有成交量等非价格列时明确报错", func(t *testing.T) {
		res := &MXQueryResult{
			TableCount: 1,
			Tables: []MXTable{{
				HeadLabel: "日期",
				Columns:   []string{"日期", "成交量", "换手率"},
				Rows: []map[string]string{
					{"日期": "2026-11-02", "成交量": "1200", "换手率": "1.2"},
				},
			}},
		}
		if _, err := pickLatestQuoteFromMX(res, "600519", "cn"); err == nil {
			t.Fatal("没有价格列时必须报错，不能把成交量/换手率当价格")
		}
	})

	t.Run("空结果报错", func(t *testing.T) {
		if _, err := pickLatestQuoteFromMX(&MXQueryResult{}, "x", "cn"); err == nil {
			t.Fatal("无表时必须报错")
		}
	})
}
