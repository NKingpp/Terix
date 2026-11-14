package db

import (
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// PaperTradePlan 是模拟盘一笔持仓的计划（含止盈止损）。
//
// # 为什么不用 paper_orders 推
//
// paper_orders 只记「发生了什么」，记不下「打算在哪里退出」。
// 没有退出计划，模拟盘的仓位永远不会平 —— 已实现盈亏永远是 0，
// 那模拟盘就失去了唯一的价值（验证策略能不能赚到钱）。
type PaperTradePlan struct {
	ID           int64
	AccountID    int64
	InstrumentID int64
	Symbol       string // 关联 instruments 带出，便于展示
	PosSide      string // long | short
	Qty          string
	TpPrice      string
	SlPrice      string
	DraftID      int64
	OpenedAt     string
}

// InsertPaperTradePlan 开仓时登记计划。TP/SL 都空则不登记（无退出条件，平不了）。
func (d *DB) InsertPaperTradePlan(p PaperTradePlan) (int64, error) {
	if strings.TrimSpace(p.TpPrice) == "" && strings.TrimSpace(p.SlPrice) == "" {
		return 0, nil
	}
	if p.PosSide != "short" {
		p.PosSide = "long"
	}
	res, err := d.Exec(`INSERT INTO paper_trade_plans
(account_id, instrument_id, pos_side, qty, tp_price, sl_price, draft_id)
VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		p.AccountID, p.InstrumentID, p.PosSide, p.Qty,
		nullIfBlank(p.TpPrice), nullIfBlank(p.SlPrice), nullIfEmptyI64(p.DraftID))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListOpenPaperTradePlans 列出未平仓的计划（带 symbol）。
func (d *DB) ListOpenPaperTradePlans(accountID int64) ([]PaperTradePlan, error) {
	q := `SELECT p.id, p.account_id, p.instrument_id, COALESCE(i.symbol,''), p.pos_side,
p.qty, COALESCE(p.tp_price,''), COALESCE(p.sl_price,''), COALESCE(p.draft_id,0),
COALESCE(p.opened_at,'')
FROM paper_trade_plans p
LEFT JOIN instruments i ON i.id = p.instrument_id
WHERE p.closed_at IS NULL`
	args := []any{}
	if accountID > 0 {
		q += ` AND p.account_id = $1`
		args = append(args, accountID)
	}
	q += ` ORDER BY p.id`
	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]PaperTradePlan, 0, 8)
	for rows.Next() {
		var p PaperTradePlan
		if err := rows.Scan(&p.ID, &p.AccountID, &p.InstrumentID, &p.Symbol, &p.PosSide,
			&p.Qty, &p.TpPrice, &p.SlPrice, &p.DraftID, &p.OpenedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// MarkPaperTradePlanClosed 抢占式平仓标记：只有第一个调用方能成功。
//
// 与 live_positions 的 closed_at 同一套语义 —— 两个 goroutine 同时
// 判定该平时，只有一个能写下平仓单。
func (d *DB) MarkPaperTradePlanClosed(id int64, reason string) (bool, error) {
	res, err := d.Exec(`UPDATE paper_trade_plans SET closed_at=now(), close_reason=$1
WHERE id=$2 AND closed_at IS NULL`, strings.TrimSpace(reason), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ReducePaperTradePlanQty 部分平仓：把计划数量改成 newQty。
//
// # 为什么不能直接用 MarkPaperTradePlanClosed（2026-11-14）
//
// MarkClosed 无条件关闭整条计划。部分平仓时调它，剩余那部分仓位
// 就从账本上消失了 —— get_positions（读 plans）看不见它，
// close_position 也找不到它，最终在盈亏统计里变成「凭空获利」。
//
// 返回最新数量（newQty ≤ 0 时返回 ""，调用方应改调 MarkPaperTradePlanClosed）。
// 条件更新要求 closed_at IS NULL：与关闭操作抢占同一行，不会双写。
func (d *DB) ReducePaperTradePlanQty(id int64, newQty string) (string, error) {
	q := strings.TrimSpace(newQty)
	r, ok := new(big.Rat).SetString(q)
	if !ok || r.Sign() <= 0 {
		return "", nil
	}
	normalized := decimalLiteral(r)
	res, err := d.Exec(`UPDATE paper_trade_plans SET qty=$1 WHERE id=$2 AND closed_at IS NULL`,
		normalized, id)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", fmt.Errorf("paper plan %d 不存在或已平仓", id)
	}
	return normalized, nil
}

// AddToPaperTradePlan 加仓合并：数量累加，TP/SL 用新值覆盖（空值不覆盖）。
//
// # 为什么加仓要合并而不是插新行（2026-11-14）
//
// 同标的同方向插两条计划会让 get_positions 显示两行残余、
// close_position 只平掉其中一条 —— 另一条成了永远平不掉的幽灵仓。
// 合并之后「一个方向一条计划」这条不变量在纸面侧也成立。
func (d *DB) AddToPaperTradePlan(id int64, addQty, tp, sl string) error {
	add, ok := new(big.Rat).SetString(strings.TrimSpace(addQty))
	if !ok || add.Sign() <= 0 {
		return fmt.Errorf("%w: 加仓量 %q 不合法", ErrOrderInvalid, addQty)
	}
	var cur string
	if err := d.QueryRow(`SELECT qty FROM paper_trade_plans WHERE id=$1 AND closed_at IS NULL`, id).
		Scan(&cur); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("paper plan %d 不存在或已平仓", id)
		}
		return err
	}
	base, ok := new(big.Rat).SetString(strings.TrimSpace(cur))
	if !ok {
		return fmt.Errorf("%w: 现有计划数量 %q 不合法", ErrOrderInvalid, cur)
	}
	total := decimalLiteral(new(big.Rat).Add(base, add))
	sets := []string{"qty=$1"}
	args := []any{total}
	if v := strings.TrimSpace(tp); v != "" {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf("tp_price=$%d", len(args)))
	}
	if v := strings.TrimSpace(sl); v != "" {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf("sl_price=$%d", len(args)))
	}
	args = append(args, id)
	_, err := d.Exec(`UPDATE paper_trade_plans SET `+strings.Join(sets, ", ")+
		fmt.Sprintf(` WHERE id=$%d AND closed_at IS NULL`, len(args)), args...)
	return err
}

// decimalLiteral 把 big.Rat 渲染成不带尾零的十进制字面量。
func decimalLiteral(r *big.Rat) string {
	if r == nil {
		return "0"
	}
	if r.IsInt() {
		return r.Num().String()
	}
	s := strings.TrimRight(r.FloatString(10), "0")
	s = strings.TrimRight(s, ".")
	if s == "" || s == "-" {
		return "0"
	}
	return s
}

// GetPaperTradePlan 按 id 取一条（平仓时要用 qty/pos_side）。
func (d *DB) GetPaperTradePlan(id int64) (PaperTradePlan, error) {
	var p PaperTradePlan
	err := d.QueryRow(`SELECT p.id, p.account_id, p.instrument_id, COALESCE(i.symbol,''),
p.pos_side, p.qty, COALESCE(p.tp_price,''), COALESCE(p.sl_price,''), COALESCE(p.draft_id,0),
COALESCE(p.opened_at,'')
FROM paper_trade_plans p LEFT JOIN instruments i ON i.id = p.instrument_id
WHERE p.id=$1`, id).Scan(&p.ID, &p.AccountID, &p.InstrumentID, &p.Symbol, &p.PosSide,
		&p.Qty, &p.TpPrice, &p.SlPrice, &p.DraftID, &p.OpenedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil
	}
	return p, err
}
