package agent

// W5 角色 prompt 分叉的测试。
//
// 三个必测点：
//  1. 渗透 intent（无 role）走原 workerSystem —— 零回归
//  2. 金融 intent 走 renderRolePrompt，且渲染无残留 {{
//  3. ctx 无 MarketClient 时走 stub，.Bars 必须含「尚未接入」而非空串

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// mkIntentNode 造一个带 payload 的 intent 节点（不落库）。
func mkIntentNode(t *testing.T, payload map[string]any) *db.Node {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("序列化 payload: %v", err)
	}
	return &db.Node{ID: 1, Kind: db.KindIntent, State: "open", Payload: raw}
}

// TestAgentKeyForIntentPenetrationReturnsWorker 渗透 intent（无 role）→ "worker"。
//
// 这是零回归的核心断言：渗透路径必须走原 workerSystem。
func TestAgentKeyForIntentPenetrationReturnsWorker(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
		want    string
	}{
		{"只有 summary（渗透意图的典型形态）", map[string]any{"summary": "探测 example.com"}, "worker"},
		{"summary + asset_ids", map[string]any{"summary": "打 1.2.3.4", "asset_ids": []any{1.0, 2.0}}, "worker"},
		{"role 为空串", map[string]any{"summary": "x", "role": ""}, "worker"},
		{"role 是空白", map[string]any{"summary": "x", "role": "  "}, "worker"},
		{"role 不是字符串", map[string]any{"summary": "x", "role": 123}, "worker"},
	}
	for _, c := range cases {
		got := agentKeyForIntent(mkIntentNode(t, c.payload))
		if got != c.want {
			t.Errorf("%s: agentKey = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestAgentKeyForIntentFinancialReturnsRole 金融 intent → 用 role 作为 agentKey。
func TestAgentKeyForIntentFinancialReturnsRole(t *testing.T) {
	for _, role := range []string{
		"analyst_market", "analyst_sentiment_crypto",
		"debate_bull", "debate_bear", "research_manager",
		"trader", "risk_aggressive", "risk_neutral",
		"risk_conservative", "portfolio_manager",
	} {
		got := agentKeyForIntent(mkIntentNode(t, map[string]any{
			"summary":       "阶段工作",
			"role":          role,
			"instrument_id": 42,
			"asset_class":   "stock",
		}))
		if got != role {
			t.Errorf("agentKey = %q, want %q", got, role)
		}
	}
}

// TestAgentKeyForIntentNilNode nil 节点不 panic，返回 worker。
func TestAgentKeyForIntentNilNode(t *testing.T) {
	if got := agentKeyForIntent(nil); got != "worker" {
		t.Errorf("nil intent → %q, want worker", got)
	}
	if got := agentKeyForIntent(&db.Node{}); got != "worker" {
		t.Errorf("空 payload → %q, want worker", got)
	}
}

// TestRenderRolePromptAnalystStubNoClient 无 MarketClient 时走 stub。
//
// 核心断言：`.Bars` 必须含「尚未接入」的**说明**，不能是空串。
// 空串会让 LLM 以为「今天没有 K 线」，说明会让它知道「这层没接通」。
func TestRenderRolePromptAnalystStubNoClient(t *testing.T) {
	intent := mkIntentNode(t, map[string]any{
		"role": "analyst_market", "instrument_id": 42, "asset_class": "stock",
	})
	ctx := context.Background() // 故意不注入 MarketClient

	out, ok := renderRolePrompt(ctx, "analyst_market", intent)
	if !ok {
		t.Fatal("analyst_market 应走 role prompt 路径")
	}
	if strings.Contains(out, "{{") {
		idx := strings.Index(out, "{{")
		end := idx + 80
		if end > len(out) {
			end = len(out)
		}
		t.Fatalf("渲染有残留占位符（位置 %d）：%q", idx, out[idx:end])
	}
	if !strings.Contains(out, "行情数据尚未接入") {
		t.Error(".Bars 必须含「尚未接入」的说明 —— 空串会让 LLM 以为「今天没有 K 线」")
	}
	if !strings.Contains(out, "行情数据未接入") {
		t.Error("DataGaps 必须写明「行情数据未接入」—— 这样 prompt 的缺失处理章节才会生效")
	}
	if strings.Contains(out, "| 数据缺口 | 无 |") {
		t.Error("DataGaps 非空时不该走 else 分支的「无」")
	}
}

// TestRenderRolePromptDebateStub 辩论类角色走 stubVarsForRole。
func TestRenderRolePromptDebateStub(t *testing.T) {
	intent := mkIntentNode(t, map[string]any{"role": "debate_bull"})
	out, ok := renderRolePrompt(context.Background(), "debate_bull", intent)
	if !ok {
		t.Fatal("debate_bull 应走 role prompt 路径")
	}
	if strings.Contains(out, "{{") {
		t.Error("渲染有残留占位符")
	}
	// 文案从「尚未接入」改成「上游尚未产出」—— 这个区分是有意的：
	// 「代码没写」与「上游还没跑完」的排查方向不同（前者等开发，后者等执行）。
	if !strings.Contains(out, "上游尚未产出") {
		t.Error("AnalystFacts 应含「上游尚未产出」说明（区分于「数据源未接入」）")
	}
	// 第 1 轮不该出现「上一轮摘要」
	if !strings.Contains(out, "这是第 1 轮") {
		t.Error("Round=1 时应走「这是第 1 轮」分支")
	}
}

// TestRenderRolePromptTraderStubSession trader 的 SessionError 必须走「未知」分支。
func TestRenderRolePromptTraderStubSession(t *testing.T) {
	intent := mkIntentNode(t, map[string]any{"role": "trader"})
	out, ok := renderRolePrompt(context.Background(), "trader", intent)
	if !ok {
		t.Fatal("trader 应走 role prompt 路径")
	}
	if strings.Contains(out, "{{") {
		t.Error("渲染有残留占位符")
	}
	if !strings.Contains(out, "交易时段未知") {
		t.Error("SessionError 非空时应走「交易时段未知」分支，" +
			"不能假设「等开盘」—— 那可能正在盘中")
	}
}

// TestRenderRolePromptPortfolioManagerStub PM 走 stub 且含 review 门槛。
func TestRenderRolePromptPortfolioManagerStub(t *testing.T) {
	intent := mkIntentNode(t, map[string]any{"role": "portfolio_manager"})
	out, ok := renderRolePrompt(context.Background(), "portfolio_manager", intent)
	if !ok {
		t.Fatal("portfolio_manager 应走 role prompt 路径")
	}
	if strings.Contains(out, "{{") {
		t.Error("渲染有残留占位符")
	}
	if !strings.Contains(out, "review 门槛检查") {
		t.Error("prompt 应含 review 四项门槛")
	}
}

// TestRenderRolePromptPenetrationReturnsFalse 渗透 agentKey 不走 role prompt。
func TestRenderRolePromptPenetrationReturnsFalse(t *testing.T) {
	intent := mkIntentNode(t, map[string]any{"summary": "探测"})
	for _, key := range []string{"worker", "planner", "mainagent", "goals", "auto", "pentest"} {
		if _, ok := renderRolePrompt(context.Background(), key, intent); ok {
			t.Errorf("%q 不该走 role prompt 路径（渗透路径必须保持原样）", key)
		}
	}
}

// TestRenderRolePromptUnknownFinancialFallback 有 role 但无 prompt → 退回 worker。
func TestRenderRolePromptUnknownFinancialFallback(t *testing.T) {
	intent := mkIntentNode(t, map[string]any{"role": "analyst_unknown"})
	if _, ok := renderRolePrompt(context.Background(), "analyst_unknown", intent); ok {
		t.Error("缺 prompt 的金融 key 应返回 false，让调用方退回 workerSystem")
	}
}

// TestBuildAnalystVarsStubFields stub 的每个字段都有明确说明。
func TestBuildAnalystVarsStubFields(t *testing.T) {
	intent := mkIntentNode(t, map[string]any{
		"role": "analyst_market", "instrument_id": 42, "asset_class": "stock",
	})
	vars := buildAnalystVars(context.Background(), intent) // 无 client

	if !strings.Contains(vars.Bars, "尚未接入") {
		t.Errorf("Bars = %q, 应含「尚未接入」说明", vars.Bars)
	}
	if len(vars.DataGaps) == 0 {
		t.Error("DataGaps 不该为空 —— 空会让 LLM 以为「数据齐全」")
	}
	if vars.BarsJSON != "[]" {
		t.Errorf("BarsJSON = %q, want \"[]\"（空数组而不是空串，便于 JSON 解析）", vars.BarsJSON)
	}
	// Instrument 从 payload 的 asset_class 推出
	if vars.Instrument.AssetClass != "stock" {
		t.Errorf("AssetClass = %q, want stock（应从 payload 读）", vars.Instrument.AssetClass)
	}
}

// TestBuildAnalystVarsCryptoMarket payload 的 asset_class=crypto 时 market 自动填 crypto。
func TestBuildAnalystVarsCryptoMarket(t *testing.T) {
	intent := mkIntentNode(t, map[string]any{
		"role": "analyst_sentiment_crypto", "instrument_id": 7, "asset_class": "crypto",
	})
	vars := buildAnalystVars(context.Background(), intent)
	if vars.Instrument.Market != "crypto" {
		t.Errorf("Market = %q, want crypto", vars.Instrument.Market)
	}
}

// TestInstrumentFromPayloadNoFakeSymbol 缺 symbol 时留空，不编造。
//
// payload 里只有 instrument_id 与 asset_class，没有 symbol。
// 编一个假 symbol 会让 LLM 分析错标的 —— 留空 + 让 DataGaps 说明更安全。
func TestInstrumentFromPayloadNoFakeSymbol(t *testing.T) {
	intent := mkIntentNode(t, map[string]any{"role": "analyst_market", "instrument_id": 42})
	vars := buildAnalystVars(context.Background(), intent)
	if vars.Instrument.Symbol != "" {
		t.Errorf("Symbol = %q, 不该编造（payload 里没有 symbol）", vars.Instrument.Symbol)
	}
}

// TestFundamentalQuestionLabel 问句锚点选择。
//
// us 只用 ticker：实测妙想对英文公司名（NVIDIA Corp）返回 0 表，
// 对同标的 ticker（NVDA）返回 2 表。cn/hk 用「中文名 代码」。
func TestFundamentalQuestionLabel(t *testing.T) {
	cases := []struct {
		market, name, sym, want string
	}{
		{"us", "NVIDIA Corp", "NVDA", "NVDA"},
		{"us", "AAPL", "AAPL", "AAPL"},
		{"us", "", "AAPL", "AAPL"},
		{"hk", "腾讯控股", "00700.HK", "腾讯控股 00700.HK"},
		{"cn", "贵州茅台", "600519", "贵州茅台 600519"},
		{"cn", "", "600519", "600519"},
		{"cn", "苹果(AAPL)", "AAPL", "苹果(AAPL)"}, // name 已含代码，不重复拼
		{"cn", "", "", ""},
	}
	for _, c := range cases {
		if got := fundamentalQuestionLabel(c.market, c.name, c.sym); got != c.want {
			t.Errorf("market=%s name=%q sym=%q → %q, want %q", c.market, c.name, c.sym, got, c.want)
		}
	}
}

// TestWithMarketClientNilIsNoop 注入 nil 等于不注入。
func TestWithMarketClientNilIsNoop(t *testing.T) {
	ctx := WithMarketClient(context.Background(), nil)
	if _, ok := MarketClientFrom(ctx); ok {
		t.Error("注入 nil 后 MarketClientFrom 应返回 ok=false")
	}
}

// TestWithExplorationStoreNilIsNoop 同上。
func TestWithExplorationStoreNilIsNoop(t *testing.T) {
	ctx := WithExplorationStore(context.Background(), nil)
	if _, ok := ExplorationStoreFrom(ctx); ok {
		t.Error("注入 nil 后 ExplorationStoreFrom 应返回 ok=false")
	}
}

// TestInstrumentIDFromPayloadTypeDrift instrument_id 的类型漂移处理。
//
// # 为什么必须测这个
//
// materialize 写入时是 int64，但 JSONB 读回来经 encoding/json 后数字一律是
// **float64**。直接 `p["instrument_id"].(int64)` 会断言失败返回零值 ——
// 静默变成「无标的」，没有任何报错。
func TestInstrumentIDFromPayloadTypeDrift(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
		want    int64
	}{
		// 从 DB 读回的标准形态
		{"float64（DB 读回）", map[string]any{"instrument_id": float64(5)}, 5},
		{"float64 大数", map[string]any{"instrument_id": float64(1234567)}, 1234567},
		// 内存中直接构造
		{"int64", map[string]any{"instrument_id": int64(7)}, 7},
		{"int", map[string]any{"instrument_id": 9}, 9},
		// 缺失
		{"无字段", map[string]any{"summary": "x"}, 0},
		{"nil 值", map[string]any{"instrument_id": nil}, 0},
		{"字符串（非数字）", map[string]any{"instrument_id": "abc"}, 0},
		{"空串", map[string]any{"instrument_id": ""}, 0},
	}
	for _, c := range cases {
		got := instrumentIDFromPayload(mkIntentNode(t, c.payload))
		if got != c.want {
			t.Errorf("%s: instrumentIDFromPayload = %d, want %d", c.name, got, c.want)
		}
	}
	// nil intent 不 panic
	if got := instrumentIDFromPayload(nil); got != 0 {
		t.Errorf("nil intent → %d, want 0", got)
	}
}

// TestAnalystVarsStubTwoKindsOfMissing 两种「标的缺失」必须给不同的 gap。
//
// 区分它们是因为排查方向不同：
//   - instID == 0   → 配置/代码问题（payload 没传或类型不对）
//   - instID != 0   → 未实现的代码（执行器还没接 instruments 查询）
func TestAnalystVarsStubTwoKindsOfMissing(t *testing.T) {
	t.Run("无 instrument_id", func(t *testing.T) {
		intent := mkIntentNode(t, map[string]any{"role": "analyst_market"})
		vars := analystVarsStub(context.Background(), intent)
		found := false
		for _, g := range vars.DataGaps {
			if strings.Contains(g, "标的未知") {
				found = true
			}
		}
		if !found {
			t.Errorf("无 id 时应给「标的未知」，实际 DataGaps=%v", vars.DataGaps)
		}
	})

	t.Run("有 instrument_id 但未加载", func(t *testing.T) {
		intent := mkIntentNode(t, map[string]any{
			"role": "analyst_market", "instrument_id": float64(42),
		})
		vars := analystVarsStub(context.Background(), intent)
		found := false
		for _, g := range vars.DataGaps {
			if strings.Contains(g, "instrument_id=42") && strings.Contains(g, "未加载") {
				found = true
			}
		}
		if !found {
			t.Errorf("有 id 时应给「instrument_id=42 未加载」，实际 DataGaps=%v", vars.DataGaps)
		}
		// 两条 gap 都要有
		if len(vars.DataGaps) < 2 {
			t.Errorf("应有「标的信息未加载」+「行情未接入」两条，实际 %d 条: %v",
				len(vars.DataGaps), vars.DataGaps)
		}
	})
}

// TestWorkerSystemUnchangedForPentestIntent 渗透路径的 sysBody 与金融路径完全隔离。
//
// 断言「不含金融关键词」比「含渗透关键词」更强 —— 两个分支都可能含通用文案，
// 但只有金融分支才会有「技术面分析师」这类角色名。
func TestWorkerSystemUnchangedForPentestIntent(t *testing.T) {
	intent := mkIntentNode(t, map[string]any{"summary": "探测 example.com 的 SQL 注入"})

	agentKey := agentKeyForIntent(intent)
	if agentKey != "worker" {
		t.Fatalf("渗透 intent 的 agentKey = %q, want worker", agentKey)
	}

	// 走 role prompt 应返回 false（调用方据此退回 workerSystem）
	out, ok := renderRolePrompt(context.Background(), agentKey, intent)
	if ok {
		t.Fatal("渗透 agentKey 不该走 role prompt 路径")
	}
	if out != "" {
		t.Errorf("返回 false 时 out 应为空，实际 %d 字节", len(out))
	}

	// 对照：确认 workerSystem 的产物里没有金融角色文案
	pentestBody := workerSystem("", "", t.TempDir(), t.TempDir())
	for _, financialKeyword := range []string{
		"技术面分析师", "多头研究员", "组合经理", "加密情绪分析师",
		"emit_rating", "review 门槛检查",
	} {
		if strings.Contains(pentestBody, financialKeyword) {
			t.Errorf("渗透 sysBody 不该含金融角色文案 %q —— 说明两个分支串了",
				financialKeyword)
		}
	}
	// 渗透关键词在（确认确实是渗透 prompt）
	if !strings.Contains(pentestBody, "执行者") {
		t.Error("渗透 sysBody 应含「执行者」—— 确认拿到的是 worker 模板")
	}
}
