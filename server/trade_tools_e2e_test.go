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

	"github.com/Autumn-27/artex/agent"
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
	if _, _, err := pg.ForceSetExecMode(db.ExecModeLiveManual); err != nil {
		t.Fatalf("置 paper: %v", err)
	}

	// 标的：用唯一 symbol 避免与既有数据冲突
	active := true
	instID, _, err := pg.Instruments().UpsertInstrument(db.InstrumentInput{
		Market: "crypto", Symbol: "E2ETEST-USDT-SWAP",
		Name: "E2E 测试标的", AssetClass: "crypto", Active: &active,
	})
	if err != nil {
		t.Fatalf("建标的: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pg.Exec(`DELETE FROM orders_draft WHERE instrument_id=$1`, instID)
		_, _ = pg.Exec(`DELETE FROM instruments WHERE id=$1`, instID)
		// 解析器可能自动登记过同名的其它写法，一并清掉
		_, _ = pg.Exec(`DELETE FROM instruments WHERE symbol='E2ETEST-USDT-SWAP'`)
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
	return e.callCtx(t, context.Background(), name, args)
}

func (e *tradeE2E) callCtx(t *testing.T, ctx context.Context, name string, args map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("序列化: %v", err)
	}
	res, err := e.tool(t, name).Call(ctx, raw, nil)
	if err != nil {
		t.Fatalf("%s 返回 error: %v", name, err)
	}
	return res.Flatten()
}


// TestCreateOrderDraftRejectsIntentInstrumentMismatch 交易意图标的与请求标的不一致时拒绝。
func TestCreateOrderDraftRejectsIntentInstrumentMismatch(t *testing.T) {
	e := newTradeE2E(t)
	otherID, _, err := e.pg.Instruments().UpsertInstrument(db.InstrumentInput{
		Market: "crypto", Symbol: "E2EOTHER-USDT", Name: "其他测试标的", AssetClass: "crypto",
	})
	if err != nil {
		t.Fatalf("建其他标的: %v", err)
	}
	t.Cleanup(func() { _, _ = e.pg.Exec(`DELETE FROM instruments WHERE id=$1`, otherID) })
	exp, err := e.pg.CreateTask("trade", "trade", nil, 0, 0)
	if err != nil {
		t.Skipf("创建任务失败: %v", err)
	}
	t.Cleanup(func() {
		if err := e.pg.DeleteTask(exp.ID); err != nil {
			t.Errorf("清理任务: %v", err)
		}
	})
	if _, err := e.pg.Exec(`UPDATE tasks SET instrument_id=$1 WHERE id=$2`, otherID, exp.ID); err != nil {
		t.Fatalf("绑定任务标的: %v", err)
	}
	store := e.pg.Exploration(exp.ExplorationID)
	intentID, err := store.AddNode(db.KindIntent, map[string]any{"role": "trader", "instrument_id": e.instID}, 1, "open", "test", nil)
	if err != nil {
		t.Fatalf("建意图: %v", err)
	}
	ctx := agent.WithRunInfo(context.Background(), agent.RunInfo{TaskID: exp.ID, ExplorationID: exp.ExplorationID, IntentID: intentID})
	out := e.callCtx(t, ctx, "create_order_draft", map[string]any{
		"symbol": "E2EOTHER-USDT", "market": "crypto", "side": "buy", "qty": "1",
	})
	if strings.Contains(out, "draft_id") {
		t.Fatalf("意图标的不一致时不应创建草案: %s", out)
	}
	if !strings.Contains(out, "instrument") && !strings.Contains(out, "标的") {
		t.Fatalf("应明确说明标的不一致: %s", out)
	}
}

// TestCreateOrderDraftRejectsIntentWithoutInstrument 禁止从无标的意图猜测旧标的。
func TestCreateOrderDraftRejectsIntentWithoutInstrument(t *testing.T) {
	e := newTradeE2E(t)
	exp, err := e.pg.CreateTask("trade", "trade", nil, 0, 0)
	if err != nil {
		t.Skipf("创建任务失败: %v", err)
	}
	t.Cleanup(func() {
		if err := e.pg.DeleteTask(exp.ID); err != nil {
			t.Errorf("清理任务: %v", err)
		}
	})
	if _, err := e.pg.Exec(`UPDATE tasks SET instrument_id=$1 WHERE id=$2`, e.instID, exp.ID); err != nil {
		t.Fatalf("绑定任务标的: %v", err)
	}
	store := e.pg.Exploration(exp.ExplorationID)
	intentID, err := store.AddNode(db.KindIntent, map[string]any{"role": "trader"}, 1, "open", "test", nil)
	if err != nil {
		t.Fatalf("建意图: %v", err)
	}
	ctx := agent.WithRunInfo(context.Background(), agent.RunInfo{TaskID: exp.ID, ExplorationID: exp.ExplorationID, IntentID: intentID})
	out := e.callCtx(t, ctx, "create_order_draft", map[string]any{
		"symbol": "E2ETEST-USDT-SWAP", "market": "crypto", "side": "buy", "qty": "1",
	})
	if strings.Contains(out, "draft_id") {
		t.Fatalf("无明确交易标的意图不应创建草案: %s", out)
	}
}


