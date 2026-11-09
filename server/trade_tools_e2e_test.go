package server

// 交易工具的端到端验证：建草案 → 过闸 → 执行。
//
// # 这组测试验的是「真的走了 Gate」
//
// 前面 trade_tools_test.go 验的是注册与入参校验（静态）。
// 这里跑真实链路：写库、Gate.Authorize 判定、paper 记账。
//
// 用户的要求是「通过工具实现下单，否则后面无法进行拦截」——
// 所以证据必须是「草案过闸才执行」，而不只是「工具存在」。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/exec"
	actool "github.com/Autumn-27/norma/tool"
)

// tradeE2E 准备一个可跑交易链的环境。
type tradeE2E struct {
	pg      *db.DB
	tts     *TradeToolSet
	draftID int64
	acctID  int64
	instID  int64
}

// newTradeE2E 建标的 + 账户，强制 paper 模式。
func newTradeE2E(t *testing.T) *tradeE2E {
	t.Helper()
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	t.Cleanup(func() { m.Close() })

	pg := m.pg
	if _, _, err := pg.ForceSetExecMode(db.ExecModePaper); err != nil {
		t.Fatalf("置 paper: %v", err)
	}

	// 标的：用唯一 symbol 避免与既有数据冲突
	active := true
	instID, _, err := pg.Instruments().UpsertInstrument(db.InstrumentInput{
		Market: "crypto", Symbol: "E2ETEST-USDT",
		Name: "E2E 测试标的", AssetClass: "crypto", Active: &active,
	})
	if err != nil {
		t.Fatalf("建标的: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pg.Exec(`DELETE FROM orders_draft WHERE instrument_id=$1`, instID)
		_, _ = pg.Exec(`DELETE FROM instruments WHERE id=$1`, instID)
	})

	// 账户
	acct, err := pg.ExecAccounts().UpsertAccount(db.ExecAccountInput{
		Channel: "okx", ChannelAcctID: "e2e-trade-acct",
		Label: "E2E", Market: "crypto", MarketSupport: []string{"crypto"}, Active: true,
	})
	if err != nil {
		t.Fatalf("建账户: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pg.Exec(`DELETE FROM paper_orders WHERE account_id=$1`, acct.ID)
		_, _ = pg.Exec(`DELETE FROM exec_accounts WHERE id=$1`, acct.ID)
	})

	return &tradeE2E{
		pg: pg, tts: NewTradeToolSet(pg, exec.NewGate(pg, nil)),
		acctID: acct.ID, instID: instID,
	}
}

// tool 按名字取工具。
func (e *tradeE2E) tool(t *testing.T, name string) actool.CoreTool {
	t.Helper()
	for _, tl := range e.tts.Tools() {
		if tl.Name() == name {
			return tl
		}
	}
	t.Fatalf("找不到工具 %s", name)
	return nil
}

// call 调用工具，返回 flatten 后的文本。
func (e *tradeE2E) call(t *testing.T, name string, args map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("序列化: %v", err)
	}
	res, err := e.tool(t, name).Call(context.Background(), raw, nil)
	if err != nil {
		t.Fatalf("%s 返回 error: %v", name, err)
	}
	return res.Flatten()
}

