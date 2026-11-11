package agent

// W5 金融角色的 vars 构造与 prompt 渲染。
//
// # 这个文件解决的问题
//
// worker 原本只有一种身份（渗透执行者），prompt 与 vars 都是固定的。
// W5 引入 10 个金融角色后，同一个 worker 执行器要按 intent 的 role 加载不同的
// prompt 和不同的 vars 类型（WorkerVars vs AnalystVars vs DebateVars ...）。
//
// # 为什么 vars 构造器自己取数（而不是让 LLM 调工具）
//
// 证据链一致性：vars 里的 .Bars 和 emit_fact 的 snapshot_ids 必须指向同一份数据。
// 如果 .Bars 由构造器取、snapshot_id 由 LLM 调 get_kline 拿，两者可能是两次
// 独立取数（不同时刻、不同参数），导致 snapshot 与引用它的 fact 对不上。
//
// 所以 fetchBars 内部一次完成：调 MarketClient → captureKlineSnapshot → 拿到
// (数据, snapshot_id)。两者同源，证据链不会分裂。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
)

// 数据未接入时的显式说明。
//
// **为什么不是空字符串**：空字符串会让 LLM 以为「数据是空的（比如今天没有 K 线）」，
// 而明确的说明让它知道「这一层没接通」。两者会导致完全不同的输出 ——
// 前者可能编造「K 线数据缺失但趋势看起来向上」，后者会老实说「无法分析」。
const (
	notWiredBars = "（行情数据尚未接入 —— 本次分析无 K 线可用，请在 rationale 里明确说明此限制，不要推测走势）"
	notWiredGap  = "行情数据未接入（W5 执行器尚未接入 MarketClient）"
	notWiredInd  = `{"_unavailable":"技术指标未计算（行情数据未接入）"}`

	// 新闻 / 财报 / 申报文件的「未接入」显式说明。
	//
	// **为什么都不能是空串**：空串在 prompt 里渲染成「新闻条目：」（后面什么都没有），
	// LLM 会读成「今天没有新闻」——那是分析结论；而真相是「执行器没去取」——
	// 那是数据状态。两者导致完全不同的输出：前者可能给 neutral 并声称「无消息面」，
	// 后者会给 unavailable 并写进 data_gaps。
	notWiredNews        = "（新闻数据尚未接入 —— 本次分析无新闻可用。这是数据状态，不是「今天没有消息」。请在 data_gaps 写明并给 stance=unavailable）"
	// akshare 侧只提供 A 股结构化财报；us/hk 由妙想（.MXData）承担。
	// 所以这里**不能**无条件命令 stance=unavailable —— 那会让已有妙想报表的
	// 美股/港股分析被强行判成「无数据」。条件写在文案里，由模型按 .MXData 判定。
	notWiredFundamental = "（本轮 akshare 结构化财报无数据。若下方「妙想财务数据」也为空，才是真正的缺口，" +
		"请写进 data_gaps 并给 stance=unavailable；若妙想有表，用它分析，**不要**判 unavailable）"
	// us/hk 专用措辞：这不是故障也不是能力缺口，是「该市场财报在本系统走妙想」。
	notAkshareFundamentalUS = "（akshare 不提供 %s 结构化财报 —— 该市场财报请读「妙想财务数据」；" +
		"妙想也为空时才算数据缺口）"
	// 申报文件通道（SEC EDGAR / 巨潮公告）**确实还没接**。
	// 与上面两条的区别：这条是长期能力缺口，不是装配问题 —— 所以要写明原因，
	// 让 LLM 把它归到 source_unavailable 而不是 not_applicable。
	notWiredFilings = "（申报文件通道未接入：SEC EDGAR / 巨潮公告均未实现。这是能力缺口，不是「该公司近期没有申报文件」）"

	// 妙想（MX）通道的「未接入」说明。
	//
	// 同样不能是空串：妙想是**补充**通道，未配置是常态（没设 MX_APIKEY）。
	// 空串会让 LLM 以为「妙想返回了零条资讯」，进而得出「今天没有公告/研报」——
	// 那是分析结论；真相是这条通道没开。
	notWiredMX = "（东方财富妙想通道未启用 —— 未配置 MX_APIKEY。这不是「没有数据」，请在 data_gaps 写明并改用其他通道）"
)

// 妙想数据进 prompt 的上限。资讯 content 实测可达数千字，全塞进去会挤掉
// 其他证据；20 条足以覆盖一个标的近期事件面。
const (
	mxNewsVarsLimit  = 20
	mxNewsVarsRunes  = 600 // 单条 content 截断长度
	mxDataVarsTables = 5   // 问句结果最多带几张表进 prompt
)

