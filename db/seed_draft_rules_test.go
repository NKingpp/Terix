package db

// seedDraftRules 的验收测试。
//
// 重点不是「插进去了」，而是：
//  ① 幂等 —— 重复调用不重复插
//  ② 不覆盖用户修改 —— 用户改过的规则不该被 seed 重置
//  ③ 全部 5 条都在，且 match_type 正确（闸门的 matcher 按类型分发）

import (
	"testing"
)

// TestSeedDraftRulesInsertsAll 五条内置规则都插进去了。
func TestSeedDraftRulesInsertsAll(t *testing.T) {
	d := testOpen(t)

	n, err := d.CountBuiltinDraftRules()
	if err != nil {
		t.Fatalf("CountBuiltinDraftRules: %v", err)
	}
	if want := len(builtinDraftRules); n != want {
		t.Fatalf("内置草案规则 = %d 条，期望 %d 条", n, want)
	}

	// 逐条核实 match_type 与 pattern_params 非空
	for _, r := range builtinDraftRules {
		var mt, params string
		err := d.QueryRow(`SELECT match_type, pattern_params
FROM intercept_rules WHERE name=$1`, r.Name).Scan(&mt, &params)
		if err != nil {
			t.Fatalf("读 %s 失败: %v", r.Name, err)
		}
		if mt != r.MatchType {
			t.Errorf("%s: match_type = %q，期望 %q", r.Name, mt, r.MatchType)
		}
		if params == "" || params == "{}" {
			t.Errorf("%s: pattern_params 为空 —— matcher 会报「配置错误」", r.Name)
		}
	}
}

// TestSeedDraftRulesIdempotent 重复调用不产生重复行。
//
// 这是最关键的一条：Open() 每次启动都会调 seedDraftRules，
// 如果不幂等，规则会随启动次数线性增长，而 matcher 会重复求值。
func TestSeedDraftRulesIdempotent(t *testing.T) {
	d := testOpen(t)

	before, err := d.CountBuiltinDraftRules()
	if err != nil {
		t.Fatalf("第一次数: %v", err)
	}

	// 再 seed 三次
	for i := 0; i < 3; i++ {
		if err := seedDraftRules(d); err != nil {
			t.Fatalf("第 %d 次 seed: %v", i+2, err)
		}
	}

	after, err := d.CountBuiltinDraftRules()
	if err != nil {
		t.Fatalf("第二次数: %v", err)
	}
	if after != before {
		t.Fatalf("重复 seed 后规则数从 %d 变成 %d —— 不幂等", before, after)
	}
}

// TestSeedDraftRulesDoesNotOverwrite 用户改过的规则不被 seed 重置。
//
// # 为什么这条重要
//
// 用户可能把默认上限从 10000 改成自己想要的（比如 3000，因为他的账户小）。
// 如果 seed 用 ON CONFLICT DO UPDATE，每次启动都会重置回 10000 ——
// 而 10000 可能远超他的风险承受能力。
func TestSeedDraftRulesDoesNotOverwrite(t *testing.T) {
	d := testOpen(t)
	const ruleName = "[内置] 草案-单笔名义金额上限"

	// 备份原值，测试后恢复（避免污染同包其他测试）
	var origParams string
	if err := d.QueryRow(`SELECT pattern_params FROM intercept_rules WHERE name=$1`, ruleName).Scan(&origParams); err != nil {
		t.Fatalf("读原值: %v", err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`UPDATE intercept_rules SET pattern_params=$2 WHERE name=$1`, ruleName, origParams)
	})

	// 用户把上限改成 3000
	const userParams = `{"max":"3000","field":"notional"}`
	if _, err := d.Exec(`UPDATE intercept_rules SET pattern_params=$2 WHERE name=$1`, ruleName, userParams); err != nil {
		t.Fatalf("改规则: %v", err)
	}

	// seed 一次
	if err := seedDraftRules(d); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// 用户的值必须还在。
	//
	// 比 `max` 字段而不是整段文本：jsonb 的 text 表示会重排空格
	// （`{"max":"3000"}` 读回来是 `{"max": "3000"}`），整段比较会假失败。
	var gotMax string
	if err := d.QueryRow(`SELECT pattern_params->>'max' FROM intercept_rules WHERE name=$1`, ruleName).Scan(&gotMax); err != nil {
		t.Fatalf("读回: %v", err)
	}
	if gotMax != "3000" {
		t.Errorf("seed 覆盖了用户修改：max = %q，期望 \"3000\"", gotMax)
	}
}

// TestSeedDraftRulesNoDuplicateNames 同名规则只有一条（WHERE NOT EXISTS 的保证）。
func TestSeedDraftRulesNoDuplicateNames(t *testing.T) {
	d := testOpen(t)
	for _, r := range builtinDraftRules {
		var n int
		if err := d.QueryRow(`SELECT count(*) FROM intercept_rules WHERE name=$1`, r.Name).Scan(&n); err != nil {
			t.Fatalf("数 %s: %v", r.Name, err)
		}
		if n != 1 {
			t.Errorf("%s 有 %d 行，期望 1 行", r.Name, n)
		}
	}
}

