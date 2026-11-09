package server

// 执行层调度的验收测试（WE-11）。
//
// # 测什么、不测什么
//
// 不测「ticker 是否每 60s 触发」—— 那需要 sleep，会让测试变慢且不稳。
// 测 `step()` 的行为：它是 tick 的实际内容，且可以被直接调用。
//
// 这符合「测可确定的逻辑，不测时间」：ticker 的语义由 Go 标准库保证，
// 我要验的是「到点之后做什么」。

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/exec"
)

// execTickerEnv 准备一个只含超时扫描的 ticker（querier 为 nil，模拟渠道未接入）。
type execTickerEnv struct {
	*testing.T
	pg     *db.DB
	ticker *execTicker
	acct   *db.ExecAccount
}

func newExecTickerEnv(t *testing.T, tag string) *execTickerEnv {
	t.Helper()
	// 用 NewManager 拿 pg 连接 —— 与 setupToolResolve 一致，
	// 不构造完整 Server（那会拉起 engine/scheduler 等无关组件）。
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { m.Close() })
	pg := m.pg

	// 建账户
	uniq := tag + "-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	acct, aErr := pg.ExecAccounts().UpsertAccount(db.ExecAccountInput{
		Channel:       "okx",
		ChannelAcctID: uniq,
		Label:         "exec ticker test",
		Market:        "crypto",
		MarketSupport: []string{"crypto"},
		Active:        true,
	})
	if aErr != nil {
		t.Fatalf("建账户: %v", aErr)
	}

	t.Cleanup(func() {
		// ⚠️ orders_draft 没有 account 列 —— 所以「按 account_id 删草案」永远匹配不到。
		// 草案的 cleanup 必须在建它的地方按 draft_id 注册（见 mkStaleOrder 的 cleanup）。
		//
		// 这里只删真正有 account_id 的两张表（加上它们的下游 exec_reconciliations）。
		//
		// # 顺序：先删叶子后根
		//
		// 外键链是 exec_reconciliations → orders_execution，
		// 且 orders_execution 对 orders_draft 是 ON DELETE RESTRICT ——
		// 顺序错会让 RESTRICT 挡住，整条链静默失效。
		//
		// ⚠️ 实测（2026-10-29）：原先只删 orders_execution + exec_accounts，
		// 而 mkStaleOrder 建的 orders_draft 从未被删 —— 每次跑留 1 条残留。
		// 加上 TestMain 的净增量检查后才发现。
		for _, q := range []string{
			`DELETE FROM exec_reconciliations WHERE account_id=$1`,
			`DELETE FROM orders_execution WHERE account_id=$1`,
			`DELETE FROM exec_accounts WHERE id=$1`,
		} {
			if _, err := pg.Exec(q, acct.ID); err != nil {
				t.Errorf("清理账户 %d 失败: %v | SQL: %s", acct.ID, err, q)
			}
		}
	})

	// querier / auto 均为 nil：只跑超时扫描（这是「渠道未接入」的真实状态）。
	// 自动交易器传 nil 意味着这些测试不会触发下单 —— 那正是想要的：
	// 调度器的对账/超时逻辑应与自动交易解耦。
	tk := newExecTicker(pg, nil, nil)
	return &execTickerEnv{T: t, pg: pg, ticker: tk, acct: acct}
}

