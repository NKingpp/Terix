package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// TradeRecord 是一次工具下单的台账（表 trade_records）。
//
// # 为什么需要它（而不是复用 orders_draft/orders_execution）
//
// 那两张表是「无人值守循环」的产物：草案要有 signal 才有 task 归属，
// 执行单的 draft_id 是 NOT NULL 外键。而 LLM 直接调工具下单时既没有草案、
// 也不该被逼着先造一条信号 —— 硬塞只会造出一批没有信号的空壳草案，
// 事后分不清「策略出的单」和「人喊的单」。
//
// # TaskID 为什么必须直存
//
// 归属要回答「这个任务赚了多少」。旧路径是
// fills → cl_ord_id → execution → draft → signal → task，五跳；
// 任何一环断了盈亏就归不上账（2026-11-14 任务 21/22 的平仓腿就是这样丢的）。
// 工具调用当场就知道任务，直接落一列。
type TradeRecord struct {
	ID             int64  `json:"id"`
	TaskID         int64  `json:"task_id"` // 0 = 不在任务上下文（人对主 agent 直接喊单）
	AccountID      int64  `json:"account_id"`
	InstrumentID   int64  `json:"instrument_id"`
	InstID         string `json:"inst_id"`
	Action         string `json:"action"` // open | close | set_leverage
	Side           string `json:"side"`   // buy | sell
	PosSide        string `json:"pos_side"`
	Qty            string `json:"qty"`
	OrdType        string `json:"ord_type"` // market | limit
	Px             string `json:"px"`
	Leverage       string `json:"leverage"`
	TpPrice        string `json:"tp_price"`
	SlPrice        string `json:"sl_price"`
	ReduceOnly     bool   `json:"reduce_only"`
	ClientOrderID  string `json:"client_order_id"`
	ChannelOrderID string `json:"channel_order_id"`
	State          string `json:"state"` // submitted | filled | failed
	// PositionEffect 记录这一单对持仓做了什么：open | scale_in | reduce | reverse。
	//
	// action 回答「这是开仓还是平仓」，但回答不了「开在已有仓上是加仓」——
	// 而加仓与开仓的成本口径、后续平仓方式、TP/SL 挂法都不同。
	// close / set_leverage 恒为空。
	PositionEffect string `json:"position_effect,omitempty"`
	Error          string `json:"error,omitempty"`
	Mode           string `json:"mode"`
	Operator       string `json:"operator"`
	// CreatedAt 只在列表/展示时需要，写入时不填。
	CreatedAt string `json:"created_at,omitempty"`
}

// position_effect 取值（与 exec.Effect* 一致；db 包不 import exec，
// 所以这里重复定义常量值，注释指明同源）。
const (
	TradeEffectOpen    = "open"
	TradeEffectScaleIn = "scale_in"
	TradeEffectReduce  = "reduce"
	TradeEffectReverse = "reverse"
)

// TradeRecordState 是 trade_records.state 的取值。
const (
	// TradeRecordSubmitted 表示交易所已受理（成交明细要等 fills 同步）。
	TradeRecordSubmitted = "submitted"
	// TradeRecordFilled 表示已知全部成交。
	TradeRecordFilled = "filled"
	// TradeRecordFailed 表示没发出去 / 被拒。
	TradeRecordFailed = "failed"
)

// InsertTradeRecord 落一条台账，返回 id。
//
// client_order_id 为空时不参与唯一约束（SQLite 允许多个 NULL）——
// set_leverage 这类没有单号的动作就靠这一点共存。
func (d *DB) InsertTradeRecord(r TradeRecord) (int64, error) {
	if strings.TrimSpace(r.InstID) == "" {
		return 0, fmt.Errorf("%w: inst_id 不能为空", ErrOrderInvalid)
	}
	if r.Action != "open" && r.Action != "close" && r.Action != "set_leverage" {
		return 0, fmt.Errorf("%w: action %q 不合法", ErrOrderInvalid, r.Action)
	}
	switch r.State {
	case TradeRecordSubmitted, TradeRecordFilled, TradeRecordFailed:
	default:
		return 0, fmt.Errorf("%w: state %q 不合法", ErrOrderInvalid, r.State)
	}
	switch r.PositionEffect {
	case "", TradeEffectOpen, TradeEffectScaleIn, TradeEffectReduce, TradeEffectReverse:
	default:
		return 0, fmt.Errorf("%w: position_effect %q 不合法（只能是空、open、scale_in、reduce、reverse）",
			ErrOrderInvalid, r.PositionEffect)
	}
	// 只有 open 动作能带 effect：close 带 effect 会让「平仓」与「加仓」
	// 在报表里混成一类。
	if r.Action != "open" && r.PositionEffect != "" {
		return 0, fmt.Errorf("%w: action=%s 不能带 position_effect（只有 open 有语义）",
			ErrOrderInvalid, r.Action)
	}
	if strings.TrimSpace(r.Operator) == "" {
		r.Operator = "agent"
	}
	var id int64
	err := d.QueryRow(`INSERT INTO trade_records
(task_id, account_id, instrument_id, inst_id, action, side, pos_side, qty, ord_type, px,
 leverage, tp_price, sl_price, reduce_only, client_order_id, channel_order_id,
 state, position_effect, error, mode, operator)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)
RETURNING id`,
		nullIfEmptyI64(r.TaskID), r.AccountID, nullIfEmptyI64(r.InstrumentID),
		strings.TrimSpace(r.InstID), r.Action,
		nullIfBlank(r.Side), nullIfBlank(r.PosSide), nullIfBlank(r.Qty),
		nullIfBlank(r.OrdType), nullIfBlank(r.Px), nullIfBlank(r.Leverage),
		nullIfBlank(r.TpPrice), nullIfBlank(r.SlPrice), boolToInt(r.ReduceOnly),
		nullIfBlank(r.ClientOrderID), nullIfBlank(r.ChannelOrderID),
		r.State, nullIfBlank(r.PositionEffect), nullIfBlank(r.Error),
		strings.TrimSpace(r.Mode), strings.TrimSpace(r.Operator),
	).Scan(&id)
	return id, err
}

