package agent

// 按数据源拆分的行情工具（2026-11-02 用户需求）。
//
// # 为什么要把「一个 get_quote」拆成「每个源一个工具」
//
// 原设计：get_quote 内部按 meta.Channels 自动降级（akshare → okx），
// agent 只看到一个工具、不知道数据来自哪个源。
//
// 问题在于**降级是静默的**：
//   - agent 无法显式指定「我就要东财的数据」
//   - 无法对比两个源的差异（价差、延迟、覆盖范围）
//   - 出错时不知道是哪个源挂了（错误只说「所有通道失败」）
//   - 新数据源接入后，agent 不会主动用它（因为不在降级链里就看不见）
//
// 用户的要求是「多个行情数据源分别作为不同的工具，并在可用时提供」，
// 所以这里给每个源一个独立工具，并且**只在该源真的可用时才注册**。
//
// # 工具命名
//
//	quote_akshare   A股/港股/美股（akshare sidecar，:8791）
//	quote_mx        东方财富妙想（需 API Key）
//	quote_okx       加密（OKX CLI，需凭证）
//
// 与既有 get_quote 并存：get_quote 保留自动降级语义（适合「随便给我一个价」），
// 这些新工具用于「必须指定源」或「对比多源」的场景。
//
// # 「可用时提供」怎么判定
//
// 注册阶段（Tools()）只检查**静态可判定**的条件：
//   - akshare：client 非 nil（sidecar 地址已配置）
//   - mx：client 非 nil（API Key 已配）
//   - okx：client 非 nil（CLI 可解析）
//
// 不做运行时探测（Health）—— 那会在每次装配时打网络，而装配发生在
// 每个任务开始时。真正的不可用由**工具调用**时的错误体现，文案里带源名。
//
// 这个取舍的代价：sidecar 挂了但 client 还在时，工具仍会出现，
// agent 调用后拿到明确错误。相比「启动时探测超时导致工具消失」，
// 前者可诊断、后者只会让 agent 困惑「为什么没有这个工具」。

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	acperm "github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// sourceQuoteTool 是一个「指定数据源的行情查询」工具定义。
type sourceQuoteTool struct {
	// Name 是工具名（如 quote_akshare）。
	Name string
	// SourceLabel 是给人看的源名称（出现在描述与错误里）。
	SourceLabel string
	// Channel 是该源使用的通道常量。
	Channel Channel
	// Client 是执行查询的客户端；nil 表示该源不可用，工具不注册。
	Client MarketClient
	// Markets 是该源支持的市场（用于描述与入参校验）。
	Markets []string
	// Note 是使用提示（如「仅 A 股」「需 sidecar」）。
	Note string
}

// allSourceQuoteTools 返回**全部**按源工具的声明（含不可用的源）。
//
// # 为什么需要「全部」这一版（关键）
//
// 有两个用途截然不同的实例：
//
//	种子壳 NewMarketToolSet(nil)      → 写 tools 表，决定「谁能用」
//	运行实例 buildDomainReg 构造的     → 真正执行，决定「怎么取数」
//
// 种子壳的 client 全是 nil。若它也只返回可用源，则**没有任何按源工具
// 能被写进 tools 表** —— ToolResolve 按 tools 表过滤，结果就是
// 「工具定义了但 agent 永远拿不到」，且无任何报错。
//
// 所以：种子里必须有全部名字（否则表里没行）；
// 运行时再按可用性过滤（否则 agent 会看到必然失败的工具）。
func (m *MarketToolSet) allSourceQuoteTools() []actool.CoreTool {
	if m == nil {
		return nil
	}
	// 种子壳没有 client，但工具声明仍要生成 —— 用一个 nil-client 的
	// 临时实例来产声明（handler 永不被调用）。
	probe := m
	if m.client == nil && m.crypto == nil {
		probe = &MarketToolSet{}
	}
	var out []actool.CoreTool
	for _, c := range probe.sourceQuoteDecls() {
		out = append(out, probe.buildSourceQuoteTool(c))
	}
	// mx 单独构造（问句接口，非 MarketClient）。
	// 种子壳（client 全 nil）也要产出声明 —— 否则 tools 表没行。
	if tl, ok := probe.mxQuoteToolSeed(); ok {
		out = append(out, tl)
	}
	return out
}

