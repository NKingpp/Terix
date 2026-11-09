package server

// Workers 配置的验收测试（W3-15 的「下限校验」部分）。
//
// # 测什么
//
//   1. 不设置 → defaultWorkers（3）
//   2. 设 8 → 8
//   3. 设 2（低于 minWorkers=4）→ 退回 defaultWorkers
//   4. 设 0 / 负数 → 退回 defaultWorkers
//
// # 为什么第 3 条重要
//
// `defaultWorkers=3` 与 `minWorkers=4` 是有意的不一致（见 L15）。
// 不测的话，将来有人「修」其中一边会静默改变行为。

import (
	"testing"
)

func newWorkersTestManager(t *testing.T) *Manager {
	t.Helper()
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	t.Cleanup(func() { m.Close() })
	// 清掉可能残留的 workers 设置。
	t.Cleanup(func() {
		if _, e := m.pg.Exec(`DELETE FROM settings WHERE key=$1`, settingWorkers); e != nil {
			t.Logf("清理 workers 设置失败: %v", e)
		}
	})
	return m
}

// TestWorkersDefaultWhenUnset 不设置时返回 defaultWorkers。
func TestWorkersDefaultWhenUnset(t *testing.T) {
	m := newWorkersTestManager(t)

	if _, err := m.pg.Exec(`DELETE FROM settings WHERE key=$1`, settingWorkers); err != nil {
		t.Fatalf("清设置: %v", err)
	}
	if got := m.Workers(); got != defaultWorkers {
		t.Errorf("未设置时 Workers() = %d, want %d", got, defaultWorkers)
	}
}

// TestWorkersAboveMinReturnsSetValue 设 8 → 返回 8（不是 defaultWorkers）。
//
// # 这条验证「返回实际生效值」
//
// 如果 Workers() 返回的是「用户设置的值」而不是「实际生效值」，
// 那设 8 时两者相同，测不出区别 —— 所以这条只验证「不误退回默认」。
func TestWorkersAboveMinReturnsSetValue(t *testing.T) {
	m := newWorkersTestManager(t)

	if err := m.SetWorkers(8); err != nil {
		t.Fatalf("SetWorkers(8): %v", err)
	}
	if got := m.Workers(); got != 8 {
		t.Errorf("设 8 后 Workers() = %d, want 8", got)
	}
}

// TestWorkersBelowMinFallsBack 设 2（< minWorkers=4）→ 退回 defaultWorkers。
//
// # 这是「返回实际生效值」的核心断言
//
// 用户设了 2，而系统实际用 3（defaultWorkers）。
// Workers() 必须返回 3 而不是 2 —— 否则调用方以为用的是 2。
func TestWorkersBelowMinFallsBack(t *testing.T) {
	m := newWorkersTestManager(t)

	// 前提：minWorkers 确实大于 1，且 defaultWorkers >= minWorkers 或至少不等
	// 如果两者相等，这条测试无意义（请检查 L15 是否已关掉）。
	if defaultWorkers == minWorkers {
		t.Skipf("defaultWorkers == minWorkers == %d，本条无意义（L15 可能已修）", defaultWorkers)
	}

	if err := m.SetWorkers(2); err != nil {
		t.Fatalf("SetWorkers(2): %v", err)
	}
	got := m.Workers()
	if got != defaultWorkers {
		t.Errorf("设 2（< minWorkers=%d）后 Workers() = %d, want %d（defaultWorkers）\n"+
			"如果返回 2，说明 Workers() 返回的是「用户设置值」而不是「实际生效值」",
			minWorkers, got, defaultWorkers)
	}
}

// TestWorkersZeroOrNegativeFallsBack 库里存了 0 / 负数 → 读时退回 defaultWorkers。
//
// # 为什么要绕过 SetWorkers 直接写库
//
// `SetWorkers` 本身拒绝 <=0（返回 error），所以正常路径写不进非法值。
// 但「库里的值可能是外部写进去的」（运维手工改、旧版本遗留、迁移脚本）——
// 而 `Workers()` 是读取路径，它必须能处理这种情况。
//
// # 这测的是「读取路径的健壮性」，不是「写入路径的校验」
//
// 两者是不同的防线：
//
//	写入校验：拦住新写入的非法值（SetWorkers 已做）
//	读取兜底：处理已经存在的非法值（这条测的）
func TestWorkersZeroOrNegativeFallsBack(t *testing.T) {
	m := newWorkersTestManager(t)

	for _, raw := range []string{"0", "-1", "-100"} {
		if _, err := m.pg.Exec(`
INSERT INTO settings(key, value) VALUES ($1, $2)
ON CONFLICT (key) DO UPDATE SET value=$2`, settingWorkers, raw); err != nil {
			t.Fatalf("写 %q: %v", raw, err)
		}
		if got := m.Workers(); got != defaultWorkers {
			t.Errorf("库里是 %q 时 Workers() = %d, want %d", raw, got, defaultWorkers)
		}
	}
}

// TestSetWorkersRejectsNonPositive 写入层拒绝 <=0。
//
// 与上一条对照：上条测「读时兜底」，这条测「写时拒绝」。
// 两条合起来才是完整的防线。
func TestSetWorkersRejectsNonPositive(t *testing.T) {
	m := newWorkersTestManager(t)

	for _, v := range []int{0, -1, -100} {
		if err := m.SetWorkers(v); err == nil {
			t.Errorf("SetWorkers(%d) 应返回错误（写入校验），实际 nil", v)
		}
	}
}

// TestWorkersNonNumericFallsBack 设置值不是数字 → 退回 defaultWorkers。
func TestWorkersNonNumericFallsBack(t *testing.T) {
	m := newWorkersTestManager(t)

	if _, err := m.pg.Exec(`
INSERT INTO settings(key, value) VALUES ($1, 'abc')
ON CONFLICT (key) DO UPDATE SET value='abc'`, settingWorkers); err != nil {
		t.Fatalf("写非法设置: %v", err)
	}
	if got := m.Workers(); got != defaultWorkers {
		t.Errorf("设置值非数字时 Workers() = %d, want %d", got, defaultWorkers)
	}
}
