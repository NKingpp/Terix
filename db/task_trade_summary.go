package db

// 任务级成交汇总（实盘口径）。
//
// # 数据源
//
// trade_fills —— 每笔成交含实际成交价与数量，从交易所同步而来。
// 纸面模式删除后这里只统计实盘；旧的 paper_orders 表不再读取。
//
// # 口径
//
// 加权平均成本法：净数量与已实现盈亏逐笔累加，与 exec.ComputePnLByInst
// 一致（两处口径不同会让「目标验收」和「交易记录页」显示不同数字）。
//
// 不含手续费 —— 目标验收看的是策略本身赚了多少价格差，
// 手续费是执行成本，混进去会让「差 0.5 USDT 达标」这种判定失真。

import (
	"context"
	"database/sql"
	"math/big"
	"strings"
)

// TaskTradeSummary 是一个任务的成交汇总。
type TaskTradeSummary struct {
	RealizedTotal string           // 已实现盈亏，精确十进制
	OpenPositions int              // 净数量 != 0 的标的数
	NetQty        map[int64]string // 每标的净数量（十进制字面量，带符号）
	Fills         int              // 成交笔数
}

// TaskTradeSummaryFor 汇总某任务的实盘成交。
//
// instrumentID > 0 时只统计该标的（目标可以只针对一个标的验收）。
func TaskTradeSummaryFor(ctx context.Context, conn *DB, taskID int64, instrumentID int64) (TaskTradeSummary, error) {
	out := TaskTradeSummary{RealizedTotal: "0", NetQty: map[int64]string{}}
	if conn == nil || taskID <= 0 {
		return out, nil
	}

	// 成交按「逐任务净持仓 + 比例拆份额」归属本任务（db/fill_attribution.go）。
	//
	// 早先这里是 `trade_fills JOIN trade_records ON client_order_id = cl_ord_id`：
	// 交易所 OCO/TP/SL 触发的平仓腿 clOrdId 是 `O` 前缀、平台从不写台账行，
	// 于是整腿被丢弃 —— 实测任务 39 漏 26 笔 / 53 张，算出的净持仓 -53 与
	// 母账 pos=0 相反，人工验收因此永远 409。
	//
	// 这里用 SplitSz（本任务占的份额），不是 FillSz：一条 algo 平仓腿会同时平掉
	// 多个任务的持仓，用整笔数量会让每个任务都被平光。手续费不在本函数口径内
	// （RealizedTotal 只看价差），故不读 SplitFee。
	fills, err := conn.TaskFills(ctx, taskID, 0, "")
	if err != nil {
		return out, err
	}
	// instrument_id 只能从台账取（trade_fills 只有 inst_id 字符串）。
	instOf := map[string]int64{}
	if rows, iErr := conn.QueryContext(ctx, `SELECT DISTINCT inst_id, COALESCE(instrument_id,0)
FROM trade_records WHERE task_id = $1 AND instrument_id IS NOT NULL`, taskID); iErr == nil {
		for rows.Next() {
			var sym string
			var id int64
			if rows.Scan(&sym, &id) == nil && id > 0 {
				instOf[sym] = id
			}
		}
		rows.Close()
	}

	type led struct {
		net  *big.Rat
		cost *big.Rat // 净额 × 成本价（只对净额 != 0 有意义）
	}
	leds := map[int64]*led{}
	realized := new(big.Rat)

	for _, f := range fills {
		instID := instOf[f.InstID]
		if instID == 0 {
			// 归属不到 instrument 的成交（该标的没有任何台账行）不计入 ——
			// 计进去会让 instrumentID 过滤失准。
			continue
		}
		if instrumentID > 0 && instID != instrumentID {
			continue
		}
		qty, okQ := new(big.Rat).SetString(strings.TrimSpace(f.SplitSz))
		px, okP := new(big.Rat).SetString(strings.TrimSpace(f.FillPx))
		if !okQ || !okP || qty.Sign() <= 0 {
			continue
		}
		side := f.Side
		l, exists := leds[instID]
		if !exists {
			l = &led{net: new(big.Rat), cost: new(big.Rat)}
			leds[instID] = l
		}

		// 带符号数量：买 +，卖 -
		signed := new(big.Rat).Set(qty)
		if strings.EqualFold(strings.TrimSpace(side), "sell") {
			signed.Neg(signed)
		}

		// 加权平均成本：同向加仓摊平均价；反向平仓结算已实现盈亏。
		sameDir := l.net.Sign() == 0 || (l.net.Sign() > 0) == (signed.Sign() > 0)
		if sameDir {
			l.net.Add(l.net, signed)
			l.cost.Add(l.cost, new(big.Rat).Mul(qty, px))
		} else {
			closing := new(big.Rat).Set(signed)
			if new(big.Rat).Abs(closing).Cmp(new(big.Rat).Abs(l.net)) > 0 {
				closing.Set(l.net)
				closing.Neg(closing)
			}
			// 均价 = cost / |net|
			avg := new(big.Rat)
			if l.net.Sign() != 0 {
				avg.Quo(l.cost, new(big.Rat).Abs(l.net))
			}
			// 平仓方向与持仓相反：卖出平多 = +，买入平空 = +
			gain := new(big.Rat).Sub(px, avg)
			gain.Mul(gain, new(big.Rat).Abs(closing))
			if l.net.Sign() < 0 {
				gain.Neg(gain) // 平空：跌了才赚
			}
			realized.Add(realized, gain)

			// 剩余持仓成本按均价重算
			l.net.Add(l.net, signed)
			l.cost.Mul(new(big.Rat).Abs(l.net), avg)
		}
		out.Fills++
	}

	for instID, l := range leds {
		if l.net.Sign() == 0 {
			continue
		}
		out.OpenPositions++
		out.NetQty[instID] = ratLiteral(l.net)
	}
	out.RealizedTotal = ratLiteral(realized)
	if out.RealizedTotal == "" {
		out.RealizedTotal = "0"
	}
	return out, nil
}

