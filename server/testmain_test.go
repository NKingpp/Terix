package server

// 测试用 SQLite 库（每包一个临时文件）。
//
// # 与 PostgreSQL 时代的差别
//
// 删掉的：`db.DSN()` 取共享 dev PG、`sql.Open("pgx")`、
// `pg_advisory_lock(7337741002)` 跨包串行、`ARTEX_TEST_ALLOW_NO_PG` 静默跳过开关。
//
// 保留的：通知事件清理 + 残留守卫（6 张只增不改的表跑完不该涨）——
// 它们是**本包自身**的卫生检查，与后端无关（见 ARCH-SQLite-Migration.md §3.7）。
//
// # 为什么保留「TestMain 统一清事件」而不是逐个测试 cleanup
//
// server 包有 30+ 个测试各自 NewManager，其中 10+ 个会经 report_finding /
// AddFinding 间接写 notification_events。逐个加 cleanup 会漏（已经漏了 29 行）。
//
// 而 notification_events 没有外键、findings.task_id 是 ON DELETE SET NULL
// （故意保留 finding 做审计），所以「删 task/finding」永远清不掉事件。
//
// 本包测试串行，id 上界是可靠分界 —— 这一点在 SQLite 下依然成立。

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// suiteDBPath 是本包测试共享的库文件路径（TestMain 初始化）。
var suiteDBPath string

// testDBPath 返回共享测试库路径。
func testDBPath(t *testing.T) string {
	t.Helper()
	if suiteDBPath == "" {
		t.Fatal("TestMain 未初始化测试库")
	}
	return suiteDBPath
}

func TestMain(m *testing.M) {
	path := os.Getenv("ARTEX_DB_PATH")
	cleanup := func() {}
	if path == "" {
		dir, err := os.MkdirTemp("", "server-test-*")
		if err != nil {
			fmt.Fprintf(os.Stderr, "创建测试临时目录失败: %v\n", err)
			os.Exit(1)
		}
		path = filepath.Join(dir, "test.db")
		cleanup = func() { os.RemoveAll(dir) }
	}
	suiteDBPath = path

	// 把测试库路径写进环境变量：还有一批测试走 db.DSN() 自行 db.Open，
	// 让它们也落到这个临时库上，而不是生产库 data/quantex.db。
	if err := os.Setenv("ARTEX_DB_PATH", path); err != nil {
		fmt.Fprintf(os.Stderr, "设置 ARTEX_DB_PATH 失败: %v\n", err)
		os.Exit(1)
	}

	d, err := db.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "打开测试库失败（%s）: %v\n", path, err)
		os.Exit(1)
	}

	wm, wmErr := db.TakeNotificationWatermark(d.DB)
	if wmErr != nil {
		fmt.Fprintf(os.Stderr, "⚠ 通知表上界查询失败，本次跳过事件清理: %v\n", wmErr)
	}
	before, countErr := db.CountResidueRows(d.DB)
	if countErr != nil {
		fmt.Fprintf(os.Stderr, "⚠ 残留基准查询失败，本次跳过守卫: %v\n", countErr)
	}

	code := m.Run()

	// 模拟盘自动账户是运行时按需创建的（paper 自动成交链路），测试里也可能被
	// 触发。它属于已知的测试副作用，守卫对比前先清掉，避免误报残留。
	_, _ = d.Exec(`DELETE FROM exec_accounts WHERE channel_account_id=$1`, paperAutoAccountRef)

	// 先清本测试包产生的事件，再让守卫对比 —— 顺序不能反。
	// 反了的话守卫会把「已知会产生的测试副作用」当残留报错，
	// 那种守卫会因为「永远红」而被无视。
	if wmErr == nil {
		wm.Purge(d.DB)
	}
	if countErr == nil && db.ReportResidue(d.DB, before) && code == 0 {
		code = 1 // 测试全过但有残留 → 整包失败
	}
	if closeErr := d.Close(); closeErr != nil && code == 0 {
		fmt.Fprintf(os.Stderr, "关闭测试库失败: %v\n", closeErr)
		code = 1
	}
	cleanup()
	os.Exit(code)
}
