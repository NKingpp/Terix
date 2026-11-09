package agent

import (
	"context"
	"encoding/json"

	actool "github.com/Autumn-27/norma/tool"
)

// 本文件把「内置工具」从纯代码变成可枚举、可被 DB 覆盖的目录：
//   - BuiltinToolSeeds()：把三个执行 agent 的内置工具集展开成 seed 记录（key +
//     描述 + 参数 schema + 默认绑定的 agent），供服务端开机幂等播种进 tools 表。
//   - ToolResolve 钩子：运行时按 DB 里的 tools 行对已装配的工具做「按 agent 过滤 +
//     覆盖描述/schema + 注入参数默认值」。key/handler 仍在代码层，DB 只改「散文与默认值」。
// handler（Call 行为）永远来自代码——DB 改不了它，只能改模型看到的说明与缺省入参。

// ToolSeed 是一个内置工具的可播种快照：key 即 CoreTool.Name()（与 handler 死绑，
// UI 只读），Desc/Schema 取自代码里的工具定义，Agents 是代码默认把它给了哪些 agent。
type ToolSeed struct {
	Key    string         // = CoreTool.Name()，主键，不可改
	Desc   string         // 顶层描述（可在 UI 覆盖）
	Schema map[string]any // 参数 JSON-Schema（结构只读，description/default 可在 UI 改）
	Agents []string       // 默认绑定的 agent key（worker/planner/mainagent）
}

