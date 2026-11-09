package agent

// 各金融角色的 vars 构造（真的读兄弟节点产出）。
//
// # 设计
//
// buildRoleVars 是唯一入口，按 agentKey 分派到具体构造器。每个构造器：
//  1. 从 payload 读静态字段（instrument_id / round / max_rounds）——
//     必须用 intFromAny，不能用 .(int)（C1.9：JSONB 数字都是 float64）
//  2. 从兄弟节点读动态产出，**双层**：摘要进表格，全量进末尾
//  3. 两者都缺失时给显式说明，不是空串

import (
	"context"
	"fmt"
	"strings"

	"github.com/Autumn-27/artex/db"
)

// buildRoleVars 按 agentKey 构造对应的 vars 类型。
//
// 返回 nil 表示「不认识的金融角色」—— 调用方应退回 worker 路径。
func buildRoleVars(ctx context.Context, agentKey string, intent *db.Node) any {
	switch {
	case strings.HasPrefix(agentKey, "analyst_"):
		return buildAnalystRoleVars(ctx, agentKey, intent)
	case strings.HasPrefix(agentKey, "debate_"):
		return buildDebateVars(ctx, agentKey, intent)
	case agentKey == RoleResearchManager:
		return buildResearchManagerVars(ctx, intent)
	case agentKey == RoleTrader:
		return buildTraderVars(ctx, intent)
	case strings.HasPrefix(agentKey, "risk_"):
		return buildRiskVars(ctx, intent)
	case agentKey == RolePortfolioManager:
		return buildPMVars(ctx, intent)
	case agentKey == RoleScreener:
		return buildScreenerVars(ctx, intent)
	}
	return nil
}

// storeFrom 从 ctx 取 ExplorationStore（nil 安全）。
func storeFrom(ctx context.Context) *db.ExplorationStore {
	if ts, ok := ExplorationStoreFrom(ctx); ok {
		return ts
	}
	return nil
}

// readFacts / readTheses / readSignals 是三个常用读取组合。
//
// 抽出来是因为至少 4 个角色各要其中一种；每次内联写 siblingOutputs 调用
// 会让「perRoleLimit 该给几」这个决策散落多处。

// readFacts 读分析师事实（每个 role 最新一条）。
func readFacts(ts *db.ExplorationStore) (summary, full string) {
	nodes, err := siblingOutputs(ts, db.KindFact, analystRoles, 1)
	if err != nil {
		return fmt.Sprintf("（读取分析师事实失败: %v）", err), ""
	}
	if len(nodes) == 0 {
		return notWiredSibling, ""
	}
	return summarizeSiblings(nodes, "份事实"), joinSiblings(nodes)
}

// readTheses 读指定角色的论点（perRoleLimit=1 取最新）。
func readTheses(ts *db.ExplorationStore, roles []string) (summary, full string) {
	nodes, err := siblingOutputs(ts, db.KindThesis, roles, 1)
	if err != nil {
		return fmt.Sprintf("（读取论点失败: %v）", err), ""
	}
	if len(nodes) == 0 {
		return notWiredSibling, ""
	}
	return summarizeSiblings(nodes, "条论点"), joinSiblings(nodes)
}

// buildDebateVars 构造辩论 vars。
//
// 第 1 轮没有 PriorRoundSummary（对手还没说过话）；第 2 轮起读对手上一轮的 thesis。
func buildDebateVars(ctx context.Context, agentKey string, intent *db.Node) DebateVars {
	p := decodePayload(intent)
	ts := storeFrom(ctx)

	vars := DebateVars{
		Instrument:      instrumentFromPayload(intent, ts),
		AsOf:            asOfFromPayload(p),
		SkippedAnalysts: stringSliceFromAny(p["skipped_analysts"]),
		Round:           intFromAny(p["round"]),
		MaxRounds:       intFromAny(p["max_rounds"]),
	}
	if vars.Round <= 0 {
		vars.Round = 1 // 缺失时按第 1 轮处理（不会误读「上一轮」）
	}
	if vars.MaxRounds <= 0 {
		vars.MaxRounds = vars.Round
	}

	// 全部分析师事实（双层）
	vars.AnalystFacts, vars.AnalystFactsJSON = readFacts(ts)

	// 第 2 轮起读对手上一轮
	if vars.Round > 1 {
		if other := otherDebateRole(agentKey); other != "" {
			if s, found, err := siblingOutput(ts, db.KindThesis, other); err == nil && found {
				vars.PriorRoundSummary = s
			}
		}
	}
	return vars
}

