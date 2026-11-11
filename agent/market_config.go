package agent

// 金融工具的代码级约束（不入 DB）。
//
// # 为什么不放 tools 表
//
// §1.1.2 要求 AssetClasses / Channels / Timeout / SourceKind 四项元数据。
// 但 `tools` 表只有 key/system/description/schema/agents/enabled/kind/exec/
// deferred/updated_at —— 没有这四列。
//
// 选择写死在代码里而不是扩表，理由：
//
//  1. **这些是架构约束，不是运营配置**。`Channels` 决定降级顺序（新浪→腾讯），
//     顺序写错会导致「本该走新浪却走了腾讯」，是必须重新测试的死代码。
//     让 UI 随手可改等于把可测的约束变成不可测的配置。
//  2. `AssetClasses` 决定 handler 会不会处理某个标的 —— 写错就是 bug。
//  3. `Timeout` 在 handler 里用 context.WithTimeout 更直接、更可读。
//  4. 扩表要改 db.Tool 结构 + ListTools/UpsertTool 四处同步，工作量与当初
//     `pattern_params` 相当，但收益远小于它（那个是运营要调的规则参数）。
//
// 与「阶段图模板放代码常量」「applicable_asset_classes 放代码常量」保持
// 同一取舍：架构约束进代码，运营参数进 DB。

import "time"

// SourceKind 标注数据的时间语义。用常量而非裸字符串：
// 裸字符串 "dayly" 拼错编译不报错，常量拼错立刻编译失败。
type SourceKind string

const (
	// SourceRealtime 是实时 tick（当前无通道提供，预留）。
	SourceRealtime SourceKind = "realtime"
	// SourceDaily 是日线收盘价。**注意语义**：盘中调用拿到的是昨日收盘价，
	// 因为当日 K 线尚未收盘。见 sidecars/akshare/README.md 的「语义边界」。
	SourceDaily SourceKind = "daily"
	// SourceDerived 是从其他数据算出来的（如指标），不直接来自行情源。
	SourceDerived SourceKind = "derived"
)

// Channel 是一个数据通道的标识。
//
// 带市场后缀（`akshare:cn` 而不是 `akshare`）：同一个 akshare 通道对不同市场的
// 能力不同 —— cn 有实时快照 + 腾讯备用源，hk/us 只有新浪单源。不带后缀会让
// 「akshare 可用」这种判断掩盖「hk 其实只有一条路」的事实。
type Channel string

const (
	ChannelAkshareCN Channel = "akshare:cn"
	ChannelAkshareHK Channel = "akshare:hk"
	ChannelAkshareUS Channel = "akshare:us"
	ChannelOKX       Channel = "okx"
	// ChannelMX 是东方财富妙想（MX Skills）。
	//
	// # 为什么不复用 akshare:cn
	//
	// 两者是**完全不同的数据来源与协议**：akshare 走本地 sidecar 的结构化
	// endpoint（symbol+period），妙想走东财云的自然语言问句接口（HTTP + apikey）。
	// 复用同一个 Channel 常量会让「降级链里 akshare 挂了换 akshare」这种
	// 判断永远成立不了，也让快照表的 source 字段无法区分证据来自哪家。
	//
	// 不带市场后缀：妙想一次问句可同时返回 A股/港股/美股的对比表
	// （实测「宁德时代三季度营收」返回 300750.SZ 与 03750.HK 两行），
	// 按市场切分反而会把一次请求拆成多次。
	ChannelMX Channel = "mx"
)

// marketToolMeta 是一个金融工具的静态元数据。
type marketToolMeta struct {
	// AssetClasses 是适用的资产类别。handler 内用来拒绝不适用的标的
	// （例如 get_crypto_flow 不接受 stock）。
	AssetClasses []string
	// Channels 是降级顺序。空表示该工具不走外部通道（如 compute_indicator
	// 是纯计算，SourceKind=derived）。
	Channels []Channel
	// Timeout 是单次调用的超时。§1.2 定的工具级默认值。
	Timeout time.Duration
	// SourceKind 是数据的时间语义。
	SourceKind SourceKind
}

