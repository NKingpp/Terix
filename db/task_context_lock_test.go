package db

// LLM 配置的引用一致性测试（SQLite 版）。
//
// # 与 PostgreSQL 版的关系：语义降级，理由明确
//
// 原文件用 `FOR UPDATE` + `pg_blocking_pids()` 的**行锁探测**验证
// 「先锁 task 再锁 profile」这套加锁顺序，目的是防 PG 的死锁检测器报 40P01。
//
// SQLite 没有行锁，也没有死锁：`_txlock=immediate` 让每个写事务在 BEGIN 时
// 就拿全局写锁，第二个写者只会**排队**（busy_timeout 内）或明确报
// SQLITE_BUSY —— 不可能出现「两个事务互相等对方」。所以：
//
//   - 加锁顺序这件事**不再有可观察的行为**（没有行锁可选）
//   - 原测试的阻塞探测（pg_blocking_pids）在 SQLite 下没有等价物
//
// 保留下来的、真正还有价值的不变量：**并发改 profile 与改引用时结果必须自洽**
// （不出现「profile 没了但 agent 还指着它」）。这个文件用并发压测覆盖它，
// 同时验证 BUSY 重试真的管用（否则测试会红）。
//
// 行锁顺序的证据保留在 git 历史里，不需要在 SQLite 下伪造一个等价物。

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestConcurrentProfileBindingStaysConsistent 并发把 profile 绑到 agent /
// conversation / task 上，再删掉其中一个，最后断言没有悬空引用。
//
// # 为什么断言「状态自洽」而不是「没有报错」
//
// 单写者下报错只能说明 BUSY 重试不够；真正的风险是**成功但状态错**
// （A 的绑定被 B 的写入覆盖、删 profile 后 task 还指着它）。
// 那种错误不会报错，只会让「配置看起来生效了但实际没生效」。
//
// # 为什么每个断言都要排除 NULL
//
// 「绑定为 NULL」是删除 profile 后的**正确**结果；而「绑定指向不存在的 id」
// 才是错。两者必须区分，否则测试会在正确行为上误报。
func TestConcurrentProfileBindingStaysConsistent(t *testing.T) {
	d := testOpen(t)
	suffix := time.Now().UnixNano()

	// 3 个 profile：并发绑定时互相抢占，能覆盖「替换而非追加」的路径。
	profileIDs := make([]int64, 0, 3)
	for i := 0; i < 3; i++ {
		p, err := d.SaveProfile(&LLMProfile{
			Name:   fmt.Sprintf("sqlite-consistency-%d-%d", suffix, i),
			Format: "openai",
			Model:  fmt.Sprintf("consistency-%d", i),
			APIKey: "test-key",
		})
		if err != nil {
			t.Fatalf("SaveProfile[%d]: %v", i, err)
		}
		profileIDs = append(profileIDs, p)
	}
	agentKey := fmt.Sprintf("sqlite_consistency_%d", suffix)
	if _, err := d.CreateAgent(agentKey, "consistency agent", ""); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	conv, err := d.CreateConversation("planner", fmt.Sprintf("consistency conv %d", suffix), nil)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	task, err := d.CreateTask("consistency task", "concurrent binding", nil, 0, 0)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	t.Cleanup(func() {
		_ = d.DeleteTask(task.ID)
		_ = d.DeleteConversation(conv.ID)
		_ = d.DeleteAgent(agentKey)
		for _, id := range profileIDs {
			_ = d.DeleteProfile(id)
		}
	})

	// 并发绑定：每个 goroutine 抢一个不同的 profile。谁最后写谁生效 ——
	// 但无论谁生效，值必须是这三个之一。
	errs := make(chan error, 3)
	var wg sync.WaitGroup
	for i := range profileIDs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := profileIDs[i]
			if err := d.SetAgentLLMProfile(agentKey, &id); err != nil {
				errs <- fmt.Errorf("SetAgentLLMProfile[%d]: %w", i, err)
				return
			}
			if err := d.UpdateConversationProfile(conv.ID, &id); err != nil {
				errs <- fmt.Errorf("UpdateConversationProfile[%d]: %w", i, err)
				return
			}
			if err := d.ReplaceTaskLLMProfiles(task.ID, []int64{id}, id); err != nil {
				errs <- fmt.Errorf("ReplaceTaskLLMProfiles[%d]: %w", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		// BUSY 说明重试策略不够 —— 那是真问题，必须失败而不是跳过。
		t.Fatalf("并发绑定失败（BUSY 重试不生效？）: %v", err)
	}

	valid := make(map[int64]bool, len(profileIDs))
	for _, id := range profileIDs {
		valid[id] = true
	}
	gotAgent, err := d.GetAgentByKey(agentKey)
	if err != nil || gotAgent == nil {
		t.Fatalf("GetAgentByKey: agent=%+v err=%v", gotAgent, err)
	}
	if gotAgent.LLMProfileID == nil || !valid[*gotAgent.LLMProfileID] {
		t.Fatalf("agent 的 LLMProfileID 非法: %v", gotAgent.LLMProfileID)
	}
	gotConv, err := d.GetConversation(conv.ID)
	if err != nil || gotConv == nil {
		t.Fatalf("GetConversation: conv=%+v err=%v", gotConv, err)
	}
	if gotConv.LLMProfileID == nil || !valid[*gotConv.LLMProfileID] {
		t.Fatalf("conversation 的 LLMProfileID 非法: %v", gotConv.LLMProfileID)
	}
	gotTask, err := d.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	// task 侧是「列表 + 一个 active」，两者都必须落在合法集合内。
	for _, id := range gotTask.LLMProfileIDs {
		if !valid[id] {
			t.Fatalf("task.LLMProfileIDs 含非法 id %d: %v", id, gotTask.LLMProfileIDs)
		}
	}
	if gotTask.ActiveLLMProfileID != nil && !valid[*gotTask.ActiveLLMProfileID] {
		t.Fatalf("task.ActiveLLMProfileID 非法: %v", *gotTask.ActiveLLMProfileID)
	}

	// 删掉 agent 正在用的那个 profile：绑定必须被清掉，不能留悬空指针。
	active := *gotAgent.LLMProfileID
	if err := d.DeleteProfile(active); err != nil {
		t.Fatalf("DeleteProfile(%d): %v", active, err)
	}
	afterDelete, err := d.GetAgentByKey(agentKey)
	if err != nil || afterDelete == nil {
		t.Fatalf("删除后 GetAgentByKey: agent=%+v err=%v", afterDelete, err)
	}
	if afterDelete.LLMProfileID != nil && *afterDelete.LLMProfileID == active {
		t.Fatalf("agent 仍指向已删除的 profile %d（悬空引用）", active)
	}
}
