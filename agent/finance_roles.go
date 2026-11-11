package agent

// planner 可以指派的执行角色白名单。
//
// # 为什么需要这个文件（2026-11-11）
//
// 在此之前 `add_intent` 只有 summary/asset_ids/parent_ids/priority 四个参数，
// planner 根本无法指定 role —— 而 role 是 `agentKeyForIntent` 决定用哪套 prompt
// 与哪个工具子集的唯一依据。于是 planner 追加的意图全部落在通用 worker 上：
//
//	#705 <EMPTY>  基于账户权益筛选可执行标的
//	#770 <EMPTY>  trader 执行目标662：提交限价空单   ← 该是 trader 却给了通用 worker
//
// 建图时创建的那 14 个节点有 role（materialize.go 写入），所以「角色链路」本身
// 是通的 —— 缺的是**运行中补派**的能力。
//
// # 合法 role 的唯一事实来源是 BuiltinPromptSeeds()
//
// 「合法」= `renderRolePrompt` 能成功 = 有模板 + `buildRoleVars` 认得。
// 没模板时 `renderRolePrompt` 返回 false，节点静默退回 worker —— 那正是你要根治的
// 退化形态，绝不能让它出现在可指派清单里。TestFinancialRoleKeysMatchPromptSeeds
// 双向锁住两处不漂移。
//
// # 为什么不能用 isFinancialPrompt 的前缀判断
//
// `RoleAnalystSentiment`（股票版）命中 `analyst_` 前缀，但 promptcatalog.go 明确
// **不注册**它（无可靠的社交情绪源，Reddit/StockTwits 需付费 API）。用前缀当白名单
// 会把一个跑不起来的角色放进 enum，planner 派了它只会产出 unavailable。

// financialRoleOrder 是 planner 可指派的执行角色，顺序即 add_intent 的 enum 顺序。
//
// # 为什么写死顺序而不是从 BuiltinPromptSeeds 派生
//
// 这份列表会进工具 schema 与 planner prompt。map 迭代顺序随机 → 派生出的清单
// 每轮抖动，既破坏 prompt 缓存，也让模型看到不稳定的枚举。
//
// 按业务流排序（选品 → 分析 → 辩论 → 研究 → 交易 → 风控 → 组合），
// 让模型读清单时就能看出阶段先后。
var financialRoleOrder = []string{
	RoleScreener,
	RoleAnalystMarket, RoleAnalystNews, RoleAnalystFundamental, RoleAnalystSentimentCrypto,
	RoleDebateBull, RoleDebateBear,
	RoleResearchManager, RoleTrader,
	RoleRiskAggressive, RoleRiskConservative, RoleRiskNeutral,
	RolePortfolioManager,
}

// FinancialRoleKeys 返回可指派的执行角色（副本 —— 调用方可随意改，不影响内部状态）。
func FinancialRoleKeys() []string { return append([]string(nil), financialRoleOrder...) }

// IsFinancialRole 判断 role 是否可指派。
func IsFinancialRole(role string) bool { _, ok := financialRoleSet[role]; return ok }

var financialRoleSet = func() map[string]bool {
	m := make(map[string]bool, len(financialRoleOrder))
	for _, r := range financialRoleOrder {
		m[r] = true
	}
	return m
}()

// ExpectedStageRoles 返回该资产类别的阶段图**本该有**哪些执行角色（去重、不含 screener）。
//
// # 用途
//
// planner 的 `graph_overview` 拿它跟图里实际存在的 role 比对，报出
// `stage_graph_missing` —— 让「图没建出来」这件事可观测。
//
// 2026-11-11 task 11 的教训：screener 因 model_error 被标 blocked → 不调
// set_task_instrument → 下游 13 个节点（由 MaterializeDownstream 创建）**从未存在**。
// 而在此之前 planner 只看到「recent_done 里没有 trader」，与「压根没建」长得一样，
// 只能自己派无 role 的意图瞎补。
//
// # 为什么 InstrumentID 传 1
//
// 传 >0 表示「不展开选品阶段」，于是返回的集合里不含 screener。
// 「缺 screener」由调用方根据自己的「任务是否绑标的」判断，不靠这张表 ——
// 因为绑了标的的任务本来就不该有 screener，那不是缺失。
func ExpectedStageRoles(assetClass string) []string {
	g, err := Expand(assetClass, ExpandOpts{InstrumentID: 1})
	if err != nil {
		// 未知资产类别（future/option）Expand 会报错。返回 nil 让调用方
		// 当作「没有期望」，而不是报一堆假的缺失。
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, n := range g.Nodes {
		if n.Kind != KindIntent || n.Role == "" || seen[n.Role] {
			continue
		}
		seen[n.Role] = true
		out = append(out, n.Role)
	}
	return out
}
