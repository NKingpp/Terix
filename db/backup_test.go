package db

// 备份与 BUSY 重试的自检。
//
// 这两个测试覆盖的是"迁移后才会出问题"的两类行为：
//   1. Backup 在 WAL 下的正确性 + 两个实测过的坑（目标已存在、父目录不存在）
//   2. 写锁竞争时 IsBusy 的识别与自动重试（busy_timeout 之外的那层兜底）

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// openTestDB 在 t.TempDir 下建一个全新的库。
func openTestDB(t *testing.T) *DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestBackup(t *testing.T) {
	d := openTestDB(t)

	// 写入可验证的数据（含中文与金额字符串，覆盖 TEXT 承载的列）
	if _, err := d.Exec(`INSERT INTO companies(name,nkey) VALUES($1,$2)`, "测试公司", "test-company"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := d.Exec(`INSERT INTO instruments(market,symbol,asset_class,name) VALUES('cn','600519','stock','贵州茅台')`); err != nil {
		t.Fatalf("insert instrument: %v", err)
	}

	dst := filepath.Join(t.TempDir(), "backup.db")
	if err := d.Backup(dst); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	// 备份文件必须存在且非空
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat backup: %v", err)
	}
	if fi.Size() == 0 {
		t.Fatal("备份文件大小为 0")
	}

	// 独立打开备份，读回数据（这是"备份可用"的唯一判据）
	bak, err := sql.Open("sqlite", BuildDSN(dst))
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer bak.Close()

	var name string
	if err := bak.QueryRow(`SELECT name FROM companies WHERE nkey=$1`, "test-company").Scan(&name); err != nil {
		t.Fatalf("读回 companies: %v", err)
	}
	if name != "测试公司" {
		t.Fatalf("数据不一致: got %q", name)
	}
	var symbol, iname string
	if err := bak.QueryRow(`SELECT symbol,name FROM instruments WHERE market='cn'`).Scan(&symbol, &iname); err != nil {
		t.Fatalf("读回 instruments: %v", err)
	}
	if symbol != "600519" || iname != "贵州茅台" {
		t.Fatalf("数据不一致: %s/%s", symbol, iname)
	}

	// 备份里也必须有 schema（不是空库）
	var tables int
	if err := bak.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`).Scan(&tables); err != nil {
		t.Fatalf("count tables: %v", err)
	}
	if tables < 60 {
		t.Fatalf("备份里表数异常: %d", tables)
	}
}

// TestBackupOverwritesExistingTarget 覆盖实测坑之一：
// VACUUM INTO 目标已存在会报 "output file already exists (1)"。
// Backup 的语义是覆盖，所以第二次调用必须也成功。
func TestBackupOverwritesExistingTarget(t *testing.T) {
	d := openTestDB(t)
	dst := filepath.Join(t.TempDir(), "backup.db")

	if err := d.Backup(dst); err != nil {
		t.Fatalf("第一次 Backup: %v", err)
	}
	first, _ := os.Stat(dst)

	// 再写点数据，第二次备份应覆盖且包含新数据
	if _, err := d.Exec(`INSERT INTO companies(name,nkey) VALUES('第二次','second')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := d.Backup(dst); err != nil {
		t.Fatalf("第二次 Backup（应覆盖）: %v", err)
	}
	second, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if second.Size() < first.Size() {
		t.Fatalf("覆盖后文件反而变小: %d -> %d", first.Size(), second.Size())
	}

	bak, err := sql.Open("sqlite", BuildDSN(dst))
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer bak.Close()
	var n int
	if err := bak.QueryRow(`SELECT COUNT(*) FROM companies WHERE nkey='second'`).Scan(&n); err != nil {
		t.Fatalf("读回: %v", err)
	}
	if n != 1 {
		t.Fatalf("覆盖后不含新数据: n=%d", n)
	}
}

// TestBackupCreatesParentDir 覆盖实测坑之二：
// 目标目录不存在会报 "unable to open database file (14)"。
func TestBackupCreatesParentDir(t *testing.T) {
	d := openTestDB(t)
	dst := filepath.Join(t.TempDir(), "a", "b", "c", "backup.db")

	if err := d.Backup(dst); err != nil {
		t.Fatalf("Backup 到不存在的多级目录: %v", err)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("备份文件未生成: %v", err)
	}
}