// fillNotWiredVars 给无 MarketClient 的场景填上「未接入」说明。
func fillNotWiredVars(vars *AnalystVars, agentKey string) {
	switch agentKey {
	case RoleAnalystNews:
		vars.News, vars.NewsJSON = notWiredNews, "[]"
		vars.Macro, vars.MacroJSON = notWiredNews, "[]"
		vars.MXNews, vars.MXNewsJSON = notWiredMX, "[]"
		vars.DataGaps = []string{"新闻与宏观数据未接入（执行器未拿到 MarketClient）"}
	case RoleAnalystFundamental:
		vars.Fundamentals, vars.FundamentalsJSON = notWiredFundamental, "[]"
		vars.Filings, vars.FilingsJSON = notWiredFilings, "[]"
		vars.MXData, vars.MXDataJSON = notWiredMX, "[]"
		vars.DataGaps = []string{"财务数据未接入（执行器未拿到 MarketClient）"}
	default:
		vars.Bars, vars.BarsJSON = notWiredBars, "[]"
		vars.DataGaps = []string{notWiredGap}
	}
}

// analystVarsStub 构造「数据未接入」状态的 AnalystVars。
//
// 用于 ctx 里没有 MarketClient 的场景（单元测试、未装配的部署）。
func analystVarsStub(ctx context.Context, intent *db.Node) AnalystVars {
	instID := instrumentIDFromPayload(intent)
	var store *db.ExplorationStore
	if ts, ok := ExplorationStoreFrom(ctx); ok {
		store = ts
	}
	inst := instrumentFromPayload(intent, store)

	// 两种「标的缺失」必须分开说明 —— 它们的排查方向完全不同：
	//   instID == 0 → payload 里没有 id，或类型漂移读成 0（配置/代码问题）
	//   instID != 0 → id 有，但执行器还没接 DB 查询（未实现的代码）
	// 混成一条「标的未知」会让运维分不清该查配置还是查代码。
	var gaps []string
	if instID == 0 {
		gaps = append(gaps, "标的未知（payload 里无 instrument_id，或类型不符）")
	} else {
		gaps = append(gaps, fmt.Sprintf(
			"标的信息未加载（instrument_id=%d，执行器尚未接入 instruments 表查询）", instID))
	}
	gaps = append(gaps, notWiredGap)

	return AnalystVars{
		Instrument:  inst,
		AsOf:        time.Now().UTC().Format("2006-01-02"),
		Bars:        notWiredBars,
		BarsJSON:    "[]",
		Indicators:  notWiredInd,
		SnapshotIDs: nil,
		DataGaps:    gaps,
		PastContext: "",
	}
}

// instrumentIDFromPayload 从 payload 里安全取 instrument_id。
//
// # 为什么要处理三种类型
//
// materialize 写入时是 int64，但 JSONB 读回来经 encoding/json 反序列化后
// 数字一律变成 **float64**。直接 `p["instrument_id"].(int64)` 会类型断言失败
// 返回零值 —— **静默变成「无标的」，没有任何报错**。
//
// 三种都处理：float64（从 DB 读回的标准形态）、int64（内存中直接构造时）、
// json.Number（若调用方启用了 UseNumber）。
//
// 返回 0 表示「无标的」—— **0 是「缺失」，不是「id 为 0 的某个标的」**。
// 这是「缺失 ≠ 零」在 ID 上的体现。
func instrumentIDFromPayload(intent *db.Node) int64 {
	if intent == nil || len(intent.Payload) == 0 {
		return 0
	}
	var p map[string]any
	if err := json.Unmarshal(intent.Payload, &p); err != nil {
		return 0
	}
	return int64FromAny(p["instrument_id"])
}

// instrumentFromPayload 从 intent 的 payload 里读标的。
//
// Expand 把 instrument_id / asset_class 写进了每个节点的 basePayload
// （见 pipeline.go 的 basePayload），所以 intent 自带这两个字段。
//
// **为什么不查 DB**：instrument_id 只是个 id，symbol/name 需要查 instruments 表；
// 但 v1 的 payload 里只有 id 与 asset_class，没有 symbol。执行器（第三步）拿到
// DB 句柄后再补。当前符号为空时，prompt 里 {{.Instrument.Symbol}} 渲染成空串 ——
// 这是已知限制，**比填假值好**（假 symbol 会让 LLM 分析错标的）。
// instrumentFromPayload 从 payload + DB 组装标的视图。
//
// # 三级降级
//
//  1. 有 instrument_id 且能查 DB → 完整信息（symbol/name/asset_class/market/currency/exchange）
//  2. 有 instrument_id 但查不到（DB 无此 id / store 为 nil）→ 至少填 asset_class 推出的 market
//  3. 无 instrument_id → 全空（调用方会在 DataGaps 里说明「标的未知」）
//
// **第 2 级为什么至少填 market**：market 决定用哪个行情通道（crypto→OKX，其余→akshare）。
// 即使查不到标的详情，知道 market 仍然能让取数走对通道。
func instrumentFromPayload(intent *db.Node, store *db.ExplorationStore) InstrumentView {
	if intent == nil || len(intent.Payload) == 0 {
		return InstrumentView{}
	}
	var p map[string]any
	if err := json.Unmarshal(intent.Payload, &p); err != nil {
		return InstrumentView{}
	}

	// 先填能从 payload 直接得到的（asset_class / 由它推出的 market）
	view := InstrumentView{}
	if v, ok := p["asset_class"].(string); ok {
		view.AssetClass = v
	}
	if view.AssetClass == AssetClassCrypto {
		view.Market = "crypto"
	}

	// 再尝试用 instrument_id 查全量信息
	id := int64FromAny(p["instrument_id"])
	if id <= 0 || store == nil {
		return view
	}
	inst, err := store.DB().Instruments().GetInstrumentByID(id)
	if err != nil || inst == nil {
		// 降级：保留 payload 推出的一半，不返回全空
		return view
	}
	v := InstrumentView{
		Symbol:     inst.Symbol,
		AssetClass: inst.AssetClass,
		Market:     inst.Market,
	}
	if inst.Name != nil {
		v.Name = *inst.Name
	}
	if inst.Currency != nil {
		v.Currency = *inst.Currency
	}
	if inst.Exchange != nil {
		v.Exchange = *inst.Exchange
	}
	return v
}

