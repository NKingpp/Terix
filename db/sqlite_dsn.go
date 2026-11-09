package db

// SQLite 连接串与时间格式（全项目唯一实现）。
//
// # 为什么 DSN 必须在这里拼
//
// 有四个 pragma 是**连接级**的（每条新连接都要重设），不写在 DSN 里就等于没设：
//
//	foreign_keys(1)     SQLite 默认关闭外键。不设的话所有 REFERENCES / ON DELETE
//	                    CASCADE 都是装饰品 —— 删父行不级联，孤儿行无声积累。
//	busy_timeout(5000)  写锁等待上限。不设时遇到并发写立刻报 SQLITE_BUSY。
//	journal_mode(WAL)   不加则是 rollback journal，读会阻塞写。
//	_txlock=immediate   见下。
//
// # 为什么 _txlock=immediate 是硬要求
//
// SQLite 的 BEGIN 默认是 deferred：事务开始时不拿写锁，直到第一条写语句才升级。
// 两个 deferred 事务"先读后写"会互相等对方释放读锁 —— 真死锁，busy_timeout 救不了
// （实测：立刻返回 SQLITE_BUSY，不等超时）。改成 immediate 后 Begin() 直接拿
// RESERVED 锁，把死锁退化成排队。
//
// 注意驱动只在 !opts.ReadOnly 时用 BEGIN IMMEDIATE，所以 ReadOnly 事务仍是
// deferred 读事务 —— 这是对的（读不该抢写锁）。
//
// # 时间格式为什么是 TEXT
//
// 全库时间列用 TEXT 存 ISO8601 UTC 毫秒（2026-11-08T04:04:15.275Z）：
//   - 字典序 == 时间序，WHERE ts >= $1 天然正确
//   - 前端 new Date(x) 直接可解析
//   - 现有 Go 结构体字段本来就是 string，db.ParseDBTime 直接可用
//
// 禁止用 CURRENT_TIMESTAMP（只到秒、空格分隔）或 datetime('now')：混用会破坏
// 上述两条性质。

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// busyTimeoutMS 是写锁等待上限。5s 足以覆盖绝大多数写事务；
	// 超过 5s 的长写事务属于设计问题（见 docs/quantex/ARCH-SQLite-Migration.md §6.4），
	// 应该缩短事务而不是调大这个值。
	busyTimeoutMS = 5000

	// isoTimeLayout 是 SQLNow 产出的格式，也是 ParseDBTime 的首选解析格式。
	isoTimeLayout = "2006-01-02T15:04:05.000Z"
)

// SQLNow 是可嵌进 SQL 的"当前时刻"表达式。
//
// 全项目只允许这一个写法。%f 给 SS.SSS（毫秒），Z 表示 UTC。
// 与 NowISO() 的格式完全一致，所以 SQL 写入与 Go 写入的值可以直接比较。
const SQLNow = `strftime('%Y-%m-%dT%H:%M:%fZ','now')`

// NowISO 返回与 SQLNow 完全同格式的当前时刻串，用于绑定参数。
//
// 为什么不直接把 time.Time 当参数绑：驱动默认写 t.String() 形态
// （"2026-01-02 03:04:05.123456789 +0000 UTC"），strftime 解析不了，
// 字典序也不是时间序。所有时间参数都要先转成本函数的结果。
func NowISO() string { return time.Now().UTC().Format(isoTimeLayout) }

// FormatISO 把 time.Time 转成与 SQLNow 同格式的串。
func FormatISO(t time.Time) string { return t.UTC().Format(isoTimeLayout) }

// BuildDSN 把库文件路径拼成 modernc.org/sqlite 的 DSN。
//
// path 必须是绝对路径：相对路径会随进程 CWD 漂移，导致"换了启动目录就换了一个库"。
// 实测中文路径、含空格路径、Windows 正斜杠都能正常工作。
func BuildDSN(path string) string {
	q := url.Values{}
	// _pragma 可重复；驱动按字典序应用，但会强制把 busy_timeout 排在最前
	// （见 modernc/sqlite sqlite.go:146 附近），所以顺序不敏感。
	q.Add("_pragma", "busy_timeout("+strconv.Itoa(busyTimeoutMS)+")")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Set("_txlock", "immediate")
	// 兜底：万一有代码漏了 NowISO 直接把 time.Time 绑进来，
	// 让它写出 strftime/ParseDBTime 都能解析的格式，而不是 t.String()。
	q.Set("_time_format", "sqlite")
	return "file:" + filepath.ToSlash(path) + "?" + q.Encode()
}

// resolveDSN 决定 Open 的入参是"库文件路径"还是"已拼好的 DSN"。
//
// 兼容两种调用：db.Open("/abs/path/x.db") 与 db.Open(BuildDSN(path))。
// 带 "?" 或 "file:" 前缀的视为已拼好（测试里直接给内存库时会用到）。
func resolveDSN(dsnOrPath string) (dsn, path string) {
	s := strings.TrimSpace(dsnOrPath)
	if strings.HasPrefix(s, "file:") {
		// 从 DSN 里抠出文件路径，供 ensureDataDir 建父目录
		base := strings.TrimPrefix(s, "file:")
		if i := strings.IndexByte(base, '?'); i >= 0 {
			base = base[:i]
		}
		return s, filepath.FromSlash(base)
	}
	if strings.Contains(s, "?") {
		return s, ""
	}
	return BuildDSN(s), s
}

// ensureDataDir 创建库文件所在目录。
//
// SQLite 不会自动建父目录，缺目录时报 "unable to open database file (14)" ——
// 这个错误信息完全不提目录，第一次遇到很容易误判成权限问题。
func ensureDataDir(path string) error {
	if path == "" || strings.Contains(path, ":memory:") {
		return nil
	}
	dir := filepath.Dir(path)
	if dir == "" || dir == "." {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建数据库目录失败（%s）: %w", dir, err)
	}
	return nil
}

// Backup 把当前库完整快照到 dst。
//
// # 为什么用 VACUUM INTO 而不是拷文件
//
// WAL 模式下最近的事务还在 .db-wal 里，只拷 .db 会丢数据。VACUUM INTO 生成的是
// 单文件完整快照（不需要 -wal/-shm），且过程中不阻塞读、只在最后短暂持写锁。
//
// # 两个实测过的坑
//
//   - 目标已存在 -> `SQL logic error: output file already exists (1)`。
//     所以先 os.Remove；本函数语义是"覆盖"。
//   - 目标目录不存在 -> `unable to open database file (14)`。
//     所以先 MkdirAll。
//
// 路径必须走参数化（VACUUM INTO ?），实测可用；不要字符串拼接。
func (d *DB) Backup(dst string) error {
	if strings.TrimSpace(dst) == "" {
		return fmt.Errorf("备份目标路径为空")
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("创建备份目录失败（%s）: %w", filepath.Dir(dst), err)
	}
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("清理旧备份失败（%s）: %w", dst, err)
	}
	if _, err := d.Exec(`VACUUM INTO ?`, dst); err != nil {
		return fmt.Errorf("备份失败（%s）: %w", dst, err)
	}
	return nil
}
