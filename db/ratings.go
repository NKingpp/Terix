package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// =====================================================================
// 评级(ratings)数据层
// =====================================================================

// 评级取值,与 schema.sql 的 CHECK 约束一致。
const (
	RatingBuy         = "buy"
	RatingOverweight  = "overweight"
	RatingHold        = "hold"
	RatingUnderweight = "underweight"
	RatingSell        = "sell"
	RatingReview      = "review"
)

var (
	// ErrRatingInvalid 表示评级取值或入参非法。
	ErrRatingInvalid = errors.New("invalid rating")
	// ErrRatingNotFound 表示评级记录不存在。
	ErrRatingNotFound = errors.New("rating not found")
)

// validRatings 是评级白名单。
var validRatings = map[string]bool{
	RatingBuy: true, RatingOverweight: true, RatingHold: true,
	RatingUnderweight: true, RatingSell: true, RatingReview: true,
}

// Rating is a row in the ratings table.
// Confidence 与信号同理用 *string 承载 NUMERIC(5,4),避免精度丢失。
// EvidenceNodeIDs 由 SQL 侧 to_json 转文本再反序列化 —— pgx 的 database/sql
// 驱动没有 []int64 的 Scan 目标(实测报 unsupported Scan),这条路径最省事。
type Rating struct {
	ID           int64  `json:"id"`
	TaskID       *int64 `json:"task_id,omitempty"`
	InstrumentID *int64 `json:"instrument_id,omitempty"`
	// IntentID 是产出这条评级的 intent 节点 id（可为 nil）。
	//
	// 用途：PM intent 结束时，用 `WHERE intent_id=$1` 精确定位「这个 intent
	// 产出的评级」，从而决定是否生成订单草案（见 server/engine.go）。
	IntentID        *int64  `json:"intent_id,omitempty"`
	Rating          string  `json:"rating"`
	Confidence      *string `json:"confidence,omitempty"`
	DecidedAt       *string `json:"decided_at,omitempty"`
	EvidenceNodeIDs []int64 `json:"evidence_node_ids"`
	CreatedAt       string  `json:"created_at"`
}

// RatingInput 是 InsertRating 的入参。
type RatingInput struct {
	TaskID       *int64
	InstrumentID *int64
	// IntentID 是产出这条评级的 intent 节点 id。
	//
	// # 为什么需要它
	//
	// 「PM intent 结束时触发评级 → 草案」需要一个精确判据：
	// 「这个 intent 产出的评级是哪一条」。
	//
	// 时间窗口（created_at >= state_at）是猜测 —— 同一秒内两条 intent 结束时会取错。
	// 而把 rating_id 写进 intent 的 payload 会让 intent 承担「记住产出物」的职责。
	//
	// ⚠️ 可为 NULL：历史数据、以及（将来）人工补录的评级没有 intent 来源。
	IntentID        *int64
	Rating          string
	Confidence      string
	DecidedAt       string // 空表示 NULL;非空需是 PG 可解析的时间字面量
	EvidenceNodeIDs []int64
	// ConfidenceReason 是「为什么给这个置信度」。v1.5 新增。
	ConfidenceReason string
	// ReviewReasons 仅 rating='review' 时有值，必须含四个键：
	// data_gaps / conflicting_facts / skipped_analysts / next_step。
	// v1.5 新增；DB 侧有 ck_rating_review_reasons 约束兜底。
	ReviewReasons json.RawMessage
}

// RatingStore operates on the ratings table.
type RatingStore struct{ db *DB }

// Ratings returns the rating store associated with this DB.
func (d *DB) Ratings() *RatingStore { return &RatingStore{db: d} }

// ratingColumns 与 scanRating 的扫描顺序必须一致。
// evidence_node_ids 在 SQLite 里是 JSON 数组文本（原 bigint[]），
// 形如 "[1,2,3]"；NULL 或 PG 遗留的 '{}' 折成空数组。
const ratingColumns = `id, task_id, instrument_id, intent_id, rating, confidence,
	decided_at,
	CASE WHEN COALESCE(evidence_node_ids,'') IN ('','{}') THEN NULL ELSE evidence_node_ids END,
	created_at`

// scanRating 从一行结果读出一条 Rating。
func scanRating(row interface{ Scan(...any) error }) (*Rating, error) {
	var (
		r         Rating
		taskID    sql.NullInt64
		instID    sql.NullInt64
		intentID  sql.NullInt64
		conf      sql.NullString
		decidedAt sql.NullString
		evidence  sql.NullString
	)
	// 扫描顺序必须与 ratingColumns 一致（intent_id 在 instrument_id 之后）。
	if err := row.Scan(
		&r.ID, &taskID, &instID, &intentID, &r.Rating, &conf,
		&decidedAt, &evidence, &r.CreatedAt,
	); err != nil {
		return nil, err
	}
	r.TaskID = nullInt64Ptr(taskID)
	r.InstrumentID = nullInt64Ptr(instID)
	r.IntentID = nullInt64Ptr(intentID)
	if conf.Valid {
		v := conf.String
		r.Confidence = &v
	}
	if decidedAt.Valid {
		v := decidedAt.String
		r.DecidedAt = &v
	}
	ids, err := decodeBigintArray(evidence)
	if err != nil {
		return nil, err
	}
	r.EvidenceNodeIDs = ids
	return &r, nil
}