// buildAnalystVars 构造分析师类角色的 vars。
//
// 分两条路径：
//   - ctx 有 MarketClient → 真实取数（含 snapshot_id）
//   - ctx 无 MarketClient → stub（明确说明「未接入」）
func buildAnalystVars(ctx context.Context, intent *db.Node) AnalystVars {
	cli, ok := MarketClientFrom(ctx)
	if !ok {
		return analystVarsStub(ctx, intent)
	}
	vars := analystVarsStub(ctx, intent)
	vars.DataGaps = nil
	vars.Indicators = `{}`

	bars, barsJSON, snapIDs, gaps := fetchBarsForVars(ctx, cli, intent)
	if len(bars) > 0 {
		vars.Bars = bars
		vars.BarsJSON = barsJSON
	}
	if len(snapIDs) > 0 {
		vars.SnapshotIDs = snapIDs
	}
	if len(gaps) > 0 {
		vars.DataGaps = gaps
	}
	return vars
}

// buildAnalystRoleVars 按**具体**分析师角色构造 vars（analyst_news / analyst_fundamental）。
//
// # 为什么不能只有 buildAnalystVars
//
// buildAnalystVars 只取 K 线 —— 对 analyst_market 正确，但对 news/fundamental
// 是错的：它们两个的模板里根本没有 {{.Bars}}，却需要 {{.News}} / {{.Fundamentals}}。
// 若只走 buildAnalystVars，这两个角色的 prompt 会渲染出空字符串，LLM 拿到
// 「新闻条目：（空）」会当成「今天没有新闻」，而实际是执行器没去取。
//
// # 为什么按 role 分派而不是「三类数据都取一遍」
//
// 取数是 HTTP 调用（news 15s / fundamental 20s 超时）。analyst_market 不需要
// 新闻和财报，给它取等于让每轮分析多两次可能失败的远程调用，还多两条
// 本不该出现的 data_gaps（「新闻取不到」对技术面分析师是无关噪声）。
//
// 返回的 vars **总是包含** Instrument/AsOf/DataGaps/PastContext —— 三条路径共用。
func buildAnalystRoleVars(ctx context.Context, agentKey string, intent *db.Node) AnalystVars {
	vars := analystVarsStub(ctx, intent)
	// stub 的 DataGaps 含「行情未接入」。news/fundamental 不依赖行情，
	// 保留它会让它们把「没接 K 线」写成自己的缺口 —— 那是别的角色的维度。
	vars.DataGaps = nil
	vars.Indicators = `{}`

	cli, ok := MarketClientFrom(ctx)
	if !ok {
		// 无 client：把「未接入」写进各自的数据字段，而不是留空。
		// 空字符串会让 LLM 以为「数据是空的」，明确说明让它知道「这层没通」。
		fillNotWiredVars(&vars, agentKey)
		return vars
	}

	switch agentKey {
	case RoleAnalystNews:
		news, newsJSON, macro, macroJSON, snapIDs, gaps := fetchNewsAndMacroForVars(ctx, cli, intent)
		vars.News, vars.NewsJSON = news, newsJSON
		vars.Macro, vars.MacroJSON = macro, macroJSON
		vars.SnapshotIDs = snapIDs
		vars.DataGaps = gaps
		// 妙想资讯：与财新/财联社互补（公告 + 研报评级是那两个源没有的维度）。
		mxSummary, mxJSON, mxGaps := fetchMXNewsForVars(ctx, intent)
		vars.MXNews, vars.MXNewsJSON = mxSummary, mxJSON
		vars.DataGaps = uniqueStrings(append(vars.DataGaps, mxGaps...))
	case RoleAnalystFundamental:
		f, fJSON, summary, snapIDs, gaps := fetchFundamentalForVars(ctx, cli, intent)
		vars.Fundamentals, vars.FundamentalsJSON = summary, fJSON
		_ = f
		vars.SnapshotIDs = snapIDs
		vars.DataGaps = gaps
		// 申报文件通道未接入 —— 必须显式说明，不能让 LLM 以为「近期没有公告」。
		vars.Filings = notWiredFilings
		vars.FilingsJSON = "[]"
		// 妙想问句：akshare 财报只有 A 股 + 白名单科目；妙想能问自由口径，
		// 也覆盖港股/美股（那边 akshare 返回 NO_DATA）。
		mxSummary, mxJSON, mxGaps := fetchMXQueryForVars(ctx, intent, fundamentalQuestion(ctx, intent))
		vars.MXData, vars.MXDataJSON = mxSummary, mxJSON
		vars.DataGaps = uniqueStrings(append(vars.DataGaps, mxGaps...))
	default:
		// analyst_market / analyst_sentiment_crypto 等：只要 K 线。
		bars, barsJSON, snapIDs, gaps := fetchBarsForVars(ctx, cli, intent)
		if len(bars) > 0 {
			vars.Bars = bars
			vars.BarsJSON = barsJSON
		}
		vars.SnapshotIDs = snapIDs
		vars.DataGaps = gaps
	}
	return vars
}