// mxQuoteToolSeed 产出一个「仅用于写 tools 表」的 quote_mx 声明。
//
// 种子壳没有 mx client，但工具声明必须存在（否则没有表行 → agent 拿不到）。
// handler 永不被调用，所以用空实现占位。
func (m *MarketToolSet) mxQuoteToolSeed() (actool.CoreTool, bool) {
	// Prompt：这段是 quote_mx 与另两个工具最重要的差异说明。
	//
	// 用户明确指出过这个风险：「Open/High/Low/Volume 留空而不是填 0——
	// 0 会被读成跌停了，空值不会」。但仅靠「不填 0」不够 ——
	// agent 还得知道空值是什么意思，否则会自行猜测。
	const prompt = mxQuotePrompt

	return readToolWithPrompt(
		"quote_mx", "用【东方财富妙想】查询标的最新价（结构化输出）。"+
			"支持市场：cn / hk / us（妙想无加密数据）。"+
			"需要更自由的问句请用 mx_query。",
		prompt,
		obj(map[string]any{
			"symbol": str("标的代码，如 600519 / 00700 / AAPL"),
			"market": str("市场：cn|hk|us"),
		}, "symbol"),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			return actool.Errorf("quote_mx 尚未装配（server 启动时未初始化 mx 客户端）"), nil
		},
	), true
}

// sourceQuoteTools 返回**当前可用**的按源行情工具（运行期用）。
//
// 只返回 Client 非 nil 的项 —— 这就是「在可用时提供」的实现。
func (m *MarketToolSet) sourceQuoteTools() []actool.CoreTool {
	if m == nil {
		return nil
	}
	out := make([]actool.CoreTool, 0)
	for _, c := range m.sourceQuoteDecls() {
		if c.Client == nil {
			continue // 该源不可用 → 不注册
		}
		out = append(out, m.buildSourceQuoteTool(c))
	}
	// mx：客户端可用才注册
	if tl, ok := m.mxQuoteTool(); ok {
		out = append(out, tl)
	}
	return out
}

// sourceQuoteDecls 返回各源的声明（含 Client，可能为 nil）。
func (m *MarketToolSet) sourceQuoteDecls() []sourceQuoteTool {
	return []sourceQuoteTool{
		{
			Name:        "quote_akshare",
			SourceLabel: "AkShare（本地 sidecar :8791）",
			Channel:     ChannelAkshareCN,
			Client:      m.akshareSource(),
			Markets:     []string{"cn", "hk", "us"},
			Note:        "A股/港股/美股。数据来自 akshare 开源库，无需 API Key，但需 sidecar 运行。",
		},
		{
			Name:        "quote_okx",
			SourceLabel: "OKX（加密）",
			Channel:     ChannelOKX,
			Client:      m.okxSource(),
			Markets:     []string{"crypto"},
			Note:        "加密行情。需在凭证页配置 OKX 交易凭证，且 CLI 可用。",
		},
	}
}

