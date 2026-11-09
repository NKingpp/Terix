package agent

// 阶段图：把「一次投研任务要建哪些节点」变成确定性的数据展开。
//
// # 这不是 DAG 运行时
//
// StageGraph 只描述「要建哪些 exploration_nodes 与 edges」。所有并发调度、
// 门控、claim 都由 server/engine.go 与 db/exploration.go 承担。这里不查 DB、
// 不调 LLM、不读配置 —— 纯函数，输入 assetClass + opts，输出图。
//
// # 确定性是硬要求
//
// 同一 (assetClass, opts) 必须产出**完全相同**的图（节点数、顺序、Ref、
// Priority、边都一致）。因此：
//   - 节点用 []StageNode **按代码写死的顺序**追加，绝不用 map 迭代
//     （Go 的 map 迭代顺序随机，用 map 会导致两次调用顺序不同）
//   - 不依赖 time.Now()（AsOf 由调用方传入）、不依赖随机数
//
// TestExpandDeterministic 专门验证这一点 —— 它能抓住「看起来对但不可复现」
// 的实现，这是纯数据展开最容易犯又最难发现的错误。

import (
	"fmt"
	"time"
)

// 节点 Kind。与 db 层 exploration_nodes.kind 对应。
const (
	KindIntent     = "intent"
	KindJoin       = "join"
	KindFact       = "fact"
	KindThesis     = "thesis"
	KindSignal     = "signal"
	KindConclusion = "conclusion"
)

// 角色常量。Role 决定 prompt 与工具子集，join 节点为空。
const (
	RoleAnalystMarket    = "analyst_market"
	RoleAnalystNews      = "analyst_news"
	RoleAnalystSentiment = "analyst_sentiment"
	// RoleAnalystSentimentCrypto 是**加密专用**的情绪分析师，不是股票版的复用删段。
	//
	// 为什么必须独立：股票版的示例全是 Reddit / StockTwits / 分析师评级，
	// 对加密是误导 —— 模型会照着示例去找加密不存在的东西，或者自己「补」一个
	// 它认为合理的加密情绪源（比如用价格动量冒充情绪），而那个源大概率是错的。
	// 加密版基于 .Derivatives（资金费率 / 持仓量），见 PROMPTS-W5.md #4。
	//
	// 架构文档 §4.1 明确要求独立版本。
	RoleAnalystSentimentCrypto = "analyst_sentiment_crypto"
	RoleAnalystFundamental     = "analyst_fundamental"

	RoleDebateBull = "debate_bull"
	RoleDebateBear = "debate_bear"

	RoleResearchManager = "research_manager"
	RoleTrader          = "trader"

	RoleRiskAggressive   = "risk_aggressive"
	RoleRiskConservative = "risk_conservative"
	RoleRiskNeutral      = "risk_neutral"

	RolePortfolioManager = "portfolio_manager"

	// RoleScreener 是「选品员」—— 只在任务**未绑定标的**时出现。
	//
	// # 为什么需要它
	//
	// 用户允许不指定标的、让系统自己挑。但原有阶段图从分析师开始，
	// 而分析师 prompt 里写死了 {{.Instrument.Symbol}} —— 无标的时它是空的，
	// 分析师只能瞎猜（实测：连搜 6 次 SearchExtraTools 找工具，最后靠
	// 任务描述里的文字巧合对上）。
	//
	// 所以无标的任务必须在最前面插一个显式的「选品」阶段：
	// 它拿策略描述去扫候选，产出候选清单与理由，后面的分析师才有标的可分析。
	RoleScreener = "screener"
	// v1.6 删除了 RoleConclusion：PROMPTS-W5 没有 conclusion 角色，
	// conclusion 只是 research_manager 的输出字段名。
)

