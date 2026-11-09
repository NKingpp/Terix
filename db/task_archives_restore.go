package db

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// CompleteTaskArchive performs the hot-store compaction only after the external
// package has been fully written and checksummed. The task/exploration rows remain
// as minimal ID stubs; all heavyweight task-owned rows move into the package.
func (d *DB) CompleteTaskArchive(
	archiveID int64,
	snapshot *TaskArchiveSnapshot,
	archivePath, sha256 string,
	originalSize, compressedSize int64,
) error {
	if snapshot == nil || snapshot.FormatVersion != TaskArchiveFormatVersion {
		return ErrTaskArchiveFormatMismatch
	}
	countsRaw, err := json.Marshal(snapshot.DataCounts)
	if err != nil {
		return err
	}
	statsRaw, err := json.Marshal(snapshot.AggregateStats)
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := coordinateWithSchemaMigration(tx); err != nil {
		return err
	}
	var taskID, expID int64
	var state string
	// 原 PG 版 `FOR UPDATE OF archive,task` 锁两行；SQLite 单写者（BEGIN IMMEDIATE）
	// 已全局互斥，行锁是多余的，删掉即等价（ARCH §3.3）。
	if err := tx.QueryRow(`SELECT archive.task_id,task.exploration_id,archive.state
FROM task_archives archive JOIN tasks task ON task.id=archive.task_id
WHERE archive.id=$1`, archiveID).Scan(&taskID, &expID, &state); err != nil {
		return err
	}
	if state != Archiving || taskID != snapshot.TaskID || expID != snapshot.ExplorationID {
		return fmt.Errorf("%w: archive snapshot identity/state mismatch", ErrTaskArchiveState)
	}
	var dependent int64
	err = tx.QueryRow(`SELECT child.id FROM task_relations relation
JOIN tasks child ON child.id=relation.task_id AND child.deleted_at IS NULL
WHERE relation.source_task_id=$1 LIMIT 1`, taskID).Scan(&dependent)
	if err == nil {
		return fmt.Errorf("%w: task %d", ErrTaskArchiveDependent, dependent)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	// PG 的 `LOCK TABLE assets, exploration_anchors IN SHARE ROW EXCLUSIVE MODE`
	// 用来挡并发改归属；SQLite 没有表级锁语法，BEGIN IMMEDIATE 已经是全库写互斥，
	// 直接删除（ARCH §3.3）。
	if _, err := tx.Exec(`DELETE FROM intercept_pending WHERE COALESCE(task_id,'')=$1`, strconv.FormatInt(taskID, 10)); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM llm_records WHERE COALESCE(task_id,'')=$1`, strconv.FormatInt(taskID, 10)); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM llm_usage WHERE COALESCE(task_id,'')=$1 OR exploration_id=$2`, strconv.FormatInt(taskID, 10), expID); err != nil {
		return err
	}
	for _, table := range []string{"skill_usage", "tool_usage"} {
		if _, err := tx.Exec(`DELETE FROM `+table+` WHERE task_id=$1 OR exploration_id=$2`, taskID, expID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM side_question_sessions WHERE task_id=$1`, taskID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM findings WHERE task_id=$1`, taskID); err != nil {
		return err
	}
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`DELETE FROM task_relations WHERE task_id=$1 OR source_task_id=$1`, []any{taskID}},
		{`DELETE FROM task_asset_links WHERE task_id=$1`, []any{taskID}},
		{`DELETE FROM task_llm_profiles WHERE task_id=$1`, []any{taskID}},
		{`DELETE FROM task_scope WHERE task_id=$1`, []any{taskID}},
		{`DELETE FROM task_constraints WHERE exploration_id=$1`, []any{expID}},
		{`DELETE FROM activity WHERE exploration_id=$1`, []any{expID}},
		{`DELETE FROM exploration_nodes WHERE exploration_id=$1`, []any{expID}},
	} {
		if _, err := tx.Exec(statement.query, statement.args...); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`
UPDATE assets SET task_ids=(
    SELECT COALESCE(json_group_array(value),'[]') FROM json_each(assets.task_ids)
    WHERE CAST(value AS INTEGER) <> $1
  )
WHERE EXISTS (SELECT 1 FROM json_each(assets.task_ids) WHERE CAST(value AS INTEGER)=$1)`, taskID); err != nil {
		return err
	}
	if len(snapshot.ExclusiveAssetIDs) > 0 {
		exclusiveRaw, err := json.Marshal(snapshot.ExclusiveAssetIDs)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM assets
WHERE EXISTS (SELECT 1 FROM json_each($2) WHERE CAST(value AS INTEGER)=assets.id)
  AND assets.company_id IS NULL
  AND NOT EXISTS (
      SELECT 1 FROM tasks task
      WHERE task.id<>$1 AND task.deleted_at IS NULL
        AND EXISTS (SELECT 1 FROM json_each(assets.task_ids) WHERE CAST(value AS INTEGER)=task.id)
  )
  AND NOT EXISTS (
      SELECT 1 FROM exploration_anchors anchor JOIN exploration_nodes node ON node.id=anchor.node_id
      JOIN tasks task ON task.exploration_id=node.exploration_id
      WHERE anchor.asset_id=assets.id AND task.id<>$1 AND task.deleted_at IS NULL
  )`, taskID, string(exclusiveRaw)); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE explorations SET description='',goal='',status='open' WHERE id=$1`, expID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE tasks SET
 name='',category_id=NULL,description='',goal='',paused=true,queued=false,queued_at=NULL,queue_mode='',
 llm_profile_id=NULL,active_llm_profile_id=NULL,llm_chain_revision=llm_chain_revision+1,
 company_id=NULL,parent_ref=NULL,timeout_seconds=0,coverage_enabled=true,pinned_at=NULL,
 first_run_at=NULL,deadline_at=NULL,archived_at=now(),deleted_at=now()
WHERE id=$1`, taskID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE task_archives SET
	 state=$2,phase='ready',progress=100,error='',archive_path=$3,sha256=$4,
	 original_size=$5,compressed_size=$6,data_counts=$7,aggregate_stats=$8,
	 format_version=$9,archived_at=now(),warnings='[]'
WHERE id=$1`, archiveID, ArchiveReady, archivePath, sha256, originalSize, compressedSize,
		string(countsRaw), string(statsRaw), snapshot.FormatVersion); err != nil {
		return err
	}
	return tx.Commit()
}

