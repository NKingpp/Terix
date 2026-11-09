package db

// FreezeAccountTx 的验收测试（WE-10）。
//
// 核心要证明的不是「三件事都做了」，而是**三件事在同事务里**：
// 任何一步失败，前面做的全部回滚，不会留下半成品状态。

import (
	"errors"
	"strconv"
	"testing"
	"time"
)

// mkFreezeAcct 建一个可用的执行账户（带唯一 channel_acct_id）。
func mkFreezeAcct(t *testing.T, d *DB, tag string) *ExecAccount {
	t.Helper()
	uniq := tag + "-" + t.Name() + "-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	a, err := d.ExecAccounts().UpsertAccount(ExecAccountInput{
		Channel:       "okx",
		ChannelAcctID: uniq,
		Label:         "冻结测试账户",
		Market:        "crypto",
		MarketSupport: []string{"crypto"},
		Active:        true,
	})
	if err != nil {
		t.Fatalf("建账户失败: %v", err)
	}
	t.Cleanup(func() {
		// 顺序：先删审计，再解绑授权，最后删账户。
		//
		// # 为什么必须删 exec_audit
		//
		// FreezeAccountTx 会往 exec_audit 写一条 action='exec_frozen' 的记录
		// （审计是 append-only 的，业务侧没有删除入口 —— 那是对的，生产环境审计不该能删）。
		//
		// 但测试里必须自己清，否则它按 detail->>'account_id' 关联的账户被删后，
		// 这些审计就成了「孤儿」，永久累积。
		//
		// 实测（2026-10-29）：exec_audit 每次跑 db 包涨 5 条，全部来自
		// TestFreezeAccountTx* 这 4 个测试。而 db 包的 TestMain 原先没有残留守卫，
		// 所以谁都没发现 —— 直到把 exec_* 六张表加进守卫的检查范围。
		// 清理必须按 **数值** 比较。
		//
		// # 为什么不能用 detail->>'account_id'（PG 写法）
		//
		// SQLite 没有 `->>` 操作符，这条 DELETE 会报
		// `unrecognized token: ">"`，而错误被 `_, _ =` 吞掉 →
		// 审计永远删不掉 → 每次跑 db 包涨 5 行，残留守卫报红。
		//
		// # 为什么不能用 = $1（text 参数）
		//
		// FreezeAccountTx 写 detail 时 account_id 是 int64（exec_freeze.go:223），
		// json.Marshal 后 JSON 里是数字，json_extract 返回 **integer**。
		// SQLite 严格区分类型：
		//
		//	json_extract(detail,'$.account_id') = '42'   → 永不命中（text vs integer）
		//	json_extract(detail,'$.account_id') = 42     → 命中
		_, _ = d.Exec(`DELETE FROM exec_audit
   WHERE json_extract(detail,'$.account_id') = $1`, a.ID)
		// 授权表有 account_ref 但无外键，顺序不强制；按「先子后父」便于读。
		_, _ = d.Exec(`DELETE FROM exec_authorizations WHERE account_ref=$1`, uniq)
		_, _ = d.Exec(`DELETE FROM exec_accounts WHERE id=$1`, a.ID)
	})
	return a
}

// grantFreezeAuth 给账户授一条活跃授权。
func grantFreezeAuth(t *testing.T, d *DB, acct *ExecAccount) {
	t.Helper()
	_, err := d.ExecAuthorizations().Grant(ExecAuthorizationInput{
		Channel:           acct.Channel,
		AccountRef:        acct.ChannelAcctID,
		Mode:              string(ExecModeLiveManual),
		FromMode:          string(ExecModePaper),
		DisclaimerVersion: "freeze-test-v1",
		ConfirmPhrase:     "我已知晓风险",
		RulesRevision:     1,
		GrantedBy:         "freeze-test",
	})
	if err != nil {
		t.Fatalf("授权失败: %v", err)
	}
}

