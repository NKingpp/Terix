package agent

// 金融工具集：行情 / K线 / 指标。
//
// # 为什么独立成 MarketToolSet 而不是塞进 ToolSet
//
//  1. 金融工具是一整个领域，与渗透工具（资产/漏洞/企业）无共享逻辑。
//  2. 它需要自己的 HTTP client（健康检查、warmup 等待、8 个错误码映射、重试），
//     塞进已有 15+ 字段的 ToolSet 会让后者更臃肿。
//  3. **测试可注入假 client**：ToolSet 的每个字段都要真 PG 才能构造，
//     而 MarketToolSet 只要一个 MarketClient interface。
//
// # assetClass 过滤为什么在 handler 内而不是 ToolResolve
//
// ToolResolve（server/assembly.go）在**装配工具列表时**执行，每个 agent 一次。
// 而 assetClass 是**每次工具调用的标的属性** —— 装配时根本不知道要查哪只股票。
// 放 ToolResolve 会让它需要预知所有可能的标的，逻辑上不成立。
//
// 所以过滤在 handler 内：**用 market 参数**选出该市场的通道链
// （cn → akshare:cn，hk → akshare:hk，us → akshare:us，crypto → okx）。
//
// 为什么不查 instruments 表拿 asset_class：
//  1. market 已从入参拿到，再查表是重复劳动。
//  2. 查表会给 MarketToolSet 引入 DB 依赖，破坏「注入假 client 即可单测」
//     这一前提 —— 现在整个工具集零 DB 依赖，测试不需要 PG。
//  3. asset_class（stock/etf/index）对「走哪个通道」无影响：cn 的 stock 和
//     etf 都走 akshare:cn，不会因为类别不同换源。
//
// 若未来出现「同类 market 下不同 asset_class 要走不同通道」的真实需求
// （例如 crypto 的 spot 走 OKX、而 tokenized-stock 走别的源），那时再注入
// *db.DB 查表。当前不需要。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	actool "github.com/Autumn-27/norma/tool"

	"github.com/Autumn-27/artex/db"
)

// MarketToolSet 持有金融工具需要的依赖。
//
// client 可为 nil —— builtinToolsByAgent() 用一个 nil 的空壳来读
// Name/Description/InputSchema，构造期不解引用（与 NewToolSet(nil,"") 同一原则）。
type MarketToolSet struct {
	// client 是默认通道（akshare），非 crypto 的 quote/kline/macro 走它。
	client MarketClient
	// crypto 是 crypto 专用通道（OKX），nil 时 crypto 标的不可用。
	//
	// 为什么按能力分两个 client 而不是一个大接口：
	// akshare 不做 crypto，OKX 不做宏观。塞进同一个接口会让
	// AkshareClient 必须实现一个只会 return CHANNEL_NOT_APPLICABLE 的方法 ——
	// 一个类型被迫实现它做不到的方法，说明接口切错了。
	// 未来加期权链会有第三个（OptionClient），按领域能力切分是正确演进方向。
	crypto MarketClient
	// cryptoFlow 是加密衍生品资金流（资金费率/持仓量），仅 OKX 实现。
	// 与 crypto 分开的理由见 CryptoFlowClient 的注释（接口隔离）。
	cryptoFlow CryptoFlowClient
	// db 是**可选**的快照落库目标，nil 表示不落快照（取数功能不受影响）。
	//
	// 为什么是可选：MarketToolSet 的核心价值是「注入假 client 即可单测」，
	// 这个前提要求它不强制依赖 DB。测试用 nil client + nil db 跑纯逻辑，
	// 生产装配时才有 db。
	//
	// 落快照失败**不阻断取数** —— 快照是附加证据链，不是主功能。见
	// snapshot_capture.go 顶部的说明。
	db *db.DB
	// mx 是东方财富妙想客户端（自然语言问句 + 资讯）。nil 表示该通道未启用。
	//
	// # 为什么单独一个字段而不是复用 client
	//
	// client 是 MarketClient 接口（quote/kline/macro），妙想不是它的实现
	// （理由见 mx_client.go 文件头）。硬塞进 client 只能靠类型断言区分，
	// 一旦装配搞反，行情会静默走错通道。
	//
	// # 为什么用 setter 而不是改构造器签名
	//
	// 三个构造器都被测试、工具目录种子壳与 server 装配调用。改签名会让
	// 所有调用点跟着改，而收益为零 —— 妙想是可选的补充通道。
	// 不调用 SetMXClient = 不启用（工具仍会注册，调用时返回明确的「未启用」）。
	mx *MXClient
}

// NewMarketToolSet 构造工具集（只用默认通道）。
func NewMarketToolSet(client MarketClient) *MarketToolSet {
	return &MarketToolSet{client: client}
}

