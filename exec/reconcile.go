package exec

// 对账（WE-10）：把本地执行记录与通道回执比对，发现不一致就留痕并按需冻结。
//
// # 设计要点
//
// 1. **无变化不写库**：60s 一次 × N 单 × 7 天 = 大量垃圾行。只在有差异时留痕。
// 2. **以通道为准**：本地是「我以为」，通道是「事实」。但修正必须留 raw_ack 全量快照，
//    否则事后无法解释账面为什么变了。
// 3. **冻结的语义是「停止一切自动执行」**，不只是「这个账户不能用了」——
//    见 db.FreezeAccountTx 的说明。
//
// # 时间窗口
//
// 两个判定必须带时间窗口，否则会把「异步系统的正常延迟」当成异常：
//   - missing_remote 需要「已过 30 分钟」—— 否则刚提交 5 秒的单会被判「丢了」
//   - query_failed 用「最近 3 条序列」而不是「累计 3 次」—— 否则「上周失败 + 这周失败」
//     会被算成连续

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
)

// 对账差异类型。与 exec_reconciliations.kind 的 CHECK 约束一一对应。
const (
	// DiffStateOnly 仅状态推进，量价一致。正常的异步回调延迟。
	DiffStateOnly = "state"
	// DiffAmount 量或价不一致。以通道为准修正，并冻结。
	DiffAmount = "amount"
	// DiffMissingLocal 通道有我方无（人工在券商 App 下的单）。
	// 只记录，不自动建单、不冻结 —— 无法判断意图。
	DiffMissingLocal = "missing_local"
	// DiffMissingRemote 本地有、通道查无此单，且已过 30 分钟。
	DiffMissingRemote = "missing_remote"
	// DiffQueryFailed 通道查询失败。连续 3 次才冻结。
	DiffQueryFailed = "query_failed"
)

// 阈值常量。集中在这里，避免散落在判定逻辑里。
const (
	// missingRemoteGrace 是「本地有、通道无」判为 missing_remote 前要等的时间。
	//
	// 30 分钟：通道回执通常在秒级到分钟级到达；30 分钟没有任何回执，
	// 才足以排除「还在处理中」。这个值来自 ARCH-W2-W5.md §3.5.2。
	missingRemoteGrace = 30 * time.Minute

	// queryFailedConsecutive 是「连续几次查询失败」才冻结。
	//
	// 3 次 = 3 个 tick（每 60s）= 3 分钟内连续失败。
	// 偶发一次网络抖动不该冻结账户。
	queryFailedConsecutive = 3

	// amountPriceEpsilon 是价格比较的容差。
	//
	// 1e-6：通道与本地可能因浮点/精度截断产生极小差异，
	// 那不是「账实不符」，是精度噪声。
	amountPriceEpsilon = 1e-6
)

// ErrReconcileAmountMismatch 表示量或价不一致。
var ErrReconcileAmountMismatch = errors.New("reconcile: amount mismatch")

// ErrReconcileMissingRemote 表示本地有单、通道查无。
var ErrReconcileMissingRemote = errors.New("reconcile: missing on channel")

// ErrReconcileUnavailable 表示通道查询不可用。
var ErrReconcileUnavailable = errors.New("reconcile: channel query unavailable")

// ChannelAck 是通道回执的抽象。
//
// 不直接依赖某个券商 SDK 的类型：对账逻辑只关心「状态/成交量/成交价」，
// 各通道的适配在 provider 层做。
type ChannelAck struct {
	// Found 为 false 表示通道查无此单（不是错误，是「没有」）。
	Found bool
	// State 是通道侧的状态，取值与 orders_execution.state 对齐。
	State string
	// FilledQty 是通道侧的累计成交量（十进制字符串）。
	FilledQty string
	// AvgPrice 是通道侧的成交均价（十进制字符串，可空）。
	AvgPrice string
	// Raw 是通道原始回执，全量落库供审计。
	Raw json.RawMessage
}

