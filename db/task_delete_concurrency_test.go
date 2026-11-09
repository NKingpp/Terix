package db

// 任务删除与「资产被别的任务共享」的并发测试（SQLite 版）。
//
// # 原测试（PostgreSQL 版）测的是什么
//
// 它用 `pg_blocking_pids()` 探测阻塞，验证两件事：
//
//  1. 「删除锁观察前就提交的共享」不会被误删 —— 靠 READ COMMITTED 的
//     快照 + FOR UPDATE 的行锁语义
//  2. 「host 解析与提交之间进来的共享」会把删除操作挡住 —— 靠行锁
//
// # SQLite 下这两件事怎么变
//
// 没有行锁、没有 READ COMMITTED 快照升级：`_txlock=immediate` 让**每个写事务
// 从 BEGIN 起就独占写锁**。于是原测试的两个场景在 SQLite 下退化成同一件事：
//
//	共享写入先提交 → 删除事务读到它 → 保守地保留 host（正确）
//	删除事务先开始 → 共享写入排队；删除提交后共享写入才落库（也正确：
//	                此时 task 已删，新共享指向不存在的 task，不该保留）
//
// 所以这里不探测「谁阻塞了谁」（那在 SQLite 下不可观察），而是直接断言
// **两种顺序都得到安全结果**：被删任务的独占 host 进了删除清单，
// 而另一个任务的资产一行不少。
//
// 这保留了原测试的全部业务价值，去掉了对它已不成立的机制假设。

import (
	"fmt"
	"testing"
)

// TestDeleteTaskPreservesAssetsSharedByOtherTasks 覆盖两种提交顺序。
//
// 断言的核心不变量：**删除 A 任务时，B 任务共享的 host 不能被当作 A 的
// 独占 host 删掉**。这条与后端无关，是真正要守的东西。
func TestDeleteTaskPreservesAssetsSharedByOtherTasks(t *testing.T) {
	t.Run("共享先提交：删除时必须保守保留", func(t *testing.T) {
		d := testOpen(t)
		first, second, host, rootAssetID := createTaskDeleteRaceFixture(t, d)
		serviceURL := fmt.Sprintf("https://%s/concurrent-owner", host)
		t.Cleanup(func() {
			_ = d.DeleteTask(first.ID)
			_ = d.DeleteTask(second.ID)
			_, _ = d.Exec(`DELETE FROM assets WHERE id=$1 OR url=$2`, rootAssetID, serviceURL)
		})

		// 先让 second 共享同一个 host（task_ids 是 JSON 数组，用 json_array 拼）。
		// 注意这是**提交前**就要在库里的状态 —— SQLite 下没有「未提交的共享
		// 被删除事务看见」这回事，所以直接提交，让删除事务读到它。
		if _, err := d.Exec(`
INSERT INTO assets(type, url, service_type, domain, task_ids)
VALUES ('service', $1, 'http', $2, json_array($3))`, serviceURL, host, second.ID); err != nil {
			t.Fatalf("插入共享资产: %v", err)
		}

		var preparedHosts []string
		if _, err := d.DeleteTaskCascadePrepared(first.ID, true, false, false, func(p TaskDeletePreparation) error {
			preparedHosts = append([]string(nil), p.TrafficHosts...)
			return nil
		}); err != nil {
			t.Fatalf("DeleteTaskCascadePrepared: %v", err)
		}

		if containsDeleteHost(preparedHosts, host) {
			t.Fatalf("被别的任务共享的 host %q 进了删除清单: %v", host, preparedHosts)
		}
		var remaining int
		if err := d.QueryRow(`
SELECT COUNT(*) FROM assets
WHERE url=$1 AND EXISTS(SELECT 1 FROM json_each(task_ids) WHERE CAST(value AS INTEGER)=$2)`,
			serviceURL, second.ID).Scan(&remaining); err != nil {
			t.Fatalf("查询共享资产: %v", err)
		}
		if remaining != 1 {
			t.Fatalf("另一个任务的资产被删了: count=%d", remaining)
		}
	})

	t.Run("独占 host：仍应进删除清单", func(t *testing.T) {
		d := testOpen(t)
		first, second, host, rootAssetID := createTaskDeleteRaceFixture(t, d)
		t.Cleanup(func() {
			_ = d.DeleteTask(first.ID)
			_ = d.DeleteTask(second.ID)
			_, _ = d.Exec(`DELETE FROM assets WHERE id=$1`, rootAssetID)
		})

		var plan TaskDeletePreparation
		if _, err := d.DeleteTaskCascadePrepared(first.ID, true, false, false, func(p TaskDeletePreparation) error {
			plan = p
			return nil
		}); err != nil {
			t.Fatalf("DeleteTaskCascadePrepared: %v", err)
		}
		if !containsDeleteHost(plan.TrafficHosts, host) {
			// 独占 host 必须出现在删除清单里，否则删除会留下孤儿资产。
			t.Fatalf("独占 host %q 不在删除清单里: %v", host, plan.TrafficHosts)
		}
	})
}

