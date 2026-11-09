package agent

// 交易上下文：最新价 + 交易时段（trader 用的 TradinSessionView）。
//
// # 三态设计（见 TraderVars.SessionError 的注释）
//
//	IsOpen=true               → 盘中，可以按「当前可执行」描述
//	IsOpen=false, 无 error    → 确定闭市，给 NextOpen
//	SessionError 非空         → **未知**（日历查询失败 / market 不支持）
//
// 第三态是最容易漏的：如果把「查不到」当成「闭市」，trader 会建议「等下一次
// 开盘」，而实际可能正在盘中 —— 一句话就错过一个交易日。

import (
	"context"
	"fmt"
	"time"

	"github.com/Autumn-27/artex/db"
)

// buildTradingContext 取最新价与交易时段。
//
// 返回 (lastPrice, session, sessionError)。
// sessionError 非空表示「交易时段无法确定」—— 与「确定闭市」不同。
func buildTradingContext(ctx context.Context, inst InstrumentView, asOf string) (
	lastPrice string, session TradingSessionView, sessionErr string) {

	// ---- 交易日历（market_hours）----
	if inst.Market == "" {
		sessionErr = "交易时段未知（标的的 market 未确定）"
	} else {
		now := time.Now().UTC()
		if asOf != "" {
			// 用 as_of 而不是 now：回测/补跑时「当前时段」应该按分析基准日算，
			// 否则会拿今天的交易时段去解释上周的数据。
			if d, err := time.Parse("2006-01-02", asOf); err == nil {
				now = d
			}
		}
		buildSession(inst.Market, now, &session, &sessionErr)
	}

	// ---- 最新价 ----
	if cli, ok := MarketClientFrom(ctx); ok && inst.Symbol != "" {
		meta, found := marketToolMetas["get_quote"]
		if found {
			qctx, cancel := context.WithTimeout(ctx, meta.Timeout)
			defer cancel()
			if q, err := cli.FetchQuote(qctx, inst.Symbol, inst.Market, ""); err == nil &&
				q != nil && q.Last != "" {
				lastPrice = q.Last
			}
		}
	}
	return lastPrice, session, sessionErr
}

// buildSession 填 TradingSessionView，失败时填 sessionErr（不填默认值）。
//
// **不填默认时区（UTC）** —— 那会让 trader 以为是 UTC 时区而非「不知道」。
func buildSession(market string, at time.Time, out *TradingSessionView, errOut *string) {
	loc, locErr := LocationOf(market)
	if locErr != nil {
		*errOut = fmt.Sprintf("交易时段未知（无法确定 %q 的时区: %v）", market, locErr)
		return
	}
	*out = TradingSessionView{Timezone: loc.String()}

	sess, isOpen, sErr := SessionAt(market, at)
	if sErr != nil {
		*errOut = fmt.Sprintf("交易时段未知（会话查询失败: %v）", sErr)
		out.IsOpen = false
		return
	}

	out.IsOpen = isOpen
	if isOpen {
		out.Open = sess.Open.Format(time.RFC3339)
		out.Close = sess.Close.Format(time.RFC3339)
		return
	}

	// 闭市：算下一个开盘时间。
	// 用 AddTradingDays(+1) 而不是「今天晚些时候」—— 已闭市意味着今天不再开。
	// 注意 A 股调休补班表在 market_hours 里有已知缺口，调休周可能偏一天。
	next, nErr := AddTradingDays(market, at, 1)
	if nErr != nil {
		*errOut = fmt.Sprintf("交易时段未知（下一个交易日计算失败: %v）", nErr)
		return
	}
	nextSess, _, nsErr := SessionAt(market, next)
	if nsErr != nil {
		*errOut = fmt.Sprintf("交易时段未知（下一日会话查询失败: %v）", nsErr)
		return
	}
	out.NextOpen = nextSess.Open.Format(time.RFC3339)
}

// ensure db import is used（db 在本文件只用于类型，见下方）
var _ = (*db.ExplorationStore)(nil)
