package agent

import (
	"bytes"
	"text/template"
	"time"
)

// PromptOverride, if set, returns the stored system-prompt template for an agent
// key and whether one exists. The server wires it to the PG agent_prompts table.
// When nil or no override exists, agents use their built-in default prompt — so
// behavior is identical until a user edits a prompt in the UI.
var PromptOverride func(agentKey string) (string, bool)

// Prompt-variable structs — fields mirror each agent's catalog (docs §5a) so a
// user template referencing a catalog variable renders; referencing anything else
// fails template execution and falls back to the built-in default.
type PlannerVars struct{ Goal, Scope, AssetSummary, DataDir, Now string }
type WorkerVars struct{ ProxyAddr, WorkerName, DataDir, Now string }

// InstrumentView 是标的的只读视图（模板变量 {{.Instrument}} 的展开源）。
//
// 为什么单独一个类型而不是直接用 db.Instrument：模板只该看到「给 LLM 判断用的
// 那几个字段」，而不是 DB 行的全部（含 active/created_at 这类内部列）。
// 而且 db.Instrument 的 Name 是 *string —— 模板里 {{.Instrument.Name}} 会渲染成
// 指针地址，必须转成 string。
type InstrumentView struct {
	Symbol     string
	Name       string
	AssetClass string
	Market     string
	Currency   string
	Exchange   string
}

// AnalystVars 是分析师类角色（analyst_*）的模板变量。
//
// 与 WorkerVars 完全不同：WorkerVars 是执行上下文（ProxyAddr/DataDir/Now ——
// ProxyAddr 是历史遗留字段，金融场景下由全球代理统一注入，不参与分析逻辑），
// AnalystVars 是金融分析上下文（标的/行情/指标/证据链）。
//
// # 大数据变量的双层设计（Bars / BarsJSON）
//
//   - Bars 是**摘要**（"20 根日线（2026-09-28 ~ 2026-10-26）"），放「可用输入」表格
//   - BarsJSON 是**全量**（json 数组），放末尾「完整数据」节
//
// 为什么拆开：表格单元格里塞 3KB JSON 会把 markdown 表格撑断（多行内容会
// 终止表格），而 LLM 判断趋势又必须读到具体数值。拆成「摘要给人快速理解规模」
// + 「全量供实际计算」两层，两边都满足。
type AnalystVars struct {
	Instrument InstrumentView
	AsOf       string

	Bars     string // 摘要，如 "20 根日线（2026-09-28 ~ 2026-10-26）"
	BarsJSON string // 全量 JSON 数组

	Indicators  string // 小数据，直接 JSON 原文
	SnapshotIDs []string
	DataGaps    []string
	PastContext string // 历史反思；当前恒为空（reflector 未实现），模板用 {{if}} 处理

	// ── analyst_news 专用（其余分析师恒为零值，模板不引用它们）──────────
	//
	// # 为什么放在 AnalystVars 而不是新建一个 NewsVars
	//
	// vars 的类型由 buildRoleVars 统一返回 any，renderSystem 再按模板字段取。
	// 三者（news / fundamental / market）共享 Instrument/AsOf/SnapshotIDs/
	// DataGaps/PastContext 六个字段 —— 拆成三个类型会让那六个字段和三份
	// 构造逻辑重复三遍，而它们的数据来源完全相同（payload + fetchBars 那条链）。
	//
	// 代价是「类型上不阻止填错字段」：market 的 vars 里也有 News 字段。
	// 这一点由模板静态校验兜底（validateTemplateFields 只检查「模板引用的
	// 字段是否存在」，不检查「该角色该不该有它」），且各 prompt 的可用输入
	// 表格是三份独立文本，不会互相串用。见 prompt_render_test.go。
	//
	// 双层设计同 Bars/BarsJSON：摘要进表格，全量放末尾独立节。
	News      string // 摘要，如 "12 条新闻（财新 8 / 财联社 4）"
	NewsJSON  string // 全量 JSON 数组
	Macro     string // 宏观序列摘要
	MacroJSON string // 宏观序列全量

	// ── analyst_fundamental 专用 ──────────────────────────────────────
	//
	// 报表矩阵（科目 × 报告期）最多 40 行 × 5 列，塞表格会撑断 —— 同样拆两层。
	Fundamentals     string // 摘要，如 "40 行 × 4 期（2026-09-30 最新）"
	FundamentalsJSON string // 全量 JSON
	Filings          string // 申报文件摘要；通道未接入时是显式说明而不是空串
	FilingsJSON      string // 申报文件全量

	// ── 东方财富妙想（MX）补充数据 ────────────────────────────────────
	//
	// 这两个来源与上面的 akshare 数据**不重叠**：妙想是自然语言问句接口，
	// 能拿到 akshare 结构化 endpoint 给不了的东西（研报评级、资金流、
	// 「近三年毛利率」这类自由口径），也覆盖 akshare 财报通道缺失的港股/美股。
	//
	// 为什么同样是双层：妙想的表是动态的（列数不定），且资讯 content 实测
	// 可达数千字 —— 塞进 markdown 表格单元格必然撑断表格。
	//
	// 为什么字段放这里而不是新建 MXNewsVars / MXDataVars：与 News/Fundamentals
	// 同一取舍（见上方注释）—— 共享 6 个通用字段的构造逻辑不该复制三份。
	MXNews     string // 妙想资讯摘要，如 "8 条（公告 2 / 研报 3 / 新闻 3）"
	MXNewsJSON string // 妙想资讯全量（含 content/type/rating/institution）
	MXData     string // 妙想问句结果摘要，如 "2 张表（贵州茅台(600519.SH)...）"
	MXDataJSON string // 妙想问句结果全量（tables/columns/rows）
}

