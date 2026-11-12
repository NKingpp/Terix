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

// AugmentTools returns base plus the agent's visible skill/MCP tools, the
// DeferredInfo, and a cleanup func the caller must defer (closes MCP clients).
// Built-in base tools are kept as-is — never filtered (内置工具留代码层，不做可见性过滤).
func AugmentTools(ctx context.Context, agentKey string, base []actool.CoreTool) ([]actool.CoreTool, DeferredInfo, func()) {
	var (
		def     DeferredInfo
		cleanup = func() {}
		out     = base
	)
	// Financial planners orchestrate through task tools only. Do not connect
	// skills/MCPs: arbitrary host tools can bypass trading authorization.
	ts, _ := ExplorationStoreFrom(ctx)
	financialPlanner := agentKey == "planner" && plannerIsFinancial(ctx)
	var scoped map[string]actool.CoreTool
	if financialPlanner {
		allowed := make(map[string]bool)
		for _, t := range NewToolSet(ts, "planner").PlannerTools() {
			allowed[t.Name()] = true
		}
		allowed["TodoWrite"] = true
		scoped = make(map[string]actool.CoreTool)
		for _, t := range base {
			if allowed[t.Name()] {
				scoped[t.Name()] = t
			}
		}
	}
	if ToolAugment != nil && !financialPlanner {
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
	if financialPlanner {
		// 保留解析器的绑定、元信息与计量包装，仅过滤允许的工具名。
		filtered := make([]actool.CoreTool, 0, len(out))
		for _, t := range out {
			if _, ok := scoped[t.Name()]; ok {
				filtered = append(filtered, t)
				delete(scoped, t.Name())
			}
		}
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