// RestoreTaskArchive restores PostgreSQL rows from a verified manifest. It is
// idempotent for accounting/traffic retry scenarios and returns non-fatal
// warnings for global objects that intentionally are not recreated.
func (d *DB) RestoreTaskArchive(archiveID int64, snapshot *TaskArchiveSnapshot, remainingTimeoutSeconds int64) ([]string, error) {
	return d.restoreTaskArchive(archiveID, snapshot, remainingTimeoutSeconds, nil)
}

// RestoreTaskArchiveWithLLMRecords restores a v2 package whose heavyweight LLM
// record history is stored as a sequence of JSON objects outside manifest.json.
func (d *DB) RestoreTaskArchiveWithLLMRecords(
	archiveID int64,
	snapshot *TaskArchiveSnapshot,
	remainingTimeoutSeconds int64,
	llmRecords io.Reader,
) ([]string, error) {
	if llmRecords == nil {
		return nil, errors.New("nil streamed LLM record reader")
	}
	return d.restoreTaskArchive(archiveID, snapshot, remainingTimeoutSeconds, llmRecords)
}

func (d *DB) restoreTaskArchive(
	archiveID int64,
	snapshot *TaskArchiveSnapshot,
	remainingTimeoutSeconds int64,
	llmRecords io.Reader,
) ([]string, error) {
	if snapshot == nil || !IsTaskArchiveFormatSupported(snapshot.FormatVersion) {
		return nil, ErrTaskArchiveFormatMismatch
	}
	streamedLLMRecords := snapshot.StreamedTables["llm_records"]
	if (len(snapshot.StreamedTables) > 0 && snapshot.FormatVersion < 2) ||
		len(snapshot.StreamedTables) > 1 ||
		(len(snapshot.StreamedTables) == 1 && streamedLLMRecords != TaskArchiveLLMRecordsPath) {
		return nil, fmt.Errorf("%w: unsupported streamed table metadata", ErrTaskArchiveFormatMismatch)
	}
	if streamedLLMRecords != "" && llmRecords == nil {
		return nil, fmt.Errorf("%w: streamed LLM records are missing", ErrTaskArchiveFormatMismatch)
	}
	// # 为什么整个还原跑在一条专用连接上，且暂存发生在 BEGIN 之前
	//
	// 流式 llm_records 要走磁盘文件逐行 json.Decode（一个任务可上万行）。
	// 若把它放进写事务里，事务时长与归档包大小成正比，期间全库写入排队，
	// 超过 busy_timeout(5s) 后并发写方直接拿 SQLITE_BUSY（ARCH §6.4.2 #1）。
	//
	// 所以先在这条连接上把流解析进 TEMP 暂存表 —— 实测 TEMP 表写的是独立的
	// temp 库，**不占主库写锁**（另一连接持写事务时 TEMP 写入 0ms 成功）——
	// 再用一条事务做「TEMP → 主表」的一次性 INSERT…SELECT。原子性保持不变：
	// 主表上仍然只有一次提交点。
	ctx := context.Background()
	conn, err := d.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close() //nolint:errcheck
	stagedLLMRecords := int64(0)
	if streamedLLMRecords != "" {
		stagedLLMRecords, err = stageArchiveLLMRecords(ctx, conn, llmRecords)
		if err != nil {
			return nil, fmt.Errorf("restore llm_records (staging): %w", err)
		}
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := coordinateWithSchemaMigration(tx); err != nil {
		return nil, err
	}
	var taskID, expID int64
	var state string
	if err := tx.QueryRow(`SELECT archive.task_id,task.exploration_id,archive.state
FROM task_archives archive JOIN tasks task ON task.id=archive.task_id
WHERE archive.id=$1`, archiveID).Scan(&taskID, &expID, &state); err != nil {
		return nil, err
	}
	if state != Restoring || taskID != snapshot.TaskID || expID != snapshot.ExplorationID {
		return nil, fmt.Errorf("%w: restore snapshot identity/state mismatch", ErrTaskArchiveState)
	}
	warnings := []string{}
	assetMap, assetWarnings, err := restoreArchiveAssets(tx, taskID, snapshot.Tables["assets"])
	if err != nil {
		return nil, err
	}
	warnings = append(warnings, assetWarnings...)

	// The task row exists as an archived stub. Restore its global references only
	// when the current instance still owns them; never recreate categories/profiles.
	taskRows, err := decodeArchiveRows(snapshot.Tables["tasks"])
	if err != nil || len(taskRows) != 1 {
		return nil, fmt.Errorf("restore task row: expected one row: %w", err)
	}
	taskRow := taskRows[0]
	categoryID, _ := jsonInt64(taskRow["category_id"])
	if categoryID > 0 && !rowExists(tx, "task_categories", categoryID) {
		taskRow["category_id"] = nil
		warnings = append(warnings, fmt.Sprintf("任务分类 %d 已删除，已恢复为未分类", categoryID))
	}
	companyID, _ := jsonInt64(taskRow["company_id"])
	if companyID > 0 && !rowExists(tx, "companies", companyID) {
		taskRow["company_id"] = nil
		warnings = append(warnings, fmt.Sprintf("任务企业 %d 已删除，企业关联已跳过", companyID))
	}
	for _, key := range []string{"llm_profile_id", "active_llm_profile_id"} {
		profileID, _ := jsonInt64(taskRow[key])
		if profileID > 0 && !rowExists(tx, "llm_profiles", profileID) {
			taskRow[key] = nil
			warnings = append(warnings, fmt.Sprintf("LLM 配置 %d 已删除，已从任务配置中移除", profileID))
		}
	}
	if err := restoreExplorationStub(tx, snapshot.Tables["explorations"], expID); err != nil {
		return nil, err
	}
	if err := restoreTaskStub(tx, taskRow, taskID, remainingTimeoutSeconds); err != nil {
		return nil, err
	}

	remappedTables, err := remapArchiveAssetReferences(snapshot.Tables, assetMap)
	if err != nil {
		return nil, err
	}
	remappedTables["findings"], err = normalizeArchivedFindingVersions(remappedTables["findings"])
	if err != nil {
		return nil, err
	}
	// Insert graph rows in foreign-key order. The archived stub has no graph rows,
	// so an ID conflict signals external corruption and must stop the restore.
	for _, table := range []string{"exploration_nodes", "exploration_edges", "exploration_anchors", "task_constraints", "activity"} {
		if err := insertArchiveRows(tx, table, remappedTables[table]); err != nil {
			return nil, fmt.Errorf("restore %s: %w", table, err)
		}
	}
	if warning, err := restoreTaskRelations(tx, taskID, remappedTables["task_relations"]); err != nil {
		return nil, err
	} else {
		warnings = append(warnings, warning...)
	}
	for _, table := range []string{"side_question_sessions", "side_question_requests"} {
		if err := insertArchiveRows(tx, table, remappedTables[table]); err != nil {
			return nil, fmt.Errorf("restore %s: %w", table, err)
		}
	}
	if warning, err := restoreTaskScopes(tx, remappedTables["task_scope"]); err != nil {
		return nil, err
	} else {
		warnings = append(warnings, warning...)
	}
	if warning, err := restoreTaskLLMProfiles(tx, remappedTables["task_llm_profiles"]); err != nil {
		return nil, err
	} else {
		warnings = append(warnings, warning...)
	}
	for _, table := range []string{"task_asset_links", "findings"} {
		if err := insertArchiveRows(tx, table, remappedTables[table]); err != nil {
			return nil, fmt.Errorf("restore %s: %w", table, err)
		}
	}
	if err := restoreFindingTrafficTx(tx, snapshot); err != nil {
		return nil, fmt.Errorf("restore finding traffic: %w", err)
	}
	if streamedLLMRecords != "" {
		// TEMP 暂存表已在事务外灌好；这里一条语句落进主表（原子且短）。
		if _, err := tx.Exec(`INSERT INTO llm_records(
    id, ts, model, profile_name, session_id, task_id, worker, latency_ms,
    input_tokens, output_tokens, cache_read, cache_write, status, error,
    request_body, response_body, raw_request, raw_response)
SELECT id, ts, model, profile_name, session_id, task_id, worker, latency_ms,
    input_tokens, output_tokens, cache_read, cache_write, status, error,
    request_body, response_body, raw_request, raw_response
FROM temp.artex_archive_llm_records`); err != nil {
			return nil, fmt.Errorf("restore llm_records: %w", err)
		}
		if expected := snapshot.DataCounts["llm_records"]; stagedLLMRecords != expected {
			return nil, fmt.Errorf("restore llm_records: row count %d does not match manifest %d", stagedLLMRecords, expected)
		}
	} else if err := insertArchiveRows(tx, "llm_records", remappedTables["llm_records"]); err != nil {
		return nil, fmt.Errorf("restore llm_records: %w", err)
	}
	for _, table := range []string{"llm_usage", "skill_usage", "tool_usage"} {
		if err := insertArchiveRows(tx, table, remappedTables[table]); err != nil {
			return nil, fmt.Errorf("restore %s: %w", table, err)
		}
	}
	if err := restoreInterceptRows(tx, remappedTables["intercept_pending"]); err != nil {
		return nil, err
	}
	warningsRaw, _ := json.Marshal(warnings)
	if _, err := tx.Exec(`UPDATE task_archives SET warnings=$2,phase='database_restored',progress=85,error='' WHERE id=$1`, archiveID, string(warningsRaw)); err != nil {
		return nil, err
	}
	return warnings, tx.Commit()
}

// restoreExplorationStub 用存档行覆盖探索记录。
//
// SQLite 没有 `json_populate_record`（它靠列顺序把 JSON 灌成一行），所以改成
// 逐字段 `json_extract` 赋值：字段名显式写在 SQL 里，改 schema 时立刻可见。
func restoreExplorationStub(tx *sql.Tx, raw json.RawMessage, expID int64) error {
	row := string(firstArchiveRow(raw))
	_, err := tx.Exec(`UPDATE explorations SET
 description=json_extract($2,'$.description'),
 goal=json_extract($2,'$.goal'),
 status=json_extract($2,'$.status'),
 created_at=COALESCE(json_extract($2,'$.created_at'), created_at),
 updated_at=COALESCE(json_extract($2,'$.updated_at'), updated_at)
WHERE id=$1`, expID, row)
	return err
}

func restoreTaskStub(tx *sql.Tx, row map[string]any, taskID, remaining int64) error {
	raw, err := json.Marshal(row)
	if err != nil {
		return err
	}
	// json_populate_record 没有 SQLite 等价物 → 逐字段 json_extract。
	// `$2` 是单行 JSON 对象（不是数组）。
	_, err = tx.Exec(`UPDATE tasks SET
 name=json_extract($2,'$.name'),
 category_id=json_extract($2,'$.category_id'),
 description=json_extract($2,'$.description'),
 goal=json_extract($2,'$.goal'),
 status=json_extract($2,'$.status'),
 paused=json_extract($2,'$.paused'),
 queued=false, queued_at=NULL, queue_mode='',
 llm_profile_id=json_extract($2,'$.llm_profile_id'),
 active_llm_profile_id=json_extract($2,'$.active_llm_profile_id'),
 llm_chain_revision=json_extract($2,'$.llm_chain_revision'),
 company_id=json_extract($2,'$.company_id'),
 parent_ref=json_extract($2,'$.parent_ref'),
 timeout_seconds=json_extract($2,'$.timeout_seconds'),
 plan_heartbeat_seconds=json_extract($2,'$.plan_heartbeat_seconds'),
 coverage_enabled=json_extract($2,'$.coverage_enabled'),
 pinned_at=json_extract($2,'$.pinned_at'),
 first_run_at=json_extract($2,'$.first_run_at'),
 deadline_at=CASE WHEN json_extract($2,'$.paused') AND $3>0
                  THEN strftime('%Y-%m-%dT%H:%M:%fZ','now','+'||$3||' seconds')
                  ELSE json_extract($2,'$.deadline_at') END,
 deleted_at=NULL, archived_at=NULL,
 completed_at=json_extract($2,'$.completed_at'),
 created_at=COALESCE(json_extract($2,'$.created_at'), created_at),
 updated_at=json_extract($2,'$.updated_at')
WHERE id=$1`, taskID, string(raw), remaining)
	return err
}

func firstArchiveRow(raw json.RawMessage) json.RawMessage {
	var rows []json.RawMessage
	if json.Unmarshal(raw, &rows) != nil || len(rows) == 0 {
		return json.RawMessage("{}")
	}
	return rows[0]
}

func decodeArchiveRows(raw json.RawMessage) ([]map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var rows []map[string]any
	if err := decoder.Decode(&rows); err != nil {
		return nil, err
	}
	return rows, nil
}

func rowExists(tx *sql.Tx, table string, id int64) bool {
	if table != "task_categories" && table != "llm_profiles" && table != "companies" && table != "tasks" && table != "assets" {
		return false
	}
	var exists bool
	_ = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM `+table+` WHERE id=$1)`, id).Scan(&exists)
	return exists
}

func insertArchiveRows(tx *sql.Tx, table string, raw json.RawMessage) error {
	// 归档行缺列时按 NULL 写入（绕过列 DEFAULT）—— 这是 json_populate_recordset
	// 的语义，SQLite 侧的 json_extract 对缺失键同样返回 NULL，行为一致。
	// 但 v3 之前的归档没有 side question 的 memory / context_info 字段，
	// 而这两列是 NOT NULL，所以先补一个空对象。
	if (table == "side_question_sessions" || table == "side_question_requests") && len(raw) > 0 {
		var rows []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &rows); err != nil {
			return err
		}
		field := "memory"
		if table == "side_question_requests" {
			field = "context_info"
		}
		for _, row := range rows {
			if len(row[field]) == 0 || string(row[field]) == "null" {
				row[field] = json.RawMessage(`{}`)
			}
		}
		var err error
		raw, err = json.Marshal(rows)
		if err != nil {
			return err
		}
	}
	allowed := map[string]bool{
		"exploration_nodes": true, "exploration_edges": true, "exploration_anchors": true,
		"task_constraints": true, "activity": true, "task_asset_links": true, "findings": true,
		"llm_records": true, "llm_usage": true, "skill_usage": true, "tool_usage": true,
		"side_question_sessions": true, "side_question_requests": true,
	}
	if !allowed[table] {
		return fmt.Errorf("archive restore table %q is not allowed", table)
	}
	if rawRowCount(raw) == 0 {
		return nil
	}
	// # 替代 json_populate_recordset
	//
	// PG 的 json_populate_recordset 按**列顺序**把 JSON 数组灌成行；SQLite 没有
	// 等价物（也没有按列顺序取值的手段）。这里改成按列名取值：
	//
	//	INSERT INTO t("c1","c2",...) SELECT json_extract(value,'$."c1"'), ... FROM json_each($1)
	//
	// 语义等价性：json_populate_recordset 对**缺失的键填 NULL**（因此绕过 DEFAULT，
	// 见上面的注释）；json_extract 对缺失键同样返回 NULL。两边一致。
	// NOT NULL 且无 DEFAULT 的列在两边都会报约束错误 —— 归档行的列清单来自
	// `SELECT *`，本来就齐全，不会命中。
	//
	// 列清单在运行期从 pragma_table_info 取，而不是硬编码：加列时不用改这里。
	// 表名走白名单（上面的 allowed），列名从 pragma 读出来再加双引号，无注入面。
	columns, err := tableColumns(tx, table)
	if err != nil {
		return err
	}
	_, err = tx.Exec(buildArchiveInsertRows(table, columns, ""), string(raw))
	return err
}

// stageArchiveLLMRecords 把 NDJSON 流解析进 TEMP 暂存表，返回行数。
//
// # 为什么暂存表是 TEMP 而不是普通表
//
// TEMP 表住在 SQLite 的 temp 库里，**写入不占主库写锁**（实测：另一连接持写事务
// 时 TEMP 写入 0ms 成功）。所以「解析上万行 NDJSON」这段耗时操作完全不阻塞
// 其它写方 —— 这正是 ARCH §6.4.2 #1 要求的「IO 移出写事务」。
//
// # 为什么仍然逐行 Exec
//
// 实测 20k 行：逐行 prepared Exec(同一事务) 645ms，分块 json_each 646ms —— 没有
// 差别，因为瓶颈是每行的 JSON 编解码而不是提交次数（全程只提交一次）。逐行流式
// 写入的内存占用是常数，不需要把整个归档包读进内存拼一个大数组。
//
// 表名固定为 temp.artex_archive_llm_records，由调用方用 INSERT..SELECT 一次性
// 搬进主表，主表上仍然只有一个提交点（原子性不变）。
func stageArchiveLLMRecords(ctx context.Context, conn *sql.Conn, reader io.Reader) (int64, error) {
	columns, err := tableColumnsContext(ctx, conn, "llm_records")
	if err != nil {
		return 0, err
	}
	quoted := make([]string, len(columns))
	ph := make([]string, len(columns))
	for i, col := range columns {
		quoted[i] = quoteIdent(col)
		ph[i] = fmt.Sprintf("$%d", i+1)
	}
	create := `CREATE TEMP TABLE IF NOT EXISTS artex_archive_llm_records (` +
		strings.TrimSuffix(strings.Join(quoted, " TEXT,"), ",") + ` TEXT)`
	if _, err := conn.ExecContext(ctx, create); err != nil {
		return 0, err
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM temp.artex_archive_llm_records`); err != nil {
		return 0, err
	}
	statement, err := conn.PrepareContext(ctx,
		`INSERT INTO temp.artex_archive_llm_records(`+strings.Join(quoted, ",")+`) VALUES (`+strings.Join(ph, ",")+`)`)
	if err != nil {
		return 0, err
	}
	defer statement.Close()
	decoder := json.NewDecoder(reader)
	decoder.UseNumber()
	var count int64
	for {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return 0, err
		}
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			return 0, errors.New("streamed archive row must be a JSON object")
		}
		var row map[string]any
		if err := json.Unmarshal(trimmed, &row); err != nil {
			return 0, err
		}
		args := make([]any, len(columns))
		for i, col := range columns {
			args[i] = normalizeArchiveValue(row[col])
		}
		if _, err := statement.ExecContext(ctx, args...); err != nil {
			return 0, err
		}
		count++
	}
	return count, nil
}