// decodeBigintArray 把 JSON 数组文本还原成 []int64。
// NULL 与空数组都归一化成空切片,调用方不必判空。
func decodeBigintArray(raw sql.NullString) ([]int64, error) {
	if !raw.Valid || strings.TrimSpace(raw.String) == "" {
		return []int64{}, nil
	}
	var ids []int64
	if err := json.Unmarshal([]byte(raw.String), &ids); err != nil {
		return nil, fmt.Errorf("decode evidence_node_ids: %w", err)
	}
	if ids == nil {
		return []int64{}, nil
	}
	return ids, nil
}

// InsertRating 写入一条评级并返回新 id。
func (s *RatingStore) InsertRating(in RatingInput) (int64, error) {
	rating := strings.TrimSpace(in.Rating)
	if !validRatings[rating] {
		return 0, fmt.Errorf("%w: rating %q 不在 buy/overweight/hold/underweight/sell/review 之内",
			ErrRatingInvalid, in.Rating)
	}
	if c := strings.TrimSpace(in.Confidence); c != "" {
		for _, r := range c {
			if (r < '0' || r > '9') && r != '.' && r != '-' {
				return 0, fmt.Errorf("%w: confidence %q 不是合法十进制数", ErrRatingInvalid, c)
			}
		}
	}
	// 空切片按 NULL 存：与「未记录证据」语义一致。
	var evidence any
	if len(in.EvidenceNodeIDs) > 0 {
		raw, mErr := json.Marshal(in.EvidenceNodeIDs)
		if mErr != nil {
			return 0, fmt.Errorf("序列化 evidence_node_ids 失败: %w", mErr)
		}
		evidence = string(raw)
	}
	var reviewReasons any
	if len(in.ReviewReasons) > 0 {
		reviewReasons = string(in.ReviewReasons)
	}
	var id int64
	err := s.db.QueryRow(`
INSERT INTO ratings(task_id, instrument_id, intent_id, rating, confidence, decided_at,
                    evidence_node_ids, confidence_reason, review_reasons)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING id`,
		in.TaskID, in.InstrumentID, in.IntentID, rating,
		optNormalizedDecimal(in.Confidence, decimalScaleConfidence), normalizeOptText(in.DecidedAt), evidence,
		normalizeOptText(in.ConfidenceReason), reviewReasons,
	).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

// GetRating 按 id 取一条评级,查不到返回 ErrRatingNotFound。
func (s *RatingStore) GetRating(id int64) (*Rating, error) {
	row := s.db.QueryRow(`SELECT `+ratingColumns+` FROM ratings WHERE id = $1`, id)
	r, err := scanRating(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrRatingNotFound
		}
		return nil, err
	}
	return r, nil
}

// ListRatingsByInstrument 列出某标的的评级历史,最新在前。
// 结果为空返回空切片而非 nil。
func (s *RatingStore) ListRatingsByInstrument(instrumentID int64) ([]*Rating, error) {
	if instrumentID <= 0 {
		return nil, fmt.Errorf("%w: instrument_id 必须为正", ErrRatingInvalid)
	}
	rows, err := s.db.Query(`
SELECT `+ratingColumns+` FROM ratings WHERE instrument_id = $1
ORDER BY created_at DESC, id DESC`, instrumentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]*Rating, 0, 16)
	for rows.Next() {
		r, err := scanRating(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetLatestRatingByIntent 取某个 intent 产出的最新一条评级。
//
// # 为什么可能返回多条、只取一条
//
// 一个 intent 理论上只产出一次评级（emit_rating 是 PM 的最后动作），
// 但如果模型多次调用 emit_rating，库里会有多条。
//
// 取最新一条（`ORDER BY id DESC`）而不是报错 —— 因为报错会让
// 「模型多写了一行」变成「整条链中断」，而取最新是更宽容且可解释的选择。
//
// 返回 nil, nil 表示该 intent 没有产出评级（不是错误 —— PM 可能没走到 emit_rating）。
func (s *RatingStore) GetLatestRatingByIntent(intentID int64) (*Rating, error) {
	if intentID <= 0 {
		return nil, nil
	}
	row := s.db.QueryRow(`SELECT `+ratingColumns+` FROM ratings
 WHERE intent_id = $1 ORDER BY id DESC LIMIT 1`, intentID)
	r, err := scanRating(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil // 没有评级 —— 正常情况，不是错误
		}
		return nil, err
	}
	return r, nil
}
