package db

import (
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// =====================================================================
// 纸上交易/实盘订单(paper_orders)数据层
// =====================================================================

// 订单模式。
const (
	OrderModePaper = "paper"
	OrderModeLive  = "live"
)

// 订单状态。与 schema.sql 的 CHECK 约束一一对应。
const (
	OrderStatusDraft      = "draft"
	OrderStatusRiskPassed = "risk_passed"
	OrderStatusSubmitted  = "submitted"
	OrderStatusFilled     = "filled"
	OrderStatusPartial    = "partial"
	OrderStatusRejected   = "rejected"
	OrderStatusCancelled  = "cancelled"
	OrderStatusSettled    = "settled"
)

var (
	// ErrOrderInvalid 表示入参非法(模式、方向、数量、状态转换)。
	ErrOrderInvalid = errors.New("invalid paper order")
	// ErrOrderNotFound 表示订单不存在。
	ErrOrderNotFound = errors.New("paper order not found")
)

// validOrderModes 是模式白名单。
var validOrderModes = map[string]bool{
	OrderModePaper: true, OrderModeLive: true,
}

// orderTransitions 是订单状态机的合法转换表。
//
//	draft ──────► risk_passed ──► submitted ──┬─► filled ──► settled
//	  │               │                 │       └─► partial ──► settled
//	  ├─► cancelled   ├─► cancelled     ├─► cancelled
//	  └─► rejected    └─► rejected      └─► rejected
//
// 终点状态(filled/settled/rejected/cancelled)不再有出边。
// 显式列举而非「状态序号比较」:允许 partial→filled 这类补单场景,
// 又不放行 submitted→draft 这类回退。
var orderTransitions = map[string][]string{
	OrderStatusDraft: {
		OrderStatusRiskPassed, OrderStatusCancelled, OrderStatusRejected,
	},
	OrderStatusRiskPassed: {
		OrderStatusSubmitted, OrderStatusCancelled, OrderStatusRejected,
	},
	OrderStatusSubmitted: {
		OrderStatusFilled, OrderStatusPartial, OrderStatusCancelled, OrderStatusRejected,
	},
	OrderStatusPartial: {
		OrderStatusFilled, OrderStatusSettled, OrderStatusCancelled,
	},
	OrderStatusFilled: {
		OrderStatusSettled,
	},
	OrderStatusRejected:  {},
	OrderStatusCancelled: {},
	OrderStatusSettled:   {},
}

// canTransit 判断 from→to 是否为合法状态转换。
func canTransit(from, to string) bool {
	for _, next := range orderTransitions[from] {
		if next == to {
			return true
		}
	}
	return false
}

// PaperOrder is a row in the paper_orders table.
// Qty / Price / FilledQty / AvgFillPrice 是 NUMERIC,一律字符串承载。
type PaperOrder struct {
	ID           int64  `json:"id"`
	TaskID       *int64 `json:"task_id,omitempty"`
	SignalID     *int64 `json:"signal_id,omitempty"`
	InstrumentID *int64 `json:"instrument_id,omitempty"`
	// AccountID 是这笔纸面单归属的执行账户（WE-9）。
	// 老数据为 nil（加列前的历史）。
	AccountID     *int64  `json:"account_id,omitempty"`
	Mode          string  `json:"mode"`
	Side          string  `json:"side"`
	Qty           string  `json:"qty"`
	Price         *string `json:"price,omitempty"`
	FilledQty     string  `json:"filled_qty"`
	AvgFillPrice  *string `json:"avg_fill_price,omitempty"`
	Status        string  `json:"status"`
	BrokerOrderID *string `json:"broker_order_id,omitempty"`
	SubmittedAt   *string `json:"submitted_at,omitempty"`
	FilledAt      *string `json:"filled_at,omitempty"`
	CreatedAt     string  `json:"created_at"`
}

// PaperOrderInput 是 InsertOrder 的入参。
// Status 留空走 schema 默认值 draft。
type PaperOrderInput struct {
	TaskID   *int64
	SignalID *int64
	// AccountID 让 paper 单能按账户统计（WE-9）。
	//
	// 新写入要求非空（见 InsertFilledOrder 的校验）：
	// 没有账户的 paper 单无法参与任何账户级指标。
	// 老数据是 NULL（加列前的历史），查询时不传 accountID 会统计全部含 NULL。
	AccountID    *int64
	InstrumentID *int64
	Mode         string
	Side         string
	Qty          string
	Price        string
	Status       string
}

// EquityPoint 是 GetEquityCurve 的一行:某日的累计成交金额与订单数。
// 用累计成交额而非浮动盈亏,是因为曲线只依赖已成交的确定性数据,
// 不需要行情侧参与;真正的净值曲线待行情接入后另起。
type EquityPoint struct {
	Day          string `json:"day"`
	OrderCount   int64  `json:"order_count"`
	FilledAmount string `json:"filled_amount"`
}

// PaperOrderStore operates on the paper_orders table.
type PaperOrderStore struct{ db *DB }

// PaperOrders returns the paper order store associated with this DB.
func (d *DB) PaperOrders() *PaperOrderStore { return &PaperOrderStore{db: d} }

// paperOrderColumns 与 scanPaperOrder 的扫描顺序必须一致。
const paperOrderColumns = `id, task_id, signal_id, instrument_id, account_id, mode, side,
	qty, price, filled_qty, avg_fill_price, status,
	broker_order_id, submitted_at, filled_at, created_at`

// scanPaperOrder 从一行结果读出一条 PaperOrder。
func scanPaperOrder(row interface{ Scan(...any) error }) (*PaperOrder, error) {
	var (
		o           PaperOrder
		taskID      sql.NullInt64
		signalID    sql.NullInt64
		instID      sql.NullInt64
		accountID   sql.NullInt64
		price       sql.NullString
		avgFill     sql.NullString
		brokerID    sql.NullString
		submittedAt sql.NullString
		filledAt    sql.NullString
	)
	if err := row.Scan(
		&o.ID, &taskID, &signalID, &instID, &accountID, &o.Mode, &o.Side,
		&o.Qty, &price, &o.FilledQty, &avgFill, &o.Status,
		&brokerID, &submittedAt, &filledAt, &o.CreatedAt,
	); err != nil {
		return nil, err
	}
	o.TaskID = nullInt64Ptr(taskID)
	o.SignalID = nullInt64Ptr(signalID)
	o.InstrumentID = nullInt64Ptr(instID)
	o.AccountID = nullInt64Ptr(accountID)
	o.Price = nullStringPtr(price)
	o.AvgFillPrice = nullStringPtr(avgFill)
	o.BrokerOrderID = nullStringPtr(brokerID)
	o.SubmittedAt = nullStringPtr(submittedAt)
	o.FilledAt = nullStringPtr(filledAt)
	return &o, nil
}

// InsertOrder 写入一条订单并返回新 id。初始状态只允许 draft 或直接 risk_passed。
func (s *PaperOrderStore) InsertOrder(in PaperOrderInput) (int64, error) {
	mode := strings.TrimSpace(in.Mode)
	if !validOrderModes[mode] {
		return 0, fmt.Errorf("%w: mode %q 不在 paper/live 之内", ErrOrderInvalid, in.Mode)
	}
	side := strings.TrimSpace(in.Side)
	if !validOrderSides[side] {
		return 0, fmt.Errorf("%w: side %q 不在 buy/sell 之内", ErrOrderInvalid, in.Side)
	}
	if !validDecimalLiteral(in.Qty) {
		return 0, fmt.Errorf("%w: qty %q 不是合法十进制数", ErrOrderInvalid, in.Qty)
	}
	if p := strings.TrimSpace(in.Price); p != "" && !validDecimalLiteral(p) {
		return 0, fmt.Errorf("%w: price %q 不是合法十进制数", ErrOrderInvalid, p)
	}
	status := strings.TrimSpace(in.Status)
	if status == "" {
		status = OrderStatusDraft
	}
	if _, ok := orderTransitions[status]; !ok {
		return 0, fmt.Errorf("%w: 未知初始状态 %q", ErrOrderInvalid, in.Status)
	}
	if status != OrderStatusDraft && status != OrderStatusRiskPassed {
		return 0, fmt.Errorf("%w: 新单只能从 draft/risk_passed 起,收到 %q", ErrOrderInvalid, status)
	}

	var id int64
	err := s.db.QueryRow(`
INSERT INTO paper_orders(task_id, signal_id, instrument_id, account_id, mode, side, qty, price, status)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING id`,
		in.TaskID, in.SignalID, in.InstrumentID, in.AccountID, mode, side,
		normalizeDecimalInput(in.Qty, decimalScaleAmount),
		normalizeOptDecimal(in.Price, decimalScaleAmount), status,
	).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

// GetOrder 按 id 取一条订单,查不到返回 ErrOrderNotFound。
func (s *PaperOrderStore) GetOrder(id int64) (*PaperOrder, error) {
	row := s.db.QueryRow(`SELECT `+paperOrderColumns+` FROM paper_orders WHERE id = $1`, id)
	o, err := scanPaperOrder(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrOrderNotFound
		}
		return nil, err
	}
	return o, nil
}

// TransitStatus 把订单从当前状态推进到 to。
//
// 校验分两层:先在事务外读当前状态并按 orderTransitions 判断,非法转换立即
// 返回 fmt.Errorf("非法状态转换 %s→%s", ...) —— 这是调用方的编程错误,不是
// 数据竞争,必须显式报错而非静默 UPDATE 0 行;随后 UPDATE 带上 status=$cur
// 条件,若中途被别的连接抢先改过则影响 0 行,重读后重判。
//
// 进入 submitted / filled 时顺带写时间戳,避免调用方再单独发一条 UPDATE。
func (s *PaperOrderStore) TransitStatus(id int64, to string) error {
	to = strings.TrimSpace(to)
	if _, ok := orderTransitions[to]; !ok {
		return fmt.Errorf("%w: 未知目标状态 %q", ErrOrderInvalid, to)
	}
	// 最多重试 3 次:状态机是短事务,竞争窗口极小;超过 3 次说明有异常循环。
	for attempt := 0; attempt < 3; attempt++ {
		cur, err := s.GetOrder(id)
		if err != nil {
			return err
		}
		from := cur.Status
		if from == to {
			return nil // 幂等:已处于目标状态,不算错误
		}
		if !canTransit(from, to) {
			return fmt.Errorf("非法状态转换 %s→%s", from, to)
		}

		res, err := s.db.Exec(`
UPDATE paper_orders
SET status = $1,
    submitted_at = CASE WHEN $1 = 'submitted' THEN COALESCE(submitted_at, now()) ELSE submitted_at END,
    filled_at    = CASE WHEN $1 IN ('filled')  THEN COALESCE(filled_at, now())    ELSE filled_at    END
WHERE id = $2 AND status = $3`, to, id, from)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 1 {
			return nil
		}
		// 0 行 = 读后被并发改动,重新读一次再判。
	}
	return fmt.Errorf("%w: 订单 %d 状态并发变更,重试 3 次仍失败", ErrOrderInvalid, id)
}

// ListOrdersByTask 列出某任务下的订单,最新在前。
// 结果为空返回空切片而非 nil。
func (s *PaperOrderStore) ListOrdersByTask(taskID int64) ([]*PaperOrder, error) {
	if taskID <= 0 {
		return nil, fmt.Errorf("%w: task_id 必须为正", ErrOrderInvalid)
	}
	rows, err := s.db.Query(`
SELECT `+paperOrderColumns+` FROM paper_orders WHERE task_id = $1
ORDER BY created_at DESC, id DESC`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]*PaperOrder, 0, 16)
	for rows.Next() {
		o, err := scanPaperOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// GetEquityCurve 按成交日聚合已成交/部分成交订单的成交额与单数。
//
// 口径:只统计 filled 与 partial,金额 = filled_qty * COALESCE(avg_fill_price, price);
// 两者都为空的行不计入(没有成交价就算不出金额)。filled_qty 为 0 的 partial
// 也不计入。day 取 filled_at,缺失时退回 created_at 的日期。
//
// # 为什么金额聚合搬到 Go（SQLite 迁移）
//
// 金额列是 TEXT，SQL 算术只能 CAST(... AS REAL)（float64）。成交额是累加量，
// float64 的 15-17 位有效数字不够。这里按日分组用 big.Rat 精确累加，
// 输出按 numeric(20,8) 的精度（8 位小数）。
//
// # 为什么筛行必须 CAST
//
// filled_qty 是 TEXT，SQLite 的跨类型排序把任何 TEXT 排在数字之上，
// `filled_qty > 0` 对 '0' 也成立（实测）。只有 CAST 成 REAL 才是数值比较；
// 它只决定「哪些行进入累进」，不参与金额计算，所以不引入精度问题。
func (s *PaperOrderStore) GetEquityCurve(mode string) ([]*EquityPoint, error) {
	mode = strings.TrimSpace(mode)
	if mode == "" {
		mode = OrderModePaper
	}
	if !validOrderModes[mode] {
		return nil, fmt.Errorf("%w: mode %q 不在 paper/live 之内", ErrOrderInvalid, mode)
	}
	// substr(ts,1,10) 取 YYYY-MM-DD：时间列是 ISO8601 UTC TEXT，
	// 等价于原来的 to_char(... AT TIME ZONE 'UTC','YYYY-MM-DD')。
	rows, err := s.db.Query(`
SELECT substr(COALESCE(filled_at, created_at),1,10) AS day,
       filled_qty,
       COALESCE(avg_fill_price, price)
FROM paper_orders
WHERE mode = $1
  AND status IN ('filled','partial')
  AND CAST(filled_qty AS REAL) > 0
  AND COALESCE(avg_fill_price, price) IS NOT NULL
ORDER BY day`, mode)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type bucket struct {
		count  int64
		amount *big.Rat
	}
	order := make([]string, 0, 32)
	buckets := make(map[string]*bucket, 32)
	for rows.Next() {
		var (
			day  string
			qtyS string
			pxS  string
		)
		if err := rows.Scan(&day, &qtyS, &pxS); err != nil {
			return nil, err
		}
		qty, qErr := ParseDecimal(qtyS)
		if qErr != nil {
			return nil, fmt.Errorf("paper_orders.filled_qty 非法: %w", qErr)
		}
		px, pErr := ParseDecimal(pxS)
		if pErr != nil {
			return nil, fmt.Errorf("paper_orders 成交价非法: %w", pErr)
		}
		b, ok := buckets[day]
		if !ok {
			b = &bucket{amount: new(big.Rat)}
			buckets[day] = b
			order = append(order, day)
		}
		b.count++
		b.amount = DecimalAdd(b.amount, DecimalMul(qty, px))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]*EquityPoint, 0, len(order))
	for _, day := range order {
		b := buckets[day]
		out = append(out, &EquityPoint{
			Day:          day,
			OrderCount:   b.count,
			FilledAmount: DecimalString(b.amount, decimalScaleNotional),
		})
	}
	return out, nil
}

// InsertFilledOrder 在一个事务里「落一张已成交的 paper 单」+「标记草案已执行」。
//
// # 为什么是一个原子方法而不是两次调用
//
// 两件事必须同事务：
//   - 只落单不标记 → 下次还能重复执行同一张草案（重复下单）
//   - 只标记不落单 → 草案永远执行不了（单据丢失）
//
// 拆成两个方法，调用方忘写第二个就是一个静默的重复下单漏洞。
//
// # 为什么要求 accountID 非空
//
// 没有账户的 paper 单无法参与任何账户级指标，而「统计时静默跳过它」
// 会让收益率看起来比实际好。所以在写入点就拒绝，而不是让脏数据流进去。
//
// 老数据仍是 NULL（加列前的历史），查询时按需处理。
//
// # 为什么 status 直接是 filled
//
// paper 不做部分成交模拟：一张单要么全成，要么不成。
// 所以跳过 submitted/partial 两个中间态，直接 filled。
// 这与「虚拟账本口径」一致，见 exec/paper.go 的说明。
func (s *PaperOrderStore) InsertFilledOrder(
	in PaperOrderInput, draftID int64, executedNote string,
) (int64, error) {
	if in.AccountID == nil || *in.AccountID <= 0 {
		return 0, fmt.Errorf("%w: account_id 不能为空（paper 单必须归属账户）", ErrOrderInvalid)
	}
	if draftID <= 0 {
		return 0, fmt.Errorf("%w: draft_id 必须为正，实际 %d", ErrOrderInvalid, draftID)
	}
	note := strings.TrimSpace(executedNote)
	if note == "" {
		return 0, fmt.Errorf("%w: executed_note 不能为空（用于防重复执行）", ErrOrderInvalid)
	}

	mode := strings.TrimSpace(in.Mode)
	if !validOrderModes[mode] {
		return 0, fmt.Errorf("%w: mode %q 不在 paper/live 之内", ErrOrderInvalid, in.Mode)
	}
	side := strings.TrimSpace(in.Side)
	if !validOrderSides[side] {
		return 0, fmt.Errorf("%w: side %q 不在 buy/sell 之内", ErrOrderInvalid, in.Side)
	}
	if !validDecimalLiteral(in.Qty) {
		return 0, fmt.Errorf("%w: qty %q 不是合法十进制数", ErrOrderInvalid, in.Qty)
	}
	price := strings.TrimSpace(in.Price)
	if price != "" && !validDecimalLiteral(price) {
		return 0, fmt.Errorf("%w: price %q 不是合法十进制数", ErrOrderInvalid, price)
	}
	if price == "" {
		// 成交价为空时「已成交」是自相矛盾的：无法计算 notional。
		return 0, fmt.Errorf("%w: 已成交的单必须有 price", ErrOrderInvalid)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	// 条件更新草案：只有 executed_note 为空的才标记。
	//
	// 用 UPDATE ... WHERE executed_note IS NULL 而不是「先查后写」：
	// 并发两次 ExecutePaper 时，只有一个能改到行（另一个 RowsAffected=0），
	// 于是只有一张单落库。这是防重复执行的关键 —— 不能靠调用方保证。
	res, err := tx.Exec(`UPDATE orders_draft SET executed_note = $2
WHERE id = $1 AND (executed_note IS NULL OR executed_note = '')`, draftID, note)
	if err != nil {
		return 0, fmt.Errorf("标记草案已执行失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		// 可能：草案不存在，或已被别的执行消费。
		var exists bool
		if e := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM orders_draft WHERE id=$1)`, draftID).Scan(&exists); e != nil {
			return 0, e
		}
		if !exists {
			return 0, fmt.Errorf("草案 %d 不存在", draftID)
		}
		return 0, fmt.Errorf("%w: 草案 %d 已被执行过", ErrOrderInvalid, draftID)
	}

	var id int64
	if err := tx.QueryRow(`
INSERT INTO paper_orders(task_id, signal_id, instrument_id, account_id, mode, side,
                         qty, price, filled_qty, avg_fill_price, status, submitted_at, filled_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $7, $8, $9, now(), now())
RETURNING id`,
		in.TaskID, in.SignalID, in.InstrumentID, in.AccountID, mode, side,
		normalizeDecimalInput(in.Qty, decimalScaleAmount),
		NormalizeDecimal(price, decimalScaleAmount), OrderStatusFilled,
	).Scan(&id); err != nil {
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}