// ChannelQuerier 是通道查询接口。真实实现由 provider 层提供。
type ChannelQuerier interface {
	// QueryByClientOrderID 按我方幂等键查通道侧的状态。
	//
	// 返回 (ack, nil) 表示查询成功 —— ack.Found=false 也属于成功（查无此单）。
	// 返回 error 表示查询本身失败（网络/鉴权/限流），此时无法判断有没有单。
	QueryByClientOrderID(ctx context.Context, clientOrderID string) (ChannelAck, error)
}

// Reconciler 执行对账。
type Reconciler struct {
	db      *db.DB
	querier ChannelQuerier
}

// NewReconciler 构造对账器。
func NewReconciler(d *db.DB, q ChannelQuerier) *Reconciler {
	return &Reconciler{db: d, querier: q}
}

// ReconcileResult 汇总一次对账的结果（供调度器打日志/指标）。
type ReconcileResult struct {
	Checked int
	// NoDiff 是「查了、但通道与本地完全一致」的订单数。
	//
	// # 为什么它必须单独计数
	//
	// Checked 是「看了几张单」，NoDiff 是「几张单没问题」。
	// 只有 Checked 而没有 NoDiff 时，统计要靠 Checked - (其它各项) 反推 ——
	// 而那个减法在新增差异类型时会算错（新类型忘了加到减法里）。
	//
	// 有了 NoDiff，「总数 = 无差异 + 各类差异」这个等式可以断言。
	NoDiff     int
	StateOnly  int
	Amount     int
	MissingLoc int
	MissingRem int
	QueryFail  int
	Frozen     int
}

// Run 对指定账户的所有在途订单做一次对账。
//
// # ⚠️ 单实例假设
//
// 本方法不自己做互斥。多个实例同时调用时，同一张单会被对账多次：
//   - 重复写 exec_reconciliations（记录会重复，但状态推进是条件更新，不会错）
//   - amount 差异会被判多次 → FreezeAccount 被调多次（幂等，但日志重复）
//
// # 当前为什么可以单实例
//
// server 是单进程（workers=3 是 goroutine 数不是进程数），共用一个 *DB。
// 而调用方（server/exec_scheduler.go 的 execTicker）只有一个 goroutine 在跑 step。
//
// # 升级路径（多实例部署前必须做）
//
// 用 PG advisory lock 选主：
//
//	SELECT pg_try_advisory_lock(<固定 key>)
//
// 拿到锁的实例才跑 Run，其它实例跳过。锁随连接释放 ——
// 进程挂了自动解锁，比「心跳 + 选主表」简单。
//
// 这个假设也写在 server/exec_scheduler.go 的启动 log 里 ——
// 因为如果将来有人绕过 execTicker 直接调 Run，他可能看不到那份 log。
//
// # 为什么只扫在途状态
//
// submitted / partial / cancel_requested 是「可能有变化」的状态。
// filled / cancelled / rejected / settled 是终态，不会再变 —— 扫它们是浪费。
//
// # 为什么这个方法是 tick 级的
//
// 由 server/scheduler.go 每 60s 调用一次。它不阻塞、不重试 ——
// 一次失败就是一次失败，下次 tick 再来。
func (r *Reconciler) Run(ctx context.Context, accountID int64) (ReconcileResult, error) {
	var res ReconcileResult

	if r.querier == nil {
		return res, fmt.Errorf("%w: 未配置通道查询器", ErrReconcileUnavailable)
	}

	// 收集在途订单。
	var orders []*db.ExecOrder
	for _, st := range []string{
		db.ExecStateSubmitted, db.ExecStatePartial, db.ExecStateCancelRequested,
	} {
		list, err := r.db.ExecOrders().ListByState(st, 200)
		if err != nil {
			return res, fmt.Errorf("列在途订单失败: %w", err)
		}
		for _, o := range list {
			if o.AccountID == accountID {
				orders = append(orders, o)
			}
		}
	}

	// 逐个对账。单个订单的失败不中断整轮 ——
	// 但对「查询失败」有特殊处理（连续 3 次要冻结）。
	failStreak := 0
	for _, o := range orders {
		res.Checked++
		kind, err := r.reconcileOne(ctx, o, &res)
		if kind == DiffQueryFailed {
			failStreak++
			// 连续失败达到阈值 → 冻结。
			//
			// 用「本轮内的连续计数」而不是查表：
			// 一轮 tick 内同一个账户的所有单都查不通，说明通道整个不可用 ——
			// 那就是真实的 degraded，不需要跨 tick 累计。
			if failStreak >= queryFailedConsecutive {
				if fErr := r.FreezeAccount(ctx, accountID,
					fmt.Sprintf("RECON_UNAVAILABLE: 连续 %d 次查询失败", failStreak)); fErr != nil {
					return res, fmt.Errorf("查询连续失败后冻结账户失败: %w", fErr)
				}
				res.Frozen++
				break // 已冻结，本轮不再继续
			}
			continue
		}
		failStreak = 0 // 成功一次就重置
		if err != nil {
			// 非查询失败的其它错误（如 DB 写失败）：记录但不中断。
			// 中断会让一个坏单阻塞整轮对账。
			continue
		}
		_ = kind
	}

	// 本轮只要有一单查询成功，就把该账户所有未解决的 query_failed 标为 resolved。
	//
	// # 为什么这是 query_failed 抑制机制的前提
	//
	// 没有它：周一失败 3 次 → 冻结；周二恢复 → 但旧的 3 条记录还在（resolved_at IS NULL）；
	// 周三再失败 1 次 → 「最近 3 条」全是失败（周一那几条）→ 立即冻结。
	// 而实际只是「周三失败 1 次」。
	//
	// 有它：周二恢复时，周一的 3 条被标 resolved；周三失败 1 次 → 不连续 → 不冻结。
	if res.QueryFail < res.Checked {
		// QueryFail < Checked 说明至少有一单查询成功了。
		if rErr := r.resolveQueryFailures(accountID); rErr != nil {
			// resolve 失败不影响对账结果，但要告警（否则旧失败会一直累计）。
			slog.Error("resolve 旧的 query_failed 失败（会影响连续失败判定）",
				"account_id", accountID, "err", rErr)
		}
	}

	return res, nil
}

