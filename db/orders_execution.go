package db

// 实盘订单（orders_execution）数据层。
//
// # 与 paper_orders 的区别（为什么不能合并成一张表）
//
//   paper_orders    模拟撮合，字段是「我们算出来的」（price / avg_fill_price）
//   orders_execution 真实下单，字段是「通道告诉我们什么」（channel_order_id / raw_ack）
//
// 更关键：orders_execution 有 draft_id 外键（必须来自一张已过风控的草稿）、
// 有 mode 列（同一账户可跑不同模式）、有 client_order_id 幂等键。
// 这些在纸面场景都不存在 —— 合成一张表会让一半列长期为 NULL。
//
// # 没有 symbol / side 列
//
// 标的与方向在 orders_draft 上（通过 draft_id 关联）。这里不重复存 ——
// 重复存会有「两个地方不一致」的风险，而实盘的 symbol 写错是灾难性的。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// 订单状态常量。与 schema 的 CHECK 约束一一对应。
const (
	ExecStateDraft           = "draft"
	ExecStateRiskPending     = "risk_pending"
	ExecStateRiskPassed      = "risk_passed"
	ExecStateRiskDenied      = "risk_denied"
	ExecStateSubmitted       = "submitted"
	ExecStatePartial         = "partial"
	ExecStateFilled          = "filled"
	ExecStateRejected        = "rejected"
	ExecStateCancelRequested = "cancel_requested"
	ExecStateCancelled       = "cancelled"
	ExecStateSettled         = "settled"
)

var (
	// ErrExecOrderNotFound 表示订单不存在。
	ErrExecOrderNotFound = errors.New("exec order not found")
	// ErrExecTransitionInvalid 表示状态转换不合法。
	ErrExecTransitionInvalid = errors.New("invalid exec order state transition")
	// ErrExecDuplicateClientOrderID 表示幂等键冲突。
	ErrExecDuplicateClientOrderID = errors.New("duplicate client_order_id")
)

// execStateTransitions 是实盘订单状态机的合法转换表。
//
//	draft ──► risk_pending ──┬─► risk_passed ──► submitted ──┬─► partial ──► filled ──► settled
//	                          │                              ├─► filled
//	                          └─► risk_denied                └─► rejected
//
//	submitted ──► cancel_requested ──┬─► cancelled
//	                                  ├─► filled     （撤单请求到达前已成交）
//	                                  └─► partial    （部分成交后才收到撤单）
//	partial ──┬─► filled
//	          ├─► settled
//	          ├─► cancel_requested
//	          └─► cancelled      （撤掉剩余，已成交部分保留 → filled_qty > 0）
//
// # 三条刻意的设计选择
//
//  1. **risk_denied 是终点**：被风控拒绝的订单不能「再想想」重试 ——
//     要下就新建一单（新的风控评估）。允许重试等于允许绕过风控。
//  2. **cancel_requested 是独立状态而不是直接 cancelled**：撤单是异步的，
//     通道可能拒绝撤单（已成交）。直接标 cancelled 会在通道拒绝时留下「本地已撤、实际已成交」的错位。
//     注意 cancel_requested → filled 是**合法**的 —— 撤单请求不等于撤单成功。
//  3. **filled 只能到 settled**：已成交的订单不能再撤（钱已经花了）。
//
// # partial → cancelled 为什么必须存在
//
// 部分成交后用户撤单，通道撤掉剩余部分，本地终态是 cancelled 且 filled_qty > 0。
// 没有这条的话，「部分成交 + 撤单成功」这个最常见的场景无法落库 ——
// 只能卡在 cancel_requested，而那不是终态，调度器会永远扫到它。
var execStateTransitions = map[string][]string{
	ExecStateDraft:           {ExecStateRiskPending},
	ExecStateRiskPending:     {ExecStateRiskPassed, ExecStateRiskDenied},
	ExecStateRiskPassed:      {ExecStateSubmitted, ExecStateRiskDenied},
	ExecStateRiskDenied:      {},
	ExecStateSubmitted:       {ExecStatePartial, ExecStateFilled, ExecStateRejected, ExecStateCancelRequested},
	ExecStatePartial:         {ExecStateFilled, ExecStateSettled, ExecStateCancelRequested, ExecStateCancelled},
	ExecStateCancelRequested: {ExecStateCancelled, ExecStateFilled, ExecStatePartial},
	ExecStateFilled:          {ExecStateSettled},
	ExecStateRejected:        {},
	ExecStateCancelled:       {},
	ExecStateSettled:         {},
}

