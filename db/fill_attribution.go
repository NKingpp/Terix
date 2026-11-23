package db

// 成交 → 任务 的归属（含交易所 algo 触发的平仓腿）。
//
// # 为什么不能只靠 cl_ord_id = trade_records.client_order_id
//
// 自带 clOrdId 的单（工具下单）能直接连回任务。但 OCO/TP/SL 触发时，
// 交易所生成的是**它自己的** clOrdId（`O` + 数字），我方从不写 trade_records 行
// —— 于是这些平仓腿被 INNER JOIN 整体丢弃。
//
// 实测（任务 39）：trade_fills 519 笔，能 JOIN 上的只有 493 笔；漏掉的 26 笔
// 正是 R2/R3 的平仓腿。后果不是「数字偏小」而是**净持仓符号都错**：
// 平台算出 open_qty=-53、realized=-0.0037，母账真值是 pos=0、+1474.4353。
// 人工验收因此必然 409（持仓未平），任务永远收不了尾。
//
// # 归属规则：逐任务净持仓 + 按比例拆份额
//
// 按 (account, inst) 时间升序走一遍，维护每个任务当前的净持仓：
//
//   - 有主成交（clOrdId / ordId 能连回台账）→ 整笔归该任务，累加其净持仓。
//   - 无主成交（交易所 algo 平仓腿）→ 拆给当前所有有持仓的任务，
//     份额 = size · |open_t| / Σ|open|，符号跟随这笔成交。
//     没有任何任务有持仓时不归属（不猜）。
//
// 份额夹在 |open_t| 以内：algo 腿不可能平掉某任务没持有的量。
//
// # 为什么不能用「持仓回合（episode）」
//
// 旧规则把「净额从 0 离开到回到 0」当一个回合，整个回合归回合首笔能连回台账的
// 任务。两个反例都来自生产（账户 4，任务 42/45）：
//
//  1. 一个回合能横跨多个任务。任务 45 在 2026-11-20T15:14–11-21T01:15 开了 20 张
//     空，而任务 42 同账户同标的手上还有 1 张空 —— 净额是一条连续的 -1→-21 曲线。
//     回合首笔是 42 的，于是 45 的 20 张开仓全被算给 42：TaskFills(45)=1 笔，
//     TaskFills(42)=6 笔、realized -891.6452（真值 -19.3920）。
//  2. 一笔成交能覆盖多个任务的持仓。11-21T12:02 的 SL 腿是**一个** fill 行、
//     21 张，其中 1 张属 42、20 张属 45。整笔归任何一方都错：
//     归 45 → 45 净持仓 -18（真值 -19）；归 42 → 45 净持仓 -39。按 1:20 拆才对。
//
// big.Rat 保证精确：1/21·21 == 1、20/21·21 == 20，无浮点漂移。
//
// # 为什么不用「按 instId 取最近一条草案」
//
// BackfillFillDraftID 用的时间窗启发式在同一标的反复开平时会把历史成交
// 算到最新一次上（实测 open_qty 从 0.1 变 -0.2，比不归属更糟）。
// 归属由成交与持仓数据本身定义，不依赖任何时间猜测。

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"strings"
)

// TaskFill 是一笔成交归属于某任务的份额。
//
// 一笔成交可以被拆给多个任务：交易所 algo 触发的平仓腿只对应一个 fill 行，
// 但它平掉的是多个任务各自的持仓（实测任务 45 的 21 张 SL 腿里
// 1 张属于任务 42、20 张属于任务 45）。整笔归任何一方都会让另一方的
// 净持仓算错，必须按份额拆。
type TaskFill struct {
	TradeFill
	// SplitSz 是本任务占这笔成交的份额（方向仍由 Side 决定，这里只是数量）。
	// 整笔归属时它等于 FillSz。
	SplitSz string
	// SplitFee 是本任务占的手续费份额，按 SplitSz/FillSz 同比例分摊。
	SplitFee string
}

