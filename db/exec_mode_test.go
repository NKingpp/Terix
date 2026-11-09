package db

// 执行模式（settings.exec_mode）的测试。
//
// # 这组测试守的四个语义
//
//  1. key 不存在时返回 paper（不是错误）—— 首次启动不能让系统起不来
//  2. 值被写坏时报错（不回退 paper）—— 静默回退会掩盖「以为在实盘实际在纸面」
//  3. 并发 TransitionMode 只有一个成功 —— 防「frozen 被覆盖成 live」
//  4. ForceSetExecMode 返回 (changed, previous) 供启动降级写 activity 用

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

// TestExecModeMissingReturnsPaper key 不存在时返回 paper。
//
// # 为什么这条必须测
//
// 首次启动时 settings 里没有 exec_mode。如果返回错误，系统起不来；
// 如果返回零值 ExecMode("")，后续所有校验都会因为「未知模式」而失败。
func TestExecModeMissingReturnsPaper(t *testing.T) {
	d := execTestDB(t)
	clearExecMode(t, d)

	got, err := d.GetExecMode()
	if err != nil {
		t.Fatalf("key 不存在时不该报错，实际: %v", err)
	}
	if got != ExecModePaper {
		t.Errorf("key 不存在时应返回 paper，实际 %q", got)
	}
}

// TestExecModeEmptyValueReturnsPaper 值为空串时也返回 paper。
func TestExecModeEmptyValueReturnsPaper(t *testing.T) {
	d := execTestDB(t)
	clearExecMode(t, d)
	if _, err := d.Exec(`INSERT INTO settings (key, value) VALUES ($1,'')`, ExecModeKey); err != nil {
		t.Fatalf("写入空值失败: %v", err)
	}

	got, err := d.GetExecMode()
	if err != nil {
		t.Fatalf("空值不该报错: %v", err)
	}
	if got != ExecModePaper {
		t.Errorf("空值应返回 paper，实际 %q", got)
	}
}

// TestExecModeCorruptValueErrors 值被写坏时报错，**不回退 paper**。
//
// # 为什么不回退是更安全的
//
// 如果配成了 "live_manul"（拼错），回退 paper 会让用户以为「实盘已启用」
// 但实际每笔都走纸面 —— 他会在真实行情里错过交易机会而毫无察觉。
// 报错能立刻暴露配置问题。
func TestExecModeCorruptValueErrors(t *testing.T) {
	d := execTestDB(t)
	clearExecMode(t, d)
	if _, err := d.Exec(`INSERT INTO settings (key, value) VALUES ($1,'live_manul')`, ExecModeKey); err != nil {
		t.Fatalf("写入坏值失败: %v", err)
	}

	_, err := d.GetExecMode()
	if !errors.Is(err, ErrExecModeUnknown) {
		t.Fatalf("坏值应返回 ErrExecModeUnknown，实际: %v", err)
	}
}

// TestExecModeRoundTrip 设置后能读回。
func TestExecModeRoundTrip(t *testing.T) {
	d := execTestDB(t)
	clearExecMode(t, d)

	ok, err := d.TransitionMode(ExecModePaper, ExecModeLiveManual)
	if err != nil {
		t.Fatalf("paper → live_manual 应成功: %v", err)
	}
	if !ok {
		t.Fatal("应返回 ok=true")
	}

	got, err := d.GetExecMode()
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if got != ExecModeLiveManual {
		t.Errorf("模式 = %q, want live_manual", got)
	}
}

