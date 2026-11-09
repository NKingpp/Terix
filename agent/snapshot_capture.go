package agent

// 行情快照落库：把取数结果写成 snapshots 行，让 emit_fact 的 snapshot_ids
// 能指向真实证据。
//
// # 为什么在工具层而不在 MarketClient 里
//
// AkshareClient / OkxClient 不该知道 snapshots 表的存在。如果让 client 写 snapshot，
// 就得给它一个 *db.DB，那就破坏了 MarketToolSet 零 DB 依赖（可用假 client 单测）
// 这个前提。所以 client 保持无状态纯取数，落库在工具层。
//
// # 三要素的设计依据
//
// snapshots 的去重键是 (source, endpoint, params_hash, date_trunc('second', collected_at))。
// 三者各自的作用：
//   - params_hash：区分「不同请求」。必须含所有影响结果的参数（见各 endpoint 的清单）。
//   - collected_at：用**数据的业务时间**，不是采集时间。这是 W1-5 定的 INVARIANT。
//   - checksum：检测「同 dedup key 下内容是否真变了」。见 KNOWN-LIMITATIONS L3。
//
// # 已知限制
//
// ON CONFLICT DO NOTHING 在「同秒同参数但内容不同」时静默丢弃后者（L3）。
// InsertSnapshot 会记 warn 让这事可见，但不改数据语义。

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
)

