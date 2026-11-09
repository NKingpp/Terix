package exec

// ExecutePaper 的验收测试（WE-9）。
//
// 重点不是「落了一张单」，而是三个前置条件各自守住：
//   ① 没有 verdict 记录 → 拒绝（必须先过闸门）
//   ② verdict 不是 allow → 拒绝
//   ③ 已经执行过 → 拒绝（防重复下单）
//
// 以及原子性：落单 + 标记草案同事务，不会出现「有单没标记」或「标记了没单」。

import (
	"errors"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// executePaperViaGate 走完整流程：先过闸门，再执行。
//
// 这个 helper 存在的意义是复现**真实调用顺序** ——
// ExecutePaper 要求「在此之前已经过闸门」，测试必须照着这个顺序来，
// 而不是直接调 ExecutePaper 再手动插 verdict（那测不到前置条件）。
func executePaperViaGate(t *testing.T, g *Gate, draftID, accountID int64) (int64, error) {
	t.Helper()
	res, err := g.Authorize(t.Context(), GateRequest{
		DraftID: draftID, AccountID: accountID, Mode: db.ExecModePaper,
	})
	if err != nil {
		t.Fatalf("Authorize 出错: %v", err)
	}
	if res.Decision != DecisionAllow {
		t.Fatalf("闸门未放行（%s: %s），无法测 ExecutePaper", res.ErrCode, res.Reason)
	}
	id, execErr := g.ExecutePaper(t.Context(), draftID, accountID)
	if execErr == nil && id > 0 {
		// 清理：paper_orders 是全局表，不清理会污染 db 包的
		// TestPaperOrderListAndEquityCurve（它断言「某天的成交额」，
		// 而残留行会改变那个聚合值）。
		t.Cleanup(func() {
			_, _ = g.db.Exec(`DELETE FROM paper_orders WHERE id=$1`, id)
		})
	}
	return id, execErr
}

// TestExecutePaperHappyPath 正常路径：过闸门 → 落一张 filled 的 paper 单。
func TestExecutePaperHappyPath(t *testing.T) {
	g, done := gateTestEnv(t)
	defer done()
	resetToPaper(t, g)

	acct := newGateTestAccount(t, g, "paper-happy")
	draftID := newGateTestDraft(t, g, "crypto")

	orderID, err := executePaperViaGate(t, g, draftID, acct.ID)
	if err != nil {
		t.Fatalf("ExecutePaper: %v", err)
	}
	if orderID <= 0 {
		t.Fatalf("应返回有效 order id，实际 %d", orderID)
	}

	o, err := g.db.PaperOrders().GetOrder(orderID)
	if err != nil {
		t.Fatalf("读订单: %v", err)
	}
	if o.Status != db.OrderStatusFilled {
		t.Errorf("status = %q，期望 filled（paper 立即全额成交）", o.Status)
	}
	if strings.TrimSpace(o.FilledQty) == "" || strings.TrimSpace(o.FilledQty) == "0" {
		t.Errorf("filled_qty 应已写入且非零，实际 %q", o.FilledQty)
	}
	if o.AccountID == nil || *o.AccountID != acct.ID {
		t.Errorf("account_id 应为 %d，实际 %v", acct.ID, o.AccountID)
	}
	if o.Mode != db.OrderModePaper {
		t.Errorf("mode = %q，期望 paper", o.Mode)
	}

	// 草案应被标记为已执行
	d, err := g.db.OrderDrafts().GetDraft(draftID)
	if err != nil {
		t.Fatalf("读草案: %v", err)
	}
	if d.ExecutedNote == nil || !strings.Contains(*d.ExecutedNote, "paper executed") {
		t.Errorf("草案的 executed_note 应被写入，实际 %v", d.ExecutedNote)
	}
}

// TestExecutePaperRequiresVerdict 没有风控记录 → 拒绝，并指明「必须先过闸门」。
//
// # 这条守的是「跳过闸门直接执行」
//
// 如果没有这个检查，调用方可以直接调 ExecutePaper 绕过风控 ——
// 而风控是本项目的核心。
func TestExecutePaperRequiresVerdict(t *testing.T) {
	g, done := gateTestEnv(t)
	defer done()
	resetToPaper(t, g)

	acct := newGateTestAccount(t, g, "paper-noverdict")
	draftID := newGateTestDraft(t, g, "crypto")
	// 故意不调 Authorize

	_, err := g.ExecutePaper(t.Context(), draftID, acct.ID)
	if err == nil {
		t.Fatal("没有风控记录时应拒绝")
	}
	if !errors.Is(err, ErrPaperNotAuthorized) {
		t.Fatalf("应返回 ErrPaperNotAuthorized，实际 %v", err)
	}
	// 错误信息要能直接指导排查：指出「必须先调 Gate.Authorize」
	if !strings.Contains(err.Error(), "Gate.Authorize") {
		t.Errorf("错误信息应提示先过闸门，实际：%v", err)
	}

	// 草案不该被标记为已执行
	d, _ := g.db.OrderDrafts().GetDraft(draftID)
	if d.ExecutedNote != nil && strings.TrimSpace(*d.ExecutedNote) != "" {
		t.Error("被拒绝的草案不该有 executed_note")
	}
}

// TestExecutePaperRejectsDeniedVerdict verdict 是 deny 时拒绝。
//
// # 为什么这条重要
//
// 不能只查「有 verdict 记录」就放行 —— 那等于「有过求值就能执行」，
// 而 deny 的记录也是「有求值」。
func TestExecutePaperRejectsDeniedVerdict(t *testing.T) {
	g, done := gateTestEnv(t)
	defer done()
	resetToPaper(t, g)

	acct := newGateTestAccount(t, g, "paper-denied")
	draftID := newGateTestDraft(t, g, "crypto")

	// 手动写一条 deny 的 verdict（模拟「过闸门被拒了但调用方仍想执行」）
	if _, err := g.db.InsertDraftRiskVerdict(db.DraftRiskVerdictInput{
		DraftID:       draftID,
		Verdict:       "deny",
		Reason:        "测试：手动写入的拒绝",
		RulesRevision: 1,
		EvaluatedBy:   "test",
	}); err != nil {
		t.Fatalf("写 verdict: %v", err)
	}

	_, err := g.ExecutePaper(t.Context(), draftID, acct.ID)
	if err == nil {
		t.Fatal("verdict=deny 时应拒绝执行")
	}
	if !errors.Is(err, ErrPaperNotAuthorized) {
		t.Fatalf("应返回 ErrPaperNotAuthorized，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "deny") {
		t.Errorf("错误信息应说明判定值是 deny，实际：%v", err)
	}
}

// TestExecutePaperRejectsDoubleExecution 同一张草案执行两次 → 第二次拒绝。
//
// # 这条守的是「重复下单」
//
// 而重复下单在 live 模式是真金白银的损失，所以 paper 也要守住同样的语义。
func TestExecutePaperRejectsDoubleExecution(t *testing.T) {
	g, done := gateTestEnv(t)
	defer done()
	resetToPaper(t, g)

	acct := newGateTestAccount(t, g, "paper-double")
	draftID := newGateTestDraft(t, g, "crypto")

	if _, err := executePaperViaGate(t, g, draftID, acct.ID); err != nil {
		t.Fatalf("第一次执行: %v", err)
	}

	_, err := g.ExecutePaper(t.Context(), draftID, acct.ID)
	if err == nil {
		t.Fatal("第二次执行应被拒绝")
	}
	if !errors.Is(err, ErrPaperAlreadyExecuted) {
		t.Fatalf("应返回 ErrPaperAlreadyExecuted，实际 %v", err)
	}

	// 库里应该只有一张单
	var n int
	if e := g.db.QueryRow(`SELECT count(*) FROM paper_orders WHERE account_id=$1`, acct.ID).Scan(&n); e != nil {
		t.Fatalf("数订单: %v", e)
	}
	if n != 1 {
		t.Errorf("应有 1 张 paper 单，实际 %d 张 —— 重复下单漏洞", n)
	}
}

// TestExecutePaperRejectsBlockedAccount 账户被冻结后不能执行。
//
// # 为什么 ExecutePaper 要重新查账户
//
// 「闸门通过」到「执行」之间账户可能被冻结。
// 而 Authorize 里的账户检查发生在更早时刻 —— 不能依赖它。
func TestExecutePaperRejectsBlockedAccount(t *testing.T) {
	g, done := gateTestEnv(t)
	defer done()
	resetToPaper(t, g)

	acct := newGateTestAccount(t, g, "paper-blocked")
	draftID := newGateTestDraft(t, g, "crypto")

	// 先过闸门（此时账户可用）
	res, err := g.Authorize(t.Context(), GateRequest{
		DraftID: draftID, AccountID: acct.ID, Mode: db.ExecModePaper,
	})
	if err != nil || res.Decision != DecisionAllow {
		t.Fatalf("前置条件不成立：闸门未放行（%v / %s）", err, res.Reason)
	}

	// 再冻结账户
	if err := g.db.ExecAccounts().Block(acct.ID, "测试：闸门后冻结"); err != nil {
		t.Fatalf("冻结账户: %v", err)
	}
	t.Cleanup(func() { _ = g.db.ExecAccounts().Unblock(acct.ID) })

	_, err = g.ExecutePaper(t.Context(), draftID, acct.ID)
	if err == nil {
		t.Fatal("账户被冻结后应拒绝执行")
	}
	if !errors.Is(err, db.ErrExecAccountBlocked) {
		t.Fatalf("应返回 ErrExecAccountBlocked，实际 %v", err)
	}
}

// TestExecutePaperRejectsZeroPrice 草案没有 ref_price → 执行层拒绝。
//
// # 这条要绕开闸门，因为闸门根本不会放行
//
// 「市价单（无 ref_price）」在 matcher 里是「名义金额无法计算」→ fail-closed deny。
// 所以这类草案永远过不了闸门 —— 闸门层已经守住了。
//
// 但执行层仍然要自己检查：万一将来闸门的口径变了（比如允许「无价但有量」的单），
// 执行层不能跟着放宽。这是纵深防御的第二层。
//
// 所以这个测试手工写一条 allow 的 verdict（模拟「闸门放行了」），
// 然后验证执行层仍然因为缺价而拒绝。
func TestExecutePaperRejectsZeroPrice(t *testing.T) {
	g, done := gateTestEnv(t)
	defer done()
	resetToPaper(t, g)

	acct := newGateTestAccount(t, g, "paper-noprice")
	// 建一张 ref_price 为 NULL 的草案
	draftID := newGateTestDraftWithPrice(t, g, "crypto", "0.0000000001", "")

	// 手工写一条 allow 的 verdict，模拟「闸门已放行」。
	// 这不代表真实流程会发生 —— 它只是为了把测试聚焦到「执行层自己的检查」。
	if _, err := g.db.InsertDraftRiskVerdict(db.DraftRiskVerdictInput{
		DraftID:       draftID,
		Verdict:       "allow",
		Reason:        "测试：模拟闸门放行（真实流程不会放行无价单）",
		RulesRevision: 0,
		EvaluatedBy:   "test",
	}); err != nil {
		t.Fatalf("写 verdict: %v", err)
	}

	_, err := g.ExecutePaper(t.Context(), draftID, acct.ID)
	if err == nil {
		t.Fatal("没有 ref_price 时执行层应拒绝（纵深防御第二层）")
	}
	if !strings.Contains(err.Error(), "ref_price") {
		t.Errorf("错误信息应提到 ref_price，实际：%v", err)
	}

	// 草案不该被标记为已执行
	d, _ := g.db.OrderDrafts().GetDraft(draftID)
	if d.ExecutedNote != nil && strings.TrimSpace(*d.ExecutedNote) != "" {
		t.Error("被拒绝的草案不该有 executed_note")
	}
}

// TestGetPaperEquityCurveMarksFeeNotIncluded EquityCurve 必须标明「不含手续费」。
//
// # 为什么这条要测
//
// 「缺失」必须出现在数据里，不能只在文档里 —— 文档没人读，API 字段会被消费。
// 如果 FeeIncluded 被误改成 true，前端就会把毛收益当净收益展示。
func TestGetPaperEquityCurveMarksFeeNotIncluded(t *testing.T) {
	g, done := gateTestEnv(t)
	defer done()

	curve, err := g.GetPaperEquityCurve(t.Context())
	if err != nil {
		t.Fatalf("GetPaperEquityCurve: %v", err)
	}
	if curve.FeeIncluded {
		t.Error("FeeIncluded 必须为 false —— paper 不记手续费，" +
			"标 true 会让用户把毛收益当净收益")
	}
}
