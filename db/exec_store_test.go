package db

// 四张 exec 表的仓储层冒烟测试。
//
// # 为什么必须有这组测试
//
// 仓储层最容易出的错是 **SQL 列顺序与 scan 参数顺序不一致** ——
// 这种错编译器完全看不见（都是 interface{}），只有真连库跑一次才暴露。
//
// 所以这里不测业务逻辑（那是 exec 包的事），只测「能读能写、字段没串位」。
// 每个测试都用唯一标签隔离，cleanup 走 t.Cleanup。

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// sharedExecDB 是包级共享的测试库实例。
//
// # 为什么共享而不是每个测试 Open 一次
//
// `db.Open` 每次都跑完整 schema.sql（含 CREATE OR REPLACE FUNCTION /
// DROP TRIGGER 等 DDL）。DDL 拿 ShareLock，而业务 DML 拿 RowShareLock，
// **两者互斥**。本包测试有 200+ 个，再加上我新增的 30 次 Open，
// 会让「某个测试正在 DML」与「下一个测试的 Open 在做 DDL」重叠，
// PostgreSQL 直接报 `deadlock detected (SQLSTATE 40P01)`。
//
// 实测证据：跳过我的两个并发测试 → 全量 PASS；
// 加上它们 → 每次都 FAIL 且失败点在 assets_test.go（别的测试上）。
//
// 共享一个实例后，整个包只跑一次 DDL，冲突窗口消失。
// 各测试之间用唯一标签隔离数据（见 execTestTag）。
var (
	sharedExecDBOnce sync.Once
	sharedExecDBInst *DB
	sharedExecDBErr  error
)

// execTestDB 返回包级共享的测试库；PG 不可用时 skip。
func execTestDB(t *testing.T) *DB {
	t.Helper()
	sharedExecDBOnce.Do(func() {
		sharedExecDBInst, sharedExecDBErr = Open(testDSN(t))
	})
	if sharedExecDBErr != nil {
		t.Skipf("postgres unavailable (%v)", sharedExecDBErr)
	}
	return sharedExecDBInst
}

// execTestTag 生成唯一标签，避免测试之间互相污染。
func execTestTag(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("t-%d", time.Now().UnixNano())
}

// newExecAccount 建一个测试账户，测试结束自动删。
func newExecAccount(t *testing.T, d *DB, tag string) *ExecAccount {
	t.Helper()
	a, err := d.ExecAccounts().UpsertAccount(ExecAccountInput{
		Channel:       "okx",
		ChannelAcctID: tag,
		Label:         "测试账户 " + tag,
		Market:        "crypto",
		MarketSupport: []string{"crypto"},
		Active:        true,
	})
	if err != nil {
		t.Fatalf("建账户失败: %v", err)
	}
	t.Cleanup(func() {
		// 先删对账/订单再删账户（外键 RESTRICT）。
		_, _ = d.Exec(`DELETE FROM exec_reconciliations WHERE account_id=$1`, a.ID)
		_, _ = d.Exec(`DELETE FROM orders_execution WHERE account_id=$1`, a.ID)
		_, _ = d.Exec(`DELETE FROM exec_accounts WHERE id=$1`, a.ID)
	})
	return a
}

// TestExecAccountRoundTrip 账户存取 + text[] 数组没串位。
func TestExecAccountRoundTrip(t *testing.T) {
	d := execTestDB(t)
	tag := execTestTag(t)
	a := newExecAccount(t, d, tag)

	if a.Channel != "okx" {
		t.Errorf("channel = %q, want okx", a.Channel)
	}
	if a.ChannelAcctID != tag {
		t.Errorf("channel_account_id = %q, want %q", a.ChannelAcctID, tag)
	}
	if a.Market != "crypto" {
		t.Errorf("market = %q, want crypto", a.Market)
	}
	// text[] 是关键：解码路径走 to_json，串位会得到 nil 或 panic。
	if len(a.MarketSupport) != 1 || a.MarketSupport[0] != "crypto" {
		t.Errorf("market_support = %v, want [crypto]", a.MarketSupport)
	}
	if !a.Active {
		t.Error("active 应为 true")
	}
	if a.BlockedAt != nil {
		t.Errorf("新账户不该被拉黑，实际 blocked_at=%v", *a.BlockedAt)
	}

	// 读回（走 ListAccounts，覆盖多行扫描）
	list, err := d.ExecAccounts().ListAccounts()
	if err != nil {
		t.Fatalf("列账户失败: %v", err)
	}
	found := false
	for _, x := range list {
		if x.ID == a.ID {
			found = true
			if len(x.MarketSupport) != 1 {
				t.Errorf("列表里的 market_support 丢了: %v", x.MarketSupport)
			}
		}
	}
	if !found {
		t.Error("刚建的账户不在列表里")
	}
}

