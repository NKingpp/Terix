package db

// 对账记录（exec_reconciliations）数据层。
//
// # 这张表是「发现不一致」的证据，不是「处理不一致」的工具
//
// 它记录：本地快照、通道快照、差异分类、谁在什么时候解决的。
// **不在本地自动修正** —— 自动修正会把「不知道哪边对」变成「按我们猜的改」，
// 而实盘场景下猜错一边就是真实的钱损失。
//
// 所以解析（resolved_at / resolved_by）是人工动作，代码只负责记录。

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Autumn-27/artex/redact"
)

// 对账差异分类。与 schema 的 CHECK 约束一一对应。
const (
	// ReconState 本地与通道的状态不一致（如本地 submitted、通道已 filled）。
	ReconState = "state"
	// ReconAmount 成交数量或均价不一致。
	ReconAmount = "amount"
	// ReconMissingLocal 通道有、本地没有（漏记了通道的单）。
	ReconMissingLocal = "missing_local"
	// ReconMissingRemote 本地有、通道没有（本地记了但通道根本没收到）。
	ReconMissingRemote = "missing_remote"
	// ReconQueryFailed 查询通道失败（**不是差异**，是「没查到」）。
	//
	// 与另外四种必须分开：前四种是「知道了不一致」，这一种是「不知道」。
	// 把「不知道」记成任意一种差异都会让对账报告失真。
	ReconQueryFailed = "query_failed"
)

// validReconKinds 是差异分类白名单，与 CHECK 一致。
var validReconKinds = map[string]bool{
	ReconState: true, ReconAmount: true,
	ReconMissingLocal: true, ReconMissingRemote: true,
	ReconQueryFailed: true,
}

// ErrReconNotFound 表示对账记录不存在。
var ErrReconNotFound = errors.New("reconciliation not found")

// ExecReconciliation 是 exec_reconciliations 的一行。
//
// 三个快照字段都用 json.RawMessage：它们的结构是通道相关的
// （fosun 与 okx 的持仓结构不同），本地不该解析。
type ExecReconciliation struct {
	ID             int64           `json:"id"`
	ExecutionID    *int64          `json:"execution_id,omitempty"`
	AccountID      int64           `json:"account_id"`
	Kind           string          `json:"kind"`
	LocalSnapshot  json.RawMessage `json:"local_snapshot"`
	RemoteSnapshot json.RawMessage `json:"remote_snapshot"`
	Diff           json.RawMessage `json:"diff"`
	ResolvedAt     *string         `json:"resolved_at,omitempty"`
	ResolvedBy     string          `json:"resolved_by"`
	CreatedAt      string          `json:"created_at"`
}

// ExecReconciliationInput 是 Record 的入参。
type ExecReconciliationInput struct {
	ExecutionID    *int64
	AccountID      int64
	Kind           string
	LocalSnapshot  json.RawMessage
	RemoteSnapshot json.RawMessage
	Diff           json.RawMessage
}

// ExecReconciliationStore 操作 exec_reconciliations 表。
type ExecReconciliationStore struct{ db *DB }

// ExecReconciliations 返回该 DB 关联的对账仓储。
func (d *DB) ExecReconciliations() *ExecReconciliationStore {
	return &ExecReconciliationStore{db: d}
}

// reconColumns 与 scanRecon 的扫描顺序必须一致。
const reconColumns = `id, execution_id, account_id, kind,
	local_snapshot, remote_snapshot, diff, resolved_at,
	COALESCE(resolved_by, ''), created_at`

// scanRecon 从一行结果读出一条 ExecReconciliation。
func scanRecon(row interface{ Scan(...any) error }) (*ExecReconciliation, error) {
	var (
		r           ExecReconciliation
		executionID sql.NullInt64
		localRaw    []byte
		remoteRaw   []byte
		diffRaw     []byte
		resolvedAt  sql.NullString
	)
	if err := row.Scan(&r.ID, &executionID, &r.AccountID, &r.Kind,
		&localRaw, &remoteRaw, &diffRaw, &resolvedAt,
		&r.ResolvedBy, &r.CreatedAt); err != nil {
		return nil, err
	}
	if executionID.Valid {
		v := executionID.Int64
		r.ExecutionID = &v
	}
	r.LocalSnapshot = normalizeJSON(localRaw)
	r.RemoteSnapshot = normalizeJSON(remoteRaw)
	r.Diff = normalizeJSON(diffRaw)
	if resolvedAt.Valid {
		s := resolvedAt.String
		r.ResolvedAt = &s
	}
	return &r, nil
}