// mxQuoteSource 返回 mx「结构化行情」工具的声明状态。
//
// # 为什么 mx 不走 sourceQuoteTool 模板
//
// 那个模板假定客户端实现 MarketClient（FetchQuote/FetchKline）。
// 妙想是**自然语言问句**接口（Query），没有结构化 FetchQuote ——
// 强行套模板就得给 MXClient 加两个假方法，那是接口污染。
//
// 所以 quote_mx 单独实现：把 {symbol, market} 拼成问句，再从返回表格里
// 抽取最新价，归一化成与 Quote 一致的形状（symbol/market/last/trading_date），
// 让 agent 不必为 mx 记另一套字段名。
func (m *MarketToolSet) mxQuoteTool() (actool.CoreTool, bool) {
	if m == nil {
		return nil, false
	}
	cli, err := m.mxSource()
	if err != nil || cli == nil {
		return nil, false
	}

	return readTool(
		"quote_mx",
		"用【东方财富妙想】查询标的最新价（结构化输出）。"+
			"内部把 symbol+market 拼成问句（如「贵州茅台最新收盘价」），"+
			"再从返回表格取最新一行的收盘价，归一成 {symbol, market, last, trading_date}。"+
			"支持市场：cn / hk / us（妙想无加密数据）。"+
			"需要更自由的问句（财务/资金流/多标的对比）请用 mx_query。"+
			"若返回 MX_NO_DATA，说明该标的在妙想库里查不到当前价，改用 quote_akshare。",
		obj(map[string]any{
			"symbol": str("标的代码，如 600519 / 00700 / AAPL"),
			"market": str("市场：cn|hk|us"),
		}, "symbol"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Symbol string `json:"symbol"`
				Market string `json:"market"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf(fmt.Sprintf("入参解析失败: %v", err)), nil
			}
			symbol := strings.TrimSpace(a.Symbol)
			if symbol == "" {
				return actool.Errorf("缺少 symbol"), nil
			}
			market := strings.ToLower(strings.TrimSpace(a.Market))
			if market == "" {
				market = "cn"
			}
			if !slices.Contains([]string{"cn", "hk", "us"}, market) {
				return actool.Errorf(fmt.Sprintf(
					"quote_mx 只支持 cn|hk|us（妙想无加密数据），收到 %q", market)), nil
			}

			// 问句里带上「最新」「收盘价」：妙想按语义解析，
			// 明确这两个词能显著减少返回无关表的情况。
			query := fmt.Sprintf("%s 最新收盘价", symbol)
			cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()

			res, qErr := cli.Query(cctx, query)
			if qErr != nil {
				if IsNoDataError(qErr) {
					return actool.Errorf(fmt.Sprintf(
						"妙想查到 0 张表（问句=%q）。该标的可能在妙想库无此数据，"+
							"可改用 quote_akshare，或换 mx_query 手写更具体的问句", query)), nil
				}
				return actool.Errorf(fmt.Sprintf("妙想查询失败：%v", qErr)), nil
			}

			q, pickErr := pickLatestQuoteFromMX(res, symbol, market)
			if pickErr != nil {
				return actool.Errorf(fmt.Sprintf(
					"妙想返回了 %d 张表，但无法提取最新价：%v；建议改用 mx_query 看原始表格结构",
					res.TableCount, pickErr)), nil
			}
			out := quoteToMap(q)
			out["source"] = "东方财富妙想（MX Skills）"
			return jsonResult(out)
		},
	), true
}

// pickLatestQuoteFromMX 从妙想返回的表格里提取最新价，归一成 Quote。
//
// # 提取规则（都是「找不到就报错」，不猜）
//
//  1. 优先选 head_label=="日期" 的表（时间序列），取最后一行
//     —— 妙想按时间升序返回，最后一行最新
//  2. 在该行的指标列里找含「收盘」的列；没有则找含「价」的列
//  3. 找不到任何价格类列 → 报错（不把「成交量」当价格）
//
// 为什么必须严格：把错误的列当价格，比查不到更危险 ——
// 用户会拿着一个看似合理的数字去做决策。
func pickLatestQuoteFromMX(res *MXQueryResult, symbol, market string) (*Quote, error) {
	if res == nil || len(res.Tables) == 0 {
		return nil, fmt.Errorf("没有返回任何表")
	}

	// 优先时间序列表
	var table *MXTable
	for i := range res.Tables {
		if res.Tables[i].HeadLabel == "日期" && len(res.Tables[i].Rows) > 0 {
			table = &res.Tables[i]
			break
		}
	}
	if table == nil {
		// 退化：任意有行的表
		for i := range res.Tables {
			if len(res.Tables[i].Rows) > 0 {
				table = &res.Tables[i]
				break
			}
		}
	}
	if table == nil {
		return nil, fmt.Errorf("所有表都没有数据行")
	}

	last := table.Rows[len(table.Rows)-1]

	// 找价格列：先「收盘」，再「价」
	var priceCol, priceVal string
	for _, c := range table.Columns {
		if strings.Contains(c, "收盘") {
			if v := strings.TrimSpace(last[c]); v != "" {
				priceCol, priceVal = c, v
				break
			}
		}
	}
	if priceCol == "" {
		for _, c := range table.Columns {
			if strings.Contains(c, "价") {
				if v := strings.TrimSpace(last[c]); v != "" {
					priceCol, priceVal = c, v
					break
				}
			}
		}
	}
	if priceCol == "" {
		return nil, fmt.Errorf("表 %q 的列 %v 里没有收盘价/价格类字段",
			table.Name, table.Columns)
	}

	// 交易日：取首列（head_label 是「日期」时首列就是日期）
	tradingDate := ""
	if table.HeadLabel == "日期" && len(table.Columns) > 0 {
		tradingDate = strings.TrimSpace(last[table.Columns[0]])
	}

	return &Quote{
		Symbol:      symbol,
		Market:      market,
		PriceKind:   "daily_close",
		TradingDate: tradingDate,
		Last:        priceVal,
		// 其余字段妙想这次问句没取（Open/High/Low/Volume）。
		// 留空而不是填 0 —— 0 会被读成「跌停了」，空值不会。
		Channel: ChannelMX,
	}, nil
}

// SourceQuoteToolNames 是全部按源行情工具的名字（含不可用源）。
var SourceQuoteToolNames = []string{"quote_akshare", "quote_mx", "quote_okx"}

// SourceQuoteToolDefaults 返回某按源工具的「模型可见描述」与 schema。
//
// # 为什么描述里要拼上 Prompt
//
// norma 的 Registry.Schemas() 才做 desc += prompt（tool/registry.go:50-53）。
// 但 tools 表的 description 列**不会**自动获得这段 —— 那是 SeedTool 写入的
// 静态快照。若不同时写进去，UI 与 /api/tools 都会缺返回值契约，
// 用户在界面上看不到「空值不是 0」这类关键说明。
//
// 所以这里返回拼接后的全文，供 RefreshToolDefaults 写库。
func SourceQuoteToolDefaults(name string) (desc string, schema map[string]any, ok bool) {
	// 用种子壳取声明（nil client 即可，只要 desc/schema）。
	seed := NewMarketToolSet(nil)
	for _, tl := range seed.Tools() {
		if tl.Name() != name {
			continue
		}
		d := tl.Description()
		if p := tl.Prompt(); p != "" {
			d += promptSep + p
		}
		return d, tl.InputSchema(), true
	}
	return "", nil, false
}

// IsSourceQuoteTool 判断某名字是否属于按源行情工具。
func IsSourceQuoteTool(name string) bool {
	return slices.Contains(SourceQuoteToolNames, name)
}

// AvailableSourceTool 判断某个按源工具当前是否可用（client 已装配）。
//
// 供 server 的 domainReg 装配时过滤：不可用的源不该注入给 agent。
func (m *MarketToolSet) AvailableSourceTool(name string) bool {
	if m == nil {
		return false
	}
	// mx 不走 sourceQuoteDecls（它是问句接口，见 mxQuoteTool 的注释），
	// 单独判断。
	if name == "quote_mx" {
		_, ok := m.mxQuoteTool()
		return ok
	}
	for _, c := range m.sourceQuoteDecls() {
		if c.Name == name {
			return c.Client != nil
		}
	}
	return false
}

// mxQuotePrompt 是 quote_mx 的返回值契约（Description 之外追加说明）。
//
// # 为什么必须显式说明空值语义
//
// 用户明确指出过风险：「Open/High/Low/Volume 留空而不是填 0 ——
// 0 会被读成跌停了，空值不会」。但仅靠「不填 0」不够：
// agent 还得知道空值代表什么，否则会自行猜测（重试、换源、或当成停牌）。
//
// 种子壳（mxQuoteToolSeed）与运行实例共用这份文案 —— 两处不一致会让
// tools 表里的描述与实际行为脱节。

// promptSep 是 Description 与 Prompt 之间的分隔，与 norma Registry.Schemas() 一致。
const promptSep = "\n\n"

const mxQuotePrompt = "返回值字段（全部小写下划线）：symbol, market, price_kind, trading_date, " +
	"last, open, high, low, prev_close, change_pct, volume, amount, is_partial, channel, source。" +
	"⚠ 本工具**只有收盘价**（last 有值）。open / high / low / volume / amount / prev_close / " +
	"change_pct 恒为空字符串，表示「本次问句没取这些数据」，不是 0，也不是停牌或跌停。" +
	"不要用它们做任何计算（振幅、涨跌幅、量比）；需要完整 OHLC 请改用 " +
	"quote_akshare（A股/港股/美股）或 get_kline。" +
	"也不要因为这几个字段为空就重试本工具 —— 重试拿到的仍是同样的空值。"

// readToolWithPrompt 构造只读工具，并带上 Spec.Prompt（追加到描述末尾）。
//
// # 为什么要这个而不是直接用 readTool
//
// readTool（agent/tools.go:283）不接受 Prompt 字段。而 Prompt 是
// norma 的「追加到工具描述末尾」机制（tool/registry.go:50-53）：
//
//	desc := t.Description()
//	if p := t.Prompt(); p != "" { desc += p }
//
// 正好适合放「怎么解读返回值」这类约定 —— 它们属于契约，不属于介绍。
// Description 说「这工具干什么」，Prompt 说「结果怎么读、边界在哪」。
func readToolWithPrompt(name, desc, prompt string, schema map[string]any,
	run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Prompt: prompt, Schema: schema,
		ReadOnly:    func(json.RawMessage) bool { return true },
		Concurrent:  func(json.RawMessage) bool { return true },
		Permissions: func(context.Context, json.RawMessage, acperm.Context) acperm.Decision { return acperm.Allowed() },
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

// quoteToMap 把 Quote 转成工具返回的 map（小写键）。
//
// # 为什么不能直接 json.Marshal(q)
//
// Quote 没有 json tag，直接序列化会产出 {"Symbol","Last","Open",...} ——
// 大写键。而 get_quote 是手写小写键（tools_market.go:377-393）。
// 两个工具都叫「查行情」，返回字段名却不同，agent 必须为每个工具记一套，
// 而且大小写混用时极易读错（"Open" 与 "open" 在不同工具里各指一个）。
//
// 这里统一成小写，与 get_quote 完全对齐。
//
// # 空串语义
//
// 没有取到的字段保留空串（不删键、不填 0）：
//   - 删键会让 agent 以为「该字段不存在」
//   - 填 0 会让 agent 以为「值是 0」（跌停/停牌）
//
// 空串 + 描述里写明「空 = 本次未取」是唯一不误导的组合。
func quoteToMap(q *Quote) map[string]any {
	return map[string]any{
		"symbol":       q.Symbol,
		"market":       q.Market,
		"price_kind":   q.PriceKind,
		"trading_date": q.TradingDate,
		"last":         q.Last,
		"open":         q.Open,
		"high":         q.High,
		"low":          q.Low,
		"prev_close":   q.PrevClose,
		"change_pct":   q.ChangePct,
		"volume":       q.Volume,
		"amount":       q.Amount,
		"is_partial":   q.IsPartial,
		"channel":      string(q.Channel),
	}
}

// buildSourceQuoteTool 为单个源构造查询工具。
func (m *MarketToolSet) buildSourceQuoteTool(src sourceQuoteTool) actool.CoreTool {
	markets := strings.Join(src.Markets, "|")
	desc := fmt.Sprintf(
		"用【%s】查询标的最新行情。\n"+
			"支持市场：%s。\n"+
			"%s\n\n"+
			"⚠ 语义：返回最新日线收盘价，不是实时 tick。\n"+
			"  trading_date 是价格所属交易日，务必据此判断时效性。\n\n"+
			"与其他源的关系：还有 quote_akshare / quote_mx / quote_okx 各自独立的工具，\n"+
			"要对比不同源的价格差异时分别调用它们（不要依赖自动降级）。",
		src.SourceLabel, markets, src.Note)

	// Prompt 讲「结果怎么读」，Description 讲「这工具干什么」。
	//
	// # 为什么必须显式说明 OHLC 完整性
	//
	// 三个按源工具的字段完整度不同（quote_mx 只有 last）。
	// agent 若不知道自己拿到的是完整 OHLC 还是只有收盘价，
	// 就会用缺失的字段做判断（比如算振幅却 high/low 为空）。
	const prompt = "返回值字段（全部小写下划线）：symbol, market, price_kind, trading_date, " +
		"last, open, high, low, prev_close, change_pct, volume, amount, is_partial, channel, source。" +
		"本工具返回**完整 OHLC**（open/high/low/volume 均有值）。" +
		"空字符串表示「上游未提供」，不是 0 —— 不要当成跌停/停牌/全天无成交。" +
		"trading_date 是价格所属交易日；is_partial=true 表示该日尚未收盘。"

	return readToolWithPrompt(
		src.Name, desc, prompt,
		obj(map[string]any{
			"symbol": str("标的代码。cn: 6位如 600519；hk: 5位如 00700；us: 字母如 AAPL；crypto: 如 BTC-USDT"),
			"market": str("市场：" + markets),
		}),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Symbol string `json:"symbol"`
				Market string `json:"market"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf(fmt.Sprintf("入参解析失败: %v", err)), nil
			}
			symbol := strings.TrimSpace(a.Symbol)
			if symbol == "" {
				return actool.Errorf("缺少 symbol"), nil
			}
			market := strings.ToLower(strings.TrimSpace(a.Market))
			if market == "" {
				market = src.Markets[0]
			}
			if !slices.Contains(src.Markets, market) {
				return actool.Errorf(fmt.Sprintf(
					"%s 不支持 market=%q（支持：%s）",
					src.Name, market, markets)), nil
			}

			// 关键：**只用本源的通道**，不做降级。
			//
			// 降级会让「quote_akshare 返回了 OKX 的数据」这种事发生 ——
			// 那正是拆分工具要消除的歧义。宁可失败并说明原因。
			q, err := src.Client.FetchQuote(ctx, symbol, market, src.Channel)
			if err != nil {
				return actool.Errorf(fmt.Sprintf(
					"%s 查询失败（未降级到其他源，以保持来源明确）：%v",
					src.SourceLabel, err)), nil
			}
			if q == nil {
				return actool.Errorf(src.SourceLabel + " 返回空结果"), nil
			}
			out := quoteToMap(q)
			out["source"] = src.SourceLabel
			return jsonResult(out)
		},
	)
}