// normalizeArchiveValue 把 JSON 解出来的值转成驱动能绑的类型。
//
// json.Number 必须转成 int64/float64，否则驱动会按字符串绑定，写进
// INTEGER 列后变成 TEXT 亲和性值，后续 ORDER BY / 比较会错。
func normalizeArchiveValue(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return n
		}
		if f, err := x.Float64(); err == nil {
			return f
		}
		return x.String()
	case bool:
		if x {
			return int64(1)
		}
		return int64(0)
	case map[string]any, []any:
		encoded, err := json.Marshal(x)
		if err != nil {
			return nil
		}
		return string(encoded)
	default:
		return v
	}
}

// tableColumns 返回表的列名（按建表顺序）。
//
// 用 pragma_table_info 而不是硬编码：归档包里的列表是「归档时的 schema」，
// 加列/删列后这里自动跟随，不会静默错位。
func tableColumns(q interface {
	Query(query string, args ...any) (*sql.Rows, error)
}, table string) ([]string, error) {
	rows, err := q.Query(`SELECT name FROM pragma_table_info($1) ORDER BY cid`, table)
	if err != nil {
		return nil, err
	}
	return scanColumnNames(rows, table)
}

// tableColumnsContext 是 tableColumns 的 *sql.Conn 版本（*sql.Conn 只有
// QueryContext，没有 Query，所以要多一个入口而不是复用接口）。
func tableColumnsContext(ctx context.Context, conn *sql.Conn, table string) ([]string, error) {
	rows, err := conn.QueryContext(ctx, `SELECT name FROM pragma_table_info($1) ORDER BY cid`, table)
	if err != nil {
		return nil, err
	}
	return scanColumnNames(rows, table)
}