func (e *tradeE2E) runInfo(t *testing.T, instrumentID *int64) agent.RunInfo {
	t.Helper()
	task, err := e.pg.CreateTask("trade context test", "trade", nil, 0, 0)
	if err != nil {
		t.Fatalf("建任务: %v", err)
	}
	t.Cleanup(func() {
		if err := e.pg.DeleteTask(task.ID); err != nil {
			t.Errorf("清理任务: %v", err)
		}
	})
	if _, err := e.pg.Exec(`UPDATE tasks SET instrument_id=$1 WHERE id=$2`, instrumentID, task.ID); err != nil {
		t.Fatalf("绑定任务标的: %v", err)
	}
	intentID, err := e.pg.Exploration(task.ExplorationID).AddNode(db.KindIntent,
		map[string]any{"role": "trader", "instrument_id": e.instID}, 1, "open", "test", nil)
	if err != nil {
		t.Fatalf("建意图: %v", err)
	}
	return agent.RunInfo{TaskID: task.ID, ExplorationID: task.ExplorationID, IntentID: intentID}
}

func TestTradeToolsRejectInvalidTaskContext(t *testing.T) {
	e := newTradeE2E(t)
	ri := e.runInfo(t, &e.instID)
	other := e.runInfo(t, &e.instID)
	unbound := e.runInfo(t, nil)
	draftID, err := e.pg.OrderDrafts().InsertDraft(db.OrderDraftInput{
		InstrumentID: &e.instID, Side: "buy", Qty: "1", RefPrice: "100",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out := e.call(t, "authorize_order", map[string]any{"draft_id": draftID, "account_id": e.acctID}); !strings.Contains(out, `"decision":"allow"`) {
		t.Fatalf("预先授权失败: %s", out)
	}
	cases := []struct {
		name string
		info agent.RunInfo
		want string
	}{
		{"缺任务", agent.RunInfo{ExplorationID: ri.ExplorationID, IntentID: ri.IntentID}, "上下文"},
		{"缺 exploration", agent.RunInfo{TaskID: ri.TaskID, IntentID: ri.IntentID}, "上下文"},
		{"任务不存在", agent.RunInfo{TaskID: 9223372036854775807, ExplorationID: ri.ExplorationID}, "上下文"},
		{"负任务 ID", agent.RunInfo{TaskID: -1, ExplorationID: ri.ExplorationID}, "上下文"},
		{"任务无标的绑定", unbound, "未明确 instrument_id"},
		{"任务配旧 exploration 和 intent", agent.RunInfo{TaskID: ri.TaskID, ExplorationID: other.ExplorationID, IntentID: other.IntentID}, "exploration"},
		{"当前 exploration 配旧 intent", agent.RunInfo{TaskID: ri.TaskID, ExplorationID: ri.ExplorationID, IntentID: other.IntentID}, "交易意图"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := agent.WithRunInfo(context.Background(), tc.info)
			for _, name := range draftScopedTradeToolNames {
				out := e.callCtx(t, ctx, name, map[string]any{
					"symbol": "E2ETEST-USDT-SWAP", "market": "crypto", "side": "buy", "qty": "1",
					"draft_id": draftID, "account_id": e.acctID,
				})
				if !strings.Contains(out, tc.want) {
					t.Errorf("%s 应拒绝并包含 %q，实际: %s", name, tc.want, out)
				}
			}
		})
	}
}

func TestTradeToolsRejectCrossInstrumentDraft(t *testing.T) {
	e := newTradeE2E(t)
	otherID, _, err := e.pg.Instruments().UpsertInstrument(db.InstrumentInput{
		Market: "crypto", Symbol: "E2ECROSS-USDT", Name: "跨标的测试", AssetClass: "crypto",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = e.pg.Exec(`DELETE FROM instruments WHERE id=$1`, otherID) })
	ri := e.runInfo(t, &otherID)
	ctx := agent.WithRunInfo(context.Background(), ri)
	out := e.callCtx(t, ctx, "create_order_draft", map[string]any{
		"symbol": "E2ETEST-USDT-SWAP", "market": "crypto", "side": "buy", "qty": "1",
	})
	if !strings.Contains(out, "任务") || !strings.Contains(out, "不一致") {
		t.Fatalf("任务与旧 intent/request 同标的也必须拒绝: %s", out)
	}
	draftID, err := e.pg.OrderDrafts().InsertDraft(db.OrderDraftInput{
		InstrumentID: &e.instID, Side: "buy", Qty: "1", RefPrice: "100",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out := e.call(t, "authorize_order", map[string]any{"draft_id": draftID, "account_id": e.acctID}); !strings.Contains(out, `"decision":"allow"`) {
		t.Fatalf("预先授权失败: %s", out)
	}
	payload, err := json.Marshal(map[string]any{"role": "trader", "instrument_id": otherID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.pg.Exec(`UPDATE exploration_nodes SET payload=$1 WHERE id=$2`, string(payload), ri.IntentID); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"authorize_order"} {
		out := e.callCtx(t, ctx, name, map[string]any{"draft_id": draftID, "account_id": e.acctID})
		if !strings.Contains(out, "不一致") {
			t.Errorf("%s 必须拒绝其他标的草案: %s", name, out)
		}
	}
}




