package evidence

// 事务外做文件 IO 的并发验证（ARCH-SQLite-Migration.md §6.4.2 #2/#3/#4）。
//
// # 这个文件测什么
//
// 三个断言，全部对应「文件 IO 曾在数据库写事务里」这个 P0 缺陷：
//
//  1. CopySnapshots 与 WithInstalledSnapshots 全程不占写锁 ——
//     另一个 goroutine 能在它们进行中完成写事务，不会 SQLITE_BUSY。
//  2. 并发两份 CopySnapshots 到不同目标：都成功，文件完整（可校验 hash）。
//  3. 安装与 GC 不互相破坏：GC 不会删掉安装中途写下的 blob。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// testBusyTimeout 是并发测试里的写事务时长上限。
//
// 取 2s：真正「事务里做 fsync」的实现会因为持锁超过这个值而让另一侧
// 拿到 SQLITE_BUSY（DSN 里 busy_timeout=5000，但本测试用 2s 收紧窗口，
// 让回归更快暴露）。
const testBusyTimeout = 2 * time.Second

// TestConcurrentCopySnapshotsNoBusy 验证两份并发复制都成功且文件完整。
func TestConcurrentCopySnapshotsNoBusy(t *testing.T) {
	s, in, _ := evidenceFixture(t)
	ctx := context.Background()
	f, err := s.DB.Exploration(in.ExplorationID).RecordFinding(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	snap := seedSnapshot(t, s, f.FindingID, "concurrent-copy",
		[]byte("concurrent copy body 0000000000000000000000000000"))

	dests := []string{
		filepath.Join(t.TempDir(), "archive-a"),
		filepath.Join(t.TempDir(), "archive-b"),
	}
	var wg sync.WaitGroup
	errs := make([]error, len(dests))
	for i, dest := range dests {
		wg.Add(1)
		go func(i int, dest string) {
			defer wg.Done()
			errs[i] = s.CopySnapshots(ctx, []db.TrafficEvidenceSnapshot{snap}, dest)
		}(i, dest)
	}
	// 同时跑一个写事务：如果 CopySnapshots 还占着写锁，这里会等到超时。
	wg.Add(1)
	var writeErr error
	go func() {
		defer wg.Done()
		writeCtx, cancel := context.WithTimeout(ctx, testBusyTimeout)
		defer cancel()
		_, writeErr = s.DB.ExecContext(writeCtx,
			`INSERT INTO settings(key, value) VALUES($1, $2)
ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
			"evidence_concurrency_probe", "ok")
	}()
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("CopySnapshots[%d] 失败: %v", i, err)
		}
	}
	if writeErr != nil {
		t.Fatalf("并发的写事务失败（说明复制路径仍占写锁）: %v", writeErr)
	}
	// 文件完整性：按 hash 校验每个目标里的两份 body。
	for _, dest := range dests {
		for _, body := range []struct {
			hash string
			len  int64
		}{{snap.ReqHash, snap.ReqLen}, {snap.RespHash, snap.RespLen}} {
			path, pathErr := hashPath(dest, body.hash)
			if pathErr != nil {
				t.Fatalf("hashPath: %v", pathErr)
			}
			if err := verifyFile(path, body.hash, body.len); err != nil {
				t.Fatalf("目标 %s 的文件不完整: %v", dest, err)
			}
		}
	}
	// staging 目录应已清空（rename 走掉了）。
	for _, dest := range dests {
		stage := filepath.Join(dest, ".staging")
		entries, readErr := os.ReadDir(stage)
		if readErr != nil && !os.IsNotExist(readErr) {
			t.Fatalf("读 .staging 失败: %v", readErr)
		}
		if len(entries) != 0 {
			t.Fatalf("%s 残留 %d 个暂存文件", stage, len(entries))
		}
	}
}

// TestInstallHoldsNoWriteLock 验证「安装期间另一个写事务能进」。
//
// 这是 #3 的回归点：以前 restore() 在事务里，安装 + restore 全程持写锁。
func TestInstallHoldsNoWriteLock(t *testing.T) {
	s, in, _ := evidenceFixture(t)
	ctx := context.Background()
	f, err := s.DB.Exploration(in.ExplorationID).RecordFinding(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	snap := seedSnapshot(t, s, f.FindingID, "install-lock", []byte("install lock body"))
	archive := t.TempDir()
	if err := s.CopySnapshots(ctx, []db.TrafficEvidenceSnapshot{snap}, archive); err != nil {
		t.Fatal(err)
	}
	// 装到同一个 Store（幂等：已存在就只校验）。
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.WithInstalledSnapshots(ctx, []db.TrafficEvidenceSnapshot{snap}, archive, func() error {
			close(entered)
			<-release
			// restore 里开自己的短事务：这就是生产路径的形态。
			_, e := s.DB.Exec(`SELECT 1`)
			return e
		})
	}()
	<-entered

	writeCtx, cancel := context.WithTimeout(ctx, testBusyTimeout)
	defer cancel()
	if _, err := s.DB.ExecContext(writeCtx,
		`INSERT INTO settings(key, value) VALUES($1, $2)
ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		"evidence_install_probe", "ok"); err != nil {
		close(release)
		<-done
		t.Fatalf("安装期间写事务被阻塞（说明文件 IO 仍在事务里）: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("WithInstalledSnapshots: %v", err)
	}
}

// TestCollectKeepsFreshBodiesThenReapsStale 验证 GC 的宽限期与回收。
//
// 用文件 mtime 断言，不依赖数据库时间 —— 因为宽限期是文件系统属性。
func TestCollectKeepsFreshBodiesThenReapsStale(t *testing.T) {
	s, in, _ := evidenceFixture(t)
	ctx := context.Background()
	f, err := s.DB.Exploration(in.ExplorationID).RecordFinding(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	snap := seedSnapshot(t, s, f.FindingID, "gc-mtime", []byte("gc mtime body"))
	path, err := hashPath(s.Dir, snap.RespHash)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.DeleteFinding(f.FindingID); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	if err := s.Collect(ctx, now); err != nil {
		t.Fatalf("Collect(now): %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("宽限期内的 blob 被删了: %v", err)
	}
	// 把 mtime 推到宽限期之外，再回收一次。
	old := now.Add(-2 * gcGracePeriod)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	if err := s.Collect(ctx, now.Add(gcGracePeriod)); err != nil {
		t.Fatalf("Collect(gcGracePeriod later): %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("过期孤儿没被回收: %v", err)
	}
}

// TestCollectNoBusyUnderConcurrentWrites 验证 GC 的数据库段不阻塞别的写。
func TestCollectNoBusyUnderConcurrentWrites(t *testing.T) {
	s, _, _ := evidenceFixture(t)
	ctx := context.Background()
	// 造一批文件：Collect 的 WalkDir 会真的遍历它们。
	for i := 0; i < 40; i++ {
		hash := sha256Hex("orphan-" + string(rune('a'+i%26)) + string(rune('0'+i/26)))
		p, err := hashPath(s.Dir, hash)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// 设为过期，好让它们被删。
	old := time.Now().Add(-2 * gcGracePeriod)
	for i := 0; i < 40; i++ {
		hash := sha256Hex("orphan-" + string(rune('a'+i%26)) + string(rune('0'+i/26)))
		if p, err := hashPath(s.Dir, hash); err == nil {
			_ = os.Chtimes(p, old, old)
		}
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	var writeErr error
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			cctx, cancel := context.WithTimeout(ctx, testBusyTimeout)
			_, writeErr = s.DB.ExecContext(cctx,
				`INSERT INTO settings(key, value) VALUES($1, $2)
ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
				"evidence_gc_probe", "v")
			cancel()
			if writeErr != nil {
				return
			}
		}
	}()
	if err := s.Collect(ctx, time.Now()); err != nil {
		close(stop)
		wg.Wait()
		t.Fatalf("并发下的 Collect 失败: %v", err)
	}
	close(stop)
	wg.Wait()
	if writeErr != nil {
		t.Fatalf("Collect 期间写事务被阻塞: %v", writeErr)
	}
}

// sha256Hex 是测试用的 hash 生成器（与生产同一算法，保证 hashPath 接受）。
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