// fetchBarsForVars 取日线并落快照，返回 (摘要, 全量JSON, snapshot id, gaps)。
//
// **取数与落快照在同一次调用里** —— 这是证据链一致性的要求（见文件头注释）。
func fetchBarsForVars(ctx context.Context, cli MarketClient, intent *db.Node) (
	summary, barsJSON string, snapIDs []string, gaps []string) {
	var store *db.ExplorationStore
	if ts, ok := ExplorationStoreFrom(ctx); ok {
		store = ts
	}
	view := instrumentFromPayload(intent, store)
	symbol, market := view.Symbol, view.Market
	if strings.TrimSpace(symbol) == "" {
		return "", "", nil, []string{"标的代码未知（intent payload 里没有 symbol）"}
	}
	if market == "" {
		market = "cn"
	}

	meta, ok := marketToolMetas["get_kline"]
	if !ok {
		return "", "", nil, []string{"get_kline 元数据缺失"}
	}
	cctx, cancel := context.WithTimeout(ctx, meta.Timeout)
	defer cancel()

	bars, _, err := fetchKlineWithFallback(cctx, cli, symbol, market, "daily", 250, meta)
	if err != nil {
		// 取数失败要显式进 DataGaps —— prompt 的「缺失数据处理」会据此降级。
		return "", "", nil, []string{
			fmt.Sprintf("行情取数失败（%v）—— 这是数据源故障，不是「今天没数据」", err),
		}
	}
	closed := closedBars(bars)
	if len(closed) == 0 {
		return "", "", nil, []string{"全部 K 线未收盘（is_partial=true），技术面分析不可用"}
	}

	first, last := closed[0].TradingDate, closed[len(closed)-1].TradingDate
	summary = fmt.Sprintf("%d 根日线（%s ~ %s）", len(closed), first, last)

	if raw, mErr := json.Marshal(closed); mErr == nil {
		barsJSON = string(raw)
	}

	// 落快照 —— 与 get_kline 工具走同一个 captureKlineSnapshot，
	// 保证 vars 里的数据与 snapshot_id 指向同一次取数。
	if store, ok := ExplorationStoreFrom(ctx); ok {
		if id, sErr := captureKlineSnapshot(store.DB(), symbol, market, "daily",
			250, ChannelAkshareCN, bars); sErr == nil && id > 0 {
			snapIDs = []string{fmt.Sprintf("%d", id)}
		}
	}
	return summary, barsJSON, snapIDs, gaps
}