// 资产类别。
//
// # 为什么 etf / index 与 stock 同模板
//
// instruments 表允许 6 种 asset_class（stock/etf/index/crypto/future/option），
// 而 MarketToolSet 的 marketToolMetas 也把 etf/index 列进了 AssetClasses。
// 也就是说行情工具认为它们是一等公民。
//
// 分析维度上，etf/index 就是股票的维度（技术面 + 情绪 + 宏观），
// 唯一差别是「没有财报」—— 而 analyst_fundamental 本来就没注册
// （缺 get_fundamentals），所以这个差别不影响图的结构。
//
// 还有一个实际依赖：get_volatility 内部要查 ^VIX 做波动率对照，而 ^VIX 是 index。
// 不放行 index 的话，那条对照路径永远走不到。
//
// future / option **不放行** —— 它们的分析框架有实质差异
// （保证金/交割/杠杆 vs 希腊字母/隐含波动率），套股票模板是「问错了问题」，
// 不是「缺几个字段」。它们需要独立的分析师组合与风险模型。
const (
	AssetClassStock  = "stock"
	AssetClassETF    = "etf"
	AssetClassIndex  = "index"
	AssetClassCrypto = "crypto"
)

// StageGraph 是一次任务的阶段骨架。
type StageGraph struct {
	Nodes []StageNode
	Edges []StageEdge
	// SkippedAnalysts 落 begin.payload.skipped_analysts。
	// Reason 必须中文可读 —— 它会原样进 prompt，英文或代号对模型无意义。
	SkippedAnalysts []Skipped
}

// StageNode 是一个待建节点。
type StageNode struct {
	Ref      string // 图内引用名，如 "analyst_market"
	Kind     string // 'intent' | 'join' | 'fact' | 'thesis' | 'signal' | 'conclusion'
	Role     string // 决定 prompt 与工具子集；join 节点为空
	GroupID  string // 同级并行节点同 group
	Priority int    // 越小越先
	// Await 是本节点等待的上游 Ref 列表。**只有 join 节点使用**（非 join 恒为 nil）。
	//
	// 赋值点：join_analysts（:231）与 join_risk（:291），用 analystRefs / riskRefs 填充。
	// 自校验：Expand 末尾会检查每个 Await 引用的 Ref 存在（:322-335）。
	//
	// **依赖的权威来源是 StageGraph.Edges**（由 link() 填充），不是本字段。
	// 两者目前表达同一份信息（join 的每条 Await 都同时有一条 link），
	// 但 materializeGraph 只读 Edges —— 因为 Edges 覆盖**全部**阶段依赖，
	// 而 Await 只覆盖 join 的直接上游。读 Await 建图会漏掉非 join 之间的边
	// （如 trader → risk_*），导致门控失效。
	Await   []string
	Payload map[string]any // 附加数据（如 as_of、instrument_id）
}

// StageEdge 是一条依赖边。
type StageEdge struct {
	From string // 上游 Ref
	To   string // 下游 Ref
}

// Skipped 记录被裁掉的阶段及原因（中文可读）。
type Skipped struct {
	Role   string
	Reason string
}

// ExpandOpts 是展开参数。
type ExpandOpts struct {
	// MaxDebateRounds 多空辩论轮数。
	// 0 表示按资产类别取默认：stock=2, crypto=1（见 defaultDebateRounds）。
	// 非 0 则覆盖，所有资产类别统一用该值。
	MaxDebateRounds int
	// MaxRiskDiscussRounds 风控讨论轮数。0 表示默认 1。
	MaxRiskDiscussRounds int
	InstrumentID         int64
	AsOf                 time.Time

	// ScreenerOnly 表示「只建选品阶段，下游等选出标的再展开」。
	//
	// # 为什么需要（2026-11-05 用户要求）
	//
	// 无标的任务原来一次性建 14 个意图，但其中 13 个都要 instrument_id ——
	// 而标的要等 screener 跑完才知道。于是那 13 个节点建出来就是空标的，
	// 分析师拿到 instrument_id=0，全部判「标的未知，无法分析」。
	//
	// 建了等于没建，还污染了任务详情页（14 个 intent 待办，实际只有一个能跑）。
	//
	// 现在分两步：
	//	① 创建任务 → 只建 screener（ScreenerOnly=true）
	//	② screener 调 set_task_instrument 选定标的后 → 展开剩余阶段
	ScreenerOnly bool
}