// buildResearchManagerVars 构造研究经理 vars。
//
// perRoleLimit=0（全部）：研究经理要裁决**全部轮次**的辩论，
// 只取最新一条会丢掉前面轮次的论证演变。
func buildResearchManagerVars(ctx context.Context, intent *db.Node) ResearchManagerVars {
	p := decodePayload(intent)
	ts := storeFrom(ctx)

	vars := ResearchManagerVars{
		SkippedAnalysts: stringSliceFromAny(p["skipped_analysts"]),
		DataGaps:        stringSliceFromAny(p["data_gaps"]),
	}

	var facts, debates []siblingNode
	if ts != nil {
		facts, _ = siblingOutputs(ts, db.KindFact, analystRoles, 1)
		debates, _ = siblingOutputs(ts, db.KindThesis, debateRoles, 0) // 0 = 全部轮次
	}
	// # 为什么空数据也要填 JSON 字段（2026-11-06 事故）
	//
	// 原来只在非空时填 JSON，空时留空串。模板里是
	//
	//	━━ 完整数据：分析师事实（全量） ━━
	//	```json
	//	{{.AnalystFactsJSON}}
	//	```
	//
	// 于是渲染出**一个空白的 JSON 区块**。模型看到「有区块但没内容」，
	// 判定「数据故障」→ 去调 list_facts / list_thesis 补数据 →
	// 而那些工具在角色上下文里不可用（list_facts 报「不在任务探索图内」）→
	// 最终裁决 inconclusive，整条链白跑。
	//
	// 实测任务 36216：2 条 analyst fact + 2 条 debate thesis 都在库里，
	// SQL 也查得出来，但 research_manager 的裁决写着
	// 「分析师事实与辩论记录在当前 agent 上下文均不可读」。
	//
	// 填一句明确说明后，模型知道「确实是空的」，就不会去调工具。
	if len(facts) == 0 {
		vars.AnalystFacts = notWiredSibling
		vars.AnalystFactsJSON = `[]（上游分析师尚未产出事实 —— 这是数据缺失，` +
			`不是你的工具出问题。请按「证据不足」裁决，不要尝试调用工具补数据）`
	} else {
		vars.AnalystFacts = summarizeSiblings(facts, "份事实")
		vars.AnalystFactsJSON = joinSiblings(facts)
	}
	if len(debates) == 0 {
		vars.DebateTranscript = notWiredSibling
		vars.DebateTranscriptJSON = `[]（上游辩论尚未产出论点 —— 同上，按证据不足裁决，` +
			`不要尝试调用工具补数据）`
	} else {
		vars.DebateTranscript = summarizeSiblings(debates, "条辩论记录")
		vars.DebateTranscriptJSON = joinSiblings(debates)
	}
	return vars
}

// buildTraderVars 构造交易员 vars（读研究裁决 + 真的查 market_hours）。
func buildTraderVars(ctx context.Context, intent *db.Node) TraderVars {
	p := decodePayload(intent)
	ts := storeFrom(ctx)

	vars := TraderVars{
		Instrument:  instrumentFromPayload(intent, ts),
		AsOf:        asOfFromPayload(p),
		AccountMode: stringFromAny(p["account_mode"]),
	}
	if vars.AccountMode == "" {
		vars.AccountMode = "paper" // 默认模拟盘 —— 保守默认
	}

	if s, found, err := siblingOutput(ts, db.KindThesis, RoleResearchManager); err != nil {
		vars.ResearchConclusion = fmt.Sprintf("（读取研究裁决失败: %v）", err)
	} else if !found {
		vars.ResearchConclusion = notWiredSibling
	} else {
		vars.ResearchConclusion = "研究裁决已就绪（完整内容见末尾）"
		vars.ResearchConclusionJSON = s
	}

	// 最新价 + 交易时段（真的调 market_hours）
	vars.LastPrice, vars.TradingSession, vars.SessionError =
		buildTradingContext(ctx, vars.Instrument, vars.AsOf)

	// 执行账户：authorize_order 的必填入参。
	//
	// 从 ctx 取（server 装配时注入），不从 payload —— planner 建意图时
	// 不知道账户，而账户是全局执行配置，不该由意图携带。
	if acct, ok := ExecAccountFrom(ctx); ok {
		vars.AccountID = acct.ID
		vars.AccountRef = acct.Ref
		vars.MinOrderAmount = acct.MinOrderAmount
	}
	return vars
}

