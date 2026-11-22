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
// # 归属规则：持仓回合（episode）
//
// 同一 (account, inst) 的成交按时间顺序走一遍净额：净额从 0 离开 → 一个回合开始，
// 净额回到 0 → 回合结束。**回合的归属取回合内第一笔能连回 trade_records 的成交**，
// 整个回合（含无主平仓腿）都归那个任务。
//
// 为什么这个规则安全：一个回合就是一次开平往返，它不可能横跨两个任务 ——
// 要横跨得让两个任务在同一账户同一标的上同时持仓，而 long_short 模式下
// pos_side 会把它们分成两条仓，净额回合天然分开。
//
// # 为什么不用「按 instId 取最近一条草案」
//
// BackfillFillDraftID 用的时间窗启发式在同一标的反复开平时会把历史成交
// 算到最新一次上（实测 open_qty 从 0.1 变 -0.2，比不归属更糟）。
// 回合边界由成交数据本身定义，不依赖任何时间猜测。

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"strings"
)

// TaskFills 返回归属于某任务的全部成交（按 filled_at、id 升序）。
//
// accountID <= 0 / instID 为空时不过滤。包含交易所 algo 触发的平仓腿。
func (d *DB) TaskFills(ctx context.Context, taskID, accountID int64, instID string) ([]TradeFill, error) {
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

	out := make([]TradeFill, 0, 64)
	for _, p := range pairs {
		fills, err := d.fillsForPair(ctx, p.account, p.inst)
		if err != nil {
			return nil, err
		}
		owner, err := d.ownerKeys(ctx, clOrdIDsOf(fills), ordIDsOf(fills))
		if err != nil {
			return nil, err
		}
		out = append(out, attributeEpisodes(fills, owner, taskID)...)
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

// attributeEpisodes 按「净额从 0 离开再回到 0」切回合，返回归属 taskID 的成交。
//
// fills 必须按 filled_at、id 升序（fillsForPair 保证）。
// owner 是 clOrdId → taskID（clOrdIDToTask 的产物）。
func attributeEpisodes(fills []TradeFill, owner map[string]int64, taskID int64) []TradeFill {
	if len(fills) == 0 {
		return nil
	}
	var (
		out    []TradeFill
		cur    []TradeFill
		curOwn int64
		net    = new(big.Rat)
		carry  *big.Rat // 反手后残留的净额（新回合的起点）
	)
	flush := func() {
		if curOwn == taskID && len(cur) > 0 {
			out = append(out, cur...)
		}
		cur = nil
		curOwn = 0
	}
	start := func() {
		cur = nil
		curOwn = 0
	}

	for _, f := range fills {
		qty, ok := new(big.Rat).SetString(strings.TrimSpace(f.FillSz))
		if !ok || qty.Sign() <= 0 {
			continue
		}
		signed := new(big.Rat).Set(qty)
		if strings.EqualFold(strings.TrimSpace(f.Side), "sell") {
			signed.Neg(signed)
		}
		if carry != nil {
			net.Set(carry)
			carry = nil
		}
		before := new(big.Rat).Set(net)
		net.Add(net, signed)

		cur = append(cur, f)
		if curOwn == 0 {
			curOwn = ownerOf(f, owner)
		}

		switch {
		case net.Sign() == 0:
			flush()
			start()
		case before.Sign() != 0 && before.Sign() != net.Sign():
			// 反手：这一笔先平掉整个旧仓、余量开反向新仓。
			// ponytail: 只切回合、不拆行 —— 反手成交整体记在被打平的回合，
			// 反向新回合的余量没有独立成交行。要逐分严谨时改为按余量拆两行副本。
			flush()
			start()
			carry = new(big.Rat).Set(net)
			curOwn = ownerOf(f, owner)
		}
	}
	flush() // 尾部仍未平的回合
	return out
}