// TestExecAccountUpsertKeepsIdentity 改 label 不该产生新记录。
func TestExecAccountUpsertKeepsIdentity(t *testing.T) {
	d := execTestDB(t)
	tag := execTestTag(t)
	a := newExecAccount(t, d, tag)

	updated, err := d.ExecAccounts().UpsertAccount(ExecAccountInput{
		Channel:       "okx",
		ChannelAcctID: tag, // 同一身份
		Label:         "改过的名字",
		Market:        "crypto",
		MarketSupport: []string{"crypto"},
		Active:        true,
	})
	if err != nil {
		t.Fatalf("二次 upsert 失败: %v", err)
	}
	if updated.ID != a.ID {
		t.Errorf("改 label 产生了新记录: %d → %d", a.ID, updated.ID)
	}
	if updated.Label != "改过的名字" {
		t.Errorf("label = %q, want 改过的名字", updated.Label)
	}
}

// TestExecAccountBlockAndRequireUsable 拉黑后 RequireUsable 拒绝。
func TestExecAccountBlockAndRequireUsable(t *testing.T) {
	d := execTestDB(t)
	tag := execTestTag(t)
	a := newExecAccount(t, d, tag)

	if _, err := d.ExecAccounts().RequireUsable(a.ID); err != nil {
		t.Fatalf("未拉黑时应可用: %v", err)
	}

	if err := d.ExecAccounts().Block(a.ID, "风控触发：日亏损超限"); err != nil {
		t.Fatalf("拉黑失败: %v", err)
	}
	_, err := d.ExecAccounts().RequireUsable(a.ID)
	if !errors.Is(err, ErrExecAccountBlocked) {
		t.Fatalf("拉黑后 RequireUsable 应返回 ErrExecAccountBlocked，实际: %v", err)
	}

	// 拉黑状态要能在列表里看到（否则用户不知道它为什么不出单）
	got, gErr := d.ExecAccounts().GetAccount(a.ID)
	if gErr != nil {
		t.Fatalf("读回失败: %v", gErr)
	}
	if got.BlockedAt == nil {
		t.Error("blocked_at 应非空")
	}
	if got.BlockReason != "风控触发：日亏损超限" {
		t.Errorf("block_reason = %q", got.BlockReason)
	}

	if err := d.ExecAccounts().Unblock(a.ID); err != nil {
		t.Fatalf("解封失败: %v", err)
	}
	if _, err := d.ExecAccounts().RequireUsable(a.ID); err != nil {
		t.Errorf("解封后应可用: %v", err)
	}
}

