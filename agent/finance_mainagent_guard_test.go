package agent

// 主 agent 在金融任务里也必须拿不到 Bash（2026-11-14 事故）。
//
// # 事故经过
//
// 之前的限制只覆盖 planner（assembly.go 的 financialPlanner）。主 agent
// （用户对话界面）在同一个金融任务上仍拿完整 SDK 默认工具集
// （Bash / Write / Edit / Monitor）+ permission.ModeBypass，
// 而容器里有 /app/bin/okx。实测任务 20 的 activity：
//
//	mainagent | Bash | okx swap leverage --instId AAPL-USDT-SWAP --lever 10 --mgnMode cross
//	mainagent | Bash | okx swap place --instId AAPL-USDT-SWAP --side buy --ordType market --sz 0.6 …
//	mainagent | Bash | okx swap place --instId TSLA-USDT-SWAP --side sell …
//	mainagent | Monitor | while true; do okx swap positions --json; done
//
// 后果：整条交易链被绕过 —— 没有草案、没有 draft_risk_verdicts、
// 没有 orders_execution、没有 TP/SL 巡检、没有幂等键、没有 ValidateBracket。
// 「Bash 能下单」与「所有交易护栏」是互斥的，只能留一个。
//
// # 为什么用白名单
//
// 黑名单永远漏：这次漏了 Monitor，下次会漏 MCP 工具、漏 skill。
// 白名单的失效方向是「工具变少」（功能受限），黑名单的失效方向是
// 「多出一个能下单的入口」（资金损失）。

import (
	"context"
	"encoding/json"
	"testing"

	acperm "github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// 金融任务下 mainagent 的白名单不得包含任何可执行外部命令的工具。
func TestFinancialMainagentWhitelistExcludesShell(t *testing.T) {
	allowed := allowedToolsForFinancialAgent("mainagent", nil)

	// 这些名字任何一个出现在白名单里，就等于主 agent 能直接下单。
	forbidden := []string{
		"Bash", "Monitor", "Write", "Edit", "MultiEdit",
		"create_order_draft", "authorize_order", "execute_paper_order",
	}
	for _, n := range forbidden {
		if allowed[n] {
			t.Errorf("金融任务下 mainagent 不该有 %q —— 它能绕过交易链直接下单", n)
		}
	}

	// 只读与任务编排工具必须保留，否则主 agent 在该任务上完全没法用。
	for _, n := range []string{"Read", "LS", "Grep", "Glob", "TodoWrite", "graph_overview"} {
		if !allowed[n] {
			t.Errorf("金融任务下 mainagent 应保留 %q", n)
		}
	}
}

// planner 的白名单同样不该出现 shell（原有约束，防止回归）。
func TestFinancialPlannerWhitelistExcludesShell(t *testing.T) {
	allowed := allowedToolsForFinancialAgent("planner", nil)
	for _, n := range []string{"Bash", "Monitor", "Write", "Edit", "MultiEdit"} {
		if allowed[n] {
			t.Errorf("金融任务下 planner 不该有 %q", n)
		}
	}
	if !allowed["add_intent"] || !allowed["prove_goal"] {
		t.Error("planner 应保留 add_intent / prove_goal")
	}
}

// 非金融 agent 不受白名单约束（nil 表示不限制）。
func TestAllowedToolsNilForNonFinancialAgent(t *testing.T) {
	if got := financialAgentRestricted(context.Background(), "worker"); got {
		t.Error("worker 不应被 financialAgentRestricted 限制（它有自己的 financeTask 判定）")
	}
	if got := financialAgentRestricted(context.Background(), "trader"); got {
		t.Error("trader 走工具绑定而非白名单")
	}
}

// 核心断言：AugmentTools 真的把 Bash 从主 agent 的工具集里摘掉了。
//
// 不依赖 DB：ctx 里没有 ExplorationStore 时 financialAgentRestricted 走
// TaskID==0 → explorationIsFinancial(nil) → false，所以这里显式构造一个
// 「任务上下文不可验证」的 ctx，让判定 fail-closed 为 true。
func TestAugmentToolsStripsBashFromFinancialMainagent(t *testing.T) {
	ctx := WithRunInfo(context.Background(), RunInfo{TaskID: 7})

	base := []actool.CoreTool{
		stubTool("Bash"), stubTool("Monitor"), stubTool("Write"), stubTool("Edit"),
		stubTool("MultiEdit"), stubTool("Read"), stubTool("LS"), stubTool("Grep"),
		stubTool("create_order_draft"),
	}
	out, _, cleanup := AugmentTools(ctx, "mainagent", base)
	defer cleanup()

	names := make(map[string]bool, len(out))
	for _, tl := range out {
		names[tl.Name()] = true
	}
	for _, n := range []string{"Bash", "Monitor", "Write", "Edit", "MultiEdit", "create_order_draft"} {
		if names[n] {
			t.Errorf("AugmentTools 后主 agent 仍持有 %q", n)
		}
	}
	if !names["Read"] {
		t.Error("Read 应保留（读报告不算取数）")
	}
}

// stubTool 是最小 CoreTool：只提供名字，Run 永不被调用。
func stubTool(name string) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name:        name,
		Description: "stub " + name,
		Schema:      map[string]any{"type": "object"},
		Permissions: func(context.Context, json.RawMessage, acperm.Context) acperm.Decision {
			return acperm.Allowed()
		},
		Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
			return actool.Text("stub"), nil
		},
	})
}

// 主 agent 必须能拿到下单三件套。
//
// # 为什么这条要单独钉
//
// 白名单是「只增不减」的反面：漏一个工具不会报错，只会让模型在对话里
// 说「我没有这个工具」—— 实测 2026-11-14 就是这样，tools 表已绑
// mainagent，但白名单没放行，模型只能空转。
func TestMainagentWhitelistIncludesTradingTools(t *testing.T) {
	allowed := allowedToolsForFinancialAgent("mainagent", nil)
	for _, want := range []string{
		"open_position", "close_position", "set_leverage",
		"get_positions", "get_task_trades", "modify_bracket",
	} {
		if !allowed[want] {
			t.Errorf("主 agent 白名单缺 %q —— tools 表绑了也拿不到", want)
		}
	}
	// 仍然不给草案三件套：增敞口走 open_position，不走草案
	for _, deny := range []string{"create_order_draft", "authorize_order", "execute_paper_order"} {
		if allowed[deny] {
			t.Errorf("主 agent 不该有 %q", deny)
		}
	}
}