// TestBuiltinDraftRulesFingerprintStable 指纹算法稳定（防止无意改动导致全部告警）。
func TestBuiltinDraftRulesFingerprintStable(t *testing.T) {
	a := builtinDraftRule{MatchType: "numeric_lte", PatternParams: `{"max":"1"}`, Action: "deny"}
	b := builtinDraftRule{MatchType: "numeric_lte", PatternParams: `{"max":"1"}`, Action: "deny"}
	if a.fingerprint() != b.fingerprint() {
		t.Error("相同定义的指纹应该相同")
	}

	c := builtinDraftRule{MatchType: "numeric_lte", PatternParams: `{"max":"2"}`, Action: "deny"}
	if a.fingerprint() == c.fingerprint() {
		t.Error("不同定义的指纹不该相同")
	}

	// 不参与指纹的字段变了，指纹不变
	d := builtinDraftRule{MatchType: "numeric_lte", PatternParams: `{"max":"1"}`, Action: "deny",
		Name: "改了名字", Priority: 999, Message: "改了文案"}
	if a.fingerprint() != d.fingerprint() {
		t.Error("name/priority/message 不该影响指纹")
	}
}

// TestBuiltinDraftRulesActionMatchesDB 断言代码定义的 action 与 DB 里的一致。
//
// # 为什么需要这条测试
//
// seed 的策略是「已存在就不覆盖」（见 seedBuiltinDraftRules），所以代码改了
// action、而 DB 里是旧值时，只有一条 slog.Warn，**没有测试会红**。
//
// 而「ask vs deny」的区别是安全语义：
//
//	deny = 无条件拒绝，无人可绕过
//	ask  = 需要人工确认
//
// 这两者不该因为「库里的旧值」而静默保持不一致。
//
// # 具体案例（2026-11-XX）
//
// 用户裁定保留 `ask`（设计变更），而代码当时是 `deny`。
// 改代码后本地库「看不到任何变化」—— 因为它已经是 ask 了。
// 所以验证方式不是「看库变化」，而是「断言代码与库一致」。
//
// ⚠️ 这条测试只在「DB 是当前代码 seed 出来的」时才有意义。
//
//	如果有人在库里手工改了，这条会红 —— 那正是我们想知道的。
func TestBuiltinDraftRulesActionMatchesDB(t *testing.T) {
	d := testOpen(t)

	for _, r := range builtinDraftRules {
		var dbAction string
		err := d.QueryRow(`SELECT action FROM intercept_rules WHERE name=$1 LIMIT 1`, r.Name).Scan(&dbAction)
		if err != nil {
			t.Fatalf("规则 %q 不在 intercept_rules 里（seed 未跑？）: %v", r.Name, err)
		}
		if dbAction != r.Action {
			t.Errorf("规则 %q 的 action 不一致：\n  代码 = %q\n  DB   = %q\n\n"+
				"seed 不会覆盖已存在的行，所以这个不一致会一直存在。\n"+
				"修法：\n"+
				"  - 如果代码是新的设计（如本轮把 deny 改成 ask），手工 UPDATE 那一行：\n"+
				"      UPDATE intercept_rules SET action='%s' WHERE name='%s';\n"+
				"  - 如果 DB 是有意改的（运营手工调过），改代码。\n"+
				"  - 两者都有理由时，先确认哪个是「设计意图」—— 而那个答案在人的脑子里，不在代码里。",
				r.Name, r.Action, dbAction, r.Action, r.Name)
		}
	}
}

// TestBuiltinDraftRulesActionDistribution 断言 5 条规则的 ask/deny 分配。
//
// # 为什么要测「分配的形态」而不只是「代码等于 DB」
//
// 上一条只能发现「代码与 DB 不一致」。而如果有人**同时**把代码和 DB
// 都改成全 deny（比如觉得「ask 太麻烦」），上一条会通过，但这个分配错了。
//
// # 分配的设计意图（见 seed_draft_rules.go 的包级注释）
//
//	ask 的 2 条：判据近似 / 阈值经验
//	  - 日内下单次数（阈值 20 是经验值）
//	  - 单标的集中度近似（判据不可靠，只数草案）
//
//	deny 的 3 条：硬约束
//	  - 单笔名义金额上限
//	  - 标的允许名单
//	  - 交易时段
func TestBuiltinDraftRulesActionDistribution(t *testing.T) {
	wantAsk := map[string]bool{
		"[内置] 草案-日内下单次数":   true,
		"[内置] 草案-单标的集中度近似": true,
	}
	wantDeny := map[string]bool{
		"[内置] 草案-单笔名义金额上限": true,
		"[内置] 草案-标的允许名单":   true,
		"[内置] 草案-交易时段":     true,
	}

	if len(builtinDraftRules) != 5 {
		t.Fatalf("内置规则数 = %d, want 5", len(builtinDraftRules))
	}

	for _, r := range builtinDraftRules {
		switch {
		case wantAsk[r.Name]:
			if r.Action != "ask" {
				t.Errorf("规则 %q 应为 ask（判据近似/阈值经验），实际 %q", r.Name, r.Action)
			}
		case wantDeny[r.Name]:
			if r.Action != "deny" {
				t.Errorf("规则 %q 应为 deny（硬约束，无人可绕过），实际 %q", r.Name, r.Action)
			}
		default:
			t.Errorf("出现未预期的内置规则 %q —— 新增规则时要同时更新本测试的分配", r.Name)
		}
	}
}