// fetchNewsAndMacroForVars 取新闻 + 一条宏观序列并各自落快照。
//
// # 为什么宏观只取一条（cpi）而不是全部
//
// 每条宏观序列进 prompt 都要占一行表格 + 一段 JSON。全取 7 个（cpi/ppi/pmi/
// gdp/m2/shibor/lpr）会把 prompt 撑到几 KB 的宏观噪声，而新闻分析师真正需要
// 的是「当前利率/通胀环境」这一个背景 —— cpi 是最直接的那个。
// 需要别的指标时模型自己调 get_macro（已绑定给它）。
//
// # 失败处理
//
// 新闻与宏观**互相独立**：一个失败不能让另一个也变成不可用。所以各自 try，
// 失败各自写一条 gap。只有两者都失败时上层才会给 stance=unavailable。
func fetchNewsAndMacroForVars(ctx context.Context, cli MarketClient, intent *db.Node) (
	newsSummary, newsJSON, macroSummary, macroJSON string, snapIDs []string, gaps []string) {
	newsJSON = "[]"
	macroJSON = "[]"

	ak, ok := cli.(*AkshareClient)
	if !ok {
		return notWiredNews, newsJSON, notWiredNews, macroJSON, nil,
			[]string{"新闻/宏观通道未接入（MarketClient 不是 akshare 实现）"}
	}

	view := instrumentFromPayload(intent, storeFrom(ctx))
	symbol, market := view.Symbol, view.Market
	if market == "" {
		market = "cn"
	}
	if view.AssetClass == AssetClassCrypto {
		// crypto 不展开本角色（analystsFor 已保证），误调时也明确说清楚。
		return notWiredNews, newsJSON, notWiredNews, macroJSON, nil,
			[]string{"加密资产无新闻/宏观通道（财新与财联社都是股票市场文本源）"}
	}

	// ---- 新闻 ----
	if meta, ok := marketToolMetas["get_news"]; ok {
		nctx, cancel := context.WithTimeout(ctx, meta.Timeout)
		// symbol 可为空：为空时取该市场全量新闻，由 prompt 的判断框架去筛相关性。
		items, err := ak.FetchNews(nctx, symbol, market, 20)
		cancel()
		if err != nil {
			if IsNoDataError(err) {
				// 「该标的近期无相关新闻」是**合法结论**，但它意味着
				// 个股维度的输入是空的 —— 必须进 DataGaps，让 prompt 走
				// 「.News 为空但 .Macro 非空」那条降级分支。
				gaps = append(gaps, fmt.Sprintf("个股新闻：%v（这是「查得到但没有」，不是源故障）", err))
			} else {
				gaps = append(gaps, fmt.Sprintf("新闻取数失败（%v）—— 这是数据源故障", err))
			}
		} else {
			newsSummary = fmt.Sprintf("%d 条新闻（%s）", len(items), newsSourcesBrief(items))
			if raw, mErr := json.Marshal(items); mErr == nil {
				newsJSON = string(raw)
			}
			if store, ok := ExplorationStoreFrom(ctx); ok {
				if id, sErr := captureNewsSnapshot(store.DB(), symbol, market, items); sErr == nil && id > 0 {
					snapIDs = append(snapIDs, fmt.Sprintf("%d", id))
				}
			}
		}
	}

	// ---- 宏观（一条：cpi）----
	// 宏观走 cli.FetchMacro（MarketClient 接口方法），不需要类型断言。
	if meta, ok := marketToolMetas["get_macro"]; ok {
		mctx, cancel := context.WithTimeout(ctx, meta.Timeout)
		macro, err := cli.FetchMacro(mctx, "cpi")
		cancel()
		if err != nil {
			gaps = append(gaps, fmt.Sprintf("宏观序列 cpi 取数失败（%v）", err))
		} else if macro != nil && len(macro.Points) > 0 {
			last := macro.Points[len(macro.Points)-1]
			macroSummary = fmt.Sprintf("%s（%s，最新一期 %s = %s）",
				macro.SeriesName, macro.Unit, last.Period, orDash(last.Value))
			if raw, mErr := json.Marshal(macro); mErr == nil {
				macroJSON = string(raw)
			}
			if store, ok := ExplorationStoreFrom(ctx); ok {
				if id, sErr := captureMacroSnapshot(store.DB(), "cpi", macro); sErr == nil && id > 0 {
					snapIDs = append(snapIDs, fmt.Sprintf("%d", id))
				}
			}
		} else {
			gaps = append(gaps, "宏观序列 cpi 返回空序列")
		}
	}

	// 两个都没取到时，给一条合并说明 —— prompt 的「无消息处理」据此给 unavailable。
	if newsJSON == "[]" && macroJSON == "[]" {
		gaps = append(gaps, "本轮无任何新闻与宏观数据返回")
	}
	return newsSummary, newsJSON, macroSummary, macroJSON, snapIDs, uniqueStrings(gaps)
}

