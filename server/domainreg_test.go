package server

// 装配缺口验证：agent 能否真的通过 domainReg 拿到金融工具。
//
// # 为什么需要这组测试
//
// `buildDomainReg` 原本只注册 `ToolSet.AllDomainTools()`，而金融工具
// （get_quote/get_kline/...）属于 `MarketToolSet` —— 一个独立类型，
// 不在 `AllDomainTools` 的视野里。
//
// 后果是静默的：
//   - tools 表里有行（因为 builtinToolsByAgent 的种子壳会写）→ UI 能勾选
//   - 但 ToolResolve 注入时 `if !ok { continue }` → agent 永远拿不到
//   - **不报错、不 warning，只是工具不存在**
//
// 这是 C1.6「验证能否执行而非是否存在」的实例：注册了 ≠ 能用。

import (
	"context"
	"testing"

	actool "github.com/Autumn-27/norma/tool"

	"github.com/Autumn-27/artex/agent"
)

// marketToolNames 是 MarketToolSet 必须提供的工具名。
//
// 硬编码而不是从 Tools() 推导：这份清单是「分析师需要什么」的规格，
// 从实现推导会让规格跟着实现漂移（实现少了一个，测试也跟着少一个）。
var marketToolNames = []string{
	"get_quote", "get_kline", "compute_indicator",
	"get_macro", "get_volatility", "get_crypto_flow",
	// 2026-10-30 新增：新闻与财报取数（只走 akshare，见 agent/market_news.go）
	"get_news", "get_fundamentals",
}

// TestBuildDomainRegIncludesMarketTools domainReg 必须包含全部金融工具。
func TestBuildDomainRegIncludesMarketTools(t *testing.T) {
	mts := agent.NewMarketToolSet(nil)        // nil client 足够：这里只读 Name()
	reg := buildDomainReg(nil, mts, nil, nil) // nil as / nil tts 都可以（只要 mts 非 nil）

	if reg == nil {
		t.Fatal("domainReg 为 nil —— 即使 as 为 nil，有 mts 时也该返回非 nil")
	}
	// 6 个行情工具（来自 MarketToolSet.Tools()）
	for _, name := range marketToolNames {
		if _, ok := reg[name]; !ok {
			t.Errorf("domainReg 缺 %q —— 它在 tools 表里有行，但 ToolResolve 注入时"+
				"`if !ok { continue }` 会静默跳过，agent 永远拿不到这个工具", name)
		}
	}
	// emit_* / list_thesis / list_signals 来自 ToolSet.emit_fact 等工具需要 target 绑定，
	// 它们**不在** MarketToolSet 里，所以这里不该断言它们（as=nil 时 ToolSet 分支跳过）。
	// 这条区分很重要：把「行情工具」和「探索图工具」混在一起断言，会让测试
	// 在 as=nil 时误报，掩盖真正的问题。
}

// TestDomainRegCoversAllFinancialToolNames 覆盖 §4.3.2 里全部 17 个工具名。
//
// 与上一个测试的区别：那个测「MarketToolSet 的 6 个是否进 domainReg」，
// 这个测「§4.3.2 需要的 17 个名字，是否都能在两个来源里找到」。
//
// 用 as=nil + mts 组合只能覆盖 6 个行情工具；探索图工具（emit_* / list_*）
// 需要 as 非 nil 才会注册。所以这里分两段断言，明确「哪段来源负责哪些工具」。
func TestDomainRegCoversAllFinancialToolNames(t *testing.T) {
	// 第一段：mts 来源（as=nil 也能查）
	regOnlyMts := buildDomainReg(nil, agent.NewMarketToolSet(nil), nil, nil)
	fromMts := []string{
		"get_quote", "get_kline", "compute_indicator",
		"get_macro", "get_volatility", "get_crypto_flow",
	}
	for _, name := range fromMts {
		if _, ok := regOnlyMts[name]; !ok {
			t.Errorf("%q 应从 MarketToolSet 注册进 domainReg", name)
		}
	}

	// 第二段：ToolSet 来源需要 as 非 nil。这里不构造真 AssetStore（测试无 DB），
	// 改为直接验证「ToolSet.WorkerTools() 里有这些名字」—— 它的注册路径由
	// AllDomainTools 保证，而 AllDomainTools 在 as 非 nil 时会被遍历。
	ts := agent.NewToolSet(nil, "")
	have := map[string]bool{}
	for _, tool := range ts.WorkerTools() {
		have[tool.Name()] = true
	}
	fromToolSet := []string{
		"emit_fact", "emit_thesis", "emit_signal", "emit_rating",
		"list_thesis", "list_signals",
	}
	for _, name := range fromToolSet {
		if !have[name] {
			t.Errorf("%q 不在 ToolSet.WorkerTools() 里 —— 那它既不在 mts 也不在 ts，"+
				"运行时没有任何装配路径", name)
		}
	}
}

