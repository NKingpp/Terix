package exec

// 「deny 无旁路」的三层防护测试。
//
// # 为什么需要这三层，而不是只测行为
//
// 只测「deny 场景返回 deny」是不够的 —— 那只证明**当前实现**正确，
// 拦不住「将来有人加一个 bypass 参数」。
//
// 旁路往往来自**新增的入口**，而新增的入口在测试构造时可能根本没被传值，
// 行为测试照样通过。
//
// 三层各拦一类改动：
//
//	第 1 层（字段白名单）  拦「新增输入开关」  —— 如 SkipRisk bool / Force bool
//	第 2 层（方法清单）    拦「新增旁路入口」  —— 如 AuthorizeForce()
//	第 3 层（行为穷举）    拦「新增特殊分支」  —— 如某个 Mode 值走特殊路径
//
// 单靠第 3 层拦不住第 1、2 层。三层都要。

import (
	"reflect"
	"testing"
)

// TestGateRequestHasNoBypassField 第 1 层：GateRequest 的字段必须是白名单内的。
//
// # 这个测试怎么起作用
//
// 如果有人给 GateRequest 加了一个字段（比如 `SkipRisk bool`），这个测试会失败。
// 失败信息要求他「先确认这个字段不能绕过 deny，再把它加进白名单」。
//
// **它不能自动判断新字段是否危险** —— 它只能强迫「加字段的人想一遍」。
// 而「想一遍」正是这类设计缺陷唯一有效的防线：没有任何静态规则能判断
// `SkipRisk` 是有害的还是无害的。
func TestGateRequestHasNoBypassField(t *testing.T) {
	// 允许的字段。新增字段必须先想清「它能不能让 deny 变成 allow」，
	// 确认不能之后再加到这里。
	//
	// 当前三个字段都无法绕过 deny：
	//   DraftID   —— 只定位草案，改不了判定
	//   AccountID —— 只定位账户，改不了判定
	//   Mode      —— **传错会直接 deny**（模式不一致检查），不是放宽
	allowed := map[string]bool{
		"DraftID":   true,
		"AccountID": true,
		"Mode":      true,
	}

	typ := reflect.TypeOf(GateRequest{})
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if !allowed[name] {
			t.Errorf("GateRequest 新增了字段 %q。\n"+
				"请先确认它不能让 deny 变成 allow（如果有人能用它绕过风控，"+
				"那这个字段就不该存在）。确认后把它加进本测试的白名单，并在注释里写明「为什么它不能绕过」。",
				name)
		}
	}
	// 反向：白名单里的字段必须真实存在（防止删了字段忘了删白名单）
	for name := range allowed {
		if _, ok := typ.FieldByName(name); !ok {
			t.Errorf("白名单里的字段 %q 已不存在，请从测试里删掉", name)
		}
	}
}

// TestGateHasSingleEntryPoint 第 2 层：Gate 只允许一个导出方法 Authorize。
//
// # 拦的是什么
//
// `AuthorizeForce()` / `AuthorizeSkipRisk()` / `AuthorizeOverride()` ——
// 任何第二个入口都是旁路。
//
// **为什么不能靠 code review 拦**：加一个方法看起来是「新增功能」，
// 而不是「修改现有逻辑」——review 时不会警觉。
// 而这个测试会立刻红。
func TestGateHasSingleEntryPoint(t *testing.T) {
	typ := reflect.TypeOf(&Gate{})
	var exported []string
	for i := 0; i < typ.NumMethod(); i++ {
		m := typ.Method(i)
		// 只统计导出方法（reflect 的 Method 只返回导出方法，这里显式判断更清楚）
		if m.PkgPath == "" {
			exported = append(exported, m.Name)
		}
	}

	// WithClock 是允许的：它**只影响「今天」的边界计算**，
	// 不能改变任何判定分支。但为了避免「用 WithClock 塞进別的东西」，
	// 这里显式列出并注释理由。
	allowed := map[string]bool{
		"Authorize": true,
		"WithClock": true,
		// GetPaperEquityCurve 是**只读**查询，不参与判定、不改变任何状态。
		// 它只把 paper_orders 的汇总读数包装成带「不含手续费」标记的结构体。
		// 放进 allowed 的理由：它无法用来绕过任何 deny —— 它不返回 Decision。
		"GetPaperEquityCurve": true,
		// RulesFingerprint 是**只读查询**：返回当前规则集的内容指纹，
		// 不改变任何状态、不返回 Decision。
		//
		// 放进 allowed 的理由：它无法用来绕过任何 deny ——
		// 它只回答「现在的规则是什么」，不回答「这张单能不能过」。
		"RulesFingerprint": true,
		// ExecutePaper 是「执行」而不是「判定」：它**要求先有 allow 的 verdict**
		// 才肯落单（见 exec/paper.go 的前置条件检查）。
		//
		// 换句话说：它不能绕过任何 deny ——
		//   - 没有 verdict → 拒绝（ErrPaperNotAuthorized）
		//   - verdict 是 deny → 拒绝
		//   - verdict 的 rules_revision 与当前不符 → 拒绝
		//
		// 所以把它加进 allowed 不削弱「deny 无旁路」：
		// 唯一的放行入口仍然是 Authorize，ExecutePaper 只是 Authorize 之后的动作。
		"ExecutePaper": true,
	}

	for _, name := range exported {
		if !allowed[name] {
			t.Errorf("Gate 新增了导出方法 %q。\n"+
				"如果它是「绕过某个校验」的入口，那它不该存在 —— deny 无旁路。\n"+
				"如果它只是配置/调试辅助（如 WithClock），把它加进本测试的 allowed，"+
				"并在注释里写明「为什么它不能改变判定结果」。",
				name)
		}
	}

	// 必须存在且只有一个 Authorize
	count := 0
	for _, name := range exported {
		if name == "Authorize" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("Gate 必须有且只有一个 Authorize 方法，实际 %d 个", count)
	}
}