func scanColumnNames(rows *sql.Rows, table string) ([]string, error) {
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(columns) == 0 {
		return nil, fmt.Errorf("表 %s 不存在或没有列", table)
	}
	return columns, nil
}

// quoteIdent 给 SQLite 标识符加双引号。
//
// 必须转义内嵌的双引号（`"` → `""`）：列名来自 pragma（可信），但白名单里的
// 表名是常量，两边都走这个函数就不会因为以后有人加了个带引号的列名而破语法。
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// quoteStringLiteral 把字符串转成 SQL 单引号字面量（用于 JSON 路径 `$."col"`）。
func quoteStringLiteral(s string) string {
	return `'` + strings.ReplaceAll(s, `'`, `''`) + `'`
}

// appendTaskIDToAsset 把 taskID 追加到资产的 task_ids 数组（已存在则不动）。
//
// 原 PG 写法 `CASE WHEN $1=ANY(task_ids) THEN task_ids ELSE array_append(task_ids,$1) END`。
// SQLite 侧：存在性用 json_each 判断，追加用 json_insert('$[#]',…)（'$[#]' 是
// 「数组末尾」路径，等价 array_append）。需要 COALESCE 兜住脏值：task_ids 若
// 不是合法 JSON 数组，json_insert 会报错 —— 那种脏数据本身就不可恢复。
func appendTaskIDToAsset(tx *sql.Tx, taskID, assetID int64) error {
	_, err := tx.Exec(`UPDATE assets SET task_ids=CASE
    WHEN EXISTS (SELECT 1 FROM json_each(assets.task_ids) WHERE CAST(value AS INTEGER)=$1) THEN task_ids
    ELSE json_insert(task_ids, '$[#]', $1)
END
WHERE id=$2`, taskID, assetID)
	return err
}