// NewMarketToolSetWithCryptoAndFlow 构造带 crypto + 资金流的完整工具集。
func NewMarketToolSetWithCryptoAndFlow(market, crypto MarketClient, flow CryptoFlowClient) *MarketToolSet {
	return &MarketToolSet{client: market, crypto: crypto, cryptoFlow: flow}
}

// SetDB 注入快照落库目标。不调用 = 不落快照（取数功能不受影响）。
func (m *MarketToolSet) SetDB(d *db.DB) { m.db = d }

// SetMXClient 注入妙想客户端。不调用 = 妙想两个工具返回「未启用」。
//
// 传 nil 或不传等价 —— 都不启用。不 panic、不报错：未配置是可诊断状态，
// 由工具 handler 给出明确错误（见 mxSource）。
func (m *MarketToolSet) SetMXClient(c *MXClient) { m.mx = c }

// NewMarketToolSetWithCrypto 构造带 crypto 通道的工具集。
//
// 两个 client 都可为 nil —— builtinToolsByAgent() 用全 nil 的空壳读
// Name/Description/InputSchema。
func NewMarketToolSetWithCrypto(market, crypto MarketClient) *MarketToolSet {
	return &MarketToolSet{client: market, crypto: crypto}
}

// clientFor 按 market 返回该用的通道客户端。
//
// crypto 走 crypto client，其余走默认 client。
func (m *MarketToolSet) clientFor(market string) (MarketClient, error) {
	if market == "crypto" {
		if m.crypto == nil {
			return nil, fmt.Errorf("crypto 通道未配置（OKX 客户端未注入）")
		}
		return m.crypto, nil
	}
	if m.client == nil {
		return nil, fmt.Errorf("market 通道未配置（MarketClient 未注入）")
	}
	return m.client, nil
}

// Tools 返回全部金融工具。
//
// 顺序即 seed 顺序。与 marketToolMetas 必须一一对应
// （TestMarketToolMetasCoverAllTools 校验）。
func (m *MarketToolSet) Tools() []actool.CoreTool {
	tools := []actool.CoreTool{
		m.computeIndicator(), // 纯计算，无 IO —— 最先可验证
		m.getQuote(),
		m.getKline(),
		m.getMacro(),
		m.getVolatility(),
		m.getCryptoFlow(),
		m.getIndicatorOkx(), // 服务端指标（需 OKX 网络）
		m.getNews(),         // 新闻（只走 akshare，见 market_news.go）
		m.getFundamentals(), // 财报（只走 akshare，仅 A 股）
		m.mxQuery(),         // 妙想自然语言问句（行情/财务，见 tools_mx.go）
		m.mxNews(),          // 妙想资讯（新闻/公告/研报）
		// 按数据源拆分的行情工具（2026-11-02 用户需求）。
		//
		// 「在可用时提供」在这里体现：sourceQuoteTools() 内部按
		// Client 是否为 nil 过滤，不可用的源不会出现在返回值里。
		// 所以 agent 看到的工具列表会随凭证/sidecar 状态变化 —— 这不是 bug，
		// 是「只暴露真能用的能力」的刻意设计（见 tools_market_source.go 顶部）。
	}
	// 动态追加按源拆分的行情工具。
	//
	// # 为什么这里是「全部」而不是「可用」
	//
	// Tools() 被两个实例调用，语义不同：
	//   - 种子壳 NewMarketToolSet(nil)：写 tools 表，决定「谁能用」
	//   - 运行实例（buildDomainReg）：真正执行，决定「怎么取数」
	//
	// 种子壳 client 全 nil。若这里也按可用性过滤，则没有任何按源工具能
	// 写进 tools 表 → ToolResolve 按表过滤 → agent 永远拿不到，且无报错。
	//
	// 运行时过滤在 ToolResolve 之后发生（server 侧），见 buildDomainReg。
	return append(tools, m.allSourceQuoteTools()...)
}

// ---------------------------------------------------------------------
// compute_indicator：纯计算，不依赖 sidecar
// ---------------------------------------------------------------------

