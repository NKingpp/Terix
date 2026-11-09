package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// =====================================================================
// 下单草稿(orders_draft)数据层
// =====================================================================

// 风控结论取值,与 schema.sql 的 CHECK 约束一致。
const (
	RiskVerdictPass = "pass"
	RiskVerdictDeny = "deny"
	RiskVerdictAsk  = "ask"
)

// 买卖方向取值。
const (
	OrderSideBuy  = "buy"
	OrderSideSell = "sell"
)

var (
	// ErrDraftInvalid 表示入参非法(方向、数量、风控结论)。
	ErrDraftInvalid = errors.New("invalid order draft")
	// ErrDraftNotFound 表示草稿不存在。
	ErrDraftNotFound = errors.New("order draft not found")
)

// validRiskVerdicts 是风控结论白名单。
var validRiskVerdicts = map[string]bool{
	RiskVerdictPass: true, RiskVerdictDeny: true, RiskVerdictAsk: true,
}

// validOrderSides 是方向白名单。
var validOrderSides = map[string]bool{
	OrderSideBuy: true, OrderSideSell: true,
}

// OrderDraft is a row in the orders_draft table.
// Qty / RefPrice 是 NUMERIC,一律用字符串承载,禁止 float64。
type OrderDraft struct {
	ID           int64   `json:"id"`
	SignalID     *int64  `json:"signal_id,omitempty"`
	InstrumentID *int64  `json:"instrument_id,omitempty"`
	Side         string  `json:"side"`
	Qty          string  `json:"qty"`
	RefPrice     *string `json:"ref_price,omitempty"`
	RiskVerdict  *string `json:"risk_verdict,omitempty"`
	ExportedAt   *string `json:"exported_at,omitempty"`
	ExecutedNote *string `json:"executed_note,omitempty"`
	CreatedAt    string  `json:"created_at"`
	// TaskID 由 JOIN signals 带出,orders_draft 本身不存 task_id。
	TaskID *int64 `json:"task_id,omitempty"`
}

// OrderDraftInput 是 InsertDraft 的入参。
type OrderDraftInput struct {
	SignalID *int64
	// RatingID 记录「草案由哪条评级生成」，是「评级→草案」幂等的唯一判据。
	//
	// 唯一性由 DB 的部分唯一索引 uq_orders_draft_rating 保证（不是应用层查重）——
	// 应用层「先 SELECT 再 INSERT」在并发下会同时通过（TOCTOU），
	// 而 UNIQUE 让只有一个能成功，另一个拿 23505（调用方转成 created=false）。
	RatingID     *int64
	InstrumentID *int64
	Side         string
	Qty          string
	RefPrice     string
	RiskVerdict  string
	ExecutedNote string
}

// OrderDraftStore operates on the orders_draft table.
type OrderDraftStore struct{ db *DB }

// OrderDrafts returns the order draft store associated with this DB.
func (d *DB) OrderDrafts() *OrderDraftStore { return &OrderDraftStore{db: d} }

// draftColumns 与 scanDraft 的扫描顺序必须一致。
// TaskID 走 LEFT JOIN 取,草稿可能没有关联信号(手写下单)。
const draftColumns = `od.id, od.signal_id, od.instrument_id, od.side, od.qty,
	od.ref_price, od.risk_verdict, od.exported_at, od.executed_note,
	od.created_at, s.task_id`

// scanDraft 从一行结果读出一条 OrderDraft。
func scanDraft(row interface{ Scan(...any) error }) (*OrderDraft, error) {
	var (
		d          OrderDraft
		signalID   sql.NullInt64
		instID     sql.NullInt64
		refPrice   sql.NullString
		risk       sql.NullString
		exportedAt sql.NullString
		note       sql.NullString
		taskID     sql.NullInt64
	)
	if err := row.Scan(
		&d.ID, &signalID, &instID, &d.Side, &d.Qty,
		&refPrice, &risk, &exportedAt, &note, &d.CreatedAt, &taskID,
	); err != nil {
		return nil, err
	}
	d.SignalID = nullInt64Ptr(signalID)
	d.InstrumentID = nullInt64Ptr(instID)
	d.RiskVerdict = nullStringPtr(risk)
	d.ExportedAt = nullStringPtr(exportedAt)
	d.ExecutedNote = nullStringPtr(note)
	d.TaskID = nullInt64Ptr(taskID)
	if refPrice.Valid {
		v := refPrice.String
		d.RefPrice = &v
	}
	return &d, nil
}

