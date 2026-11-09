package db

// 纸面持仓与盈亏计算的数据层（2026-11-04）。
//
// # 为什么不新建 paper_positions 表
//
// 直觉做法是加一张持仓表，成交时更新持仓。但那是**有状态的复制**：
// 持仓是成交单的函数（Σ买入 - Σ卖出），多一份存储就有多一份不一致的机会 ——
// 而这次要修的问题恰恰是「统计口径与真实数据不符」。
//
// 已有数据的唯一事实来源是 paper_orders（filled_qty + avg_fill_price）。
// 所以持仓**按需聚合**：从成交单算出每个标的的净持仓与成本。
// 好处是不会对不上；代价是每次查询都要聚合 —— 对概览页的规模（几十条单）
// 完全可接受。
//
// # 为什么成本用「加权平均」而不是 FIFO/LIFO
//
// FIFO/LIFO 需要逐笔匹配买卖，要有 lot 概念，而且要处理「卖出超过持仓」
// 这类脏数据。加权平均只需两个累加量（总数量、总金额），对缺失平仓记录的
// 历史数据也稳健。券商 App 的「持仓成本价」默认也是加权平均。
//
// # 为什么 db 层只返回持仓、不算盈亏
//
// 盈亏需要**现价**，而现价来自行情客户端（agent 包）。让 db 包 import agent
// 会形成反向依赖（agent 已经 import db）。所以这里只给持仓的成本与数量，
// 由 server 层配上现价再算。
//
// # 为什么累进在 Go 层做（SQLite 迁移）
//
// 金额列在 SQLite 里是 TEXT，SQL 算术只能 CAST(... AS REAL)，即 float64：
// 10 次累加 0.1 得 0.99999999999999988898。成本基数与持仓的比较会真的判错方向，
// 而金融系统里算错钱是事故。所以 SQL 只筛行 + 排序，累进全走 db/decimal.go
// 的 big.Rat（见该文件与 decimal_test.go 的手算基准）。
// 原 PG 版的四层 CTE（filled→running→settled→agg）就是这里的
// positionAccumulator.apply 逐行等价物。

