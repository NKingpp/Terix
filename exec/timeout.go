package exec

// 超时扫描（WE-11）：把长时间没有回执的在途订单转成 cancel_requested。
//
// # 与对账的分工
//
//	对账（exec/reconcile.go）：**查通道**，以事实为准推进状态
//	超时扫描（本文件）：**不查通道**，只按本地时间判断「等太久了」
//
// 两者都会改 state，可能撞车。优先级是**对账优先**：
// 「通道说已成交」是事实，「超时」只是「我们等不及了」。
//
// # 怎么保证对账优先
//
// 超时的 UPDATE 带状态条件（`WHERE id=$1 AND state=$2`）：
// 如果对账已经把它改成 filled，这里改不动（RowsAffected=0），静默跳过。
// 这是条件更新天然提供的优先级，不需要额外的锁或顺序控制。
//
// # 单实例假设
//
// 与 Reconciler 相同：多个实例同时扫会重复转状态（条件更新让重复无害，
// 但日志会重复）。多实例部署时需加 pg advisory lock 选主。

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Autumn-27/artex/db"
)

// defaultTimeoutThreshold 是「多久没回执算超时」。
//
// 30 分钟：与 reconcile 的 missingRemoteGrace 一致 ——
// 同一个「等太久了」的门槛，不该在两条路径上给不同的值。
const defaultTimeoutThreshold = 30 * time.Minute

// TimeoutScanner 把超时的在途订单转成 cancel_requested。
type TimeoutScanner struct {
	db        *db.DB
	threshold time.Duration
	// now 可注入（测试用固定时钟）。
	now func() time.Time
}

// NewTimeoutScanner 构造超时扫描器。
//
// threshold <= 0 时用 defaultTimeoutThreshold。
func NewTimeoutScanner(d *db.DB, threshold time.Duration) *TimeoutScanner {
	if threshold <= 0 {
		threshold = defaultTimeoutThreshold
	}
	return &TimeoutScanner{db: d, threshold: threshold, now: time.Now}
}

// TimeoutResult 汇总一次扫描的结果。
type TimeoutResult struct {
	// Scanned 是扫过的在途订单数（两个状态之和）。
	Scanned int
	// TimedOut 是实际转成 cancel_requested 的单数。
	TimedOut int
	// Skipped 是「已超过阈值，但状态已被别的路径改掉」的单数。
	//
	// 这个计数存在的意义：它区分「没超时」和「超时了但抢不过对账」。
	// 只有 TimedOut 时，运维无法判断「扫描在工作吗」还是「全被对账抢先了」。
	Skipped int
	// ByState 是按原始状态分的计数（submitted / partial），
	// 便于发现「全是 partial 超时」这类异常。
	ByState map[string]int
}

// Run 扫一次超时订单。
//
// # 为什么扫两个状态
//
// `submitted` 和 `partial` 都可能是「卡住了」：
//   - submitted：提交后完全没有回执
//   - partial：部分成交后不再推进
//
// 文档 §3.4.1 明确 `partial → cancel_requested` 是合法转换
// （撤掉剩余部分，已成交部分保留）。
//
// # 为什么用条件 UPDATE 而不是「先查后改」
//
// 先 SELECT 拿到一批，再逐个 UPDATE —— 这中间对账可能已经改了状态。
// 条件 UPDATE（`WHERE id=$1 AND state=$2`）让「状态已变」的那次更新
// 自然 RowsAffected=0，不需要额外的事务或锁。
func (t *TimeoutScanner) Run(ctx context.Context) (TimeoutResult, error) {
	res := TimeoutResult{ByState: map[string]int{}}

	cutoff := t.now().Add(-t.threshold)

	// 两个状态都要扫。顺序无关 —— 每个订单只可能在一个状态上。
	for _, st := range []string{db.ExecStateSubmitted, db.ExecStatePartial} {
		// 查该状态下 state_at 早于 cutoff 的单。
		//
		// 用 state_at 而不是 created_at：这些单可能已经 partial 过，
		// created_at 是第一次提交时间，而我们要判断的是
		// 「上一次状态变更之后过了多久还没有新进展」。
		orders, err := t.listStale(st, cutoff)
		if err != nil {
			return res, fmt.Errorf("列 %s 超时订单失败: %w", st, err)
		}

		for _, o := range orders {
			res.Scanned++
			ok, trErr := t.db.ExecOrders().Transition(o.ID, st, db.ExecStateCancelRequested)
			if trErr != nil {
				// 转换不合法（不该发生 —— CanExecTransit 允许 partial→cancel_requested）
				// 或 DB 错误。记录但不中断整轮。
				slog.Error("超时转 cancel_requested 失败",
					"order_id", o.ID, "from", st, "err", trErr)
				continue
			}
			if !ok {
				// RowsAffected=0：状态已被别的路径（对账）改掉。
				// 这不是错误 —— 是对账优先的正常表现。
				res.Skipped++
				slog.Debug("超时扫描跳过：状态已被推进",
					"order_id", o.ID, "expected_state", st)
				continue
			}
			res.TimedOut++
			res.ByState[st]++

			// 明确记录：state 改了，但 filled_qty / avg_price 未动。
			//
			// partial 的单已有部分成交，那部分是「已实现」的 ——
			// 清掉它等于抹掉一笔真实成交记录。
			slog.Info("订单超时转 cancel_requested",
				"order_id", o.ID, "from", st,
				"filled_qty", o.FilledQty, // 记录已成交量，证明它被保留
				"threshold", t.threshold.String())
		}
	}

	return res, nil
}

// listStale 列某状态下 state_at 早于 cutoff 的订单。
//
// threshold 判定放在 SQL 里（`state_at < $2`）而不是 Go 里：
// 让数据库用索引（idx_exec_state 含 state_at），避免全表拉到内存再过滤。
func (t *TimeoutScanner) listStale(state string, cutoff time.Time) ([]*db.ExecOrder, error) {
	// 复用 db 层的查询能力：ListByState 返回该状态的前 N 条（按 state_at 升序），
	// 这里再按 cutoff 过滤。
	//
	// ponytail: 当前用 ListByState + Go 侧过滤，因为 db 层没有
	// 「按 state + state_at 范围」的方法。数据量小（在途单最多几百）时够用。
	// 升级路径：给 db 层加 ListStaleOrders(state, before, limit)，
	// 把过滤下推到 SQL，然后删掉这里的 Go 侧过滤。
	all, err := t.db.ExecOrders().ListByState(state, 500)
	if err != nil {
		return nil, err
	}
	out := make([]*db.ExecOrder, 0, len(all))
	for _, o := range all {
		stateAt, pErr := db.ParseDBTime(o.StateAt)
		if pErr != nil {
			// 解析失败时跳过并告警 —— 不中断整轮（否则一张坏单阻塞所有超时处理）。
			//
			// 这里的错误信息带原始值（%q 由 ParseDBTime 提供），
			// 使「格式不对」可区分于「不需要处理」。
			slog.Error("超时扫描：无法解析 state_at",
				"order_id", o.ID, "raw", o.StateAt, "err", pErr)
			continue
		}
		if stateAt.Before(cutoff) {
			out = append(out, o)
		}
	}
	return out, nil
}
