package db

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// TradeFill 是一笔成交明细（trade_fills 的一行）。
//
// Fee 是**负数**（OKX 用负号表示支出）。计算净盈亏时直接相加，
// 不要取绝对值 —— 取绝对值会把支出当成收入。
type TradeFill struct {
	ID        int64  `json:"id"`
	FillID    string `json:"fill_id"`
	AccountID int64  `json:"account_id"`
	InstID    string `json:"inst_id"`
	Side      string `json:"side"` // buy | sell
	PosSide   string `json:"pos_side"`
	FillPx    string `json:"fill_px"`
	FillSz    string `json:"fill_sz"`
	Fee       string `json:"fee"`
	FeeCcy    string `json:"fee_ccy"`
	OrdID     string `json:"ord_id"`
	ClOrdID   string `json:"cl_ord_id"`
	DraftID   int64  `json:"draft_id"`
	FilledAt  string `json:"filled_at"`
}

// InsertFill 落一笔成交。
//
// 幂等：fill_id 有 UNIQUE 约束，重复插入返回 (0, false, nil) 而不是错误 ——
// 调用方每轮都会拉最近窗口，重复是常态，不该当故障。
func (d *DB) InsertFill(f TradeFill) (int64, bool, error) {
	if strings.TrimSpace(f.FillID) == "" {
		return 0, false, fmt.Errorf("fill_id 不能为空")
	}
	if f.PosSide == "" {
		f.PosSide = "net"
	}
	res, err := d.Exec(`INSERT INTO trade_fills
(fill_id, account_id, inst_id, side, pos_side, fill_px, fill_sz, fee, fee_ccy, ord_id, cl_ord_id, draft_id, filled_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
ON CONFLICT (fill_id) DO NOTHING`,
		f.FillID, f.AccountID, f.InstID, strings.ToLower(f.Side), f.PosSide,
		f.FillPx, f.FillSz, defaultStr(f.Fee, "0"), nullIfBlank(f.FeeCcy),
		nullIfBlank(f.OrdID), nullIfBlank(f.ClOrdID), nullIfEmptyI64(f.DraftID), f.FilledAt)
	if err != nil {
		return 0, false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return 0, false, nil // 已存在
	}
	id, _ := res.LastInsertId()
	return id, true, nil
}

// FillsFilter 是交易记录的查询条件（全部可选）。
type FillsFilter struct {
	TaskID    int64
	AccountID int64
	InstID    string
	Limit     int
}

// ListFills 按任务/账户/标的查成交明细。
//
// # 任务怎么关联（关键）
//
// trade_fills 没有 task_id 列：一笔成交属于哪个任务，取决于它的 cl_ord_id
// 对应哪张 orders_execution、进而哪张 orders_draft、进而哪条 signals.task_id。
// 这条链是三段 JOIN，所以不能只按 trade_fills 单表查。
//
// 为什么不直接给 trade_fills 加 task_id：成交是交易所侧的事实，可能在
// 我方记录之前就已经发生（手工单、对账补拉）。把任务写死在行上，
// 一旦归属判断错了就永远错，而 JOIN 是按当前数据算的，能自愈。
func (d *DB) ListFills(f FillsFilter) ([]TradeFill, error) {
	if f.Limit <= 0 || f.Limit > 2000 {
		f.Limit = 500
	}
	q := `SELECT tf.id, tf.fill_id, tf.account_id, tf.inst_id, tf.side, tf.pos_side,
tf.fill_px, tf.fill_sz, tf.fee, COALESCE(tf.fee_ccy,''), COALESCE(tf.ord_id,''),
COALESCE(tf.cl_ord_id,''), COALESCE(tf.draft_id,0), tf.filled_at
FROM trade_fills tf`
	args := []any{}
	where := []string{}

	// 归属路径（任一命中即算本任务）：
	//   ① trade_records.task_id（工具下单，2026-11-14 起的主路径）
	//   ② tf.draft_id 直接命中（旧 TradeLoop 写入时已知草案）
	//   ③ tf.cl_ord_id → orders_execution.client_order_id
	//   ④ tf.ord_id   → orders_execution.channel_order_id
	//
	// ④ 不是冗余：**OKX 对部分成交不返回 clOrdId**（2026-11-14 实测手工单
	// 的 clOrdId 为空串）。只靠 ③ 会让交易记录页在该情形下永远空白，
	// 而 ordId 是每笔成交必有的字段。
	//
	// ① 排在最前：工具下单不产生 orders_execution / signals 链路，
	// 只靠 ②③④ 会让「LLM 直接下的单」在任务盈亏里永远为 0。
	q += `
LEFT JOIN trade_records tr ON (tr.client_order_id = tf.cl_ord_id OR (tf.cl_ord_id IS NULL AND tr.channel_order_id = tf.ord_id))
LEFT JOIN orders_execution oe ON (oe.client_order_id = tf.cl_ord_id OR oe.channel_order_id = tf.ord_id)
LEFT JOIN orders_draft od ON od.id = COALESCE(tf.draft_id, oe.draft_id)`
	if f.TaskID > 0 {
		q += "\nLEFT JOIN signals s ON s.id = od.signal_id"
		n := len(args) + 1
		where = append(where, fmt.Sprintf(
			`(tr.task_id = $%d OR (od.signal_id IS NOT NULL AND s.task_id = $%d))`, n, n))
		args = append(args, f.TaskID)
	}
	if f.AccountID > 0 {
		where = append(where, fmt.Sprintf("tf.account_id = $%d", len(args)+1))
		args = append(args, f.AccountID)
	}
	if s := strings.TrimSpace(f.InstID); s != "" {
		where = append(where, fmt.Sprintf("tf.inst_id = $%d", len(args)+1))
		args = append(args, s)
	}
	if len(where) > 0 {
		q += "\nWHERE " + strings.Join(where, " AND ")
	}
	q += fmt.Sprintf("\nORDER BY tf.filled_at ASC, tf.id ASC LIMIT $%d", len(args)+1)
	args = append(args, f.Limit)

	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]TradeFill, 0, 16)
	for rows.Next() {
		var tf TradeFill
		if err := rows.Scan(&tf.ID, &tf.FillID, &tf.AccountID, &tf.InstID, &tf.Side, &tf.PosSide,
			&tf.FillPx, &tf.FillSz, &tf.Fee, &tf.FeeCcy, &tf.OrdID,
			&tf.ClOrdID, &tf.DraftID, &tf.FilledAt); err != nil {
			return nil, err
		}
		out = append(out, tf)
	}
	return out, rows.Err()
}