// TestFreezeAccountTxFreezesAll 正常路径：三件事都做了。
func TestFreezeAccountTxFreezesAll(t *testing.T) {
	d := testOpen(t)
	acct := mkFreezeAcct(t, d, "freeze-all")
	grantFreezeAuth(t, d, acct)

	// 先把模式设成 live_manual，这样才能验证「降模式」有效。
	if err := d.SetSetting(ExecModeKey, string(ExecModeLiveManual)); err != nil {
		t.Fatalf("设 live_manual 失败: %v", err)
	}
	t.Cleanup(func() { _ = d.SetSetting(ExecModeKey, string(ExecModePaper)) })

	res, err := d.FreezeAccountTx(acct.ID, acct.Channel, acct.ChannelAcctID, "测试：对账发现差异")
	if err != nil {
		t.Fatalf("FreezeAccountTx: %v", err)
	}
	if !res.AuthorizationRevoked {
		t.Error("应撤销授权")
	}
	if !res.AccountBlocked {
		t.Error("应拉黑账户")
	}
	if !res.ModeDowngraded {
		t.Error("应降模式")
	}
	if !res.Changed() {
		t.Error("Changed() 应为 true")
	}

	// 逐项核实持久化状态
	a, err := d.ExecAccounts().GetAccount(acct.ID)
	if err != nil {
		t.Fatalf("读账户: %v", err)
	}
	if a.BlockedAt == nil {
		t.Error("账户应被拉黑")
	}
	if a.BlockReason != "测试：对账发现差异" {
		t.Errorf("block_reason = %q", a.BlockReason)
	}

	if _, err := d.ExecAuthorizations().ActiveFor(acct.Channel, acct.ChannelAcctID); !errors.Is(err, ErrAuthorizationRequired) {
		t.Errorf("活跃授权应已撤销，实际 err=%v", err)
	}

	mode, err := d.GetExecMode()
	if err != nil {
		t.Fatalf("读模式: %v", err)
	}
	if mode != ExecModePaper {
		t.Errorf("模式应降为 paper，实际 %s", mode)
	}
}

// TestFreezeAccountTxIdempotent 重复冻结不报错，且 Changed()=false。
func TestFreezeAccountTxIdempotent(t *testing.T) {
	d := testOpen(t)
	acct := mkFreezeAcct(t, d, "freeze-idem")
	grantFreezeAuth(t, d, acct)
	if err := d.SetSetting(ExecModeKey, string(ExecModeLiveManual)); err != nil {
		t.Fatalf("设 live_manual: %v", err)
	}
	t.Cleanup(func() { _ = d.SetSetting(ExecModeKey, string(ExecModePaper)) })

	first, err := d.FreezeAccountTx(acct.ID, acct.Channel, acct.ChannelAcctID, "第一次")
	if err != nil {
		t.Fatalf("第一次冻结: %v", err)
	}
	if !first.Changed() {
		t.Fatal("第一次应有改动")
	}

	// 记下首次的 blocked_at —— 第二次不该覆盖它（首次的原因才是审计要的）。
	before, _ := d.ExecAccounts().GetAccount(acct.ID)

	second, err := d.FreezeAccountTx(acct.ID, acct.Channel, acct.ChannelAcctID, "第二次")
	if err != nil {
		t.Fatalf("第二次冻结应成功（幂等），实际 %v", err)
	}
	if second.Changed() {
		t.Error("第二次不该有改动（幂等）")
	}

	after, _ := d.ExecAccounts().GetAccount(acct.ID)
	if after.BlockReason != "第一次" {
		t.Errorf("block_reason 不该被覆盖，实际 %q", after.BlockReason)
	}
	if after.BlockedAt == nil || before.BlockedAt == nil || *after.BlockedAt != *before.BlockedAt {
		t.Error("blocked_at 不该被覆盖")
	}
}

