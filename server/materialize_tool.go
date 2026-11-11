package server

// materialize_downstream 工具的 seed（把工具行写进 tools 表并绑给 planner）。
//
// # 为什么必须 seed（而不是只写 agent 侧的工具实现）
//
// `wireTools` 的 `ToolResolve` 按 **tools 表**过滤工具列表：没有对应的行，
// 工具就被当作「非内置工具」丢弃 —— 模型永远看不到它。
//
// 这与 `SeedSetInstrumentTool` 完全同构，保持同一套模式便于排查。
//
// # 为什么绑 planner 而不是别人
//
// 这个工具解决的是「规划阶段的图不完整」：planner 读到 graph_overview 的
// stage_graph_missing 后自己决定补建。worker 在跑单条意图，它既看不到全图，
// 也没有权力改图结构。

import (
	"encoding/json"
	"log"

	"github.com/Autumn-27/artex/agent"
)

// SeedMaterializeDownstreamTool 把 materialize_downstream 写进 tools 表并绑给 planner。
//
// flag 守卫：一次性迁移。种子只做「插入 + 补绑」，不覆盖用户在 UI 上改过的
// 描述或绑定（与 SeedSetInstrumentTool / seedTradeToolBindings 同一模式）。
func (s *Server) SeedMaterializeDownstreamTool() {
	if s.m == nil || s.m.pg == nil {
		return
	}
	const flag = "materialize_downstream_tool_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}

	// ① 写工具行。nil 依赖足够 —— 只需要 Name/Description/Schema。
	t := agent.NewToolSet(nil, "planner")
	tl := t.MaterializeDownstreamTool()
	schema, _ := json.Marshal(tl.InputSchema())
	// agents 留空：实际绑定由下一步完成。写死会让「用户后来解绑」被下次 seed 覆盖。
	if err := s.m.pg.SeedTool(tl.Name(), tl.Description(), schema, nil); err != nil {
		log.Printf("[pipeline] seed %s 失败: %v", tl.Name(), err)
		return // 不设 flag，下次启动重试
	}

	// ② 只给 planner。
	if err := s.m.pg.AddAgentToToolBinding("planner",
		[]string{tl.Name()}); err != nil {
		log.Printf("[pipeline] planner 工具绑定失败: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}