// TestExecAuthorizationGrantAndActive 授权写入 + 读活跃 + 撤销。
func TestExecAuthorizationGrantAndActive(t *testing.T) {
	d := execTestDB(t)
	tag := execTestTag(t)
	ref := "acct-" + tag
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM exec_authorizations WHERE account_ref=$1`, ref)
	})

	id, err := d.ExecAuthorizations().Grant(ExecAuthorizationInput{
		Channel:           "okx",
		AccountRef:        ref,
		Mode:              string(ExecModeLiveManual),
		FromMode:          string(ExecModePaper),
		DisclaimerVersion: "v1",
		ConfirmPhrase:     "我已知晓实盘风险",
		LimitsSnapshot:    json.RawMessage(`{"max_single_amount":"1000"}`),
		RulesRevision:     7,
	})
	if err != nil {
		t.Fatalf("授权失败: %v", err)
	}
	if id <= 0 {
		t.Fatalf("id 应为正，实际 %d", id)
	}

	a, err := d.ExecAuthorizations().ActiveFor("okx", ref)
	if err != nil {
		t.Fatalf("读活跃授权失败: %v", err)
	}
	if a.Mode != string(ExecModeLiveManual) {
		t.Errorf("mode = %q", a.Mode)
	}
	if a.FromMode != string(ExecModePaper) {
		t.Errorf("from_mode = %q", a.FromMode)
	}
	if a.DisclaimerVersion != "v1" {
		t.Errorf("disclaimer_version = %q", a.DisclaimerVersion)
	}
	if a.RulesRevision != 7 {
		t.Errorf("rules_revision = %d, want 7", a.RulesRevision)
	}
	// limits_snapshot 是 JSONB，必须原样保留
	var snap map[string]string
	if err := json.Unmarshal(a.LimitsSnapshot, &snap); err != nil {
		t.Fatalf("limits_snapshot 不是合法 JSON: %v", err)
	}
	if snap["max_single_amount"] != "1000" {
		t.Errorf("limits_snapshot 内容错: %v", snap)
	}

	if err := d.ExecAuthorizations().Revoke("okx", ref, "用户手动撤销"); err != nil {
		t.Fatalf("撤销失败: %v", err)
	}
	if _, err := d.ExecAuthorizations().ActiveFor("okx", ref); !errors.Is(err, ErrAuthorizationRequired) {
		t.Errorf("撤销后 ActiveFor 应返回 ErrAuthorizationRequired，实际: %v", err)
	}
}

// TestExecAuthorizationRegrantRevokesOld 重复授权会自动撤销旧的
// （schema 有部分唯一索引 uq_auth_active，不撤销就撞索引）。
func TestExecAuthorizationRegrantRevokesOld(t *testing.T) {
	d := execTestDB(t)
	tag := execTestTag(t)
	ref := "acct2-" + tag
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM exec_authorizations WHERE account_ref=$1`, ref)
	})

	firstID, err := d.ExecAuthorizations().Grant(ExecAuthorizationInput{
		Channel: "okx", AccountRef: ref,
		Mode: string(ExecModeLiveWhitelist), FromMode: string(ExecModePaper),
		DisclaimerVersion: "v1", ConfirmPhrase: "确认",
		RulesRevision: 1,
	})
	if err != nil {
		t.Fatalf("首次授权失败: %v", err)
	}

	secondID, err := d.ExecAuthorizations().Grant(ExecAuthorizationInput{
		Channel: "okx", AccountRef: ref,
		Mode: string(ExecModeLiveManual), FromMode: string(ExecModeLiveWhitelist),
		DisclaimerVersion: "v2", ConfirmPhrase: "确认升级",
		RulesRevision: 2,
	})
	if err != nil {
		t.Fatalf("二次授权（应自动撤销旧的）失败: %v", err)
	}
	if secondID == firstID {
		t.Fatal("二次授权应产生新记录")
	}

	active, err := d.ExecAuthorizations().ActiveFor("okx", ref)
	if err != nil {
		t.Fatalf("读活跃授权失败: %v", err)
	}
	if active.ID != secondID {
		t.Errorf("活跃授权应是新的那条 (%d)，实际 %d", secondID, active.ID)
	}

	// 旧的那条应已撤销（审计可见）
	old, err := d.ExecAuthorizations().GetByID(firstID)
	if err != nil {
		t.Fatalf("读旧授权失败: %v", err)
	}
	if old.RevokedAt == nil {
		t.Error("旧授权应被自动撤销（revoked_at 非空）")
	}
	if old.RevokedReason == "" {
		t.Error("自动撤销应写原因（否则审计时无法判断是升档还是手动撤）")
	}
}

// TestExecOrderRoundTripAndIdempotency 订单存取 + 幂等键。
func TestExecOrderRoundTripAndIdempotency(t *testing.T) {
	d := execTestDB(t)
	tag := execTestTag(t)
	a := newExecAccount(t, d, tag)
	draftID := newDraftForExecTest(t, d)

	coid := "coid-" + tag
	id, err := d.ExecOrders().InsertOrder(ExecOrderInput{
		DraftID: draftID, AccountID: a.ID,
		Mode: string(ExecModeLiveManual), ClientOrderID: coid, Qty: "1.5",
	})
	if err != nil {
		t.Fatalf("插入订单失败: %v", err)
	}

	got, err := d.ExecOrders().GetOrder(id)
	if err != nil {
		t.Fatalf("读订单失败: %v", err)
	}
	if got.ClientOrderID != coid {
		t.Errorf("client_order_id = %q", got.ClientOrderID)
	}
	if got.Qty != "1.5000000000" {
		t.Errorf("qty = %q，NUMERIC(28,10) 应保留 10 位小数", got.Qty)
	}
	if got.FilledQty != "0.0000000000" {
		t.Errorf("filled_qty 默认应为 0，实际 %q", got.FilledQty)
	}
	if got.State != ExecStateDraft {
		t.Errorf("默认 state 应为 draft，实际 %q", got.State)
	}
	if got.AvgPrice != nil {
		t.Errorf("未成交时 avg_price 应为 nil，实际 %v", *got.AvgPrice)
	}

	// 幂等键冲突必须是可识别的哨兵错误，而不是裸 23505
	_, err = d.ExecOrders().InsertOrder(ExecOrderInput{
		DraftID: draftID, AccountID: a.ID,
		Mode: string(ExecModeLiveManual), ClientOrderID: coid, Qty: "2",
	})
	if !errors.Is(err, ErrExecDuplicateClientOrderID) {
		t.Fatalf("重复 client_order_id 应返回 ErrExecDuplicateClientOrderID，实际: %v", err)
	}

	// 按幂等键能读回原单（这是幂等重试的正确姿势）
	byCoid, err := d.ExecOrders().GetByClientOrderID(coid)
	if err != nil {
		t.Fatalf("按幂等键读回失败: %v", err)
	}
	if byCoid.ID != id {
		t.Errorf("按幂等键读回的 id = %d, want %d", byCoid.ID, id)
	}
}

