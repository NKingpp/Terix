package server

// 纸面盈亏计算（2026-11-04）。
//
// # 分工
//
//	db 层    —— 从成交单聚合出持仓（净数量 + 加权成本 + 已实现盈亏）
//	本文件   —— 配上现价，算浮动盈亏与总盈亏
//
// 为什么现价必须在 server 层取：行情客户端是 agent 包的接口，
// 而 db 包不能 import agent（会形成循环依赖）。
//
// # 两类盈亏必须分开呈现
//
//	已实现  —— 已平仓部分，只依赖成交数据，随时可算
//	浮动    —— 持仓 × (现价 - 成本)，需要现价
//
// 合并成「总盈亏」一个数字会让用户无法判断：那笔钱是落袋了，
// 还是只是账面浮盈（随时会吐回去）。金融上这两者的决策含义完全不同。
//
// # 现价取不到时怎么办
//
// **不估算、不给 0**。只返回已实现部分，并明确标注「浮动盈亏未计算」
// 及原因。给一个 0 会被读成「刚好打平」，那是比缺失更糟的错 ——
// 用户会基于假数字做决策。

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// paperPnlResult 是本文件的计算产物（映射到 DTO 前）。
type paperPnlResult struct {
	// PositionCount 是有净持仓的标的数。
	PositionCount int64
	// FilledOrderCount 是参与计算的成交单数。
	FilledOrderCount int64
	// RealizedTotal 是已实现盈亏（精确 decimal 字符串）。
	RealizedTotal string
	// UnrealizedTotal 是浮动盈亏。
	//
	// nil 表示**无法计算**（现价取不到或没有持仓），不是 0。
	// 调用方必须区分这两种情况 —— 展示成 "—" 而不是 "0.00"。
	UnrealizedTotal *string
	// PricedPositions / TotalPositions 说明浮动盈亏覆盖了多少持仓。
	//
	// 部分标的取不到现价时，UnrealizedTotal 只含能取到的那部分 ——
	// 这个比例让用户知道那个数字覆盖了多少。
	PricedPositions int64
	TotalPositions  int64
	// Note 说明当前状态（为什么没有浮动盈亏 / 覆盖了多少）。
	Note string
}

// computePaperPnl 计算纸面盈亏。
//
// mode 空则默认 paper。现价按 instrument 的 market 分派到对应行情客户端
// （crypto 走 OKX，其余走 akshare）。
func (s *Server) computePaperPnl(ctx context.Context, mode string) (paperPnlResult, error) {
	if mode == "" {
		mode = "paper"
	}
	var out paperPnlResult

	positions, err := db.ListPaperPositions(ctx, s.m.pg.DB, mode)
	if err != nil {
		return out, err
	}
	counts, err := db.CountPaperPositions(ctx, s.m.pg.DB, mode)
	if err != nil {
		return out, err
	}
	out.PositionCount = counts.PositionCount
	out.FilledOrderCount = counts.FilledOrderCount

	realized, err := db.SumRealizedPnlNumeric(ctx, s.m.pg.DB, mode)
	if err != nil {
		return out, err
	}
	out.RealizedTotal = realized

	// 没有任何成交 → 明确说明，不算盈亏。
	if counts.FilledOrderCount == 0 {
		out.Note = "尚未产生纸面成交"
		return out, nil
	}

	// 逐个取现价。只对有净持仓的标的取（净数量为 0 的浮动盈亏恒为 0）。
	//
	// # 为什么串行而不是并发
	//
	// 概览页的持仓数是个位数到几十。行情接口本身有 1-3 秒延迟，
	// 并发能省时间但会让下游数据源承受突发并发（akshare sidecar 是单进程）。
	// 用一个短的 per-call 超时 + 总数上限来控制最坏情况。
	const (
		perCallTimeout = 3 * time.Second
		maxPositions   = 20 // 超过这个数只算前 20 个，其余标注未覆盖
	)
	var unrealizedSum float64
	priced := int64(0)
	for i, p := range positions {
		if i >= maxPositions {
			break
		}
		if isZeroDecimal(p.NetQty) || p.CostBasis == nil {
			continue // 无持仓或成本未知 → 浮动盈亏对它无意义
		}
		price, ok := s.quotePriceCached(ctx, p.Symbol, p.Market, perCallTimeout)
		if !ok {
			continue // 该标的取不到现价 → 不参与浮动盈亏，也不填 0
		}
		qty, okQ := parseDecimal(p.NetQty)
		cost, okC := parseDecimal(*p.CostBasis)
		if !okQ || !okC {
			continue
		}
		unrealizedSum += qty * (price - cost)
		priced++
	}
	out.TotalPositions = counts.PositionCount
	out.PricedPositions = priced

	if priced == 0 {
		out.Note = "已有成交，但当前无法取得任何持仓标的的现价，浮动盈亏未计算"
		return out, nil
	}
	// 格式化到 8 位小数，去掉末尾多余的 0（与其它金额字段风格一致）。
	uv := formatDecimal(unrealizedSum, 8)
	out.UnrealizedTotal = &uv
	if priced < counts.PositionCount {
		out.Note = fmt.Sprintf("部分持仓无法取得现价，浮动盈亏仅覆盖 %d/%d 个标的",
			priced, counts.PositionCount)
	} else {
		out.Note = ""
	}
	return out, nil
}

