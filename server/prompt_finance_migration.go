package server

// 把通用角色的提示词从「渗透测试平台」语境迁移到「金融交易平台」语境
// （2026-11-03）。
//
// # 为什么需要一次独立的迁移，而不是 bump 既有的四个 reseed flag
//
// 项目已有四个一次性迁移函数（reseedGoalsPrompt / reseedMainAgentPrompt /
// reseedPlannerPrompt / reseedWorkerPrompt，见 orchestration.go:531-660），
// 它们各自守护一个 settings flag，**当前四个 flag 全为 true**：
//
//	goals_prompt_constraint_step_v1      | true
//	mainagent_prompt_goalless_intent_v1  | true
//	planner_prompt_compact_realistic_v2  | true
//	worker_prompt_compact_v4             | true
//
// 那四个 flag 的语义是「某次具体的内容修订已应用过」，一条 flag 对应一次
// 明确的改动（加约束步 / 加反问引导 / 精简重构 / 收敛上下文段）。本次是
// **领域语境的整体改写**，与那四次修订不是同一类事件。若去 bump 它们的
// 版本号（v2→v3、v4→v5），会把「这次是语境迁移」这件事藏进别人的版本序列里，
// 下一个改这些函数的人看不出发生过什么。
//
// 所以：独立函数 + 独立 flag，一条 flag 表达一件事。
//
// # 为什么统一用 agent.BuiltinPromptSeeds()
//
// 提示词正文的唯一事实来源是代码常量（promptcatalog / planner / worker /
// goals / mainagent）。迁移只负责「把 DB 里那份旧快照刷成当前代码默认」，
// 不内嵌任何文案 —— 否则文案就有两份，改一处忘另一处。
//
// # 为什么安全（不会丢用户自定义）
//
// ResetPromptToDefault 走 SavePrompt：**追加一个新版本**并切换
// current_prompt_id，旧版本完整保留在 agent_prompts 历史里。
// 用户若曾自定义，可从版本记录找回（DB 里 worker 已有 165 个版本，
// 说明这套版本机制一直在用）。
//
// # 边界
//
// 只处理五个「通用角色」。金融分析角色（analyst_* / debate_* / risk_* /
// trader / portfolio_manager / research_manager）的提示词本来就是金融语境，
// 不在此列 —— 把它们一并重置反而会抹掉用户的调优。

import (
	"encoding/json"
	"log"

	"github.com/Autumn-27/artex/agent"
)

// financeContextPromptKeys 是本次需要金融化的通用角色。
//
// 顺序无关紧要（各自独立查询 + 独立失败不阻断其他），但按「从入口到执行」
// 的业务顺序排列，便于日志阅读。
var financeContextPromptKeys = []string{
	"mainagent", // 人类操作员接口
	"planner",   // 规划者（被频繁唤醒）
	"worker",    // 执行者（完成单条意图后停止）
	"goals",     // 目标分解器
	"auto",      // 平台操作助手
}

// reseedFinanceContextPrompts 把通用角色的提示词刷成当前代码默认（金融语境）。
//
// 一次性：flag 置 true 后不再执行。全新库无需处理 —— seedPrompts 首次
// 插入时直接用代码默认，不存在旧快照问题。
func (s *Server) reseedFinanceContextPrompts() {
	const flag = "prompts_finance_context_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	// 无论成败只尝试一次：中途失败时数据库可能已被部分更新，
	// 反复重试会让「哪几个角色已迁移」变得不可预测。
	// 失败时留 log，由人工按日志补做（ResetPromptToDefault 幂等：
	// 内容相同不会产生新版本，见下方 cur == tmpl 的短路）。
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }()

	seeds := agent.BuiltinPromptSeeds()
	migrated := 0
	for _, key := range financeContextPromptKeys {
		tmpl := seeds[key]
		if tmpl == "" {
			// 该 key 没有代码默认（例如 goals_finance 这种只在特定分叉用的），跳过。
			continue
		}
		a, err := s.m.pg.GetAgentByKey(key)
		if err != nil || a == nil {
			// agent 行不存在（全新库尚未建 / 已被删除）。跳过，不视为错误。
			continue
		}
		// 已经是代码默认 → 不产生重复版本。这条短路让本函数可安全重跑。
		if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
			continue
		}
		if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
			log.Printf("[prompts] %s 金融化重刷失败: %v", key, err)
			continue
		}
		migrated++
		log.Printf("[prompts] %s 提示词已切换为金融语境默认（旧版本保留在历史）", key)
	}
	if migrated > 0 {
		log.Printf("[prompts] 金融语境迁移完成：%d 个角色", migrated)
	}
}

