package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// =====================================================================
// 观点信号(signals)数据层
// =====================================================================

// 信号方向取值,与 schema.sql 的 CHECK 约束一致。
const (
	SignalDirectionLong    = "long"
	SignalDirectionShort   = "short"
	SignalDirectionNeutral = "neutral"
)

// 信号状态取值。status 在 schema 里没有 CHECK(与 findings.status 同策略,
// 由服务层白名单校验),这里给出常用值供调用方引用。
const (
	SignalStatusOpen      = "open"
	SignalStatusSettled   = "settled"
	SignalStatusCancelled = "cancelled"
)

var (
	// ErrSignalInvalid 表示入参非法(方向、必填字段、confidence 格式)。
	ErrSignalInvalid = errors.New("invalid signal")
	// ErrSignalNotFound 表示信号不存在。
	ErrSignalNotFound = errors.New("signal not found")
)

// validSignalDirections 是方向白名单。
var validSignalDirections = map[string]bool{
	SignalDirectionLong: true, SignalDirectionShort: true, SignalDirectionNeutral: true,
}

// Signal is a row in the signals table.
// Confidence 用 *string 承载 NUMERIC(5,4):NUMERIC 扫 float64 会丢精度,
// 且 0 与 NULL 语义不同(0 是「确信度为零」,NULL 是「未评」)。
type Signal struct {
	ID           int64   `json:"id"`
	TaskID       *int64  `json:"task_id,omitempty"`
	InstrumentID *int64  `json:"instrument_id,omitempty"`
	NodeID       *int64  `json:"node_id,omitempty"`
	SignalType   string  `json:"signal_type"`
	Direction    string  `json:"direction"`
	Confidence   *string `json:"confidence,omitempty"`
	Horizon      *string `json:"horizon,omitempty"`
	Rationale    *string `json:"rationale,omitempty"`
	Status       string  `json:"status"`
	CreatedAt    string  `json:"created_at"`
}

// SignalInput 是 InsertSignal 的入参。
// Confidence 为空字符串表示不填(NULL);非空则必须是合法十进制字面量。
type SignalInput struct {
	TaskID       *int64
	InstrumentID *int64
	NodeID       *int64
	SignalType   string
	// Action 是准备执行的动作：entry/exit/scale_in/scale_out/hold/review。
	// 与 SignalType 正交 —— SignalType 是信号来源（technical/fundamental/news），
	// Action 是准备做什么。空表示「只记录信号，未决定动作」。
	Action     string
	Direction  string
	Confidence string
	Horizon    string
	Rationale  string
	Status     string
}

// SignalStore operates on the signals table.
type SignalStore struct{ db *DB }

// Signals returns the signal store associated with this DB.
func (d *DB) Signals() *SignalStore { return &SignalStore{db: d} }

// signalColumns 与 scanSignal 的扫描顺序必须一致。
const signalColumns = `id, task_id, instrument_id, node_id, signal_type, direction,
	confidence, horizon, rationale, status, created_at`

// scanSignal 从一行结果读出一条 Signal。
func scanSignal(row interface{ Scan(...any) error }) (*Signal, error) {
	var (
		s        Signal
		taskID   sql.NullInt64
		instID   sql.NullInt64
		nodeID   sql.NullInt64
		conf     sql.NullString
		horizon  sql.NullString
		rational sql.NullString
	)
	if err := row.Scan(
		&s.ID, &taskID, &instID, &nodeID, &s.SignalType, &s.Direction,
		&conf, &horizon, &rational, &s.Status, &s.CreatedAt,
	); err != nil {
		return nil, err
	}
	s.TaskID = nullInt64Ptr(taskID)
	s.InstrumentID = nullInt64Ptr(instID)
	s.NodeID = nullInt64Ptr(nodeID)
	if conf.Valid {
		v := conf.String
		s.Confidence = &v
	}
	if horizon.Valid {
		v := horizon.String
		s.Horizon = &v
	}
	if rational.Valid {
		v := rational.String
		s.Rationale = &v
	}
	return &s, nil
}

// nullInt64Ptr 把 NULL 映射为 nil,非 NULL 映射为值指针。
func nullInt64Ptr(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}

// validateSignalInput 校验必填字段与枚举。
func validateSignalInput(in SignalInput) error {
	if strings.TrimSpace(in.SignalType) == "" {
		return fmt.Errorf("%w: signal_type 不能为空", ErrSignalInvalid)
	}
	if !validSignalDirections[strings.TrimSpace(in.Direction)] {
		return fmt.Errorf("%w: direction %q 不在 long/short/neutral 之内",
			ErrSignalInvalid, in.Direction)
	}
	if c := strings.TrimSpace(in.Confidence); c != "" {
		// 只做形状校验,真正的范围由 DB 的 NUMERIC(5,4) 兜底。
		for _, r := range c {
			if (r < '0' || r > '9') && r != '.' && r != '-' {
				return fmt.Errorf("%w: confidence %q 不是合法十进制数", ErrSignalInvalid, c)
			}
		}
	}
	return nil
}

// InsertSignal 写入一条信号并返回新 id。
func (s *SignalStore) InsertSignal(in SignalInput) (int64, error) {
	if err := validateSignalInput(in); err != nil {
		return 0, err
	}
	status := strings.TrimSpace(in.Status)
	if status == "" {
		status = SignalStatusOpen
	}
	var id int64
	err := s.db.QueryRow(`
INSERT INTO signals(task_id, instrument_id, node_id, signal_type, direction,
                    confidence, horizon, rationale, status, action)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING id`,
		in.TaskID, in.InstrumentID, in.NodeID,
		strings.TrimSpace(in.SignalType), strings.TrimSpace(in.Direction),
		optNormalizedDecimal(in.Confidence, decimalScaleConfidence), normalizeOptText(in.Horizon),
		normalizeOptText(in.Rationale), status,
		normalizeOptText(in.Action),
	).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

// GetSignal 按 id 取一条信号。
func (s *SignalStore) GetSignal(id int64) (*Signal, error) {
	row := s.db.QueryRow(`SELECT `+signalColumns+` FROM signals WHERE id = $1`, id)
	sig, err := scanSignal(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrSignalNotFound
		}
		return nil, err
	}
	return sig, nil
}

// ListSignalsByTask 列出某任务下的信号,最新在前。
// 结果为空返回空切片而非 nil。
func (s *SignalStore) ListSignalsByTask(taskID int64) ([]*Signal, error) {
	if taskID <= 0 {
		return nil, fmt.Errorf("%w: task_id 必须为正", ErrSignalInvalid)
	}
	rows, err := s.db.Query(`
SELECT `+signalColumns+` FROM signals WHERE task_id = $1 ORDER BY created_at DESC, id DESC`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]*Signal, 0, 16)
	for rows.Next() {
		sig, err := scanSignal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sig)
	}
	return out, rows.Err()
}

// UpdateSignalStatus 更新信号状态。status 为空或信号不存在都返回错误,
// 不做静默成功——调用方必须能发现「改了 0 行」。
func (s *SignalStore) UpdateSignalStatus(id int64, status string) error {
	status = strings.TrimSpace(status)
	if status == "" {
		return fmt.Errorf("%w: status 不能为空", ErrSignalInvalid)
	}
	res, err := s.db.Exec(`UPDATE signals SET status = $1 WHERE id = $2`, status, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrSignalNotFound
	}
	return nil
}