// nextAutoincrementID 返回表的下一个 AUTOINCREMENT 值（不写入）。
//
// PG 用 `nextval(pg_get_serial_sequence(t,'id'))` 在脚本里抢号；SQLite 对应的是
// `sqlite_sequence`，它只在 AUTOINCREMENT 表插过行之后才存在。
//
// 返回的是 max(seq, max(id)) + 1，并把 sqlite_sequence 回写到该值 —— 这样
// 后续 INSERT 不会再发到这个号，等于「预约」（比 PG 的 nextval 更严格：PG 只是
// 推进序列，若调用方最终没用这个号就留了个空洞，这里不会）。
func nextAutoincrementID(tx *sql.Tx, table string) (int64, error) {
	var seq sql.NullInt64
	err := tx.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name=$1`, table).Scan(&seq)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	var maxID int64
	if err := tx.QueryRow(`SELECT COALESCE(max(id),0) FROM ` + quoteIdent(table)).Scan(&maxID); err != nil {
		return 0, err
	}
	next := maxID + 1
	if seq.Valid && seq.Int64 >= next {
		next = seq.Int64 + 1
	}
	if _, err := tx.Exec(`INSERT INTO sqlite_sequence(name, seq) VALUES($1,$2)
ON CONFLICT (name) DO UPDATE SET seq=excluded.seq`, table, next); err != nil {
		return 0, err
	}
	return next, nil
}

// buildArchiveInsertRows 处理**一批**行（$1 是 JSON 数组）。
//
// conflict 是完整的冲突子句（如 `ON CONFLICT(id) DO NOTHING`），空串表示不追加。
//
// ⚠️ SQLite 的 `ON CONFLICT DO NOTHING` **必须带冲突目标**：不带目标的写法在
// `INSERT … SELECT` 上直接语法错误（实测 `near "DO"`）。所以调用点要显式给出
// 主键或唯一键列。也不能图省事换成 `INSERT OR IGNORE` —— 后者连 NOT NULL 和
// CHECK 违规一起吞掉，而 PG 的 `ON CONFLICT DO NOTHING` 只吞唯一性冲突。
func buildArchiveInsertRows(table string, columns []string, conflict string) string {
	// json_each 逐元素展开，每个元素是 `value` 里的一个 JSON 对象。
	stmt := archiveInsertPrefix(table, columns, "value") + ` FROM json_each($1)`
	if conflict != "" {
		// ⚠️ `FROM json_each(...)` 后直接跟 `ON CONFLICT` 会被当成 JOIN 的 ON，
		// 报 `near "DO": syntax error`（实测）。插一句 `WHERE true` 断开歧义，
		// 语义不变（json_each 无过滤条件）。
		stmt += " WHERE true " + conflict
	}
	return stmt
}

// buildArchiveInsertRow 处理**单个**行对象（$1 是 JSON 对象）。
//
// 不能复用 buildArchiveInsertRows：`json_each` 对 JSON 对象是遍历它的**成员值**，
// 不是「当成一行」—— 直接喂对象会让每列都取到同一个成员（或报 malformed JSON）。
// 所以这里直接从 `$1` 取路径。
func buildArchiveInsertRow(table string, columns []string, conflict string) string {
	stmt := archiveInsertPrefix(table, columns, "$1")
	if conflict != "" {
		stmt += " " + conflict
	}
	return stmt
}

// archiveInsertPrefix 拼公用的
// `INSERT INTO t(col,…) SELECT json_extract(<src>,'$.col'),…`。
//
// src 是行来源表达式：批处理时是 json_each 的 `value`，单行时是 `$1`。
func archiveInsertPrefix(table string, columns []string, src string) string {
	quoted := make([]string, len(columns))
	exprs := make([]string, len(columns))
	for i, col := range columns {
		quoted[i] = quoteIdent(col)
		exprs[i] = `json_extract(` + src + `,'$.'||` + quoteStringLiteral(col) + `)`
	}
	return `INSERT INTO ` + quoteIdent(table) + `(` + strings.Join(quoted, ",") + `)
SELECT ` + strings.Join(exprs, ",")
}

func restoreArchiveAssets(tx *sql.Tx, taskID int64, raw json.RawMessage) (map[int64]int64, []string, error) {
	rows, err := decodeArchiveRows(raw)
	if err != nil {
		return nil, nil, err
	}
	mapping := make(map[int64]int64, len(rows))
	warnings := []string{}
	for _, row := range rows {
		oldID, ok := jsonInt64(row["id"])
		if !ok || oldID <= 0 {
			return nil, nil, errors.New("archived asset has invalid id")
		}
		if companyID, ok := jsonInt64(row["company_id"]); ok && companyID > 0 && !rowExists(tx, "companies", companyID) {
			row["company_id"] = nil
			row["company_source"] = "explicit"
			warnings = append(warnings, fmt.Sprintf("资产 %d 的企业 %d 已删除，已恢复为未归属", oldID, companyID))
		}
		if existing, found, err := findArchiveAssetNaturalID(tx, row); err != nil {
			return nil, nil, err
		} else if found {
			mapping[oldID] = existing
			if err := appendTaskIDToAsset(tx, taskID, existing); err != nil {
				return nil, nil, err
			}
			continue
		}
		candidate := oldID
		if rowExists(tx, "assets", oldID) {
			// PG 用 `nextval(pg_get_serial_sequence('assets','id'))` 抢一个新 id；
			// SQLite 的等价物是 `sqlite_sequence`（AUTOINCREMENT 的计数器）。
			// 直接用 max(seq)+1 并回写，保证后续 INSERT 不会撞到这个 id。
			nextID, err := nextAutoincrementID(tx, "assets")
			if err != nil {
				return nil, nil, err
			}
			candidate = nextID
			row["id"] = candidate
			warnings = append(warnings, fmt.Sprintf("资产 ID %d 已被占用，恢复为 %d", oldID, candidate))
		}
		row["task_ids"] = mergeJSONTaskID(row["task_ids"], taskID)
		assetRaw, _ := json.Marshal(row)
		columns, err := tableColumns(tx, "assets")
		if err != nil {
			return nil, nil, err
		}
		res, err := tx.Exec(buildArchiveInsertRow("assets", columns, "ON CONFLICT(id) DO NOTHING"), string(assetRaw))
		if err != nil {
			return nil, nil, err
		}
		inserted, _ := res.RowsAffected()
		if inserted == 0 {
			existing, found, findErr := findArchiveAssetNaturalID(tx, row)
			if findErr != nil || !found {
				return nil, nil, errors.Join(findErr, fmt.Errorf("could not restore asset %d", oldID))
			}
			candidate = existing
			if err := appendTaskIDToAsset(tx, taskID, existing); err != nil {
				return nil, nil, err
			}
		}
		mapping[oldID] = candidate
	}
	return mapping, warnings, nil
}

func mergeJSONTaskID(value any, taskID int64) []int64 {
	out := []int64{}
	seen := map[int64]bool{}
	if values, ok := value.([]any); ok {
		for _, item := range values {
			if id, ok := jsonInt64(item); ok && id > 0 && !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	if !seen[taskID] {
		out = append(out, taskID)
	}
	return out
}

func findArchiveAssetNaturalID(tx *sql.Tx, row map[string]any) (int64, bool, error) {
	typeName, _ := row["type"].(string)
	stringValue := func(key string) string {
		value, _ := row[key].(string)
		return value
	}
	var query string
	var args []any
	switch typeName {
	case "root_domain":
		query, args = `SELECT id FROM assets WHERE type='root_domain' AND domain=$1`, []any{stringValue("domain")}
	case "ip":
		query, args = `SELECT id FROM assets WHERE type='ip' AND ip=$1`, []any{stringValue("ip")}
	case "subdomain":
		query, args = `SELECT id FROM assets WHERE type='subdomain' AND domain=$1 AND COALESCE(record_type,'')=$2`, []any{stringValue("domain"), stringValue("record_type")}
	case "app":
		if bundle := stringValue("bundle_id"); bundle != "" {
			query, args = `SELECT id FROM assets WHERE type='app' AND bundle_id=$1`, []any{bundle}
		} else {
			query, args = `SELECT id FROM assets WHERE type='app' AND bundle_id IS NULL AND app_name=$1`, []any{stringValue("app_name")}
		}
	case "service":
		if stringValue("service_type") == "http" {
			query, args = `SELECT id FROM assets WHERE type='service' AND service_type='http' AND url=$1`, []any{stringValue("url")}
		} else {
			port, _ := jsonInt64(row["port"])
			query, args = `SELECT id FROM assets WHERE type='service' AND service_type='other' AND COALESCE(domain,'')=$1 AND COALESCE(ip,'')=$2 AND port=$3 AND service_name=$4`, []any{stringValue("domain"), stringValue("ip"), port, stringValue("service_name")}
		}
	case "endpoint":
		query, args = `SELECT id FROM assets WHERE type='endpoint' AND url=$1 AND method=$2`, []any{stringValue("url"), stringValue("method")}
	default:
		return 0, false, fmt.Errorf("unsupported archived asset type %q", typeName)
	}
	var id int64
	err := tx.QueryRow(query, args...).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return id, err == nil, err
}

func remapArchiveAssetReferences(tables map[string]json.RawMessage, mapping map[int64]int64) (map[string]json.RawMessage, error) {
	out := make(map[string]json.RawMessage, len(tables))
	for name, raw := range tables {
		out[name] = raw
	}
	for _, table := range []string{"exploration_anchors", "task_asset_links"} {
		rows, err := decodeArchiveRows(tables[table])
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			if old, ok := jsonInt64(row["asset_id"]); ok {
				if replacement, exists := mapping[old]; exists {
					row["asset_id"] = replacement
				}
			}
		}
		out[table], _ = json.Marshal(rows)
	}
	for _, table := range []string{"exploration_nodes", "findings"} {
		rows, err := decodeArchiveRows(tables[table])
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			key := "asset_ids"
			container := row
			if table == "exploration_nodes" {
				payload, ok := row["payload"].(map[string]any)
				if !ok {
					continue
				}
				container = payload
			}
			if values, ok := container[key].([]any); ok {
				for i, value := range values {
					if old, ok := jsonInt64(value); ok {
						if replacement, exists := mapping[old]; exists {
							values[i] = replacement
						}
					}
				}
				container[key] = values
			}
		}
		out[table], _ = json.Marshal(rows)
	}
	return out, nil
}

func restoreTaskRelations(tx *sql.Tx, taskID int64, raw json.RawMessage) ([]string, error) {
	rows, err := decodeArchiveRows(raw)
	if err != nil {
		return nil, err
	}
	warnings := []string{}
	for _, row := range rows {
		sourceID, ok := jsonInt64(row["source_task_id"])
		if !ok || !liveTaskExists(tx, sourceID) {
			warnings = append(warnings, fmt.Sprintf("来源任务 %d 不可用，继承关系已跳过", sourceID))
			continue
		}
		created, _ := row["created_at"].(string)
		if _, err := tx.Exec(`INSERT INTO task_relations(task_id,source_task_id,created_at)
VALUES($1,$2,COALESCE($3,now()))
ON CONFLICT (task_id, source_task_id) DO NOTHING`, taskID, sourceID, nilIfEmptyString(created)); err != nil {
			return nil, err
		}
	}
	return warnings, nil
}

func restoreTaskScopes(tx *sql.Tx, raw json.RawMessage) ([]string, error) {
	rows, err := decodeArchiveRows(raw)
	if err != nil {
		return nil, err
	}
	warnings := []string{}
	kept := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if companyID, ok := jsonInt64(row["company_id"]); ok && companyID > 0 && !rowExists(tx, "companies", companyID) {
			warnings = append(warnings, fmt.Sprintf("企业 %d 已删除，关联范围已跳过", companyID))
			continue
		}
		kept = append(kept, row)
	}
	if len(kept) == 0 {
		return warnings, nil
	}
	encoded, _ := json.Marshal(kept)
	// 原 `ON CONFLICT DO NOTHING` 没有目标；SQLite 要求显式目标，task_scope 的
	// 唯一性是主键 (id)，所以用 id 作冲突目标。
	columns, err := tableColumns(tx, "task_scope")
	if err != nil {
		return nil, err
	}
	_, err = tx.Exec(buildArchiveInsertRows("task_scope", columns, "ON CONFLICT(id) DO NOTHING"), string(encoded))
	return warnings, err
}

func restoreTaskLLMProfiles(tx *sql.Tx, raw json.RawMessage) ([]string, error) {
	rows, err := decodeArchiveRows(raw)
	if err != nil {
		return nil, err
	}
	warnings := []string{}
	position := 0
	for _, row := range rows {
		profileID, ok := jsonInt64(row["profile_id"])
		if !ok || !rowExists(tx, "llm_profiles", profileID) {
			warnings = append(warnings, fmt.Sprintf("LLM 配置 %d 已删除，配置链项已跳过", profileID))
			continue
		}
		taskID, _ := jsonInt64(row["task_id"])
		status, _ := row["status"].(string)
		lastError, _ := row["last_error"].(string)
		exhaustedAt, _ := row["exhausted_at"].(string)
		createdAt, _ := row["created_at"].(string)
		updatedAt, _ := row["updated_at"].(string)
		// 冲突目标是主键 (task_id, profile_id)：同一 profile 已在该任务链上时跳过。
		if _, err := tx.Exec(`INSERT INTO task_llm_profiles(task_id,profile_id,position,status,last_error,exhausted_at,created_at,updated_at)
VALUES($1,$2,$3,$4,NULLIF($5,''),$6,COALESCE($7,now()),COALESCE($8,now()))
ON CONFLICT (task_id, profile_id) DO NOTHING`, taskID, profileID, position, status, lastError,
			nilIfEmptyString(exhaustedAt), nilIfEmptyString(createdAt), nilIfEmptyString(updatedAt)); err != nil {
			return nil, err
		}
		position++
	}
	return warnings, nil
}

func restoreInterceptRows(tx *sql.Tx, raw json.RawMessage) error {
	rows, err := decodeArchiveRows(raw)
	if err != nil {
		return err
	}
	for _, row := range rows {
		// Archives predating approval snapshots lack this NOT NULL column.
		if source, _ := row["decision_source"].(string); source == "" {
			source = "unknown"
			if row["rule_id"] != nil {
				source = "rule"
			} else if reason, _ := row["reason"].(string); strings.HasPrefix(reason, "[模型]") {
				source = "model"
			}
			row["decision_source"] = source
		}
		if status, _ := row["status"].(string); status == "pending" {
			row["status"] = "timeout"
			row["reason"] = "任务归档期间审批已超时"
			row["decided_at"] = time.Now().UTC()
			if audit, ok := row["audit"].(map[string]any); ok {
				audit["effective_action"] = "deny"
				audit["decision_reason"] = "任务归档期间审批已超时"
				audit["execution_status"] = "not_executed"
			}
		} else if audit, ok := row["audit"].(map[string]any); ok && audit["execution_status"] == "awaiting_result" {
			// An archived run cannot resume its former result callback.
			audit["execution_status"] = "unknown"
		}
	}
	if len(rows) == 0 {
		return nil
	}
	encoded, _ := json.Marshal(rows)
	columns, err := tableColumns(tx, "intercept_pending")
	if err != nil {
		return err
	}
	_, err = tx.Exec(buildArchiveInsertRows("intercept_pending", columns, ""), string(encoded))
	return err
}

func liveTaskExists(tx *sql.Tx, id int64) bool {
	var exists bool
	_ = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM tasks WHERE id=$1 AND deleted_at IS NULL)`, id).Scan(&exists)
	return exists
}

