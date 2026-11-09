package intercept

// ask 规则的超时强制 —— 这是 2026-10-30 发现的 P0 的回归测试。
//
// # 背景（为什么这个测试必须存在）
//
// HandleAsk 在 TimeoutEnabled=false 时进入**无限期** select，唯一出口是
// pending channel 的决策。而审批 UI（/system/intercept/approvals）已随渗透 UI
// 整目录删除 —— 所以 ask 规则一旦没有超时，Agent 的工具调用会永久卡死
// 直到任务被取消。
//
// 实测（2026-10-30）：库里两条启用中的 ask 规则
// （[内置] 草案-日内下单次数 / 单标的集中度近似）都是 timeout_enabled=false，
// 而 timeout_seconds=60 形同虚设 —— 因为那个分支根本不读它。
//
// # 这个测试防的是什么
//
// 它防「有人把 timeoutEnabledFor 改回直接透传 DB 值」。
// 那种改动不会报错、不会崩溃，只会让 Agent 在特定规则下永久卡住 ——
// 而那种卡住只在「用户真的下单到阈值」时才发生，极难复现。

import (
	"os"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestAskAlwaysHasTimeout(t *testing.T) {
	// 模拟「DB 里 timeout_enabled=false」的那两条规则。
	r := compiledRule{InterceptRule: db.InterceptRule{
		ID: 42, Name: "[内置] 草案-日内下单次数",
		Action:         "ask",
		TimeoutEnabled: false, // ← 关键：DB 里就是这个值
		TimeoutSeconds: 60,
		TimeoutAction:  "deny",
	}}

	if !timeoutEnabledFor(r) {
		t.Fatal("ask 规则的 TimeoutEnabled 必须为 true —— " +
			"否则 HandleAsk 会无限期阻塞，而审批 UI 不存在，" +
			"Agent 的工具调用会永久卡死直到任务取消")
	}
	if got := timeoutSecondsFor(r); got != 60 {
		t.Errorf("timeoutSecondsFor=%d, want 60", got)
	}
}

func TestAskTimeoutSecondsFallsBackTo60(t *testing.T) {
	// timeout_seconds=0 时兜底 60 —— 否则 Decision 看起来像「不超时」。
	r := compiledRule{InterceptRule: db.InterceptRule{
		Action:         "ask",
		TimeoutEnabled: false,
		TimeoutSeconds: 0,
	}}
	if got := timeoutSecondsFor(r); got != 60 {
		t.Errorf("ask + TimeoutSeconds=0 应兜底为 60，实际 %d", got)
	}
}

func TestNonAskKeepsDBTimeout(t *testing.T) {
	// allow/deny 不走 HandleAsk，所以保持透传 —— 不改变既有语义。
	for _, action := range []string{"allow", "deny"} {
		r := compiledRule{InterceptRule: db.InterceptRule{
			Action: action, TimeoutEnabled: false, TimeoutSeconds: 0,
		}}
		if timeoutEnabledFor(r) {
			t.Errorf("%s 不应被强制启用超时", action)
		}
		if got := timeoutSecondsFor(r); got != 0 {
			t.Errorf("%s 的 TimeoutSeconds 应保持 0，实际 %d", action, got)
		}
	}
}

// TestTimeoutBranchEmitsActivity 是「超时必须留用户可见记录」的源码级回归。
//
// # 为什么用源码断言而不是行为测试
//
// HandleAsk 需要真实 *db.DB + pendingManager + channel，行为测试要构造
// 一整套 DB 并等 60 秒。而这里要防的是「有人删掉 emit 那几行」——
// 那是一个**结构**问题，不是行为问题：只要那几行还在，行为就成立。
//
// 代价：它不能证明 emit 的**内容**正确。所以配套的文案在下面一起断言。
//
// # 它防的是什么
//
// 2026-10-30 发现：超时分支只写 DB（ResolveIntercept）而不 emit activity。
// 请求阶段有 emit（intercept_request），所以用户知道「有个东西在等我」；
// 而超时后没有 —— 用户看到那条「请求审批」还在，以为还在等他，
// 而实际已按 timeout_action 拒绝了。
//
// 对金融交易，「还在等」和「已拒绝」的用户动作完全不同。
func TestTimeoutBranchEmitsActivity(t *testing.T) {
	src, err := os.ReadFile("intercept.go")
	if err != nil {
		t.Fatalf("读取 intercept.go 失败: %v", err)
	}
	code := string(src)

	// 切出超时分支（从 timer.C 到下一个 case）。
	i := strings.Index(code, "case <-timer.C:")
	if i < 0 {
		t.Fatal("找不到超时分支 case <-timer.C: —— 代码结构变了，本测试需要更新")
	}
	tail := code[i:]
	j := strings.Index(tail, "case <-ctx.Done():")
	if j < 0 {
		t.Fatal("找不到超时分支的结束标记 case <-ctx.Done():")
	}
	branch := tail[:j]

	// ① 必须 emit 一条 Kind="intercept_timeout" 的 activity。
	if !strings.Contains(branch, `"intercept_timeout"`) {
		t.Error("超时分支必须 emit Kind=\"intercept_timeout\" 的 activity —— " +
			"否则超时拒绝是静默的，用户会以为「还在审批中」而实际已被拒绝")
	}

	// ② 必须同时覆盖 chat（AppendConvActivity）与 task（taskEmit）两条渠道 ——
	//    只覆盖一条会让另一类会话的静默问题照旧。
	for _, need := range []string{"AppendConvActivity", "taskEmit"} {
		if !strings.Contains(branch, need) {
			t.Errorf("超时分支必须覆盖 %s 渠道（两类会话都要能看到）", need)
		}
	}

	// ③ Summary 必须写明秒数与决策结果 —— 只写「超时了」不够，
	//    用户需要知道「按什么策略处理了」。
	if !strings.Contains(branch, "secs") || !strings.Contains(branch, "verb") {
		t.Error("超时 activity 的 Summary 必须含秒数与决策词（放行/拒绝）")
	}

	// ④ 不许复用 intercept_request 这个 Kind —— 超时是「已有结果」，
	//    而 request 是「等人处理」，前端可能要按 Kind 区分样式。
	if strings.Contains(branch, `Kind: "intercept_request"`) {
		t.Error("超时分支不应复用 intercept_request 这个 Kind")
	}
}
