package evidence

// 测试用 SQLite 库。
//
// # 与 PG 时代的差别
//
// 原来是「连一个共享 dev PG + 持 advisory lock 7337741002 串行五个包 +
// 残留守卫」。SQLite 下每个包一个临时文件 —— 进程内隔离，不需要跨包串行，
// advisory lock 整套删除（ARCH-SQLite-Migration.md §3.7）。
//
// 残留守卫保留：它是**本包自身**的卫生检查（跑完不该留下成长表行），
// 与数据库是不是 SQLite 无关。
//
// # 为什么共享一个库实例
//
// schema 有 67 表 + 125 索引 + 25 触发器，每个测试各开一次太慢。
// 用进程级临时目录（不是 t.TempDir：那绑到第一个测试的生命周期），
// 在 TestMain 结束时删。

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// suiteDirOnce 保证「外部指定库」与「自动建临时库」两条路径都只初始化一次。
var suiteDir string
var suiteDSN string

func evidenceTestDSN(t *testing.T) string {
	t.Helper()
	if suiteDSN == "" {
		t.Fatal("TestMain 未初始化测试库")
	}
	return suiteDSN
}

func TestMain(m *testing.M) {
	dsn := os.Getenv("ARTEX_DB_PATH")
	var cleanup func()
	if dsn == "" {
		dir, err := os.MkdirTemp("", "evidence-test-*")
		if err != nil {
			fmt.Fprintf(os.Stderr, "创建测试临时目录失败: %v\n", err)
			os.Exit(1)
		}
		dsn = filepath.Join(dir, "test.db")
		cleanup = func() { os.RemoveAll(dir) }
	} else {
		cleanup = func() {}
	}
	suiteDir, suiteDSN = filepath.Dir(dsn), dsn

	// 把测试库路径写进环境变量：还有一批测试走 db.DSN() 自行 db.Open，
	// 让它们也落到这个临时库上，而不是生产库 data/quantex.db。
	if err := os.Setenv("ARTEX_DB_PATH", dsn); err != nil {
		fmt.Fprintf(os.Stderr, "设置 ARTEX_DB_PATH 失败: %v\n", err)
		os.Exit(1)
	}

	pg, err := db.Open(dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "打开测试库失败（%s）: %v\n", dsn, err)
		os.Exit(1)
	}

	wm, wmErr := db.TakeNotificationWatermark(pg.DB)
	if wmErr != nil {
		fmt.Fprintf(os.Stderr, "⚠ 通知表上界查询失败，本次跳过事件清理: %v\n", wmErr)
	}
	before, countErr := db.CountResidueRows(pg.DB)
	if countErr != nil {
		fmt.Fprintf(os.Stderr, "⚠ 残留基准查询失败，本次跳过守卫: %v\n", countErr)
	}

	code := m.Run()

	// 先清本包产生的事件，再让守卫对比 —— 顺序不能反。
	if wmErr == nil {
		wm.Purge(pg.DB)
	}
	if countErr == nil && db.ReportResidue(pg.DB, before) && code == 0 {
		code = 1
	}
	// 必须先关库再删目录：Windows 上未关闭的文件句柄会让 RemoveAll 失败。
	if closeErr := pg.Close(); closeErr != nil && code == 0 {
		fmt.Fprintf(os.Stderr, "关闭测试库失败: %v\n", closeErr)
		code = 1
	}
	cleanup()
	os.Exit(code)
}