// quotePriceCached 取标的现价，带进程内短缓存。
//
// # 为什么需要缓存
//
// 概览页每次刷新都调一次 summary。同一标的的现价在几秒内不会变，
// 而行情接口有 1-3 秒延迟 —— 没有缓存时，用户频繁刷新会把
// sidecar 打满、页面也越来越慢。
//
// 缓存 30 秒：短到不会让用户看到过时价格，长到能吸收刷新洪峰。
func (s *Server) quotePriceCached(ctx context.Context, symbol, market string, timeout time.Duration) (float64, bool) {
	if symbol == "" {
		return 0, false
	}
	key := market + ":" + symbol

	if v, ok := s.pnlPrices.get(key); ok {
		return v, true
	}
	// 按市场选客户端：crypto 走 OKX，其余走 akshare。
	var cli agent.MarketClient
	if market == "crypto" {
		cli = s.cryptoCl
	} else {
		cli = s.marketCl
	}
	if cli == nil {
		return 0, false
	}

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	q, err := cli.FetchQuote(cctx, symbol, market, "")
	if err != nil || q == nil {
		return 0, false
	}
	price, ok := parseDecimal(q.Last)
	if !ok {
		return 0, false
	}
	s.pnlPrices.put(key, price)
	return price, true
}

// ---------------------------------------------------------------------
// 小工具：decimal 字符串 ↔ float64
// ---------------------------------------------------------------------

// parseDecimal 把 decimal 字符串转成 float64。
//
// ⚠️ 只用于**展示侧汇总**（浮动盈亏），不用于记账。
// 记账金额一律在 SQL 侧用 numeric 累加（见 db.SumRealizedPnlNumeric）。
func parseDecimal(v string) (float64, bool) {
	if v == "" {
		return 0, false
	}
	var f float64
	if _, err := fmt.Sscan(v, &f); err != nil {
		return 0, false
	}
	return f, true
}

// isZeroDecimal 判断 decimal 字符串是否为 0（含 0.0000000000 这类）。
func isZeroDecimal(v string) bool {
	f, ok := parseDecimal(v)
	return ok && f == 0
}

// formatDecimal 把 float64 格式化为固定小数位并去掉末尾 0。
func formatDecimal(v float64, prec int) string {
	s := strconv.FormatFloat(v, 'f', prec, 64)
	// 去掉末尾的 0 与孤立的小数点
	for len(s) > 0 && s[len(s)-1] == '0' {
		s = s[:len(s)-1]
	}
	if len(s) > 0 && s[len(s)-1] == '.' {
		s = s[:len(s)-1]
	}
	if s == "" || s == "-" {
		return "0"
	}
	return s
}

// ---------------------------------------------------------------------
// 现价短缓存
// ---------------------------------------------------------------------

// pnlPriceCache 是概览页专用的现价缓存（30 秒 TTL）。
//
// # 为什么不用通用缓存库
//
// 需求极简：一个 map + TTL，几十个键。引入依赖不值得。
// 而且这是**纯展示用的容错缓存** —— 缓存丢了只是慢一点，不会算错。
//
// # 并发安全
//
// 概览页可能被多个标签页同时刷。用 sync.Map 而不是加锁：读多写少，
// 且天然避免「持锁取行情」这种会让整个接口串行化的写法。
type pnlPriceCache struct {
	m sync.Map // map[string]pnlPriceEntry
}

type pnlPriceEntry struct {
	price  float64
	expiry time.Time
}

// pnlPriceTTL 是现价缓存存活时间。
//
// 30 秒的依据：概览页是「看一眼现状」的场景，不是盯盘。
// 更短会让频繁刷新打满行情接口，更长会让用户看到明显过时的价格。
const pnlPriceTTL = 30 * time.Second

func (c *pnlPriceCache) get(key string) (float64, bool) {
	v, ok := c.m.Load(key)
	if !ok {
		return 0, false
	}
	e, ok := v.(pnlPriceEntry)
	if !ok || time.Now().After(e.expiry) {
		return 0, false
	}
	return e.price, true
}

func (c *pnlPriceCache) put(key string, price float64) {
	c.m.Store(key, pnlPriceEntry{price: price, expiry: time.Now().Add(pnlPriceTTL)})
}