// nullStringPtr 把 NULL 映射为 nil,非 NULL 映射为值指针。
func nullStringPtr(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	v := n.String
	return &v
}

// validDecimalLiteral 粗校验十进制字面量形状;范围由 DB 的 NUMERIC 兜底。
func validDecimalLiteral(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && r != '.' && r != '-' {
			return false
		}
	}
	return true
}

// InsertDraft 写入一条下单草稿并返回新 id。
func (s *OrderDraftStore) InsertDraft(in OrderDraftInput) (int64, error) {
	side := strings.TrimSpace(in.Side)
	if !validOrderSides[side] {
		return 0, fmt.Errorf("%w: side %q 不在 buy/sell 之内", ErrDraftInvalid, in.Side)
	}
	if !validDecimalLiteral(in.Qty) {
		return 0, fmt.Errorf("%w: qty %q 不是合法十进制数", ErrDraftInvalid, in.Qty)
	}
	if strings.TrimSpace(in.Qty) == "0" || strings.TrimSpace(in.Qty) == "0.0" {
		return 0, fmt.Errorf("%w: qty 不能为 0", ErrDraftInvalid)
	}
	if rp := strings.TrimSpace(in.RefPrice); rp != "" && !validDecimalLiteral(rp) {
		return 0, fmt.Errorf("%w: ref_price %q 不是合法十进制数", ErrDraftInvalid, rp)
	}
	if rv := strings.TrimSpace(in.RiskVerdict); rv != "" && !validRiskVerdicts[rv] {
		return 0, fmt.Errorf("%w: risk_verdict %q 不在 pass/deny/ask 之内", ErrDraftInvalid, in.RiskVerdict)
	}
	var id int64
	err := s.db.QueryRow(`
INSERT INTO orders_draft(signal_id, rating_id, instrument_id, side, qty, ref_price, risk_verdict, executed_note)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id`,
		in.SignalID, in.RatingID, in.InstrumentID, side,
		normalizeDecimalInput(in.Qty, decimalScaleAmount),
		normalizeOptDecimal(in.RefPrice, decimalScaleAmount),
		normalizeOptText(in.RiskVerdict), normalizeOptText(in.ExecutedNote),
	).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

// GetDraft 按 id 取一条草稿,查不到返回 ErrDraftNotFound。
func (s *OrderDraftStore) GetDraft(id int64) (*OrderDraft, error) {
	row := s.db.QueryRow(`
SELECT `+draftColumns+`
FROM orders_draft od LEFT JOIN signals s ON s.id = od.signal_id
WHERE od.id = $1`, id)
	d, err := scanDraft(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrDraftNotFound
		}
		return nil, err
	}
	return d, nil
}

// UpdateRiskVerdict 写入风控结论。草稿已导出(exported_at 非空)时拒绝修改 ——
// 已经给出去的单子事后改风控结论没有意义,只会让审计记录对不上。
func (s *OrderDraftStore) UpdateRiskVerdict(id int64, verdict string) error {
	verdict = strings.TrimSpace(verdict)
	if !validRiskVerdicts[verdict] {
		return fmt.Errorf("%w: risk_verdict %q 不在 pass/deny/ask 之内", ErrDraftInvalid, verdict)
	}
	res, err := s.db.Exec(`
UPDATE orders_draft SET risk_verdict = $1
WHERE id = $2 AND exported_at IS NULL`, verdict, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		// 区分「不存在」与「已导出不可改」,调用方能给出准确提示。
		if _, getErr := s.GetDraft(id); getErr != nil {
			return getErr
		}
		return fmt.Errorf("%w: 草稿 %d 已导出,风控结论不可再改", ErrDraftInvalid, id)
	}
	return nil
}

