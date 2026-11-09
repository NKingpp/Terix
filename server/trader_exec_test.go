package server

// 交易员「建草案 + 过闸」链路的验收测试（2026-11-06）。
//
// # 事故
//
// 全流程跑到 trader 后它拒绝下单：
//
//	"当前执行环境无 OKX demo 下单通道（无 shell/无 OKX API 凭证/浏览器）"
//	"无法下单，已如实写回"
//
// 但工具其实可用（create_order_draft / authorize_order 都 deferred=false、
// 都绑给了 trader）。真正原因是 prompt 写着「你唯一的职责是产出交易信号」
// 且「本次为模拟盘…你的输出不会被真的执行」—— 工具与提示自相矛盾。
//
// 修法两条：
//  1. prompt 改成「三项产出：信号 + 草案 + 过闸」
//  2. 系统注入 account_id（authorize_order 的必填），否则 trader 只能放弃

import (
	"context"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/agent"
)

// TestExecAccountFromRoundTrip ctx 注入/读取对称。
func TestExecAccountFromRoundTrip(t *testing.T) {
	ctx := context.Background()
	if _, ok := agent.ExecAccountFrom(ctx); ok {
		t.Fatal("空 ctx 不该有账户")
	}

	ctx = agent.WithExecAccount(ctx, agent.ExecAccount{ID: 42, Ref: "demo", MinOrderAmount: 10})
	acct, ok := agent.ExecAccountFrom(ctx)
	if !ok {
		t.Fatal("注入后应能取回")
	}
	if acct.ID != 42 || acct.Ref != "demo" || acct.MinOrderAmount != 10 {
		t.Errorf("账户字段不符: %+v", acct)
	}
}

// TestWithExecAccountIgnoresZeroID 零 ID 等于不注入。
//
// # 为什么重要
//
// 装配点在没有 active 账户时传零值。若零值被注入，trader 会拿
// account_id=0 去调 authorize_order —— 那会返回「account_id 无效」，
// 而 prompt 里 {{if .AccountID}} 判断依赖「零值 = 未配置」的语义。
func TestWithExecAccountIgnoresZeroID(t *testing.T) {
	ctx := agent.WithExecAccount(context.Background(), agent.ExecAccount{ID: 0})
	if _, ok := agent.ExecAccountFrom(ctx); ok {
		t.Error("零 ID 不该被注入")
	}
}

// TestTraderPromptRequiresDraftAndAuthorize trader 的 prompt 必须要求建草案。
//
// # 这是本次修复的核心断言
//
// 改之前 prompt 写「你唯一的职责是产出交易信号」，模型照做了，
// 于是永远不下单。这条测试锁住「prompt 必须提到这两个工具」。
func TestTraderPromptRequiresDraftAndAuthorize(t *testing.T) {
	body := agent.BuiltinPromptSeeds()["trader"]
	if body == "" {
		t.Fatal("trader prompt 缺失")
	}
	for _, want := range []string{"create_order_draft", "authorize_order", "draft_id", "account_id"} {
		if !strings.Contains(body, want) {
			t.Errorf("trader prompt 应提到 %q（否则模型不会调用它）", want)
		}
	}
}

// TestTraderPromptExplainsQtyIsBaseCcy prompt 必须说清 qty 是币量不是金额。
//
// # 为什么
//
// 草案的 qty 是币的数量。模型若把「10 USDT」填进 qty，
// 实际会买入 10 BTC（约 78 万 USDT）—— 在实盘是灾难性的。
//
// 限价单会被限额挡住，但市价单可能直接成交。
func TestTraderPromptExplainsQtyIsBaseCcy(t *testing.T) {
	body := agent.BuiltinPromptSeeds()["trader"]
	if !strings.Contains(body, "币的数量") {
		t.Error("trader prompt 必须明确 qty 是币的数量（不是金额）——否则会买错数量级")
	}
}

// TestPlannerPromptForbidsDirectOrdering planner 不该自己派「去下单」的意图。
//
// # 事故（2026-11-06 任务 36213）
//
// planner 建了一条无 role 的意图：
//
//	"在 OKX 模拟盘对 BTC-USDT 提交一笔市价买入单"
//
// 它落到普通 worker 手里（无 role → agentKey=worker），而 worker 没有
// 下单工具，于是：
//
//	work#3 ~ "no CLI or Bash tool available—only Read, LS, and web search"
//	work#3 ~ SearchExtraTools {"query":"okx trade order demo paper credential api place buy"} → 无匹配
//	work#3 ~ 反复空转十几轮 → 写「无下单通道」fact
//
// 真正的下单工具绑在 trader 角色上。planner 派这条意图既无效又阻塞链路。
func TestPlannerPromptForbidsDirectOrdering(t *testing.T) {
	body := agent.BuiltinPromptSeeds()["planner"]
	if body == "" {
		t.Fatal("planner prompt 缺失")
	}
	// 必须明确"不要派下单意图"，并点名 trader 是执行方。
	for _, want := range []string{"执行动作不归你", "create_order_draft", "trader"} {
		if !strings.Contains(body, want) {
			t.Errorf("planner prompt 应含 %q（否则它会自己派下单意图，白烧一轮）", want)
		}
	}
}

// TestPlannerPromptForbidsAccountBalanceIntent 「查账户余额」也不该派。
//
// 账户信息由执行层提供，分析阶段拿不到 —— 派了必然空转。
func TestPlannerPromptForbidsAccountBalanceIntent(t *testing.T) {
	body := agent.BuiltinPromptSeeds()["planner"]
	if !strings.Contains(body, "查账户余额") {
		t.Error("planner prompt 应明确「不要派查账户余额的意图」")
	}
}

// TestPlannerPromptForbidsVacuousGoalClosure planner 不能用「空真」标目标达成。
//
// # 事故（2026-11-06 任务 36215）
//
// 目标是「给出明确建议；若建议做多则执行 10 USDT 买入」。
// research_manager 还在跑、裁决尚未产出时，planner 就调了 prove_goal：
//
//	"该目标条件为「若结论为做多」，当前综合 stance=bearish，做多条件不成立，
//	 不触发买入动作，目标条件式成立（空真）"
//
// 后果链：
//
//	目标标 met → 系统取消所有运行中 worker（research_manager 停在 stopped）
//	→ trader 永不启动 → 一笔单都没下，任务却「完成」了
//
// 空真在命题逻辑里为真，但在任务语义里是偷懒：用户要「分析完再决定」，
// 不是「不分析就声明无需行动」。
func TestPlannerPromptForbidsVacuousGoalClosure(t *testing.T) {
	body := agent.BuiltinPromptSeeds()["planner"]
	if body == "" {
		t.Fatal("planner prompt 缺失")
	}
	for _, want := range []string{"空真", "若 X 则 Y", "是否已经被实际检验过"} {
		if !strings.Contains(body, want) {
			t.Errorf("planner prompt 应含 %q（否则会用过早判定掐断下游）", want)
		}
	}
}
