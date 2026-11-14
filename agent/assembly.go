package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"runtime/debug"
	"strings"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// DeferredInfo carries the deferred-tools wiring an agent needs to build its
// Options: the MCP tool names whose schemas are withheld, the subset listed in the
// global system-prompt block (non-skill-gated), and the shared session unlock set.
// UnlockSkill unlocks a named skill's MCPs — hosts call it to rebuild the unlock set
// from history on a resumed session (design doc C2).
type DeferredInfo struct {
	FindingGuidance string            // derived from the final permitted tools, including DB overrides
	Deferred        []string          // all MCP tool names (schema withheld)
	GlobalNames     []string          // MCP names to list in the system-prompt block
	Unlock          *actool.UnlockSet // shared call-gate; nil when no MCP tools
	UnlockSkill     func(skillName string)
}

// ToolAugment, if set, returns the EXTRA tools an agent should see beyond its
// built-in base set — the agent's visible skills (packed into one Skill meta-tool)
// and visible MCP servers (expanded to mcp__server__tool). It also returns the
// DeferredInfo describing how those MCP tools are deferred/gated. The server wires
// it to the PG agent_visibility table. cleanup releases any spawned MCP clients.
//
// When nil, agents run with only their built-in tools — behavior is unchanged
// until a user assigns a skill/MCP to the agent in the UI.
var ToolAugment func(ctx context.Context, agentKey string) (extra []actool.CoreTool, def DeferredInfo, cleanup func())

// plannerIsFinancial 优先使用任务绑定；任务上下文不可验证时收紧权限。
func plannerIsFinancial(ctx context.Context) bool {
	ts, _ := ExplorationStoreFrom(ctx)
	ri := RunInfoFrom(ctx)
	if ri.TaskID == 0 {
		return explorationIsFinancial(ts)
	}
	if ts == nil || ts.DB() == nil {
		return true
	}
	task, err := ts.DB().GetTask(ri.TaskID)
	if err != nil || task == nil || task.ExplorationID != ts.ID() || (ri.ExplorationID != 0 && ri.ExplorationID != ts.ID()) {
		return true
	}
	if task.InstrumentID != nil {
		return true
	}
	// 未绑定标的的选品任务仍按阶段图识别；读取失败不得扩大权限。
	nodes, err := ts.ListByKind(db.KindIntent, 50)
	if err != nil {
		return true
	}
	for _, n := range nodes {
		var p struct {
			AssetClass string `json:"asset_class"`
		}
		if json.Unmarshal(n.Payload, &p) == nil && strings.TrimSpace(p.AssetClass) != "" {
			return true
		}
	}
	return false
}

// financialAgentRestricted 判断这个 agent 在**本任务**里是否必须被限制到白名单。
//
// # 为什么要覆盖 mainagent（2026-11-14 实测）
//
// 原来只有 planner 受限。主 agent（用户对话界面）在同一个金融任务上仍然拿着
// 完整 SDK 默认工具集（Bash/Write/Edit/Monitor）+ permission.ModeBypass，
// 而容器里有 /app/bin/okx。实测任务 20 的 activity：
//
//	mainagent | Bash | okx swap leverage --instId AAPL-USDT-SWAP --lever 10 --mgnMode cross
//	mainagent | Bash | okx swap place --instId AAPL-USDT-SWAP --side buy --ordType market --sz 0.6 …
//	mainagent | Bash | okx swap place --instId TSLA-USDT-SWAP --side sell …
//	mainagent | Monitor | while true; do okx swap positions --json; done
//
// 也就是说「分析→草案→闸门→执行」这条链被整条绕过：没有草案、没有
// draft_risk_verdicts、没有 orders_execution、没有 TP/SL 巡检、没有幂等键。
// 这正是「Bash 能下单」与「我们做的所有交易护栏」互斥的证明。
//
// 判据与 planner 同源（任务绑定标的，或图里带 asset_class），读不到时收紧。
func financialAgentRestricted(ctx context.Context, agentKey string) bool {
	if agentKey != "planner" && agentKey != "mainagent" {
		return false
	}
	ts, _ := ExplorationStoreFrom(ctx)
	ri := RunInfoFrom(ctx)
	if ri.TaskID == 0 {
		return explorationIsFinancial(ts)
	}
	if ts == nil || ts.DB() == nil {
		return true // fail-closed：验证不了就收紧
	}
	task, err := ts.DB().GetTask(ri.TaskID)
	if err != nil || task == nil || task.ExplorationID != ts.ID() ||
		(ri.ExplorationID != 0 && ri.ExplorationID != ts.ID()) {
		return true
	}
	if task.InstrumentID != nil {
		return true
	}
	// 未绑定标的的选品任务仍按阶段图识别；读取失败不得扩大权限。
	return explorationIsFinancial(ts)
}

