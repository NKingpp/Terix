package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// DraftExecParams 是一张草案的执行参数（侧表 draft_exec_params）。
//
// 与 orders_draft 分表的理由见 schema_sqlite.sql 的注释：SQLite 加列不可幂等。
// 历史草案没有记录时 Get 返回零值 → market 单、无 TP/SL、不 reduceOnly，
// 与加列前行为一致。
type DraftExecParams struct {
	DraftID    int64
	OrderType  string // market | limit
	PosSide    string // ""（按 side 推导）| long | short | net
	ReduceOnly bool
	TpPrice    string
	SlPrice    string
}

// Normalize 填默认值。
func (p *DraftExecParams) Normalize() {
	if p.OrderType == "" {
		p.OrderType = "market"
	}
}

// GetDraftExecParams 读草案执行参数（无记录 → 默认值，err=nil）。
func (d *DB) GetDraftExecParams(draftID int64) (DraftExecParams, error) {
	out := DraftExecParams{DraftID: draftID, OrderType: "market"}
	var (
		pos, tp, sl sql.NullString
		reduce      sql.NullInt64
	)
	err := d.QueryRow(`SELECT order_type, pos_side, reduce_only, tp_price, sl_price
FROM draft_exec_params WHERE draft_id=$1`, draftID).Scan(&out.OrderType, &pos, &reduce, &tp, &sl)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if pos.Valid {
		out.PosSide = pos.String
	}
	out.ReduceOnly = reduce.Valid && reduce.Int64 != 0
	if tp.Valid {
		out.TpPrice = tp.String
	}
	if sl.Valid {
		out.SlPrice = sl.String
	}
	if out.OrderType == "" {
		out.OrderType = "market"
	}
	return out, nil
}

// UpsertDraftExecParams 写草案执行参数（幂等）。空串价格落 NULL。
func (d *DB) UpsertDraftExecParams(p DraftExecParams) error {
	p.Normalize()
	if p.OrderType != "market" && p.OrderType != "limit" {
		return fmt.Errorf("order_type 只能是 market 或 limit，收到 %q", p.OrderType)
	}
	switch p.PosSide {
	case "", "long", "short", "net":
	default:
		return fmt.Errorf("pos_side 只能是 long/short/net/空，收到 %q", p.PosSide)
	}
	_, err := d.Exec(`INSERT INTO draft_exec_params(draft_id, order_type, pos_side, reduce_only, tp_price, sl_price)
VALUES ($1,$2,$3,$4,$5,$6)
ON CONFLICT (draft_id) DO UPDATE SET order_type=excluded.order_type, pos_side=excluded.pos_side,
  reduce_only=excluded.reduce_only, tp_price=excluded.tp_price, sl_price=excluded.sl_price`,
		p.DraftID, p.OrderType, p.PosSide, boolToInt(p.ReduceOnly),
		nullIfBlank(p.TpPrice), nullIfBlank(p.SlPrice))
	return err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullIfBlank(s string) any {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return s
}

// LivePosition 是一条被我方跟踪的实盘持仓（侧表 live_positions）。
//
// 它不是我方发明的账：真实持仓以交易所为准，这张表只负责回答两个问题：
// 「这个仓是哪张草案开的」与「谁已经把它平了」（closed_at 抢占）。
type LivePosition struct {
	ID           int64
	AccountID    int64
	InstID       string
	PosSide      string
	Qty          string
	AvgPx        string
	DraftID      int64
	OpenedAt     string
	ClosedAt     string
	CloseClOrdID string
}

// OpenLivePosition 开一条持仓跟踪记录。
func (d *DB) OpenLivePosition(p LivePosition) (int64, error) {
	if strings.TrimSpace(p.PosSide) == "" {
		p.PosSide = "net"
	}
	res, err := d.Exec(`INSERT INTO live_positions(account_id, inst_id, pos_side, qty, avg_px, draft_id, last_seen_at)
VALUES ($1,$2,$3,$4,$5,$6,now())`,
		p.AccountID, p.InstID, p.PosSide, p.Qty, nullIfBlank(p.AvgPx), nullIfEmptyI64(p.DraftID))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListOpenLivePositions 列出未平仓的跟踪记录。
//
// accountID <= 0 表示**不过滤账户**（跨账户查同一标的）。
//
// # 为什么要这个语义
//
// 调用方有两类：交易循环手里一定有账户 id（只关心自己的仓）；
// 而 create_order_draft 在建草案时并不知道会由哪个账户执行（账户由
// 执行侧决定），它只需要知道「这个标的是不是有仓、多少」。
//
// 早先这里传 0 会静默返回空列表（WHERE account_id=0 匹配不到任何行），
// 于是平仓量校验形同虚设 —— 是最难发现的那类「校验写了但从不生效」。
func (d *DB) ListOpenLivePositions(accountID int64) ([]LivePosition, error) {
	q := `SELECT id, account_id, inst_id, pos_side, qty, COALESCE(avg_px,''), COALESCE(draft_id,0),
COALESCE(opened_at,''), COALESCE(closed_at,''), COALESCE(close_clordid,'')
FROM live_positions WHERE closed_at IS NULL`
	args := []any{}
	if accountID > 0 {
		q += ` AND account_id=$1`
		args = append(args, accountID)
	}
	q += ` ORDER BY id`
	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]LivePosition, 0, 8)
	for rows.Next() {
		var p LivePosition
		if err := rows.Scan(&p.ID, &p.AccountID, &p.InstID, &p.PosSide, &p.Qty, &p.AvgPx, &p.DraftID,
			&p.OpenedAt, &p.ClosedAt, &p.CloseClOrdID); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// MarkLivePositionClosed 抢占式平仓标记：只有第一个调用方能成功。
//
// 这是「LLM 平仓草案」与「watchdog 强平」之间的去重闸门 —— 两者都先尝试
// 打这个标记（条件 UPDATE ... WHERE closed_at IS NULL），只有一方 RowsAffected=1。
func (d *DB) MarkLivePositionClosed(id int64, clOrdID string) (bool, error) {
	res, err := d.Exec(`UPDATE live_positions SET closed_at=now(), close_clordid=$1
WHERE id=$2 AND closed_at IS NULL`, clOrdID, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// TouchLivePosition 更新 last_seen_at（交易所侧仍能看到该仓时调用）。
func (d *DB) TouchLivePosition(id int64) error {
	_, err := d.Exec(`UPDATE live_positions SET last_seen_at=now() WHERE id=$1 AND closed_at IS NULL`, id)
	return err
}

func nullIfEmptyI64(v int64) any {
	if v <= 0 {
		return nil
	}
	return v
}