// TestBackupRejectsEmptyPath 空路径应明确报错，而不是生成一个奇怪的文件。
func TestBackupRejectsEmptyPath(t *testing.T) {
	d := openTestDB(t)
	if err := d.Backup("   "); err == nil {
		t.Fatal("空路径应当报错")
	}
}

// TestIsBusyRecognisesLockError 验证 IsBusy 能识别真实的写锁超时。
//
// 做法：另一条连接持写事务，本连接写 → busy_timeout 用尽 → SQLITE_BUSY。
// 这是唯一能稳定产出 BUSY 的方式（*sqlite.Error 字段未导出，无法构造）。
func TestBusyRecognisesLockError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "busy.db")
	d, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	// 用一条独立连接持有写锁
	holder, err := sql.Open("sqlite", BuildDSN(path))
	if err != nil {
		t.Fatalf("open holder: %v", err)
	}
	defer holder.Close()
	// 关掉重试，让它尽快失败
	holder.SetMaxOpenConns(1)

	tx, err := holder.Begin()
	if err != nil {
		t.Fatalf("holder begin: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO companies(name,nkey) VALUES('holder','holder')`); err != nil {
		t.Fatalf("holder insert: %v", err)
	}

	// 直接走底层 DB（绕开 withBusyRetry），产出原始 BUSY 错误
	_, rawErr := d.DB.Exec(`INSERT INTO companies(name,nkey) VALUES('blocked','blocked')`)
	if rawErr == nil {
		tx.Rollback()
		t.Fatal("期望写锁冲突，但插入成功了")
	}
	if !IsBusy(rawErr) {
		tx.Rollback()
		t.Fatalf("IsBusy 未识别写锁冲突: %v", rawErr)
	}
	tx.Rollback()

	// 释放后同池应恢复（BUSY 不污染连接池）
	if _, err := d.DB.Exec(`INSERT INTO companies(name,nkey) VALUES('after','after')`); err != nil {
		t.Fatalf("释放锁后写入失败: %v", err)
	}
}