// snapshotParamsHash 计算请求参数的确定性指纹。
//
// 格式：v1|<endpoint>|<k1>=<v1>|<k2>=<v2>|...
//   - v1 前缀：允许未来改格式而不与旧快照混淆
//   - endpoint 前置：不同端点的同名参数不会撞
//   - 键按字典序：字段顺序固定，不依赖调用方传参顺序
//   - 值 url.QueryEscape：防止值里含 | 破坏结构
//
// **调用方必须把「所有影响结果的参数」都放进 params。** 漏一个就会让
// 两次不同请求算出同一个 hash 而错误去重。各 endpoint 的清单：
//
//	kline → symbol, market, period, count, channel
//	quote → symbol, market, channel
//	macro → indicator
//
// channel 必须进 hash：akshare 的 cn/hk/us 是不同市场；okx 与 akshare 对同一
// symbol 返回的数据也不同（日线收盘价 vs 实时价）。
//
// **as_of 当前不进 hash**：它是分析上下文，不影响实际请求（get_kline 拿的是
// 最新数据，不是 as_of 那天的）。若将来实现「as_of 命中本地 bars」的分支，
// as_of 就成了真实请求参数，必须加进来。
func snapshotParamsHash(endpoint string, params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString("v1|")
	b.WriteString(endpoint)
	for _, k := range keys {
		b.WriteString("|")
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(url.QueryEscape(params[k]))
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// snapshotChecksum 计算响应内容的确定性指纹。
//
// 只哈希「我们实际使用的字段」，按固定顺序拼接。
//
// 为什么不是原始响应体：MarketClient 返回的是已解析结构，raw JSON 在
// akshare_client.do() 里解析后就丢了。给它加 Raw []byte 会污染领域结构
// （Bar 是 22055 行，每行带 raw 内存会炸）。而 checksum 的用途是
// 「检测同 dedup key 下内容是否真变了」，不是完整取证 —— 未被解析的字段
// 不影响业务决策。若将来需要原始响应取证，正解是 sidecar 侧落 raw。
//
// **字段顺序必须与 Bar/Quote 结构体的字段声明顺序一致。** 否则将来有人
// 重排结构体字段，checksum 计算会静默改变（同样的数据算出不同 checksum，
// 被误判为「内容变了」）。
//
// 用 \x1f（ASCII Unit Separator）分隔：数字字符串不含控制字符，
// 比 | 更安全（值里可能含 |，不会含 \x1f）。
func snapshotChecksum(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return hex.EncodeToString(sum[:])
}

// barsChecksum 按 Bar 的字段声明顺序拼出 checksum。
// Bar: TradingDate, Open, High, Low, Close, Volume, Amount, IsPartial
func barsChecksum(bars []Bar) string {
	parts := make([]string, 0, len(bars)*8)
	for _, b := range bars {
		parts = append(parts,
			b.TradingDate, b.Open, b.High, b.Low, b.Close,
			b.Volume, b.Amount, boolStr(b.IsPartial),
		)
	}
	return snapshotChecksum(parts...)
}

// quoteChecksum 按 Quote 的字段声明顺序拼出 checksum。
// Quote: TradingDate, Last, Open, High, Low, PrevClose, ChangePct, Volume, Amount, IsPartial
func quoteChecksum(q *Quote) string {
	return snapshotChecksum(
		q.TradingDate, q.Last, q.Open, q.High, q.Low,
		q.PrevClose, q.ChangePct, q.Volume, q.Amount, boolStr(q.IsPartial),
	)
}

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// closedBars 过滤掉未收盘的 bar。
//
// **为什么必须过滤**：未收盘 bar 的 trading_date 是当天，落 snapshot 会让
// 「同一天不同时刻」撞 dedup key（KNOWN-LIMITATIONS L3 的触发场景：
// 09:30 与 14:00 取到不同价格但相同 collected_at → 后者被静默丢弃）。
//
// P0 只在盘后跑分析（当日 K 线已收盘），所以正常情况下所有 bar 都是已收盘的。
// 盘中调 get_quote 拿日线收盘价时 is_partial 恒为 false。
//
// 若将来要盘中跑分析，两处必须改：
//  1. 未收盘 bar 的 collected_at 带上采集时刻（秒精度）
//  2. 或对 is_partial=true 的 bar 跳过 snapshot 落库（只用于实时展示）
//
// 当前按方案 2 实现：不落库，返回数据但 snapshot_ids 为空，
// emit_fact 会带 snapshot_absence_reason 留痕。
func closedBars(bars []Bar) []Bar {
	out := make([]Bar, 0, len(bars))
	for _, b := range bars {
		if !b.IsPartial {
			out = append(out, b)
		}
	}
	return out
}

// captureKlineSnapshot 为 K 线响应落一条快照，返回 snapshot id。
//
// 返回 ("", nil) 表示「没有可落库的数据」（全部 bar 未收盘）——
// 这不是错误，调用方应把空 id 传给 emit_fact 的 snapshot_ids。
//
// collectedAt 用**最后一根已收盘 bar 的 trading_date**（天精度）。
// 日频策略的语义是「每个交易日一份数据」：同一天取 3 次，数据应相同
// （收盘后），产生 3 条快照是污染。
func captureKlineSnapshot(
	store *db.DB,
	symbol, market, period string,
	count int,
	ch Channel,
	bars []Bar,
) (snapshotID int64, err error) {
	if store == nil {
		return 0, fmt.Errorf("落快照失败: DB 未初始化")
	}
	closed := closedBars(bars)
	if len(closed) == 0 {
		return 0, nil // 全部未收盘，无快照可落
	}

	params := map[string]string{
		"symbol":  symbol,
		"market":  market,
		"period":  period,
		"count":   fmt.Sprintf("%d", count),
		"channel": string(ch),
	}
	snap, _, err := store.Snapshots().InsertSnapshot(db.SnapshotInput{
		Source:      "akshare", // 见下方说明
		Endpoint:    "/kline",
		ParamsHash:  snapshotParamsHash("kline", params),
		CollectedAt: closed[len(closed)-1].TradingDate,
		Checksum:    barsChecksum(closed),
	})
	if err != nil {
		return 0, fmt.Errorf("落 K 线快照失败: %w", err)
	}
	return snap.ID, nil
}

// captureQuoteSnapshot 为行情响应落一条快照，返回 snapshot id。
//
// collected_at 的取值分两种情况：
//   - crypto（okx）：用 TradingDate（由 ticker 的 ts 毫秒时间戳转出），秒精度
//   - 其余（akshare 日线）：用 TradingDate，天精度
//
// 为什么 crypto 是秒精度：OKX ticker 是真·实时，每次 ts 都不同，
// 每次都是不同的市场状态，不该去重。
//
// is_partial=true 时**不落快照**（返回 0, nil）：盘中取日线时当日 bar 未收盘，
// 落库会撞 dedup key。
func captureQuoteSnapshot(store *db.DB, q *Quote) (snapshotID int64, err error) {
	if store == nil {
		return 0, fmt.Errorf("落快照失败: DB 未初始化")
	}
	if q == nil {
		return 0, nil
	}
	if q.IsPartial {
		// 见 closedBars 注释：未收盘的数据不落快照，避免 dedup key 碰撞。
		return 0, nil
	}
	if strings.TrimSpace(q.TradingDate) == "" {
		// 没有业务时间就无法构造稳定的 dedup key，宁可不落也不要用采集时间冒充。
		return 0, nil
	}

	source := "akshare"
	if q.Market == "crypto" {
		source = "okx"
	}
	params := map[string]string{
		"symbol":  q.Symbol,
		"market":  q.Market,
		"channel": string(q.Channel),
	}
	snap, _, err := store.Snapshots().InsertSnapshot(db.SnapshotInput{
		Source:      source,
		Endpoint:    "/quote",
		ParamsHash:  snapshotParamsHash("quote", params),
		CollectedAt: q.TradingDate,
		Checksum:    quoteChecksum(q),
	})
	if err != nil {
		return 0, fmt.Errorf("落行情快照失败: %w", err)
	}
	return snap.ID, nil
}

// 确保 time 被引用（collected_at 的精度语义在注释里讨论，实现用字符串）。
var _ = time.Second

// captureMacroSnapshot 为宏观指标落一条快照。
//
// collected_at 取**最后一个数据点的 period**（如 "2026-09"）——
// 宏观数据是月/季频，同一个月内多次查询应命中去重（数据不会月中变化）。
//
// 为什么不用采集时间：宏观数据的业务时间就是它的 period。用采集时间会让
// 「同一个月查 3 次」产生 3 条快照，而它们指的是同一份数据。
func captureMacroSnapshot(store *db.DB, indicator string, m *Macro) (int64, error) {
	if store == nil {
		return 0, fmt.Errorf("落快照失败: DB 未初始化")
	}
	if m == nil || len(m.Points) == 0 {
		// 无数据点时不落快照（没有可比对的业务时间）。
		return 0, nil
	}
	collectedAt := m.Points[len(m.Points)-1].Period
	if strings.TrimSpace(collectedAt) == "" {
		return 0, nil
	}
	params := map[string]string{"indicator": indicator}
	snap, _, err := store.Snapshots().InsertSnapshot(db.SnapshotInput{
		Source:      "akshare",
		Endpoint:    "/macro",
		ParamsHash:  snapshotParamsHash("macro", params),
		CollectedAt: collectedAt,
		Checksum:    macroChecksum(m),
	})
	if err != nil {
		return 0, fmt.Errorf("落宏观快照失败: %w", err)
	}
	return snap.ID, nil
}

// macroChecksum 按 Macro 的序列内容算 checksum（period+value 逐点）。
func macroChecksum(m *Macro) string {
	parts := make([]string, 0, len(m.Points)*2+2)
	parts = append(parts, m.SeriesName, m.Unit)
	for _, p := range m.Points {
		parts = append(parts, p.Period, p.Value)
	}
	return snapshotChecksum(parts...)
}

// captureIndicatorSnapshot 落一条服务端指标快照。
//
// # 参数哈希必须含 5 个维度
//
// indicator + symbol + bar + params + backtest_time —— 缺任何一个都会让
// 不同查询 dedup 成同一条快照。
//
// 最容易漏的是 backtest_time：同一个 rsi/1Dutc/14 在「今天」和「回测到上个月」
// 是两个完全不同的结果，哈希不含它就会把回测值覆盖掉实时值。
func captureIndicatorSnapshot(store *db.DB, r *IndicatorResult) (int64, error) {
	if store == nil {
		return 0, fmt.Errorf("落快照失败: DB 未初始化")
	}
	if r == nil || len(r.Values) == 0 {
		return 0, nil // 空值不落 —— 没有可比对的业务内容
	}
	collectedAt := r.TS
	if strings.TrimSpace(collectedAt) == "" {
		return 0, nil
	}
	params := map[string]string{
		"indicator":     r.Indicator,
		"symbol":        r.Symbol,
		"bar":           r.Bar,
		"params":        "", // 传进来的 Params 未回传到 Result（服务端默认值未知），
		"backtest_time": strconv.FormatInt(r.BacktestTime, 10),
	}
	snap, _, err := store.Snapshots().InsertSnapshot(db.SnapshotInput{
		Source:      "okx",
		Endpoint:    "/indicator",
		ParamsHash:  snapshotParamsHash("indicator", params),
		CollectedAt: collectedAt,
		Checksum:    indicatorChecksum(r),
	})
	if err != nil {
		return 0, fmt.Errorf("落指标快照失败: %w", err)
	}
	return snap.ID, nil
}

// indicatorChecksum 对指标结果算稳定校验和。
//
// 键排序后拼接 —— map 遍历顺序随机，不排序会让同一份数据每次算出不同 checksum，
// 从而被当成「内容冲突」反复告警（L3 那条 warning）。
func indicatorChecksum(r *IndicatorResult) string {
	keys := make([]string, 0, len(r.Values))
	for k := range r.Values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys)*2+4)
	parts = append(parts, r.Indicator, r.Symbol, r.Bar, r.TS)
	for _, k := range keys {
		parts = append(parts, k, r.Values[k])
	}
	return snapshotChecksum(parts...)
}