// buildRiskVars 构造三方风控 vars（读交易提案 + 研究裁决）。
func buildRiskVars(ctx context.Context, intent *db.Node) RiskVars {
	p := decodePayload(intent)
	ts := storeFrom(ctx)

	vars := RiskVars{
		Instrument:      instrumentFromPayload(intent, ts),
		SkippedAnalysts: stringSliceFromAny(p["skipped_analysts"]),
		Round:           intFromAny(p["round"]),
	}
	if vars.Round <= 0 {
		vars.Round = 1
	}

	if s, found, _ := siblingOutput(ts, db.KindSignal, RoleTrader); found {
		vars.TraderProposal = "交易提案已就绪（完整内容见末尾）"
		vars.TraderProposalJSON = s
	} else {
		vars.TraderProposal = notWiredSibling
	}
	if s, found, _ := siblingOutput(ts, db.KindThesis, RoleResearchManager); found {
		vars.ResearchConclusion = "研究裁决已就绪（完整内容见末尾）"
		vars.ResearchConclusionJSON = s
	} else {
		vars.ResearchConclusion = notWiredSibling
	}
	return vars
}

// buildPMVars 构造组合经理 vars（读提案 + 三方风控 + 分析师事实）。
//
// PM 有 3 个大数据输入，各自双层，渲染时分开小标题。
func buildPMVars(ctx context.Context, intent *db.Node) PMVars {
	p := decodePayload(intent)
	ts := storeFrom(ctx)

	vars := PMVars{
		Instrument:      instrumentFromPayload(intent, ts),
		SkippedAnalysts: stringSliceFromAny(p["skipped_analysts"]),
		DataGaps:        stringSliceFromAny(p["data_gaps"]),
	}

	if s, found, _ := siblingOutput(ts, db.KindSignal, RoleTrader); found {
		vars.TraderProposal = "交易提案已就绪（完整内容见末尾）"
		vars.TraderProposalJSON = s
	} else {
		vars.TraderProposal = notWiredSibling
	}

	var risks []siblingNode
	if ts != nil {
		risks, _ = siblingOutputs(ts, db.KindThesis, riskRoles, 1) // 各取最新一条
	}
	if len(risks) == 0 {
		vars.RiskDebateSummary = notWiredSibling
	} else {
		vars.RiskDebateSummary = summarizeSiblings(risks, "份风控意见")
		vars.RiskDebateSummaryJSON = joinSiblings(risks)
	}

	vars.AnalystFacts, vars.AnalystFactsJSON = readFacts(ts)
	return vars
}

// asOfFromPayload 从 payload 读 as_of（Expand 写的是 RFC3339）。
func asOfFromPayload(p map[string]any) string {
	s := stringFromAny(p["as_of"])
	if s == "" {
		return ""
	}
	// 只取日期部分 —— prompt 里「分析基准日」是日精度
	if i := strings.IndexByte(s, 'T'); i > 0 {
		return s[:i]
	}
	return s
}

// ScreenerVars 是选品员模板的变量。
//
// # 为什么不用 AnalystVars
//
// 选品员**没有标的**（这正它存在的理由），所以 AnalystVars 里的
// Instrument / Bars / Indicators 全是空的 —— 而空字段在模板里会渲染成
// 「标的：（）」「日线：」这种看起来像数据故障的东西，把模型带向
// 「数据没接好」的错误结论。
//
// 只保留它真正需要的一个字段：数据缺口说明（用来解释为什么没有标的）。
type ScreenerVars struct {
	DataGaps []string
}

// buildScreenerVars 构造选品员的模板变量。
//
// 信息极少是刻意的：选品员的任务是「按策略去市场里找」，
// 它能依赖的只有 user 消息里的意图摘要与自己调 get_quote 拿到的实时数据。
// 给它更多预置上下文（比如任务目标）反而会让它以为「已经知道答案」。
func buildScreenerVars(ctx context.Context, intent *db.Node) ScreenerVars {
	return ScreenerVars{
		DataGaps: []string{
			"本任务未指定具体标的 —— 需要你先筛选出候选",
		},
	}
}