// TestGateDenyHasNoOverride 第 3 层：deny 场景下穷举参数组合，永远是 deny。
//
// # 与第 1、2 层的分工
//
// 第 1、2 层拦「新增的入口」，这一层拦「现有入口里的新增分支」。
// 比如有人写了「如果 Mode == frozen 则放行」这种逻辑 ——
// 字段没变、方法没变，但行为变了。这层能抓到。
func TestGateDenyHasNoOverride(t *testing.T) {
	g, done := gateTestEnv(t)
	defer done()

	// 构造一个「必然 deny」的场景：账户被拉黑。
	acct := newGateTestAccount(t, g, "blocked")
	if err := g.db.ExecAccounts().Block(acct.ID, "测试：故意拉黑"); err != nil {
		t.Fatalf("拉黑失败: %v", err)
	}
	draftID := newGateTestDraft(t, g, "us")

	modes := []string{"paper", "live_whitelist", "live_manual", "frozen"}
	draftIDs := []int64{draftID, 0, -1, 1 << 40}
	accountIDs := []int64{acct.ID, 0, -1, 1 << 40}

	for _, m := range modes {
		for _, dID := range draftIDs {
			for _, aID := range accountIDs {
				res, err := g.Authorize(t.Context(), GateRequest{
					DraftID: dID, AccountID: aID, Mode: modeOf(m),
				})
				if err != nil {
					// 返回 error 也算没放行，但这里要求「无 error 的明确 deny」
					// 更严格 —— 因为调用方可能把 error 当成「重试」。
					t.Fatalf("mode=%s draft=%d acct=%d 返回 error（应为明确 deny）: %v",
						m, dID, aID, err)
				}
				if res.Decision == DecisionAllow {
					t.Fatalf("账户被拉黑时绝不该 allow：mode=%s draft=%d acct=%d\n"+
						"这是一个 bypass 漏洞 —— deny 必须无旁路。", m, dID, aID)
				}
				if res.Decision == DecisionAsk {
					t.Fatalf("账户被拉黑时不该 ask（ask 意味着人工可以放行）：mode=%s draft=%d acct=%d",
						m, dID, aID)
				}
			}
		}
	}
}

// TestGateFrozenRejectsEverything 第 3 层补充：frozen 时连 paper 都拒。
func TestGateFrozenRejectsEverything(t *testing.T) {
	g, done := gateTestEnv(t)
	defer done()

	acct := newGateTestAccount(t, g, "ok")
	draftID := newGateTestDraft(t, g, "us")

	// 切到 frozen
	if _, err := g.db.TransitionMode(paperMode(), frozenMode()); err != nil {
		t.Fatalf("切 frozen 失败: %v", err)
	}
	t.Cleanup(func() { _, _, _ = g.db.ForceSetExecMode(paperMode()) })

	// 含 paper 在内，全部 deny
	for _, m := range []string{"paper", "live_whitelist", "live_manual", "frozen"} {
		res, err := g.Authorize(t.Context(), GateRequest{
			DraftID: draftID, AccountID: acct.ID, Mode: modeOf(m),
		})
		if err != nil {
			t.Fatalf("mode=%s 返回 error: %v", m, err)
		}
		if res.Decision != DecisionDeny {
			t.Errorf("frozen 时 mode=%s 应 deny，实际 %s（%s）", m, res.Decision, res.Reason)
		}
	}
}
