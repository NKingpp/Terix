package agent

// 本文件把内置 agent 的「默认提示词正文」(段 [A]) 变成可枚举、可被服务端幂等
// 播种进 agent_prompts 表的目录 —— 镜像 toolcatalog.go 的 BuiltinToolSeeds()。
//
// # raw string 与反引号（写新 prompt 前必读）
//
// 本文件与 prompt_*.go 里的模板都是 Go raw string（反引号包裹），
// **raw string 内部不能出现反引号** —— 一旦出现会提前终止字符串，导致语法错误
// （典型症状：`syntax error: unexpected name xxx`）。
//
// markdown 里常需要反引号（代码块 ``` / 行内 `标识符`）。处理方式**统一用拼接**：
//
//	const tmpl = `...正文...
//	` + "```json" + `
//	{{.Data}}
//	` + "```" + `
//	...继续正文...`
//
// **不要为了避开反引号而修改 prompt 正文**（如把 `review` 改成 review）：
// 那会改变 markdown 语义 —— LLM 对 ` `review` ` 的理解是「枚举值/标识符」，
// 对 review 的理解是「普通词」。拼接是机械动作（照抄原文），改正文是判断动作
// （每次都要决定这个反引号是语法还是内容），后者会出错。
//
// 只包含【可编辑正文】：段 [B] trafficTool 与段 [C] 中间产物输出规约 是代码固定
// 注入(见 worker.go 的 workerTrafficBlock/artifactSpec)，不入库、不可编辑，因此
// 不在种子里。种子文本用 Go 模板占位({{.Goal}} 等)，渲染时按运行期变量填充。

// autoDefaultTmpl is the built-in "Auto" platform-operator agent's prompt. Auto
// runs via the chat page and drives the platform through tools: task ops
// (spawn/list/pause/hint + read graph/traces) and platform management
// (create/modify skill, custom tool, MCP). It seeds into agent_prompts like the
// other built-ins.
//
// # 为什么措辞是「金融交易平台」而不是「渗透测试平台」
//
// 2026-10-30 改：项目已从渗透测试改造为金融交易平台 Terix。这个 prompt 原先
// 说「你不亲自渗透」+ 引导 agent 读「漏洞(含 flag)」—— 而金融任务没有漏洞、
// 没有 flag。留着会让 Auto 在用户让它「看看任务进展」时去找漏洞字段。
//
// 同时删掉了对 `list_task_findings` 的指引：**该工具在代码里根本没有实现**
// （只在 reporter 的工具绑定清单里出现过一次字符串）。prompt 教 agent 调一个
// 不存在的工具，它会反复尝试然后在对话里报「工具不可用」。
const autoDefaultTmpl = `你是 **Auto**，这个金融交易平台的「操作助手」。你不亲自做投资分析，而是**用工具操作平台**、按用户指令把事情办好。

你能做的（取决于给你开放了哪些工具）：
1. **任务操作**：list_tasks 看全局、spawn_task 起子任务（一条投研任务）、get_task_graph 读某任务的进展、get_task_worker_trace 看某个分析步骤的执行过程、pause_task 暂停、add_task_hint 给任务注入提示。
2. **平台管理**：create_skill / update_skill 建改技能；create_custom_tool / update_custom_tool 建改自定义工具(command/script/http)；create_mcp / update_mcp 建改 MCP 服务器。

原则：
- 先看清现状(list_tasks / get_task_graph 等)再动手；一步到位、少空转。
- 建/改 skill、工具、MCP 时，把用户意图翻译成正确的结构化参数(kind/exec/schema 等)，字段拿不准就按最小可用填。
- 用人话简洁汇报你做了什么、结果如何；只根据工具真实返回作答，不臆造。
- 只在授权范围内操作。`

// DefaultAssistantPrompt is the starter/fallback body for CUSTOM conversational
// agents — they have no per-key in-code default. It is seeded into agent_prompts
// when a custom agent is created (so the editor isn't blank) and used as the
// render fallback in RunChat when the DB prompt is somehow missing.
const DefaultAssistantPrompt = `你是一个乐于助人的 AI 助手。请用简洁、准确的中文回答用户的问题；在需要时使用可用的工具来完成任务。只做用户要求的事，不臆造信息。`

// BuiltinPromptSeeds returns each built-in agent's default EDITABLE prompt body
// keyed by agent key. The server seeds these into agent_prompts on startup (only
// when an agent has no prompt yet), so the DB becomes the authoritative, editable
// source while the same string stays as the in-code render fallback.
func BuiltinPromptSeeds() map[string]string {
	return map[string]string{
		// 两套目标分解器正文，按任务的 instrumentID 分叉（见 goals.go 的 goalsBodyFor）。
		//
		// # 为什么是两个独立 key 而不是一个 key 两套正文
		//
		// renderSystem 先查 PromptOverride(agentKey) —— 用户可能改过 "goals"。
		// 如果金融任务共用 "goals"，那份旧的渗透措辞覆盖会压过 goalsFinanceTmpl，
		// 让双模式对已部署环境失效。
		"goals":         goalsDefaultTmpl,
		"goals_finance": goalsFinanceTmpl,
		"planner":       plannerDefaultTmpl,
		"mainagent":     mainAgentDefaultTmpl,
		"worker":        workerDefaultTmpl,
		"auto":          autoDefaultTmpl,

		// W5 金融分析角色。key 与 agents.key 一致（见 db/builtinFinancialAgents）。
		//
		// analyst_news / analyst_fundamental 于 2026-10-30 解禁：取数工具
		// get_news / get_fundamentals 已实现（见 tools_news.go），prompt 与
		// 工具绑定三处对齐（promptcatalog / db.builtinFinancialAgents /
		// server.financialToolBindings）。
		//
		// analyst_sentiment（股票版）仍不注册：无可靠社交情绪源
		// （Reddit/StockTwits 需付费 API，P0 不做）。
		// screener 只在「任务未绑标的」时出现在阶段图里（见 pipeline.go 的阶段 0）。
		// 它排在最前，因为它是唯一能把「策略」变成「具体标的」的角色。
		"screener":                 screenerTmpl,
		"analyst_market":           analystMarketTmpl,
		"analyst_news":             analystNewsTmpl,
		"analyst_fundamental":      analystFundamentalTmpl,
		"analyst_sentiment_crypto": analystSentimentCryptoTmpl,
		"debate_bull":              debateBullTmpl,
		"debate_bear":              debateBearTmpl,

		"research_manager":  researchManagerTmpl,
		"trader":            traderTmpl,
		"risk_aggressive":   riskAggressiveTmpl,
		"risk_neutral":      riskNeutralTmpl,
		"risk_conservative": riskConservativeTmpl,
		"portfolio_manager": portfolioManagerTmpl,
	}
}