// TestExecReconciliationRoundTrip 对账记录存取 + 解析。
func TestExecReconciliationRoundTrip(t *testing.T) {
	d := execTestDB(t)
	tag := execTestTag(t)
	a := newExecAccount(t, d, tag)

	id, err := d.ExecReconciliations().Record(ExecReconciliationInput{
		AccountID:      a.ID,
		Kind:           ReconAmount,
		LocalSnapshot:  json.RawMessage(`{"qty":"1"}`),
		RemoteSnapshot: json.RawMessage(`{"qty":"0.9"}`),
		Diff:           json.RawMessage(`{"qty_delta":"0.1"}`),
	})
	if err != nil {
		t.Fatalf("写对账记录失败: %v", err)
	}

	got, err := d.ExecReconciliations().GetReconciliation(id)
	if err != nil {
		t.Fatalf("读对账记录失败: %v", err)
	}
	if got.Kind != ReconAmount {
		t.Errorf("kind = %q", got.Kind)
	}
	if got.ResolvedAt != nil {
		t.Error("新记录不该有 resolved_at")
	}
	var diff map[string]string
	if err := json.Unmarshal(got.Diff, &diff); err != nil {
		t.Fatalf("diff 不是合法 JSON: %v", err)
	}
	if diff["qty_delta"] != "0.1" {
		t.Errorf("diff 内容错: %v", diff)
	}

	// 未解析列表应能查到
	unresolved, err := d.ExecReconciliations().ListUnresolved(50)
	if err != nil {
		t.Fatalf("列未解析失败: %v", err)
	}
	found := false
	for _, r := range unresolved {
		if r.ID == id {
			found = true
		}
	}
	if !found {
		t.Error("刚写的未解析记录不在列表里")
	}

	if err := d.ExecReconciliations().Resolve(id, "张三"); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	after, err := d.ExecReconciliations().GetReconciliation(id)
	if err != nil {
		t.Fatalf("解析后读回失败: %v", err)
	}
	if after.ResolvedAt == nil {
		t.Error("解析后 resolved_at 应非空")
	}
	if after.ResolvedBy != "张三" {
		t.Errorf("resolved_by = %q", after.ResolvedBy)
	}

	// 重复解析应报错（而不是静默成功）
	if err := d.ExecReconciliations().Resolve(id, "李四"); err == nil {
		t.Error("重复解析应报错，不能静默成功")
	}
}

// TestExecReconKindValidation 非法分类被拒（防止 query_failed 被记成 state）。
func TestExecReconKindValidation(t *testing.T) {
	d := execTestDB(t)
	tag := execTestTag(t)
	a := newExecAccount(t, d, tag)

	_, err := d.ExecReconciliations().Record(ExecReconciliationInput{
		AccountID: a.ID,
		Kind:      "some_random_kind",
	})
	if !errors.Is(err, ErrOrderInvalid) {
		t.Errorf("非法 kind 应返回 ErrOrderInvalid，实际: %v", err)
	}
}

// newDraftForExecTest 建一张 orders_draft（orders_execution 有外键指向它）。
func newDraftForExecTest(t *testing.T, d *DB) int64 {
	t.Helper()
	var id int64
	err := d.QueryRow(`INSERT INTO orders_draft (side, qty) VALUES ('buy', 1) RETURNING id`).Scan(&id)
	if err != nil {
		t.Fatalf("建 orders_draft 失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM orders_execution WHERE draft_id=$1`, id)
		_, _ = d.Exec(`DELETE FROM orders_draft WHERE id=$1`, id)
	})
	return id
}
