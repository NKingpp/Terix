package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// =====================================================================
// 原始数据快照索引(snapshots)数据层
// =====================================================================

var (
	// ErrSnapshotInvalid 表示入参非法(缺 source/endpoint/checksum 等)。
	ErrSnapshotInvalid = errors.New("invalid snapshot")
	// ErrSnapshotNotFound 表示按去重键查不到快照。
	ErrSnapshotNotFound = errors.New("snapshot not found")
	// ErrSnapshotConflict 表示同去重键对应不同内容，不能返回旧证据 ID。
	ErrSnapshotConflict = errors.New("snapshot content conflict")
)

// Snapshot is a row in the snapshots table.
// BodyRef 指向内容存储(文件/对象存储),不内联 body。
type Snapshot struct {
	ID          int64   `json:"id"`
	Source      string  `json:"source"`
	Endpoint    string  `json:"endpoint"`
	ParamsHash  string  `json:"params_hash"`
	CollectedAt string  `json:"collected_at"`
	BodyRef     *string `json:"body_ref,omitempty"`
	Checksum    string  `json:"checksum"`
	CreatedAt   string  `json:"created_at"`
}

// SnapshotInput 是 InsertSnapshot 的入参。
type SnapshotInput struct {
	Source      string
	Endpoint    string
	ParamsHash  string
	CollectedAt string // 必填,PG 可解析的时间字面量
	BodyRef     string
	Checksum    string
}

// SnapshotStore operates on the snapshots table.
type SnapshotStore struct{ db *DB }

// Snapshots returns the snapshot store associated with this DB.
func (d *DB) Snapshots() *SnapshotStore { return &SnapshotStore{db: d} }

// snapshotColumns 与 scanSnapshot 的扫描顺序必须一致。
//
// 时间列在 SQLite 里就是 ISO8601 文本，不需要 `::text`。
const snapshotColumns = `id, source, endpoint, params_hash, collected_at,
	body_ref, checksum, created_at`

// scanSnapshot 从一行结果读出一条 Snapshot。
func scanSnapshot(row interface{ Scan(...any) error }) (*Snapshot, error) {
	var (
		s    Snapshot
		body sql.NullString
	)
	if err := row.Scan(
		&s.ID, &s.Source, &s.Endpoint, &s.ParamsHash,
		&s.CollectedAt, &body, &s.Checksum, &s.CreatedAt,
	); err != nil {
		return nil, err
	}
	if body.Valid {
		v := body.String
		s.BodyRef = &v
	}
	return &s, nil
}

// validateSnapshotInput 校验必填字段。
func validateSnapshotInput(in SnapshotInput) error {
	if strings.TrimSpace(in.Source) == "" {
		return fmt.Errorf("%w: source 不能为空", ErrSnapshotInvalid)
	}
	if strings.TrimSpace(in.Endpoint) == "" {
		return fmt.Errorf("%w: endpoint 不能为空", ErrSnapshotInvalid)
	}
	if strings.TrimSpace(in.ParamsHash) == "" {
		return fmt.Errorf("%w: params_hash 不能为空", ErrSnapshotInvalid)
	}
	if strings.TrimSpace(in.Checksum) == "" {
		return fmt.Errorf("%w: checksum 不能为空", ErrSnapshotInvalid)
	}
	if strings.TrimSpace(in.CollectedAt) == "" {
		return fmt.Errorf("%w: collected_at 不能为空", ErrSnapshotInvalid)
	}
	return nil
}