// builtinToolsByAgent 用一个「只读空壳」ToolSet（nil stores）构造每个执行 agent 的
// 领域工具集。工具构造函数只把闭包塞进 Spec、构造期不解引用 store，所以 nil 安全——
// 这些工具在这里只用来读 Name()/Description()/InputSchema()，绝不 Call。
//
// 刻意【不含】SDK 通用工具 actool.DefaultTools()（Read/Write/Edit/MultiEdit/LS/Glob/
// Grep/Bash）：它们每个 agent 都固定拥有、没有「绑定到谁」的取舍，且说明大多在 Prompt()
// 里（本表只覆盖 Description()，会造成半覆盖误导）。不 seed → 无 DB 行 → ToolResolve
// 原样放行、不覆盖，行为与从前一致。只有 artex 自己的领域工具入表可管。
func builtinToolsByAgent() map[string][]actool.CoreTool {
	ts := NewToolSet(nil, "")
	// 金融工具用 nil client 构造空壳 —— 只读 Name/Description/InputSchema，
	// 构造期不解引用 client，所以 nil 安全（与 NewToolSet(nil,"") 同一原则）。
	// 必须进这张表，否则 tools 表没有种子行，ToolResolve 会把金融工具当
	// 「未知工具」无条件放行，DB 里的启用/绑定开关就失效了。
	//
	// 这里的 mts 是**种子壳**（nil client / nil DB）—— 只用于读 Name/Desc/Schema，
	// 绝不会被 Call。它的唯一用途是让金融工具在 tools 表里有行、能绑到角色上。
	//
	// 运行时真正可调用的是 `server.buildDomainReg` 里构造的那个实例
	// （带 server 装配的 marketCl/cryptoCl/cryptoFlow + SetDB），
	// 由 ToolResolve 按 tools.agents 注入给对应 agent。
	//
	// 两个实例分工：
	//   种子壳（这里）—— 写 tools 表，决定「谁能用」
	//   运行实例（buildDomainReg）—— 真正执行，决定「怎么取数」
	//
	// 这也是为什么改了这里必须重启：tools.agents 是启动时 seed 的。
	mts := NewMarketToolSet(nil)
	return map[string][]actool.CoreTool{
		"mainagent": append(ts.MainAgentTools(), mts.Tools()...),
		"planner":   append(ts.PlannerTools(), mts.Tools()...),
		"worker":    append(ts.WorkerTools(), mts.Tools()...),
		// goals（目标拆解器）默认绑 set_goals + set_constraints：靠它们把拆出的目标、
		// 抽出的操作约束写进库。与 mainagent 共用同一受管工具，web 端可改描述/schema、按 agent 勾选。
		"goals": {ts.setGoals(), ts.setConstraints()},
		// auto 默认绑漏洞上报 + 资产管理工具，其他域工具可在 UI 按需勾选。
		// 新库由此 seed 写入；老库由 seedAutoDefaultBindings 迁移。
		"auto": {ts.addFinding(), ts.insertAssets(), ts.addCompanyScope(), ts.listAssets(), ts.listCompanies()},
		// pentest（独立渗透 agent）默认绑：查资产 / 插资产 / 报漏洞 / 查漏洞 / 查企业。
		// 新库由此 seed 写入；老库由 seedPentestDefaultBindings 迁移。
		"pentest": {ts.listAssets(), ts.insertAssets(), ts.addFinding(), ts.listFindings(), ts.listCompanies()},

		// ── W5 金融分析角色 ──────────────────────────────────────────────
		//
		// 每个角色的工具子集按 ARCH-W2-W5 §4.3.2 定。这里是**代码默认绑定**，
		// 运行时 ToolResolve 按 tools.agents 过滤 —— 所以这里写的必须与 spec 一致，
		// 否则角色会拿到不该有的工具（如分析师拿到 insert_assets）或被过滤到空。
		//
		// 注意 worker 的 emit_* / list_thesis / list_signals 也在这里出现：
		// 那些工具的 Tools 实例来自 ts（ToolSet），不是 mts，所以直接用 ts.xxx()。

		// analyst_market: get_quote get_kline compute_indicator get_volatility
		//                 list_facts emit_fact
		//
		// mx_query：妙想是东财权威数据库，技术面分析常要「换手率/量比/资金流」
		// 这类 akshare 拿不到的结构化字段 —— 用自然语言问句补这一层。
		// screener: get_quote get_kline compute_indicator get_volatility
		//           list_facts emit_fact
		//
		// 与 analyst_market 几乎相同，但**不给 mx_query/mx_news**：
		// 选品阶段只需要「能算的条件」（价格/涨跌幅/波动率），
		// 而妙想是自然语言问句接口，返回的是非结构化文本 ——
		// 用它做批量候选筛选既慢又不可靠（每个候选一次问句）。
		//
		// 刻意不给 emit_rating/emit_signal：选品不是结论，
		// 产出候选清单（emit_fact）就够，评级是分析师之后的事。
		// set_task_instrument 不在 agent 包 —— 它由 server 装配（见
		// server/set_instrument_tool.go）。这里只列 agent 包内能构造的工具，
		// 否则会 import 循环（server 依赖 agent）。
		"screener": {
			mts.getQuote(), mts.getKline(), mts.computeIndicator(),
			mts.getVolatility(), ts.listFacts(), ts.emitFact(),
		},
		"analyst_market": {
			mts.getQuote(), mts.getKline(), mts.computeIndicator(),
			mts.getVolatility(), mts.mxQuery(), ts.listFacts(), ts.emitFact(),
		},
		// analyst_sentiment_crypto: get_crypto_flow get_macro list_facts emit_fact
		//
		// **刻意不给 mx_***：妙想没有加密数据（见 marketToolMetas 的
		// mx_query/mx_news 注释）。给它等于绑一个永远返回 MX_NO_DATA 的工具，
		// 而空结果在 prompt 里会被读成「今天没有加密消息面事件」。
		"analyst_sentiment_crypto": {
			mts.getCryptoFlow(), mts.getMacro(), ts.listFacts(), ts.emitFact(),
		},
		// analyst_news: get_quote get_news get_macro mx_news list_facts emit_fact
		//
		// get_news 只覆盖 cn/us/hk（见 marketToolMetas 的 AssetClasses 注释）——
		// 财新与财联社都是股票市场文本源，没有加密新闻。
		//
		// mx_news 是**核心补充**：妙想资讯混合返回新闻/公告/研报（含机构评级），
		// 比 get_news 的「财新+财联社」覆盖面宽得多，且研报评级是 get_news 没有的维度。
		"analyst_news": {
			mts.getQuote(), mts.getNews(), mts.getMacro(), mts.mxNews(),
			ts.listFacts(), ts.emitFact(),
		},
		// analyst_fundamental: get_quote get_fundamentals mx_query list_facts emit_fact
		//
		// mx_query：get_fundamentals 只有 A 股、只有白名单科目；妙想能问
		// 「近三年毛利率」「单季度扣非净利润」这类 get_fundamentals 给不了的问句，
		// 也能覆盖港股/美股（get_fundamentals 对它们返回 NO_DATA）。
		"analyst_fundamental": {
			mts.getQuote(), mts.getFundamentals(), mts.mxQuery(),
			ts.listFacts(), ts.emitFact(),
		},
		// debate_bull / debate_bear: list_facts list_thesis emit_thesis
		"debate_bull": {ts.listFacts(), ts.listThesis(), ts.emitThesis()},
		"debate_bear": {ts.listFacts(), ts.listThesis(), ts.emitThesis()},
		// research_manager: list_facts list_thesis emit_thesis
		"research_manager": {ts.listFacts(), ts.listThesis(), ts.emitThesis()},
		// trader: get_quote mx_query list_thesis emit_signal
		//
		// mx_query 用于确认最新价/涨跌幅口径（交易员下单前要一个可复核的最新数），
		// 但不给它 emit_fact —— 交易员产出的是 signal，不是事实。
		"trader": {mts.getQuote(), mts.mxQuery(), ts.listThesis(), ts.emitSignal()},
		// risk_*: list_thesis list_facts emit_thesis
		"risk_aggressive":   {ts.listThesis(), ts.listFacts(), ts.emitThesis()},
		"risk_neutral":      {ts.listThesis(), ts.listFacts(), ts.emitThesis()},
		"risk_conservative": {ts.listThesis(), ts.listFacts(), ts.emitThesis()},
		// portfolio_manager: list_facts list_thesis list_signals emit_rating
		"portfolio_manager": {ts.listFacts(), ts.listThesis(), ts.listSignals(), ts.emitRating()},
	}
}

