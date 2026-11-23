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
// # 归属规则：逐任务逐方向净持仓 + 按比例拆份额
//
// 按 (account, inst) 时间升序走一遍，维护每个**仓位桶**的净持仓。
// 桶的键是 (task, pos_side)：long_short 模式下同一任务在同一标的上可以同时持
// 多、空两条独立仓，按任务单一净额分账会让 algo 腿把同向仓越平分越大
// （实测账户 4 是 long_short：291 笔 long + 855 笔 short）。exec/pnl.go 的
// groupSideOf 已按 pos_side 分账，这里必须同口径。
//
//   - 有主成交（clOrdId / ordId 能连回台账）→ 整笔归该任务的同向桶。
//   - 无主成交（交易所 algo 平仓腿）→ 只分给「这笔成交能平掉」的桶
//     （符号相反的桶；同向桶收到份额只会把仓位放大），
//     份额 = size · |open_b| / Σ|open_b|，符号跟随这笔成交。
//     没有可被平掉的持仓时不归属（不猜）。
//
// 份额夹在 |open_b| 以内：algo 腿不可能平掉某个桶没持有的量。夹仓时
// 多出来的 size-Σshares 张确实无从归属，**丢弃而不改派**。
//
// # 精确性
//
// 归属内部全程 big.Rat，无浮点漂移。但 SplitSz 是字符串出口（ratLiteral 只留
// 10 位小数），三个 1/3 相加会得到 0.9999999999 而不是 1，下游重新累加后净持仓
// 判为非零 → 人工验收报「持仓未平」—— 正是本函数要修的病。所以出口用
// **最大余额法**：前 n-1 个份额各自取 10 位字面量，最后一个取
// qty - Σ(已分配)，保证 ΣSplitSz 精确等于 FillSz。
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

// posSideOf 归一化 pos_side（与 exec/pnl.go 的 groupSideOf 同口径）。
// 桶键必须和汇总层一致，否则归属按一个方向切、汇总按另一个方向加。
func posSideOf(posSide string) string {
	switch strings.ToLower(strings.TrimSpace(posSide)) {
	case "long":
		return "long"
	case "short":
		return "short"
	}
	return ""
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

// bucket 是一个仓位桶：同一任务在 long_short 模式下可以同时持多、空两条
// 独立仓（exec/pnl.go 的 groupSideOf 已按 pos_side 分账，这里必须同口径）。
type bucket struct {
	task    int64
	posSide string
}

// attributeByOpenShares 逐笔维护各仓位桶净持仓，返回 taskID 占有的成交份额。
//
// fills 必须按 filled_at、id 升序（fillsForPair 保证）。
// owner 是 clOrdId / ordId → taskID（ownerKeys 的产物）。
//
// 有主成交整笔归该任务同向桶；无主成交（交易所 algo 平仓腿）按各桶当前
// |净持仓| 比例拆分，且只分给这笔成交能平掉的桶。拆分必须逐笔做：只有走到
// 那一笔时各任务真实的持仓才是需要对冲的量。
func attributeByOpenShares(fills []TradeFill, owner map[string]int64, taskID int64) []TaskFill {
	opens := map[bucket]*big.Rat{}
	add := func(b bucket, delta *big.Rat) {
		cur, ok := opens[b]
		if !ok {
			cur = new(big.Rat)
			opens[b] = cur
		}
		cur.Add(cur, delta)
	}
	var (
		out []TaskFill
		// splitSeq 轮转「吸收舍入残差」的桶。固定让同一个桶吸收会让它每笔多
		// 1e-10，几笔下来累积成幻影持仓；轮转后每桶的累计误差互相抵消。
		splitSeq int
	)

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
			add(bucket{task: t, posSide: posSideOf(f.PosSide)}, signed)
			if t == taskID {
				out = append(out, TaskFill{TradeFill: f, SplitSz: f.FillSz, SplitFee: f.Fee})
			}
			continue
		}

		// 无主成交：只分给「这笔成交能平掉」的桶 —— 同向持仓不会被平掉，
		// 收到份额只会把仓位越平分越大（long 2 + short 1，卖出 1 张时
		// 多头本该减半而不是变 1.33）。
		recipients := make([]bucket, 0, len(opens))
		total := new(big.Rat)
		for b, o := range opens {
			if o.Sign() == 0 || o.Sign() == signed.Sign() {
				continue
			}
			recipients = append(recipients, b)
			total.Add(total, new(big.Rat).Abs(o))
		}
		if total.Sign() == 0 {
			continue // 没有可被平掉的持仓：无从归属，不猜
		}
		sort.Slice(recipients, func(i, j int) bool {
			if recipients[i].task != recipients[j].task {
				return recipients[i].task < recipients[j].task
			}
			return recipients[i].posSide < recipients[j].posSide
		})

		// 精确份额（同时夹在 |open_b| 以内：algo 腿不可能平掉它没持有的量）。
		shares := make([]*big.Rat, len(recipients))
		sumExact := new(big.Rat)
		for i, b := range recipients {
			o := opens[b]
			share := new(big.Rat).Mul(qty, new(big.Rat).Abs(o))
			share.Quo(share, total)
			if avail := new(big.Rat).Abs(o); share.Cmp(avail) > 0 {
				share.Set(avail)
			}
			shares[i] = share
			sumExact.Add(sumExact, share)
		}

		// 最大余额法：前 n-1 个取 10 位字面量，吸收者拿 sumExact - Σ(已分配)，
		// 保证 ΣSplitSz 精确等于 FillSz（无夹仓时）。夹仓多出来的
		// qty - Σshares 张确实无从归属，丢弃而不改派。
		absorb := splitSeq % len(recipients)
		splitSeq++
		render := make([]*big.Rat, len(recipients))
		acc := new(big.Rat)
		for i := range recipients {
			if i == absorb {
				continue
			}
			if r, ok := new(big.Rat).SetString(ratLiteral(shares[i])); ok {
				render[i] = r
				acc.Add(acc, r)
			} else {
				render[i] = new(big.Rat)
			}
		}
		render[absorb] = new(big.Rat).Sub(sumExact, acc)
		if render[absorb].Sign() < 0 {
			render[absorb].SetInt64(0) // 舍入不可能到这，防御
		}

		for i, b := range recipients {
			delta := new(big.Rat).Set(shares[i]) // 内部仍用精确值累加
			if signed.Sign() < 0 {
				delta.Neg(delta)
			}
			add(b, delta)

			if b.task != taskID {
				continue
			}
			tf := f
			// 拆分后这一行属于被平掉的那个桶；成交自身 pos_side 为空
			// （部分交易所字段缺失）时退回桶的方向。
			if posSideOf(tf.PosSide) != b.posSide {
				tf.PosSide = b.posSide
			}
			out = append(out, TaskFill{
				TradeFill: tf,
				SplitSz:   ratLiteral(render[i]),
				SplitFee:  feeShare(f.Fee, render[i], qty),
			})
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