// fetchFundamentalForVars 取财报摘要并落快照。
//
// 返回 (*Fundamental, JSON 全量, 摘要, snapshot_ids, gaps)。第一个返回值
// 给调用方判空用（当前调用点用 _ 丢弃，保留是为了将来算同比要用它）。
func fetchFundamentalForVars(ctx context.Context, cli MarketClient, intent *db.Node) (
	*Fundamental, string, string, []string, []string) {
	ak, ok := cli.(*AkshareClient)
	if !ok {
		return nil, "[]", notWiredFundamental, nil,
			[]string{"财报通道未接入（MarketClient 不是 akshare 实现）"}
	}
	view := instrumentFromPayload(intent, storeFrom(ctx))
	symbol, market := view.Symbol, view.Market
	if strings.TrimSpace(symbol) == "" {
		return nil, "[]", notWiredFundamental, nil,
			[]string{"标的代码未知（intent payload 里没有 symbol）"}
	}
	if market == "" {
		market = "cn"
	}
	if view.AssetClass == AssetClassCrypto {
		return nil, "[]", notWiredFundamental, nil,
			[]string{"加密资产无财报（维度不存在，不是数据缺失）"}
	}

	meta, ok := marketToolMetas["get_fundamentals"]
	if !ok {
		return nil, "[]", notWiredFundamental, nil, []string{"get_fundamentals 元数据缺失"}
	}
	cctx, cancel := context.WithTimeout(ctx, meta.Timeout)
	defer cancel()

	f, err := ak.FetchFundamental(cctx, symbol, market, "abstract")
	if err != nil {
		if IsNoDataError(err) {
			// us/hk 不是故障也不是缺口 —— 这两个市场的结构化财报由妙想通道承担，
			// 而妙想数据在 vars.MXData 里（本函数管不到）。只有 cn 才是真「无数据」。
			if market == "us" || market == "hk" {
				return nil, "[]", fmt.Sprintf(notAkshareFundamentalUS, market), nil,
					[]string{fmt.Sprintf("akshare 无 %s 结构化财报（非故障；该市场财报见妙想财务数据）", market)}
			}
			return nil, "[]", notWiredFundamental, nil,
				[]string{fmt.Sprintf("财务数据不可用：%v（能力缺口，重试无用）", err)}
		}
		return nil, "[]", notWiredFundamental, nil,
			[]string{fmt.Sprintf("财务数据取数失败（%v）—— 这是数据源故障", err)}
	}

	summary := fmt.Sprintf("%d 行 × %d 期（最新报告期 %s）",
		len(f.Rows), len(f.Periods), firstOrDash(f.Periods))
	raw := "[]"
	if b, mErr := json.Marshal(f); mErr == nil {
		raw = string(b)
	}
	var snapIDs []string
	if store, ok := ExplorationStoreFrom(ctx); ok {
		if id, sErr := captureFundamentalSnapshot(store.DB(), f); sErr == nil && id > 0 {
			snapIDs = append(snapIDs, fmt.Sprintf("%d", id))
		}
	}
	return f, raw, summary, snapIDs, nil
}

// newsSourcesBrief 汇总新闻来源分布（"财新 8 / 财联社 4"）。
//
// 放在摘要里而不是让 LLM 自己数：两个源的**可信度与时效性不同**
// （财新偏深度、财联社偏快讯），分析师看到分布才知道这批证据的构成。
func newsSourcesBrief(items []NewsItem) string {
	counts := map[string]int{}
	var order []string
	for _, it := range items {
		s := it.Source
		if s == "" {
			s = "unknown"
		}
		if counts[s] == 0 {
			order = append(order, s)
		}
		counts[s]++
	}
	parts := make([]string, 0, len(order))
	for _, s := range order {
		label := s
		switch s {
		case "caixin":
			label = "财新"
		case "cls":
			label = "财联社"
		}
		parts = append(parts, fmt.Sprintf("%s %d", label, counts[s]))
	}
	return strings.Join(parts, " / ")
}

// uniqueStrings 去重并保序（多条 gap 可能描述同一件事）。
func uniqueStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func firstOrDash(ss []string) string {
	if len(ss) == 0 || strings.TrimSpace(ss[0]) == "" {
		return "未知"
	}
	return ss[0]
}