// TestTradeChainCreateAuthExecute 完整链路：草案 → 过闸 → paper 执行。
//
// 这是「下单能被拦截」的正面证据：三步都成功，且 paper_orders 有记录。
func TestTradeChainCreateAuthExecute(t *testing.T) {
	e := newTradeE2E(t)

	// ① 建草案
	out := e.call(t, "create_order_draft", map[string]any{
		"symbol": "E2ETEST-USDT", "market": "crypto",
		"side": "buy", "qty": "1", "ref_price": "100",
	})
	if !strings.Contains(out, "draft_id") {
		t.Fatalf("建草案失败，返回: %s", out)
	}
	var cr struct {
		DraftID int64 `json:"draft_id"`
	}
	if err := json.Unmarshal([]byte(out), &cr); err != nil {
		t.Fatalf("解析 draft_id: %v (out=%s)", err, out)
	}
	if cr.DraftID <= 0 {
		t.Fatalf("draft_id 无效: %d", cr.DraftID)
	}

	// ② 过闸
	authOut := e.call(t, "authorize_order", map[string]any{
		"draft_id": cr.DraftID, "account_id": e.acctID,
	})
	if !strings.Contains(authOut, "allow") {
		t.Fatalf("闸门应 allow，实际: %s", authOut)
	}

	// ③ paper 执行
	execOut := e.call(t, "execute_paper_order", map[string]any{
		"draft_id": cr.DraftID, "account_id": e.acctID,
	})
	if !strings.Contains(execOut, "paper_order_id") {
		t.Fatalf("paper 执行失败，返回: %s", execOut)
	}

	// ④ 账本必须有记录
	var n int
	if err := e.pg.QueryRow(
		`SELECT count(*) FROM paper_orders WHERE account_id=$1`, e.acctID,
	).Scan(&n); err != nil {
		t.Fatalf("查 paper_orders: %v", err)
	}
	if n == 0 {
		t.Error("工具报告执行成功，但 paper_orders 没有记录 —— 说明没有真正走 Gate.ExecutePaper")
	}

	// ⑤ 闸门必须留下判定记录（这是「拦截可追溯」的证据）
	//
	// ExecutePaper 的前置条件之一就是 draft_risk_verdicts 最新一条 == allow。
	// 所以这里断言它不是空 —— 空表说明 authorize_order 没有真正写记录，
	// 而 agent 仍能执行（那意味着闸门被绕过）。
	var verdict string
	qErr := e.pg.QueryRow(
		`SELECT verdict FROM draft_risk_verdicts WHERE draft_id=$1
		 ORDER BY id DESC LIMIT 1`, cr.DraftID,
	).Scan(&verdict)
	if qErr != nil {
		t.Fatalf("查 draft_risk_verdicts: %v（空表说明 authorize_order 没写判定记录，闸门形同虚设）", qErr)
	}
	// 注意：db 层把 GateDecision 的 "allow" 归一化成表里的 "pass"
	// （见 db/draft_risk_verdicts.go:48 —— intercept 用 allow，表里用 pass）。
	// 所以这里断言 "pass" 而不是 "allow"，别被字面差异误导。
	if verdict != "pass" {
		t.Errorf("判定记录 verdict = %q, want pass（allow 归一化成 pass，见 db 层）", verdict)
	}
}

// TestTradeExecuteRequiresAuthorization 未过闸就执行必须失败。
//
// # 这条是「拦截」的核心断言
//
// 如果 execute_paper_order 能在没有 draft_risk_verdicts 的情况下执行，
// 那么闸门形同虚设 —— agent 可以跳过 authorize_order 直接下单。
func TestTradeExecuteRequiresAuthorization(t *testing.T) {
	e := newTradeE2E(t)

	// 直接建草案，**不过闸**
	draftID, err := e.pg.OrderDrafts().InsertDraft(db.OrderDraftInput{
		InstrumentID: &e.instID, Side: "buy", Qty: "1", RefPrice: "100",
	})
	if err != nil {
		t.Fatalf("建草案: %v", err)
	}

	out := e.call(t, "execute_paper_order", map[string]any{
		"draft_id": draftID, "account_id": e.acctID,
	})
	// 应失败（返回错误文本，不含 paper_order_id）
	if strings.Contains(out, "paper_order_id") {
		t.Fatalf("未过闸的草案不应执行成功，实际: %s", out)
	}
}

// TestTradeToolsRejectInvalidIDs 无效 ID 要在碰库之前被拦住。
func TestTradeToolsRejectInvalidIDs(t *testing.T) {
	e := newTradeE2E(t)

	for _, name := range []string{"authorize_order", "execute_paper_order"} {
		out := e.call(t, name, map[string]any{"draft_id": 0, "account_id": 0})
		if !strings.Contains(out, "无效") {
			t.Errorf("%s 对无效 ID 应提示「无效」，实际: %s", name, out)
		}
	}
}

// TestTradeExecuteRefusesLive paper 工具在 live 模式下必须拒绝。
//
// # 为什么不静默降级
//
// 若 live 模式下 execute_paper_order 悄悄记一笔纸面账，
// agent 会以为「已下单」而实际市场什么都没发生 —— 那是最危险的假成功。
func TestTradeExecuteRefusesLive(t *testing.T) {
	e := newTradeE2E(t)
	t.Cleanup(func() { _, _, _ = e.pg.ForceSetExecMode(db.ExecModePaper) })

	if _, _, err := e.pg.ForceSetExecMode(db.ExecModeLiveManual); err != nil {
		t.Fatalf("置 live: %v", err)
	}

	draftID, err := e.pg.OrderDrafts().InsertDraft(db.OrderDraftInput{
		InstrumentID: &e.instID, Side: "buy", Qty: "1", RefPrice: "100",
	})
	if err != nil {
		t.Fatalf("建草案: %v", err)
	}

	out := e.call(t, "execute_paper_order", map[string]any{
		"draft_id": draftID, "account_id": e.acctID,
	})
	if strings.Contains(out, "paper_order_id") {
		t.Fatalf("live 模式下不该执行 paper 单，实际: %s", out)
	}
	if !strings.Contains(out, "只执行 paper") {
		t.Errorf("应明确说明只执行 paper，实际: %s", out)
	}
}