// mkStaleOrder 建一张已超时的 submitted 单。
func (e *execTickerEnv) mkStaleOrder(age time.Duration) *db.ExecOrder {
	e.Helper()
	instID, _, iErr := e.pg.Instruments().UpsertInstrument(db.InstrumentInput{
		Market: "crypto", Symbol: "BTC-USDT", AssetClass: "crypto",
	})
	if iErr != nil {
		e.Fatalf("建标的: %v", iErr)
	}
	draftID, err := e.pg.OrderDrafts().InsertDraft(db.OrderDraftInput{
		InstrumentID: &instID, Side: "buy", Qty: "10", RefPrice: "100",
	})
	if err != nil {
		e.Fatalf("建草案: %v", err)
	}
	// draft 自己注册清理 —— 它不挂在 acct.ID 上（draft 无 account 列），
	// 所以 newExecTickerEnv 那个按 account_id 的 cleanup 删不到它。
	e.T.Cleanup(func() {
		_, _ = e.pg.Exec(`DELETE FROM orders_execution WHERE draft_id=$1`, draftID)
		_, _ = e.pg.Exec(`DELETE FROM orders_draft WHERE id=$1`, draftID)
	})
	clientID := "sched-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	orderID, err := e.pg.ExecOrders().InsertOrder(db.ExecOrderInput{
		DraftID:       draftID,
		AccountID:     e.acct.ID,
		Mode:          string(db.ExecModePaper),
		State:         db.ExecStateSubmitted,
		ClientOrderID: clientID,
		Qty:           "10",
	})
	if err != nil {
		e.Fatalf("建执行单: %v", err)
	}
	if _, err := e.pg.Exec(`UPDATE orders_execution SET state_at=$2 WHERE id=$1`,
		orderID, time.Now().Add(-age)); err != nil {
		e.Fatalf("改 state_at: %v", err)
	}
	o, err := e.pg.ExecOrders().GetOrder(orderID)
	if err != nil {
		e.Fatalf("读执行单: %v", err)
	}
	return o
}

// TestExecTickerStepRunsTimeoutScan step() 应该跑超时扫描（querier 为 nil 时也对）。
func TestExecTickerStepRunsTimeoutScan(t *testing.T) {
	e := newExecTickerEnv(t, "tick-timeout")
	o := e.mkStaleOrder(31 * time.Minute)

	// 直接调 step，不启动 ticker（避免 sleep）
	e.ticker.step(context.Background())

	got, err := e.pg.ExecOrders().GetOrder(o.ID)
	if err != nil {
		t.Fatalf("读执行单: %v", err)
	}
	if got.State != db.ExecStateCancelRequested {
		t.Errorf("已超时的单应转 cancel_requested，实际 %s", got.State)
	}
}

// TestExecTickerNilQuerierSkipsReconcile 没有 querier 时，对账不跑（不 panic）。
func TestExecTickerNilQuerierSkipsReconcile(t *testing.T) {
	e := newExecTickerEnv(t, "tick-noquerier")

	if e.ticker.reconciler != nil {
		t.Fatal("querier 为 nil 时 reconciler 应为 nil")
	}

	// step 不该因为 reconciler 为 nil 而 panic
	e.ticker.step(context.Background())
}

// TestExecTickerHasQuerierEnablesReconcile 传了 querier 时，reconciler 应被构造。
func TestExecTickerHasQuerierEnablesReconcile(t *testing.T) {
	pg := execTestDBForTicker(t)
	tk := newExecTicker(pg, &fakeQuerierForTicker{}, nil)
	if tk.reconciler == nil {
		t.Fatal("传了 querier 时应构造 reconciler")
	}
}

// TestExecTickerIntervalIs60s 间隔应是从「通道速率限制」推导出的 60s。
//
// 为什么断言这个常量：如果有人把它改成 5s（与触发器 tick 对齐），
// 通道会被打爆。这个测试固定「两条调度线节奏不同」这个设计决定。
func TestExecTickerIntervalIs60s(t *testing.T) {
	pg := execTestDBForTicker(t)
	tk := newExecTicker(pg, nil, nil)
	if tk.interval != 60*time.Second {
		t.Errorf("执行层间隔应为 60s（对账要查通道，有速率限制），实际 %v", tk.interval)
	}
}

// TestExecTickerRunStopsOnContextCancel ctx 取消后 Run 应返回。
func TestExecTickerRunStopsOnContextCancel(t *testing.T) {
	e := newExecTickerEnv(t, "tick-ctx")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		e.ticker.Run(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
		// 正常返回
	case <-time.After(3 * time.Second):
		t.Fatal("ctx 取消后 Run 应在 3s 内返回 —— 否则 goroutine 泄漏")
	}
}

// fakeQuerierForTicker 是给「构造 reconciler」用的空实现。
type fakeQuerierForTicker struct{}

func (f *fakeQuerierForTicker) QueryByClientOrderID(_ context.Context, _ string) (exec.ChannelAck, error) {
	return exec.ChannelAck{Found: false}, nil
}

// execTestDBForTicker 返回一个可用的 *db.DB（不带账户等测试数据）。
func execTestDBForTicker(t *testing.T) *db.DB {
	t.Helper()
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { m.Close() })
	return m.pg
}