// TaskFills 返回归属于某任务的成交份额（按 filled_at、id 升序）。
//
// accountID <= 0 / instID 为空时不过滤。包含交易所 algo 触发的平仓腿。
func (d *DB) TaskFills(ctx context.Context, taskID, accountID int64, instID string) ([]TaskFill, error) {
	if taskID <= 0 {
		return nil, nil
	}
	type pair struct {
		account int64
		inst    string
	}
	// 候选 (account, inst)：至少有一笔成交能连回本任务。
	//
	// 三条来源都要算，少一条就会让某类归属路径的成交整批消失：
	//   ① trade_records（工具下单，主路径）
	//   ② orders_execution.client_order_id → draft → signal（旧草案链路）
	//   ③ orders_execution.channel_order_id → 成交 ord_id（OKX 对部分成交不返回 clOrdId）
	q := `SELECT DISTINCT tf.account_id, tf.inst_id
FROM trade_fills tf
LEFT JOIN trade_records tr ON tr.client_order_id = tf.cl_ord_id
LEFT JOIN orders_execution oe ON (oe.client_order_id = tf.cl_ord_id OR oe.channel_order_id = tf.ord_id)
LEFT JOIN orders_draft od ON od.id = COALESCE(tf.draft_id, oe.draft_id)
LEFT JOIN signals s ON s.id = od.signal_id
WHERE tr.task_id = $1 OR (od.signal_id IS NOT NULL AND s.task_id = $1)`
	args := []any{taskID}
	if accountID > 0 {
		q += fmt.Sprintf(" AND tf.account_id = $%d", len(args)+1)
		args = append(args, accountID)
	}
	if s := strings.TrimSpace(instID); s != "" {
		q += fmt.Sprintf(" AND tf.inst_id = $%d", len(args)+1)
		args = append(args, s)
	}
	rows, err := d.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	var pairs []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.account, &p.inst); err != nil {
			rows.Close()
			return nil, err
		}
		pairs = append(pairs, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(pairs) == 0 {
		return nil, nil
	}

	out := make([]TaskFill, 0, 64)
	for _, p := range pairs {
		fills, err := d.fillsForPair(ctx, p.account, p.inst)
		if err != nil {
			return nil, err
		}
		owner, err := d.ownerKeys(ctx, clOrdIDsOf(fills), ordIDsOf(fills))
		if err != nil {
			return nil, err
		}
		out = append(out, attributeByOpenShares(fills, owner, taskID)...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].FilledAt != out[j].FilledAt {
			return out[i].FilledAt < out[j].FilledAt
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// fillsForPair 拉某账户某标的的全部成交（时间升序）。
func (d *DB) fillsForPair(ctx context.Context, accountID int64, instID string) ([]TradeFill, error) {
	rows, err := d.QueryContext(ctx, `SELECT id, fill_id, account_id, inst_id, side, pos_side,
fill_px, fill_sz, fee, COALESCE(fee_ccy,''), COALESCE(ord_id,''), COALESCE(cl_ord_id,''),
COALESCE(draft_id,0), filled_at
FROM trade_fills WHERE account_id=$1 AND inst_id=$2 ORDER BY filled_at ASC, id ASC`,
		accountID, instID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TradeFill
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

// ownerOf 取一笔成交的归属：先 clOrdId，再 ordId（OKX 对部分成交不返回 clOrdId）。
func ownerOf(f TradeFill, owner map[string]int64) int64 {
	if tid, hit := owner[strings.TrimSpace(f.ClOrdID)]; hit {
		return tid
	}
	if tid, hit := owner[strings.TrimSpace(f.OrdID)]; hit {
		return tid
	}
	return 0
}

func clOrdIDsOf(fills []TradeFill) []string {
	out := make([]string, 0, len(fills))
	for _, f := range fills {
		if s := strings.TrimSpace(f.ClOrdID); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// ordIDsOf 取成交的 ordId 列表（归属的第二顺位键）。
func ordIDsOf(fills []TradeFill) []string {
	out := make([]string, 0, len(fills))
	for _, f := range fills {
		if s := strings.TrimSpace(f.OrdID); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// ownerKeys 是「归属键 → 任务 id」的映射。键可以是 clOrdId，也可以是 ordId。
//
// 三条来源（缺一就会让某类成交整批失去归属）：
//
//	① trade_records.client_order_id / channel_order_id → task_id（工具下单，主路径）
//	② orders_execution.client_order_id → draft → signal.task_id（旧草案链路）
//	③ orders_execution.channel_order_id → draft → signal.task_id
//	   （OKX 对部分成交不返回 clOrdId，此时只能靠 ordId）
func (d *DB) ownerKeys(ctx context.Context, clOrdIDs, ordIDs []string) (map[string]int64, error) {
	out := map[string]int64{}
	put := func(m map[string]int64) {
		for cl, tid := range m {
			if tid > 0 {
				out[cl] = tid
			}
		}
	}
	m1, err := d.tradeRecordsTaskByKey(ctx, clOrdIDs)
	if err != nil {
		return nil, err
	}
	put(m1)
	m2, err := d.tradeRecordsTaskByKey(ctx, ordIDs)
	if err != nil {
		return nil, err
	}
	put(m2)
	m3, err := d.legacyChainTaskByKey(ctx, clOrdIDs)
	if err != nil {
		return nil, err
	}
	put(m3)
	m4, err := d.legacyChainTaskByKey(ctx, ordIDs)
	if err != nil {
		return nil, err
	}
	put(m4)
	return out, nil
}

// tradeRecordsTaskByKey 用 trade_records 的 client_order_id / channel_order_id 反查任务。
func (d *DB) tradeRecordsTaskByKey(ctx context.Context, keys []string) (map[string]int64, error) {
	out := map[string]int64{}
	for _, chunk := range chunkStrings(keys, 500) {
		ph := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, v := range chunk {
			ph[i] = fmt.Sprintf("$%d", i+1)
			args[i] = v
		}
		in := strings.Join(ph, ",")
		rows, err := d.QueryContext(ctx, `SELECT COALESCE(client_order_id,''), COALESCE(channel_order_id,''), COALESCE(task_id,0)
FROM trade_records WHERE client_order_id IN (`+in+`) OR channel_order_id IN (`+in+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var cl, ch string
			var tid int64
			if err := rows.Scan(&cl, &ch, &tid); err != nil {
				rows.Close()
				return nil, err
			}
			if tid <= 0 {
				continue
			}
			if cl = strings.TrimSpace(cl); cl != "" {
				out[cl] = tid
			}
			if ch = strings.TrimSpace(ch); ch != "" {
				out[ch] = tid
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// legacyChainTaskByKey 用 orders_execution → draft → signal 反查任务（旧草案链路）。
func (d *DB) legacyChainTaskByKey(ctx context.Context, keys []string) (map[string]int64, error) {
	out := map[string]int64{}
	for _, chunk := range chunkStrings(keys, 500) {
		ph := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, v := range chunk {
			ph[i] = fmt.Sprintf("$%d", i+1)
			args[i] = v
		}
		in := strings.Join(ph, ",")
		rows, err := d.QueryContext(ctx, `SELECT COALESCE(oe.client_order_id,''), COALESCE(oe.channel_order_id,''), s.task_id
FROM orders_execution oe
JOIN orders_draft od ON od.id = oe.draft_id
JOIN signals s ON s.id = od.signal_id
WHERE (oe.client_order_id IN (`+in+`) OR oe.channel_order_id IN (`+in+`)) AND s.task_id IS NOT NULL`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var cl, ch string
			var tid int64
			if err := rows.Scan(&cl, &ch, &tid); err != nil {
				rows.Close()
				return nil, err
			}
			if tid <= 0 {
				continue
			}
			if cl = strings.TrimSpace(cl); cl != "" {
				out[cl] = tid
			}
			if ch = strings.TrimSpace(ch); ch != "" {
				out[ch] = tid
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// chunkStrings 去重并切成最多 size 个一组的批次（SQLite 参数上限 999）。
func chunkStrings(in []string, size int) [][]string {
	clean := make([]string, 0, len(in))
	seen := map[string]struct{}{}
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		clean = append(clean, v)
	}
	var out [][]string
	for start := 0; start < len(clean); start += size {
		end := start + size
		if end > len(clean) {
			end = len(clean)
		}
		out = append(out, clean[start:end])
	}
	return out
}

// attributeByOpenShares 逐笔维护各任务净持仓，返回 taskID 占有的成交份额。
//
// fills 必须按 filled_at、id 升序（fillsForPair 保证）。
// owner 是 clOrdId / ordId → taskID（ownerKeys 的产物）。
//
// 有主成交整笔归该任务；无主成交（交易所 algo 平仓腿）按各任务当前 |净持仓|
// 比例拆分。拆分必须逐笔做：只有走到那一笔时各任务的持仓才是真实需要被平掉的量。
func attributeByOpenShares(fills []TradeFill, owner map[string]int64, taskID int64) []TaskFill {
	opens := map[int64]*big.Rat{}
	add := func(t int64, delta *big.Rat) {
		cur, ok := opens[t]
		if !ok {
			cur = new(big.Rat)
			opens[t] = cur
		}
		cur.Add(cur, delta)
	}
	var out []TaskFill

	for _, f := range fills {
		qty, ok := new(big.Rat).SetString(strings.TrimSpace(f.FillSz))
		if !ok || qty.Sign() <= 0 {
			continue
		}
		signed := new(big.Rat).Set(qty)
		if strings.EqualFold(strings.TrimSpace(f.Side), "sell") {
			signed.Neg(signed)
		}

		if t := ownerOf(f, owner); t > 0 {
			add(t, signed)
			if t == taskID {
				out = append(out, TaskFill{TradeFill: f, SplitSz: f.FillSz, SplitFee: f.Fee})
			}
			continue
		}

		// 无主成交：分母是此刻所有任务 |净持仓| 之和。
		total := new(big.Rat)
		ids := make([]int64, 0, len(opens))
		for t, o := range opens {
			if o.Sign() == 0 {
				continue
			}
			ids = append(ids, t)
			total.Add(total, new(big.Rat).Abs(o))
		}
		if total.Sign() == 0 {
			continue // 没有任何任务持仓：无从归属，不猜
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

		for _, t := range ids {
			o := opens[t]
			share := new(big.Rat).Mul(qty, new(big.Rat).Abs(o))
			share.Quo(share, total)
			// 该任务最多只能被平掉 |open_t| 张：algo 腿不可能关掉它没持有的量。
			// 只有这笔成交本身大于 Σ|open|（手工单等病态输入）时才会触发；
			// 此时多出来的 size-Σshares 张确实无从归属，丢弃而不改派。
			avail := new(big.Rat).Abs(o)
			if share.Cmp(avail) > 0 {
				share.Set(avail)
			}
			delta := new(big.Rat).Set(share)
			if signed.Sign() < 0 {
				delta.Neg(delta)
			}
			add(t, delta)
			if t != taskID {
				continue
			}
			splitSz, splitFee := f.FillSz, f.Fee
			if share.Cmp(qty) != 0 {
				splitSz, splitFee = ratLiteral(share), feeShare(f.Fee, share, qty)
			}
			out = append(out, TaskFill{TradeFill: f, SplitSz: splitSz, SplitFee: splitFee})
		}
	}
	return out
}

// feeShare 按份额摊手续费（符号与整笔一致：OKX 用负数表示支出）。
func feeShare(fee string, share, size *big.Rat) string {
	f, ok := new(big.Rat).SetString(strings.TrimSpace(fee))
	if !ok {
		return fee
	}
	r := new(big.Rat).Quo(share, size)
	r.Mul(r, f)
	return ratLiteral(r)
}
