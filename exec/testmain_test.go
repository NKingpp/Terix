package exec

// TestMain 用 db 包的共享残留守卫检查「跑完整套后表涨了吗」。
//
// # 为什么复用 db.ReportResidue 而不自己数
//
// server / agent 两个包已经用它（见 db/residue_guard.go）。三处各写一套的话，
// 「要检查哪些表」这个清单会分叉 —— 而本轮正是这么漏的：
// exec 包原来只查 orders_draft，而 exec_audit 同时在被写、当天涨了 25 条，
// 却不在检查范围内，所以从未被发现。
//
// 所以「检查哪些表」的定义只放在 db/residue_guard.go 一处。
//
// # SQLite 迁移后删掉的东西
//
// `db.DSN()`、`sql.Open("pgx")`、`pg_advisory_lock(7337741002)`。
// 每个包一个临时库文件，进程内天然隔离，不需要跨包串行。
//
// # 局限
//
// 它只能在整套跑完时发现「有表涨了」，无法定位到具体测试。
// 定位方法：逐个跑单个测试，每次前后对比那几张表的 count。
//
// # 为什么静态 grep 做不到这件事
//
// 「这行 InsertDraft 后面有没有跟 cleanup」需要 AST 分析，
// 而且「cleanup 写了但外键挡住」这类失效 grep 完全看不见
// （2026-10-29 实证：cleanup 里漏删 exec_reconciliations，
//  导致 DELETE FROM orders_execution 被 RESTRICT 挡住，整条链静默失效）。

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
		dir, err := os.MkdirTemp("", "exec-test-*")
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

	// 先清本包产生的事件，再让守卫对比 —— 顺序不能反（否则守卫会把
	// 「已知会产生的测试副作用」当残留报错，那种永远红的守卫会被无视）。
	if wmErr == nil {
		wm.Purge(d.DB)
	}

	// 即使测试本身已失败，也要检查残留 —— 因为残留可能是失败的**原因**。
	// 但只在 code == 0 时才把它改成 1（已经失败就不必再改）。
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
