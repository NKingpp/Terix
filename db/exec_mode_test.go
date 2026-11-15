package db

// 执行模式（settings.exec_mode）的测试。
//
// # 这组测试守的四个语义
//
//  1. key 不存在时返回 live_manual（不是错误）—— 首次启动不能让系统起不来
//  2. 值被写坏时报错 —— 静默回退会掩盖「以为在实盘实际没在」
//  3. 并发 TransitionMode 只有一个成功
//  4. 三档之间自由互转

import (
	"errors"
	"sync"
	"testing"
)

// clearExecMode 删除 exec_mode 设置（测试隔离用）。
func clearExecMode(t *testing.T, d *DB) {
	t.Helper()
	if _, err := d.Exec(`DELETE FROM settings WHERE key=$1`, ExecModeKey); err != nil {
		t.Fatalf("清理 exec_mode 失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM settings WHERE key=$1`, ExecModeKey)
	})
}

// TestExecModeMissingDefaults key 不存在时返回 live_manual。
//
// 首次启动时 settings 里没有 exec_mode。返回错误会让系统起不来；
// 返回零值会让后续校验全部因「未知模式」失败。
func TestExecModeMissingDefaults(t *testing.T) {
	d := execTestDB(t)
	clearExecMode(t, d)

	got, err := d.GetExecMode()
	if err != nil {
		t.Fatalf("key 不存在时不该报错，实际: %v", err)
	}
	if got != ExecModeLiveManual {
		t.Errorf("key 不存在时应返回 live_manual，实际 %q", got)
	}
}

// TestExecModeEmptyValueDefaults 值为空串时同样返回 live_manual。
func TestExecModeEmptyValueDefaults(t *testing.T) {
	d := execTestDB(t)
	clearExecMode(t, d)
	if _, err := d.Exec(`INSERT INTO settings (key, value) VALUES ($1,'')`, ExecModeKey); err != nil {
		t.Fatalf("写入空值失败: %v", err)
	}

	got, err := d.GetExecMode()
	if err != nil {
		t.Fatalf("空值不该报错: %v", err)
	}
	if got != ExecModeLiveManual {
		t.Errorf("空值应返回 live_manual，实际 %q", got)
	}
}

// TestExecModeCorruptValueErrors 值被写坏时报错，不回退默认值。
//
// 如果配成了 "live_manul"（拼错），静默回退会让用户以为配置生效了
// 而实际跑在别的档位 —— 报错能立刻暴露。
func TestExecModeCorruptValueErrors(t *testing.T) {
	d := execTestDB(t)
	clearExecMode(t, d)
	if _, err := d.Exec(`INSERT INTO settings (key, value) VALUES ($1,'live_manul')`, ExecModeKey); err != nil {
		t.Fatalf("写入坏值失败: %v", err)
	}

	if _, err := d.GetExecMode(); !errors.Is(err, ErrExecModeUnknown) {
		t.Fatalf("坏值应返回 ErrExecModeUnknown，实际: %v", err)
	}
}

// TestExecModeRoundTrip 首次设置后能读回（key 原本不存在）。
func TestExecModeRoundTrip(t *testing.T) {
	d := execTestDB(t)
	clearExecMode(t, d)

	ok, err := d.TransitionMode(ExecModeLiveManual, ExecModeLiveAuto)
	if err != nil {
		t.Fatalf("live_manual → live_auto 应成功: %v", err)
	}
	if !ok {
		t.Fatal("应返回 ok=true")
	}

	got, err := d.GetExecMode()
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if got != ExecModeLiveAuto {
		t.Errorf("模式 = %q, want live_auto", got)
	}
}

// TestExecModeAllTransitionsAllowed 三档之间任意互转都合法。
func TestExecModeAllTransitionsAllowed(t *testing.T) {
	modes := []ExecMode{ExecModeLiveWhitelist, ExecModeLiveManual, ExecModeLiveAuto}
	for _, from := range modes {
		for _, to := range modes {
			if from == to {
				continue
			}
			if !CanTransitionExecMode(from, to) {
				t.Errorf("%s → %s 应合法（无人值守，人可随时换档）", from, to)
			}
		}
	}
}