// TestExecRetriesOnBusy 验证 DB.Exec 的自动重试真的生效。
//
// 场景：另一连接持写锁 300ms 后释放，本连接 Exec 应当重试成功而不是报错。
// 用短持有时间确保落在 busy_timeout(5s) 内，重试路径与 timeout 路径都能覆盖。
func TestBusyExecRetries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "retry.db")
	d, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	holder, err := sql.Open("sqlite", BuildDSN(path))
	if err != nil {
		t.Fatalf("open holder: %v", err)
	}
	defer holder.Close()

	// 先把 busy_timeout 压到很小，强制走"busy_timeout 用尽 → 重试"这条路，
	// 否则默认 5s 会一直等，测不到重试逻辑。
	if _, err := d.DB.Exec(`PRAGMA busy_timeout(50)`); err != nil {
		t.Fatalf("set busy_timeout: %v", err)
	}

	tx, err := holder.Begin()
	if err != nil {
		t.Fatalf("holder begin: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO companies(name,nkey) VALUES('holder2','holder2')`); err != nil {
		t.Fatalf("holder insert: %v", err)
	}

	var released atomic.Bool
	go func() {
		time.Sleep(300 * time.Millisecond)
		tx.Rollback()
		released.Store(true)
	}()

	// 这条 Exec 会在 busy_timeout(50ms) 后失败一次，然后由 withBusyRetry 重试；
	// 300ms 后 holder 释放，后续重试应当成功。
	done := make(chan error, 1)
	go func() {
		_, err := d.Exec(`INSERT INTO companies(name,nkey) VALUES('retried','retried')`)
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Exec 在释放锁后仍失败（重试没生效）: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Exec 超时（可能重试逻辑死循环或未退出）")
	}

	if !released.Load() {
		t.Fatal("测试自身有问题：holder 未释放")
	}

	// 数据必须真的写进去了
	var n int
	if err := d.DB.QueryRow(`SELECT COUNT(*) FROM companies WHERE nkey='retried'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("重试后数据缺失: n=%d", n)
	}
}

// TestIsBusyInjection 验证注入钩子真的被 withBusyRetry 使用。
//
// 为什么需要这个测试：*sqlite.Error 的字段未导出，无法构造"看起来像 BUSY"的错误。
// 所以生产代码留了 isBusyFn 这个 var 入口，测试替换它来驱动重试分支。
// 如果哪天有人把 isBusyFn 直接改成调用 IsBusy（去掉间接层），这个测试会失败，
// 提醒他"测试能力会一起消失"。
func TestBusyInjectionHook(t *testing.T) {
	orig := isBusyFn
	t.Cleanup(func() { isBusyFn = orig })

	d := openTestDB(t)

	// 让 isBusyFn 说"这是 BUSY"，并让底层 Exec 真实失败。
	// 底层用非法 SQL 制造稳定失败，再看 withBusyRetry 是否按 BUSY 处理
	// （会重试 busyRetryDelays 次，因此 isBusyFn 被调用多次）。
	calls := 0
	isBusyFn = func(error) bool {
		calls++
		return true
	}
	_, err := d.Exec(`THIS IS NOT SQL`)
	if err == nil {
		t.Fatal("非法 SQL 应当报错")
	}
	if calls == 0 {
		t.Fatal("注入的 isBusyFn 未被调用：withBusyRetry 没走到判定")
	}
	// withBusyRetry 的结构是 for attempt := 0; attempt <= busyRetryDelays; attempt++，
	// 所以总共尝试 busyRetryDelays+1 次（初试 1 次 + 重试 busyRetryDelays 次），
	// 每次失败都会调一次 isBusyFn。
	if want := busyRetryDelays + 1; calls != want {
		t.Fatalf("重试次数不符: isBusyFn 被调用 %d 次，期望 %d", calls, want)
	}
}

// TestOpenIsIdempotent 验证重复 Open 同一个库不会失败（schema 幂等 + seed 幂等）。
func TestOpenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idem.db")

	d1, err := Open(path)
	if err != nil {
		t.Fatalf("第一次 Open: %v", err)
	}
	var agents1 int
	d1.QueryRow(`SELECT COUNT(*) FROM agents`).Scan(&agents1)
	d1.Close()

	d2, err := Open(path)
	if err != nil {
		t.Fatalf("第二次 Open: %v", err)
	}
	defer d2.Close()
	var agents2 int
	d2.QueryRow(`SELECT COUNT(*) FROM agents`).Scan(&agents2)

	if agents1 != agents2 {
		t.Fatalf("重复 Open 改变了 agents 数量: %d -> %d", agents1, agents2)
	}
	if agents1 == 0 {
		t.Fatal("seed 没有写入任何 agent")
	}

	// 第二次 Open 后 pragma 仍然生效（连接级设置）
	var fk int
	if err := d2.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatalf("read pragma: %v", err)
	}
	if fk != 1 {
		t.Fatalf("foreign_keys 未生效: %d", fk)
	}
}

// TestForeignKeyEnforced 验证外键真的被强制（漏设 foreign_keys=1 时这条会失败）。
func TestForeignKeyEnforced(t *testing.T) {
	d := openTestDB(t)
	_, err := d.Exec(`INSERT INTO paper_orders(mode,side,qty,account_id) VALUES('paper','buy','1',999999)`)
	if err == nil {
		t.Fatal("外键未生效：插入了不存在的 account_id")
	}
	if !IsForeignKeyViolation(err) {
		t.Fatalf("IsForeignKeyViolation 未识别: %v", err)
	}
}

// TestUniqueViolationDetected 验证唯一冲突判定（含 instruments 的列名匹配）。
func TestUniqueViolationDetected(t *testing.T) {
	d := openTestDB(t)
	if _, err := d.Exec(`INSERT INTO instruments(market,symbol,asset_class) VALUES('us','AAPL','stock')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	_, err := d.Exec(`INSERT INTO instruments(market,symbol,asset_class) VALUES('us','AAPL','stock')`)
	if err == nil {
		t.Fatal("唯一键未生效")
	}
	if !IsUniqueViolation(err) {
		t.Fatalf("IsUniqueViolation 未识别: %v", err)
	}
	// T01 实测回填：UNIQUE 报列名而不是约束名
	want := "instruments.market, instruments.symbol"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("message 里没有 %q: %v", want, err)
	}
}