// TestExecModeFrozenCannotGoLive frozen → live_* 被拒（硬规则）。
func TestExecModeFrozenCannotGoLive(t *testing.T) {
	d := execTestDB(t)
	clearExecMode(t, d)

	// 先设成 frozen
	if _, err := d.TransitionMode(ExecModePaper, ExecModeFrozen); err != nil {
		t.Fatalf("paper → frozen 应合法: %v", err)
	}

	for _, target := range []ExecMode{ExecModeLiveWhitelist, ExecModeLiveManual} {
		ok, err := d.TransitionMode(ExecModeFrozen, target)
		if !errors.Is(err, ErrExecModeTransitionInvalid) {
			t.Errorf("frozen → %s 应返回 ErrExecModeTransitionInvalid，实际: %v", target, err)
		}
		if ok {
			t.Errorf("frozen → %s 不该成功", target)
		}
	}

	// 模式仍是 frozen
	got, _ := d.GetExecMode()
	if got != ExecModeFrozen {
		t.Errorf("被拒后模式不该变，实际 %q", got)
	}
}

// TestExecModeFrozenOnlyExitIsPaper frozen 的唯一出口是 paper。
func TestExecModeFrozenOnlyExitIsPaper(t *testing.T) {
	d := execTestDB(t)
	clearExecMode(t, d)
	if _, err := d.TransitionMode(ExecModePaper, ExecModeFrozen); err != nil {
		t.Fatalf("设 frozen 失败: %v", err)
	}

	ok, err := d.TransitionMode(ExecModeFrozen, ExecModePaper)
	if err != nil {
		t.Fatalf("frozen → paper 应合法: %v", err)
	}
	if !ok {
		t.Fatal("应成功")
	}
	got, _ := d.GetExecMode()
	if got != ExecModePaper {
		t.Errorf("模式 = %q, want paper", got)
	}
}

// TestExecModeConcurrentOnlyOneWins 并发 TransitionMode 只有一个成功。
//
// # 这条守的是「frozen 不能被覆盖」
//
// 两个请求同时改模式（一个改成 frozen、一个改成 live_manual），
// 如果都成功，后写的会覆盖先写的 —— 而「frozen 被覆盖成 live_manual」
// 意味着冻结失效，用户以为已经停了但实际还在下单。
func TestExecModeConcurrentOnlyOneWins(t *testing.T) {
	d := execTestDB(t)
	clearExecMode(t, d)
	// 起点是 paper
	if _, err := d.TransitionMode(ExecModePaper, ExecModePaper); err != nil && !errors.Is(err, ErrExecModeTransitionInvalid) {
		// paper→paper 不合法（表里没有自环），忽略这次错误 —— 起点本来就是 paper（不存在）
		t.Logf("初始化 paper 时的提示: %v", err)
	}

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		wins  int
		errs  []error
		start = make(chan struct{})
	)
	targets := []ExecMode{ExecModeFrozen, ExecModeLiveManual}
	for _, to := range targets {
		wg.Add(1)
		go func(target ExecMode) {
			defer wg.Done()
			<-start
			ok, err := d.TransitionMode(ExecModePaper, target)
			mu.Lock()
			if err != nil {
				errs = append(errs, err)
			} else if ok {
				wins++
			}
			mu.Unlock()
		}(to)
	}
	close(start)
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("不该有错误: %v", errs[0])
	}
	if wins != 1 {
		t.Fatalf("应恰好一个成功，实际 %d 个", wins)
	}

	// 库里终态必须是那两个之一（不是空、不是其他）
	got, err := d.GetExecMode()
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if got != ExecModeFrozen && got != ExecModeLiveManual {
		t.Errorf("终态 = %q，不在预期集合里", got)
	}
}

// TestExecModeWrongFromReturnsFalse from 不匹配返回 (false, nil) 不是错误。
func TestExecModeWrongFromReturnsFalse(t *testing.T) {
	d := execTestDB(t)
	clearExecMode(t, d)
	if _, err := d.TransitionMode(ExecModePaper, ExecModeLiveManual); err != nil {
		t.Fatalf("设 live_manual 失败: %v", err)
	}

	// 当前是 live_manual，但调用方以为还是 paper
	ok, err := d.TransitionMode(ExecModePaper, ExecModeLiveWhitelist)
	if err != nil {
		t.Fatalf("from 不匹配不该报错，实际: %v", err)
	}
	if ok {
		t.Error("from 不匹配应返回 ok=false")
	}
	// 模式未变
	got, _ := d.GetExecMode()
	if got != ExecModeLiveManual {
		t.Errorf("被拒后模式不该变，实际 %q", got)
	}
}