// UpdateTradeRecordResult 回填通道单号 / 状态（发单后才知道 ordId）。
func (d *DB) UpdateTradeRecordResult(id int64, channelOrderID, state, errText string) error {
	if id <= 0 {
		return nil
	}
	_, err := d.Exec(`UPDATE trade_records
SET channel_order_id = COALESCE(NULLIF($2,''), channel_order_id),
    state = $3,
    error = NULLIF($4,'')
WHERE id = $1`, id, strings.TrimSpace(channelOrderID), state, strings.TrimSpace(errText))
	return err
}

// BackfillTradeRecordStates 按成交明细回填 trade_records.state。
//
// # 为什么需要（2026-11-15 对账事故）
//
// 下单时我们只知道「交易所受理了」，于是台账写 submitted。而实际上
// 市价单几乎立刻成交 —— 台账却永远停在 submitted，事后核对
// 「这单到底成交没有、成交多少」只能去交易所查。
//
// 更糟的是它会被误读：实测有人看到 state=submitted 就断定
// 「0 成交」，进而怀疑整条执行链，而交易所那边 150 张早就全额成交。
//
// # 判据
//
// 按 cl_ord_id 汇总 fills 的成交数量：
//
//	sum >= qty  → filled
//	0 < sum < qty → 仍是 submitted（部分成交，语义上还在进行）
//	sum == 0    → 不动（可能真的没成交，也可能还没同步到）
//
// 只更新 state='submitted' 的行 —— filled/failed 是终态，不该被覆盖
// （否则一次漏同步会把已经确认成交的记录改回未定）。
func (d *DB) BackfillTradeRecordStates(accountID int64) (int, error) {
	if accountID <= 0 {
		return 0, nil
	}
	rows, err := d.Query(`
SELECT tr.id, tr.qty, COALESCE(SUM(CAST(tf.fill_sz AS REAL)), 0) AS filled
FROM trade_records tr
LEFT JOIN trade_fills tf ON tf.cl_ord_id = tr.client_order_id
WHERE tr.account_id = $1 AND tr.state = 'submitted'
  AND COALESCE(tr.client_order_id,'') <> ''
GROUP BY tr.id, tr.qty`, accountID)
	if err != nil {
		return 0, err
	}
	type row struct {
		id     int64
		qty    float64
		filled float64
	}
	var todo []row
	for rows.Next() {
		var r row
		var qtyStr string
		if err := rows.Scan(&r.id, &qtyStr, &r.filled); err != nil {
			rows.Close()
			return 0, err
		}
		r.qty, _ = strconv.ParseFloat(strings.TrimSpace(qtyStr), 64)
		if r.filled > 0 {
			todo = append(todo, r)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	updated := 0
	for _, r := range todo {
		// 允许 1e-9 的浮点误差：fill_sz 与 qty 都是十进制字符串，
		// 累加后可能有尾数差。
		if r.qty > 0 && r.filled+1e-9 < r.qty {
			continue // 部分成交，保持 submitted
		}
		res, uErr := d.Exec(`UPDATE trade_records SET state='filled'
WHERE id=$1 AND state='submitted'`, r.id)
		if uErr != nil {
			return updated, uErr
		}
		if n, _ := res.RowsAffected(); n > 0 {
			updated++
		}
	}
	return updated, nil
}

// TaskIDsByClientOrderIDs 批量把 clOrdId 映射到任务 id。
//
// 供 fills 归属使用：成交同步拿到的只有 clOrdId，而 PnL 要按任务算。
// 返回的 map 只含**有任务归属**的（task_id 为空的不放进来 —— 那种记录
// 属于「不在任务里的手工单」，不该算进任何任务的 KPI）。
func (d *DB) TaskIDsByClientOrderIDs(clOrdIDs []string) (map[string]int64, error) {
	out := map[string]int64{}
	clean := make([]string, 0, len(clOrdIDs))
	for _, id := range clOrdIDs {
		if s := strings.TrimSpace(id); s != "" {
			clean = append(clean, s)
		}
	}
	if len(clean) == 0 {
		return out, nil
	}
	// 分批：SQLite 的参数上限是 999，一批 500 足够且不会踩线。
	const batch = 500
	for start := 0; start < len(clean); start += batch {
		end := start + batch
		if end > len(clean) {
			end = len(clean)
		}
		chunk := clean[start:end]
		ph := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, v := range chunk {
			ph[i] = fmt.Sprintf("$%d", i+1)
			args[i] = v
		}
		rows, err := d.Query(`SELECT client_order_id, task_id FROM trade_records
WHERE client_order_id IN (`+strings.Join(ph, ",")+`) AND task_id IS NOT NULL`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var cl string
			var tid int64
			if err := rows.Scan(&cl, &tid); err != nil {
				rows.Close()
				return nil, err
			}
			out[cl] = tid
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// ListTradeRecords 按条件列出交易台账（新→旧）。
func (d *DB) ListTradeRecords(f TradeRecordFilter) ([]TradeRecord, error) {
	if f.Limit <= 0 || f.Limit > 1000 {
		f.Limit = 200
	}
	q := `SELECT id, COALESCE(task_id,0), account_id, COALESCE(instrument_id,0), inst_id,
action, COALESCE(side,''), COALESCE(pos_side,''), COALESCE(qty,''), COALESCE(ord_type,''),
COALESCE(px,''), COALESCE(leverage,''), COALESCE(tp_price,''), COALESCE(sl_price,''),
reduce_only, COALESCE(client_order_id,''), COALESCE(channel_order_id,''),
state, COALESCE(position_effect,''), COALESCE(error,''), mode, operator, COALESCE(created_at,'')
FROM trade_records`
	where := []string{}
	args := []any{}
	if f.TaskID > 0 {
		args = append(args, f.TaskID)
		where = append(where, fmt.Sprintf("task_id = $%d", len(args)))
	}
	if f.AccountID > 0 {
		args = append(args, f.AccountID)
		where = append(where, fmt.Sprintf("account_id = $%d", len(args)))
	}
	if s := strings.TrimSpace(f.InstID); s != "" {
		args = append(args, s)
		where = append(where, fmt.Sprintf("inst_id = $%d", len(args)))
	}
	if len(where) > 0 {
		q += "\nWHERE " + strings.Join(where, " AND ")
	}
	args = append(args, f.Limit)
	q += fmt.Sprintf("\nORDER BY id DESC LIMIT $%d", len(args))

	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]TradeRecord, 0, 16)
	for rows.Next() {
		var r TradeRecord
		var reduce int
		if err := rows.Scan(&r.ID, &r.TaskID, &r.AccountID, &r.InstrumentID, &r.InstID,
			&r.Action, &r.Side, &r.PosSide, &r.Qty, &r.OrdType, &r.Px, &r.Leverage,
			&r.TpPrice, &r.SlPrice, &reduce, &r.ClientOrderID, &r.ChannelOrderID,
			&r.State, &r.PositionEffect, &r.Error, &r.Mode, &r.Operator, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.ReduceOnly = reduce != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// TradeRecordFilter 是 ListTradeRecords 的过滤条件（零值 = 不过滤）。
type TradeRecordFilter struct {
	TaskID    int64
	AccountID int64
	InstID    string
	Limit     int
}

// GetTradeRecord 按 id 取一条（查不到时返回零值 + ErrTradeRecordNotFound）。
func (d *DB) GetTradeRecord(id int64) (TradeRecord, error) {
	var r TradeRecord
	var reduce int
	err := d.QueryRow(`SELECT id, COALESCE(task_id,0), account_id, COALESCE(instrument_id,0), inst_id,
action, COALESCE(side,''), COALESCE(pos_side,''), COALESCE(qty,''), COALESCE(ord_type,''),
COALESCE(px,''), COALESCE(leverage,''), COALESCE(tp_price,''), COALESCE(sl_price,''),
reduce_only, COALESCE(client_order_id,''), COALESCE(channel_order_id,''),
state, COALESCE(position_effect,''), COALESCE(error,''), mode, operator
FROM trade_records WHERE id=$1`, id).Scan(
		&r.ID, &r.TaskID, &r.AccountID, &r.InstrumentID, &r.InstID,
		&r.Action, &r.Side, &r.PosSide, &r.Qty, &r.OrdType, &r.Px, &r.Leverage,
		&r.TpPrice, &r.SlPrice, &reduce, &r.ClientOrderID, &r.ChannelOrderID,
		&r.State, &r.PositionEffect, &r.Error, &r.Mode, &r.Operator)
	if errors.Is(err, sql.ErrNoRows) {
		return r, fmt.Errorf("%w: %d", ErrTradeRecordNotFound, id)
	}
	if err != nil {
		return r, err
	}
	r.ReduceOnly = reduce != 0
	return r, nil
}

// ErrTradeRecordNotFound 表示台账里没有这条记录。
var ErrTradeRecordNotFound = errors.New("trade record not found")