// CanExecTransit 判断 from→to 是否为合法状态转换。
//
// 导出是给 exec 包的状态机用的 —— 两层校验（Go 侧先判、SQL 侧再判）
// 不是冗余：Go 侧给出可读的错误，SQL 侧防并发竞态。
func CanExecTransit(from, to string) bool {
	for _, next := range execStateTransitions[from] {
		if next == to {
			return true
		}
	}
	return false
}

// ExecOrder 是 orders_execution 的一行。
//
// Qty / FilledQty / AvgPrice 是 NUMERIC(28,10)，一律用 string 承载 ——
// float64 会在 28 位精度上失真，而实盘金额不能失真。
type ExecOrder struct {
	ID             int64           `json:"id"`
	DraftID        int64           `json:"draft_id"`
	AccountID      int64           `json:"account_id"`
	Mode           string          `json:"mode"`
	State          string          `json:"state"`
	RiskVerdictID  *int64          `json:"risk_verdict_id,omitempty"`
	ClientOrderID  string          `json:"client_order_id"`
	ChannelOrderID *string         `json:"channel_order_id,omitempty"`
	Qty            string          `json:"qty"`
	FilledQty      string          `json:"filled_qty"`
	AvgPrice       *string         `json:"avg_price,omitempty"`
	RawAck         json.RawMessage `json:"raw_ack"`
	StateAt        string          `json:"state_at"`
	CreatedAt      string          `json:"created_at"`
}

// ExecOrderInput 是 InsertOrder 的入参。
type ExecOrderInput struct {
	DraftID       int64
	AccountID     int64
	Mode          string
	State         string
	RiskVerdictID *int64
	ClientOrderID string
	Qty           string
}

// validExecOrderModes 是订单模式白名单，与 CHECK 一致。
var validExecOrderModes = map[string]bool{
	string(ExecModePaper):         true,
	string(ExecModeLiveWhitelist): true,
	string(ExecModeLiveManual):    true,
	string(ExecModeLiveAuto):      true,
}

// ExecOrderStore 操作 orders_execution 表。
type ExecOrderStore struct{ db *DB }

// ExecOrders 返回该 DB 关联的实盘订单仓储。
func (d *DB) ExecOrders() *ExecOrderStore { return &ExecOrderStore{db: d} }

// execOrderColumns 与 scanExecOrder 的扫描顺序必须一致。
const execOrderColumns = `id, draft_id, account_id, mode, state, risk_verdict_id,
	client_order_id, channel_order_id, qty, filled_qty,
	avg_price, raw_ack, state_at, created_at`

// scanExecOrder 从一行结果读出一条 ExecOrder。
func scanExecOrder(row interface{ Scan(...any) error }) (*ExecOrder, error) {
	var (
		o             ExecOrder
		riskVerdictID sql.NullInt64
		channelOrder  sql.NullString
		avgPrice      sql.NullString
		rawAck        []byte
	)
	if err := row.Scan(&o.ID, &o.DraftID, &o.AccountID, &o.Mode, &o.State,
		&riskVerdictID, &o.ClientOrderID, &channelOrder, &o.Qty, &o.FilledQty,
		&avgPrice, &rawAck, &o.StateAt, &o.CreatedAt); err != nil {
		return nil, err
	}
	// 读路径统一到 numeric(28,10) 的位数形态。
	//
	// # 为什么读侧也要规范化（不只是写侧）
	//
	// 列的 DDL 默认值是 TEXT 的 '0'（原 PG 是 numeric 的 0，读出来是
	// "0.0000000000"）。所以「从没被写过」的行与「被写过」的行位数不同，
	// 前端会看到两种形态。规范化放在读侧，一处覆盖所有来源（默认值、
	// 历史行、直连 SQL 写的行）。
	o.Qty = NormalizeDecimal(o.Qty, decimalScaleAmount)
	o.FilledQty = NormalizeDecimal(o.FilledQty, decimalScaleAmount)
	if riskVerdictID.Valid {
		v := riskVerdictID.Int64
		o.RiskVerdictID = &v
	}
	if channelOrder.Valid {
		s := channelOrder.String
		o.ChannelOrderID = &s
	}
	if avgPrice.Valid {
		s := NormalizeDecimal(avgPrice.String, decimalScaleAmount)
		o.AvgPrice = &s
	}
	if len(rawAck) > 0 {
		o.RawAck = json.RawMessage(rawAck)
	} else {
		o.RawAck = json.RawMessage(`{}`)
	}
	return &o, nil
}