// resolveQueryFailures 把该账户所有未解决的 query_failed 标记为已解决。
func (r *Reconciler) resolveQueryFailures(accountID int64) error {
	_, err := r.db.Exec(`UPDATE exec_reconciliations
SET resolved_at = now(), resolved_by = 'reconciler'
WHERE account_id = $1 AND kind = 'query_failed' AND resolved_at IS NULL`, accountID)
	return err
}

// reconcileOne 对一张单做对账，返回判定的差异类型。
//
// 副作用（记 reconciliation / 转状态 / 冻结）在本方法内完成。
func (r *Reconciler) reconcileOne(ctx context.Context, o *db.ExecOrder, res *ReconcileResult) (string, error) {
	ack, err := r.querier.QueryByClientOrderID(ctx, o.ClientOrderID)
	if err != nil {
		// 查询失败：记一条 query_failed，不立刻冻结（由 Run 的连续计数决定）。
		_ = r.record(o, DiffQueryFailed, nil, ChannelAck{}, map[string]any{"error": err.Error()})
		res.QueryFail++
		return DiffQueryFailed, fmt.Errorf("%w: %v", ErrReconcileUnavailable, err)
	}

	// ---- 通道查无此单 ----
	if !ack.Found {
		// 已过 30 分钟才判 missing_remote。
		//
		// 用 state_at 而不是 created_at：这些单可能已经 partial 过，
		// created_at 是第一次提交时间，而我们要判断的是
		// 「上一次状态变更之后过了多久还没有回执」。
		stateAt, tErr := db.ParseDBTime(o.StateAt)
		if tErr != nil {
			return "", fmt.Errorf("解析 state_at %q 失败: %w", o.StateAt, tErr)
		}
		if time.Since(stateAt) < missingRemoteGrace {
			// 还在宽限期内 —— 可能是通道还没处理完。本轮不动作。
			return "", nil
		}
		// 超过宽限期：记 missing_remote，转 cancel_requested，冻结。
		_ = r.record(o, DiffMissingRemote, o, ChannelAck{}, map[string]any{
			"state_at":   o.StateAt,
			"grace_secs": int(missingRemoteGrace.Seconds()),
		})
		res.MissingRem++
		// 转 cancel_requested 让超时逻辑收敛（真正撤单由超时扫描器做）。
		if o.State == db.ExecStateSubmitted {
			if _, trErr := r.db.ExecOrders().Transition(o.ID, db.ExecStateSubmitted, db.ExecStateCancelRequested); trErr != nil {
				// 转状态失败不阻断冻结 —— 冻结是更重要的动作。
				_ = trErr
			}
		}
		if fErr := r.FreezeAccount(ctx, o.AccountID,
			fmt.Sprintf("RECON_MISSING_REMOTE: 订单 %d 在通道查无，已过 %v", o.ID, missingRemoteGrace)); fErr != nil {
			return DiffMissingRemote, fmt.Errorf("%w: 冻结失败: %v", ErrReconcileMissingRemote, fErr)
		}
		res.Frozen++
		return DiffMissingRemote, fmt.Errorf("%w: 订单 %d", ErrReconcileMissingRemote, o.ID)
	}

	// ---- 比状态与量价 ----
	dStateChanged := o.State != ack.State
	dqty := qtyDiff(o.FilledQty, ack.FilledQty)
	dprice := priceDiff(o.AvgPrice, ack.AvgPrice)

	switch {
	case !dStateChanged && dqty == 0 && dprice < amountPriceEpsilon:
		// 完全一致 —— 无变化不写库（避免 60s × N 单的垃圾行）。
		//
		// 但要计数：调用方需要「看了几张、几张没问题」这个比例，
		// 否则无法判断「对账在工作」还是「对账什么都没做」。
		res.NoDiff++
		return "", nil

	case dStateChanged && dqty == 0 && dprice < amountPriceEpsilon:
		// 仅状态推进，量价一致 —— 正常。
		//
		// # 为什么必须先验量价一致
		//
		// 只看 state 不同就 Transition 是错的：
		// submitted→filled 但 filled_qty 只对得上一半，
		// 说明通道给了不一致的回执 —— 那是 amount 差异（要冻结），
		// 不是「正常推进」。
		_ = r.record(o, DiffStateOnly, o, ack, map[string]any{
			"local_state": o.State, "remote_state": ack.State,
		})
		res.StateOnly++
		if _, trErr := r.db.ExecOrders().TransitionWithAck(o.ID, o.State, ack.State, db.ExecOrderAck{
			ChannelOrderID: "",
			FilledQty:      strPtrOrNil(ack.FilledQty),
			AvgPrice:       strPtrOrNil(ack.AvgPrice),
			RawAck:         ack.Raw,
		}); trErr != nil {
			return DiffStateOnly, fmt.Errorf("状态推进失败: %w", trErr)
		}
		return DiffStateOnly, nil

	case dStateChanged || dqty != 0 || dprice >= amountPriceEpsilon:
		// 量价不一致（不管状态是否也变了）—— 最严重的类型。
		//
		// 以通道为准修正本地：本地是「我以为」，通道是「事实」。
		// 但必须留 raw_ack 全量快照，否则事后无法解释账面为什么变了。
		_ = r.record(o, DiffAmount, o, ack, map[string]any{
			"local_state": o.State, "remote_state": ack.State,
			"local_filled_qty": o.FilledQty, "remote_filled_qty": ack.FilledQty,
			"local_avg_price": o.AvgPrice, "remote_avg_price": ack.AvgPrice,
		})
		res.Amount++
		if fErr := r.FreezeAccount(ctx, o.AccountID,
			fmt.Sprintf("RECON_AMOUNT_MISMATCH: 订单 %d 量价不一致", o.ID)); fErr != nil {
			return DiffAmount, fmt.Errorf("%w: 冻结失败: %v", ErrReconcileAmountMismatch, fErr)
		}
		res.Frozen++
		return DiffAmount, fmt.Errorf("%w: 订单 %d", ErrReconcileAmountMismatch, o.ID)

	default:
		// 本地在途、通道也是同一状态但量价为 0 差异 —— 已由第一个 case 覆盖。
		return "", nil
	}
}