// InsertSnapshot 写入一条快照。
// (source, endpoint, params_hash, collected_at 截断到秒) 相同且 checksum 相同，
// 返回已有记录 + created=false；内容不同时返回 ErrSnapshotConflict，
// 防止新数据引用旧证据，已有快照不覆盖。
//
// SQLite 侧：时间列是 TEXT（ISO8601 UTC 毫秒），去重键里的
// `date_trunc('second', collected_at)` 退化为 `substr(collected_at,1,19)`，
// 与 schema_sqlite.sql 的 `uq_snapshots_dedup` 表达式索引完全一致。
func (s *SnapshotStore) InsertSnapshot(in SnapshotInput) (snap *Snapshot, created bool, err error) {
	if err := validateSnapshotInput(in); err != nil {
		return nil, false, err
	}
	row := s.db.QueryRow(`
INSERT INTO snapshots(source, endpoint, params_hash, collected_at, body_ref, checksum)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (source, endpoint, params_hash, substr(collected_at,1,19))
DO NOTHING
RETURNING `+snapshotColumns,
		strings.TrimSpace(in.Source), strings.TrimSpace(in.Endpoint),
		strings.TrimSpace(in.ParamsHash), strings.TrimSpace(in.CollectedAt),
		normalizeOptText(in.BodyRef), strings.TrimSpace(in.Checksum),
	)
	snap, err = scanSnapshot(row)
	if err == nil {
		return snap, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}

	// ON CONFLICT DO NOTHING 不返回行 —— 命中已有记录,按去重键把它查回来。
	existing, err := s.GetSnapshotByDedupKey(
		in.Source, in.Endpoint, in.ParamsHash, in.CollectedAt)
	if err != nil {
		return nil, false, err
	}
	if existing.Checksum != strings.TrimSpace(in.Checksum) {
		return nil, false, fmt.Errorf("%w: source=%s endpoint=%s collected_at=%s existing_id=%d",
			ErrSnapshotConflict, in.Source, in.Endpoint, in.CollectedAt, existing.ID)
	}
	return existing, false, nil
}

// GetSnapshotByDedupKey 按去重键取一条快照。
// collected_at 在比较前截断到秒,与唯一索引表达式保持一致。
func (s *SnapshotStore) GetSnapshotByDedupKey(source, endpoint, paramsHash, collectedAt string) (*Snapshot, error) {
	if strings.TrimSpace(source) == "" || strings.TrimSpace(endpoint) == "" ||
		strings.TrimSpace(paramsHash) == "" || strings.TrimSpace(collectedAt) == "" {
		return nil, fmt.Errorf("%w: 去重键各字段都不能为空", ErrSnapshotInvalid)
	}
	row := s.db.QueryRow(`
SELECT `+snapshotColumns+` FROM snapshots
WHERE source = $1 AND endpoint = $2 AND params_hash = $3
  AND substr(collected_at,1,19) = substr($4,1,19)`,
		strings.TrimSpace(source), strings.TrimSpace(endpoint),
		strings.TrimSpace(paramsHash), strings.TrimSpace(collectedAt))
	snap, err := scanSnapshot(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrSnapshotNotFound
		}
		return nil, err
	}
	return snap, nil
}

// ListSnapshotsBySource 列出某来源的快照,最新在前。
// limit <=0 时用 defaultSnapshotListLimit,超过 maxSnapshotListLimit 时截断。
func (s *SnapshotStore) ListSnapshotsBySource(source string, limit int) ([]*Snapshot, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return nil, fmt.Errorf("%w: source 不能为空", ErrSnapshotInvalid)
	}
	if limit <= 0 {
		limit = defaultSnapshotListLimit
	}
	if limit > maxSnapshotListLimit {
		limit = maxSnapshotListLimit
	}
	rows, err := s.db.Query(`
SELECT `+snapshotColumns+` FROM snapshots WHERE source = $1
ORDER BY collected_at DESC, id DESC LIMIT $2`, source, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]*Snapshot, 0, 16)
	for rows.Next() {
		snap, err := scanSnapshot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, snap)
	}
	return out, rows.Err()
}

const (
	defaultSnapshotListLimit = 200
	maxSnapshotListLimit     = 1000
)

// ExistingIDs 返回 ids 中在 snapshots 表里真实存在的那些。
//
// 用途：emit_fact 校验 snapshot_ids —— 悬空 id 必须拒绝，因为这是证据回溯的
// 唯一通道，允许悬空等于放弃可审计性。
//
// 一次查询而不是逐个 SELECT：N 条证据逐个查会 N 次往返。
func (s *SnapshotStore) ExistingIDs(ids []int64) (map[int64]bool, error) {
	out := make(map[int64]bool, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(`SELECT id FROM snapshots WHERE id IN (SELECT CAST(value AS INTEGER) FROM json_each($1))`,
		int64SliceJSON(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}