import (
	"context"
	"database/sql"
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

// PaperPosition 是一个标的的纸面净持仓（已聚合买卖）。
type PaperPosition struct {
	InstrumentID int64
	// Symbol / Name / Market 来自 instruments 表的关联，便于前端直接展示。
	Symbol string
	Name   string
	Market string
	// NetQty 是净持仓数量（买入累加、卖出累减）。
	//
	// ⚠️ 可能为负：卖空。前端展示时要按方向说明，不能一律叫「持仓」。
	NetQty string
	// CostBasis 是净持仓的加权平均成本价（净成本 按净数量摊）。
	//
	// 卖出时按卖出前的均价冲减成本，所以这个值在多次买卖后仍是「当前持仓的均价」。
	// 净持仓为 0 时无意义（返回 nil）。
	CostBasis *string
	// RealizedAmount 是通过已平仓部分实现的金额（卖出价 × 卖出量 - 对应成本）。
	//
	// 这是**唯一不需要现价就能算出的盈亏** —— 因为它只依赖已成交数据。
	// 有它才能在没有行情的情况下也给出「已实现盈亏」。
	RealizedAmount string
}

// PaperPositionCounts 是持仓的规模统计（不含金额）。
//
// 金额汇总走 SumRealizedPnlNumeric（精确十进制，不经过 float）。
type PaperPositionCounts struct {
	// PositionCount 是有净持仓（净数量 != 0）的标的数。
	PositionCount int64
	// FilledOrderCount 是参与计算的成交单数。
	FilledOrderCount int64
}

// CountPaperFilledOrders 返回有成交的纸面单数（供 pnl 段的「有没有成交」判断）。
//
// 与 CountDashboardPaperOrders 的区别：那个是概览页专用（同样口径），
// 这个供 pnl 计算时复用，避免两处各写一遍 WHERE 条件而漂移。
func CountPaperFilledOrders(ctx context.Context, conn *sql.DB) (int64, error) {
	var n int64
	err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM paper_orders WHERE filled_qty > 0`).Scan(&n)
	return n, err
}

// paperFill 是一笔参与持仓计算的成交（已按 instrument_id, ts, id 排序）。
type paperFill struct {
	InstrumentID int64
	Side         string
	Qty          *big.Rat
	Price        *big.Rat
}

// loadPaperFills 取出参与持仓聚合的成交单，按累进顺序排好。
//
// # 为什么 ORDER BY 必须是 ts, id
//
// 已实现盈亏依赖「卖出当时的均价」，那是买卖顺序的函数。
// ts 是 ISO8601 TEXT，字典序 == 时间序（见 db/sqlite_dsn.go）；id 是稳定的
// tie-breaker，同一时刻的多笔单按插入顺序累进。
//
// # 为什么比较都用 CAST
//
// filled_qty / 价格是 TEXT。SQLite 的跨类型排序把任何 TEXT 排在数字之上，
// 所以 `filled_qty > 0` 对 '0' 也成立（实测）。这里必须 CAST 成 REAL 才能正确筛行。
// 只用于**筛行**，筛出来的值再进 big.Rat 精确累进 —— 精度损失不影响筛选。
func loadPaperFills(ctx context.Context, conn *sql.DB, mode string) ([]paperFill, error) {
	rows, err := conn.QueryContext(ctx, `
SELECT instrument_id, side, filled_qty, COALESCE(avg_fill_price, price)
FROM paper_orders
WHERE mode = $1
  AND status IN ('filled','partial')
  AND CAST(filled_qty AS REAL) > 0
  AND COALESCE(avg_fill_price, price) IS NOT NULL
  -- instrument_id 为 NULL 的行无法关联标的，也取不到现价，算不出盈亏。
  AND instrument_id IS NOT NULL
ORDER BY instrument_id, COALESCE(filled_at, created_at), id`, mode)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]paperFill, 0, 64)
	for rows.Next() {
		var (
			f    paperFill
			qtyS string
			pxS  string
		)
		if err := rows.Scan(&f.InstrumentID, &f.Side, &qtyS, &pxS); err != nil {
			return nil, err
		}
		q, qErr := ParseDecimal(qtyS)
		if qErr != nil {
			return nil, fmt.Errorf("paper_orders.filled_qty 非法（instrument_id=%d）: %w", f.InstrumentID, qErr)
		}
		p, pErr := ParseDecimal(pxS)
		if pErr != nil {
			return nil, fmt.Errorf("paper_orders 成交价非法（instrument_id=%d）: %w", f.InstrumentID, pErr)
		}
		f.Qty, f.Price = q, p
		out = append(out, f)
	}
	return out, rows.Err()
}

// positionAccumulator 是单个标的的累进状态。
//
// # 算法（逐行累进，等价于原四层 CTE + 窗口函数）
//
//	买入：netQty += q；totalCost += q*px
//	卖出：若净持仓 > 0：
//	          avgCost  = totalCost / netQty      （卖出前的均价）
//	          realized += q * (px - avgCost)
//	          totalCost -= q * avgCost           （按均价冲减成本）
//	          netQty    -= q
//	      否则（卖空）：netQty -= q；totalCost -= q*px
//
// 全部用 big.Rat，中间量精确。
//
// # 与原 PG SQL 的一处差异（有意修正）
//
// 原 SQL 的 avgCost 是 `SUM(buy q*px) OVER (...)/prev_qty` —— 分子只含买入金额，
// 分母却是**净**数量。一笔买入 + 一笔卖出时两者等价（现有测试覆盖的正是这个场景）；
// 但同一标的出现第二笔卖出时，分子没扣掉第一笔卖出已冲减的成本，avgCost 会被算大。
//
// 例：买 100@10 → 卖 50@12 → 卖 25@13
//
//	原 SQL: 第二笔卖出 avgCost = 1000/50 = 20（应为 10）
//	        → realized 少计 250，remain_cost 少扣 250，成本价算成 0
//
// 本实现按本文件顶部注释里**写明的**算法（净成本/净数量）。
type positionAccumulator struct {
	netQty    *big.Rat
	totalCost *big.Rat
	realized  *big.Rat
}

func newPositionAccumulator() *positionAccumulator {
	return &positionAccumulator{
		netQty:    new(big.Rat),
		totalCost: new(big.Rat),
		realized:  new(big.Rat),
	}
}

// apply 累进一笔成交。
func (a *positionAccumulator) apply(f paperFill) {
	amount := DecimalMul(f.Qty, f.Price)
	switch f.Side {
	case "buy":
		a.netQty = DecimalAdd(a.netQty, f.Qty)
		a.totalCost = DecimalAdd(a.totalCost, amount)
	case "sell":
		if DecimalSign(a.netQty) > 0 {
			if avgCost, ok := DecimalQuo(a.totalCost, a.netQty); ok {
				freed := DecimalMul(f.Qty, avgCost)
				a.realized = DecimalAdd(a.realized, DecimalSub(amount, freed))
				a.totalCost = DecimalSub(a.totalCost, freed)
			}
		} else {
			// 卖空：没有可冲减的成本，卖出所得记成负成本。
			a.totalCost = DecimalSub(a.totalCost, amount)
		}
		a.netQty = DecimalSub(a.netQty, f.Qty)
	}
}

// aggregatePaperPositions 把成交按标的累进成持仓。
//
// 返回值按 instrument_id 出现顺序（SQL 已按 instrument_id 排序，所以是升序）。
func aggregatePaperPositions(fills []paperFill) ([]*PaperPosition, map[int64]*positionAccumulator) {
	order := make([]int64, 0, 16)
	accs := make(map[int64]*positionAccumulator, 16)
	for _, f := range fills {
		a, ok := accs[f.InstrumentID]
		if !ok {
			a = newPositionAccumulator()
			accs[f.InstrumentID] = a
			order = append(order, f.InstrumentID)
		}
		a.apply(f)
	}
	positions := make([]*PaperPosition, 0, len(order))
	for _, id := range order {
		a := accs[id]
		p := &PaperPosition{
			InstrumentID:   id,
			NetQty:         DecimalString(a.netQty, decimalScaleAmount),
			RealizedAmount: DecimalString(a.realized, decimalScaleAmount),
		}
		// 成本价 = 净成本 / 净数量，仅在净数量 > 0 时有意义。
		if DecimalSign(a.netQty) > 0 {
			if basis, ok := DecimalQuo(a.totalCost, a.netQty); ok {
				s := DecimalString(basis, decimalScaleAmount)
				p.CostBasis = &s
			}
		}
		positions = append(positions, p)
	}
	return positions, accs
}

// ListPaperPositions 聚合出当前所有纸面持仓（含已实现盈亏）。
//
// # 口径（逐条对应 positionAccumulator，改一处必须改这里）
//
//  1. 只算 filled / partial 且有成交量的单 —— 未成交的下单意图不构成持仓
//  2. 买入：qty 累加、金额累加
//  3. 卖出：qty 累减；金额按「卖出前均价 × 卖出量」冲减
//  4. 净数量为 0 的标的仍返回（它的 RealizedAmount 是已实现盈亏，不能丢）
//  5. 成本价 = 净成本 / 净数量，仅在净数量 > 0 时有意义
//
// # 为什么卖出要冲减成本而不是单独记
//
// 例：买 100 股 @10（成本 1000），卖 50 股 @12（收入 600）。
//   - 净数量 = 50
//   - 若净金额 = 1000 - 600 = 400，成本价 = 400/50 = 8  ← 错，真实成本还是 10
//
// 所以卖出必须**同时**结出已实现盈亏，并按原均价冲减成本。
func ListPaperPositions(ctx context.Context, conn *sql.DB, mode string) ([]*PaperPosition, error) {
	if mode == "" {
		mode = "paper"
	}
	fills, err := loadPaperFills(ctx, conn, mode)
	if err != nil {
		return nil, err
	}
	positions, _ := aggregatePaperPositions(fills)
	if len(positions) == 0 {
		return positions, nil
	}
	// 标的展示字段单独一次查询补上（几十个持仓，比在成交查询里 JOIN 再去重更省）。
	args := make([]any, 0, len(positions))
	placeholders := make([]string, 0, len(positions))
	for i, p := range positions {
		args = append(args, p.InstrumentID)
		placeholders = append(placeholders, "$"+strconv.Itoa(i+1))
	}
	rows, err := conn.QueryContext(ctx, `
SELECT id, COALESCE(symbol,''), COALESCE(name,''), COALESCE(market,'')
FROM instruments WHERE id IN (`+strings.Join(placeholders, ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type meta struct{ symbol, name, market string }
	metas := make(map[int64]meta, len(positions))
	for rows.Next() {
		var id int64
		var m meta
		if err := rows.Scan(&id, &m.symbol, &m.name, &m.market); err != nil {
			return nil, err
		}
		metas[id] = m
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, p := range positions {
		if m, ok := metas[p.InstrumentID]; ok {
			p.Symbol, p.Name, p.Market = m.symbol, m.name, m.market
		}
	}
	return positions, nil
}

// SumRealizedPnlNumeric 汇总已实现盈亏（精确十进制字符串）。
//
// 复用同一套累进逻辑，只取 realized 之和 —— 与 ListPaperPositions 必定一致
// （以前是两段各自复制的窗口 SQL，「改一处漏另一处」的风险由此消失）。
func SumRealizedPnlNumeric(ctx context.Context, conn *sql.DB, mode string) (string, error) {
	if mode == "" {
		mode = "paper"
	}
	fills, err := loadPaperFills(ctx, conn, mode)
	if err != nil {
		return "0", err
	}
	_, accs := aggregatePaperPositions(fills)
	total := new(big.Rat)
	for _, a := range accs {
		total = DecimalAdd(total, a.realized)
	}
	return DecimalString(total, decimalScaleAmount), nil
}

// CountPaperPositions 统计持仓规模（净持仓标的数 + 参与计算的成交单数）。
//
// # 为什么单独一个函数
//
// 概览页只需要「有几个持仓」这个数字，不必把每个持仓拉回来。
// 金额汇总走 SumRealizedPnlNumeric —— 两者分开是因为：
// 金额必须精确累进，而计数用 COUNT 更直接。
func CountPaperPositions(ctx context.Context, conn *sql.DB, mode string) (PaperPositionCounts, error) {
	var c PaperPositionCounts
	if mode == "" {
		mode = "paper"
	}
	// 净持仓数 = 每个标的的买卖差额不为 0 的个数。
	// filled_qty 是 TEXT，必须 CAST 才参与数值比较（计数不涉及金额精度）。
	err := conn.QueryRowContext(ctx, `
SELECT COUNT(*) FROM (
    SELECT instrument_id
    FROM paper_orders
    WHERE mode = $1 AND status IN ('filled','partial')
      AND CAST(filled_qty AS REAL) > 0
      AND instrument_id IS NOT NULL
    GROUP BY instrument_id
    HAVING SUM(CASE WHEN side='buy' THEN CAST(filled_qty AS REAL)
                    ELSE -CAST(filled_qty AS REAL) END) <> 0
) t`, mode).Scan(&c.PositionCount)
	if err != nil {
		return c, err
	}
	err = conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM paper_orders WHERE mode=$1 AND filled_qty > 0`, mode).Scan(&c.FilledOrderCount)
	return c, err
}
