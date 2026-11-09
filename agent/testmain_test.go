package agent

// 测试用 SQLite 库（每包一个临时文件）。
//
// # 与 PostgreSQL 时代的差别
//
// 删掉的：`db.DSN()` 取共享 dev PG、`sql.Open("pgx")`、
// `pg_advisory_lock(7337741002)` 跨包串行、`ARTEX_TEST_ALLOW_NO_PG` 静默跳过开关。
//
// 保留的：残留守卫（6 张只增不改的表跑完不该涨）—— 它是**本包自身**的卫生
// 检查，与后端无关（见 ARCH-SQLite-Migration.md §3.7）。
//
// # 为什么不再需要 fail-fast
//
// PG 版没 DSN 就没法跑，所以默认 fail-fast 逼人配 DSN。SQLite 有合理默认值，
// 没配置就自动用临时库 —— 于是「默认就能跑」和「fail-fast」不再矛盾。

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// suiteDBPath 是本包测试共享的库文件路径（TestMain 初始化）。
var suiteDBPath string

// testDBPath 返回共享测试库路径（供各测试自行 db.Open）。
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
		dir, err := os.MkdirTemp("", "agent-test-*")
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

	if wmErr == nil {
		wm.Purge(d.DB)
	}
	if countErr == nil && db.ReportResidue(d.DB, before) && code == 0 {
		code = 1
	}
	if closeErr := d.Close(); closeErr != nil && code == 0 {
		fmt.Fprintf(os.Stderr, "关闭测试库失败: %v\n", closeErr)
		code = 1
	}
	cleanup()
	os.Exit(code)
}
