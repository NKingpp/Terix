package exec

// 超时扫描的验收测试（WE-11）。
//
// 核心不是「转了状态」，而是三件事：
//   ① partial 超时 → cancel_requested，且 **filled_qty 保留**（已成交的部分不能丢）
//   ② 未到阈值不动
//   ③ 状态已被对账改掉时，超时扫描改不动（条件 UPDATE 的优先级保证）

import (
	"strconv"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// timeoutEnv 准备一个账户 + 可注入时钟的扫描器。
type timeoutEnv struct {
	*testing.T
	g    *Gate
	s    *TimeoutScanner
	acct *db.ExecAccount
	now  time.Time
}

func newTimeoutEnv(t *testing.T, tag string) *timeoutEnv {
	t.Helper()
	g, done := gateTestEnv(t)
	t.Cleanup(done)
	resetToPaper(t, g)

	acct := newGateTestAccount(t, g, tag)
	// 固定时钟到 UTC 正午，避免跨越日界时判定漂移。
	fixed := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	s := NewTimeoutScanner(g.db, 30*time.Minute)
	s.now = func() time.Time { return fixed }

	t.Cleanup(func() {
		_, _ = g.db.Exec(`DELETE FROM orders_execution WHERE account_id=$1`, acct.ID)
	})

	return &timeoutEnv{T: t, g: g, s: s, acct: acct, now: fixed}
}

// mkOrder 建一张指定状态与 state_at 的执行单。
//
// filledQty 可指定（测 partial 时用）。
func (e *timeoutEnv) mkOrder(state string, stateAt time.Time, filledQty string) *db.ExecOrder {
	e.Helper()
	inst := newGateTestInstrument(e.T, e.g, "crypto", "BTC-USDT", "crypto")
	draftID, err := e.g.db.OrderDrafts().InsertDraft(db.OrderDraftInput{
		InstrumentID: &inst.ID,
		Side:         "buy",
		Qty:          "10",
		RefPrice:     "100",
	})
	if err != nil {
		e.Fatalf("建草案: %v", err)
	}
	trackGateTestDraft(e.T, e.g, draftID)
	clientID := "timeout-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	orderID, err := e.g.db.ExecOrders().InsertOrder(db.ExecOrderInput{
		DraftID:       draftID,
		AccountID:     e.acct.ID,
		Mode:          string(db.ExecModePaper),
		State:         state,
		ClientOrderID: clientID,
		Qty:           "10",
	})
	if err != nil {
		e.Fatalf("建执行单: %v", err)
	}
	// 改 state_at 与 filled_qty
	if filledQty != "" {
		if _, err := e.g.db.Exec(`UPDATE orders_execution SET state_at=$2, filled_qty=$3, avg_price=100 WHERE id=$1`,
			orderID, stateAt, filledQty); err != nil {
			e.Fatalf("改 state_at/filled_qty: %v", err)
		}
	} else {
		if _, err := e.g.db.Exec(`UPDATE orders_execution SET state_at=$2 WHERE id=$1`, orderID, stateAt); err != nil {
			e.Fatalf("改 state_at: %v", err)
		}
	}
	o, err := e.g.db.ExecOrders().GetOrder(orderID)
	if err != nil {
		e.Fatalf("读执行单: %v", err)
	}
	return o
}

// ---- ① submitted 超时 → cancel_requested ----

func TestTimeoutSubmittedBeyondThreshold(t *testing.T) {
	e := newTimeoutEnv(t, "to-submitted")
	o := e.mkOrder(db.ExecStateSubmitted, e.now.Add(-31*time.Minute), "")

	res, err := e.s.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.TimedOut != 1 {
		t.Errorf("应有 1 单超时，实际 %+v", res)
	}

	got, _ := e.g.db.ExecOrders().GetOrder(o.ID)
	if got.State != db.ExecStateCancelRequested {
		t.Errorf("状态应为 cancel_requested，实际 %s", got.State)
	}
}

// ---- ② 未到阈值不动 ----

func TestTimeoutWithinThresholdDoesNothing(t *testing.T) {
	e := newTimeoutEnv(t, "to-within")
	o := e.mkOrder(db.ExecStateSubmitted, e.now.Add(-5*time.Minute), "")

	res, err := e.s.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.TimedOut != 0 {
		t.Errorf("未到阈值不该转状态，实际 %+v", res)
	}
	got, _ := e.g.db.ExecOrders().GetOrder(o.ID)
	if got.State != db.ExecStateSubmitted {
		t.Errorf("状态应保持 submitted，实际 %s", got.State)
	}
}

// ---- ③★ partial 超时 → cancel_requested，且 filled_qty 保留（最关键）----

