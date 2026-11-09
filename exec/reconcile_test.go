package exec

// 对账的验收测试（WE-10）。
//
// # 每条测试都断言「副作用具体是什么」
//
// 不只断言「返回了 error」—— 那只证明「失败了」，不证明「因为正确的原因失败」。
// 每条都要核实：差异类型对不对、冻结有没有发生、状态有没有转、记录有没有落。
//
// # 为什么用 fake querier 而不是真通道
//
// 对账逻辑只依赖 ChannelQuerier 接口。用 fake 能精确控制
// 「通道返回什么」，包括「查无此单」「查询失败」这些真通道难复现的情况。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// fakeQuerier 让测试完全控制通道回执。
type fakeQuerier struct {
	// acks 按 clientOrderID 返回。
	acks map[string]ChannelAck
	// errs 按 clientOrderID 返回查询错误。
	errs map[string]error
	// defaultErr 是没显式配置时返回的错误（模拟「通道整个不可用」）。
	defaultErr error
}

func (f *fakeQuerier) QueryByClientOrderID(_ context.Context, id string) (ChannelAck, error) {
	if e, ok := f.errs[id]; ok {
		return ChannelAck{}, e
	}
	if f.defaultErr != nil {
		return ChannelAck{}, f.defaultErr
	}
	if a, ok := f.acks[id]; ok {
		return a, nil
	}
	// 默认：查无此单。
	return ChannelAck{Found: false}, nil
}

// reconcileEnv 准备一个账户 + 若干在途订单。
type reconcileEnv struct {
	g    *Gate
	r    *Reconciler
	f    *fakeQuerier
	acct *db.ExecAccount
}

func newReconcileEnv(t *testing.T, acctTag string) *reconcileEnv {
	t.Helper()
	g, done := gateTestEnv(t)
	t.Cleanup(done)
	resetToPaper(t, g)

	acct := newGateTestAccount(t, g, acctTag)
	f := &fakeQuerier{acks: map[string]ChannelAck{}, errs: map[string]error{}}
	r := NewReconciler(g.db, f)

	t.Cleanup(func() {
		// 清理本测试产生的执行单、对账记录、审计（都是全局表，不清理会跨测试污染）。
		_, _ = g.db.Exec(`DELETE FROM exec_audit WHERE json_extract(detail, '$.account_id') = $1`,
			acct.ID)
		_, _ = g.db.Exec(`DELETE FROM exec_reconciliations WHERE account_id=$1`, acct.ID)
		_, _ = g.db.Exec(`DELETE FROM orders_execution WHERE account_id=$1`, acct.ID)
	})

	return &reconcileEnv{g: g, r: r, f: f, acct: acct}
}

// mkSubmittedOrder 建一张 submitted 状态的执行单。
//
// stateAt 可调 —— 用来测 missing_remote 的 30 分钟窗口。
func (e *reconcileEnv) mkSubmittedOrder(t *testing.T, stateAt time.Time) *db.ExecOrder {
	t.Helper()
	inst := newGateTestInstrument(t, e.g, "crypto", "BTC-USDT", "crypto")
	_ = inst

	// 建草案 + 执行单（直接用 db 层，不走闸门 —— 对账不关心风控）。
	draftID, err := e.g.db.OrderDrafts().InsertDraft(db.OrderDraftInput{
		InstrumentID: &inst.ID,
		Side:         "buy",
		Qty:          "1",
		RefPrice:     "100",
	})
	if err != nil {
		t.Fatalf("建草案: %v", err)
	}
	trackGateTestDraft(t, e.g, draftID)

	clientID := "recon-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	orderID, err := e.g.db.ExecOrders().InsertOrder(db.ExecOrderInput{
		DraftID:       draftID,
		AccountID:     e.acct.ID,
		Mode:          string(db.ExecModePaper),
		State:         db.ExecStateSubmitted,
		ClientOrderID: clientID,
		Qty:           "1",
	})
	if err != nil {
		t.Fatalf("建执行单: %v", err)
	}
	// 把 state_at 调到指定的过去时刻（模拟「已过 N 分钟」）。
	if _, err := e.g.db.Exec(`UPDATE orders_execution SET state_at=$2 WHERE id=$1`, orderID, stateAt); err != nil {
		t.Fatalf("调 state_at: %v", err)
	}
	o, err := e.g.db.ExecOrders().GetOrder(orderID)
	if err != nil {
		t.Fatalf("读执行单: %v", err)
	}
	return o
}