// defaultDebateRounds 按资产类别给辩论轮次默认值。
//
// 为什么 crypto 只 1 轮：crypto 无 fundamental 维度（见 skipped_analysts），
// 多空双方各只有 market/news/sentiment 三个证据源。第二轮辩论无法引入
// 新证据，只会重复陈述——那是纯 token 浪费，且会稀释真正有信息量的论点。
// stock 有基本面这个独立维度，第二轮能就「估值 vs 技术面矛盾」展开，价值明显。
func defaultDebateRounds(assetClass string) int {
	if assetClass == AssetClassCrypto {
		return 1
	}
	return 2
}

// analystsFor 返回该资产类别的分析师角色（按固定顺序）。
//
// 顺序即节点顺序。crypto 少 fundamental —— 加密无财报与 SEC filing。
func analystsFor(assetClass string) []string {
	// 只注册**工具齐全**的分析师 —— 注册了但工具不存在，分析师会反复尝试
	// 调用不存在的工具然后返回 unavailable，比不注册更糟：它在 facts 里留下
	// 「该维度数据缺失」，让下游辩论误以为「数据源故障」而不是「该角色没上线」。
	//
	// # 2026-10-30 解禁 news + fundamental
	//
	// 原先三个分析师都因缺数据源被排除（KNOWN-LIMITATIONS L5）。实测后
	// news 与 fundamental 的阻塞已解除：
	//
	//   - analyst_news：akshare 侧新增 get_news，走财新（stock_news_main_cx）
	//     + 财联社（stock_info_global_cls）两个**非东财**源，含美股内容。
	//     东财（_em）仍被 IP 封禁，FORBIDDEN_SOURCES 不变。
	//   - analyst_fundamental：新增 get_fundamentals，走新浪财报
	//     （stock_financial_report_sina / stock_financial_abstract），实测可用。
	//     ⚠️ 目前只有 A 股通道；美股/港股返回 _unavailable，分析师会在
	//     DataGaps 里如实说明「该市场财报通道未接入」。
	//   - analyst_sentiment（股票版）：**仍然不注册** —— 无可靠社交情绪源
	//     （Reddit/StockTwits 需付费 API，且架构已定 P0 不做）。加密侧有
	//     analyst_sentiment_crypto 走资金费率/持仓量，那是独立实现。
	//
	// 恢复顺序按「证据价值从高到低」：fundamental（财报是估值锚）→ news（事件驱动）。
	if assetClass == AssetClassCrypto {
		// crypto：market + sentiment_crypto（资金费率/持仓量）齐全。
		// 无 fundamental（加密无财报）、无 news（加密新闻源未接入）。
		return []string{RoleAnalystMarket, RoleAnalystSentimentCrypto}
	}
	// stock/etf/index：market + fundamental + news 齐全。
	// etf/index 走同一套——它们也有持仓与估值数据可读（新浪接口按代码取）。
	return []string{RoleAnalystMarket, RoleAnalystFundamental, RoleAnalystNews}
}

// riskRolesFor 返回该资产类别的风控角色。
//
// crypto 无 aggressive —— 激进方的核心论据是「基本面被低估 / 财报超预期」，
// 无基本面锚时该前提不成立，留着只会产出空洞的激进意见。
func riskRolesFor(assetClass string) []string {
	if assetClass == AssetClassCrypto {
		return []string{RoleRiskConservative, RoleRiskNeutral}
	}
	return []string{RoleRiskAggressive, RoleRiskConservative, RoleRiskNeutral}
}