func (m *MarketToolSet) computeIndicator() actool.CoreTool {
	meta := marketToolMetas["compute_indicator"]
	return readTool(
		"compute_indicator",
		"计算技术指标。**推荐用法：只传 symbol + market + indicator，工具自己去取 K 线。**\n"+
			"支持：MACD(12,26,9) / RSI(14) / ATR(14) / BOLL(20,2) / KDJ(9,3,3) / VWAP(累积)。\n"+
			"指标与 TA-Lib 逐值对撞验证过（容差 1e-5），warmup 期返回空字符串（不是 0）。\n"+
			"\n"+
			"⚠ **不要手抄 K 线数组**：给 symbol 让工具取数。手抄 200+ 个数字必然漏项，\n"+
			"而工具要求各序列长度严格相等，一旦不齐就整批失败（实测模型因此连错 6 次）。\n"+
			"只有当你手上已有一个**短**序列（<30 根）时才用显式数组模式。\n"+
			"\n"+
			"显式数组模式要求 highs/lows/closes 长度严格相等且按时间升序（旧→新）。\n"+
			"注意：VWAP 是累积型（从序列起点累加）。要按日重置需先用 trading_date 分组。",
		obj(map[string]any{
			"symbol":       str("标的代码（推荐）。给了它就自动取 K 线，无需传 highs/lows/closes"),
			"market":       str("市场：cn|hk|us|crypto。与 symbol 一起用"),
			"count":        intp("取多少根 K 线，默认 250，范围 20..5000。仅 symbol 模式有效"),
			"kline_period": str("K 线周期：daily|weekly|monthly，默认 daily。仅 symbol 模式有效"),
			"indicator":    str("指标名：macd|rsi|atr|boll|kdj|vwap"),
			"highs":        strArray("最高价序列（升序）。vwap/atr/kdj/boll 需要。仅显式数组模式"),
			"lows":         strArray("最低价序列（升序）。vwap/atr/kdj/boll 需要。仅显式数组模式"),
			"closes":       strArray("收盘价序列（升序）。全部指标都需要。仅显式数组模式"),
			"volumes":      strArray("成交量序列（升序）。仅 vwap 需要。仅显式数组模式"),
			"period":       intp("指标周期参数，默认按指标取（RSI/ATR=14，BOLL=20）"),
		}),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			if m.client == nil && meta.Timeout == 0 {
				// 理论上不会走到，仅防 meta 配置被误改
				return actool.Errorf("compute_indicator 配置异常"), nil
			}
			var a struct {
				Symbol      string   `json:"symbol"`
				Market      string   `json:"market"`
				Count       int      `json:"count"`
				KlinePeriod string   `json:"kline_period"`
				Indicator   string   `json:"indicator"`
				Highs       []string `json:"highs"`
				Lows        []string `json:"lows"`
				Closes      []string `json:"closes"`
				Volumes     []string `json:"volumes"`
				Period      int      `json:"period"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf(fmt.Sprintf("入参解析失败: %v", err)), nil
			}
			ind := strings.ToLower(strings.TrimSpace(a.Indicator))
			if ind == "" {
				return actool.Errorf("缺少 indicator"), nil
			}

			highs, lows, closes, volumes := a.Highs, a.Lows, a.Closes, a.Volumes
			symbol := strings.TrimSpace(a.Symbol)

			// ---- symbol 模式：自己去取 K 线 ----
			//
			// # 为什么加这个模式（2026-11-05 实测）
			//
			// 原实现要求调用方把 OHLCV 数组搬进来。而模型取 250 根 K 线后
			// **手动抄写**数组 —— 实测抄出 high=248 / low=249 / close=249
			// 这种不齐的长度，工具严格拒绝，它又反复重试 6 次仍失败。
			//
			// 根因不是能力问题而是接口设计：让 LLM 搬运 750+ 个数字必然出错。
			// 给 symbol 让工具自己取，彻底消除这个错误类型。
			if symbol != "" {
				market := strings.ToLower(strings.TrimSpace(a.Market))
				if market == "" {
					market = "cn"
				}
				if !marketAllowed(meta, market) {
					return actool.Errorf(fmt.Sprintf("compute_indicator 不支持 market=%q", market)), nil
				}
				period := strings.ToLower(strings.TrimSpace(a.KlinePeriod))
				if period == "" {
					period = "daily"
				}
				count := a.Count
				if count <= 0 {
					count = 250
				}
				if count > 5000 {
					count = 5000
				}
				cli, cErr := m.clientFor(market)
				if cErr != nil {
					return actool.Errorf(fmt.Sprintf("compute_indicator 取数未启用：%v", cErr)), nil
				}
				cctx, cancel := context.WithTimeout(ctx, meta.Timeout)
				defer cancel()
				bars, _, fErr := fetchKlineWithFallback(cctx, cli, symbol, market, period, count, meta)
				if fErr != nil {
					return actool.Errorf(fmt.Sprintf("compute_indicator 取 K 线失败: %v", fErr)), nil
				}
				// 只取已收盘的 bar —— 未收盘的 OHLC 还在变，算出来的指标会失真。
				// 与 get_kline 的 closedBars 语义一致。
				usable := make([]Bar, 0, len(bars))
				for _, b := range bars {
					if !b.IsPartial {
						usable = append(usable, b)
					}
				}
				if len(usable) == 0 {
					return actool.Errorf("没有已收盘的 K 线可用于计算（全部 is_partial）"), nil
				}
				if len(usable) < 20 {
					return actool.Errorf(fmt.Sprintf(
						"可用的已收盘 K 线只有 %d 根，不足以计算指标（至少需要 20 根）", len(usable))), nil
				}
				highs = make([]string, 0, len(usable))
				lows = make([]string, 0, len(usable))
				closes = make([]string, 0, len(usable))
				volumes = make([]string, 0, len(usable))
				for _, b := range usable {
					highs = append(highs, b.High)
					lows = append(lows, b.Low)
					closes = append(closes, b.Close)
					volumes = append(volumes, b.Volume)
				}
			} else if len(closes) == 0 {
				return actool.Errorf(
					"缺少数据：传 symbol + market 让工具自己取 K 线，" +
						"或者传 closes（及指标所需的 highs/lows）显式数组"), nil
			}

			// 纯计算仍受 meta.Timeout 保护 —— 防止超长序列卡住 agent 循环。
			cctx, cancel := context.WithTimeout(ctx, meta.Timeout)
			defer cancel()
			type result struct {
				payload map[string]any
				err     error
			}
			ch := make(chan result, 1)
			go func() {
				p, err := runIndicator(ind, highs, lows, closes, volumes, a.Period)
				if err == nil && symbol != "" {
					// 告诉调用方数据来源，便于它写 fact 时带上标的与根数。
					p["symbol"] = symbol
					p["bars_used"] = len(closes)
				}
				ch <- result{p, err}
			}()
			select {
			case <-cctx.Done():
				return actool.Errorf(fmt.Sprintf("compute_indicator 超时(%s)：序列过长或指标名错误", meta.Timeout)), nil
			case r := <-ch:
				if r.err != nil {
					return actool.Errorf(fmt.Sprintf("计算失败: %v", r.err)), nil
				}
				return jsonResult(r.payload)
			}
		},
	)
}

// runIndicator 按指标名分派到 indicators.go 的纯函数。
func runIndicator(ind string, highs, lows, closes, volumes []string, period int) (map[string]any, error) {
	switch ind {
	case "macd":
		macd, signal, hist, err := MACD(closes)
		if err != nil {
			return nil, err
		}
		return map[string]any{"indicator": "macd", "params": map[string]any{"fast": 12, "slow": 26, "signal": 9},
			"macd": macd, "signal": signal, "hist": hist}, nil
	case "rsi":
		if period <= 0 {
			period = 14
		}
		v, err := RSI(closes, period)
		if err != nil {
			return nil, err
		}
		return map[string]any{"indicator": "rsi", "params": map[string]any{"period": period}, "values": v}, nil
	case "atr":
		if period <= 0 {
			period = 14
		}
		v, err := ATR(highs, lows, closes, period)
		if err != nil {
			return nil, err
		}
		return map[string]any{"indicator": "atr", "params": map[string]any{"period": period}, "values": v}, nil
	case "boll":
		if period <= 0 {
			period = 20
		}
		up, mid, lo, err := BOLL(closes, period, 2.0)
		if err != nil {
			return nil, err
		}
		return map[string]any{"indicator": "boll", "params": map[string]any{"period": period, "k": 2.0},
			"upper": up, "mid": mid, "lower": lo}, nil
	case "kdj":
		k, d, j, err := KDJ(highs, lows, closes, 9, 3, 3)
		if err != nil {
			return nil, err
		}
		return map[string]any{"indicator": "kdj", "params": map[string]any{"k": 9, "d": 3, "smooth": 3},
			"k": k, "d": d, "j": j}, nil
	case "vwap":
		v, err := VWAP(highs, lows, closes, volumes)
		if err != nil {
			return nil, err
		}
		return map[string]any{"indicator": "vwap",
			"params": map[string]any{"mode": "cumulative",
				"note": "累积型。按日重置需调用方按 trading_date 分组后分别调用"},
			"values": v}, nil
	default:
		return nil, fmt.Errorf("未知指标 %q（支持 macd|rsi|atr|boll|kdj|vwap）", ind)
	}
}

// ---------------------------------------------------------------------
// get_quote：日线收盘价语义
// ---------------------------------------------------------------------

func (m *MarketToolSet) getQuote() actool.CoreTool {
	meta := marketToolMetas["get_quote"]
	return readTool(
		"get_quote",
		"查询标的最新行情。\n"+
			"⚠ 语义边界：返回的是**最新日线收盘价**，不是实时 tick。\n"+
			"  - 盘中调用 → 昨日收盘价（当日 K 线尚未收盘）\n"+
			"  - 盘后调用 → 当日收盘价\n"+
			"  - 周末/节假日 → 上一交易日收盘价\n"+
			"响应里的 trading_date 是价格所属交易日，务必据此判断时效性。\n"+
			"不要用它在盘中做「当前价」判断——会看到昨收。\n"+
			"覆盖市场：cn(A股) / hk(港股) / us(美股) / crypto(加密，走 OKX)。",
		obj(map[string]any{
			"symbol": str("标的代码。cn: 6位数字如 600519；hk: 5位如 00700；us: 字母如 AAPL；crypto: 如 BTC-USDT"),
			"market": str("市场：cn|hk|us|crypto"),
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
				market = "cn"
			}
			// assetClass 过滤：market 决定 asset_class 的候选范围。
			// 这里不查 instruments 表（该工具对任何合法代码都应尝试），
			// 只用 market 选出该市场的通道链。
			if !marketAllowed(meta, market) {
				return actool.Errorf(fmt.Sprintf("get_quote 不支持 market=%q（支持: cn|hk|us|crypto）", market)), nil
			}

			cli, err := m.clientFor(market)
			if err != nil {
				return actool.Errorf(fmt.Sprintf("get_quote 未启用：%v", err)), nil
			}

			cctx, cancel := context.WithTimeout(ctx, meta.Timeout)
			defer cancel()

			q, err := fetchQuoteWithFallback(cctx, cli, symbol, market, meta)
			if err != nil {
				// **不许返回空结果冒充「没数据」** —— 必须让调用方看见失败原因。
				return actool.Errorf(fmt.Sprintf("get_quote 失败: %v", err)), nil
			}
			var dateNote string
			if q.PriceKind == "daily_close" {
				dateNote = fmt.Sprintf("这是 %s 的日线收盘价，不是实时价。", q.TradingDate)
			}

			// 落快照：让 emit_fact 的 snapshot_ids 能指向真实证据。
			// 失败不阻断取数 —— 快照是附加证据链，不是主功能。
			// 未收盘数据（is_partial）刻意不落库，返回 0（见 snapshot_capture.go）。
			snapshotID := int64(0)
			if m.db != nil {
				if id, sErr := captureQuoteSnapshot(m.db, q); sErr != nil {
					slog.Warn("snapshot 落库失败（不阻断取数）",
						"endpoint", "get_quote", "symbol", q.Symbol, "market", q.Market,
						"err", sErr, "limitation", "L4")
				} else {
					snapshotID = id
				}
			}

			out := map[string]any{
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
				"note":         dateNote,
			}
			if snapshotID > 0 {
				out["snapshot_id"] = snapshotID
			} else if q.IsPartial {
				// 与 emit_fact 的 snapshot_absence_reason 同一语义：
				// 未收盘数据不落快照，但要说明原因，不能让调用方以为是漏了。
				out["snapshot_absent_reason"] = "unclosed_bar_not_captured"
			}
			return jsonResult(out)
		},
	)
}

// ---------------------------------------------------------------------
// get_kline
// ---------------------------------------------------------------------

func (m *MarketToolSet) getKline() actool.CoreTool {
	meta := marketToolMetas["get_kline"]
	return readTool(
		"get_kline",
		"查询 K 线（日/周/月），按时间升序（旧→新）返回。\n"+
			"覆盖市场：cn(A股) / hk(港股) / us(美股) / crypto(加密)。\n"+
			"多源降级：cn 走 新浪→腾讯，hk/us 只有新浪单源。\n"+
			"返回的 is_partial=true 表示该 bar 的 OHLC 有缺失（停牌/指数缺失），\n"+
			"  这类 bar 在计算指标时应跳过，**不要当 0 处理**。\n"+
			"注意：日线数据盘中取到的是「截至昨日」的序列，当日 K 线收盘后才有。",
		obj(map[string]any{
			"symbol": str("标的代码（同 get_quote）"),
			"market": str("市场：cn|hk|us|crypto"),
			"period": str("周期：daily|weekly|monthly，默认 daily"),
			"count":  intp("返回根数，默认 250，范围 1..5000"),
		}),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Symbol string `json:"symbol"`
				Market string `json:"market"`
				Period string `json:"period"`
				Count  int    `json:"count"`
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
			if !marketAllowed(meta, market) {
				return actool.Errorf(fmt.Sprintf("get_kline 不支持 market=%q", market)), nil
			}
			period := strings.ToLower(strings.TrimSpace(a.Period))
			if period == "" {
				period = "daily"
			}
			if period != "daily" && period != "weekly" && period != "monthly" {
				return actool.Errorf(fmt.Sprintf("period 非法: %q（daily|weekly|monthly）", period)), nil
			}
			count := a.Count
			if count <= 0 {
				count = 250
			}
			if count > 5000 {
				count = 5000
			}

			cli, err := m.clientFor(market)
			if err != nil {
				return actool.Errorf(fmt.Sprintf("get_kline 未启用：%v", err)), nil
			}

			cctx, cancel := context.WithTimeout(ctx, meta.Timeout)
			defer cancel()

			bars, ch, err := fetchKlineWithFallback(cctx, cli, symbol, market, period, count, meta)
			if err != nil {
				return actool.Errorf(fmt.Sprintf("get_kline 失败: %v", err)), nil
			}

			// 落快照。closedBars 在 captureKlineSnapshot 内部过滤未收盘 bar，
			// 全部未收盘时返回 0（不阻断取数）。
			snapshotID := int64(0)
			if m.db != nil {
				if id, sErr := captureKlineSnapshot(m.db, symbol, market, period, count, ch, bars); sErr != nil {
					slog.Warn("snapshot 落库失败（不阻断取数）",
						"endpoint", "get_kline", "symbol", symbol, "market", market,
						"err", sErr, "limitation", "L4")
				} else {
					snapshotID = id
				}
			}

			out := map[string]any{
				"symbol":  symbol,
				"market":  market,
				"period":  period,
				"count":   len(bars),
				"channel": string(ch),
				"bars":    bars,
			}
			if snapshotID > 0 {
				out["snapshot_id"] = snapshotID
			}
			return jsonResult(out)
		},
	)
}

// ---------------------------------------------------------------------
// 通道降级
// ---------------------------------------------------------------------

// marketAllowed 检查该工具是否支持该市场。
//
// 现阶段的映射：market → 需要的 Channel。cn/hk/us 走 akshare 对应通道，
// crypto 走 OKX。
func marketAllowed(meta marketToolMeta, market string) bool {
	switch market {
	case "cn", "hk", "us", "crypto":
		// 都允许；具体通道是否实现由 client 决定
		return len(meta.AssetClasses) > 0
	default:
		return false
	}
}

// channelForMarket 返回该市场的通道链（按 meta.Channels 的顺序过滤）。
func channelForMarket(meta marketToolMeta, market string) []Channel {
	var want []Channel
	switch market {
	case "cn":
		want = []Channel{ChannelAkshareCN}
	case "hk":
		want = []Channel{ChannelAkshareHK}
	case "us":
		want = []Channel{ChannelAkshareUS}
	case "crypto":
		want = []Channel{ChannelOKX}
	}
	var out []Channel
	for _, c := range meta.Channels {
		for _, w := range want {
			if c == w {
				out = append(out, c)
			}
		}
	}
	return out
}

// fetchQuoteWithFallback 按通道链依次尝试，返回首个成功结果。
//
// 降级条件严格：只有 MarketError.Retryable=true 才换下一个通道。
// SYMBOL_NOT_FOUND / BAD_REQUEST 不重试 —— 换源不会让不存在的标的存在。
func fetchQuoteWithFallback(ctx context.Context, client MarketClient, symbol, market string, meta marketToolMeta) (*Quote, error) {
	chans := channelForMarket(meta, market)
	if len(chans) == 0 {
		return nil, fmt.Errorf("market=%q 无可用通道", market)
	}
	var firstErr error
	for _, ch := range chans {
		q, err := client.FetchQuote(ctx, symbol, market, ch)
		if err == nil {
			return q, nil
		}
		var me *MarketError
		if errors.As(err, &me) {
			if me.Channel == "" {
				me.Channel = ch
			}
			if !me.Retryable {
				return nil, err // 不可重试：立刻返回，不浪费下一个通道
			}
		}
		if firstErr == nil {
			firstErr = err
		}
		// 上下文已取消/超时：不再尝试后续通道
		if ctx.Err() != nil {
			return nil, fmt.Errorf("已取消或超时: %w（最后错误: %v）", ctx.Err(), err)
		}
	}
	return nil, firstErr
}

// fetchKlineWithFallback 同上，返回实际使用的通道。
func fetchKlineWithFallback(ctx context.Context, client MarketClient, symbol, market, period string, count int, meta marketToolMeta) ([]Bar, Channel, error) {
	chans := channelForMarket(meta, market)
	if len(chans) == 0 {
		return nil, "", fmt.Errorf("market=%q 无可用通道", market)
	}
	var firstErr error
	for _, ch := range chans {
		bars, err := client.FetchKline(ctx, symbol, market, period, count, ch)
		if err == nil {
			return bars, ch, nil
		}
		var me *MarketError
		if errors.As(err, &me) {
			if me.Channel == "" {
				me.Channel = ch
			}
			if !me.Retryable {
				return nil, ch, err
			}
		}
		if firstErr == nil {
			firstErr = err
		}
		if ctx.Err() != nil {
			return nil, ch, fmt.Errorf("已取消或超时: %w（最后错误: %v）", ctx.Err(), err)
		}
	}
	return nil, "", firstErr
}

// strArray 是 JSON-Schema 的字符串数组属性。
func strArray(desc string) map[string]any {
	return map[string]any{
		"type":        "array",
		"items":       map[string]any{"type": "string"},
		"description": desc,
	}
}

// 确保 time 被使用（meta.Timeout 是 time.Duration）。
var _ = time.Second

// ---------------------------------------------------------------------
// get_macro：宏观指标序列
// ---------------------------------------------------------------------

// getMicroIndicator 是 akshareFetchMacro 的薄封装，方便测试替换。
// 提取成变量是为了让 get_volatility 的 VIX 降级路径能在测试里被触发。
var macroIndicators = map[string]bool{
	"cpi": true, "ppi": true, "pmi": true, "gdp": true,
	"m2": true, "shibor": true, "lpr": true,
}

func (m *MarketToolSet) getMacro() actool.CoreTool {
	meta := marketToolMetas["get_macro"]
	return readTool(
		"get_macro",
		"查询宏观经济指标的时间序列。\n"+
			"支持指标：cpi(居民消费价格) / ppi(工业生产者出厂价格) / pmi(制造业采购经理指数) /\n"+
			"  gdp(国内生产总值) / m2(货币供应) / shibor(银行间拆借利率) / lpr(贷款市场报价利率)。\n"+
			"返回 {series_name, unit, points:[{period, value}], channel}，points 按时间升序。\n"+
			"注意：这是**宏观层面**数据，不针对个股。判断个股时要结合其所属行业与市值。\n"+
			"数据来自 akshare 宏观接口，更新频率为月/季，不是日频。",
		obj(map[string]any{
			"indicator": str("指标代号：cpi|ppi|pmi|gdp|m2|shibor|lpr"),
		}, "indicator"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Indicator string `json:"indicator"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf(fmt.Sprintf("入参解析失败: %v", err)), nil
			}
			indicator := strings.ToLower(strings.TrimSpace(a.Indicator))
			if indicator == "" {
				return actool.Errorf("缺少 indicator"), nil
			}
			if !macroIndicators[indicator] {
				return actool.Errorf(fmt.Sprintf(
					"不支持的 indicator=%q（支持: cpi|ppi|pmi|gdp|m2|shibor|lpr）", indicator)), nil
			}
			cli, err := m.clientFor("cn") // 宏观指标走 akshare（cn 通道）
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			cctx, cancel := context.WithTimeout(ctx, meta.Timeout)
			defer cancel()
			macro, err := cli.FetchMacro(cctx, indicator)
			if err != nil {
				// 与 get_quote 同一原则：不返回空结果冒充「没数据」。
				return actool.Errorf(fmt.Sprintf("get_macro 失败: %v", err)), nil
			}
			if macro == nil {
				return actool.Errorf("get_macro 失败: 上游返回空序列"), nil
			}
			points := make([]map[string]string, 0, len(macro.Points))
			for _, p := range macro.Points {
				points = append(points, map[string]string{"period": p.Period, "value": p.Value})
			}
			out := map[string]any{
				"indicator":   indicator,
				"series_name": macro.SeriesName,
				"unit":        macro.Unit,
				"points":      points,
				"point_count": len(points),
				"channel":     string(macro.Channel),
			}
			// 落快照：与 get_kline/get_quote 同一套证据链。
			snapshotID := int64(0)
			if m.db != nil {
				if id, sErr := captureMacroSnapshot(m.db, indicator, macro); sErr != nil {
					slog.Warn("snapshot 落库失败（不阻断取数）",
						"endpoint", "get_macro", "indicator", indicator,
						"err", sErr, "limitation", "L4")
				} else {
					snapshotID = id
				}
			}
			if snapshotID > 0 {
				out["snapshot_id"] = snapshotID
			}
			return jsonResult(out)
		})
}