// defaultUnbound：这些 system 工具会照常入目录（web 端可见、可手动按 agent 勾选），但
// 默认【不绑任何 agent】——ToolResolve 对空绑定的工具对所有 agent 一律丢弃,须显式 opt-in。
// 之所以仍留在某个 agent 的 base 工具集里(如 goal_met 在 PlannerTools):一是让 seed 能
// 构造它拿到 desc/schema,二是用户手动绑回后运行时 base 里有它、ToolResolve 才留得住。
//
// goal_met：绕过逐个 prove_goal、直接从全局宣布【整个任务完成】,权重大且有误判风险,又与
// 「prove_goal 标记最后一个目标 → 自动收官」重复,故默认不给任何 agent,需要时再手动绑。
var defaultUnbound = map[string]bool{"goal_met": true}

// BuiltinToolSeeds 把各 agent 的内置工具集去重合并成 seed 列表：同名工具（如 list_assets
// 多个 agent 都有）合成一条，Agents 取并集；defaultUnbound 里的工具则强制绑定为空。
func BuiltinToolSeeds() []ToolSeed {
	byAgent := builtinToolsByAgent()
	order := []string{"mainagent", "goals", "planner", "worker", "auto", "pentest"}

	type acc struct {
		tool   actool.CoreTool
		agents []string
	}
	m := map[string]*acc{}
	var keys []string
	for _, ak := range order {
		for _, t := range byAgent[ak] {
			a, ok := m[t.Name()]
			if !ok {
				a = &acc{tool: t}
				m[t.Name()] = a
				keys = append(keys, t.Name())
			}
			a.agents = append(a.agents, ak)
		}
	}

	out := make([]ToolSeed, 0, len(keys))
	for _, k := range keys {
		a := m[k]
		agents := a.agents
		if defaultUnbound[k] {
			agents = []string{} // 入目录、可手动绑，但默认不给任何 agent（存 [] 而非 null，与其它工具一致）
		}
		out = append(out, ToolSeed{
			Key:    k,
			Desc:   a.tool.Description(),
			Schema: a.tool.InputSchema(),
			Agents: agents,
		})
	}
	return out
}

// ToolResolve, if set, post-processes an agent's fully-assembled tool list against
// the DB tools table: it drops tools not bound to this agent (or globally disabled)
// and wraps the rest so the model sees the DB-overridden description/schema and
// 缺省入参 get injected. Tools with no matching DB row (MCP/skill/host tools like
// traffic) pass through untouched. nil = tools unchanged. Wired in server/assembly.go.
var ToolResolve func(ctx context.Context, agentKey string, tools []actool.CoreTool) []actool.CoreTool

