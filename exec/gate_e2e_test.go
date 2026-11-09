package exec

// WE-8 全链路集成测试：草案创建 → 风控求值 → verdict 落库 → Gate.Authorize。
//
// # 与单元测试的区别
//
// 单元测试每个只验一个分支，用的是手工造的数据。
// 这里**手工串起完整链路**，验的是「各环节接得上」：
//
//	InsertDraft → EvaluateDraft → InsertDraftRiskVerdict → Gate.Authorize
//
// # 为什么仍需要 CLI 真跑（见 cmd/execdemo）
//
// 集成测试在测试进程里，init() 顺序、连接池、环境变量都与真实运行不同。
// 它证明「接得上」，但不证明「真实进程里跑得起来」。

import (
	"testing"

	"github.com/Autumn-27/artex/db"
)

// TestGateEndToEnd 全链路：建草案 → 求值 → 落库 → 闸门放行 → 建执行订单。
func TestGateEndToEnd(t *testing.T) {
	g, done := gateTestEnv(t)
	defer done()
	resetToPaper(t, g)
	ensureAllowPassRules(t, g)
	setLimits(t, g, "100000", "1000000", 100)

	acct := newGateTestAccount(t, g, "e2e")

	// ---- 1. 建标的 + 草案 ----
	inst := newGateTestInstrument(t, g, "crypto", "BTC-USDT", "crypto")
	draftID, err := g.db.OrderDrafts().InsertDraft(db.OrderDraftInput{
		InstrumentID: &inst.ID,
		Side:         "buy",
		Qty:          "1",
		RefPrice:     "100",
	})
	if err != nil {
		t.Fatalf("建草案失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = g.db.Exec(`DELETE FROM draft_risk_verdicts WHERE draft_id=$1`, draftID)
		_, _ = g.db.Exec(`DELETE FROM orders_execution WHERE draft_id=$1`, draftID)
		_, _ = g.db.Exec(`DELETE FROM orders_draft WHERE id=$1`, draftID)
	})

	// ---- 2. 第一次求值（模拟草案创建时的评估）----
	draft, err := g.db.OrderDrafts().GetDraft(draftID)
	if err != nil {
		t.Fatalf("读草案失败: %v", err)
	}
	decision, err := g.evaluate(t.Context(), draft, inst, db.ExecModePaper)
	if err != nil {
		t.Fatalf("求值失败: %v", err)
	}
	if decision.Action != "allow" {
		t.Fatalf("首次求值应 allow，实际 %s（%s）", decision.Action, decision.Message)
	}

	// ---- 3. 落库（verdict + 指纹快照）----
	fp, err := g.rulesFingerprint()
	if err != nil {
		t.Fatalf("算指纹失败: %v", err)
	}
	snap := `{"fingerprint":"` + fp + `","limits_revision":1}`
	verdictID, err := g.db.InsertDraftRiskVerdict(db.DraftRiskVerdictInput{
		DraftID:       draftID,
		Verdict:       decision.Action, // allow → 归一成 pass
		Reason:        decision.Message,
		RulesRevision: 1,
		RulesSnapshot: []byte(snap),
		EvaluatedBy:   "e2e-test",
	})
	if err != nil {
		t.Fatalf("写 verdict 失败: %v", err)
	}
	if verdictID <= 0 {
		t.Fatalf("verdict id 应为正，实际 %d", verdictID)
	}

	// ---- 4. 校验两套记录一致（成败在此）----
	latest, err := g.db.LatestDraftRiskVerdict(draftID)
	if err != nil {
		t.Fatalf("读 verdict 失败: %v", err)
	}
	if latest.Verdict != "pass" {
		t.Errorf("verdict 应为 pass（allow 归一），实际 %q", latest.Verdict)
	}
	afterDraft, err := g.db.OrderDrafts().GetDraft(draftID)
	if err != nil {
		t.Fatalf("重读草案失败: %v", err)
	}
	if afterDraft.RiskVerdict == nil || *afterDraft.RiskVerdict != "pass" {
		t.Errorf("orders_draft.risk_verdict 应同步为 pass，实际 %v", afterDraft.RiskVerdict)
	}

	// ---- 5. 闸门放行 ----
	res, err := g.Authorize(t.Context(), GateRequest{
		DraftID: draftID, AccountID: acct.ID, Mode: db.ExecModePaper,
	})
	if err != nil {
		t.Fatalf("闸门返回 error: %v", err)
	}
	if res.Decision != DecisionAllow {
		t.Fatalf("闸门应 allow，实际 %s（%s：%s）", res.Decision, res.ErrCode, res.Reason)
	}
	if res.RulesFingerprint == "" {
		t.Error("结果应带规则指纹（审计锚）")
	}

	// ---- 6. 建执行订单（模拟 paper 落库）----
	orderID, err := g.db.ExecOrders().InsertOrder(db.ExecOrderInput{
		DraftID:       draftID,
		AccountID:     acct.ID,
		Mode:          string(db.ExecModePaper),
		ClientOrderID: "e2e-" + t.Name(),
		Qty:           "1",
	})
	if err != nil {
		t.Fatalf("建执行订单失败: %v", err)
	}
	if orderID <= 0 {
		t.Fatalf("order id 应为正，实际 %d", orderID)
	}
	order, err := g.db.ExecOrders().GetOrder(orderID)
	if err != nil {
		t.Fatalf("读执行订单失败: %v", err)
	}
	if order.State != db.ExecStateDraft {
		t.Errorf("新订单应为 draft，实际 %q", order.State)
	}
}