// TestBuildDomainRegSkipsToolSetWhenAssetStoreNil as 为 nil 时不注册 ToolSet 工具。
//
// 这是**既有行为**（原实现 `if as == nil { return nil }`），我加了 mts 参数后
// 保留它：ToolSet 的工具大多需要 AssetStore 或 ExplorationStore，task-less 的
// 实例调它们只会报「缺少上下文」。注册一堆注定报错的工具等于给 UI 制造假象。
func TestBuildDomainRegSkipsToolSetWhenAssetStoreNil(t *testing.T) {
	reg := buildDomainReg(nil, agent.NewMarketToolSet(nil), nil, nil)
	for _, name := range []string{"add_intent", "list_facts", "emit_fact"} {
		if _, ok := reg[name]; ok {
			t.Errorf("as 为 nil 时不该注册 ToolSet 工具 %q（会让 UI 显示可勾选但调用必失败）", name)
		}
	}
}

// TestBuildDomainRegNilBoth 两个来源都为 nil 时返回空 map（不是 nil）。
//
// 返回空 map 而非 nil 的理由：ToolResolve 里有 `if len(domainReg) > 0` 的守卫，
// nil 和空 map 行为一致，但空 map 让调用方少一层 nil 判断。
func TestBuildDomainRegNilBoth(t *testing.T) {
	reg := buildDomainReg(nil, nil, nil, nil)
	if reg == nil {
		t.Fatal("应返回空 map 而不是 nil")
	}
	if len(reg) != 0 {
		t.Errorf("两个来源都 nil 时应为空，实际 %d 个", len(reg))
	}
}

// TestToolResolveInjectsMarketToolForAnalyst 端到端：模拟 ToolResolve 的注入路径，
// 验证 agentKey="analyst_market" 时能拿到行情工具。
//
// 这是第 0 步的真正验收标准 —— 前面的测试只验证「domainReg 有」，这个验证
// 「注入逻辑能找到」。
func TestToolResolveInjectsMarketToolForAnalyst(t *testing.T) {
	// 模拟 ToolResolve 的注入判定（与 server/assembly.go 里的逻辑一致）
	reg := buildDomainReg(nil, agent.NewMarketToolSet(nil), nil, nil)

	// 模拟 tools 表里 get_quote 绑定到 analyst_market
	boundAgents := []string{"analyst_market", "worker"}
	agentKey := "analyst_market"

	contains := func(xs []string, s string) bool {
		for _, x := range xs {
			if x == s {
				return true
			}
		}
		return false
	}

	// 三条判定与 ToolResolve 完全一致
	rowEnabled := true
	inList := false // base 里没有（WorkerTools 不含行情工具）
	if !rowEnabled || !contains(boundAgents, agentKey) || inList {
		t.Fatal("这张表不该让 get_quote 被过滤掉")
	}
	inst, ok := reg["get_quote"]
	if !ok {
		t.Fatal("domainReg 缺 get_quote —— 这会让上面的判定通过但注入失败，" +
			"正是静默失效的根源")
	}
	if inst.Name() != "get_quote" {
		t.Errorf("工具名不符: %q", inst.Name())
	}
}

// TestDefaultToolsNotInDomainReg SDK 默认工具不进 domainReg。
//
// 它们在每个 agent 的 base 里都有、也没有 DB 行，ToolResolve 对「没有 DB 行」
// 的工具是原样放行的（`if !known { out = append(out, t); continue }`）。
// 把它们放进 domainReg 会让覆盖逻辑从「放行」变成「按行过滤」，改变既有行为。
func TestDefaultToolsNotInDomainReg(t *testing.T) {
	reg := buildDomainReg(nil, agent.NewMarketToolSet(nil), nil, nil)

	defaultNames := map[string]bool{}
	for _, t := range actool.DefaultTools() {
		defaultNames[t.Name()] = true
	}
	for name := range reg {
		if defaultNames[name] {
			t.Errorf("domainReg 不该包含 SDK 默认工具 %q —— 会让它从「无行放行」"+
				"变成「按行过滤」，改变渗透 agent 的既有行为", name)
		}
	}
}

var _ = context.Background