// DecorateTool wraps t so Description()/InputSchema() report the DB overrides and
// Call() injects scalar parameter defaults (from schema's "default" props) whenever
// the model omitted them. Name/Prompt/permission/scheduler flags delegate to t, so
// the tool's identity and handler are unchanged. Empty desc/schema fall back to t's.
func DecorateTool(t actool.CoreTool, desc string, schema map[string]any) actool.CoreTool {
	if desc == "" {
		desc = t.Description()
	}
	if len(schema) == 0 {
		schema = t.InputSchema()
	}
	return &overriddenTool{CoreTool: t, desc: desc, schema: schema}
}

// overriddenTool is a CoreTool decorator: it embeds the original (so all behavioral
// methods — Prompt/IsReadOnly/IsConcurrencySafe/CheckPermissions/Name — delegate)
// and overrides only the model-facing description/schema plus default injection.
type overriddenTool struct {
	actool.CoreTool
	desc   string
	schema map[string]any
}

func (o *overriddenTool) Description() string         { return o.desc }
func (o *overriddenTool) InputSchema() map[string]any { return o.schema }

// AcceptsRawInput 必须显式转发。
//
// # 为什么（2026-11-05 事故）
//
// harness 用**类型断言**发现它：
//
//	type rawInputTool interface{ AcceptsRawInput() bool }
//	r, ok := t.(rawInputTool)
//
// 而 AcceptsRawInput 刻意不在 CoreTool 接口里（见 norma/tool 的注释：
// 加进公开接口会破坏所有实现方）。所以 struct 嵌入 CoreTool **不会**
// 提升这个方法 —— 装饰后的工具类型断言必然失败，RawInput 配置被静默丢弃。
//
// 后果：readTool 设了 RawInput=true 想让工具自己修「多个 JSON 拼接」的入参，
// 但经过本装饰器后 harness 仍然先做 schema 校验 → 拼接入参必被拒
// → 模型退化成空调用 → 全流程卡死（实测三次）。
func (o *overriddenTool) AcceptsRawInput() bool {
	if r, ok := o.CoreTool.(interface{ AcceptsRawInput() bool }); ok {
		return r.AcceptsRawInput()
	}
	return false
}

func (o *overriddenTool) Call(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	return o.CoreTool.Call(ctx, injectDefaults(in, o.schema), tc)
}

// injectDefaults fills scalar parameter defaults declared in the (possibly edited)
// schema into the input JSON whenever the model omitted the field or left it empty/
// null. Structure (names/types/required) is untouched — only缺省值 are merged in.
func injectDefaults(in json.RawMessage, schema map[string]any) json.RawMessage {
	defs := scalarDefaults(schema)
	if len(defs) == 0 {
		return in
	}
	m := map[string]json.RawMessage{}
	if len(in) > 0 {
		if err := json.Unmarshal(in, &m); err != nil {
			return in // non-object input: don't touch it
		}
	}
	changed := false
	for k, dv := range defs {
		if cur, ok := m[k]; !ok || isEmptyJSON(cur) {
			m[k] = dv
			changed = true
		}
	}
	if !changed {
		return in
	}
	b, err := json.Marshal(m)
	if err != nil {
		return in
	}
	return b
}

// scalarDefaults extracts properties[k]["default"] for scalar params (string/
// integer/number/boolean). Array/object defaults are skipped: merging them is
// ambiguous and not worth the surprise.
func scalarDefaults(schema map[string]any) map[string]json.RawMessage {
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		return nil
	}
	out := map[string]json.RawMessage{}
	for name, raw := range props {
		p, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		dv, ok := p["default"]
		if !ok || dv == nil {
			continue
		}
		switch p["type"] {
		case "string", "integer", "number", "boolean":
			if b, err := json.Marshal(dv); err == nil {
				out[name] = b
			}
		}
	}
	return out
}

func isEmptyJSON(raw json.RawMessage) bool {
	s := string(raw)
	return s == "null" || s == `""`
}