// normalizeJSON 把可能为空的 JSONB 输出归一化成 `{}`，避免调用方拿到 nil
// 后 JSON 序列化成 `null`。
func normalizeJSON(raw []byte) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(raw)
}

// Record 写入一条对账记录。
//
// # kind 必须显式给，不给默认值
//
// 因为「默认成 state」会让 query_failed 被误记成状态差异 ——
// 而这两个的处置完全不同（一个要人工查、一个要重试查询）。
func (s *ExecReconciliationStore) Record(in ExecReconciliationInput) (int64, error) {
	kind := strings.TrimSpace(in.Kind)
	if !validReconKinds[kind] {
		return 0, fmt.Errorf("%w: 不支持的差异分类 %q（支持: state / amount / missing_local / missing_remote / query_failed）",
			ErrOrderInvalid, kind)
	}
	if in.AccountID <= 0 {
		return 0, fmt.Errorf("%w: account_id 必须为正", ErrOrderInvalid)
	}

	// 三个快照列都要脱敏。
	//
	// # 为什么在这里（唯一的写入点）而不是在调用方
	//
	// `Record` 是对账记录的唯一写入入口。在这里脱敏意味着：
	// 将来新增任何调用方，它写入的内容自动是干净的。
	//
	// 而如果在调用方（exec/reconcile.go）脱敏，那新增调用方必须记得也加 ——
	// 那依赖记性。
	//
	// # 为什么 remote_snapshot 尤其需要
	//
	// 它存的是通道的 raw_ack 原文。vaultd 出口已做过一次脱敏
	// （exec/vaultd 的 redactRaw），但那是「vaultd 的承诺」；
	// 这里是「对账表自己的保证」—— 两者是不同层次，不能互相替代。
	localSnap := redact.MaskJSON([]byte(normalizeJSON(in.LocalSnapshot)))
	remoteSnap := redact.MaskJSON([]byte(normalizeJSON(in.RemoteSnapshot)))
	diffSnap := redact.MaskJSON([]byte(normalizeJSON(in.Diff)))

	var id int64
	err := s.db.QueryRow(`
INSERT INTO exec_reconciliations
    (execution_id, account_id, kind, local_snapshot, remote_snapshot, diff)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id`,
		in.ExecutionID, in.AccountID, kind,
		localSnap, remoteSnap, diffSnap,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("写入对账记录失败: %w", err)
	}
	return id, nil
}

// GetReconciliation 按 id 取一条。
func (s *ExecReconciliationStore) GetReconciliation(id int64) (*ExecReconciliation, error) {
	row := s.db.QueryRow(`SELECT `+reconColumns+` FROM exec_reconciliations WHERE id=$1`, id)
	r, err := scanRecon(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrReconNotFound
		}
		return nil, err
	}
	return r, nil
}

// ListUnresolved 列出未解析的差异（按时间倒序 —— 最新的先看）。
//
// 这是对账界面的主查询：用户打开页面要看到「现在有哪些不一致待处理」。
func (s *ExecReconciliationStore) ListUnresolved(limit int) ([]*ExecReconciliation, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT `+reconColumns+`
FROM exec_reconciliations WHERE resolved_at IS NULL
ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*ExecReconciliation, 0, limit)
	for rows.Next() {
		r, err := scanRecon(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListByAccount 列出某账户的对账历史（含已解析的）。
func (s *ExecReconciliationStore) ListByAccount(accountID int64, limit int) ([]*ExecReconciliation, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT `+reconColumns+`
FROM exec_reconciliations WHERE account_id=$1
ORDER BY created_at DESC LIMIT $2`, accountID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*ExecReconciliation, 0, limit)
	for rows.Next() {
		r, err := scanRecon(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Resolve 标记一条差异已人工处理。
//
// resolvedBy 必填：对账是人工动作，「谁解决的」在事后追责时是第一问。
// 不允许匿名解析。
func (s *ExecReconciliationStore) Resolve(id int64, resolvedBy string) error {
	by := strings.TrimSpace(resolvedBy)
	if by == "" {
		return fmt.Errorf("%w: resolved_by 不能为空（对账解析必须记录责任人）", ErrOrderInvalid)
	}
	res, err := s.db.Exec(`UPDATE exec_reconciliations
SET resolved_at = now(), resolved_by = $2
WHERE id = $1 AND resolved_at IS NULL`, id, by)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// 两种可能：记录不存在，或已被别人解析。
		// 区分开：先查存在性，给调用方准确信息。
		if _, gErr := s.GetReconciliation(id); gErr != nil {
			return ErrReconNotFound
		}
		return fmt.Errorf("%w: 该差异已被解析", ErrReconNotFound)
	}
	return nil
}