// TestGateEndToEndDenyPath 全链路拒绝路径：规则 deny → verdict 记 deny → 闸门拒。
func TestGateEndToEndDenyPath(t *testing.T) {
	g, done := gateTestEnv(t)
	defer done()
	resetToPaper(t, g)
	ensureDenyRule(t, g)

	acct := newGateTestAccount(t, g, "e2edeny")
	draftID := newGateTestDraft(t, g, "crypto")

	// 闸门应 deny
	res, err := g.Authorize(t.Context(), GateRequest{
		DraftID: draftID, AccountID: acct.ID, Mode: db.ExecModePaper,
	})
	if err != nil {
		t.Fatalf("闸门返回 error: %v", err)
	}
	// 2026-11-02 用户授权关闭风控规则拦截：deny 规则不再阻断。
	if res.Decision != DecisionAllow {
		t.Fatalf("风控拦截已关闭，应 allow，实际 %s", res.Decision)
	}

	// deny 也必须能落库（审计需要知道「为什么拒」）
	// 显式写 deny：这条测试要验的是「deny 判定也能落库」，
	// 不该依赖 res.Decision（2026-11-02 风控拦截关闭后它恒为 allow）。
	if _, err := g.db.InsertDraftRiskVerdict(db.DraftRiskVerdictInput{
		DraftID:       draftID,
		Verdict:       "deny",
		Reason:        "测试：验证 deny 判定可落库",
		RulesRevision: 1,
		RulesSnapshot: []byte(`{"fingerprint":"test"}`),
		EvaluatedBy:   "e2e-deny",
	}); err != nil {
		t.Fatalf("deny 落库失败: %v", err)
	}
	latest, err := g.db.LatestDraftRiskVerdict(draftID)
	if err != nil {
		t.Fatalf("读 verdict 失败: %v", err)
	}
	if latest.Verdict != "deny" {
		t.Errorf("verdict 应为 deny，实际 %q", latest.Verdict)
	}
	if latest.Reason == "" {
		t.Error("deny 必须记录原因（否则事后无法解释为什么拒）")
	}
}

// TestGateVerdictConsistency 两套记录一致性：写入后 orders_draft 必须同步。
//
// # 这条守的是什么
//
// 两套记录描述同一件事。不一致时以 verdicts 为准 —— 但那只是「读的时候以谁为准」，
// 写入时**必须同时写两边**，否则列表页会显示过期结论。
func TestGateVerdictConsistency(t *testing.T) {
	g, done := gateTestEnv(t)
	defer done()

	draftID := newGateTestDraft(t, g, "crypto")

	// 依次写 pass → deny，最后两套都应反映 deny
	for _, v := range []string{"pass", "deny"} {
		if _, err := g.db.InsertDraftRiskVerdict(db.DraftRiskVerdictInput{
			DraftID: draftID, Verdict: v, Reason: "一致性测试 " + v,
			RulesRevision: 1, RulesSnapshot: []byte(`{}`), EvaluatedBy: "test",
		}); err != nil {
			t.Fatalf("写 %s 失败: %v", v, err)
		}

		latest, err := g.db.LatestDraftRiskVerdict(draftID)
		if err != nil {
			t.Fatalf("读 verdict 失败: %v", err)
		}
		draft, err := g.db.OrderDrafts().GetDraft(draftID)
		if err != nil {
			t.Fatalf("读草案失败: %v", err)
		}
		if draft.RiskVerdict == nil {
			t.Fatalf("写 %s 后 orders_draft.risk_verdict 仍为 NULL", v)
		}
		if *draft.RiskVerdict != latest.Verdict {
			t.Errorf("两套记录不一致：orders_draft=%q，verdicts=%q",
				*draft.RiskVerdict, latest.Verdict)
		}
	}
}