// TestForceSetExecModeReportsChange 启动降级要能区分「真降了」和「本来就是 paper」。
//
// # 为什么要区分
//
// changed=true  → 写一条 activity（有意义的降级事件）
// changed=false → 什么都不写（否则每次重启都刷一条噪音）
func TestForceSetExecModeReportsChange(t *testing.T) {
	d := execTestDB(t)
	clearExecMode(t, d)

	// 情形 1：本来就是 paper（key 不存在也视为 paper）→ changed=false
	changed, prev, err := d.ForceSetExecMode(ExecModePaper)
	if err != nil {
		t.Fatalf("首次降级失败: %v", err)
	}
	if changed {
		t.Error("本来就是 paper 时 changed 应为 false（不写 activity）")
	}
	if prev != string(ExecModePaper) {
		t.Errorf("previous 应为 paper，实际 %q", prev)
	}

	// 情形 2：先设成 live_manual，再降级 → changed=true, prev=live_manual
	if _, err := d.TransitionMode(ExecModePaper, ExecModeLiveManual); err != nil {
		t.Fatalf("设 live_manual 失败: %v", err)
	}
	changed, prev, err = d.ForceSetExecMode(ExecModePaper)
	if err != nil {
		t.Fatalf("降级失败: %v", err)
	}
	if !changed {
		t.Error("从 live_manual 降级应 changed=true")
	}
	if prev != string(ExecModeLiveManual) {
		t.Errorf("previous 应为 live_manual，实际 %q", prev)
	}
	got, _ := d.GetExecMode()
	if got != ExecModePaper {
		t.Errorf("降级后模式 = %q, want paper", got)
	}
}

// TestForceSetExecModeFromFrozen 从 frozen 降级也是 changed=true。
//
// 这条单独测：frozen 是最需要「重启降 paper」的状态，
// 而 frozen 的转换表只允许 → paper，所以能走通。
func TestForceSetExecModeFromFrozen(t *testing.T) {
	d := execTestDB(t)
	clearExecMode(t, d)
	if _, err := d.TransitionMode(ExecModePaper, ExecModeFrozen); err != nil {
		t.Fatalf("设 frozen 失败: %v", err)
	}

	changed, prev, err := d.ForceSetExecMode(ExecModePaper)
	if err != nil {
		t.Fatalf("从 frozen 降级失败: %v", err)
	}
	if !changed {
		t.Error("从 frozen 降级应 changed=true")
	}
	if prev != string(ExecModeFrozen) {
		t.Errorf("previous 应为 frozen，实际 %q", prev)
	}
}

// TestExecModeTransitionsTableComplete 4 个模式都在转换表里 + frozen 只能到 paper。
func TestExecModeTransitionsTableComplete(t *testing.T) {
	all := []ExecMode{ExecModePaper, ExecModeLiveWhitelist, ExecModeLiveManual, ExecModeFrozen}
	if len(all) != 4 {
		t.Fatalf("模式常量应为 4 个，实际 %d", len(all))
	}
	for _, m := range all {
		if _, ok := execModeTransitions[m]; !ok {
			t.Errorf("模式 %q 不在转换表里", m)
		}
	}
	// frozen 的出边必须恰好只有 paper
	if got := execModeTransitions[ExecModeFrozen]; len(got) != 1 || got[0] != ExecModePaper {
		t.Errorf("frozen 的出边应只有 [paper]，实际 %v", got)
	}
	// 每个模式都能到 frozen（除 frozen 自己）
	for _, m := range []ExecMode{ExecModePaper, ExecModeLiveWhitelist, ExecModeLiveManual} {
		if !CanTransitionExecMode(m, ExecModeFrozen) {
			t.Errorf("%s → frozen 应合法（随时可冻结）", m)
		}
	}
}
