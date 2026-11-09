package agent

// get_news / get_fundamentals：新闻与财报取数工具。
//
// # 为什么这两个工具单独一个文件
//
// tools_market.go 已经 800 行（quote/kline/indicator/macro/volatility），
// 再塞两个自成一类的取数端点会让人按「文件长度」而非「能力边界」去找代码。
// 新闻与财报的共同点是：它们只挂在 *AkshareClient 上、不进 MarketClient 接口
// （理由见 market_news.go 文件头），是「非行情类金融数据」这一档。
//
// # 为什么用类型断言而不是给 MarketToolSet 加一个 *AkshareClient 字段
//
// 两个方案都能拿到 FetchNews：
//
//	A) 加字段 akshare *AkshareClient，构造器里赋值
//	B) 调用点对 m.client 做类型断言
//
// 选 B。理由：
//  1. **不引入「同一依赖存在两份」的漂移风险**。m.client 已经是 akshare 实例
//     （生产装配见 server/server.go：s.marketCl = agent.NewAkshareClient("")），
//     再存一个字段就有两个指向同一实例的指针；将来若有人只更新其中一个
//     （例如测试里换 client），新闻会静默走了旧实例，而行情走了新的。
//     断言只有「一个事实来源」，不存在不一致。
//  2. **构造器零改动**。三个构造器（NewMarketToolSet / WithCrypto /
//     WithCryptoAndFlow）都被测试和工具目录种子壳（nil client）调用，
//     加参数会让所有调用点跟着改，而收益为零。
//  3. nil 安全天然成立：nil client 断言失败 → 返回「未注入」而不是 nil deref。
//     种子壳（builtinToolsByAgent 用 NewMarketToolSet(nil)）只读
//     Name/Description/InputSchema，永不 Call，这条路径也不受影响。
//
// 代价是断言失败时要给一条清楚的错误（见 newsSource 的返回值），
// 而不是静默返回空结果。

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	actool "github.com/Autumn-27/norma/tool"

	"github.com/Autumn-27/artex/db"
)

// reportTypes 是 /fundamental 支持的报表类型（与 sidecar 的 _KEY_ROWS 键一致）。
var reportTypes = map[string]bool{
	"资产负债表":    true,
	"利润表":      true,
	"现金流量表":    true,
	"abstract": true,
}

// newsSource 取可用的 *AkshareClient。
//
// 返回第二值是「为什么不可用」的说明 —— 直接拼进工具错误里，
// 让模型看到原因（「通道未注入」≠「没有数据」）。
func (m *MarketToolSet) newsSource(name string) (*AkshareClient, error) {
	if m.client == nil {
		return nil, fmt.Errorf("%s 未启用：MarketClient 未注入", name)
	}
	ak, ok := m.client.(*AkshareClient)
	if !ok {
		// 走到这里说明装配把 crypto/别的实现塞进了 client 位。
		// 不静默返回空：那是「装配错误」伪装成「没有新闻」。
		return nil, fmt.Errorf(
			"%s 只支持 akshare 通道，当前 MarketClient 是 %T（新闻与财报不进 MarketClient 接口，见 market_news.go）",
			name, m.client)
	}
	return ak, nil
}

// ---------------------------------------------------------------------
// get_news
// ---------------------------------------------------------------------

