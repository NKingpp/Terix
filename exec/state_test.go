package exec

// WE-3 五条硬规则的测试（§5.1 验收标准）。
//
// # 测试策略：纯校验用假数据，副作用用真库
//
//	规则 1/2/3 → ValidateTransition 是纯函数，用表驱动覆盖所有组合
//	规则 4/5   → 要写库，用真 PG（skiff 无 PG 时 skip）
//
// 这样前三条可以在没有数据库的环境跑，且能穷举组合。

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// strPtr / int64Ptr 是构造限额字段的小工具。
func strPtr(s string) *string { return &s }
func int64Ptr(n int64) *int64 { return &n }

// fullLimits 返回一组完整的限额（三个字段都正）。
func fullLimits() *db.ExecutionLimits {
	return &db.ExecutionLimits{
		MaxSingleAmount: strPtr("10000"),
		MaxDailyAmount:  strPtr("50000"),
		MaxDailyOrders:  int64Ptr(10),
	}
}

// validAuth 返回一条与 fullLimits 匹配的授权（目标 live_manual）。
func validAuth(rev int64) *db.ExecAuthorization {
	snap, _ := BuildLimitsSnapshot(fullLimits())
	return &db.ExecAuthorization{
		ID:             1,
		Channel:        "okx",
		AccountRef:     "acct-1",
		Mode:           string(db.ExecModeLiveManual),
		LimitsSnapshot: snap,
		RulesRevision:  rev,
	}
}

// TestRule1FrozenRejectsLive 规则 1：frozen → live_* 被拒。
func TestRule1FrozenRejectsLive(t *testing.T) {
	for _, target := range []db.ExecMode{db.ExecModeLiveWhitelist, db.ExecModeLiveManual} {
		err := ValidateTransition(TransitionRequest{
			From: db.ExecModeFrozen,
			To:   target,
			// 即使授权和限额都齐全，也必须拒绝 —— 这就是「硬规则」
			Auth:                 validAuth(1),
			CurrentLimits:        fullLimits(),
			CurrentRulesRevision: 1,
		})
		if err == nil {
			t.Fatalf("frozen → %s 必须被拒（即使授权齐全）", target)
		}
		if !errors.Is(err, &ModeError{Code: CodeModeFrozenRejectsLive}) {
			t.Errorf("frozen → %s 应返回 CodeModeFrozenRejectsLive，实际: %v", target, err)
		}
	}
}

// TestRule1FrozenToPaperAllowed frozen → paper 是唯一合法出口。
func TestRule1FrozenToPaperAllowed(t *testing.T) {
	err := ValidateTransition(TransitionRequest{From: db.ExecModeFrozen, To: db.ExecModePaper})
	if err != nil {
		t.Fatalf("frozen → paper 应合法: %v", err)
	}
}

// TestRule2AuthRequired 规则 2a：升级到 live 没有授权 → 拒绝。
func TestRule2AuthRequired(t *testing.T) {
	err := ValidateTransition(TransitionRequest{
		From:          db.ExecModePaper,
		To:            db.ExecModeLiveWhitelist,
		Auth:          nil, // 没有授权
		CurrentLimits: fullLimits(),
	})
	if !errors.Is(err, &ModeError{Code: CodeAuthRequired}) {
		t.Fatalf("无授权升级应返回 CodeAuthRequired，实际: %v", err)
	}
}

// TestRule2AuthModeMustMatch 规则 2b：授权目标模式必须精确匹配。
//
// # 为什么不能有包含关系
//
// live_whitelist 与 live_manual 的约束维度不同（标的限制 vs 每笔确认），
// 授权了 A 不等于授权了 B。
func TestRule2AuthModeMustMatch(t *testing.T) {
	auth := validAuth(1)
	auth.Mode = string(db.ExecModeLiveWhitelist) // 只授权了白名单

	err := ValidateTransition(TransitionRequest{
		From:                 db.ExecModePaper,
		To:                   db.ExecModeLiveManual, // 想升到手动（更宽松）
		Auth:                 auth,
		CurrentLimits:        fullLimits(),
		CurrentRulesRevision: 1,
	})
	if !errors.Is(err, &ModeError{Code: CodeAuthStale}) {
		t.Fatalf("授权模式不匹配应返回 CodeAuthStale，实际: %v", err)
	}
}

// TestRule2RulesRevisionMismatch 规则 2c：规则版本变了 → 授权过期。
func TestRule2RulesRevisionMismatch(t *testing.T) {
	err := ValidateTransition(TransitionRequest{
		From:                 db.ExecModePaper,
		To:                   db.ExecModeLiveManual,
		Auth:                 validAuth(1), // 授权时 rev=1
		CurrentLimits:        fullLimits(),
		CurrentRulesRevision: 2, // 现在 rev=2
	})
	if !errors.Is(err, &ModeError{Code: CodeAuthStale}) {
		t.Fatalf("规则版本变化应返回 CodeAuthStale，实际: %v", err)
	}
}