// InsertOrder 插入一条实盘订单。
//
// ClientOrderID 是幂等键（UNIQUE 约束）：同一笔意图重复提交时，
// 第二次会撞唯一索引。**这是有意的** —— 网络重试不该产生两张单。
// 调用方应捕获 ErrExecDuplicateClientOrderID 并读回已有订单，而不是失败重试。
func (s *ExecOrderStore) InsertOrder(in ExecOrderInput) (int64, error) {
	clientOrderID := strings.TrimSpace(in.ClientOrderID)
	if clientOrderID == "" {
		return 0, fmt.Errorf("%w: client_order_id 不能为空（幂等键）", ErrOrderInvalid)
	}
	mode := strings.TrimSpace(in.Mode)
	if !validExecOrderModes[mode] {
		return 0, fmt.Errorf("%w: 不支持的订单模式 %q", ErrOrderInvalid, mode)
	}
	state := strings.TrimSpace(in.State)
	if state == "" {
		state = ExecStateDraft
	}
	if _, ok := execStateTransitions[state]; !ok {
		return 0, fmt.Errorf("%w: 未知订单状态 %q", ErrOrderInvalid, state)
	}
	if strings.TrimSpace(in.Qty) == "" {
		return 0, fmt.Errorf("%w: qty 不能为空", ErrOrderInvalid)
	}
	if in.AccountID <= 0 || in.DraftID <= 0 {
		return 0, fmt.Errorf("%w: account_id 与 draft_id 必须为正", ErrOrderInvalid)
	}

	var id int64
	err := s.db.QueryRow(`
INSERT INTO orders_execution
    (draft_id, account_id, mode, state, risk_verdict_id, client_order_id, qty)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id`,
		in.DraftID, in.AccountID, mode, state, in.RiskVerdictID, clientOrderID,
		normalizeDecimalInput(in.Qty, decimalScaleAmount),
	).Scan(&id)
	if err != nil {
		// 唯一索引冲突翻译成可识别的哨兵错误 —— 调用方据此走「读回已有」而不是报错。
		if execClientOrderIDViolation(err) {
			return 0, fmt.Errorf("%w: %s", ErrExecDuplicateClientOrderID, clientOrderID)
		}
		return 0, fmt.Errorf("插入实盘订单失败: %w", err)
	}
	return id, nil
}

// GetOrder 按 id 取一条订单。
func (s *ExecOrderStore) GetOrder(id int64) (*ExecOrder, error) {
	row := s.db.QueryRow(`SELECT `+execOrderColumns+` FROM orders_execution WHERE id=$1`, id)
	o, err := scanExecOrder(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrExecOrderNotFound
		}
		return nil, err
	}
	return o, nil
}

// GetByClientOrderID 按幂等键取订单。
//
// 返回 ErrExecOrderNotFound 时才是「真的没这单」；网络重试场景下
// 调用方应先用它查、查不到再 InsertOrder。
func (s *ExecOrderStore) GetByClientOrderID(clientOrderID string) (*ExecOrder, error) {
	row := s.db.QueryRow(`SELECT `+execOrderColumns+`
FROM orders_execution WHERE client_order_id=$1`, strings.TrimSpace(clientOrderID))
	o, err := scanExecOrder(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrExecOrderNotFound
		}
		return nil, err
	}
	return o, nil
}

