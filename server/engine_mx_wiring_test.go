package server

// 妙想 client 必须真的到达 vars 构造器（Engine 层注入的验收）。
//
// # 这组测试守的是什么
//
// 2026-11-09 任务 #6：planner 把「美股基本面/事件面」判成 dead end 跳过，
// 理由是 `MX_APIKEY unconfigured`。而实际上凭证页里 mx key 是有效的，
// 热注入日志也能看到 `[mx] 已刷新妙想客户端` —— 问题在于
// **Engine 从来没有把 mx client 放进 workCtx**。
//
// `agent.WithMXClient` 全库零调用点。于是 fetchMXQueryForVars 走
// mxForVars(ctx) → nil → 返回 notWiredMX 文案「未配置 MX_APIKEY」，
// 分析师把这句话写进 data_gaps，planner 读到 fact 就放弃整个方向。
// 一次装配遗漏，最终表现为「模型自己决定不做」。
//
// 所以这里钉住：Engine 注入了 client 之后，上下文里必须真的能取到它。

import (
	"context"
	"testing"

	"github.com/Autumn-27/artex/agent"
)

// TestEngineInjectsMXClientIntoWorkCtx 注入了就一定能从 ctx 取到。
func TestEngineInjectsMXClientIntoWorkCtx(t *testing.T) {
	e := &Engine{}
	want := agent.NewMXClient("test-mx-key-for-wiring")
	e.SetMXClientFunc(func() *agent.MXClient { return want })

	ctx := e.withMXClient(context.Background())
	got, ok := agent.MXClientFrom(ctx)
	if !ok {
		t.Fatal("ctx 里没有 mx client —— vars 构造器会报「通道未启用」")
	}
	if got != want {
		t.Error("取到的不是注入的那个实例")
	}
}

// TestEngineMXClientNilIsNoop 未配置时不能塞一个「未配置的 client」进 ctx。
//
// WithMXClient 本身对未配置实例是 no-op，但这条测试守住调用方也别传脏值：
// 否则将来有人改成裸 ctx.WithValue，未配置的 client 会让
// mxForVars 走进「已注入但 apiKey 为空」的另一个错误分支。
func TestEngineMXClientNilIsNoop(t *testing.T) {
	e := &Engine{}
	e.SetMXClientFunc(func() *agent.MXClient { return nil })
	if _, ok := agent.MXClientFrom(e.withMXClient(context.Background())); ok {
		t.Error("nil client 不该进入 ctx")
	}
}

// TestEngineMXClientFollowsHotReload 凭证页保存后，新 client 必须立刻生效。
//
// 这是「传函数而不是传快照」的理由：Engine 若持有启动那刻的实例，
// 用户后来在页面上配 key 就不会生效，且没有任何报错。
func TestEngineMXClientFollowsHotReload(t *testing.T) {
	e := &Engine{}
	var cur *agent.MXClient
	e.SetMXClientFunc(func() *agent.MXClient { return cur })

	// 启动时没有 key
	if _, ok := agent.MXClientFrom(e.withMXClient(context.Background())); ok {
		t.Fatal("前置：未配置时不该有 client")
	}

	// 用户保存凭证 → 热注入
	cur = agent.NewMXClient("key-saved-later")

	got, ok := agent.MXClientFrom(e.withMXClient(context.Background()))
	if !ok || got != cur {
		t.Error("兑换后应立刻拿到新 client —— 否则「配了 key 仍报未启用」会复现")
	}
}

// TestMarketToolSetMXClientReflectsCurrent 工具集必须能报出当前 client。
func TestMarketToolSetMXClientReflectsCurrent(t *testing.T) {
	m := agent.NewMarketToolSet(nil)
	if m.MXClient() != nil {
		t.Fatal("未注入时不该有 client")
	}
	c := agent.NewMXClient("k")
	m.SetMXClient(c)
	if m.MXClient() != c {
		t.Error("MXClient() 应返回当前实例")
	}
	m.SetMXClient(nil)
	if m.MXClient() != nil {
		t.Error("清空后应返回 nil")
	}
}