func nilIfEmptyString(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

// CompleteTaskArchiveRestore removes compact metadata after every external
// component has been verified and the package has been consumed.
func (d *DB) CompleteTaskArchiveRestore(archiveID int64) error {
	// SQLite 没有 `DELETE … USING`，改成 EXISTS 子查询（语义等价）。
	res, err := d.Exec(`DELETE FROM task_archives
WHERE id=$1
  AND EXISTS (SELECT 1 FROM tasks task
              WHERE task.id=task_archives.task_id
                AND task.deleted_at IS NULL AND task.archived_at IS NULL)`, archiveID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ErrTaskArchiveState
	}
	return nil
}

// DeleteTaskArchiveStub permanently removes the cold task after its package has
// been staged for deletion. Dependency protection is rechecked transactionally.
func (d *DB) DeleteTaskArchiveStub(archiveID int64) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := coordinateWithSchemaMigration(tx); err != nil {
		return err
	}
	var taskID, expID int64
	var state string
	if err := tx.QueryRow(`SELECT archive.task_id,task.exploration_id,archive.state
FROM task_archives archive JOIN tasks task ON task.id=archive.task_id
WHERE archive.id=$1`, archiveID).Scan(&taskID, &expID, &state); err != nil {
		return err
	}
	if state != Deleting {
		return ErrTaskArchiveState
	}
	var dependent int64
	err = tx.QueryRow(`SELECT task_id FROM task_archives
WHERE id<>$1
  AND EXISTS (SELECT 1 FROM json_each(task_archives.source_task_ids) WHERE CAST(value AS INTEGER)=$2)
LIMIT 1`, archiveID, taskID).Scan(&dependent)
	if err == nil {
		return fmt.Errorf("%w: task %d", ErrTaskArchiveDeleteBlocked, dependent)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM tasks WHERE id=$1 AND archived_at IS NOT NULL`, taskID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM explorations WHERE id=$1`, expID); err != nil {
		return err
	}
	return tx.Commit()
}

// ArchivedAggregateStats returns compact summaries used by global dashboards so
// cold data does not disappear from historical totals.
func (d *DB) ArchivedAggregateStats() ([]json.RawMessage, error) {
	rows, err := d.Query(`SELECT archive.aggregate_stats
FROM task_archives archive
JOIN tasks task ON task.id=archive.task_id
WHERE task.archived_at IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []json.RawMessage
	for rows.Next() {
		var raw json.RawMessage
		if err := rows.Scan(jsonScan{&raw}); err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	return out, rows.Err()
}