// ListDraftsByTask 列出某任务下所有信号派生出的草稿,最新在前。
// orders_draft 没有 task_id 列,经 signals.task_id 关联过滤。
func (s *OrderDraftStore) ListDraftsByTask(taskID int64) ([]*OrderDraft, error) {
	if taskID <= 0 {
		return nil, fmt.Errorf("%w: task_id 必须为正", ErrDraftInvalid)
	}
	rows, err := s.db.Query(`
SELECT `+draftColumns+`
FROM orders_draft od
JOIN signals s ON s.id = od.signal_id
WHERE s.task_id = $1
ORDER BY od.created_at DESC, od.id DESC`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]*OrderDraft, 0, 16)
	for rows.Next() {
		d, err := scanDraft(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// CountDraftsSince 统计某标的在某时间窗内的草案笔数（供 intercept 的 daily_count 用）。
//
// # 为什么这里直接写 SQL 而不是复用上层
//
// CountFunc 的接口是 `func(ctx, CountQuery) (int64, error)`，它的语义就是
// 「按这些条件数一数」。放在 db 层让 intercept 不必知道表结构。
//
// # 为什么按 created_at 而不是 state_at
//
// 草案没有 state_at —— 它是「意图」，不是「执行」。风控规则 4/5 关心的是
// 「今天产生了多少下单意图」，所以按创建时间。
func (d *DB) CountDraftsSince(ctx context.Context, instrumentID int64, sinceIncl, untilExcl time.Time) (int64, error) {
	var n int64
	err := d.QueryRowContext(ctx, `
SELECT COUNT(*) FROM orders_draft
WHERE instrument_id = $1
  AND created_at >= $2 AND created_at < $3`, instrumentID, FormatISO(sinceIncl), FormatISO(untilExcl)).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// ErrDraftExistsForRating 表示该评级已生成过草案（唯一索引冲突）。
//
// # 为什么单独一个哨兵
//
// 「已有草案」不是错误 —— 它让调用方返回 created=false 而不是 500。
// 而把它当成通用 DB 错误会让调用方需要解析 SQLSTATE，那是脆弱的分支。
var ErrDraftExistsForRating = errors.New("该评级已生成过草案")

// InsertDraftForRating 幂等地从评级生成草案。
//
// # 为什么单独一个方法而不是给 InsertDraft 加参数
//
// `InsertDraft` 是通用的（也用于手写下单 / 交易信号），不该感知「幂等」这个概念。
// 而本方法专门服务「评级 → 草案」这条路径，它的幂等语义是这个调用场景的属性。
//
// # 返回值
//
//	(id, true, nil)   —— 本次真的建了
//	(0, false, nil)   —— 该评级已有草案（幂等命中）
//	(0, false, err)   —— 其它错误
func (s *OrderDraftStore) InsertDraftForRating(in OrderDraftInput) (int64, bool, error) {
	if in.RatingID == nil {
		return 0, false, fmt.Errorf("%w: InsertDraftForRating 要求 RatingID 非空（它是幂等键）", ErrDraftInvalid)
	}
	id, err := s.InsertDraft(in)
	if err != nil {
		// 唯一键冲突（PG 23505 / SQLite 2067）。这里唯一的唯一约束是
		// uq_orders_draft_rating，所以冲突必然来自「该评级已有草案」。
		//
		// # 为什么按错误码而不是按错误文本匹配
		//
		// 错误文本随 locale 与驱动版本变化；错误码是稳定的判定依据。
		// 注入钩子 isUniqueFn 让测试能模拟这个分支（*sqlite.Error 无法构造）。
		if isUniqueViolation(err) {
			return 0, false, nil // 幂等命中 —— 不是错误
		}
		return 0, false, err
	}
	return id, true, nil
}

// isUniqueViolation 判断错误是否为唯一约束冲突（PG 23505 / SQLite 2067、1555）。
func isUniqueViolation(err error) bool { return isUniqueFn(err) }

// InsertDraftQtyExpr 用一个 SQL 表达式作为 qty（而不是字面量）。
//
// # 为什么需要它
//
// 「评级 → 草案」的数量要按「固定名义额 ÷ 参考价」算，而那是 NUMERIC 运算。
// 在 Go 里用 float64 中转会掉精度（金额不能用浮点），
// 所以让 DB 算，再把结果直接写入。
//
// # 安全性
//
// qtyExpr 是**调用方构造的 SQL 片段**，会拼进 INSERT。所以调用方必须
// 只传自己拼的、参数已校验的表达式（如 server/rating_draft.go 的 draftQtyFromRating）。
// 这个方法刻意不接受用户输入。
//
// # 退化行为
//
// 如果 qtyExpr 为空，等价于调用 InsertDraft（走 Qty 字段）。
func (s *OrderDraftStore) InsertDraftQtyExpr(in OrderDraftInput, qtyExpr string) (int64, bool, error) {
	if strings.TrimSpace(qtyExpr) == "" {
		if in.RatingID == nil {
			return 0, false, fmt.Errorf("%w: 需要 RatingID（它是幂等键）", ErrDraftInvalid)
		}
		return s.InsertDraftForRating(in)
	}
	if in.RatingID == nil {
		return 0, false, fmt.Errorf("%w: InsertDraftQtyExpr 要求 RatingID 非空（它是幂等键）", ErrDraftInvalid)
	}
	side := strings.TrimSpace(in.Side)
	if !validOrderSides[side] {
		return 0, false, fmt.Errorf("%w: side %q 不在 buy/sell 之内", ErrDraftInvalid, in.Side)
	}
	if rp := strings.TrimSpace(in.RefPrice); rp != "" && !validDecimalLiteral(rp) {
		return 0, false, fmt.Errorf("%w: ref_price %q 不是合法十进制数", ErrDraftInvalid, rp)
	}
	if rv := strings.TrimSpace(in.RiskVerdict); rv != "" && !validRiskVerdicts[rv] {
		return 0, false, fmt.Errorf("%w: risk_verdict %q 不在 pass/deny/ask 之内", ErrDraftInvalid, in.RiskVerdict)
	}

	var id int64
	err := s.db.QueryRow(`
INSERT INTO orders_draft(signal_id, rating_id, instrument_id, side, qty, ref_price, risk_verdict, executed_note)
VALUES ($1, $2, $3, $4, `+qtyExpr+`, $5, $6, $7)
RETURNING id`,
		in.SignalID, in.RatingID, in.InstrumentID, side,
		normalizeOptText(in.RefPrice),
		normalizeOptText(in.RiskVerdict), normalizeOptText(in.ExecutedNote),
	).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, false, nil // 幂等命中
		}
		return 0, false, err
	}
	return id, true, nil
}

// ListAutoExecutable 列出可被自动交易执行的草案（最多 limit 条）。
//
// # 判定条件（三者缺一不可）
//
//	risk_verdict = 'pass'   已过风控，不是未判定/拒绝
//	exported_at IS NULL     还没发出去过 —— 发过的不重发（幂等的第一道闸）
//	executed_note IS NULL   还没被执行 —— 已执行的不能再来一次
//
// instrument_id IS NOT NULL 也要求：没有标的就组不出下单请求。
//
// # 为什么 exported_at 是必要条件
//
// 自动执行成功后会写 exported_at。若不排除，下一个 tick 会把同一草案
// 再发一遍 —— 而 client_order_id 相同，渠道可能当成两笔新单（取决于
// 渠道是否真的实现去重）。不依赖渠道行为更安全。
//
// # 为什么按 id 升序
//
// 老草案先执行。信号有实效性，积压时先处理更早的通常更合理。
func (s *OrderDraftStore) ListAutoExecutable(limit int) ([]*OrderDraft, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Query(`
SELECT `+draftColumns+`
FROM orders_draft od
LEFT JOIN signals s ON s.id = od.signal_id
WHERE od.risk_verdict = 'pass'
  AND od.exported_at IS NULL
  AND od.executed_note IS NULL
  AND od.instrument_id IS NOT NULL
ORDER BY od.id ASC
LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]*OrderDraft, 0, limit)
	for rows.Next() {
		d, err := scanDraft(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// MarkExported 记录草案已下发到渠道（写 exported_at）。
//
// # 为什么单独一个方法而不是复用 UpdateRiskVerdict
//
// 两者语义不同：verdict 是「判定结果」，exported_at 是「已发出去的物理事实」。
// 混在一个方法里会让调用方难以只更新其中一个。
//
// # 条件更新
//
// 只在 exported_at 仍为空时写入。并发下两个 tick 都想标同一单时，
// 只有一个能成功（返回 affected=0 的那个应该当作「别人已经发过了」）。
func (s *OrderDraftStore) MarkExported(id int64) (bool, error) {
	res, err := s.db.Exec(`
UPDATE orders_draft SET exported_at = now()
WHERE id = $1 AND exported_at IS NULL`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