// LocalNetPositions 按标的汇总本地成交推算的净持仓（正=多，负=空）。
//
// # 与 PnL 的关系
//
// PnL 是「逐笔加权成本法」算出来的商业口径（含已实现盈亏）。
// 这里要的是更朴素的东西：**净敞口**。对账只关心「我这边认为还有几手」，
// 不需要成本和盈亏 —— 而且那两者都是派生的，不能用来互相校验。
//
// 用 SQL 聚合而不是拉全量到内存：单个账户的成交可能有几千条，对账每轮都要跑。
func (d *DB) LocalNetPositions(accountID int64) (map[string]string, error) {
	if accountID <= 0 {
		return map[string]string{}, nil
	}
	rows, err := d.Query(`
SELECT inst_id,
       SUM(CASE WHEN LOWER(side)='buy' THEN CAST(fill_sz AS REAL)
                ELSE -CAST(fill_sz AS REAL) END) AS net
FROM trade_fills WHERE account_id = $1
GROUP BY inst_id`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var inst string
		var net float64
		if err := rows.Scan(&inst, &net); err != nil {
			return nil, err
		}
		// 抹掉浮点噪声后再格式化。
		//
		// # 为什么（2026-11-15 首次上线就踩到）
		//
		// SQLite 没有十进制类型，SUM(CAST(fill_sz AS REAL)) 会积累误差：
		// 一买一卖「应该正好抵消」时能算出 -1.4e-17。直接格式化就成了
		// "-0"，而对账侧交易所返回 "0" —— 两者数值相等，字符串不等，
		// 于是漂移检测报了一条假阳性。
		//
		// 按 8 位小数归整：OKX 最细的合约张数精度是 1e-8 级别，再细的就是噪声。
		if math.Abs(net) < 5e-9 {
			net = 0
		} else {
			net = math.Round(net*1e8) / 1e8
		}
		out[inst] = strconv.FormatFloat(net, 'f', -1, 64)
	}
	return out, rows.Err()
}