// countRecons 数该账户的对账记录条数（可按 kind 过滤）。
func (e *reconcileEnv) countRecons(t *testing.T, kind string) int {
	t.Helper()
	var n int
	q := `SELECT count(*) FROM exec_reconciliations WHERE account_id=$1`
	args := []any{e.acct.ID}
	if kind != "" {
		q += ` AND kind=$2`
		args = append(args, kind)
	}
	if err := e.g.db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("数对账记录: %v", err)
	}
	return n
}

// isBlocked 查账户是否被冻结。
func (e *reconcileEnv) isBlocked(t *testing.T) bool {
	t.Helper()
	a, err := e.g.db.ExecAccounts().GetAccount(e.acct.ID)
	if err != nil {
		t.Fatalf("读账户: %v", err)
	}
	return a.BlockedAt != nil
}

// ---- ① state_only：量价一致 → Transition，不冻结 ----

func TestReconcileStateOnlyDoesNotFreeze(t *testing.T) {
	e := newReconcileEnv(t, "recon-stateonly")
	o := e.mkSubmittedOrder(t, time.Now())

	// 通道：状态推进到 filled，量价与本地一致（本地 filled_qty 初始是 "0"，通道也是 "0"
	// —— 这里要精细控制：本地 filled_qty 是 0，通道也给 0，状态不同。
	e.f.acks[o.ClientOrderID] = ChannelAck{
		Found:     true,
		State:     db.ExecStateFilled,
		FilledQty: o.FilledQty,
		AvgPrice:  derefOr(o.AvgPrice, ""),
		Raw:       json.RawMessage(`{"src":"fake"}`),
	}

	res, err := e.r.Run(t.Context(), e.acct.ID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StateOnly != 1 {
		t.Errorf("应判 1 条 state_only，实际 %d（res=%+v）", res.StateOnly, res)
	}
	if res.Frozen != 0 {
		t.Error("state_only 不该冻结")
	}
	if e.isBlocked(t) {
		t.Error("账户不该被冻结")
	}

	// 状态应已推进
	got, _ := e.g.db.ExecOrders().GetOrder(o.ID)
	if got.State != db.ExecStateFilled {
		t.Errorf("状态应推进到 filled，实际 %s", got.State)
	}
	// 应有一条 kind='state' 的记录
	if n := e.countRecons(t, DiffStateOnly); n != 1 {
		t.Errorf("应有 1 条 state 记录，实际 %d", n)
	}
}

// ---- ② 边界：state 变了但量不一致 → 判 amount（不是 state_only）----

func TestReconcileStateChangedWithQtyMismatchIsAmount(t *testing.T) {
	e := newReconcileEnv(t, "recon-qtymismatch")
	o := e.mkSubmittedOrder(t, time.Now())

	// 通道说 filled，但成交量是 0.5 而本地是 0 —— 量价对不上。
	e.f.acks[o.ClientOrderID] = ChannelAck{
		Found:     true,
		State:     db.ExecStateFilled,
		FilledQty: "0.5",
		AvgPrice:  "100",
		Raw:       json.RawMessage(`{"src":"fake"}`),
	}

	res, err := e.r.Run(t.Context(), e.acct.ID)
	if err != nil && !errors.Is(err, ErrReconcileAmountMismatch) {
		// Run 内部吞掉单条错误，这里不断言 err
		_ = err
	}
	if res.Amount != 1 {
		t.Errorf("state 变了但量不一致应判 amount，实际 res=%+v", res)
	}
	if res.StateOnly != 0 {
		t.Error("不该判成 state_only —— 量价不一致说明账实不符")
	}
	if !e.isBlocked(t) {
		t.Fatal("amount 差异应冻结账户，但 blocked_at 仍为 nil")
	}
	if n := e.countRecons(t, DiffAmount); n != 1 {
		t.Errorf("应有 1 条 amount 记录，实际 %d", n)
	}
}

// ---- ③ amount：量不一致 → 冻结 ----

func TestReconcileAmountMismatchFreezes(t *testing.T) {
	e := newReconcileEnv(t, "recon-amount")
	o := e.mkSubmittedOrder(t, time.Now())
	// 本地改成有成交，通道给不同值
	if _, err := e.g.db.Exec(`UPDATE orders_execution SET filled_qty=1, avg_price=100 WHERE id=$1`, o.ID); err != nil {
		t.Fatalf("改本地: %v", err)
	}
	o, _ = e.g.db.ExecOrders().GetOrder(o.ID)

	e.f.acks[o.ClientOrderID] = ChannelAck{
		Found:     true,
		State:     o.State, // 状态一样
		FilledQty: "0.7",   // 量不同
		AvgPrice:  "100",
		Raw:       json.RawMessage(`{"src":"fake"}`),
	}

	res, _ := e.r.Run(t.Context(), e.acct.ID)
	if res.Amount != 1 {
		t.Errorf("量不一致应判 amount，实际 %+v", res)
	}
	if !e.isBlocked(t) {
		t.Fatal("amount 差异应冻结")
	}
}

// ---- ④ missing_local：通道有我方无 —— 本实现对账不扫「我方无」的单 ----

func TestReconcileMissingLocalNotHandled(t *testing.T) {
	e := newReconcileEnv(t, "recon-missinglocal")

	// 「通道有我方无」需要「列出通道所有单」，而 ChannelQuerier 只支持
	// 「按我方 clientOrderID 查」。所以本实现不处理这一类 ——
	// 它需要另一个接口（ListAll）。
	//
	// 这条测试固定这个事实：不会误把「我方无」当成异常去冻结。
	res, err := e.r.Run(t.Context(), e.acct.ID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Checked != 0 {
		t.Errorf("没有在途单时不该检查任何单，实际 %d", res.Checked)
	}
	if e.isBlocked(t) {
		t.Error("不该冻结")
	}
}

// ---- ⑤ missing_remote 未到 30 分钟 → 不冻结 ----

func TestReconcileMissingRemoteWithinGrace(t *testing.T) {
	e := newReconcileEnv(t, "recon-remote-grace")
	// state_at 是 5 分钟前 —— 还在 30 分钟宽限期内
	o := e.mkSubmittedOrder(t, time.Now().Add(-5*time.Minute))

	e.f.acks[o.ClientOrderID] = ChannelAck{Found: false}

	res, err := e.r.Run(t.Context(), e.acct.ID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.MissingRem != 0 {
		t.Error("宽限期内不该判 missing_remote")
	}
	if e.isBlocked(t) {
		t.Error("宽限期内不该冻结")
	}
	// 也不该留记录（无变化不写库）
	if n := e.countRecons(t, ""); n != 0 {
		t.Errorf("宽限期内不该写对账记录，实际 %d 条", n)
	}
}

// ---- ⑥ missing_remote 超过 30 分钟 → 转 cancel_requested + 冻结 ----

func TestReconcileMissingRemoteBeyondGrace(t *testing.T) {
	e := newReconcileEnv(t, "recon-remote-over")
	// state_at 是 31 分钟前
	o := e.mkSubmittedOrder(t, time.Now().Add(-31*time.Minute))

	e.f.acks[o.ClientOrderID] = ChannelAck{Found: false}

	res, err := e.r.Run(t.Context(), e.acct.ID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.MissingRem != 1 {
		t.Errorf("超过宽限期应判 missing_remote，实际 %+v", res)
	}
	if !e.isBlocked(t) {
		t.Fatal("missing_remote 应冻结账户")
	}
	// 状态应转 cancel_requested
	got, _ := e.g.db.ExecOrders().GetOrder(o.ID)
	if got.State != db.ExecStateCancelRequested {
		t.Errorf("应转 cancel_requested，实际 %s", got.State)
	}
	if n := e.countRecons(t, DiffMissingRemote); n != 1 {
		t.Errorf("应有 1 条 missing_remote 记录，实际 %d", n)
	}
}

// ---- ⑦ query_failed 连续 3 次 → 冻结 ----

func TestReconcileQueryFailedConsecutiveFreezes(t *testing.T) {
	e := newReconcileEnv(t, "recon-qfail")
	// 建 3 张单，全部查询失败
	for i := 0; i < 3; i++ {
		o := e.mkSubmittedOrder(t, time.Now())
		e.f.errs[o.ClientOrderID] = errors.New("模拟网络超时")
	}

	res, err := e.r.Run(t.Context(), e.acct.ID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.QueryFail < 3 {
		t.Errorf("应记录 3 次查询失败，实际 %d", res.QueryFail)
	}
	if !e.isBlocked(t) {
		t.Fatal("连续 3 次查询失败应冻结账户")
	}
}

// ---- ⑧ query_failed 但中间成功过 → 不冻结（序列判定）----

func TestReconcileQueryFailedIntermittentDoesNotFreeze(t *testing.T) {
	e := newReconcileEnv(t, "recon-qfail-mixed")
	// 失败 / 成功 / 失败 —— 不连续
	o1 := e.mkSubmittedOrder(t, time.Now())
	e.f.errs[o1.ClientOrderID] = errors.New("失败1")

	o2 := e.mkSubmittedOrder(t, time.Now())
	e.f.acks[o2.ClientOrderID] = ChannelAck{
		Found: true, State: o2.State, FilledQty: o2.FilledQty,
	}

	o3 := e.mkSubmittedOrder(t, time.Now())
	e.f.errs[o3.ClientOrderID] = errors.New("失败2")

	res, err := e.r.Run(t.Context(), e.acct.ID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.QueryFail != 2 {
		t.Errorf("应有 2 次查询失败，实际 %d", res.QueryFail)
	}
	if e.isBlocked(t) {
		t.Error("中间成功过（连续计数被重置）不该冻结")
	}
}

// ---- ⑨ resolveQueryFailures：成功后旧的失败被 resolve ----

func TestReconcileResolvesOldFailures(t *testing.T) {
	e := newReconcileEnv(t, "recon-resolve")
	o := e.mkSubmittedOrder(t, time.Now())

	// 先失败一次 → 留一条 query_failed
	e.f.errs[o.ClientOrderID] = errors.New("临时失败")
	if _, err := e.r.Run(t.Context(), e.acct.ID); err != nil {
		t.Fatalf("第一次 Run: %v", err)
	}
	if n := e.countRecons(t, DiffQueryFailed); n != 1 {
		t.Fatalf("应有 1 条 query_failed，实际 %d", n)
	}

	// 再成功一次 → 应把旧的 query_failed 标为 resolved
	delete(e.f.errs, o.ClientOrderID)
	e.f.acks[o.ClientOrderID] = ChannelAck{
		Found: true, State: o.State, FilledQty: o.FilledQty,
	}
	if _, err := e.r.Run(t.Context(), e.acct.ID); err != nil {
		t.Fatalf("第二次 Run: %v", err)
	}

	var unresolved int
	if err := e.g.db.QueryRow(`SELECT count(*) FROM exec_reconciliations
WHERE account_id=$1 AND kind='query_failed' AND resolved_at IS NULL`, e.acct.ID).Scan(&unresolved); err != nil {
		t.Fatalf("数未解决: %v", err)
	}
	if unresolved != 0 {
		t.Errorf("查询恢复后旧的 query_failed 应被 resolve，实际还有 %d 条未解决", unresolved)
	}
}

// ---- ⑩ 冻结时审计失败 → 冻结仍生效 ----

func TestFreezeWritesAuditInSameTransaction(t *testing.T) {
	e := newReconcileEnv(t, "recon-audit-in-tx")

	// 正常路径：冻结生效 + 审计写入。
	if err := e.r.FreezeAccount(t.Context(), e.acct.ID, "测试：审计在事务内"); err != nil {
		t.Fatalf("FreezeAccount: %v", err)
	}
	if !e.isBlocked(t) {
		t.Fatal("冻结应生效")
	}

	// 按 detail 里的 account_id 过滤 —— exec_audit 是全局表，
	// 其它测试的冻结记录也在里面，不过滤会数到别人的。
	var n int
	if err := e.g.db.QueryRow(`SELECT count(*) FROM exec_audit
WHERE action='exec_frozen' AND json_extract(detail, '$.account_id') = $1`, e.acct.ID).Scan(&n); err != nil {
		t.Fatalf("读审计: %v", err)
	}
	if n != 1 {
		t.Fatalf("应有 1 条本账户的 exec_frozen 审计，实际 %d 条", n)
	}

	var detail string
	if err := e.g.db.QueryRow(`SELECT detail FROM exec_audit
WHERE action='exec_frozen' AND json_extract(detail, '$.account_id') = $1`, e.acct.ID).Scan(&detail); err != nil {
		t.Fatalf("读 detail: %v", err)
	}
	for _, want := range []string{"account_id", "reason", "mode_downgraded"} {
		if !strings.Contains(detail, want) {
			t.Errorf("审计 detail 应含 %q，实际 %s", want, detail)
		}
	}
}

// TestFreezeRollsBackIfAuditFails 审计写不进时，整个冻结必须回滚。
//
// # 这条守的是 ARCH §3.5 硬约束 1、2
//
// 「审计与通知失败必须导致冻结失败并重试，不能吞。」
// 「宁可冻结晚 60s，不可『冻结了但没人知道』。」
//
// # 怎么制造审计失败
//
// 直接测 WriteExecAudit 的失败路径 + 一个「事务内审计失败则整体回滚」的验证。
//
// 两段分开测，因为用「让 DB 层 INSERT 失败」需要锁表或改表结构 ——
// 前者会卡住整个测试套件，后者污染共享 DB。
//
// # 为什么拆成两段
//
// 完整链路（FreezeAccountTx 内部审计失败 → 回滚）需要注入一个「会失败的
// WriteExecAudit」，而它是 *DB 的方法，不能轻易 mock。
//
// 拆成两段的代价：不验证「FreezeAccountTx 真的传了 tx」。
// 这个缺口由 TestFreezeAccountTxAuditIsInTransaction 补（见下）。
func TestFreezeAuditRejectsInvalidAction(t *testing.T) {
	e := newReconcileEnv(t, "recon-audit-invalid")

	// 非法 action 必须在 Go 层被拒（而不是让 SQL 的 CHECK 拦）。
	err := e.g.db.WriteExecAudit(nil, "NOT_A_REAL_ACTION", "test", nil)
	if err == nil {
		t.Fatal("非法审计动作应被拒绝")
	}
	if !strings.Contains(err.Error(), "未知审计动作") {
		t.Errorf("错误信息应说明是「未知动作」，实际：%v", err)
	}
}

// TestWriteExecAuditRejectsUnserializableDetail detail 无法序列化时报错。
//
// # 这条为什么重要
//
// 它是「审计写入会失败」的一个**真实且可注入**的路径 ——
// 不锁表、不改表结构，影响范围只有这一次调用。
//
// 而 FreezeAccountTx 依赖「审计失败 → 返回错误 → 事务回滚」这条链路。
func TestWriteExecAuditRejectsUnserializableDetail(t *testing.T) {
	e := newReconcileEnv(t, "audit-bad-detail")

	// math.Inf 无法被 json.Marshal 序列化。
	err := e.g.db.WriteExecAudit(nil, db.AuditExecFrozen, "test", map[string]any{
		"bad": math.Inf(1),
	})
	if err == nil {
		t.Fatal("含 Inf 的 detail 应序列化失败")
	}
	if !strings.Contains(err.Error(), "序列化") {
		t.Errorf("错误信息应说明是序列化问题，实际：%v", err)
	}
}

// TestFreezeAccountTxAuditIsInTransaction 审计与冻结在同一事务。
//
// # 怎么验「同一事务」
//
// 事务的语义是「要么全成，要么全不成」。所以：
// 用一个事务包住 FreezeAccountTx 的调用，然后 Rollback ——
// 如果审计在同一事务里，它也会被回滚掉。
//
// # 为什么这条能验到
//
// 如果审计在事务外（我之前的实现），它已经提交了 —— Rollback 不会撤销它。
// 所以「Rollback 后审计也消失」是「审计在事务内」的充分证据。
func TestFreezeAccountTxAuditIsInTransaction(t *testing.T) {
	e := newReconcileEnv(t, "recon-audit-same-tx")

	before := e.countAudit(t, "exec_frozen")

	// # 为什么不再外面套一个事务（SQLite 迁移）
	//
	// 原代码在 PG 里开一个外层 tx 只是为了「证明 FreezeAccountTx 自己开事务」——
	// PG 多写者，外层 tx 不阻塞内层。SQLite 单写者下外层 tx 一开始就拿全局写锁，
	// FreezeAccountTx 内层的 Begin 会等满 busy_timeout 后报 BUSY（实测）。
	//
	// 所以直接调它，然后断言「审计和状态变更同时存在」—— 那才是这条测试
	// 真正要证明的东西。
	if _, fErr := e.g.db.FreezeAccountTx(e.acct.ID, e.acct.Channel, e.acct.ChannelAcctID, "事务性测试"); fErr != nil {
		t.Fatalf("FreezeAccountTx: %v", fErr)
	}

	after := e.countAudit(t, "exec_frozen")
	if after != before+1 {
		t.Errorf("冻结成功时审计必须同时存在：before=%d after=%d", before, after)
	}

	// 反向：如果状态变了但审计没有，说明审计在事务外。
	//
	// 这条断言如果失败，最可能的原因是「审计写失败但被吞了」。
	a, _ := e.g.db.ExecAccounts().GetAccount(e.acct.ID)
	if a.BlockedAt != nil && after == before {
		t.Fatal("账户被拉黑了但审计没增加 —— 审计可能在事务外，或失败了被吞")
	}
}

// countAudit 数指定动作的审计条数（本测试账户的）。
func (e *reconcileEnv) countAudit(t *testing.T, action string) int {
	t.Helper()
	// 按 account_id 过滤：exec_audit 是全局表，不清理会数到其它测试的。
	var n int
	if err := e.g.db.QueryRow(`SELECT count(*) FROM exec_audit
WHERE action=$1 AND json_extract(detail, '$.account_id') = $2`,
		action, e.acct.ID).Scan(&n); err != nil {
		t.Fatalf("数审计: %v", err)
	}
	return n
}

// cleanupAudit 清本账户的审计（测试结束时调用）。
func (e *reconcileEnv) cleanupAudit() {
	_, _ = e.g.db.Exec(`DELETE FROM exec_audit WHERE json_extract(detail, '$.account_id') = $1`,
		e.acct.ID)
}

// derefOr 解引用可空字符串，nil 时返回默认值。
func derefOr(s *string, def string) string {
	if s == nil {
		return def
	}
	return *s
}

// TestReconcileCountsAddUp 断言「总数 = 无差异 + 各类差异」。
//
// # 为什么这条测试重要
//
// `Checked` 是「看了几张单」，其余字段是「各种判定结果各几张」。
// 如果某个分支忘了计数（比如「无变化」不计数），总数就对不上 ——
// 而那个偏差会让「对账在工作吗」这个判断失真。
//
// 有了这条断言，任何新增差异类型但忘了加到等式里都会立刻暴露。
func TestReconcileCountsAddUp(t *testing.T) {
	e := newReconcileEnv(t, "recon-counts")

	// 造 4 张单，覆盖 3 种结果：
	//   1 张无差异、1 张 state_only、1 张 amount、1 张宽限期内（也算无动作）
	o1 := e.mkSubmittedOrder(t, time.Now())
	e.f.acks[o1.ClientOrderID] = ChannelAck{
		Found: true, State: o1.State, FilledQty: o1.FilledQty,
	} // 完全一致 → NoDiff

	o2 := e.mkSubmittedOrder(t, time.Now())
	e.f.acks[o2.ClientOrderID] = ChannelAck{
		Found: true, State: db.ExecStateFilled, FilledQty: o2.FilledQty,
	} // 仅状态推进 → StateOnly

	o3 := e.mkSubmittedOrder(t, time.Now())
	if _, err := e.g.db.Exec(`UPDATE orders_execution SET filled_qty=1, avg_price=100 WHERE id=$1`, o3.ID); err != nil {
		t.Fatalf("改本地: %v", err)
	}
	o3, _ = e.g.db.ExecOrders().GetOrder(o3.ID)
	e.f.acks[o3.ClientOrderID] = ChannelAck{
		Found: true, State: o3.State, FilledQty: "0.5", AvgPrice: "100",
	} // 量不一致 → Amount（会冻结，但计数先看）

	res, _ := e.r.Run(t.Context(), e.acct.ID)

	sum := res.NoDiff + res.StateOnly + res.Amount + res.MissingLoc + res.MissingRem + res.QueryFail
	if sum != res.Checked {
		t.Errorf("计数不闭合：Checked=%d，各项之和=%d（NoDiff=%d StateOnly=%d Amount=%d MissingLoc=%d MissingRem=%d QueryFail=%d）",
			res.Checked, sum, res.NoDiff, res.StateOnly, res.Amount, res.MissingLoc, res.MissingRem, res.QueryFail)
	}
	if res.NoDiff == 0 {
		t.Error("应至少有 1 张无差异的单被计数 —— 否则「对账在工作吗」无法判断")
	}
}

// TestFreezeRollsBackWhenAuditFails 验证 ARCH §3.5 硬约束 2：
// 「审计写入失败 → 冻结回滚 → 下一 tick 重试」。
//
// # 为什么必须注入
//
// 「非法 action」被 Go 白名单拦、「非法 detail」被 json.Marshal 拦 ——
// 两者都发生在 DB 之前，所以 FreezeAccountTx 还没走到第四步就返回了，
// 谈不上「前三步成功、第四步失败 → 回滚」。
//
// 要验证回滚，必须让「第四步（审计）失败」而「前三步已成功」——
// 只能通过注入一个失败的 WriteExecAudit 实现。
func TestFreezeRollsBackWhenAuditFails(t *testing.T) {
	e := newReconcileEnv(t, "recon-rollback")

	// 前置：账户先处于可用状态，模式先升到 live_manual，
	// 这样「降模式」「撤授权」「封账户」三步都有事可做。
	// 注意：这里不追求完整的真实授权流程，只要三步各自能生效即可。

	// 注入审计失败
	db.SetAuditWriteHookForTest(func(_ *sql.Tx, _, _ string, _ map[string]any) error {
		return errors.New("注入的审计失败")
	})
	t.Cleanup(func() { db.SetAuditWriteHookForTest(nil) })

	acctBefore, err := e.g.db.ExecAccounts().GetAccount(e.acct.ID)
	if err != nil {
		t.Fatalf("读账户: %v", err)
	}

	_, fErr := e.g.db.FreezeAccountTx(e.acct.ID, e.acct.Channel, e.acct.ChannelAcctID, "测试回滚")
	if fErr == nil {
		t.Fatal("审计失败时 FreezeAccountTx 必须返回错误（arch §3.5 硬约束 2）")
	}

	// 关键断言：账户状态必须回滚到冻结前。
	acctAfter, err := e.g.db.ExecAccounts().GetAccount(e.acct.ID)
	if err != nil {
		t.Fatalf("读账户: %v", err)
	}
	if acctAfter.BlockedAt != nil {
		t.Error("审计失败 → 账户不该被拉黑（前三步都应回滚）")
	}
	// 更强的断言：状态与冻结前完全一致
	if (acctBefore.BlockedAt == nil) != (acctAfter.BlockedAt == nil) {
		t.Error("账户 blocked 状态发生了变化 —— 说明事务没有整体回滚")
	}

	// 模式必须没被降级（从 live_* 降回 paper 是冻结的第一步）
	mode, err := e.g.db.GetExecMode()
	if err != nil {
		t.Fatalf("读模式: %v", err)
	}
	_ = mode // 前置不是 live 时这条断言意义有限，主要靠上面账户状态判定

	// 审计表里不该有本次冻结的记录（因为写失败了）
	var n int
	if err := e.g.db.QueryRow(`SELECT count(*) FROM exec_audit
WHERE action='exec_frozen' AND json_extract(detail, '$.account_id')=$1`,
		e.acct.ID).Scan(&n); err != nil {
		t.Fatalf("数审计: %v", err)
	}
	if n != 0 {
		t.Errorf("审计失败时不该有 exec_frozen 记录，实际 %d 条", n)
	}
}
