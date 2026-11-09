package db

// WE-4：订单状态机的并发控制测试。
//
// # 这组测试守的是什么
//
// `Transition` 用 `UPDATE ... WHERE state=$from` + `RowsAffected()` 做乐观并发控制。
// 如果少了 WHERE 里的状态条件，两个并发调用会各自读到 submitted，
// 各自写 filled 和 cancelled，**后写的覆盖先写的，且两个调用方都以为成功**。
//
// 这类 bug 在单线程测试里永远不出现 —— 只有真并发才暴露。
// 所以这里必须用真 goroutine，不能用「顺序调用两次」冒充。

import (
	"errors"
	"sync"
	"testing"
)

// newExecOrderAt 建一条处于指定状态的订单，返回 id。
func newExecOrderAt(t *testing.T, d *DB, state string) (int64, *ExecAccount) {
	t.Helper()
	tag := execTestTag(t)
	a := newExecAccount(t, d, tag)
	draftID := newDraftForExecTest(t, d)

	id, err := d.ExecOrders().InsertOrder(ExecOrderInput{
		DraftID:       draftID,
		AccountID:     a.ID,
		Mode:          string(ExecModeLiveManual),
		State:         state,
		ClientOrderID: "coid-" + tag,
		Qty:           "1",
	})
	if err != nil {
		t.Fatalf("建订单失败: %v", err)
	}
	return id, a
}

// TestExecTransitionConcurrentOnlyOneWins 两个 goroutine 同时推进同一单，只有一个成功。
//
// # 这是 WE-4 验收标准的核心
//
//	> 两个 goroutine 同时推进同一单只有一个成功
//
// 断言三件事：
//  1. 恰好一个返回 true（不是两个，也不是零个）
//  2. 另一个返回 (false, nil) —— **不是错误**，是「被别人先推进」
//  3. 库里终态是那个成功者写入的状态（没有被后写覆盖）
func TestExecTransitionConcurrentOnlyOneWins(t *testing.T) {
	d := execTestDB(t)
	id, _ := newExecOrderAt(t, d, ExecStateSubmitted)

	// 两个 goroutine 抢同一个转换：submitted → filled
	// 用 WaitGroup 确保真的并发起跑（不是顺序执行）。
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results []bool
		errs    []error
		start   = make(chan struct{})
	)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // 同时放行
			ok, err := d.ExecOrders().Transition(id, ExecStateSubmitted, ExecStateFilled)
			mu.Lock()
			results = append(results, ok)
			errs = append(errs, err)
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()

	// 1. 不能有系统错误
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d 返回错误: %v（「被别人先推进」不该是错误）", i, err)
		}
	}
	// 2. 恰好一个成功
	okCount := 0
	for _, ok := range results {
		if ok {
			okCount++
		}
	}
	if okCount != 1 {
		t.Fatalf("应恰好一个 goroutine 成功，实际 %d 个（results=%v）", okCount, results)
	}

	// 3. 库里终态正确
	got, err := d.ExecOrders().GetOrder(id)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if got.State != ExecStateFilled {
		t.Errorf("终态 = %q, want filled", got.State)
	}
}

// TestExecTransitionConcurrentDifferentTargets 两个 goroutine 推进到**不同**目标状态。
//
// # 与上一个测试的区别
//
// 上一个测「同一转换抢两次」；这个测「两个不同的转换抢同一单」——
// 后者更接近真实场景（调度器想标 filled，用户同时点了撤单）。
//
// 断言：恰好一个成功，库里终态是那个成功者的目标。
func TestExecTransitionConcurrentDifferentTargets(t *testing.T) {
	d := execTestDB(t)
	id, _ := newExecOrderAt(t, d, ExecStateSubmitted)

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		wins  = map[string]int{}
		errCh []error
		start = make(chan struct{})
	)
	// 两个目标都必须是从 submitted 出发的**合法**转换 ——
	// 否则测到的就不是并发控制，而是「非法转换被拒」（那是另一个测试的事）。
	targets := []string{ExecStateFilled, ExecStateCancelRequested}
	for _, to := range targets {
		wg.Add(1)
		go func(target string) {
			defer wg.Done()
			<-start
			ok, err := d.ExecOrders().Transition(id, ExecStateSubmitted, target)
			mu.Lock()
			if err != nil {
				errCh = append(errCh, err)
			} else if ok {
				wins[target]++
			}
			mu.Unlock()
		}(to)
	}
	close(start)
	wg.Wait()

	if len(errCh) > 0 {
		t.Fatalf("不该有错误: %v", errCh[0])
	}
	total := 0
	for _, n := range wins {
		total += n
	}
	if total != 1 {
		t.Fatalf("应恰好一个成功，实际 %d 个（wins=%v）", total, wins)
	}

	got, err := d.ExecOrders().GetOrder(id)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	// 终态必须是那个成功者的目标
	if wins[got.State] != 1 {
		t.Errorf("终态 %q 不在成功者里（wins=%v）—— 说明被覆盖了", got.State, wins)
	}
	// 两个目标都是合法转换，所以「只有一个成功」纯粹来自并发控制，
	// 而不是被合法性检查挡掉 —— 这让这个测试的断言更干净。
}