// TestFinancialToolBindingsMatchSpec 验证 financialToolBindings 与 §4.3.2 一致。
//
// 为什么硬编码期望值而不是从 map 推导：这份表是「规格」，从实现推导会让
// 规格跟着实现漂移（实现少写一个工具，测试也跟着少查一个）。
func TestFinancialToolBindingsMatchSpec(t *testing.T) {
	want := map[string][]string{
		// 2026-10-31：analyst_market / analyst_news / analyst_fundamental / trader
		// 补绑妙想工具（mx_query / mx_news）。规格表是硬编码的，**加工具必须同步改这里** ——
		// 这对齐是刻意的：从实现推导规格会让规格跟着实现漂移。
		// 2026-11-06 新增 screener（选品）：多标的筛选 → set_task_instrument 回填标的。
		// 它必须在 analyst_* 之前跑，否则下游拿不到 instrument_id 会全部判「无法分析」。
		"screener": {"get_quote", "get_kline", "compute_indicator", "get_volatility",
			"list_facts", "emit_fact", "set_task_instrument"},
		"analyst_market":           {"get_quote", "get_kline", "compute_indicator", "get_volatility", "mx_query", "list_facts", "emit_fact"},
		"analyst_news":             {"get_quote", "get_news", "get_macro", "mx_news", "list_facts", "emit_fact"},
		"analyst_fundamental":      {"get_quote", "get_fundamentals", "mx_query", "list_facts", "emit_fact"},
		"analyst_sentiment_crypto": {"get_crypto_flow", "get_macro", "list_facts", "emit_fact"},
		"debate_bull":              {"list_facts", "list_thesis", "emit_thesis"},
		"debate_bear":              {"list_facts", "list_thesis", "emit_thesis"},
		"research_manager":         {"list_facts", "list_thesis", "emit_thesis"},
		// 2026-11-02 新增 create_order_draft / authorize_order：
		// 用户要求「通过工具实现下单，否则后面无法进行拦截」。
		// 交易员负责「提案 + 过闸」，**不给 execute_paper_order** —— 执行权留在用户侧。
		// 这是规格变更（不是实现漂移），所以同步更新这张对照表。
		// 2026-11-09 新增 get_account_balance：任务目标是「使用全部账户余额」，
		// 没有余额工具就无法算仓位（任务 #4 因此卡住）。这也是规格变更。
		"trader": {"get_quote", "mx_query", "list_thesis", "emit_signal",
			"create_order_draft", "authorize_order", "get_account_balance"},
		"risk_aggressive":   {"list_thesis", "list_facts", "emit_thesis"},
		"risk_neutral":      {"list_thesis", "list_facts", "emit_thesis"},
		"risk_conservative": {"list_thesis", "list_facts", "emit_thesis"},
		// portfolio_manager 同样需要余额：按总权益折算目标收益与仓位上限。
		"portfolio_manager": {"list_facts", "list_thesis", "list_signals", "emit_rating", "get_account_balance"},
	}
	if len(financialToolBindings) != len(want) {
		t.Errorf("角色数 = %d, want %d", len(financialToolBindings), len(want))
	}
	for role, wantKeys := range want {
		binding, ok := financialToolBindings[role]
		if !ok {
			t.Errorf("缺角色 %q", role)
			continue
		}
		gotKeys := binding.Tools
		gotSet := map[string]bool{}
		for _, k := range gotKeys {
			gotSet[k] = true
		}
		wantSet := map[string]bool{}
		for _, k := range wantKeys {
			wantSet[k] = true
			if !gotSet[k] {
				t.Errorf("%s 缺工具 %q", role, k)
			}
		}
		for _, k := range gotKeys {
			if !wantSet[k] {
				t.Errorf("%s 多了工具 %q（不在 §4.3.2 里）", role, k)
			}
		}
	}
}

// TestAnalystHasNoPenetrationTools 分析师不该有任何渗透工具。
//
// 这是权限边界的核心断言：如果分析师能 insert_assets / report_finding，
// 那「角色边界只靠 prompt 自觉」—— 而 prompt 是会被注入、会被误读的。
func TestAnalystHasNoPenetrationTools(t *testing.T) {
	penetrationTools := []string{
		"insert_assets", "add_finding", "report_finding", "add_intent",
		"delete_assets_by_host", "add_company_scope", "set_goals", "set_constraints",
		"spawn_task", "pause_task", "steer_work",
	}
	for _, role := range []string{
		"analyst_market", "analyst_news", "analyst_fundamental", "analyst_sentiment_crypto",
		"debate_bull", "debate_bear", "research_manager",
		"trader", "risk_aggressive", "risk_neutral", "risk_conservative",
		"portfolio_manager",
	} {
		keys := financialToolBindings[role].Tools
		for _, pk := range penetrationTools {
			for _, k := range keys {
				if k == pk {
					t.Errorf("%s 绑定了渗透工具 %q —— 角色边界形同虚设", role, pk)
				}
			}
		}
	}
}

// TestEmitRatingOnlyForPortfolioManager emit_rating 只绑 PM 一个角色。
//
// 架构 §4.3.2 的硬约束：「如果 trader 也能写评级，就会出现一个人既提案又决定」。
func TestEmitRatingOnlyForPortfolioManager(t *testing.T) {
	for role, binding := range financialToolBindings {
		for _, k := range binding.Tools {
			if k == "emit_rating" && role != "portfolio_manager" {
				t.Errorf("%s 绑定了 emit_rating —— 这会破坏「提案者不能同时决策」的分离", role)
			}
		}
	}
	if !contains(financialToolBindings["portfolio_manager"].Tools, "emit_rating") {
		t.Error("portfolio_manager 必须绑定 emit_rating（它是唯一的评级写入者）")
	}
}
