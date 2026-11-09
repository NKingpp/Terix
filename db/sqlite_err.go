package db

// SQLite 错误码判定（取代 pgconn.PgError 的 Code() 比较）。
//
// # 实测错误码表（modernc.org/sqlite v1.52.0 / SQLite 3.53.2）
//
//	场景            Code()   base(低8位)  message
//	唯一键冲突      2067     19           constraint failed: UNIQUE constraint failed: t.a, t.b (2067)
//	主键冲突        1555     19           constraint failed: UNIQUE constraint failed: t.id (1555)
//	外键冲突        787      19           constraint failed: FOREIGN KEY constraint failed (787)
//	CHECK 失败      275      19           constraint failed: CHECK constraint failed: <约束名或表达式> (275)
//	NOT NULL        1299     19           constraint failed: NOT NULL constraint failed: t.c (1299)
//	写锁超时        5        5            database is locked (5) (SQLITE_BUSY)
//
// # 为什么 IsBusy 判主码而其它判扩展码
//
// SQLITE_BUSY 有一批扩展码（SQLITE_BUSY_SNAPSHOT=517、SQLITE_BUSY_RECOVERY 等），
// 语义都是"现在拿不到锁，重试可能成功"，所以按主码（Code()&0xff）归一。
// 而 CONSTRAINT 系列扩展码在语义上是*不同*的约束类型（UNIQUE vs FK vs CHECK），
// 必须精确判断，否则会把"外键不存在"误报成"唯一键冲突"。
//
// # 为什么保留注入钩子
//
// *sqlite.Error 的字段未导出（msg / code 都是小写），测试无法构造假错误。
// 所以判定函数留出 var 入口，测试用 t.Cleanup 替换后恢复 —— 这比"触发真实约束冲突"
// 更省事，且能覆盖"错误是 BUSY"这种难以稳定复现的分支。

import (
	"errors"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// 注入钩子：测试可临时替换，用于模拟无法构造的错误分支。
var (
	isBusyFn       = IsBusy
	isUniqueFn     = IsUniqueViolation
	isForeignKeyFn = IsForeignKeyViolation
)

// baseCode 返回 SQLite 主码（低 8 位）。扩展码在高位。
func baseCode(err error) (int, bool) {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return 0, false
	}
	return se.Code() & 0xff, true
}

// IsBusy 报告"写锁等待超时"（busy_timeout 用尽）。
//
// 这类错误是**可重试**的：另一个写事务提交后就能成功。池不会被污染
// （实测：BUSY 之后同一连接池立即恢复正常写入）。
func IsBusy(err error) bool {
	c, ok := baseCode(err)
	if !ok {
		return false
	}
	return c == sqlite3.SQLITE_BUSY || c == sqlite3.SQLITE_LOCKED
}

// IsUniqueViolation 报告唯一键或主键冲突（PG 23505 的等价物）。
func IsUniqueViolation(err error) bool {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	return se.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE ||
		se.Code() == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY
}

// IsForeignKeyViolation 报告外键冲突（PG 23503 的等价物）。
func IsForeignKeyViolation(err error) bool {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	return se.Code() == sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY
}

// IsCheckViolation 报告 CHECK 约束失败。
//
// 注意：命名 CHECK 的 message 里带约束名（可用于精确归因），
// 匿名 CHECK 带的是表达式文本。见 docs/quantex/ARCH-SQLite-Migration.md §4.2。
func IsCheckViolation(err error) bool {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	return se.Code() == sqlite3.SQLITE_CONSTRAINT_CHECK
}

// IsNotNullViolation 报告 NOT NULL 约束失败。
func IsNotNullViolation(err error) bool {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	return se.Code() == sqlite3.SQLITE_CONSTRAINT_NOTNULL
}