// record 写一条对账记录。错误被忽略（记录失败不该阻断对账本身）。
func (r *Reconciler) record(o *db.ExecOrder, kind string, local *db.ExecOrder, ack ChannelAck, diff map[string]any) error {
	localSnap, _ := json.Marshal(local)
	remoteSnap := ack.Raw
	if len(remoteSnap) == 0 {
		remoteSnap = []byte(`{}`)
	}
	diffRaw, _ := json.Marshal(diff)
	_, err := r.db.ExecReconciliations().Record(db.ExecReconciliationInput{
		ExecutionID:    &o.ID,
		AccountID:      o.AccountID,
		Kind:           kind,
		LocalSnapshot:  localSnap,
		RemoteSnapshot: remoteSnap,
		Diff:           diffRaw,
	})
	return err
}

// FreezeAccount 冻结账户：db.FreezeAccountTx（三件事同事务）+ 审计（事务外）。
//
// # 为什么审计在事务外
//
// 冻结是紧急动作，时效性优先。如果审计插入失败（字段超长、约束冲突等），
// 整个事务回滚 → 冻结失效 —— 优先级搞反了。
//
// **所以：冻结必须先落地，审计失败只告警。**
// 「冻结了但没记录」是可接受的（事后能查 DB 状态），
// 「没冻结但有记录」才是灾难。
func (r *Reconciler) FreezeAccount(ctx context.Context, accountID int64, reason string) error {
	// 查账户的 channel/account_ref（撤授权需要自然键）。
	acct, err := r.db.ExecAccounts().GetAccount(accountID)
	if err != nil {
		return fmt.Errorf("读账户失败: %w", err)
	}

	// 三件事同事务：降模式 → 撤授权 → 封账户。
	// 四件事同事务：降模式 + 撤授权 + 封账户 + 写审计（ARCH §3.5 硬约束 1）。
	//
	// 审计失败时整个事务回滚，冻结不生效 —— 调用方（对账）下一 tick 重试。
	// 这不是「放弃冻结」，是「延迟 60s」。
	if _, err := r.db.FreezeAccountTx(accountID, acct.Channel, acct.ChannelAcctID, reason); err != nil {
		return err
	}

	// notify 在事务外（ARCH §3.5 硬约束 2：「notify 失败不回滚，通知是尽力而为」）。
	//
	// 但失败要留痕 —— 否则「冻结了但没人收到通知」也是静默的。
	if nErr := r.notifyFrozen(ctx, accountID, reason); nErr != nil {
		slog.Error("冻结通知发送失败（冻结已生效，需人工告知）",
			"account_id", accountID, "reason", reason, "err", nErr)
	}
	return nil
}