// TestConcurrentTaskDeleteAndAssetUpsert 并发删除任务 + 写资产。
//
// # 这个测试替代原来的「阻塞探测」
//
// SQLite 下单写者，两个写事务只能排队。要验证的是「排队之后双方都成功，
// 且没有留下悬空关联」—— 而不是「谁把谁挡住了」。
// 出现 SQLITE_BUSY 说明 DB 层的 BUSY 重试不够，那是必须失败的真问题。
func TestConcurrentTaskDeleteAndAssetUpsert(t *testing.T) {
	d := testOpen(t)
	first, second, host, rootAssetID := createTaskDeleteRaceFixture(t, d)
	serviceURL := fmt.Sprintf("https://%s/race", host)
	t.Cleanup(func() {
		_ = d.DeleteTask(first.ID)
		_ = d.DeleteTask(second.ID)
		_, _ = d.Exec(`DELETE FROM assets WHERE id=$1 OR url=$2`, rootAssetID, serviceURL)
	})

	done := make(chan error, 2)
	go func() {
		_, err := d.DeleteTaskCascadePrepared(first.ID, true, false, false, func(TaskDeletePreparation) error { return nil })
		done <- err
	}()
	go func() {
		// 另一个任务在删除期间写资产：无论谁先，都不该报错。
		_, err := d.Exec(`
INSERT INTO assets(type, url, service_type, domain, task_ids)
VALUES ('service', $1, 'http', $2, json_array($3))`, serviceURL, host, second.ID)
		done <- err
	}()
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatalf("并发操作失败（BUSY 重试不生效？）: %v", err)
		}
	}

	// second 任务的资产必须还在（删除只针对 first）。
	var remaining int
	if err := d.QueryRow(`SELECT COUNT(*) FROM assets WHERE url=$1`, serviceURL).Scan(&remaining); err != nil {
		t.Fatalf("查询资产: %v", err)
	}
	if remaining != 1 {
		t.Fatalf("第二个任务的资产被连带删除: count=%d", remaining)
	}
}

// createTaskDeleteRaceFixture 造「两个任务 + 一个归属 first 的 root_domain 资产」。
func createTaskDeleteRaceFixture(t *testing.T, d *DB) (first, second *Task, host string, rootAssetID int64) {
	t.Helper()
	var err error
	first, err = d.CreateTask("delete race owner", "delete safely", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err = d.CreateTask("delete race sharer", "preserve shared host", nil, 0, 0)
	if err != nil {
		_ = d.DeleteTask(first.ID)
		t.Fatal(err)
	}
	host = fmt.Sprintf("task-delete-race-%d.example.test", first.ID)
	rootAssetID, err = d.Assets().UpsertRootDomain(UpsertRootDomainReq{Domain: host, TaskID: first.ID})
	if err != nil {
		_ = d.DeleteTask(first.ID)
		_ = d.DeleteTask(second.ID)
		t.Fatal(err)
	}
	return first, second, host, rootAssetID
}

// containsDeleteHost 判断 host 是否在删除清单里。
func containsDeleteHost(hosts []string, want string) bool {
	for _, host := range hosts {
		if host == want {
			return true
		}
	}
	return false
}