// CryptoSentimentVars 是加密情绪分析师的变量。
//
// # 为什么不用 AnalystVars
//
// 股票版情绪看的是 .Sentiment（score/sample_count/source），
// 加密版看的是 .Derivatives（funding_rate/open_interest/long_short_ratio）——
// **不同的数据结构，不是「同一个结构少几个字段」**。
//
// 架构 §4.1 明确要求独立版本：股票版 prompt 的示例全是 Reddit/StockTwits/
// 分析师评级，对加密是误导（模型会照着示例去找加密不存在的东西，
// 或者用价格动量冒充情绪）。
type CryptoSentimentVars struct {
	Instrument InstrumentView
	AsOf       string

	// Derivatives 是资金费率 + 持仓量的 JSON（来自 get_crypto_flow）。
	Derivatives string
	// News 是新闻摘要。get_news 工具尚未实现，当前恒为空 ——
	// 模板必须用 {{if}} 处理，并明确「无数据源 ≠ 无新闻」。
	News string

	SnapshotIDs []string
	DataGaps    []string
	PastContext string
}

// DebateVars 是辩论双方共用的变量（bull/bear 同一结构）。
//
// bull 与 bear 的差异在 **prompt 文本**（一个构造做多论证、一个针对多头反驳），
// 不在变量集 —— 两者都需要读同样的事实、跳过维度、轮次信息。
type DebateVars struct {
	Instrument InstrumentView
	AsOf       string

	// AnalystFacts 是**摘要**（"4 份事实（analyst_market / ...）"），放「可用输入」表格。
	// AnalystFactsJSON 是**全量** JSON 数组，放末尾「完整证据」节。
	//
	// 双层设计同 AnalystVars.Bars/BarsJSON：表格单元格塞 JSON 数组会撑断表格
	// （多行内容终止 markdown 表格），而 LLM 判断又需要读到具体数值。
	AnalystFacts     string
	AnalystFactsJSON string
	// SkippedAnalysts 是按 asset_class 裁掉的分析师（本就不存在该维度）。
	//
	// 与「跑了但失败」不同：Skipped 是设计如此，不该降低置信度；
	// 失败的分析师会出现在 join.payload.failed_upstreams，那是另一条通道。
	SkippedAnalysts []string

	Round     int
	MaxRounds int
	// PriorRoundSummary 是上一轮的辩论摘要。第 1 轮为空（用 {{if}} 处理）。
	PriorRoundSummary string
}

// ResearchManagerVars 是研究经理的变量。
//
// 它的产出是一条**裁决**（conclusion: bull_wins|bear_wins|draw|inconclusive），
// 那是可证伪的主张不是客观事实，所以用 emit_thesis 而不是 emit_fact。
type ResearchManagerVars struct {
	// 三个大数据变量都是双层：摘要放表格，全量放末尾。
	AnalystFacts         string // 摘要
	AnalystFactsJSON     string // 全量
	DebateTranscript     string // 摘要
	DebateTranscriptJSON string // 全量
	// SkippedAnalysts 是按 asset_class 裁掉的分析师（本就不存在该维度）。
	SkippedAnalysts []string
	DataGaps        []string
}

// TradingSessionView 是交给 trader 的交易时段视图。
//
// # 为什么不是直接用 market_hours.Session
//
// Session 只有 Open/Close（两个 time.Time），没有「当前是否开市」和
// 「下一个开盘时间」—— 而 trader 需要这两个来判断「立即执行还是等开盘」。
//
// # NextOpen 的已知偏差
//
// 用 AddTradingDays(market, today, 1) + SessionAt 算下一交易日。
// **A 股调休补班表在 W3-2 有已知缺口**（只覆盖节假日不覆盖调休上班），
// 所以 A 股在调休周时 NextOpen 可能偏差一天。这不是 trader 的错，是日历数据缺口。
type TradingSessionView struct {
	Open     string // RFC3339
	Close    string // RFC3339；闭市时为空
	IsOpen   bool
	NextOpen string // IsOpen=false 时有值；否则空
	Timezone string // IANA 名，如 "America/New_York"
}