// TestExecTransitionIllegalRejected 非法转换被拒（filled → submitted）。
func TestExecTransitionIllegalRejected(t *testing.T) {
	d := execTestDB(t)
	id, _ := newExecOrderAt(t, d, ExecStateFilled)

	ok, err := d.ExecOrders().Transition(id, ExecStateFilled, ExecStateSubmitted)
	if !errors.Is(err, ErrExecTransitionInvalid) {
		t.Fatalf("filled → submitted 应返回 ErrExecTransitionInvalid，实际: %v (ok=%v)", err, ok)
	}
	if ok {
		t.Error("非法转换不该返回 ok=true")
	}

	// 库里状态未变
	got, _ := d.ExecOrders().GetOrder(id)
	if got.State != ExecStateFilled {
		t.Errorf("非法转换后状态被改了: %q", got.State)
	}
}

// TestExecTransitionWrongFrom 用错误的 from 调用返回 ok=false（不是错误）。
//
// # 这条守的是「不匹配不等于失败」
//
// 调用方以为当前是 submitted，实际已经是 filled（别人推进了）。
// 这种情况必须返回 (false, nil) —— 让调用方「跳过」。
// 如果返回错误，调用方会重试，而重试毫无意义。
func TestExecTransitionWrongFrom(t *testing.T) {
	d := execTestDB(t)
	id, _ := newExecOrderAt(t, d, ExecStateFilled)

	// 从 submitted 推进（但实际是 filled）
	ok, err := d.ExecOrders().Transition(id, ExecStateSubmitted, ExecStateFilled)
	if err != nil {
		t.Fatalf("from 不匹配不该返回错误，实际: %v", err)
	}
	if ok {
		t.Error("from 不匹配应返回 ok=false")
	}
}

// TestExecTransitionsTableComplete 11 个状态全部在转换表里，无遗漏。
//
// # 为什么必须查这个
//
// 转换表漏一个状态，「未知订单状态」会变成 InsertOrder 拒绝 ——
// 而那个错误指向「入参错」，不是「表漏了」，排查会走弯路。
func TestExecTransitionsTableComplete(t *testing.T) {
	all := []string{
		ExecStateDraft, ExecStateRiskPending, ExecStateRiskPassed,
		ExecStateRiskDenied, ExecStateSubmitted, ExecStatePartial,
		ExecStateFilled, ExecStateRejected, ExecStateCancelRequested,
		ExecStateCancelled, ExecStateSettled,
	}
	if len(all) != 11 {
		t.Fatalf("状态常量应为 11 个，实际 %d", len(all))
	}
	for _, s := range all {
		if _, ok := execStateTransitions[s]; !ok {
			t.Errorf("状态 %q 不在转换表里（InsertOrder 会拒绝它）", s)
		}
	}
	if len(execStateTransitions) != len(all) {
		t.Errorf("转换表有 %d 项，状态有 %d 个 —— 数量不等说明有脏键",
			len(execStateTransitions), len(all))
	}
}

// TestExecTransitionPartialToCancelled 部分成交后撤单：partial → cancelled 必须合法。
//
// # 这条是 team-lead 指出的缺口
//
// 部分成交后用户撤单，通道撤掉剩余部分 → 终态 cancelled 且 filled_qty > 0。
// 没有这条转换的话，这个最常见的场景会永久卡在 cancel_requested，
// 而 cancel_requested 不是终态，调度器会一直扫到它。
func TestExecTransitionPartialToCancelled(t *testing.T) {
	d := execTestDB(t)
	id, _ := newExecOrderAt(t, d, ExecStatePartial)

	ok, err := d.ExecOrders().Transition(id, ExecStatePartial, ExecStateCancelled)
	if err != nil {
		t.Fatalf("partial → cancelled 应该合法，实际报错: %v", err)
	}
	if !ok {
		t.Fatal("partial → cancelled 应成功")
	}
	got, _ := d.ExecOrders().GetOrder(id)
	if got.State != ExecStateCancelled {
		t.Errorf("终态 = %q", got.State)
	}
}

// TestExecTransitionCancelRequestedToFilled 撤单请求到达前已成交 —— 合法。
//
// # 这条最容易漏
//
// 用户点了撤单，但订单在撤单请求到达通道前已经成交。
// 撤单请求不等于撤单成功 —— cancel_requested → filled 必须合法，
// 否则「撤单晚了」这个场景会把订单卡在 cancel_requested。
func TestExecTransitionCancelRequestedToFilled(t *testing.T) {
	d := execTestDB(t)
	id, _ := newExecOrderAt(t, d, ExecStateCancelRequested)

	ok, err := d.ExecOrders().Transition(id, ExecStateCancelRequested, ExecStateFilled)
	if err != nil {
		t.Fatalf("cancel_requested → filled 应合法: %v", err)
	}
	if !ok {
		t.Fatal("应成功")
	}
}

// TestExecTransitionTerminalStatesNoExit 四个终点状态没有任何出边。
func TestExecTransitionTerminalStatesNoExit(t *testing.T) {
	terminals := []string{
		ExecStateRiskDenied, ExecStateRejected, ExecStateCancelled, ExecStateSettled,
	}
	for _, s := range terminals {
		if got := execStateTransitions[s]; len(got) != 0 {
			t.Errorf("终点状态 %q 不该有出边，实际: %v", s, got)
		}
	}
}
