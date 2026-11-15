package exec

// WE-3 规则 4/5 的测试：需要真库（写 settings + 审计）。
//
// # 与 state_test.go 的分工
//
//	state_test.go  规则 1/2/3 —— 纯校验，不连库
//	本文件          规则 4/5  —— 涉及副作用（改 settings、写审计）

import (
	"errors"
	"sync"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// sharedDB 是包级共享的测试库实例。
//
// # 为什么共享（与 db/exec_store_test.go 的 sharedExecDB 同一理由）
//
// `db.Open` 每次跑完整 schema.sql（DDL 拿 ShareLock）。业务 DML 拿
// RowShareLock，两者互斥。本包 8 个测试各 Open 一次，与 db 包并发跑
// （`go test ./db/ ./exec/` 是并行两个进程）时，DDL 与 DML 抢锁
// → `deadlock detected (SQLSTATE 40P01)`。
//
// 实测证据：db 包里跳过我的两个并发测试 → 全量 PASS；加上 → 每次 FAIL。
var (
	sharedDBOnce sync.Once
	sharedDBInst *db.DB
	sharedDBErr  error
)

// sharedTestDB 是 testDB 的别名 —— gate_testutil_test.go 用它，
// 避免两个辅助函数各自维护一份 sync.Once。
func sharedTestDB(t *testing.T) *db.DB { return testDB(t) }

// testDB 返回包级共享的测试库（SQLite，TestMain 建临时文件）。
func testDB(t *testing.T) *db.DB {
	t.Helper()
	sharedDBOnce.Do(func() {
		sharedDBInst, sharedDBErr = db.Open(testDBPath(t))
	})
	if sharedDBErr != nil {
		t.Fatalf("打开测试库失败: %v", sharedDBErr)
	}
	return sharedDBInst
}

// testDSN 返回测试库文件路径。
//
// SQLite 没有连接串，返回值是路径；db.Open 接受路径也接受 BuildDSN 的产物。
// 不再有「没配置就 skip」—— TestMain 保证一定有库。
func testDSN(t *testing.T) string {
	t.Helper()
	return testDBPath(t)
}

// clearMode 清掉 exec_mode 设置。
func clearMode(t *testing.T, d *db.DB) {
	t.Helper()
	if _, err := d.Exec(`DELETE FROM settings WHERE key=$1`, db.ExecModeKey); err != nil {
		t.Fatalf("清理 exec_mode 失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM settings WHERE key=$1`, db.ExecModeKey)
	})
}

// recordingAuditor 记录审计调用，可配置是否失败。
type recordingAuditor struct {
	calls  []auditCall
	failOn int // >0 表示第 N 次调用返回错误（1-based）
}

type auditCall struct {
	From   db.ExecMode
	To     db.ExecMode
	Reason string
}

func (a *recordingAuditor) WriteModeChange(from, to db.ExecMode, reason string) error {
	a.calls = append(a.calls, auditCall{From: from, To: to, Reason: reason})
	if a.failOn > 0 && len(a.calls) == a.failOn {
		return errors.New("模拟审计写入失败")
	}
	return nil
}





// TestRule5AuditFailureRollsBack 规则 5：审计失败导致转换失败（且 DB 回滚）。
//
// # 这条是规则 5 的核心
//
// 「转换成功但没记录」是不可接受的 —— 那就等于没有审计。
// 所以审计失败时必须把模式改回去。
func TestRule5AuditFailureRollsBack(t *testing.T) {
	d := testDB(t)
	clearMode(t, d)

	// 起点 paper，目标 live_manual，授权与限额齐全
	req := TransitionRequest{
		From:                 db.ExecModeLiveManual,
		To:                   db.ExecModeLiveAuto,
		Auth:                 validAuth(1),
		CurrentLimits:        fullLimits(),
		CurrentRulesRevision: 1,
	}
	// 审计必然失败
	aud := &recordingAuditor{failOn: 1}

	err := ApplyTransition(d, aud, req, "测试转换")
	if err == nil {
		t.Fatal("审计失败时 ApplyTransition 必须返回错误")
	}

	// 模式必须回滚到 paper（不能停在 live_manual）
	got, gErr := d.GetExecMode()
	if gErr != nil {
		t.Fatalf("读模式失败: %v", gErr)
	}
	if got != db.ExecModeLiveManual {
		t.Errorf("审计失败后模式应回滚到 paper，实际 %q —— 「转换成功但没审计」不可接受", got)
	}
}

// TestRule5AuditSuccessCommits 审计成功时转换保留。
func TestRule5AuditSuccessCommits(t *testing.T) {
	d := testDB(t)
	clearMode(t, d)

	req := TransitionRequest{
		From:                 db.ExecModeLiveManual,
		To:                   db.ExecModeLiveAuto,
		Auth:                 validAuth(1),
		CurrentLimits:        fullLimits(),
		CurrentRulesRevision: 1,
	}
	aud := &recordingAuditor{}

	if err := ApplyTransition(d, aud, req, "测试转换"); err != nil {
		t.Fatalf("ApplyTransition 失败: %v", err)
	}
	got, _ := d.GetExecMode()
	if got != db.ExecModeLiveAuto {
		t.Errorf("模式 = %q, want live_auto", got)
	}
	if len(aud.calls) != 1 {
		t.Errorf("应写 1 条审计，实际 %d", len(aud.calls))
	}
}

// TestApplyTransitionRejectsInvalid 校验失败时不改 DB、不写审计。
func TestApplyTransitionRejectsInvalid(t *testing.T) {
	d := testDB(t)
	clearMode(t, d)
	// 设成 live_auto
	if _, err := d.TransitionMode(db.ExecModeLiveManual, db.ExecModeLiveAuto); err != nil {
		t.Fatalf("设置 live_auto 失败: %v", err)
	}

	aud := &recordingAuditor{}
	// same → same 不在转换表里（重复切换没有意义，也防重复写审计）
	err := ApplyTransition(d, aud, TransitionRequest{
		From:                 db.ExecModeLiveAuto,
		To:                   db.ExecModeLiveAuto,
		Auth:                 validAuth(1),
		CurrentLimits:        fullLimits(),
		CurrentRulesRevision: 1,
	}, "不该成功")

	if !errors.Is(err, &ModeError{Code: CodeModeTransitionInvalid}) {
		t.Fatalf("应返回 CodeModeTransitionInvalid，实际: %v", err)
	}
	// DB 未变
	if got, _ := d.GetExecMode(); got != db.ExecModeLiveAuto {
		t.Errorf("被拒后模式不该变，实际 %q", got)
	}
	// 审计未写
	if len(aud.calls) != 0 {
		t.Errorf("校验失败不该写审计，实际 %d 条", len(aud.calls))
	}
}

// TestApplyTransitionConflictReturnsSentinel 并发冲突返回 ErrModeConflict。
func TestApplyTransitionConflictReturnsSentinel(t *testing.T) {
	d := testDB(t)
	clearMode(t, d)
	// 实际已设成 live_manual
	if _, err := d.TransitionMode(db.ExecModeLiveManual, db.ExecModeLiveAuto); err != nil {
		t.Fatalf("设置失败: %v", err)
	}

	aud := &recordingAuditor{}
	// 调用方以为还是 paper，想升到 live_manual
	//
	// 目标必须与授权模式一致（validAuth 的目标是 live_manual），
	// 否则会先被「授权不匹配」拦下，测不到冲突那条路径。
	err := ApplyTransition(d, aud, TransitionRequest{
		From:                 db.ExecModeLiveManual,
		To:                   db.ExecModeLiveAuto,
		Auth:                 validAuth(1),
		CurrentLimits:        fullLimits(),
		CurrentRulesRevision: 1,
	}, "冲突测试")

	if !errors.Is(err, ErrModeConflict) {
		t.Fatalf("from 不匹配应返回 ErrModeConflict，实际: %v", err)
	}
	if len(aud.calls) != 0 {
		t.Errorf("冲突时不该写审计，实际 %d 条", len(aud.calls))
	}
}