// TestRule2LimitsSnapshotMismatch 规则 2d：限额变了 → 授权过期。
//
// # 这条守的是「授权不能变空白支票」
//
// 否则用户可以：授权（1 万上限）→ 改成 100 万 → 直接实盘。
func TestRule2LimitsSnapshotMismatch(t *testing.T) {
	auth := validAuth(1) // 快照是 10000 / 50000 / 10
	changed := fullLimits()
	changed.MaxSingleAmount = strPtr("1000000") // 改成 100 万

	err := ValidateTransition(TransitionRequest{
		From:                 db.ExecModePaper,
		To:                   db.ExecModeLiveManual,
		Auth:                 auth,
		CurrentLimits:        changed,
		CurrentRulesRevision: 1,
	})
	if !errors.Is(err, &ModeError{Code: CodeAuthStale}) {
		t.Fatalf("限额变化应返回 CodeAuthStale，实际: %v", err)
	}
}

// TestRule2AllMatchPasses 规则 2：三重一致时放行。
func TestRule2AllMatchPasses(t *testing.T) {
	err := ValidateTransition(TransitionRequest{
		From:                 db.ExecModePaper,
		To:                   db.ExecModeLiveManual,
		Auth:                 validAuth(1),
		CurrentLimits:        fullLimits(),
		CurrentRulesRevision: 1,
	})
	if err != nil {
		t.Fatalf("三重一致时应放行，实际: %v", err)
	}
}

// TestRule3LimitsIncomplete 规则 3：限额不完整 → 拒绝 live。
func TestRule3LimitsIncomplete(t *testing.T) {
	cases := []struct {
		name   string
		limits *db.ExecutionLimits
	}{
		{"全部为 nil", &db.ExecutionLimits{}},
		{"缺单笔上限", &db.ExecutionLimits{MaxDailyAmount: strPtr("50000"), MaxDailyOrders: int64Ptr(10)}},
		{"缺日上限", &db.ExecutionLimits{MaxSingleAmount: strPtr("10000"), MaxDailyOrders: int64Ptr(10)}},
		{"缺日单数", &db.ExecutionLimits{MaxSingleAmount: strPtr("10000"), MaxDailyAmount: strPtr("50000")}},
		{"单笔为 0", &db.ExecutionLimits{MaxSingleAmount: strPtr("0"), MaxDailyAmount: strPtr("50000"), MaxDailyOrders: int64Ptr(10)}},
		{"日上限为负", &db.ExecutionLimits{MaxSingleAmount: strPtr("10000"), MaxDailyAmount: strPtr("-1"), MaxDailyOrders: int64Ptr(10)}},
		{"日单数为 0", &db.ExecutionLimits{MaxSingleAmount: strPtr("10000"), MaxDailyAmount: strPtr("50000"), MaxDailyOrders: int64Ptr(0)}},
		{"完全没有限额记录", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 授权快照也用同一份（保证「一致」这一条不干扰）
			snap, _ := BuildLimitsSnapshot(c.limits)
			auth := &db.ExecAuthorization{
				Mode:           string(db.ExecModeLiveManual),
				LimitsSnapshot: snap,
				RulesRevision:  1,
			}
			err := ValidateTransition(TransitionRequest{
				From:                 db.ExecModePaper,
				To:                   db.ExecModeLiveManual,
				Auth:                 auth,
				CurrentLimits:        c.limits,
				CurrentRulesRevision: 1,
			})
			if !errors.Is(err, &ModeError{Code: CodeLimitsIncomplete}) {
				t.Fatalf("限额不完整应返回 CodeLimitsIncomplete，实际: %v", err)
			}
		})
	}
}

// TestRule3LimitsCompletePasses 限额完整时放行。
func TestRule3LimitsCompletePasses(t *testing.T) {
	err := ValidateTransition(TransitionRequest{
		From:                 db.ExecModePaper,
		To:                   db.ExecModeLiveManual,
		Auth:                 validAuth(1),
		CurrentLimits:        fullLimits(),
		CurrentRulesRevision: 1,
	})
	if err != nil {
		t.Fatalf("限额完整应放行: %v", err)
	}
}