// akshareSource / mxSource / okxSource 从 MarketToolSet 现有字段取各源客户端。
//
// # 为什么用独立的取值方法而不是直接读字段
//
// MarketToolSet 的字段会随装配演进（client / crypto / mxClient）。
// 集中在这三个方法里，将来加新源或改字段名只改一处。
// 返回 nil 表示「该源当前不可用」，调用方据此决定是否注册工具。
func (m *MarketToolSet) akshareSource() MarketClient {
	if m == nil || m.client == nil {
		return nil
	}
	return m.client
}

// okxSource 返回 OKX 客户端；凭证未注入时返回 nil。
//
// # 为什么光判断 client != nil 不够
//
// NewOkxClient("") 只要二进制在 PATH 就能构造成功 —— 即使一条凭证都没配。
// 那时 quote_okx 会被注册，但每次调用都报
// 「Not logged in — run okx-auth login first」（CLI 读 config.toml 的旧状态）。
//
// 用户配了凭证后 server 会 SetCredential 注入。所以「有没有注入」
// 才是「能不能用」的真实判据。
func (m *MarketToolSet) okxSource() MarketClient {
	if m == nil || m.crypto == nil {
		return nil
	}
	// OkxClient 实现了 CredentialForTest()（只读访问器），
	// 用它判断是否已注入凭证。非 OkxClient 实现（测试桩）不带凭证概念，
	// 视为已配置 —— 否则假 client 永远不可用，测试失去意义。
	type credProbe interface{ CredentialForTest() *OkxCredential }
	if p, ok := m.crypto.(credProbe); ok {
		if p.CredentialForTest() == nil {
			return nil
		}
	}
	return m.crypto
}
