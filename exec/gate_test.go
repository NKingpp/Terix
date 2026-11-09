package exec

// WE-8 闸门的 12 个验收测试（§5.1）。
//
// 每个测试对应一条验收标准或一个设计决策。命名一律 TestGate<场景>。

import (
	"context"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// TestGateBlockedAccountDenies 账户被拉黑 → deny。
func TestGateBlockedAccountDenies(t *testing.T) {
	g, done := gateTestEnv(t)
	defer done()
	resetToPaper(t, g)

	acct := newGateTestAccount(t, g, "blocked2")
	if err := g.db.ExecAccounts().Block(acct.ID, "测试拉黑"); err != nil {
		t.Fatalf("拉黑失败: %v", err)
	}
	draftID := newGateTestDraft(t, g, "crypto")

	res, err := g.Authorize(t.Context(), GateRequest{
		DraftID: draftID, AccountID: acct.ID, Mode: db.ExecModePaper,
	})
	if err != nil {
		t.Fatalf("不该返回 error: %v", err)
	}
	if res.Decision != DecisionDeny {
		t.Errorf("被拉黑账户应 deny，实际 %s", res.Decision)
	}
	if res.ErrCode != CodeAccountBlocked {
		t.Errorf("错误码 = %q，期望 %q", res.ErrCode, CodeAccountBlocked)
	}
}

// TestGateFrozenModeDenies 全局 frozen → deny（含 paper）。
func TestGateFrozenModeDenies(t *testing.T) {
	g, done := gateTestEnv(t)
	defer done()

	acct := newGateTestAccount(t, g, "frozen")
	draftID := newGateTestDraft(t, g, "crypto")

	if _, err := g.db.TransitionMode(paperMode(), frozenMode()); err != nil {
		t.Fatalf("切 frozen 失败: %v", err)
	}
	t.Cleanup(func() { _, _, _ = g.db.ForceSetExecMode(paperMode()) })

	// 传入 frozen（与系统一致）→ 应在冻结检查处 deny
	res, err := g.Authorize(t.Context(), GateRequest{
		DraftID: draftID, AccountID: acct.ID, Mode: db.ExecModeFrozen,
	})
	if err != nil {
		t.Fatalf("不该返回 error: %v", err)
	}
	if res.Decision != DecisionDeny || res.ErrCode != CodeModeFrozen {
		t.Errorf("frozen 应 deny/%s，实际 %s/%s", CodeModeFrozen, res.Decision, res.ErrCode)
	}
}

// TestGateModeMismatchDenies 调用方传的模式与系统不一致 → deny。
//
// # 这条守的是「传一个更宽松的模式来绕过」
func TestGateModeMismatchDenies(t *testing.T) {
	g, done := gateTestEnv(t)
	defer done()
	resetToPaper(t, g)

	acct := newGateTestAccount(t, g, "mismatch")
	draftID := newGateTestDraft(t, g, "crypto")

	// 系统是 paper，但调用方说自己是 live_manual
	res, err := g.Authorize(t.Context(), GateRequest{
		DraftID: draftID, AccountID: acct.ID, Mode: db.ExecModeLiveManual,
	})
	if err != nil {
		t.Fatalf("不该返回 error: %v", err)
	}
	if res.Decision != DecisionDeny || res.ErrCode != CodeModeMismatch {
		t.Errorf("模式不一致应 deny/%s，实际 %s/%s", CodeModeMismatch, res.Decision, res.ErrCode)
	}
}

// TestGateLimitsIncompleteLiveDenies 限额不完整 + live_* → EXEC_LIMITS_INCOMPLETE。
func TestGateLimitsIncompleteLiveDenies(t *testing.T) {
	g, done := gateTestEnv(t)
	defer done()
	resetToPaper(t, g)
	ensureAllowPassRules(t, g)

	acct := newGateTestAccount(t, g, "nolimits")
	draftID := newGateTestDraft(t, g, "crypto")

	// 切到 live_manual
	if _, err := g.db.TransitionMode(paperMode(), liveManualMode()); err != nil {
		t.Fatalf("切 live 失败: %v", err)
	}
	t.Cleanup(func() { _, _, _ = g.db.ForceSetExecMode(paperMode()) })
	// 确保没有限额
	_, _ = g.db.Exec(`DELETE FROM execution_limits WHERE key='global'`)

	res, err := g.Authorize(t.Context(), GateRequest{
		DraftID: draftID, AccountID: acct.ID, Mode: db.ExecModeLiveManual,
	})
	if err != nil {
		t.Fatalf("不该返回 error: %v", err)
	}
	// 2026-11-02 用户授权关闭限额校验：现在应放行。
	// 原断言是 DecisionDeny + CodeLimitsIncomplete。
	if res.Decision != DecisionAllow {
		t.Errorf("限额校验已关闭，应 allow，实际 %s（%s）", res.Decision, res.Reason)
	}
}

// TestGateLimitsIncompletePaperAllows 限额不完整 + paper → 允许（paper 无真实风险）。
func TestGateLimitsIncompletePaperAllows(t *testing.T) {
	g, done := gateTestEnv(t)
	defer done()
	resetToPaper(t, g)
	ensureAllowPassRules(t, g)

	acct := newGateTestAccount(t, g, "papersafe")
	draftID := newGateTestDraft(t, g, "crypto")
	// 确保没有限额
	_, _ = g.db.Exec(`DELETE FROM execution_limits WHERE key='global'`)

	res, err := g.Authorize(t.Context(), GateRequest{
		DraftID: draftID, AccountID: acct.ID, Mode: db.ExecModePaper,
	})
	if err != nil {
		t.Fatalf("不该返回 error: %v", err)
	}
	if res.Decision != DecisionAllow {
		t.Errorf("paper 缺限额应 allow（纸面无真实风险），实际 %s（%s：%s）",
			res.Decision, res.ErrCode, res.Reason)
	}
}

// TestGateAshareWhitelistDenies A股 + live_whitelist → EXEC_ASHARE_AUTO_UNSUPPORTED。
func TestGateAshareWhitelistDenies(t *testing.T) {
	g, done := gateTestEnv(t)
	defer done()
	resetToPaper(t, g)
	ensureAllowPassRules(t, g)
	setLimits(t, g, "10000", "50000", 10)

	acct := newGateTestAccount(t, g, "ashare")
	draftID := newGateTestDraft(t, g, "cn")

	if _, err := g.db.TransitionMode(paperMode(), liveWhitelistMode()); err != nil {
		t.Fatalf("切 live_whitelist 失败: %v", err)
	}
	t.Cleanup(func() { _, _, _ = g.db.ForceSetExecMode(paperMode()) })

	res, err := g.Authorize(t.Context(), GateRequest{
		DraftID: draftID, AccountID: acct.ID, Mode: db.ExecModeLiveWhitelist,
	})
	if err != nil {
		t.Fatalf("不该返回 error: %v", err)
	}
	if res.Decision != DecisionDeny || res.ErrCode != CodeAshareAutoUnsupport {
		t.Errorf("A股+live_whitelist 应 deny/%s，实际 %s/%s（%s）",
			CodeAshareAutoUnsupport, res.Decision, res.ErrCode, res.Reason)
	}
}

// TestGateAshareManualAllows A股 + live_manual → 允许（人工逐笔确认可以）。
func TestGateAshareManualAllows(t *testing.T) {
	g, done := gateTestEnv(t)
	defer done()
	resetToPaper(t, g)
	ensureAllowPassRules(t, g)
	setLimits(t, g, "10000", "50000", 10)

	acct := newGateTestAccount(t, g, "ashare2")
	draftID := newGateTestDraft(t, g, "cn")

	if _, err := g.db.TransitionMode(paperMode(), liveManualMode()); err != nil {
		t.Fatalf("切 live_manual 失败: %v", err)
	}
	t.Cleanup(func() { _, _, _ = g.db.ForceSetExecMode(paperMode()) })

	res, err := g.Authorize(t.Context(), GateRequest{
		DraftID: draftID, AccountID: acct.ID, Mode: db.ExecModeLiveManual,
	})
	if err != nil {
		t.Fatalf("不该返回 error: %v", err)
	}
	if res.Decision != DecisionAllow {
		t.Errorf("A股+live_manual 应 allow，实际 %s（%s：%s）", res.Decision, res.ErrCode, res.Reason)
	}
}

// TestGateAsharePaperAllows A股 + paper → 允许。
func TestGateAsharePaperAllows(t *testing.T) {
	g, done := gateTestEnv(t)
	defer done()
	resetToPaper(t, g)
	ensureAllowPassRules(t, g)

	acct := newGateTestAccount(t, g, "ashare3")
	draftID := newGateTestDraft(t, g, "cn")

	res, err := g.Authorize(t.Context(), GateRequest{
		DraftID: draftID, AccountID: acct.ID, Mode: db.ExecModePaper,
	})
	if err != nil {
		t.Fatalf("不该返回 error: %v", err)
	}
	if res.Decision != DecisionAllow {
		t.Errorf("A股+paper 应 allow，实际 %s（%s：%s）", res.Decision, res.ErrCode, res.Reason)
	}
}

// TestGateRulesRevisionChangeAsks 规则变更后结论可能翻转 → ask（不沿用旧结论）。
func TestGateRulesRevisionChangeAsks(t *testing.T) {
	g, done := gateTestEnv(t)
	defer done()
	resetToPaper(t, g)
	ensureAllowPassRules(t, g)

	acct := newGateTestAccount(t, g, "ruleschanged")
	draftID := newGateTestDraft(t, g, "crypto")

	// 第一次求值，拿到当前指纹
	fp1, err := g.rulesFingerprint()
	if err != nil {
		t.Fatalf("算指纹失败: %v", err)
	}
	// 手动写一条历史记录（模拟「草案创建时求过一次值」）
	snap1 := `{"fingerprint":"` + fp1 + `","limits_revision":1}`
	if _, err := g.db.InsertDraftRiskVerdict(db.DraftRiskVerdictInput{
		DraftID: draftID, Verdict: "pass", Reason: "首次求值",
		RulesRevision: 1, RulesSnapshot: []byte(snap1), EvaluatedBy: "test",
	}); err != nil {
		t.Fatalf("写历史记录失败: %v", err)
	}

	// 改规则（新建一条 enabled 规则 → 指纹变）
	if _, err := g.db.CreateInterceptRule(
		"闸门测试-变更后", "draft_field", "numeric_lte",
		"999999999999", "deny", "变更后的规则", 5, true, false, 60, "deny",
	); err != nil {
		t.Fatalf("建规则失败: %v", err)
	}
	g.rules.Invalidate()
	t.Cleanup(func() {
		_, _ = g.db.Exec(`DELETE FROM intercept_rules WHERE name='闸门测试-变更后'`)
		g.rules.Invalidate()
	})

	fp2, _ := g.rulesFingerprint()
	if fp1 == fp2 {
		t.Fatal("测试前提不成立：改规则后指纹应变化")
	}

	res, err := g.Authorize(t.Context(), GateRequest{
		DraftID: draftID, AccountID: acct.ID, Mode: db.ExecModePaper,
	})
	if err != nil {
		t.Fatalf("不该返回 error: %v", err)
	}
	if res.Decision != DecisionAsk || res.ErrCode != CodeRulesChanged {
		t.Errorf("规则变更应 ask/%s，实际 %s/%s（%s）",
			CodeRulesChanged, res.Decision, res.ErrCode, res.Reason)
	}
}

// TestGateSingleAmountExceededDenies 单笔金额超限 → deny。
func TestGateSingleAmountExceededDenies(t *testing.T) {
	g, done := gateTestEnv(t)
	defer done()
	resetToPaper(t, g)
	ensureAllowPassRules(t, g)
	setLimits(t, g, "100", "100000", 100)

	acct := newGateTestAccount(t, g, "single")
	// qty=10 × price=100 = 1000 > 单笔上限 100
	draftID := newGateTestDraftWithPrice(t, g, "crypto", "10", "100")

	res, err := g.Authorize(t.Context(), GateRequest{
		DraftID: draftID, AccountID: acct.ID, Mode: db.ExecModePaper,
	})
	if err != nil {
		t.Fatalf("不该返回 error: %v", err)
	}
	// 2026-11-02 用户授权关闭硬上限校验：单笔超限不再拦截。
	if res.Decision != DecisionAllow {
		t.Errorf("硬上限已关闭，单笔超限应 allow，实际 %s（%s）", res.Decision, res.Reason)
	}
}

// TestGateDailyAmountExceededDenies 当日累计超限 → deny（含已提交未成交）。
//
// # 这条是 TOCTOU 的核心
//
// 已提交未成交的订单**已占用额度**。不计入的话，用户可以在同一秒
// 提交 N 笔，每笔单独看都不超限，合起来超 N 倍。
func TestGateDailyAmountExceededDenies(t *testing.T) {
	g, done := gateTestEnv(t)
	defer done()
	resetToPaper(t, g)
	ensureAllowPassRules(t, g)
	// 单笔上限很宽（10000），日上限 500 —— 让「单笔通过但累计超限」
	setLimits(t, g, "10000", "500", 100)

	acct := newGateTestAccount(t, g, "daily")

	// 先造一笔「已提交未成交」400 元的订单
	prevDraft := newGateTestDraftWithPrice(t, g, "crypto", "4", "100") // 400
	_ = newGateTestOrder(t, g, acct.ID, prevDraft, db.ExecStateSubmitted, "4", "0", "")

	// 本笔 200 元（单笔没超 10000，但 400+200=600 > 500）
	draftID := newGateTestDraftWithPrice(t, g, "crypto", "2", "100")

	res, err := g.Authorize(t.Context(), GateRequest{
		DraftID: draftID, AccountID: acct.ID, Mode: db.ExecModePaper,
	})
	if err != nil {
		t.Fatalf("不该返回 error: %v", err)
	}
	// 2026-11-02 用户授权关闭硬上限校验：当日累计超限不再拦截。
	if res.Decision != DecisionAllow {
		t.Errorf("硬上限已关闭，当日累计超限应 allow，实际 %s（%s）", res.Decision, res.Reason)
	}
}

// TestGateDailyCountExceededDenies 当日笔数超限 → deny。
func TestGateDailyCountExceededDenies(t *testing.T) {
	g, done := gateTestEnv(t)
	defer done()
	resetToPaper(t, g)
	ensureAllowPassRules(t, g)
	// 日单数上限 2
	setLimits(t, g, "100000", "1000000", 2)

	acct := newGateTestAccount(t, g, "count")

	// 先造 2 笔已提交的
	for i := 0; i < 2; i++ {
		pd := newGateTestDraftWithPrice(t, g, "crypto", "1", "10")
		_ = newGateTestOrder(t, g, acct.ID, pd, db.ExecStateSubmitted, "1", "0", "")
	}

	// 第 3 笔应被拒
	draftID := newGateTestDraftWithPrice(t, g, "crypto", "1", "10")
	res, err := g.Authorize(t.Context(), GateRequest{
		DraftID: draftID, AccountID: acct.ID, Mode: db.ExecModePaper,
	})
	if err != nil {
		t.Fatalf("不该返回 error: %v", err)
	}
	// 2026-11-02 用户授权关闭硬上限校验：当日笔数不再拦截。
	if res.Decision != DecisionAllow {
		t.Errorf("硬上限已关闭，当日笔数超限应 allow，实际 %s（%s）", res.Decision, res.Reason)
	}
}

// TestGateDailyNotionalUnknownDenies 有无法估值的订单 → deny（不是当作 0）。
//
// # 这条守的是「不知道怎么算 ≠ 算出来是 0」
func TestGateDailyNotionalUnknownDenies(t *testing.T) {
	g, done := gateTestEnv(t)
	defer done()
	resetToPaper(t, g)
	ensureAllowPassRules(t, g)
	setLimits(t, g, "10000", "50000", 100)

	acct := newGateTestAccount(t, g, "unpriced")

	// 造一笔 ref_price 为 NULL（市价单）且未成交的订单 → 无法估值
	pd := newGateTestDraftWithPrice(t, g, "crypto", "5", "") // ref_price = NULL
	_ = newGateTestOrder(t, g, acct.ID, pd, db.ExecStateSubmitted, "5", "0", "")

	draftID := newGateTestDraftWithPrice(t, g, "crypto", "1", "10")
	res, err := g.Authorize(t.Context(), GateRequest{
		DraftID: draftID, AccountID: acct.ID, Mode: db.ExecModePaper,
	})
	if err != nil {
		t.Fatalf("不该返回 error: %v", err)
	}
	// 2026-11-02 用户授权关闭硬上限校验：无法估值也不再拦截。
	//
	// ⚠️ 注意这条的原始意图是「不知道怎么算 ≠ 算出来是 0」——那是个好原则，
	// 但今天起这个判断不再影响放行。若要恢复，改回 gate.go 第 11 步即可。
	if res.Decision != DecisionAllow {
		t.Errorf("硬上限已关闭，应 allow，实际 %s（%s）", res.Decision, res.Reason)
	}
}

// TestGateDraftIncompleteDenies 草案字段不齐 → EXEC_DRAFT_INCOMPLETE。
func TestGateDraftIncompleteDenies(t *testing.T) {
	g, done := gateTestEnv(t)
	defer done()
	resetToPaper(t, g)

	acct := newGateTestAccount(t, g, "incomplete")
	// 建一条 side 为空、instrument 为空的草案（模拟「还没填完」）
	var id int64
	if err := g.db.QueryRow(`INSERT INTO orders_draft (side, qty) VALUES ('buy', 1) RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("建草案失败: %v", err)
	}
	t.Cleanup(func() { _, _ = g.db.Exec(`DELETE FROM orders_draft WHERE id=$1`, id) })

	res, err := g.Authorize(t.Context(), GateRequest{
		DraftID: id, AccountID: acct.ID, Mode: db.ExecModePaper,
	})
	if err != nil {
		t.Fatalf("不该返回 error: %v", err)
	}
	if res.Decision != DecisionDeny {
		t.Errorf("草案不完整应 deny，实际 %s", res.Decision)
	}
	if res.ErrCode != CodeDraftIncomplete && res.ErrCode != CodeDraftCorrupt {
		t.Errorf("错误码应为 DRAFT_INCOMPLETE 或 DRAFT_CORRUPT，实际 %q", res.ErrCode)
	}
}

// TestGateRuleDenyShortCircuits 规则 deny 时直接返回，不再做后续校验。
func TestGateRuleDenyShortCircuits(t *testing.T) {
	g, done := gateTestEnv(t)
	defer done()
	resetToPaper(t, g)
	ensureDenyRule(t, g)
	setLimits(t, g, "10000", "50000", 100)

	acct := newGateTestAccount(t, g, "ruledeny")
	draftID := newGateTestDraft(t, g, "crypto")

	res, err := g.Authorize(t.Context(), GateRequest{
		DraftID: draftID, AccountID: acct.ID, Mode: db.ExecModePaper,
	})
	if err != nil {
		t.Fatalf("不该返回 error: %v", err)
	}
	// 2026-11-02 用户授权关闭风控规则拦截：deny 规则不再阻断放行。
	//
	// 规则仍会被求值（结果可用于审计），但不再改变 Decision。
	if res.Decision != DecisionAllow {
		t.Errorf("风控规则拦截已关闭，应 allow，实际 %s（%s）", res.Decision, res.Reason)
	}
}

// resetToPaper 把模式重置为 paper。
func resetToPaper(t *testing.T, g *Gate) {
	t.Helper()
	_, _, err := g.db.ForceSetExecMode(paperMode())
	if err != nil {
		t.Fatalf("重置为 paper 失败: %v", err)
	}
}

// 保证 context 被使用（部分测试用 t.Context()，这里留一个兜底）
var _ = context.Background