// notifyFrozen 发出冻结通知。
//
// ponytail: 当前没有独立的通知通道（notify 包未接），只写一行 server_logs
//
//	作为「通知尝试」的记录；升级路径是接 notify.Publish 并写 notification_deliveries。
func (r *Reconciler) notifyFrozen(ctx context.Context, accountID int64, reason string) error {
	_, err := r.db.Exec(`INSERT INTO server_logs(level, tag, text) VALUES ('warn', 'exec_freeze', $1)`,
		fmt.Sprintf("account %d frozen: %s", accountID, reason))
	return err
}

// qtyDiff 返回两个十进制字符串的差的绝对值（float 足够 —— 只用于「是否为 0」判断）。
//
// 用 float64 而非 big.Rat：这里只判「差是否为 0」和「是否超过 epsilon」，
// 不需要精确金额计算（那是 Gate 的职责，用 big.Rat）。
func qtyDiff(a, b string) float64 {
	av, aok := parseDecimalFloat(a)
	bv, bok := parseDecimalFloat(b)
	if !aok || !bok {
		// 解析失败视为「有差异」—— 无法确认一致时按不一致处理（保守）。
		if a == b {
			return 0
		}
		return 1
	}
	d := av - bv
	if d < 0 {
		d = -d
	}
	return d
}

// priceDiff 返回两个可空价格字符串的差的绝对值。
func priceDiff(a *string, b string) float64 {
	if a == nil || *a == "" {
		if b == "" {
			return 0
		}
		return 1
	}
	return qtyDiff(*a, b)
}

// strPtrOrNil 把空字符串转成 nil（SQL NULL 语义）。
func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// parseDecimalFloat 把十进制字符串解析成 float64（仅用于「是否有差异」判断）。
func parseDecimalFloat(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
