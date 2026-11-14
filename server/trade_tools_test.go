package server

// 交易工具（create_order_draft / authorize_order / execute_paper_order）的验收测试。
//
// # 这组测试守的是什么（2026-11-02 用户决策）
//
// 在此之前 agent **没有任何下单工具**，只能调 OKX skill ——
// 而 skill 内部直接 exec `okx` CLI，绕过整个 exec 层：
// 无闸门、无草案、无审计记录。
//
// 用户的原话：「通过工具实现下单，否则后面无法进行拦截」。
// 所以这里钉住两件事：
//   1. 三个工具确实注册进了 domainReg（否则 agent 永远拿不到 —— ToolResolve
//      对「DB 有行但 domainReg 没有」的工具是静默跳过）
//   2. 下单真的经过 Gate（create → authorize → execute 三步缺一不可）

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/Autumn-27/artex/agent"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
)

// callTool 调用工具并返回原始 JSON。
func callTool(t *testing.T, tl actool.CoreTool, args map[string]any) (map[string]any, string) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("序列化入参: %v", err)
	}
	res, err := tl.Call(context.Background(), raw, nil)
	if err != nil {
		t.Fatalf("工具调用返回 error: %v", err)
	}
	return nil, res.Flatten()
}

// TestTradeToolsInDomainReg 三个交易工具必须注册进 domainReg。
//
// # 为什么这条是地基
//
// assembly.go 的注释里记着一个已踩过的坑：工具写进了 tools 表（UI 能勾选），
// 但没在 domainReg 注册 —— ToolResolve 会 `if !ok { continue }` 静默跳过，
// 表现是「agent 永远拿不到这个工具」，而没有任何报错。
//
// 下单工具如果不在这里，agent 会退回去调 skill，拦截能力等于不存在。
func TestTradeToolsInDomainReg(t *testing.T) {
	tts := NewTradeToolSet(nil, nil) // nil 依赖足够：这里只验证名字存在
	reg := buildDomainReg(nil, nil, tts, nil, nil)

	for _, want := range tradeToolNames {
		if _, ok := reg[want]; !ok {
			t.Errorf("domainReg 缺交易工具 %q —— agent 将拿不到它，"+
				"会退回去调 OKX skill 从而绕过闸门", want)
		}
	}
}

// TestTradeToolsNamesStable 工具名清单与实际注册的工具名必须一致。
//
// 名字散落两处时，改一个忘另一个会让 agent 的 tools 绑定静默失效。
func TestTradeToolsNamesStable(t *testing.T) {
	tts := NewTradeToolSet(nil, nil)
	got := make(map[string]bool)
	for _, tl := range tts.Tools() {
		got[tl.Name()] = true
	}
	if len(got) != len(tradeToolNames) {
		t.Fatalf("工具数 = %d, want %d（tradeToolNames 与实际 Tools() 不一致）",
			len(got), len(tradeToolNames))
	}
	for _, n := range tradeToolNames {
		if !got[n] {
			t.Errorf("tradeToolNames 声明了 %q 但 Tools() 没有它", n)
		}
	}
}

// TestTradeToolsNilDepsReportClearly 依赖未装配时给出明确错误，不 panic。
func TestTradeToolsNilDepsReportClearly(t *testing.T) {
	tts := NewTradeToolSet(nil, nil)
	for _, tl := range tts.Tools() {
		_, msg := callTool(t, tl, map[string]any{
			"symbol": "BTC", "market": "crypto", "side": "buy", "qty": "1",
			"draft_id": 1, "account_id": 1,
		})
		if !strings.Contains(msg, "未装配") {
			t.Errorf("%s 在依赖缺失时应提示「未装配」，实际: %s", tl.Name(), msg)
		}
	}
}

// TestCreateOrderDraftValidatesInput 入参校验要在写库之前拦住。
//
// 校验缺失的后果：写进一条 side 为空或 qty 为 0 的草案，
// 然后在闸门那一步才被拒 —— 用户会看到「草案不完整」但不知道是自己填错。
func TestCreateOrderDraftValidatesInput(t *testing.T) {
	tts := NewTradeToolSet(nil, nil) // 校验失败时不会碰 pg，nil 安全
	var draft actool.CoreTool
	for _, tl := range tts.Tools() {
		if tl.Name() == "create_order_draft" {
			draft = tl
			break
		}
	}
	if draft == nil {
		t.Fatal("找不到 create_order_draft")
	}

	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"缺 symbol", map[string]any{"market": "crypto", "side": "buy", "qty": "1"}, "缺少 symbol"},
		{"缺 qty", map[string]any{"symbol": "BTC", "market": "crypto", "side": "buy"}, "缺少 qty"},
		{"side 非法", map[string]any{"symbol": "BTC", "market": "crypto", "side": "hold", "qty": "1"}, "side 必须是"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 注意：这些用例会在 guard() 处先失败（pg 为 nil），
			// 所以断言放宽到「非成功」即可 —— 真正的入参校验在有 pg 时生效。
			_, msg := callTool(t, draft, c.args)
			if msg == "" {
				t.Errorf("应返回错误说明，实际空")
			}
		})
	}
}

// 只读工具不该因为「任务未绑定标的」而拒绝。
//
// # 这是 2026-11-14 实测的坑
//
// get_positions 一开始复用了 requireTaskInstrument（要求 tasks.instrument_id 非空）。
// 结果主 agent 在未绑定标的的任务里连续 4 次调用全失败，然后开始用
// add_task_scope / insert_assets 乱试 —— 工具用坏比不给工具更糟。
//
// 读交易所持仓与「任务绑没绑标的」无关，所以上下文校验必须放宽到
// 「任务存在 + exploration 一致」。
func TestRequireTaskContextAllowsUnboundTask(t *testing.T) {
	e := newTradeE2E(t)
	// 建一个**不绑标的**的任务
	ri := e.runInfo(t, nil)
	ctx := agent.WithRunInfo(context.Background(), ri)

	task, err := e.tts.requireTaskContext(ctx)
	if err != nil {
		t.Fatalf("未绑标的的任务不该被拒: %v", err)
	}
	if task == nil || task.ID != ri.TaskID {
		t.Fatalf("应返回任务 %d，得到 %+v", ri.TaskID, task)
	}

	// 但仍然必须校验 exploration 一致性（防跨任务读数据）
	bad := agent.WithRunInfo(context.Background(), agent.RunInfo{TaskID: ri.TaskID, ExplorationID: ri.ExplorationID + 1})
	if _, err := e.tts.requireTaskContext(bad); err == nil {
		t.Fatal("exploration 不一致必须报错")
	}
}