// ---------------------------------------------------------------------
// get_volatility：波动率两层读数（标的自身 ATR% + 市场 ^VIX）
// ---------------------------------------------------------------------

func (m *MarketToolSet) getVolatility() actool.CoreTool {
	meta := marketToolMetas["get_volatility"]
	return readTool(
		"get_volatility",
		"波动率两层读数：\n"+
			"  1. 标的自身：用最近 K 线算 ATR14，再除以最新收盘得 atr_pct（单日波幅占价格的比例）\n"+
			"  2. 市场整体：^VIX 快照（仅 us 市场有意义，其余市场返回 unavailable_reason）\n"+
			"为什么两层都给：atr_pct 是「这只标的波动多大」，VIX 是「市场整体多恐慌」。\n"+
			"  只给 atr_pct 时你无法判断「它的波动相对大盘是高还是低」。\n"+
			"**缺失必须当缺失**：非 us 市场或 VIX 取不到时，vix_level 为空且 vix_unavailable_reason\n"+
			"  写明原因 —— 不要把它当成「波动率低」。\n"+
			"参考阈值：atr_pct > 4% 高波动，2%-4% 中，< 2% 低。",
		obj(map[string]any{
			"symbol": str("标的代码（同 get_quote）"),
			"market": str("市场：cn|hk|us|crypto"),
			"count":  intp("用于算 ATR 的 K 线根数，默认 60，范围 20..500"),
		}),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Symbol string `json:"symbol"`
				Market string `json:"market"`
				Count  int    `json:"count"`
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
			if !marketAllowed(meta, market) {
				return actool.Errorf(fmt.Sprintf("get_volatility 不支持 market=%q", market)), nil
			}
			count := a.Count
			if count <= 0 {
				count = 60
			}
			if count < 20 {
				count = 20
			}
			if count > 500 {
				count = 500
			}

			cli, err := m.clientFor(market)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			cctx, cancel := context.WithTimeout(ctx, meta.Timeout)
			defer cancel()

			// ---- 标的自身的 ATR% ----
			bars, ch, err := fetchKlineWithFallback(cctx, cli, symbol, market, "daily", count, meta)
			if err != nil {
				return actool.Errorf(fmt.Sprintf("get_volatility 取 K 线失败: %v", err)), nil
			}
			closed := closedBars(bars)
			if len(closed) < 15 {
				return actool.Errorf(fmt.Sprintf(
					"get_volatility: 已收盘 K 线仅 %d 根，不足以算 ATR14（需 ≥15）", len(closed))), nil
			}
			highs := make([]string, 0, len(closed))
			lows := make([]string, 0, len(closed))
			closes := make([]string, 0, len(closed))
			for _, b := range closed {
				highs = append(highs, b.High)
				lows = append(lows, b.Low)
				closes = append(closes, b.Close)
			}
			atrVals, err := ATR(highs, lows, closes, 14)
			if err != nil {
				return actool.Errorf(fmt.Sprintf("get_volatility 计算 ATR 失败: %v", err)), nil
			}
			// ATR 的 warmup 期返回空字符串（不是 0）—— 取最后一个非空值。
			atr := ""
			for i := len(atrVals) - 1; i >= 0; i-- {
				if strings.TrimSpace(atrVals[i]) != "" {
					atr = atrVals[i]
					break
				}
			}
			if atr == "" {
				return actool.Errorf(fmt.Sprintf(
					"get_volatility: 全部 %d 根 K 线都落在 ATR14 的 warmup 期，无法得出波动率", len(closed))), nil
			}
			lastClose := closed[len(closed)-1].Close
			atrPct := ""
			if atr != "" && lastClose != "" {
				if f, ok := pctOf(atr, lastClose); ok {
					atrPct = f
				}
			}

			out := map[string]any{
				"symbol":     symbol,
				"market":     market,
				"channel":    string(ch),
				"bar_count":  len(closed),
				"last_close": lastClose,
				"last_date":  closed[len(closed)-1].TradingDate,
				"atr14":      atr,
				"atr_pct":    atrPct,
			}

			// ---- ^VIX 市场恐慌度（仅 us） ----
			//
			// 非 us 市场直接给原因，不发请求 —— 避免用 akshare-cn 去查一个美股指数。
			if market != "us" {
				out["vix_unavailable_reason"] = fmt.Sprintf(
					"^VIX 是美股市场的波动率指数，对 market=%q 无意义", market)
			} else {
				vixMeta := marketToolMetas["get_quote"]
				vctx, vcancel := context.WithTimeout(ctx, vixMeta.Timeout)
				vq, vErr := cli.FetchQuote(vctx, "^VIX", "us", "")
				vcancel()
				if vErr != nil {
					out["vix_unavailable_reason"] = fmt.Sprintf(
						"^VIX 行情取不到（%v）—— 这是数据缺失，不是波动率低", vErr)
				} else if vq == nil || strings.TrimSpace(vq.Last) == "" {
					out["vix_unavailable_reason"] = "^VIX 返回空值 —— 这是数据缺失，不是波动率低"
				} else {
					out["vix_level"] = vq.Last
					out["vix_date"] = vq.TradingDate
				}
			}

			return jsonResult(out)
		})
}

// pctOf 返回 a/b 的百分比字符串（保留 4 位小数）。任一非数或 b=0 时返回 ok=false。
func pctOf(a, b string) (string, bool) {
	af, err := strconv.ParseFloat(strings.TrimSpace(a), 64)
	if err != nil {
		return "", false
	}
	bf, err := strconv.ParseFloat(strings.TrimSpace(b), 64)
	if err != nil || bf == 0 {
		return "", false
	}
	return strconv.FormatFloat(af/bf*100, 'f', 4, 64), true
}
