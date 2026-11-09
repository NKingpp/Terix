package server

// 第 3 步验收：ToolResolve 按 role 过滤工具的效果。
//
// # 为什么这是第 3 步的唯一验收标准
//
// 第 0 步修了两层（domainReg 有实例 / tools.agents 有绑定），但那只保证
// 「数据在库里对」。真正能不能到 agent 手上，取决于 ToolResolve 的过滤逻辑
// 在 role 被传进去时是否给出期望结果。
//
// 如果这个测试不过，说明还有第三层缺口 —— 而它的表现是静默的：
// 工具在 UI 里能勾选、DB 里绑定正确，但 agent 拿不到。

import (
	"context"
	"testing"

	actool "github.com/Autumn-27/norma/tool"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// setupToolResolve 建一个带 ToolResolve 钩子的测试环境。
//
// 直接调 wireTools 而不是构造完整 Server：ToolResolve 是包级钩子，
// 它只需要一个 *db.DB 和 domainReg。构造完整 Server 会拉起 engine/scheduler
// 等无关组件，让测试变慢且易碎。
func setupToolResolve(t *testing.T) *db.DB {
	t.Helper()
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { m.Close() })
	reg := buildDomainReg(m.Assets(), agent.NewMarketToolSet(nil), nil, nil)
	wireTools(m.pg, reg)
	t.Cleanup(func() { agent.ToolResolve = nil })
	return m.pg
}

// augmentNames 调 AugmentTools 并返回工具名集合（吃掉 def/cleanup 两个返回值）。
func augmentNames(ctx context.Context, agentKey string, base []actool.CoreTool) map[string]bool {
	tools, _, cleanup := agent.AugmentTools(ctx, agentKey, base)
	defer cleanup()
	return toolNames(tools)
}

// toolNames 提取工具名集合。
func toolNames(tools []actool.CoreTool) map[string]bool {
	m := make(map[string]bool, len(tools))
	for _, t := range tools {
		m[t.Name()] = true
	}
	return m
}

// TestFinancialAnalystToolsExcludePenetration 分析师拿金融工具、不拿渗透工具。
//
// 这是第 3 步的核心断言：工具面必须与 prompt 一致。
// 工具多 prompt 少 → LLM 会尝试调没被指示的工具（如 insert_assets 污染数据）。
func TestFinancialAnalystToolsExcludePenetration(t *testing.T) {
	// 构造一个真实的 ToolResolve 环境（DB 里已有绑定）
	setupToolResolve(t)

	// base 用 WorkerTools（模拟 worker 的初始工具集，含渗透工具但不含行情工具）
	ts := agent.NewToolSet(nil, "")
	base := ts.WorkerTools()

	ctx := context.Background()
	got := augmentNames(ctx, "analyst_market", base)

	// 断言 1：含金融工具（这些来自 domainReg 注入）
	for _, want := range []string{"get_quote", "get_kline", "compute_indicator", "list_facts", "emit_fact"} {
		if !got[want] {
			t.Errorf("analyst_market 缺 %q —— 金融工具没注入进来", want)
		}
	}
	// 断言 2：不含渗透工具
	for _, deny := range []string{"insert_assets", "add_finding", "report_finding", "add_intent"} {
		if got[deny] {
			t.Errorf("analyst_market 拿到了渗透工具 %q —— 工具面与 prompt 不一致，"+
				"LLM 会尝试调用它", deny)
		}
	}
}

// TestPentestWorkerKeepsItsTools 渗透 worker 的工具集与改动前一致。
func TestPentestWorkerKeepsItsTools(t *testing.T) {
	setupToolResolve(t)

	ts := agent.NewToolSet(nil, "")
	base := ts.WorkerTools()

	ctx := context.Background()
	got := augmentNames(ctx, "worker", base)

	// 渗透 worker 必须保留它原有的域工具。
	//
	// 注意清单里刻意**不含** list_facts —— 它只绑 mainagent/planner（跨 work 复盘
	// 是规划职责，worker 只做单条意图执行），这是既有设计不是回归。
	// 也刻意不含 add_finding —— tools 表里没有这个 key（只有 report_finding）。
	for _, want := range []string{
		"insert_assets", "report_finding", "list_assets",
		"emit_fact", "node_detail",
	} {
		if !got[want] {
			t.Errorf("worker 丢了 %q —— 渗透路径回归", want)
		}
	}
}

// TestDebateRoleToolSubset 辩论角色的工具子集与 §4.3.2 一致。
func TestDebateRoleToolSubset(t *testing.T) {
	setupToolResolve(t)

	ts := agent.NewToolSet(nil, "")
	base := ts.WorkerTools()
	ctx := context.Background()

	got := augmentNames(ctx, "debate_bull", base)

	for _, want := range []string{"list_facts", "list_thesis", "emit_thesis"} {
		if !got[want] {
			t.Errorf("debate_bull 缺 %q", want)
		}
	}
	// 辩论不该有行情工具（它不自己取数，只读分析师产出）
	for _, deny := range []string{"get_quote", "get_kline", "insert_assets"} {
		if got[deny] {
			t.Errorf("debate_bull 不该有 %q", deny)
		}
	}
}

// TestPortfolioManagerHasRatingTool PM 是唯一能写评级的角色。
func TestPortfolioManagerHasRatingTool(t *testing.T) {
	setupToolResolve(t)

	ts := agent.NewToolSet(nil, "")
	base := ts.WorkerTools()
	ctx := context.Background()

	got := augmentNames(ctx, "portfolio_manager", base)
	if !got["emit_rating"] {
		t.Error("portfolio_manager 必须有 emit_rating（它是唯一的评级写入者）")
	}
	if got["emit_signal"] {
		t.Error("portfolio_manager 不该有 emit_signal（那是 trader 的职责）")
	}
}