// allowedToolsForFinancialAgent 返回金融任务下该 agent 允许出现的工具名集合。
//
// 用白名单而不是黑名单：黑名单永远漏（今天漏了 Monitor，明天漏 MCP 工具）。
// 白名单的失效方向是「工具变少」，那只是功能受限；黑名单的失效方向是
// 「多出一个能下单的入口」，那是资金损失。
func allowedToolsForFinancialAgent(agentKey string, ts *db.ExplorationStore) map[string]bool {
	allowed := map[string]bool{"TodoWrite": true}
	switch agentKey {
	case "planner":
		for _, t := range NewToolSet(ts, "planner").PlannerTools() {
			allowed[t.Name()] = true
		}
	case "mainagent":
		for _, t := range NewToolSet(ts, "mainagent").MainAgentTools() {
			allowed[t.Name()] = true
		}
		// 只读文本工具：读报告 / 笔记 / 日志，不取数也不下单。
		for _, n := range []string{"Read", "LS", "Grep", "Glob"} {
			allowed[n] = true
		}
		// 金融只读工具 + 「改保护」。
		//
		// # 为什么这些必须放行（2026-11-14 修）
		//
		// MainAgentTools() 里没有任何行情/持仓工具 —— 它们由 domainReg 装配、
		// 经 ToolResolve 注入。只按 MainAgentTools 建白名单会把它们全部过滤掉，
		// 于是主 agent 在金融任务里看不到当前价、也看不到自己有没有仓，
		// 只能空谈。这属于「白名单粒度太粗导致的误伤」，不是安全收紧。
		//
		// # 为什么 modify_bracket 也算
		//
		// 它是「改保护」，不增敞口、不改方向、不建新仓，且有方向校验
		// （多单 sl 必须低于现价）+ 审计表记录。而人在对话里最常说的
		// 就是「把止损挪到成本价」，不给这个工具等于让主 agent 只能转述。
		//
		// 仍然**不给** create_order_draft / authorize_order：
		// 增敞口的决定权留在 trader，那条链有草案 + 闸门 + 幂等键。
		for _, n := range []string{
			"get_quote", "get_kline", "compute_indicator", "get_volatility",
			"get_macro", "get_crypto_flow", "mx_query", "mx_news", "get_fundamentals",
			"get_news", "get_account_balance",
			"get_positions", "get_task_trades", "modify_bracket",
			// 下单三件套也给主 agent：那是「人对 agent 喊单」的入口，
			// 护栏在工具内部（必须带 TP/SL、方向校验、禁同向重复开仓），
			// 不依赖「只给 trader」来保证安全。
			"open_position", "close_position", "set_leverage",
			"list_facts", "list_thesis", "list_signals",
		} {
			allowed[n] = true
		}
		for _, n := range SourceQuoteToolNames {
			allowed[n] = true
		}
	}
	return allowed
}

// AugmentTools returns base plus the agent's visible skill/MCP tools, the
// DeferredInfo, and a cleanup func the caller must defer (closes MCP clients).
// Built-in base tools are kept as-is — never filtered (内置工具留代码层，不做可见性过滤).
func AugmentTools(ctx context.Context, agentKey string, base []actool.CoreTool) ([]actool.CoreTool, DeferredInfo, func()) {
	var (
		def     DeferredInfo
		cleanup = func() {}
		out     = base
	)
	// 金融任务下 planner 与 mainagent 都只能走白名单。
	//
	// 任意 host 工具（Bash / MCP / skill）都能绕过交易授权直接下单 ——
	// 主 agent 在任务 20 里正是用 `okx swap place` 绕过整条链开仓的。
	// 详见 financialAgentRestricted 的注释。
	restricted := financialAgentRestricted(ctx, agentKey)
	ts, _ := ExplorationStoreFrom(ctx)
	var scoped map[string]bool
	if restricted {
		scoped = allowedToolsForFinancialAgent(agentKey, ts)
	}
	if ToolAugment != nil && !restricted {
		var extra []actool.CoreTool
		var cl func()
		extra, def, cl = ToolAugment(ctx, agentKey)
		if cl != nil {
			cleanup = cl
		}
		if len(extra) > 0 {
			out = append(append([]actool.CoreTool{}, base...), extra...)
		}
	}
	// DB tools table has the final say on the built-in tools: drop the ones this
	// agent isn't bound to (or that are disabled) and swap in overridden
	// descriptions/schemas + default injection. MCP/skill/host tools have no row
	// and pass through untouched, so deferred/unlock wiring stays consistent.
	if ToolResolve != nil {
		out = ToolResolve(ctx, agentKey, out)
	}
	if restricted {
		// 保留解析器的绑定、元信息与计量包装，仅过滤允许的工具名。
		filtered := make([]actool.CoreTool, 0, len(out))
		for _, t := range out {
			if scoped[t.Name()] {
				filtered = append(filtered, t)
			}
		}
		// deferred MCP 工具一律关闭：schema 被隐藏，一旦解锁就能执行任意宿主能力。
		out, def = filtered, DeferredInfo{}
	}
	out, def.FindingGuidance = findingWorkflowTools(agentKey, out)
	for i, t := range out {
		out[i] = guardPanic(t)
	}
	return out, def, cleanup
}

// guardPanic turns a panicking tool handler into an ordinary tool error. The
// harness runs each tool on its own goroutine, so a panic inside a handler can't
// be recovered by the caller that started the run — it takes the whole process
// down, and on restart the agent replays the same call and crashes again. Applied
// last, so it covers every tool the agent can reach: domain, SDK, MCP and skill.
func guardPanic(t actool.CoreTool) actool.CoreTool { return &guardedTool{CoreTool: t} }

type guardedTool struct{ actool.CoreTool }

func (g *guardedTool) Call(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (res actool.Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[tools] %s panic: %v\n%s", g.Name(), r, debug.Stack())
			res, err = actool.Errorf(fmt.Sprintf("工具 %s 内部错误：%v（本次调用已失败，可换个参数或改用别的工具）", g.Name(), r)), nil
		}
	}()
	return g.CoreTool.Call(ctx, in, tc)
}