// isFinancialPrompt 判断 key 是否属于 W5 金融分析角色。
//
// 用前缀判断而不是白名单枚举：新增金融角色时不必改这个函数。
// 渗透类的 key（goals/planner/mainagent/worker/auto/pentest）都不匹配这些前缀。
func isFinancialPrompt(key string) bool {
	for _, p := range []string{
		"analyst_", "debate_", "research_manager", "trader",
		"risk_", "portfolio_manager",
		// screener 只在无标的任务里出现（阶段 0）。它也是金融角色 ——
		// 必须走同一条「不给 Bash/Write」的分支，否则它又会绕开
		// get_quote 自己写爬虫（那正是要根治的行为）。
		"screener",
	} {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// renderRolePrompt 按 agentKey 渲染金融角色的 system prompt。
//
// 返回 ("", false) 表示「不是金融角色」，调用方应走原来的 workerSystem 路径。
func renderRolePrompt(ctx context.Context, agentKey string, intent *db.Node) (string, bool) {
	if !isFinancialPrompt(agentKey) {
		return "", false
	}
	tmpl := BuiltinPromptSeeds()[agentKey]
	if tmpl == "" {
		return "", false // 有 role 但没有 prompt → 退回 worker 路径
	}

	vars := buildRoleVars(ctx, agentKey, intent)
	if vars == nil {
		// 不认识的金融角色 —— 退回 worker 路径（调用方会调 workerSystem）
		return "", false
	}
	return renderSystem(agentKey, tmpl, vars), true
}

// agentKeyForIntent 从 intent 的 payload.role 推导 agentKey。
//
// 返回 "worker" 表示走渗透路径。**渗透 intent 的 payload 里没有 role 字段**
// （addOneIntent 只写 summary + asset_ids），所以这里返回 "worker" ——
// 与改动前行为完全一致，零回归。
//
// 只有 MaterializeGraph 建的金融阶段节点才有 role（见 materialize.go），
// 那些节点的 role 值就是 agents.key（"analyst_market" / "debate_bull" ...）。
func agentKeyForIntent(intent *db.Node) string {
	const fallback = "worker"
	if intent == nil || len(intent.Payload) == 0 {
		return fallback
	}
	var p map[string]any
	if err := json.Unmarshal(intent.Payload, &p); err != nil {
		return fallback
	}
	role, _ := p["role"].(string)
	role = strings.TrimSpace(role)
	if role == "" {
		return fallback
	}
	return role
}

// ---------------------------------------------------------------------
// 东方财富妙想（MX）：给两个分析师补一层数据
// ---------------------------------------------------------------------
//
// # 为什么在 vars 构造器里调妙想，而不是只靠工具绑定
//
// 与 fetchBarsForVars 同一理由：vars 里出现的数据必须与分析师实际读到的一致。
// 只绑工具的话，模型要不要调、调几次、用什么问句全凭它当场发挥 ——
// 而 prompt 里「可用输入」表格的 .MXNews 会是空的，模型看到空表格
// 会以为「没有资讯」（分析结论），真相是「执行器没取」（数据状态）。
//
// 所以预取一份进 vars，工具保留给「追问某个具体口径」的场景。
//
// # 失败一律进 DataGaps，绝不静默
//
// 妙想是可选通道（MX_APIKEY 可能没配）。未配置、鉴权失败、无数据、上游故障
// 四种情况的**后续动作完全不同**，所以 gap 文案按错误码分别写。

// fundamentalQuestion 为基本面分析师拼一句妙想问句。
//
// # 为什么不问「这家公司的财务报表」
//
// 妙想是 LLM 解析问句，问得越泛越容易返回一堆无关表或直接无数据。
// 带上公司名 + 指标 + 时间范围时解析最准（实测）。
//
// # 为什么 us 只用 symbol，其它市场用 name + symbol
//
// 妙想对**中文名**解析最好，但对英文全名经常直接返回 0 张表（实测：
// 「Apple Inc AAPL最近三期…」→ 1 表，而「NVIDIA Corp NVDA最近三期…」→ 0 表，
// 同期「NVDA最近三期…」→ 2 表）。即英文名不是锚点而是噪声。
// 所以 us 只给 ticker（AAPL/DVDA 这类是妙想认的唯一稳定锚点），
// hk/cn 给「中文名 代码」，两个锚点互补。
func fundamentalQuestion(ctx context.Context, intent *db.Node) string {
	view := instrumentFromPayload(intent, storeFrom(ctx))
	label := fundamentalQuestionLabel(view.Market, view.Name, view.Symbol)
	if label == "" {
		return ""
	}
	return label + "最近三期营业收入、净利润、毛利率与资产负债率"
}

// fundamentalQuestionLabel 选问句里的标的锚点。纯函数，便于测。
//
// us 只留 ticker：实测英文公司名（NVIDIA Corp）会让妙想返回 0 张表，
// 而同一标的的 ticker（NVDA）返回 2 张表 —— 英文名是噪声不是锚点。
// cn/hk 用「中文名 代码」，代码去重（name 里已含代码则不重复拼）。
func fundamentalQuestionLabel(market, name, sym string) string {
	name, sym = strings.TrimSpace(name), strings.TrimSpace(sym)
	switch {
	case sym == "":
		return name
	case market == "us":
		return sym
	case name == "" || strings.EqualFold(name, sym):
		return sym
	}
	if strings.Contains(strings.ToUpper(name), strings.ToUpper(sym)) {
		return name
	}
	return name + " " + sym
}

// fetchMXNewsForVars 取妙想资讯进 vars。
//
// 返回 (摘要, 全量JSON, gaps)。任何失败都只写 gap，不返回 error ——
// 妙想是补充通道，它挂了不能让整个新闻分析不可用（财新/财联社仍在）。
func fetchMXNewsForVars(ctx context.Context, intent *db.Node) (string, string, []string) {
	cli := mxForVars(ctx)
	if cli == nil {
		return notWiredMX, "[]", nil // 未配置是常态，不写成 DataGap（否则每轮都报同一条噪声）
	}
	view := instrumentFromPayload(intent, storeFrom(ctx))
	label := strings.TrimSpace(view.Name)
	if label == "" {
		label = strings.TrimSpace(view.Symbol)
	}
	if label == "" {
		return notWiredMX, "[]", []string{"妙想资讯未取：标的名称与代码都未知，无法构造问句"}
	}
	if view.AssetClass == AssetClassCrypto {
		// 妙想没有加密数据。这是能力边界，不是故障。
		return "（妙想覆盖 A股/港股/美股/基金/债券，无加密资讯 —— 本维度不适用）", "[]", nil
	}

	meta := marketToolMetas["mx_news"]
	cctx, cancel := context.WithTimeout(ctx, meta.Timeout)
	defer cancel()

	items, err := cli.NewsSearch(cctx, label+"最新消息")
	if err != nil {
		return notWiredMX, "[]", []string{mxGapText("妙想资讯", err)}
	}
	if len(items) > mxNewsVarsLimit {
		items = items[:mxNewsVarsLimit]
	}
	// content 截断：单条实测可达数千字（含表格垃圾），全量塞进 prompt
	// 会挤掉真正有信息量的条目。
	for i := range items {
		if r := []rune(items[i].Content); len(r) > mxNewsVarsRunes {
			items[i].Content = string(r[:mxNewsVarsRunes]) + "…（已截断）"
		}
	}
	summary := fmt.Sprintf("%d 条%s", len(items), mxNewsTypeBrief(items))
	raw := "[]"
	if b, mErr := json.Marshal(items); mErr == nil {
		raw = string(b)
	}
	return summary, raw, nil
}

// fetchMXQueryForVars 取妙想问句结果进 vars。
//
// question 为空表示构造不出问句（标的信息缺失），直接跳过并记 gap。
//
// # 为什么未配置时 us/hk 要记 gap 而 cn 不必
//
// 妙想对 cn 是**补充**通道（akshare 新浪已在），没配只是少一层口径，报 gap
// 就是每轮一条同码噪声。但 us/hk 的结构化财报 akshare 不提供，妙想是**唯一**
// 来源 —— 静默不记会让模型以为「这家公司没有财报」，那是分析结论而非数据状态。
func fetchMXQueryForVars(ctx context.Context, intent *db.Node, question string) (string, string, []string) {
	view := instrumentFromPayload(intent, storeFrom(ctx))
	cli := mxForVars(ctx)
	if cli == nil {
		if view.Market == "us" || view.Market == "hk" {
			return notWiredMX, "[]",
				[]string{fmt.Sprintf("妙想通道未配置（MX_APIKEY 缺失）：%s 财报无其它来源，本维度不可用", view.Market)}
		}
		return notWiredMX, "[]", nil
	}
	if strings.TrimSpace(question) == "" {
		return notWiredMX, "[]", []string{"妙想问句未构造：标的信息不足（symbol 与 name 都为空）"}
	}
	if view.AssetClass == AssetClassCrypto {
		return "（妙想无加密数据 —— 本维度不适用）", "[]", nil
	}

	meta := marketToolMetas["mx_query"]
	cctx, cancel := context.WithTimeout(ctx, meta.Timeout)
	defer cancel()

	res, err := cli.Query(cctx, question)
	if err != nil {
		return notWiredMX, "[]", []string{mxGapText("妙想财务数据", err)}
	}
	tables := res.Tables
	if len(tables) > mxDataVarsTables {
		tables = tables[:mxDataVarsTables]
	}
	summary := fmt.Sprintf("%d 张表（问句：%s）", len(tables), question)
	for _, t := range tables {
		if strings.TrimSpace(t.Entity) != "" {
			summary += " · " + t.Entity
		}
	}
	raw := "[]"
	if b, mErr := json.Marshal(map[string]any{
		"tables": tables, "question": question, "note": res.Note,
	}); mErr == nil {
		raw = string(b)
	}
	return summary, raw, nil
}

// mxGapText 把妙想错误翻成一条 data_gaps 文案。
//
// 分工与 mxErrText 相同（工具侧给人读的操作指引），这里给 prompt 读：
// 必须让模型分清「配置缺失」与「查得到但没有」。
func mxGapText(what string, err error) string {
	me, ok := err.(*MarketError)
	if !ok {
		return fmt.Sprintf("%s取数失败（%v）", what, err)
	}
	switch me.Code {
	case ErrMXNoData:
		return fmt.Sprintf("%s：%v（这是「查得到但没有」的合法结论，不是源故障）", what, err)
	case ErrMXAuthFailed:
		return fmt.Sprintf("%s不可用：%v（apiKey 配置问题，需人工修复；不要重试）", what, err)
	case ErrMXNotConfigured:
		return fmt.Sprintf("%s不可用：MX_APIKEY 未配置（配置缺失，不是数据缺失）", what)
	default:
		return fmt.Sprintf("%s取数失败（%v）—— 上游故障", what, err)
	}
}

// mxNewsTypeBrief 汇总资讯类型分布（"（公告 2 / 研报 3 / 新闻 3）"）。
//
// 为什么必须给出类型分布：三类资讯的可信度天差地别 —— 公告是事实、
// 研报是券商观点、INV_NEWS 里混着雪球/股吧散户发言。只给「8 条」
// 会让分析师把散户贴当成公告级别的事实。
func mxNewsTypeBrief(items []MXNewsItem) string {
	label := func(t string) string {
		switch t {
		case "NOTICE":
			return "公告"
		case "REPORT":
			return "研报"
		case "NEWS", "INV_NEWS":
			return "新闻"
		case "":
			return "未知类型"
		default:
			return t
		}
	}
	counts := map[string]int{}
	var order []string
	for _, it := range items {
		k := label(it.Type)
		if counts[k] == 0 {
			order = append(order, k)
		}
		counts[k]++
	}
	parts := make([]string, 0, len(order))
	for _, k := range order {
		parts = append(parts, fmt.Sprintf("%s %d", k, counts[k]))
	}
	if len(parts) == 0 {
		return ""
	}
	return "（" + strings.Join(parts, " / ") + "）"
}