// HasFillsFor 报告某任务是否已有任何成交（供前端区分「还没成交」与「查不到」）。
func (d *DB) HasFillsFor(taskID int64) (bool, error) {
	var n int
	err := d.QueryRow(`SELECT COUNT(*) FROM trade_fills tf
LEFT JOIN orders_execution oe ON (oe.client_order_id = tf.cl_ord_id OR oe.channel_order_id = tf.ord_id)
LEFT JOIN orders_draft od ON od.id = COALESCE(tf.draft_id, oe.draft_id)
LEFT JOIN signals s ON s.id = od.signal_id
WHERE s.task_id = $1 LIMIT 1`, taskID).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func defaultStr(s, def string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	return s
}

// DistinctInstIDsForAccount 返回某账户**我方下过单**的标的代码列表。
//
// 用途：拉成交明细时按标的逐个拉（OKX 的 fills 接口单次上限 100 条，
// 一次拉全市场会截断）。只统计我方发过的单 —— 手工在 App 下的单不进来，
// 那是刻意的：交易记录是「本系统做了什么」的账，不是「账户发生了什么」。
func (d *DB) DistinctInstIDsForAccount(accountID int64) ([]string, error) {
	// 两个来源：
	//   ① trade_records（工具下单，2026-11-14 起的主路径）—— inst_id 直存，不必 JOIN
	//   ② orders_execution → draft → instruments（旧草案链路）
	//
	// 只查 ② 的后果：工具下的单**永远不会被同步成交**，任务详情的实盘成交
	// 与盈亏恒为 0（实测任务 29：交易所已有仓、台账已落，但 live fills=0）。
	rows, err := d.Query(`SELECT DISTINCT symbol FROM (
  SELECT inst_id AS symbol FROM trade_records WHERE account_id = $1 AND inst_id <> ''
  UNION
  SELECT i.symbol FROM orders_execution oe
    JOIN orders_draft od ON od.id = oe.draft_id
    JOIN instruments i ON i.id = od.instrument_id
   WHERE oe.account_id = $1 AND i.symbol IS NOT NULL AND i.symbol <> ''
) t WHERE symbol <> '' ORDER BY symbol`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// DraftIDByClOrdID 按 cl_ord_id 反查草案 id（查不到返回 0，不报错）。
//
// 用途：落成交明细时尽量带上 draft_id，让「任务 → 成交」的关联多一条路径。
// 查不到是正常情况（该成交不是本系统的单，或 execution 行尚未落库）。
func (d *DB) DraftIDByClOrdID(clOrdID string) int64 {
	if strings.TrimSpace(clOrdID) == "" {
		return 0
	}
	var id int64
	err := d.QueryRow(`SELECT draft_id FROM orders_execution WHERE client_order_id=$1 LIMIT 1`, clOrdID).Scan(&id)
	if err != nil {
		return 0
	}
	return id
}

// AccountIDsForTask 返回该任务涉及的去重账户 id（来自其订单执行记录）。
func (d *DB) AccountIDsForTask(taskID int64) ([]int64, error) {
	// 两个来源都要算：
	//   ① trade_records（工具下单，2026-11-14 起的主路径）
	//   ② orders_execution → draft → signal（旧草案链路，兼容存量任务）
	//
	// 只查 ② 会让「工具下的单」查不到账户，同步入口直接 400 ——
	// 实测 2026-11-14 就是这样：任务 28 有 2 条台账、4 笔成交，
	// 但点「立即同步」报「没有任何下单记录」。
	rows, err := d.Query(`SELECT DISTINCT account_id FROM (
  SELECT account_id FROM trade_records WHERE task_id = $1 AND account_id > 0
  UNION
  SELECT oe.account_id FROM orders_execution oe
    JOIN orders_draft od ON od.id = oe.draft_id
    JOIN signals s ON s.id = od.signal_id
   WHERE s.task_id = $1 AND oe.account_id > 0
) t ORDER BY account_id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// TaskIDByDraftID 按草案反查任务 id（查不到返回 0）。
//
// 用于给「平仓单」补任务归属：平仓单由 TradeLoop 触发，手里只有
// paper_trade_plans.draft_id。丢了 task_id 的后果是平仓成交不归属任何任务，
// 任务盈亏里只剩开仓 → 未平仓量与已实现盈亏都会算错。
func (d *DB) TaskIDByDraftID(draftID int64) int64 {
	if draftID <= 0 {
		return 0
	}
	var id int64
	err := d.QueryRow(`SELECT COALESCE(s.task_id,0)
FROM orders_draft od LEFT JOIN signals s ON s.id = od.signal_id
WHERE od.id = $1`, draftID).Scan(&id)
	if err != nil {
		return 0
	}
	return id
}

// DraftIDByInstRecently 按「账户 + 标的」找最近一条持仓跟踪记录的草案 id。
//
// # 用途（这是交易所侧平仓的唯一归属线索）
//
// 止盈止损是挂在交易所的 OCO，触发平仓时用的是**交易所自己的 clOrdId**
// （形如 O4098…），不是我方派生的 artex…。所以平仓成交无法通过 cl_ord_id
// 关联到草案 —— 实测任务 23 只看到开仓腿，持仓永远显示 0.1 未平。
//
// 本系统的约束是「同一标的同时只持一个仓」（有仓不开新仓），
// 所以「同账户同标的最近一条跟踪记录」就是这笔平仓的来源仓。
func (d *DB) DraftIDByInstRecently(accountID int64, instID string) int64 {
	if accountID <= 0 || strings.TrimSpace(instID) == "" {
		return 0
	}
	var id int64
	err := d.QueryRow(`SELECT COALESCE(draft_id,0) FROM live_positions
WHERE account_id=$1 AND inst_id=$2 AND draft_id IS NOT NULL
ORDER BY id DESC LIMIT 1`, accountID, instID).Scan(&id)
	if err != nil {
		return 0
	}
	return id
}

// BackfillFillDraftID 给「归属不上任何订单」的成交补草案归属。
//
// # 为什么需要（幂等插入的副作用）
//
// InsertFill 用 ON CONFLICT DO NOTHING，所以早先插入时 draft_id 为空的行
// 后续同步不会更新。修复归属逻辑之后，那些行仍是空的 —— 任务盈亏里
// 只有开仓腿，持仓永远显示未平（实测任务 23）。
//
// # 为什么必须按时间窗，不能只按 instId
//
// 同一标的会反复开平（实测 NVDA 一天开了 3 次）。只按 instId 归属会把
// **历史上所有**无人认领的成交都算到最新那张草案上 —— 实测 open_qty
// 从 0.1 变成 -0.2，比不归属更糟：它给出了一个看起来精确的错数。
//
// 所以窗口取该持仓跟踪记录的 [opened_at, closed_at]（未平时用 now）。
// 「同一标的同时只有一个仓」的约束保证窗口不重叠。
//
// 只处理 cl_ord_id 不属于我方订单的行：那些是交易所 OCO 触发的平仓，
// clOrdId 是交易所生成的，无法反查。属于我方订单的成交一律不动。
func (d *DB) BackfillFillDraftID(accountID int64, instID string, draftID int64) (int, error) {
	if accountID <= 0 || draftID <= 0 || strings.TrimSpace(instID) == "" {
		return 0, nil
	}
	var opened string
	var closed sql.NullString
	if err := d.QueryRow(`SELECT COALESCE(opened_at,''), closed_at FROM live_positions
WHERE account_id=$1 AND inst_id=$2 AND draft_id=$3 ORDER BY id DESC LIMIT 1`,
		accountID, instID, draftID).Scan(&opened, &closed); err != nil {
		return 0, nil // 没有跟踪记录 → 无从判断窗口，不动
	}
	if strings.TrimSpace(opened) == "" {
		return 0, nil
	}
	until := "9999-12-31T23:59:59Z"
	if closed.Valid && strings.TrimSpace(closed.String) != "" {
		until = closed.String
	}

	// 复位与回填都**限定在本窗口内**，这样可以对同一标的的多条持仓
	// 逐个调用而互不破坏（早先的版本复位全标的，导致只有最后一次生效，
	// 任务 21/22 的平仓腿永远补不上）。
	if _, err := d.Exec(`UPDATE trade_fills SET draft_id=NULL
WHERE account_id=$1 AND inst_id=$2 AND filled_at >= $3 AND filled_at <= $4
  AND draft_id IS NOT NULL
  AND (cl_ord_id IS NULL OR cl_ord_id = ''
       OR cl_ord_id NOT IN (SELECT client_order_id FROM orders_execution))`,
		accountID, instID, opened, until); err != nil {
		return 0, err
	}

	res, err := d.Exec(`UPDATE trade_fills SET draft_id=$1
WHERE account_id=$2 AND inst_id=$3 AND draft_id IS NULL
  AND filled_at >= $4 AND filled_at <= $5
  AND (cl_ord_id IS NULL OR cl_ord_id = ''
       OR cl_ord_id NOT IN (SELECT client_order_id FROM orders_execution))`,
		draftID, accountID, instID, opened, until)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// DraftIDsForInst 返回某账户某标的所有持仓跟踪记录对应的草案 id（按时间升序）。
//
// 同一标的会反复开平，归属回填必须逐条窗口处理 —— 只取最新一条会让
// 更早那些持仓的平仓腿永远补不上（实测任务 21/22）。
func (d *DB) DraftIDsForInst(accountID int64, instID string) ([]int64, error) {
	if accountID <= 0 || strings.TrimSpace(instID) == "" {
		return nil, nil
	}
	rows, err := d.Query(`SELECT DISTINCT COALESCE(draft_id,0) FROM live_positions
WHERE account_id=$1 AND inst_id=$2 AND draft_id IS NOT NULL ORDER BY id`, accountID, instID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if id > 0 {
			out = append(out, id)
		}
	}
	return out, rows.Err()
}