// Transition 用条件 UPDATE 推进订单状态（WE-4 的核心）。
//
// # 为什么必须 WHERE state=$from
//
// 两个调度器（scheduler tick + 用户手动撤单）可能同时推进同一单。
// 只有「当前状态确实是 from」才允许改 —— 改不成就说明别人先动了。
//
// 不这样做的话，两个并发请求会各自读到 submitted，各自写 filled 和 cancelled，
// 后写的覆盖先写的，且**两个调用方都以为成功**。
//
// # 返回值语义
//
//	(ok=true,  nil) → 推进成功
//	(ok=false, nil) → **状态已被别人推进，不是错误** —— 调用方跳过即可
//	(false, err)    → 系统错误（DB 不可用等），调用方应告警
//
// **「被别人先推进」不是错误**，这是这个设计的关键：
// 如果把它当错误，调用方会重试，而重试毫无意义（状态已经变了）。
func (s *ExecOrderStore) Transition(id int64, from, to string) (bool, error) {
	from = strings.TrimSpace(from)
	to = strings.TrimSpace(to)

	// Go 侧先判合法性：给出可读错误，而不是让一次无效配置静默返回 ok=false。
	// 注意这个判断与 SQL 的 WHERE 是**两件不同的事**：
	//   - 这里防「写错了转换表」
	//   - SQL 的 WHERE 防「并发竞态」
	if !CanExecTransit(from, to) {
		return false, fmt.Errorf("%w: %s → %s", ErrExecTransitionInvalid, from, to)
	}

	res, err := s.db.Exec(`UPDATE orders_execution
SET state = $1, state_at = now()
WHERE id = $2 AND state = $3`, to, id, from)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// TransitionWithAck 推进状态并同时写入通道返回的原始应答。
//
// 与 Transition 同一套并发控制，额外写 channel_order_id / filled_qty / avg_price。
// 分开成两个方法而不是给 Transition 加一堆可选参数：
// 大多数转换（draft→risk_pending 等）没有通道应答，硬塞会让调用方传一堆 nil。
func (s *ExecOrderStore) TransitionWithAck(id int64, from, to string, ack ExecOrderAck) (bool, error) {
	from = strings.TrimSpace(from)
	to = strings.TrimSpace(to)
	if !CanExecTransit(from, to) {
		return false, fmt.Errorf("%w: %s → %s", ErrExecTransitionInvalid, from, to)
	}

	raw := ack.RawAck
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}

	res, err := s.db.Exec(`UPDATE orders_execution SET
    state = $1,
    state_at = now(),
    channel_order_id = COALESCE(NULLIF($2, ''), channel_order_id),
    filled_qty = COALESCE($3, filled_qty),
    avg_price  = COALESCE($4, avg_price),
    raw_ack    = $5
WHERE id = $6 AND state = $7`,
		to, ack.ChannelOrderID,
		nullableNormalizedDecimal(ack.FilledQty, decimalScaleAmount),
		nullableNormalizedDecimal(ack.AvgPrice, decimalScaleAmount),
		[]byte(raw), id, from)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// ExecOrderAck 是通道返回的应答中我们关心的部分。
//
// 指针字段用 nil 表示「这次应答没带这个信息」——
// 这与「带了一个空值」不同，TransitionWithAck 用 COALESCE 保留旧值。
type ExecOrderAck struct {
	ChannelOrderID string
	FilledQty      *string
	AvgPrice       *string
	RawAck         json.RawMessage
}

// ListByState 按状态列订单（调度器扫「待推进」的单用）。
//
// LIMIT 是必须的：调度器每 tick 都要扫，不限量会在积累几千单时拖慢整个 tick。
func (s *ExecOrderStore) ListByState(state string, limit int) ([]*ExecOrder, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.db.Query(`SELECT `+execOrderColumns+`
FROM orders_execution WHERE state=$1 ORDER BY state_at LIMIT $2`, state, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*ExecOrder, 0, limit)
	for rows.Next() {
		o, err := scanExecOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// execClientOrderIDViolation 判断错误是否为 client_order_id 唯一键冲突。
//
// 只认 orders_execution_client_order_id_key，不把其它唯一约束
// （如将来加的组合索引）误报成「幂等键重复」——
// 照 instruments.go 的同类做法：先看 SQLSTATE 23505，再核对约束名。
func execClientOrderIDViolation(err error) bool {
	if !IsUniqueViolation(err) {
		return false
	}
	// SQLite 的 UNIQUE 报错只有列名，没有约束名（见 ARCH §4.2）。
	return strings.Contains(err.Error(), "orders_execution.client_order_id") ||
		strings.Contains(err.Error(), "orders_execution.id")
}

// DailyUsage 是某账户当日的额度占用。
//
// # 为什么金额用 string 而不是 float64
//
// NUMERIC(28,10) 有 28 位精度。float64 只有 15-17 位有效数字，
// 在「当日累计」这种会越加越大的场景下会累积误差 —— 而金额不能有误差。
type DailyUsage struct {
	// Notional 是已估值部分的占用金额（decimal 字符串）。
	Notional string
	// OrderCount 是计入额度的订单笔数。
	OrderCount int
	// UnpricedCount 是**无法估值**的订单数。
	//
	// 非零时 Notional **不可信**，调用方必须拒绝而不是当作 0。
	//
	// # 为什么单独返回而不是算进 Notional
	//
	//   算进 0     = 统计偏小 = 不安全方向（可能放行超限订单）
	//   算成一个估值 = 编造数据（无法估值不等于估成 0）
	//   单独返回    = 让调用方显式决定怎么处理
	//
	// 「不知道怎么算」和「算出来是 0」是两件事。
	UnpricedCount int
}

// execCountedStates 是计入当日额度的订单状态。
//
// # 为什么含 submitted / partial
//
// 「已提交未成交」的订单**已经占用了通道额度**，只是还没成交。
// 不计入的话，用户可以在同一秒提交 N 笔，每笔单独看都不超限，
// 合起来超 100 倍 —— 这是 TOCTOU 在金融场景的典型形态。
//
// 不计入的：cancelled / rejected（未实际占用）、
// draft / risk_*（还没到执行层）、settled（已结算，额度已释放？——
// 见下）
//
// # 关于 settled
//
// settled 的订单**曾经**占过额度。如果排除它，一个「今天下单→今天结算」的
// 循环可以无限刷额度。
//
// 但「当日额度」的语义是「今天下了多少钱的单」，而不是「今天还欠多少钱」。
// 所以已结算的订单**仍然计入** —— 它确实是今天下的单。
//
// 这与「持仓限额」不同：后者才关心「现在还占着多少」。
const execCountedStatesSQL = `('submitted','partial','filled','settled')`

// DailyUsage 统计某账户在 [day 00:00, day+1 00:00) 的额度占用。
//
// # 时区
//
// day 必须由调用方按**交易所时区**算好。这里不做时区转换 ——
// 「日界线」的定义依赖 market（crypto 用 UTC，A 股用 Asia/Shanghai），
// 而那是调用方（gate）手里才有的信息。
//
// # 金额口径
//
//	已成交部分：filled_qty × COALESCE(avg_price, ref_price)
//	未成交部分：(qty - filled_qty) × ref_price
//
// 未成交部分用草案 ref_price（保守：按委托价估，不按更低的市价估）。
//
// # UnpricedCount
//
// ref_price 为 NULL（市价单）且尚无 avg_price 的订单 → 无法估值 → 计入 UnpricedCount，
// 金额记为 0 但这**不代表安全**。
func (s *ExecOrderStore) DailyUsage(ctx context.Context, accountID int64, dayStart, dayEnd time.Time) (DailyUsage, error) {
	if accountID <= 0 {
		return DailyUsage{}, fmt.Errorf("%w: account_id 必须为正", ErrOrderInvalid)
	}
	if !dayEnd.After(dayStart) {
		return DailyUsage{}, fmt.Errorf("%w: dayEnd 必须晚于 dayStart", ErrOrderInvalid)
	}

	// # 为什么金额在 Go 层累加（SQLite 迁移）
	//
	// 金额列是 TEXT，SQL 算术只能 CAST(... AS REAL)（float64）。当日额度是
	// **一整天累加的量**，它的用途是「是否超限」的判定 —— float64 的尾部漂移
	// 会让接近上限的单被误放行或误拒。所以 SQL 只筛行，金额走 big.Rat。
	//
	// COUNT 与 UnpricedCount 仍是数值统计，留在 SQL 里（不涉及金额精度）。
	rows, err := s.db.QueryContext(ctx, `
SELECT
    oe.filled_qty,
    oe.avg_price,
    oe.qty,
    od.ref_price
FROM orders_execution oe
JOIN orders_draft od ON od.id = oe.draft_id
WHERE oe.account_id = $1
  AND oe.state IN `+execCountedStatesSQL+`
  AND oe.state_at >= $2 AND oe.state_at < $3`,
		accountID, FormatISO(dayStart), FormatISO(dayEnd))
	if err != nil {
		return DailyUsage{}, fmt.Errorf("统计当日额度失败: %w", err)
	}
	defer rows.Close()

	notional := new(big.Rat)
	var u DailyUsage
	for rows.Next() {
		var (
			filledS, avgS, qtyS, refS sql.NullString
		)
		if err := rows.Scan(&filledS, &avgS, &qtyS, &refS); err != nil {
			return DailyUsage{}, fmt.Errorf("统计当日额度失败: %w", err)
		}
		u.OrderCount++
		if refS.Valid == false && avgS.Valid == false {
			// 市价单且尚无成交价 → 无法估值。金额记 0 但调用方必须拒绝。
			u.UnpricedCount++
			continue
		}
		amt, ok := dailyUsageNotional(filledS, avgS, qtyS, refS)
		if !ok {
			u.UnpricedCount++
			continue
		}
		notional = DecimalAdd(notional, amt)
	}
	if err := rows.Err(); err != nil {
		return DailyUsage{}, fmt.Errorf("统计当日额度失败: %w", err)
	}
	u.Notional = DecimalString(notional, decimalScaleNotional)
	return u, nil
}

// dailyUsageNotional 算一笔订单的额度占用：
//
//	已成交部分：filled_qty × COALESCE(avg_price, ref_price)
//	未成交部分：(qty - filled_qty) × COALESCE(ref_price, 0)
//
// 任一必要值为空/非法时返回 ok=false（调用方计入 UnpricedCount）。
func dailyUsageNotional(filled, avgPrice, qty, ref sql.NullString) (*big.Rat, bool) {
	filledQty := new(big.Rat)
	if filled.Valid && strings.TrimSpace(filled.String) != "" {
		r, err := ParseDecimal(filled.String)
		if err != nil {
			return nil, false
		}
		filledQty = r
	}
	totalQty := filledQty
	if qty.Valid && strings.TrimSpace(qty.String) != "" {
		r, err := ParseDecimal(qty.String)
		if err != nil {
			return nil, false
		}
		totalQty = r
	}
	// 成交价优先：avg_price 是实际成交价，ref_price 只是委托价。
	priceStr := ""
	if avgPrice.Valid && strings.TrimSpace(avgPrice.String) != "" {
		priceStr = avgPrice.String
	} else if ref.Valid && strings.TrimSpace(ref.String) != "" {
		priceStr = ref.String
	}
	if priceStr == "" {
		return nil, false
	}
	fillPrice, err := ParseDecimal(priceStr)
	if err != nil {
		return nil, false
	}
	// 未成交部分按 ref_price（保守：按委托价估），没有 ref_price 就用 0。
	restPrice := new(big.Rat)
	if ref.Valid && strings.TrimSpace(ref.String) != "" {
		if r, err := ParseDecimal(ref.String); err == nil {
			restPrice = r
		}
	}
	filledAmt := DecimalMul(filledQty, fillPrice)
	restAmt := DecimalMul(DecimalSub(totalQty, filledQty), restPrice)
	return DecimalAdd(filledAmt, restAmt), true
}