// TraderVars 是交易员的变量。
type TraderVars struct {
	Instrument InstrumentView
	AsOf       string

	// ResearchConclusion 是研究经理的裁决（摘要）。
	// ResearchConclusionJSON 是全量（放末尾，让 trader 能读到完整推理链）。
	ResearchConclusion     string
	ResearchConclusionJSON string
	LastPrice              string
	TradingSession         TradingSessionView
	// SessionError 是 SessionAt 的错误（非空表示交易时段无法确定）。
	//
	// 与 IsOpen=false 不同：那表示「确定不在交易时段」，这表示「不知道是否在交易时段」。
	// 混同会让 trader 在日历故障时仍假设「等下一次开盘」，而实际上可能正在盘中。
	//
	// 这是「缺失 ≠ 零」在时间维度上的体现。
	SessionError string

	// AccountMode 只影响措辞，不改变行为。
	// paper 模式下不该说「即将下单」；实盘模式下输出仍是意图不是指令。
	AccountMode string

	// AccountID 是执行账户主键（exec_accounts.id），authorize_order 需要它。
	//
	// # 为什么由系统注入而不是让 trader 查
	//
	// trader 的工具集里没有「列账户」的能力（那是执行域，不是分析域）。
	// 而 authorize_order 的入参必须有 account_id —— 不注入的话 trader
	// 只能瞎猜或干脆放弃（实测：它报「无下单通道」然后停止）。
	//
	// 0 表示「未配置账户」，此时 prompt 会明确告知，trader 只出信号不建草案。
	AccountID int64

	// AccountRef 是账户的可读标识（如 execdemo-account），仅用于提示文案。
	AccountRef string

	// MinOrderAmount 是该通道的最小下单金额（计价币），用于约束 qty。
	//
	// # 为什么需要（2026-11-06 实测）
	//
	// OKX 现货最小下单额是 1 USDT 等值，实测 10 USDT 才稳。trader 若给出
	// 0.0001 BTC（约 8 USDT）这种 qty，会被交易所拒且错误码笼统
	// （"All operations failed"），排查成本极高。
	//
	// 0 表示无限制（非 OKX 通道或未配置）。
	MinOrderAmount float64
}

// RiskVars 是三方风控共用的变量。
//
// 激进/中性/保守的差异在 prompt 文本（三个不同的审视视角），不在变量集 ——
// 三者都要读同样的提案、结论、标的与跳过维度。
type RiskVars struct {
	TraderProposal         string
	TraderProposalJSON     string
	ResearchConclusion     string
	ResearchConclusionJSON string
	Instrument             InstrumentView
	SkippedAnalysts        []string
	Round                  int
}

// PMVars 是组合经理的变量。
type PMVars struct {
	// PM 有 3 个大数据输入，各自双层。渲染时三个全量节分开小标题
	// （见 prompt 的「附：完整数据」），避免混成一坨。
	TraderProposal        string
	TraderProposalJSON    string
	RiskDebateSummary     string
	RiskDebateSummaryJSON string
	AnalystFacts          string
	AnalystFactsJSON      string
	SkippedAnalysts       []string
	DataGaps              []string
	Instrument            InstrumentView
}
type MainVars struct{ Goal, AssetSummary, FindingsSummary, DataDir, Now string }
type GoalsVars struct{ EngagementDescription, DataDir, Now string }

// nowStr is the server-local wall-clock string exposed as the universal {{.Now}}
// prompt variable. renderSystem runs on every agent turn/round, so this is fresh
// each run — a prompt can subtract it from a fixed start stamp to reason about
// elapsed time (e.g. a timed benchmark's "last N hours" window).
func nowStr() string { return time.Now().Format("2006-01-02 15:04:05 MST") }

// renderSystem returns the rendered system-prompt BODY (段 [A]) for agentKey.
// Precedence: the DB-stored template (if any) over the built-in default template
// (def). BOTH are Go templates now — the built-in default is seeded into the DB
// verbatim, so the two paths render identically until a user edits the prompt.
// Rendering always runs (def used to be pre-substituted plain text; it is now a
// {{.Var}} template like the DB one). On any render error we fall back to the
// default template, then to the raw default string — an agent never starts with a
// half-rendered prompt. Callers append the code-owned tail (trafficTool / 中间产物
// 输出规约) AFTER this, so those can't be edited away via the DB body.
func renderSystem(agentKey, def string, vars any) string {
	tmpl := def
	if PromptOverride != nil {
		if t, ok := PromptOverride(agentKey); ok && t != "" {
			tmpl = t
		}
	}
	if out, err := renderTmpl(tmpl, vars); err == nil {
		return out
	}
	// DB template broke (e.g. references an out-of-catalog var) → code default.
	if out, err := renderTmpl(def, vars); err == nil {
		return out
	}
	return def
}

func renderTmpl(tmpl string, vars any) (string, error) {
	t, err := template.New("p").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, vars); err != nil {
		return "", err
	}
	return b.String(), nil
}