// TestDowngradeDoesNotNeedAuth 降级不需要授权与限额（收紧永远允许）。
func TestDowngradeDoesNotNeedAuth(t *testing.T) {
	// live_manual → paper：无授权、无限额、规则版本 0，都应该放行
	err := ValidateTransition(TransitionRequest{
		From:          db.ExecModeLiveManual,
		To:            db.ExecModePaper,
		Auth:          nil,
		CurrentLimits: nil,
	})
	if err != nil {
		t.Fatalf("降级不该要求授权，实际: %v", err)
	}

	// live_manual → frozen 也应放行
	err = ValidateTransition(TransitionRequest{
		From: db.ExecModeLiveManual,
		To:   db.ExecModeFrozen,
	})
	if err != nil {
		t.Fatalf("任意 → frozen 应放行: %v", err)
	}
}

// TestLiveWhitelistToManualIsUpgrade whitelist → manual 是升级，需要重新授权。
func TestLiveWhitelistToManualIsUpgrade(t *testing.T) {
	err := ValidateTransition(TransitionRequest{
		From:          db.ExecModeLiveWhitelist,
		To:            db.ExecModeLiveManual,
		Auth:          nil, // 没重新授权
		CurrentLimits: fullLimits(),
	})
	if !errors.Is(err, &ModeError{Code: CodeAuthRequired}) {
		t.Fatalf("whitelist → manual 应要求授权，实际: %v", err)
	}
}

// TestManualToWhitelistIsDowngrade manual → whitelist 是收紧，不需要重新授权。
func TestManualToWhitelistIsDowngrade(t *testing.T) {
	err := ValidateTransition(TransitionRequest{
		From: db.ExecModeLiveManual,
		To:   db.ExecModeLiveWhitelist,
		Auth: nil,
	})
	if err != nil {
		t.Fatalf("收紧到白名单不该要求重新授权，实际: %v", err)
	}
}

// TestLimitsSnapshotNumericEquivalence 数值等价："1000" 与 "1000.0000000000" 视为相同。
//
// # 这条守的是误伤
//
// DB 的 NUMERIC(28,10) 读出来带 10 位小数，而授权时用户输入 "1000"。
// 直接字符串比较会判成不等 → 用户什么都没改，授权却显示过期。
func TestLimitsSnapshotNumericEquivalence(t *testing.T) {
	cur := &db.ExecutionLimits{
		MaxSingleAmount: strPtr("1000.0000000000"), // DB 输出的形态
		MaxDailyAmount:  strPtr("5000.0000000000"),
		MaxDailyOrders:  int64Ptr(10),
	}
	snap, err := json.Marshal(limitsSnapshot{
		MaxSingleAmount: strPtr("1000"), // 用户输入的形态
		MaxDailyAmount:  strPtr("5000"),
		MaxDailyOrders:  int64Ptr(10),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !LimitsSnapshotMatches(snap, cur) {
		t.Error("数值相等但字符串形态不同，应判为匹配（否则用户没改也会被判过期）")
	}
}

// TestLimitsSnapshotNilVsEmpty nil 与 "" 必须区分。
//
// nil = 不限制，"" = 限制为 0（禁止一切）。混为一谈会让「不限制」被当成
// 「禁止」或反之。
func TestLimitsSnapshotNilVsEmpty(t *testing.T) {
	// 快照是 nil（不限制）
	snapNil, _ := json.Marshal(limitsSnapshot{
		MaxSingleAmount: nil,
		MaxDailyAmount:  strPtr("5000"),
		MaxDailyOrders:  int64Ptr(10),
	})
	// 当前是 ""（限制为 0）
	curEmpty := &db.ExecutionLimits{
		MaxSingleAmount: strPtr(""),
		MaxDailyAmount:  strPtr("5000"),
		MaxDailyOrders:  int64Ptr(10),
	}
	if LimitsSnapshotMatches(snapNil, curEmpty) {
		t.Error("nil（不限制）与 \"\"（限制为 0）必须判为不匹配")
	}
}

// TestLimitsSnapshotCorruptFailsClosed 快照损坏时返回 false（fail-closed）。
func TestLimitsSnapshotCorruptFailsClosed(t *testing.T) {
	bad := []byte(`{"max_single_amount": [1,2,3]`) // 非法 JSON
	if LimitsSnapshotMatches(bad, fullLimits()) {
		t.Error("损坏的快照必须判为不匹配（fail-closed）")
	}
}

// TestLimitsSnapshotIgnoresRevision revision 变化不影响比对。
func TestLimitsSnapshotIgnoresRevision(t *testing.T) {
	cur := fullLimits()
	cur.Revision = 99 // revision 变了，其余没变
	snap, _ := BuildLimitsSnapshot(fullLimits())
	if !LimitsSnapshotMatches(snap, cur) {
		t.Error("revision 变化不该导致授权失效")
	}
}