// TestFreezeAccountTxRollbackOnUnknownAccount 账户不存在 → 整体回滚，授权不被撤销。
//
// # 这条测的是「事务边界」，不是「错误处理」
//
// 顺序是「撤授权 → 拉黑 → 降模式」。如果账户不存在，
// 第 ② 步失败 —— 此时第 ① 步已经执行。**必须回滚**，
// 否则会留下「授权被撤了但账户还在」的半成品。
func TestFreezeAccountTxRollbackOnUnknownAccount(t *testing.T) {
	d := testOpen(t)
	acct := mkFreezeAcct(t, d, "freeze-rollback")
	grantFreezeAuth(t, d, acct)

	// 用一个不存在的 accountID，但配 acct 真实的 channel/ref
	// —— 这样第 ① 步会成功撤掉授权，第 ② 步才会失败。
	const ghostID = int64(1 << 40)
	_, err := d.FreezeAccountTx(ghostID, acct.Channel, acct.ChannelAcctID, "幽灵账户")
	if !errors.Is(err, ErrExecAccountNotFound) {
		t.Fatalf("应返回 ErrExecAccountNotFound，实际 %v", err)
	}

	// 关键断言：授权必须还在（第 ① 步被回滚了）
	if _, aErr := d.ExecAuthorizations().ActiveFor(acct.Channel, acct.ChannelAcctID); aErr != nil {
		t.Errorf("授权应被回滚（仍活跃），实际 err=%v", aErr)
	}
}

// TestFreezeAccountTxRequiresReason 无原因时拒绝。
func TestFreezeAccountTxRequiresReason(t *testing.T) {
	d := testOpen(t)
	acct := mkFreezeAcct(t, d, "freeze-noreason")

	if _, err := d.FreezeAccountTx(acct.ID, acct.Channel, acct.ChannelAcctID, "   "); err == nil {
		t.Fatal("空原因应被拒绝")
	}
	// 账户不该被拉黑
	a, _ := d.ExecAccounts().GetAccount(acct.ID)
	if a.BlockedAt != nil {
		t.Error("拒绝后账户不该被拉黑")
	}
}

// TestFreezeAccountTxDoesNotDowngradeFrozen frozen 是更严格的状态，不该被降成 paper。
//
// # 为什么这条重要
//
// 如果实现写成「无条件 SET value='paper'」，那么「已冻结的系统」被再冻结一次
// 会变成 paper —— **那是放宽，与冻结的意图相反**。
func TestFreezeAccountTxDoesNotDowngradeFrozen(t *testing.T) {
	d := testOpen(t)
	acct := mkFreezeAcct(t, d, "freeze-frozen")

	if err := d.SetSetting(ExecModeKey, string(ExecModeFrozen)); err != nil {
		t.Fatalf("设 frozen: %v", err)
	}
	if m, _ := d.GetExecMode(); m != ExecModeFrozen {
		t.Fatalf("前置条件不成立：模式应为 frozen，实际 %s", m)
	}
	t.Cleanup(func() { _ = d.SetSetting(ExecModeKey, string(ExecModePaper)) })

	res, err := d.FreezeAccountTx(acct.ID, acct.Channel, acct.ChannelAcctID, "frozen 下冻结")
	if err != nil {
		t.Fatalf("FreezeAccountTx: %v", err)
	}
	if res.ModeDowngraded {
		t.Error("frozen 不该被降级成 paper（那是放宽）")
	}

	mode, _ := d.GetExecMode()
	if mode != ExecModeFrozen {
		t.Errorf("模式应保持 frozen，实际 %s", mode)
	}
}

// TestFreezeAccountTxNoAuthStillWorks 没有授权时也能冻结（paper 账户从未授权）。
func TestFreezeAccountTxNoAuthStillWorks(t *testing.T) {
	d := testOpen(t)
	acct := mkFreezeAcct(t, d, "freeze-noauth")
	// 故意不授权

	res, err := d.FreezeAccountTx(acct.ID, acct.Channel, acct.ChannelAcctID, "从未授权")
	if err != nil {
		t.Fatalf("FreezeAccountTx: %v", err)
	}
	if res.AuthorizationRevoked {
		t.Error("没有授权时不该报告「已撤销」")
	}
	if !res.AccountBlocked {
		t.Error("仍应拉黑账户")
	}
}
