package agent

// OKX 服务端技术指标工具。
//
// # 与 compute_indicator 的分工
//
//	compute_indicator  本地算 6 种（MACD/RSI/ATR/BOLL/KDJ/VWAP）
//	                   —— 离线可用、无网络、用于 bars 预处理
//	okx_indicator      查 OKX 服务端 70+ 种
//	                   —— 需网络、支持 backtest-time 历史时点回溯
//
// # 为什么两个都要
//
// 本地实现不了「历史时点回溯」：要用当时可见的 bars 重算，而我们现在只有
// 最新一份快照。服务端有 `--backtest-time <ms>`，能给出「在那个时刻，RSI 是多少」——
// 这是防未来函数的关键能力（回测时不能偷看未来数据）。
//
// 但服务端要网络。离线场景（sidecar 未起、网络受限）仍需本地算。
// 所以两者是互补而非替代。

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	actool "github.com/Autumn-27/norma/tool"
)

// okxIndicatorInput 是 get_indicator_okx 的入参。
type okxIndicatorInput struct {
	Indicator    string `json:"indicator"`
	Symbol       string `json:"symbol"`
	Bar          string `json:"bar"`
	Params       string `json:"params"`
	BacktestTime int64  `json:"backtest_time"`
}

// okxBarHelp 是给模型看的 bar 白名单说明。
//
// 单独抽出来是因为它出现两次（schema 的 description + 错误信息），
// 两处写不同会让人以为是两个约束。
const okxBarHelp = "K 线周期，默认 1Dutc。" +
	"可选：3m / 5m / 15m / 1H / 4H / 12Hutc / 1Dutc / 3Dutc / 1Wutc" +
	"（注意是 1Dutc，不是 1D —— CLI 不接受 1D）"

// getIndicatorOkx 查 OKX 服务端技术指标。
func (m *MarketToolSet) getIndicatorOkx() actool.CoreTool {
	meta := marketToolMetas["get_indicator_okx"]
	return readTool(
		"get_indicator_okx",
		"查询 OKX **服务端**计算的技术指标（70+ 种，覆盖远超本地 compute_indicator 的 6 种）。\n"+
			"支持的指标举例：rsi / macd / boll / kdj / ema / sma / atr / obv / cci / willr 等\n"+
			"（具体清单取决于 OKX 服务端，本工具透传，不做白名单 —— 传错指标名会返回\n"+
			"「响应里没有 indicator=xxx（实际有: [...]）」，从错误里就能看到可用名字）。\n\n"+
			"**与 compute_indicator 的分工**：\n"+
			"  - compute_indicator 在本地算 6 种，离线可用，适合批量预处理\n"+
			"  - 本工具查服务端，支持 backtest_time 做**历史时点回溯**（防未来函数）\n"+
			"回测/复盘场景要用本工具，因为本地算只能基于最新快照、会偷看未来。\n\n"+
			"返回 {indicator, symbol, bar, ts, values:{...}, backtest_time, source, snapshot_id}。\n"+
			"values 的字段随指标而变：rsi 是 {\"14\": \"39.47\"}，macd 是 {\"dif\":\"..\",\"dea\":\"..\",\"macd\":\"..\"}。\n"+
			"数值一律是 string（与全项目的精度铁律一致，解析前不要当 float 用）。",
		obj(map[string]any{
			"indicator":     str("指标名，必填。如 rsi / macd / boll / kdj / ema / atr。大小写不敏感"),
			"symbol":        str("交易对 instId，必填。如 BTC-USDT（必须带连字符，不能只写 BTC）"),
			"bar":           str(okxBarHelp),
			"params":        str("指标参数，可选。如 rsi 传 \"14\"，macd 传 \"12,26,9\"。留空用服务端默认"),
			"backtest_time": intp("历史回溯时间点（unix 毫秒），可选。0 或省略 = 实时。用于回测防未来函数"),
		}, "indicator", "symbol"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a okxIndicatorInput
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf(fmt.Sprintf("入参解析失败: %v", err)), nil
			}
			// 参数校验都在 client 里做（FetchIndicator），这里只做「能不能连上」的判断。
			//
			// 为什么校验放 client：client 是这套约束的真正拥有者（它知道 CLI 只认
			// 9 个 bar），放在工具层会有两份校验漂移的风险。工具层只负责
			// 「拿到 client」和「落快照」。
			cli, err := m.indicatorClient()
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			cctx, cancel := context.WithTimeout(ctx, meta.Timeout)
			defer cancel()

			res, fErr := cli.FetchIndicator(cctx, IndicatorQuery{
				Indicator:    a.Indicator,
				Symbol:       a.Symbol,
				Bar:          a.Bar,
				Params:       a.Params,
				BacktestTime: a.BacktestTime,
			})
			if fErr != nil {
				// 与 get_quote/get_macro 同一原则：不返回空结果冒充「没数据」。
				return actool.Errorf(fmt.Sprintf("get_indicator_okx 失败: %v", fErr)), nil
			}
			if res == nil || len(res.Values) == 0 {
				return actool.Errorf(fmt.Sprintf(
					"get_indicator_okx 失败: 服务端返回空 values（indicator=%s symbol=%s bar=%s）",
					a.Indicator, a.Symbol, a.Bar)), nil
			}

			out := map[string]any{
				"indicator":     res.Indicator,
				"symbol":        res.Symbol,
				"bar":           res.Bar,
				"ts":            res.TS,
				"values":        res.Values,
				"backtest_time": res.BacktestTime,
				"source":        res.Source,
			}

			// 落快照：与 get_kline/get_macro 同一套证据链。
			// 落库失败**不阻断取数** —— 快照是附加证据，不是主功能。
			if m.db != nil {
				if id, sErr := captureIndicatorSnapshot(m.db, res); sErr != nil {
					slog.Warn("snapshot 落库失败（不阻断取数）",
						"endpoint", "get_indicator_okx",
						"indicator", res.Indicator, "symbol", res.Symbol, "err", sErr)
				} else if id > 0 {
					out["snapshot_id"] = id
				}
			}
			return jsonResult(out)
		})
}

// indicatorClient 返回服务端指标客户端。
//
// 已录的 MarketToolSet 只有 crypto（OKX）能提供指标，所以直接取 crypto 字段
// 并断言它实现了 IndicatorClient。
//
// 断言失败说明装配错了（注入了一个不实现 FetchIndicator 的 crypto client）——
// 明确报出来，不要静默降级成「该通道不支持」。
func (m *MarketToolSet) indicatorClient() (IndicatorClient, error) {
	if m.crypto == nil {
		return nil, fmt.Errorf("okx 通道未配置（crypto 客户端未注入），get_indicator_okx 不可用")
	}
	ic, ok := m.crypto.(IndicatorClient)
	if !ok {
		return nil, fmt.Errorf("已注入的 crypto 客户端未实现 IndicatorClient（fetchIndicator）—— " +
			"装配错误：只有 OkxClient 支持服务端指标")
	}
	return ic, nil
}

// normalizeIndicatorName 把用户输入归一成小写、去空格（缓存 key 用）。
func normalizeIndicatorName(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
