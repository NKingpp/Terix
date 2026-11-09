package db

// 测试用 SQLite 库（每包一个临时文件）。
//
// # 与 PostgreSQL 时代的差别
//
// 原来：`DSN()` 拿共享 dev PG → `sql.Open("pgx")` → 持 advisory lock
// 7337741002 串行五个包 → 残留守卫。
//
// 现在：每个包自己的临时 SQLite 文件，进程内天然隔离 —— advisory lock
// 整套删除（ARCH-SQLite-Migration.md §3.7）。残留守卫保留，它是**本包自身**
// 的卫生检查，与后端无关。
//
// # 为什么不再「静默跳过」
//
// PG 版没 DSN 就 `os.Exit(m.Run())`，于是「没配置 = 全部 skip = 输出 ok」，
// 一大批测试其实从未跑过。SQLite 有默认库（临时文件），所以默认就能跑。
// 外部只要不设 ARTEX_DB_PATH 就自动用临时库。

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// testSuiteDBPath 是整套 db 测试共享的库文件路径（TestMain 初始化）。
var testSuiteDBPath string

// sharedTestDSN 返回共享测试库路径。
//
// ⚠️ 不能用 t.TempDir()：共享库的生命周期绑到「第一个调用 testOpen 的 t」，
// 那个测试一结束目录就被删，后续测试全挂。所以由 TestMain 建进程级临时目录。
func testDSN(t *testing.T) string {
	t.Helper()
	if testSuiteDBPath == "" {
		t.Fatal("TestMain 未初始化测试库")
	}
	return testSuiteDBPath
}

func TestMain(m *testing.M) {
	path := os.Getenv("ARTEX_DB_PATH")
	cleanup := func() {}
	if path == "" {
		dir, err := os.MkdirTemp("", "db-test-*")
		if err != nil {
			fmt.Fprintf(os.Stderr, "创建测试临时目录失败: %v\n", err)
			os.Exit(1)
		}
		path = filepath.Join(dir, "test.db")
		cleanup = func() { os.RemoveAll(dir) }
	}
	testSuiteDBPath = path

	// 把测试库路径写进环境变量：还有一批测试走 db.DSN() 自行 db.Open，
	// 让它们也落到这个临时库上，而不是生产库 data/quantex.db。
	if err := os.Setenv("ARTEX_DB_PATH", path); err != nil {
		fmt.Fprintf(os.Stderr, "设置 ARTEX_DB_PATH 失败: %v\n", err)
		os.Exit(1)
	}

	d, err := Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "打开测试库失败（%s）: %v\n", path, err)
		os.Exit(1)
	}

	// 测试残留守卫：跑完整套后检查 6 张只增不改的表有没有涨。
	//
	// # 为什么 db 包也需要
	//
	// 实测（2026-10-29）：db 包的测试每次跑会让 exec_audit 涨 5 条
	// （冻结账户路径会写审计），而 db 包原先只有 advisory lock、没有守卫 ——
	// 那 5 条从来没被发现过。
	wm, wmErr := TakeNotificationWatermark(d.DB)
	if wmErr != nil {
		fmt.Fprintf(os.Stderr, "⚠ 通知表上界查询失败，本次跳过事件清理: %v\n", wmErr)
	}
	before, countErr := CountResidueRows(d.DB)
	if countErr != nil {
		fmt.Fprintf(os.Stderr, "⚠ 残留基准查询失败，本次跳过守卫: %v\n", countErr)
	}

	code := m.Run()

	// 先清本包产生的事件（AddFinding 会间接写），再让守卫对比 —— 顺序不能反。
	if wmErr == nil {
		wm.Purge(d.DB)
	}
	if countErr == nil && ReportResidue(d.DB, before) && code == 0 {
		code = 1
	}
	// 必须先关库再删目录：Windows 上未关闭的文件句柄会让 RemoveAll 失败。
	if closeErr := d.Close(); closeErr != nil && code == 0 {
		fmt.Fprintf(os.Stderr, "关闭测试库失败: %v\n", closeErr)
		code = 1
	}
	cleanup()
	os.Exit(code)
}