func (m *MarketToolSet) getNews() actool.CoreTool {
	meta := marketToolMetas["get_news"]
	return readTool(
		"get_news",
		"查询财经新闻。\n"+
			"覆盖市场：cn / us / hk。**不支持 crypto** —— 现有新闻源都是股票市场文本，\n"+
			"  没有加密新闻（加密的消息面用 mx_news 或 get_crypto_flow 的资金流判断）。\n"+
			"symbol 可省略：省略时返回该市场的全量快照（按 limit 截断）；给出时**优先按代码\n"+
			"  精确取该标的的新闻**，取不到才降级到全市场快照做中文别名匹配（英伟达/NVDA 等）。\n"+
			"返回的每条含 title/summary/source(caixin|cls|eastmoney)/published/url。\n"+
			"  published 与 url **都可能为空**：财新无时间列（日期从 URL 抠）、财联社无原文链接。\n"+
			"  这是上游限制，不是解析漏了 —— 空值不要当成「很久以前」或「没有链接的假新闻」。\n"+
			"「该标的近期无相关新闻」会返回明确的 NO_DATA 错误（不是空列表），\n"+
			"  因为「没新闻」是有效结论，必须能被上层区分出来。\n"+
			"提示：想看宏观/市场层面的消息而不限定标的时，去掉 symbol 传 limit 更大值。",
		obj(map[string]any{
			"symbol": str("可选。标的代码（cn: 6位数字；hk: 5位；us: 字母）。省略表示按市场取全量"),
			"market": str("市场：cn|hk|us（默认按标的推，缺省 cn）"),
			"limit":  intp("返回条数，默认 20，范围 1..50（每条新闻进 prompt 都花 token）"),
		}),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Symbol string `json:"symbol"`
				Market string `json:"market"`
				Limit  int    `json:"limit"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf(fmt.Sprintf("入参解析失败: %v", err)), nil
			}
			market := strings.ToLower(strings.TrimSpace(a.Market))
			if market == "" {
				market = "cn"
			}
			// 显式拒绝 crypto 与未知市场 —— 不返回空结果冒充「没新闻」。
			if !marketAllowed(meta, market) || !validNewsMarkets[market] {
				return actool.Errorf(fmt.Sprintf(
					"get_news 不支持 market=%q（支持 cn|hk|us）。"+
						"加密没有接入新闻源：财新与财联社都是股票市场文本，硬查会得到空结果，"+
						"而空结果会被误读成「无消息面事件」", market)), nil
			}
			limit := a.Limit
			if limit <= 0 {
				limit = 20
			}
			if limit > 50 {
				limit = 50
			}

			ak, err := m.newsSource("get_news")
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			cctx, cancel := context.WithTimeout(ctx, meta.Timeout)
			defer cancel()

			items, err := ak.FetchNews(cctx, strings.TrimSpace(a.Symbol), market, limit)
			if err != nil {
				// NO_DATA 与源故障分开表述 —— 前者不该重试，后者该重试。
				if IsNoDataError(err) {
					return actool.Errorf(fmt.Sprintf(
						"get_news: %v（这是「查得到但没有」的合法结论，重试无用）", err)), nil
				}
				return actool.Errorf(fmt.Sprintf("get_news 失败: %v", err)), nil
			}

			snapshotID := int64(0)
			if m.db != nil {
				if id, sErr := captureNewsSnapshot(m.db, a.Symbol, market, items); sErr != nil {
					slog.Warn("snapshot 落库失败（不阻断取数）",
						"endpoint", "get_news", "market", market, "err", sErr, "limitation", "L4")
				} else {
					snapshotID = id
				}
			}

			out := map[string]any{
				"symbol":      strings.TrimSpace(a.Symbol),
				"market":      market,
				"count":       len(items),
				"items":       items,
				"channel":     string(ChannelAkshareCN),
				"note":        "published/url 可能为空：财新无时间列、财联社无原文链接（上游限制）",
				"source_note": "财新偏深度（含美股评论），财联社偏快讯（含美股盘后异动），两源互补",
			}
			if snapshotID > 0 {
				out["snapshot_id"] = snapshotID
			}
			return jsonResult(out)
		},
	)
}

// ---------------------------------------------------------------------
// get_fundamentals
// ---------------------------------------------------------------------

func (m *MarketToolSet) getFundamentals() actool.CoreTool {
	meta := marketToolMetas["get_fundamentals"]
	return readTool(
		"get_fundamentals",
		"查询财报数据（最近 4 期关键科目）。\n"+
			"report_type：资产负债表 / 利润表 / 现金流量表 / abstract（摘要，默认）。\n"+
			"返回 {symbol, market, report_type, currency, periods, columns, rows, note}：\n"+
			"  rows 每行是一个科目（首列「科目」/「指标」，其余列是报告期，**新→旧**）。\n"+
			"⚠ 只有 A 股（cn）通道。**美股/港股返回明确的 NO_DATA 错误**，原因不是故障：\n"+
			"  唯一能取美股财报的接口是东财 stock_financial_us_report_em，它以 _em 结尾，\n"+
			"  而东财对本机 IP 是封禁/可变的 —— 该接口失效时返回空 DataFrame 而不抛异常，\n"+
			"  调用方会把「通道死了」当成「这家公司没有财报」。宁可明确说「待接入」。\n"+
			"⚠ 已截断：只保留白名单科目 × 最近 4 期。完整报表 104 行 × 147 列会吃掉数万 token。\n"+
			"⚠ 不含 PE/PB/ROE —— 需要股价与股本才能算，本工具不提供，别凭印象填。",
		obj(map[string]any{
			"symbol":      str("必填。A 股代码（6位数字，如 600519；也接受 sh600519/sz000001）"),
			"market":      str("市场：cn|us|hk（默认 cn）。us/hk 当前返回 NO_DATA（通道待接入）"),
			"report_type": str("报表类型：资产负债表|利润表|现金流量表|abstract，默认 abstract"),
		}, "symbol"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Symbol     string `json:"symbol"`
				Market     string `json:"market"`
				ReportType string `json:"report_type"`
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
			if !marketAllowed(meta, market) || !validNewsMarkets[market] {
				return actool.Errorf(fmt.Sprintf(
					"get_fundamentals 不支持 market=%q（支持 cn|us|hk；加密资产无财报）", market)), nil
			}
			reportType := strings.TrimSpace(a.ReportType)
			if reportType == "" {
				reportType = "abstract"
			}
			if !reportTypes[reportType] {
				return actool.Errorf(fmt.Sprintf(
					"report_type 非法: %q（应为 资产负债表|利润表|现金流量表|abstract）", reportType)), nil
			}

			ak, err := m.newsSource("get_fundamentals")
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			cctx, cancel := context.WithTimeout(ctx, meta.Timeout)
			defer cancel()

			f, err := ak.FetchFundamental(cctx, symbol, market, reportType)
			if err != nil {
				if IsNoDataError(err) {
					return actool.Errorf(fmt.Sprintf(
						"get_fundamentals: %v —— 这是**能力缺口**（该市场财报通道未接入），"+
							"不是数据源故障，重试无用。请把这条写进 data_gaps，不要推测财报数字", err)), nil
				}
				return actool.Errorf(fmt.Sprintf("get_fundamentals 失败: %v", err)), nil
			}

			snapshotID := int64(0)
			if m.db != nil {
				if id, sErr := captureFundamentalSnapshot(m.db, f); sErr != nil {
					slog.Warn("snapshot 落库失败（不阻断取数）",
						"endpoint", "get_fundamentals", "symbol", symbol,
						"err", sErr, "limitation", "L4")
				} else {
					snapshotID = id
				}
			}

			out := map[string]any{
				"symbol":      f.Symbol,
				"market":      f.Market,
				"report_type": f.ReportType,
				"currency":    f.Currency,
				"periods":     f.Periods,
				"columns":     f.Columns,
				"rows":        f.Rows,
				"row_count":   len(f.Rows),
				"note":        f.Note,
				"channel":     string(ChannelAkshareCN),
			}
			if snapshotID > 0 {
				out["snapshot_id"] = snapshotID
			}
			return jsonResult(out)
		},
	)
}

// ---------------------------------------------------------------------
// 快照落库
// ---------------------------------------------------------------------

// snapshotTimeOrNow 把业务时间规整成 PG 可解析的时间字面量。
//
// # 为什么不吃「采集时间」
//
// 与 captureMacroSnapshot 同一原则：collected_at 用**数据的业务时间**，
// 这样同一份数据重复取会命中去重。新闻用最新一条的 published，
// 财报用最新报告期。两者都可能为空（财新无日期 / 报表缺报告日）——
// 那时退化成今天的日期：宁可让去重粒度变粗，也不要让快照写不进去。
func snapshotTimeOrNow(candidate string) string {
	s := strings.TrimSpace(candidate)
	if s == "" {
		return time.Now().UTC().Format("2006-01-02")
	}
	// 只保留 PG 一定认的前 19 个字符（"2026-10-30 14:00:00" / "2026-10-30"）。
	// 上游可能带时区后缀或中文，超出的部分截掉比让 INSERT 报错好。
	if len(s) > 19 {
		s = s[:19]
	}
	return s
}

// captureNewsSnapshot 为一次新闻取数落一条快照。
//
// checksum 覆盖每条新闻的 source+published+title —— 只哈希「我们实际使用的字段」，
// 与 quoteChecksum 同一原则（见 snapshot_capture.go 文件头）。
func captureNewsSnapshot(store *db.DB, symbol, market string, items []NewsItem) (int64, error) {
	if store == nil {
		return 0, fmt.Errorf("落快照失败: DB 未初始化")
	}
	if len(items) == 0 {
		return 0, nil // 无条目：没有可比对的业务时间，也没有内容可哈希
	}
	parts := make([]string, 0, len(items)*3+2)
	parts = append(parts, market, symbol)
	for _, it := range items {
		parts = append(parts, it.Source, it.Published, it.Title)
	}
	params := map[string]string{
		"symbol": symbol, "market": market,
		"limit": fmt.Sprintf("%d", len(items)),
	}
	snap, _, err := store.Snapshots().InsertSnapshot(db.SnapshotInput{
		Source:      "akshare",
		Endpoint:    "/news",
		ParamsHash:  snapshotParamsHash("news", params),
		CollectedAt: snapshotTimeOrNow(items[0].Published),
		Checksum:    snapshotChecksum(parts...),
	})
	if err != nil {
		return 0, fmt.Errorf("落新闻快照失败: %w", err)
	}
	return snap.ID, nil
}

// captureFundamentalSnapshot 为一次财报取数落一条快照。
//
// collected_at 取最新报告期（periods[0]，新→旧），同一个月内重复查询会命中
// 去重 —— 财报不会在季中变化。
func captureFundamentalSnapshot(store *db.DB, f *Fundamental) (int64, error) {
	if store == nil {
		return 0, fmt.Errorf("落快照失败: DB 未初始化")
	}
	if f == nil || len(f.Rows) == 0 {
		return 0, nil
	}
	parts := make([]string, 0, len(f.Periods)+len(f.Rows)*4)
	parts = append(parts, f.Symbol, f.Market, f.ReportType)
	parts = append(parts, f.Periods...)
	for _, row := range f.Rows {
		for _, c := range f.Columns {
			parts = append(parts, row[c])
		}
	}
	latest := ""
	if len(f.Periods) > 0 {
		latest = f.Periods[0]
	}
	params := map[string]string{
		"symbol": f.Symbol, "market": f.Market, "report_type": f.ReportType,
	}
	snap, _, err := store.Snapshots().InsertSnapshot(db.SnapshotInput{
		Source:      "akshare",
		Endpoint:    "/fundamental",
		ParamsHash:  snapshotParamsHash("fundamental", params),
		CollectedAt: snapshotTimeOrNow(latest),
		Checksum:    snapshotChecksum(parts...),
	})
	if err != nil {
		return 0, fmt.Errorf("落财报快照失败: %w", err)
	}
	return snap.ID, nil
}