func TestTimeoutPartialPreservesFilledQty(t *testing.T) {
	e := newTimeoutEnv(t, "to-partial")
	// 部分成交：qty=10, filled_qty=3
	o := e.mkOrder(db.ExecStatePartial, e.now.Add(-31*time.Minute), "3")

	res, err := e.s.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.TimedOut != 1 {
		t.Errorf("partial 超时应转状态，实际 %+v", res)
	}

	got, _ := e.g.db.ExecOrders().GetOrder(o.ID)
	if got.State != db.ExecStateCancelRequested {
		t.Errorf("状态应为 cancel_requested，实际 %s", got.State)
	}

	// ★ 关键断言：已成交的 3 不能丢。
	//
	// 那 3 手是真实成交过的，是「已实现盈亏」的一部分。
	// 如果转状态时清空 filled_qty，这笔记录就永久丢了 ——
	// 而对账/报表都依赖它。
	if got.FilledQty != "3.0000000000" && got.FilledQty != "3" {
		t.Errorf("filled_qty 应保留为 3（已成交部分不能丢），实际 %q", got.FilledQty)
	}
	if got.AvgPrice == nil || *got.AvgPrice == "" {
		t.Error("avg_price 应保留（已成交部分的价格）")
	}
}

// ---- ④ 对账优先：状态已被改成 filled 时，超时改不动 ----

func TestTimeoutSkipsWhenReconcileAlreadyAdvanced(t *testing.T) {
	e := newTimeoutEnv(t, "to-race")
	// 建一张已超时但仍标 submitted 的单
	o := e.mkOrder(db.ExecStateSubmitted, e.now.Add(-31*time.Minute), "")

	// 模拟「对账先跑，把它改成 filled」
	if _, err := e.g.db.Exec(`UPDATE orders_execution SET state='filled', filled_qty=10, avg_price=100 WHERE id=$1`, o.ID); err != nil {
		t.Fatalf("模拟对账: %v", err)
	}

	res, err := e.s.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// 状态应仍是 filled —— 超时扫描不该把它改回 cancel_requested
	got, _ := e.g.db.ExecOrders().GetOrder(o.ID)
	if got.State != db.ExecStateFilled {
		t.Errorf("对账已改成 filled，超时不该覆盖，实际 %s", got.State)
	}
	if res.TimedOut != 0 {
		t.Errorf("不该有任何单被超时转状态，实际 %+v", res)
	}
}

// ---- ⑤ Scanned 计数：两个状态都扫到 ----

func TestTimeoutScansBothStates(t *testing.T) {
	e := newTimeoutEnv(t, "to-both")
	e.mkOrder(db.ExecStateSubmitted, e.now.Add(-31*time.Minute), "")
	e.mkOrder(db.ExecStatePartial, e.now.Add(-31*time.Minute), "2")
	// 一张未超时的，不该被扫进 TimedOut
	e.mkOrder(db.ExecStateSubmitted, e.now.Add(-1*time.Minute), "")

	res, err := e.s.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.TimedOut != 2 {
		t.Errorf("应有 2 单超时（submitted + partial），实际 %+v", res)
	}
	if res.ByState[db.ExecStateSubmitted] != 1 || res.ByState[db.ExecStatePartial] != 1 {
		t.Errorf("ByState 应各 1，实际 %+v", res.ByState)
	}
}

// ---- ⑥ 终态订单不该被扫 ----

func TestTimeoutIgnoresTerminalStates(t *testing.T) {
	e := newTimeoutEnv(t, "to-terminal")
	// filled / cancelled / settled 都是终态，不在扫描范围内。
	for _, st := range []string{db.ExecStateFilled, db.ExecStateCancelled, db.ExecStateSettled, db.ExecStateRejected} {
		e.mkOrder(st, e.now.Add(-2*time.Hour), "")
	}

	res, err := e.s.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Scanned != 0 {
		t.Errorf("终态订单不该被扫，实际扫了 %d 张", res.Scanned)
	}

	// 而且状态都没变
	for _, st := range []string{db.ExecStateFilled, db.ExecStateCancelled} {
		var n int
		if err := e.g.db.QueryRow(`SELECT count(*) FROM orders_execution WHERE account_id=$1 AND state=$2`,
			e.acct.ID, st).Scan(&n); err != nil {
			t.Fatalf("数状态: %v", err)
		}
		if n == 0 {
			t.Errorf("状态 %s 的订单不该被改", st)
		}
	}
}

// ---- ⑦ threshold 参数化：自定义阈值生效 ----

func TestTimeoutCustomThreshold(t *testing.T) {
	e := newTimeoutEnv(t, "to-custom")
	// 用 5 分钟阈值，10 分钟前的单应超时
	e.s.threshold = 5 * time.Minute
	o := e.mkOrder(db.ExecStateSubmitted, e.now.Add(-10*time.Minute), "")

	res, err := e.s.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.TimedOut != 1 {
		t.Errorf("5 分钟阈值下，10 分钟前应超时，实际 %+v", res)
	}
	got, _ := e.g.db.ExecOrders().GetOrder(o.ID)
	if got.State != db.ExecStateCancelRequested {
		t.Errorf("状态应转 cancel_requested，实际 %s", got.State)
	}
}