// marketToolMetas 是全部金融工具的元数据表。
//
// 与 tools_market.go 里实际注册的工具必须保持一致 —— 有测试校验
// （TestMarketToolMetasCoverAllTools）防止这里漏项或多项。
var marketToolMetas = map[string]marketToolMeta{
	// ---- 按数据源拆分的行情工具（2026-11-02 用户需求）----
	//
	// 与 get_quote 的差别：get_quote 内部按 Channels 降级，这两个**只用本源**。
	// Channels 各写一条（不写降级链）—— 声明上就表达「我只走这一个源」。
	"quote_akshare": {
		AssetClasses: []string{"stock", "etf", "index"},
		Channels:     []Channel{ChannelAkshareCN},
		Timeout:      8 * time.Second,
		SourceKind:   SourceDaily,
	},
	"quote_okx": {
		AssetClasses: []string{"crypto"},
		Channels:     []Channel{ChannelOKX},
		Timeout:      8 * time.Second,
		SourceKind:   SourceDaily,
	},
	"quote_mx": {
		// 妙想是问句接口，超时比结构化 REST 长（含语义解析）。
		AssetClasses: []string{"stock", "etf", "index"},
		Channels:     []Channel{ChannelMX},
		Timeout:      20 * time.Second,
		SourceKind:   SourceDaily,
	},

	"get_quote": {
		// crypto 走 OKX，其余走 akshare。index 包含 ^VIX/^NDX 这类
		// 因子指数（它们在 instruments 表里 active=false，但查询仍合法）。
		AssetClasses: []string{"stock", "etf", "index", "crypto"},
		Channels: []Channel{
			ChannelAkshareCN, ChannelAkshareHK, ChannelAkshareUS, ChannelOKX,
		},
		Timeout:    8 * time.Second,
		SourceKind: SourceDaily,
	},
	"get_kline": {
		AssetClasses: []string{"stock", "etf", "index", "crypto"},
		Channels: []Channel{
			ChannelAkshareCN, ChannelAkshareHK, ChannelAkshareUS, ChannelOKX,
		},
		Timeout:    15 * time.Second,
		SourceKind: SourceDaily,
	},
	"compute_indicator": {
		// 两种模式：
		//   ① 显式数组模式 —— 纯计算，无 IO
		//   ② symbol 模式 —— 工具自己取 K 线再算（2026-11-05 加）
		//
		// # 为什么必须有 Channels
		//
		// 加了 symbol 模式后它会走 fetchKlineWithFallback，那里按 meta.Channels
		// 选通道。原来写 nil（纯计算），symbol 模式就会永远报
		// 「market="crypto" 无可用通道」。四个市场的通道都列上 ——
		// 与 get_kline 保持一致，否则同一个标的在两个工具里可用性不同。
		AssetClasses: []string{"stock", "etf", "index", "crypto"},
		Channels:     []Channel{ChannelAkshareCN, ChannelAkshareHK, ChannelAkshareUS, ChannelOKX},
		// 超时从 2s 放宽：symbol 模式要取 250 根 K 线，2s 不够。
		// 取数与计算共用这个上限，纯计算模式远用不完。
		Timeout:    30 * time.Second,
		SourceKind: SourceDerived,
	},
	"get_indicator_okx": {
		// 服务端指标只有 OKX 提供（70+ 种 + backtest-time 回溯）。
		//
		// AssetClasses 仍列全 4 种：虽然只有 OKX 通道，但「哪些资产类别能用」
		// 与「走哪个通道」是两个维度。crypto 标的直接能用；stock/etf/index
		// 也可以查（OKX 有对应的现货对，如 BTC-USDT 与股票无关，
		// 但用户可能想用 OKX 的指标算法横向对比）—— 由调用方选 symbol 决定。
		//
		// 实际上更有用的判断在 client 层：symbol 必须含连字符（只能是 OKX instId）。
		AssetClasses: []string{"stock", "etf", "index", "crypto"},
		Channels:     []Channel{ChannelOKX},
		Timeout:      20 * time.Second, // 服务端算 70+ 指标可能慢
		SourceKind:   SourceDerived,    // 服务端算出来的，不是原始行情
	},
	"get_macro": {
		// 宏观指标是「全市场通用」的，不属于某只标的的 asset_class。
		// 列全部：任何资产类别的分析师都可能引用宏观背景。
		AssetClasses: []string{"stock", "etf", "index", "crypto"},
		// 宏观数据只有 akshare 侧提供（OKX 没有 CPI/PMI 这类指标）。
		Channels:   []Channel{ChannelAkshareCN},
		Timeout:    15 * time.Second,
		SourceKind: SourceDaily, // 月/季频，但语义上属于「已发布的历史数据」
	},
	"get_volatility": {
		AssetClasses: []string{"stock", "etf", "index", "crypto"},
		Channels: []Channel{
			ChannelAkshareCN, ChannelAkshareHK, ChannelAkshareUS, ChannelOKX,
		},
		Timeout: 20 * time.Second, // 内部要取一次 K 线 + 一次 ^VIX 报价
		// SourceKind=derived：atr_pct 是算出来的，但底层用的日线是 SourceDaily。
		// 取"较保守"的那个 —— derived 意味着"不能直接当原始证据引用"。
		SourceKind: SourceDerived,
	},
	"get_news": {
		// **只有股票市场**，不含 crypto —— 这条是刻意的能力边界，不是漏了。
		//
		// 两个源都是股票文本源：财新 stock_news_main_cx（财经杂志的深度报道 +
		// 「华尔街原声」这类美股评论）、财联社 stock_info_global_cls（A股/美股快讯）。
		// 它们都不产加密新闻。若把 crypto 列进来，analyst_sentiment_crypto 会
		// 拿到一个「永远返回空」的工具，而空结果在 prompt 里会被读成
		// 「今天没有加密消息面事件」—— 那是把「没有数据源」包装成分析结论。
		//
		// 所以 AssetClasses 只列 stock/etf/index（index 也读同一批市场新闻，
		// 例如 ^VIX 的分析需要「美股大盘」这类背景新闻），handler 对 crypto
		// 显式报错而非返回空。
		AssetClasses: []string{"stock", "etf", "index"},
		// 新闻只有一个源家族（akshare 侧 merge 财新+财联社），不分市场通道：
		// cn/us/hk 都是同一次 HTTP 请求，参数不同而已。
		Channels:   []Channel{ChannelAkshareCN},
		Timeout:    15 * time.Second, // sidecar 内部串行拉两个源（各 ~1s）+ 合并去重
		SourceKind: SourceDaily,      // 新闻是已发生事件，不是实时快照
	},
	"get_fundamentals": {
		// **只有 A 股真正可用**，但 AssetClasses 仍列 index —— 见下方说明。
		//
		// 为什么不列 crypto：加密资产没有财报与 SEC filing，这是**维度不存在**
		// （not_applicable），不是数据缺失。列进来会让分析师以为「财报取不到，
		// 该降置信度」，而真相是这个维度对它根本不适用。
		//
		// 为什么列了 index：指数本身没有财报，但 etf/index 共用同一套新浪接口
		// 按代码取（例如某些 ETF 有基金财务数据）。handler 侧对取不到的标的
		// 返回 FUNDAMENTAL_NO_DATA，上层据此写 data_gaps —— 这是「查了但没有」，
		// 与「不该查」是两件事，交给出错路径处理比提前裁剪更诚实。
		//
		// us/hk 也在 market 参数里允许，但 sidecar 侧固定返回 NO_DATA：
		// akshare 侧唯一能取美股财报的接口是东财 stock_financial_us_report_em，
		// 它以 _em 结尾，而东财对本机 IP 封禁且状态可变 —— 该接口失效时返回空
		// DataFrame 而不抛异常，调用方会把「通道死了」当成「这家公司没有财报」。
		//
		// **us/hk 的财报由 mx 通道（ChannelMX / mx_query）提供，不是缺失**。
		// 所以本工具的 Channels 只列 akshare:cn 是正确的 —— 它描述的是「本工具
		// 自己能走哪些通道」，不是「这个维度系统里有没有数据」。判断某市场财报
		// 是否可得，要看 get_fundamentals（cn）∪ mx_query（cn/hk/us）的并集。
		AssetClasses: []string{"stock", "etf", "index"},
		Channels:     []Channel{ChannelAkshareCN},
		Timeout:      20 * time.Second, // 新浪三大报表是 104 行 × 147 列，转置+截断比 quote 慢
		SourceKind:   SourceDaily,      // 财报是已发布的定期报告
	},
	"get_crypto_flow": {
		// 只有加密有资金费率与持仓量。
		// **AssetClasses 只列 crypto** —— 这是唯一一个不覆盖全部资产类别的工具，
		// handler 会对非 crypto 显式报错（不返回空结果）。
		AssetClasses: []string{"crypto"},
		Channels:     []Channel{ChannelOKX}, // 只有 OKX 提供这两项
		Timeout:      10 * time.Second,
		// SourceKind=realtime：资金费率与持仓量是**当下快照**，不是日线。
		// 这个值影响下游「能否用于 point-in-time 回测」的判断 —— 实时值
		// 没有历史版本，回测时必须用当时记录下来的快照，不能重放。
		SourceKind: SourceRealtime,
	},

	"get_account_balance": {
		// 交易账户资产。**唯一需要有效凭证的金融工具** ——
		// 行情类全部走公开接口，取不到余额不可能是「行情挂了」。
		//
		// AssetClasses 列全 4 种：账户本身不分资产类别（一个 OKX 账户
		// 同时能交易 crypto 与 tokenized 资产），限权在通道层。
		AssetClasses: []string{"stock", "etf", "index", "crypto"},
		Channels:     []Channel{ChannelOKX},
		Timeout:      15 * time.Second,
		// realtime：余额是当下快照，没有历史版本，不能用于回测重放。
		SourceKind: SourceRealtime,
	},

	// ── 东方财富妙想（MX Skills）───────────────────────────────────────
	//
	// 两个工具都是**自然语言问句**接口：入参是一句中文问句，不是 symbol+period。
	// 所以它们与 get_quote/get_kline 的关系是「补充」而不是「替代」：
	// 结构化取一只票的日线仍走 akshare（字段确定、可复现），
	// 而「主力资金流向」「近三年毛利率趋势」这类没有结构化 endpoint 的问句走妙想。
	//
	// # 为什么 AssetClasses 不含 crypto
	//
	// 妙想是东财的 A股/港股/美股/基金/债券数据库，**没有加密数据**。
	// 列进去会让 analyst_sentiment_crypto 拿到一个永远返回 MX_NO_DATA 的工具，
	// 而空结果在 prompt 里会被读成「今天没有加密消息面事件」—— 那是把
	// 「没有数据源」包装成分析结论（与 get_news 不含 crypto 同一理由）。
	//
	// # 为什么 SourceKind 是 daily 而不是 realtime
	//
	// 妙想返回的是「已发布的行情/财务快照」，不保证是盘中 tick。把它标成
	// realtime 会让下游误以为可以用于「此刻」的判断。
	"mx_query": {
		// 只列真正的**资产类别**。妙想确实也覆盖港股/美股/A股与债券，
		// 但 cn/hk/us 是**市场**不是资产类别 —— 同一张表里出现两个维度会让
		// 将来按 asset_class 过滤的代码（如 analystsFor）无从判断。
		// 市场覆盖写在注释与工具描述里：market 参数由调用方传，handler 不按它裁剪。
		AssetClasses: []string{"stock", "etf", "index"},
		Channels:     []Channel{ChannelMX},
		// 30s：妙想是云端大模型解析问句 + 查库，比 akshare 的固定 endpoint 慢。
		// 与 NewMXClient 的 http.Client.Timeout 一致，避免「内层超时先触发、
		// 外层 context 却已取消」这种两层超时打架。
		Timeout:    30 * time.Second,
		SourceKind: SourceDaily,
	},
	"mx_news": {
		AssetClasses: []string{"stock", "etf", "index"},
		Channels:     []Channel{ChannelMX},
		Timeout:      30 * time.Second,
		// 资讯是**已发生事件**，不是实时快照 —— 与 get_news 的取值一致。
		SourceKind: SourceDaily,
	},
}