// TestExecModeConcurrentOnlyOneWins 并发切换只有一个成功。
func TestExecModeConcurrentOnlyOneWins(t *testing.T) {
	d := execTestDB(t)
	clearExecMode(t, d)
	if _, err := d.TransitionMode(ExecModeLiveManual, ExecModeLiveAuto); err != nil {
		t.Fatalf("初始化为 live_auto 失败: %v", err)
	}

	const n = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			target := ExecModeLiveWhitelist
			if i%2 == 0 {
				target = ExecModeLiveManual
			}
			ok, err := d.TransitionMode(ExecModeLiveAuto, target)
			if err != nil {
				return
			}
			if ok {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if wins != 1 {
		t.Fatalf("应恰好一个成功，实际 %d 个", wins)
	}
	got, err := d.GetExecMode()
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if got != ExecModeLiveWhitelist && got != ExecModeLiveManual {
		t.Errorf("终态 = %q，不在预期集合里", got)
	}
}

// TestExecModeWrongFromReturnsFalse from 不匹配返回 (false, nil) 不是错误。
func TestExecModeWrongFromReturnsFalse(t *testing.T) {
	d := execTestDB(t)
	clearExecMode(t, d)
	if _, err := d.TransitionMode(ExecModeLiveManual, ExecModeLiveAuto); err != nil {
		t.Fatalf("初始化为 live_auto 失败: %v", err)
	}

	ok, err := d.TransitionMode(ExecModeLiveWhitelist, ExecModeLiveManual)
	if err != nil {
		t.Fatalf("from 不匹配不该报错，实际: %v", err)
	}
	if ok {
		t.Error("from 不匹配应返回 ok=false")
	}
	got, _ := d.GetExecMode()
	if got != ExecModeLiveAuto {
		t.Errorf("模式不该被改动，实际 %q", got)
	}
}

// TestForceSetExecModeReportsChange 返回 (changed, previous)，供调用方决定是否记审计。
func TestForceSetExecModeReportsChange(t *testing.T) {
	d := execTestDB(t)
	clearExecMode(t, d)

	// 情形 1：本来就是 live_manual（key 不存在视为默认）→ changed=false
	changed, prev, err := d.ForceSetExecMode(ExecModeLiveManual)
	if err != nil {
		t.Fatalf("首次设置失败: %v", err)
	}
	if changed {
		t.Error("值未变时 changed 应为 false（不写审计）")
	}
	if prev != string(ExecModeLiveManual) {
		t.Errorf("previous 应为 live_manual，实际 %q", prev)
	}

	// 情形 2：先设成 live_auto，再强制回 live_manual → changed=true
	if _, err := d.TransitionMode(ExecModeLiveManual, ExecModeLiveAuto); err != nil {
		t.Fatalf("设 live_auto 失败: %v", err)
	}
	changed, prev, err = d.ForceSetExecMode(ExecModeLiveManual)
	if err != nil {
		t.Fatalf("强制设置失败: %v", err)
	}
	if !changed {
		t.Error("从 live_auto 改为 live_manual 应 changed=true")
	}
	if prev != string(ExecModeLiveAuto) {
		t.Errorf("previous 应为 live_auto，实际 %q", prev)
	}
	got, _ := d.GetExecMode()
	if got != ExecModeLiveManual {
		t.Errorf("模式 = %q, want live_manual", got)
	}
}

// TestExecModeTransitionsTableComplete 三个模式都在转换表里，且没有多余条目。
func TestExecModeTransitionsTableComplete(t *testing.T) {
	all := []ExecMode{ExecModeLiveWhitelist, ExecModeLiveManual, ExecModeLiveAuto}
	for _, m := range all {
		if _, ok := execModeTransitions[m]; !ok {
			t.Errorf("模式 %q 不在转换表里", m)
		}
	}
	if len(execModeTransitions) != len(all) {
		t.Errorf("转换表条目数 = %d，want %d", len(execModeTransitions), len(all))
	}
	for from, tos := range execModeTransitions {
		if len(tos) != len(all)-1 {
			t.Errorf("%s 的出边数 = %d，want %d", from, len(tos), len(all)-1)
		}
	}
}