// Expand 按 asset_class 确定性展开阶段图。
//
// 展开结果随 L5 收缩（分析师 4→1）而变化，当前默认轮次：
//
//	stock  → 13 节点（1分析师+1join+4辩论+1研究+1交易+3风控+1join+1组合）
//	crypto → 11 节点（2分析师+1join+2辩论+1研究+1交易+2风控+1join+1组合）
//
// ⚠️ 不要把这里的数字当「规格」读 —— 它随 L5 的恢复（分析师 1→4）而变。
//
//	以 pipeline_test.go 的断言为准，而那里的断言附带完整推导
//	（如 `1+1+2+1+1+3+1+1 = 11`），所以「流程改了」与「注释过期了」可以区分。
//
// # 为什么这条注释曾经是错的
//
// 它原写「stock → 17 节点 / crypto → 13 节点」，那是 L5 收缩之前（分析师 4 个）的数字。
// L5 收缩后变成 13 / 11，注释没跟着改。
//
// 这造成一个真实的坑：如果照注释硬编码 17 写断言，测试会红，
// 而红的理由看起来像「代码坏了」，实际是「注释过期」—— 排查成本 2 分钟起，
// 且每次看到红灯的人都要重新排查一遍。
func Expand(assetClass string, opts ExpandOpts) (*StageGraph, error) {
	analysts := analystsFor(assetClass)
	switch assetClass {
	case AssetClassStock, AssetClassETF, AssetClassIndex, AssetClassCrypto:
		// ok —— etf/index 与 stock 同模板（理由见 AssetClass* 常量的注释）
	default:
		// 未知资产类别必须报错，**不能返回空图** —— 空图会让上层以为
		// 「该资产类别不需要任何阶段」，实际是「我们不认识它」。
		//
		// future/option 在这里被拒是**有意的**：它们的分析框架与股票有实质差异，
		// 用股票模板会产生「看起来跑通了但问错了问题」的结果 —— 那比报错更危险。
		return nil, fmt.Errorf("未知 asset_class %q（支持 stock|etf|index|crypto；"+
			"future/option 的分析维度不同，需独立模板）", assetClass)
	}

	debateRounds := opts.MaxDebateRounds
	if debateRounds <= 0 {
		debateRounds = defaultDebateRounds(assetClass)
	}
	riskRounds := opts.MaxRiskDiscussRounds
	if riskRounds <= 0 {
		riskRounds = 1
	}
	riskRoles := riskRolesFor(assetClass)

	// 显式初始化成空切片，不是 nil。
	//
	// # 为什么这件事重要
	//
	// nil 序列化成 JSON null，[] 序列化成 []。
	// 而 SkippedAnalysts 会落进 begin.payload.skipped_analysts（JSONB）：
	//   前端拿到 null → 说「字段不存在」
	//   前端拿到 []   → 说「没有跳过项」
	// 两者语义不同，而 null 会让前端写多余的判空逻辑。
	//
	// 测试断言：TestExpandStockSkippedIsEmptySliceNotNil。
	g := &StageGraph{SkippedAnalysts: []Skipped{}}
	basePayload := map[string]any{
		"asset_class":   assetClass,
		"instrument_id": opts.InstrumentID,
	}
	if !opts.AsOf.IsZero() {
		basePayload["as_of"] = opts.AsOf.UTC().Format(time.RFC3339)
	}
	// 每个节点都要一份独立的 payload map（共享同一个 map 会导致后续
	// 修改一处影响全部节点）。
	payload := func(extra map[string]any) map[string]any {
		p := make(map[string]any, len(basePayload)+len(extra))
		for k, v := range basePayload {
			p[k] = v
		}
		for k, v := range extra {
			p[k] = v
		}
		return p
	}

	add := func(ref, kind, role, group string, prio int, extra map[string]any) {
		g.Nodes = append(g.Nodes, StageNode{
			Ref:      ref,
			Kind:     kind,
			Role:     role,
			GroupID:  group,
			Priority: prio,
			Payload:  payload(extra),
		})
	}
	link := func(from, to string) {
		g.Edges = append(g.Edges, StageEdge{From: from, To: to})
	}

	// ---- 阶段 0：选品（仅无标的任务）----
	//
	// 无标的 = 用户让系统自己挑。必须先有一个角色把「策略」变成「具体标的」，
	// 否则后面所有分析师都拿不到 {{.Instrument.Symbol}}，只能瞎猜。
	//
	// 这个节点的产出是候选清单（emit_fact 记录），planner 据此再派分析意图；
	// 图里它是分析师的**前置依赖**，所以在 join 之前串在链头。
	const refScreener = "screener"
	needsScreen := opts.InstrumentID <= 0
	if needsScreen {
		add(refScreener, KindIntent, RoleScreener, "screen", 5, map[string]any{
			"stage": "screen",
			// 标记「本任务未绑标的」，供 screener prompt 与下游判断。
			"no_instrument": true,
		})
	}
	// 只建选品阶段：下游等 set_task_instrument 选定标的后再展开。
	//
	// 此时返回的图只有一个节点 —— 刻意的，见 ExpandOpts.ScreenerOnly 注释。
	// 上层（server）在 screener 完成后调 MaterializeDownstream 补建其余阶段。
	if needsScreen && opts.ScreenerOnly {
		return g, nil
	}

	// ---- 阶段 1：分析师（并行）----
	const groupAnalysts = "analysts"
	analystRefs := make([]string, 0, len(analysts))
	for _, role := range analysts {
		ref := role // 角色名即 Ref（analyst_market / analyst_news / ...）
		add(ref, KindIntent, role, groupAnalysts, 10, map[string]any{"stage": "analysis"})
		analystRefs = append(analystRefs, ref)
		// 无标的任务：分析师要等选品完成才有标的。
		//
		// 这条边让分析师在选品结束前不启动 —— 否则它又会在空标的下瞎猜，
		// 白白浪费一轮回合（实测那会导致 6 次无效的工具搜索）。
		if needsScreen {
			link(refScreener, ref)
		}
	}

	// ---- 阶段 2：汇合分析师 ----
	const refJoinAnalysts = "join_analysts"
	add(refJoinAnalysts, KindJoin, "", groupAnalysts, 20, map[string]any{"stage": "analysis"})
	g.Nodes[len(g.Nodes)-1].Await = append([]string{}, analystRefs...)
	for _, ref := range analystRefs {
		link(ref, refJoinAnalysts)
	}

	// ---- 阶段 3：多空辩论（按轮次，bull/bear 交替）----
	//
	// 辩论阶段**不加 join 节点**：
	//   - 辩论是串行链（bull→bear→bull→bear），每轮读上一轮产出，不存在并行分支
	//   - join 的语义是「等 N 个并行分支全部完成」，串行链用它只是多一个中转
	//   - 风控阶段加 join 是因为 3 个风控方真的并行
	// 相关边：最后一条辩论边直接连向 research_manager。
	const groupDebate = "debate"
	var debateRefs []string
	prev := refJoinAnalysts
	for round := 1; round <= debateRounds; round++ {
		for _, role := range []string{RoleDebateBull, RoleDebateBear} {
			ref := fmt.Sprintf("%s_r%d", role, round)
			add(ref, KindIntent, role, groupDebate, 30+round, map[string]any{
				"stage": "debate", "round": round,
			})
			link(prev, ref)
			prev = ref
			debateRefs = append(debateRefs, ref)
		}
	}

	// ---- 阶段 4：研究经理 ----
	const refResearchManager = "research_manager"
	add(refResearchManager, KindIntent, RoleResearchManager, "research", 50,
		map[string]any{"stage": "research"})
	if len(debateRefs) > 0 {
		link(debateRefs[len(debateRefs)-1], refResearchManager)
	}

	// ---- 阶段 5：交易员 ----
	const refTrader = "trader"
	add(refTrader, KindIntent, RoleTrader, "trade", 60, map[string]any{"stage": "trade"})
	link(refResearchManager, refTrader)

	// ---- 阶段 6：风控（并行，多轮）----
	const groupRisk = "risk"
	var riskRefs []string
	for round := 1; round <= riskRounds; round++ {
		for _, role := range riskRoles {
			ref := role
			if riskRounds > 1 {
				ref = fmt.Sprintf("%s_r%d", role, round)
			}
			add(ref, KindIntent, role, groupRisk, 70+round, map[string]any{
				"stage": "risk", "round": round,
			})
			link(refTrader, ref)
			riskRefs = append(riskRefs, ref)
		}
	}

	// ---- 阶段 7：汇合风控 ----
	const refJoinRisk = "join_risk"
	add(refJoinRisk, KindJoin, "", groupRisk, 80, map[string]any{"stage": "risk"})
	g.Nodes[len(g.Nodes)-1].Await = append([]string{}, riskRefs...)
	for _, ref := range riskRefs {
		link(ref, refJoinRisk)
	}

	// ---- 阶段 8：组合经理 ----
	const refPortfolioManager = "portfolio_manager"
	add(refPortfolioManager, KindIntent, RolePortfolioManager, "decision", 90,
		map[string]any{"stage": "decision"})
	link(refJoinRisk, refPortfolioManager)

	// PM 是流程终点：它的 emit_rating 就是本次分析的最终结论。
	//
	// v1.6 删除了原「阶段 9：结论（conclusion）」节点，原因：
	//  1. PROMPTS-W5.md 里 **没有 conclusion 角色** —— 13 个角色里
	//     conclusion 只是 research_manager 的输出字段名（bull_wins/bear_wins），
	//     与 ck_node_kind 的 'conclusion' kind 撞名但不是同一个东西。
	//  2. 它不产出任何下游需要的东西：PM 的 rating 已是终态，conclusion 只是「记录」。
	//  3. 纯记录阶段让 LLM 跑一轮，token 成本上升而收益为零 ——
	//     正解是引擎在任务结束时写一条 activity，而不是让模型「封口」。
	//
	// 若将来审计确实需要独立的封口节点，正解是新增 reporter 角色 + 第 14 个 prompt，
	// 而不是复用不存在的 conclusion 角色。

	// ---- skipped_analysts ----
	if assetClass == AssetClassCrypto {
		g.SkippedAnalysts = []Skipped{
			{
				Role:   RoleAnalystFundamental,
				Reason: "加密资产无财报与 SEC filing，基本面维度不存在",
			},
			{
				Role:   RoleAnalystNews,
				Reason: "加密新闻源未接入（财新/财联社只覆盖股市场景）",
			},
			{
				Role:   RoleRiskAggressive,
				Reason: "无基本面锚时激进方前提不成立",
			},
		}
	} else {
		// 股票侧明确记录「情绪分析师缺席」——不是遗漏，是没有可靠数据源。
		//
		// # 为什么要把「没做的」也写进 skipped
		//
		// 前端会把 skipped_analysts 渲染成「本次分析未覆盖的维度」。
		// 若什么都不写，用户看到 3 个分析师会以为「情绪分析已经做了」；
		// 写进去才能让他知道「这维度没有数据，别指望报告里有情绪判断」。
		// 这与「加密没有基本面」是同一类信息：**缺失本身要可见**。
		g.SkippedAnalysts = []Skipped{
			{
				Role:   RoleAnalystSentiment,
				Reason: "股票情绪源未接入（无可靠社交数据源，P0 未做）",
			},
		}
	}

	// ---- 自校验：Await 引用的 Ref 必须存在 ----
	// 这类错误不校验的话，会在运行时「等一个永远不存在的节点」而永久阻塞，
	// 排查成本极高。宁可在展开时立刻失败。
	refs := make(map[string]bool, len(g.Nodes))
	for _, n := range g.Nodes {
		if refs[n.Ref] {
			return nil, fmt.Errorf("阶段图内部错误：Ref %q 重复", n.Ref)
		}
		refs[n.Ref] = true
	}
	for _, n := range g.Nodes {
		for _, a := range n.Await {
			if !refs[a] {
				return nil, fmt.Errorf("阶段图内部错误：节点 %q 的 Await 引用了不存在的 %q", n.Ref, a)
			}
		}
	}
	for _, e := range g.Edges {
		if !refs[e.From] {
			return nil, fmt.Errorf("阶段图内部错误：边 %q→%q 的 From 不存在", e.From, e.To)
		}
		if !refs[e.To] {
			return nil, fmt.Errorf("阶段图内部错误：边 %q→%q 的 To 不存在", e.From, e.To)
		}
	}
	return g, nil
}