// TestTimeColumnsUseTimestampDecl 把"时间列必须声明 TIMESTAMP"这条纪律锁进测试。
//
// 背景：驱动 modernc.org/sqlite 只在列声明类型精确等于 DATE/DATETIME/TIMESTAMP
// 时才把 TEXT 值转成 time.Time。若有人把某一列改回 TEXT，全项目该类字段的
// Scan 会集体报 "unsupported Scan, storing driver.Value type string into type *time.Time"。
// 这个测试在 schema 层面兜住，比等某个业务测试红更早暴露。
func TestTimeColumnsUseTimestampDecl(t *testing.T) {
	d := openTestDB(t)

	// 所有像是时间列（名字以 _at / _since / _until / ts 结尾或等于 ts）的列，
	// 声明类型必须是 TIMESTAMP / DATE / DATETIME 三者之一。
	rows, err := d.Query(`
		SELECT m.name AS tbl, p.name AS col, p.type AS decl
		  FROM sqlite_master m
		  JOIN pragma_table_info(m.name) p
		 WHERE m.type = 'table'
		   AND m.name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatalf("query table_info: %v", err)
	}
	defer rows.Close()

	// 例外：语义上是"整型时刻"而非 ISO 时间串的列。
	// traffic_evidence_snapshots.captured_at 在 PG schema 里就是 BIGINT（unix 秒），
	// 由 traffic 抓包侧写入，不参与 time.Time Scan。名字带 _at 只是历史命名。
	integerEpochCols := map[string]bool{
		"traffic_evidence_snapshots.captured_at": true,
	}

	tsSuffix := func(name string) bool {
		return name == "ts" || strings.HasSuffix(name, "_at") ||
			strings.HasSuffix(name, "_since") || strings.HasSuffix(name, "_until")
	}
	var bad []string
	for rows.Next() {
		var tbl, col, decl string
		if err := rows.Scan(&tbl, &col, &decl); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if !tsSuffix(col) {
			continue
		}
		if integerEpochCols[tbl+"."+col] {
			continue
		}
		up := strings.ToUpper(decl)
		if up != "TIMESTAMP" && up != "DATE" && up != "DATETIME" {
			bad = append(bad, tbl+"."+col+" -> "+decl)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(bad) > 0 {
		t.Fatalf("以下时间列声明不是 TIMESTAMP，time.Time Scan 会失败：\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// TestTimeColumnRejectsNumericLiteral 记录并复现亲和性陷阱。
//
// TIMESTAMP 是 NUMERIC 亲和性：往时间列写纯数字会被静默转成 INTEGER，
// 之后 time.Time Scan 报 "type int64"。这个测试的作用是**把坏行为钉住**，
// 让"未来某天有人以为写数字没事"时能立刻看到后果。
//
// 生产代码不允许这样写（见 ARCH §2.2 的写入纪律）。
func TestTimeColumnRejectsNumericLiteral(t *testing.T) {
	d := openTestDB(t)

	if _, err := d.Exec(`INSERT INTO companies(name,nkey,created_at) VALUES('num','num-key',1700000000)`); err != nil {
		t.Fatalf("insert numeric time: %v", err)
	}
	var kind string
	if err := d.QueryRow(`SELECT typeof(created_at) FROM companies WHERE nkey='num-key'`).Scan(&kind); err != nil {
		t.Fatalf("typeof: %v", err)
	}
	if kind != "integer" {
		t.Fatalf("亲和性行为变了：typeof=%s（此前实测为 integer）。"+
			"若驱动升级后改成 text，本测试与 ARCH §2.2 的陷阱说明需要同步更新", kind)
	}
	var tt time.Time
	err := d.QueryRow(`SELECT created_at FROM companies WHERE nkey='num-key'`).Scan(&tt)
	if err == nil {
		t.Fatal("写数字后 time.Time Scan 竟然成功了 —— 亲和性/驱动行为已变，请复核 ARCH §2.2")
	}
	if !strings.Contains(err.Error(), "int64") {
		t.Fatalf("错误信息与预期不符: %v", err)
	}
}

// TestJSONColumnsHaveValidCheck 把"JSON 列必须有 json_valid 校验"这条纪律锁进测试。
//
// 背景：PG 的 JSONB 类型本身拒绝非法 JSON；SQLite 的 TEXT 接受任何字节。
// 不加 CHECK 就等于静默丢掉一层数据完整性保护 —— 非法 JSON 会写进
// activity.metadata / exploration_nodes.payload 这类列，下游 json_extract
// 读到时返回 NULL 或报错，且极难定位来源。
//
// 这个测试在 schema 层面兜住：新增 JSON 列若忘了加 CHECK，这里立刻红。
func TestJSONColumnsHaveValidCheck(t *testing.T) {
	d := openTestDB(t)

	rows, err := d.Query(`
		SELECT m.name AS tbl, p.name AS col, p.type AS decl
		  FROM sqlite_master m
		  JOIN pragma_table_info(m.name) p
		 WHERE m.type = 'table'
		   AND m.name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatalf("query table_info: %v", err)
	}
	defer rows.Close()

	// 期望带 CHECK 的列：由 gen_ddl.py 的 hint=='json' 判定，等价于
	// "PG 里是 jsonb / jsonb[] / text[] / bigint[]"。这里用"列名白名单"太脆，
	// 改为直接查 sqlite_master 的建表语句里是否出现该列的 json_valid。
	var cols []struct{ tbl, col string }
	for rows.Next() {
		var tbl, col, decl string
		if err := rows.Scan(&tbl, &col, &decl); err != nil {
			t.Fatalf("scan: %v", err)
		}
		cols = append(cols, struct{ tbl, col string }{tbl, col})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	// 取出全部建表 SQL，供包含性检查
	ddl := map[string]string{}
	r2, err := d.Query(`SELECT name, sql FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatalf("query ddl: %v", err)
	}
	defer r2.Close()
	for r2.Next() {
		var n, sqlText string
		if err := r2.Scan(&n, &sqlText); err != nil {
			t.Fatalf("scan ddl: %v", err)
		}
		ddl[n] = sqlText
	}

	// 已知的 JSON 列清单（来源：schema.sql 里 jsonb / jsonb[] / text[] / bigint[]）。
	// 新增 JSON 列时必须同步加到这里 —— 这既是校验，也是一份"哪些列是 JSON"的文档。
	expectJSON := []string{
		"assets.task_ids", "assets.bound_domains", "assets.open_ports",
		"assets.record_value", "assets.technologies", "assets.auth",
		"assets.params", "assets.extra",
		"exploration_nodes.payload", "activity.metadata",
		"task_archives.warnings", "task_archives.source_task_ids",
		"task_archives.data_counts", "task_archives.aggregate_stats",
		"task_templates.intercept_rules",
		"mcp_servers.args", "mcp_servers.env", "mcp_tools_cache.schema",
		"tools.schema", "tools.agents", "tools.exec",
		"intercept_rules.pattern_params",
		"intercept_pending.tool_input", "intercept_pending.audit",
		"findings.asset_ids",
		"side_question_sessions.snapshot", "side_question_sessions.memory",
		"side_question_requests.model", "side_question_requests.usage",
		"side_question_requests.context_info",
		"notification_channels.config", "notification_channels.filter",
		"notification_events.snapshot",
		"market_credentials.extra",
		"ratings.evidence_node_ids", "ratings.review_reasons",
		"exec_accounts.market_support",
		"exec_authorizations.limits_snapshot",
		"execution_limits.whitelist_symbols",
		"draft_risk_verdicts.matched_rule_ids", "draft_risk_verdicts.rule_results",
		"draft_risk_verdicts.rules_snapshot",
		"orders_execution.raw_ack",
		"exec_reconciliations.local_snapshot", "exec_reconciliations.remote_snapshot",
		"exec_reconciliations.diff", "exec_audit.detail",
	}

	var missing []string
	for _, spec := range expectJSON {
		tbl, col := "", ""
		for i := 0; i < len(spec); i++ {
			if spec[i] == '.' {
				tbl, col = spec[:i], spec[i+1:]
				break
			}
		}
		if tbl == "" {
			t.Fatalf("测试自身错误：无法解析 %q", spec)
		}
		body, ok := ddl[tbl]
		if !ok {
			missing = append(missing, spec+"（表不存在）")
			continue
		}
		// 保留字列会被加双引号（如 "schema"），两种形态都要接受。
		low := strings.ToLower(body)
		lc := strings.ToLower(col)
		if !strings.Contains(low, "json_valid("+lc+")") &&
			!strings.Contains(low, "json_valid(\""+lc+"\")") {
			missing = append(missing, spec)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("以下 JSON 列缺少 CHECK (json_valid(...))：\n  %s\n"+
			"修复：改 tools_schema/gen_ddl.py 后重新生成（见 ARCH §2.0.1）",
			strings.Join(missing, "\n  "))
	}

	_ = cols
}

// TestNumericChecksCompareNumerically 锁住"numeric 列的比较必须是数值比较"。
//
// 背景：PG 的 NUMERIC 在 SQLite 里是 TEXT，TEXT 的 <= 是字典序：
// '3' <= '10' 为 false。若忘了 CAST，最常见的部分成交（qty=10, filled=3）
// 会被 CHECK 拒绝，实盘执行链路直接不可用（P0，实测过）。
//
// 这个测试直接对真实表写入，验证约束行为而不是只看 schema 文本。
func TestNumericChecksCompareNumerically(t *testing.T) {
	d := openTestDB(t)

	var acctID, instID int64
	if err := d.QueryRow(`INSERT INTO exec_accounts(channel,channel_account_id,market)
		VALUES('okx','num-check','us') RETURNING id`).Scan(&acctID); err != nil {
		t.Fatalf("建账户: %v", err)
	}
	if err := d.QueryRow(`INSERT INTO instruments(market,symbol,asset_class)
		VALUES('us','NUMCHK','stock') RETURNING id`).Scan(&instID); err != nil {
		t.Fatalf("建标的: %v", err)
	}

	seq := 0
	try := func(qty, filled string, wantOK bool) {
		t.Helper()
		seq++
		var did int64
		if err := d.QueryRow(`INSERT INTO orders_draft(side,qty,instrument_id)
			VALUES('buy',$1,$2) RETURNING id`, qty, instID).Scan(&did); err != nil {
			t.Fatalf("建 draft: %v", err)
		}
		_, err := d.Exec(`INSERT INTO orders_execution(
			draft_id,account_id,mode,state,client_order_id,qty,filled_qty)
			VALUES($1,$2,'paper','submitted',$3,$4,$5)`,
			did, acctID, fmt.Sprintf("num-chk-%d", seq), qty, filled)
		got := err == nil
		if got != wantOK {
			want := "成功"
			if !wantOK {
				want = "被拒"
			}
			actual := "成功"
			if err != nil {
				actual = err.Error()
			}
			t.Fatalf("qty=%s filled_qty=%s 期望%s，实际：%s", qty, filled, want, actual)
		}
	}

	// 关键：正常部分成交必须通过（这是 P0 场景）
	try("10", "3", true)
	try("100", "9", true)
	try("10", "10", true)
	try("10", "0", true)
	try("0.1", "0.05", true)

	// 超成交必须被拒
	try("10", "20", false)
	// qty 必须为正
	try("0", "0", false)
	// 微超（浮点边界）
	try("1", "1.0000000001", false)

	// 清理（orders_execution 的 FK 是 RESTRICT）
	d.Exec(`DELETE FROM orders_execution WHERE account_id=$1`, acctID)
	d.Exec(`DELETE FROM orders_draft WHERE instrument_id=$1`, instID)
	d.Exec(`DELETE FROM exec_accounts WHERE id=$1`, acctID)
	d.Exec(`DELETE FROM instruments WHERE id=$1`, instID)
}
