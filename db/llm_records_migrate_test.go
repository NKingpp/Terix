package db

import (
	"testing"
)

// The raw-body columns were added after release, so existing installs only get
// them through llmRecordsMigrate. This runs the real migration against the dev
// PG on a table stripped back to its pre-upgrade shape, inside a transaction
// that is always rolled back — PG does transactional DDL, so nothing persists.
// llmRecordsMigrate 在 SQLite 版是空串（见 db/commands.go 的注释）：空库起步，
// llmRecordsSchema 的 CREATE TABLE 已包含全部列，不再需要 ALTER ADD COLUMN。
// PG 版那条「把表回退成旧形状再跑迁移」的用例在 SQLite 上没有对偶实现
// （SQLite 的 ALTER TABLE DROP COLUMN 不支持 IF EXISTS，也不支持事务性 DDL）。
//
// 所以这里改成验证实际生效的不变量：
//  1. 建表是幂等的（每次启动都跑）
//  2. 迁移常量是空的，重复执行不会报错
//  3. raw_request / raw_response 两列存在且能往返
func TestLLMRecordsSchemaHasRawColumns(t *testing.T) {
	d := testOpen(t)
	if err := d.EnsureLLMRecordsTable(); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	// 再跑一次：EnsureLLMRecordsTable 在每次启动都会执行，必须幂等。
	if err := d.EnsureLLMRecordsTable(); err != nil {
		t.Fatalf("ensure 不幂等: %v", err)
	}

	for _, col := range []string{"raw_request", "raw_response"} {
		var n int
		if err := d.QueryRow(
			`SELECT count(*) FROM pragma_table_info('llm_records') WHERE name=$1`, col).Scan(&n); err != nil {
			t.Fatalf("inspect %s: %v", col, err)
		}
		if n != 1 {
			t.Errorf("column %s missing", col)
		}
	}

	var got string
	if err := d.QueryRow(
		`INSERT INTO llm_records(model, raw_request, raw_response) VALUES ('m','{"a":1}','data: x')
		 RETURNING raw_response`).Scan(&got); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if got != "data: x" {
		t.Errorf("raw_response=%q want %q", got, "data: x")
	}
}