// SumRealizedPnlNumeric 汇总全账户已实现盈亏（实盘）。
//
// 只做交叉参考用 —— 账本里其它任务的成交会污染它，不能作为单任务判定依据。
func SumRealizedPnlNumeric(ctx context.Context, conn *sql.DB) (string, error) {
	if conn == nil {
		return "0", nil
	}
	rows, err := conn.QueryContext(ctx, `
SELECT inst_id, side, fill_sz, fill_px FROM trade_fills
ORDER BY inst_id, filled_at, id`)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	type led struct{ net, cost *big.Rat }
	leds := map[string]*led{}
	total := new(big.Rat)
	for rows.Next() {
		var instID string
		var side, qtyS, pxS string
		if err := rows.Scan(&instID, &side, &qtyS, &pxS); err != nil {
			return "", err
		}
		qty, okQ := new(big.Rat).SetString(strings.TrimSpace(qtyS))
		px, okP := new(big.Rat).SetString(strings.TrimSpace(pxS))
		if !okQ || !okP || qty.Sign() <= 0 {
			continue
		}
		l, exists := leds[instID]
		if !exists {
			l = &led{net: new(big.Rat), cost: new(big.Rat)}
			leds[instID] = l
		}
		signed := new(big.Rat).Set(qty)
		if strings.EqualFold(strings.TrimSpace(side), "sell") {
			signed.Neg(signed)
		}
		sameDir := l.net.Sign() == 0 || (l.net.Sign() > 0) == (signed.Sign() > 0)
		if sameDir {
			l.net.Add(l.net, signed)
			l.cost.Add(l.cost, new(big.Rat).Mul(qty, px))
			continue
		}
		closing := new(big.Rat).Set(signed)
		if new(big.Rat).Abs(closing).Cmp(new(big.Rat).Abs(l.net)) > 0 {
			closing.Set(l.net)
			closing.Neg(closing)
		}
		avg := new(big.Rat)
		if l.net.Sign() != 0 {
			avg.Quo(l.cost, new(big.Rat).Abs(l.net))
		}
		gain := new(big.Rat).Sub(px, avg)
		gain.Mul(gain, new(big.Rat).Abs(closing))
		if l.net.Sign() < 0 {
			gain.Neg(gain)
		}
		total.Add(total, gain)
		l.net.Add(l.net, signed)
		l.cost.Mul(new(big.Rat).Abs(l.net), avg)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return ratLiteral(total), nil
}

// ratLiteral 把 big.Rat 渲染成不带尾零的十进制字面量（最多 10 位小数）。
func ratLiteral(r *big.Rat) string {
	if r == nil {
		return "0"
	}
	if r.IsInt() {
		return r.Num().String()
	}
	s := strings.TrimRight(r.FloatString(10), "0")
	s = strings.TrimRight(s, ".")
	if s == "" || s == "-" || s == "-0" {
		return "0"
	}
	return s
}