// financeContextToolKeys 是需要刷新描述的工具。
//
// # 为什么工具描述也属于「提示词」
//
// 这些描述经 Registry.Schemas() 直接进 llm.ToolSchema.Description ——
// 模型看到的是一段带操作规则的自然语言，不是纯参数表。
// set_goals / set_constraints 的原文举的全是渗透例子
// （「仅测当前端口，不扫其他端口」「只允许被动侦察」），
// 这会把金融 agent 的规划往错误方向引。
//
// 与 prompt 一样，SeedTool 是 ON CONFLICT DO NOTHING —— 不改代码默认
// 且不刷新的话，DB 里永远是旧文案。
var financeContextToolKeys = []string{
	"set_goals",
	"set_constraints",
}

// reseedFinanceContextToolDescriptions 把上述工具描述刷成当前代码默认（金融语境）。
//
// 独立 flag：与 reseedFinanceContextPrompts 是同一类事件（领域语境迁移），
// 但二者作用对象不同（agent 提示词 vs 工具描述），分开记 flag 便于
// 只重跑其中一半。与 refreshBuiltinToolSchemas 那批「内容修订型」迁移
// （goal_met/insert_assets/list_facts）也刻意分开 —— 理由同 prompt 侧。
func (s *Server) reseedFinanceContextToolDescriptions() {
	const flag = "tool_desc_finance_context_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }()

	// 从 BuiltinToolSeeds 取代码默认 —— 唯一事实来源，本函数不内嵌文案。
	want := make(map[string]bool, len(financeContextToolKeys))
	for _, k := range financeContextToolKeys {
		want[k] = true
	}
	migrated := 0
	for _, sd := range agent.BuiltinToolSeeds() {
		if !want[sd.Key] {
			continue
		}
		schema, _ := json.Marshal(sd.Schema)
		if err := s.m.pg.RefreshToolDefaults(sd.Key, sd.Desc, schema); err != nil {
			log.Printf("[tools] %s 描述金融化失败: %v", sd.Key, err)
			continue
		}
		migrated++
	}
	if migrated > 0 {
		log.Printf("[tools] 金融语境迁移：%d 个工具描述已刷新", migrated)
	}
}

// reseedScreenerPrompt 把选品员的提示词刷成当前代码默认。
//
// # 为什么需要单独一次迁移（2026-11-11）
//
// screener 的 prompt 原来写着「你的意图摘要见下方 user 消息的第一段」——
// 那段**不存在**：role prompt 路径下 user 消息是占位句（worker.go:633），
// 而阶段图建的 screener 节点 payload 里也没有 summary。
// 于是选品员既看不到策略、也看不到任务目标，只能按默认条件在美股里凑候选
// （任务 15：goal 明写 BTC-USDT，却选了 NVDA）。
//
// 新版把任务目标渲染进模板（ScreenerVars.TaskGoal），并加了一条
// 「目标里已写明标的代码就直接绑定、不要另选」的规则。
//
// 存量库里的 screener prompt 是旧快照（seedPrompts 首插入 only），
// 不刷就永远看不到新内容。所以用独立 flag 做一次性迁移。
func (s *Server) reseedScreenerPrompt() {
	const flag = "screener_prompt_task_goal_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	// 无论成败只尝试一次（与同文件另外两个迁移一致：避免「部分已迁移」的不可预测状态）
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }()

	tmpl := agent.BuiltinPromptSeeds()[agent.RoleScreener]
	if tmpl == "" {
		log.Printf("[prompts] screener 无代码默认，跳过迁移")
		return
	}
	a, err := s.m.pg.GetAgentByKey(agent.RoleScreener)
	if err != nil || a == nil {
		// 全新库尚未建该 agent 行 —— seedPrompts 会用最新默认，无需迁移。
		return
	}
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return // 已是默认，不产生重复版本
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] screener 重刷失败: %v", err)
		return
	}
	log.Printf("[prompts] screener 提示词已切换到含任务目标的版本（旧版本保留在历史）")
}
