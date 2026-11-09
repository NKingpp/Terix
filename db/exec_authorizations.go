package db

// 授权仪式记录（exec_authorizations）数据层。
//
// # 这张表记录的是「仪式」而不是「配置」
//
// 它是用户在升级到实盘前，逐条确认风险告知后留下的**不可变凭据**：
// 确认了哪个版本的告知、说了什么短语、当时限额快照是什么、规则版本是多少。
//
// 为什么要把限额快照和规则版本一起存：
// 三个月后用户问「我当时授权时的限额是多少、规则是什么版本」，
// 如果只存「授权了」这个事实，这个问题就永远答不了。
// 授权是一个**带上下文的事实**，上下文必须跟着一起存。
//
// # 与账户的关系是 (channel, account_ref) 而不是 account_id
//
// 实际表里没有 account_id 外键 —— 因为授权发生在账户入库**之前**
// （先授权再建账户是合理的顺序）。用通道内的账户标识串（account_ref）
// 关联，避免「授权必须先有账户」这个不必要的前置条件。

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrAuthorizationRequired 表示缺少有效的实盘授权。
var ErrAuthorizationRequired = errors.New("需要一个未撤销的实盘授权")

// ErrAuthorizationNotFound 表示没有授权记录。
var ErrAuthorizationNotFound = errors.New("authorization not found")

// 授权目标模式的白名单见 exec_mode.go 的 ExecMode* 常量。
// 这里不重复定义 —— 同一组值两处定义必然漂移。

// validAuthModes 是授权目标模式白名单，与 CHECK 约束一致。
var validAuthModes = map[string]bool{
	string(ExecModeLiveWhitelist): true,
	string(ExecModeLiveManual):    true,
	// 2026-11-05：进入实盘自动需独立授权 —— 它不是 live_manual 的延伸。
	// 授权到 live_manual 的人未必接受「无人值守下单」。
	string(ExecModeLiveAuto): true,
}

// validAuthFromModes 是「从哪个模式授权」的白名单，与 CHECK 一致。
var validAuthFromModes = map[string]bool{
	string(ExecModePaper):         true,
	string(ExecModeLiveManual):    true,
	string(ExecModeLiveWhitelist): true,
	string(ExecModeLiveAuto):      true,
}

// ExecAuthorization 是 exec_authorizations 的一行。
//
// LimitsSnapshot 用 json.RawMessage 保留原样 JSON —— 它的结构会随
// execution_limits 演进，这里不该解析它（解析就等于锁死 schema）。
type ExecAuthorization struct {
	ID                int64           `json:"id"`
	Channel           string          `json:"channel"`
	AccountRef        string          `json:"account_ref"`
	Mode              string          `json:"mode"`
	FromMode          string          `json:"from_mode"`
	DisclaimerVersion string          `json:"disclaimer_version"`
	ConfirmPhrase     string          `json:"confirm_phrase"`
	LimitsSnapshot    json.RawMessage `json:"limits_snapshot"`
	RulesRevision     int64           `json:"rules_revision"`
	GrantedAt         string          `json:"granted_at"`
	GrantedBy         string          `json:"granted_by"`
	RevokedAt         *string         `json:"revoked_at,omitempty"`
	RevokedReason     string          `json:"revoked_reason"`
}

// ExecAuthorizationInput 是 Grant 的入参。
type ExecAuthorizationInput struct {
	Channel           string
	AccountRef        string
	Mode              string
	FromMode          string
	DisclaimerVersion string
	ConfirmPhrase     string
	LimitsSnapshot    json.RawMessage
	RulesRevision     int64
	GrantedBy         string
}

// ExecAuthorizationStore 操作 exec_authorizations 表。
type ExecAuthorizationStore struct{ db *DB }

// ExecAuthorizations 返回该 DB 关联的授权仓储。
func (d *DB) ExecAuthorizations() *ExecAuthorizationStore {
	return &ExecAuthorizationStore{db: d}
}

// authColumns 与 scanAuth 的扫描顺序必须一致。
const authColumns = `id, channel, account_ref, mode, from_mode,
	disclaimer_version, confirm_phrase, limits_snapshot, rules_revision,
	granted_at, granted_by, revoked_at, COALESCE(revoked_reason, '')`

// scanAuth 从一行结果读出一条 ExecAuthorization。
func scanAuth(row interface{ Scan(...any) error }) (*ExecAuthorization, error) {
	var (
		a         ExecAuthorization
		limitsRaw []byte
		revokedAt sql.NullString
	)
	if err := row.Scan(&a.ID, &a.Channel, &a.AccountRef, &a.Mode, &a.FromMode,
		&a.DisclaimerVersion, &a.ConfirmPhrase, &limitsRaw, &a.RulesRevision,
		&a.GrantedAt, &a.GrantedBy, &revokedAt, &a.RevokedReason); err != nil {
		return nil, err
	}
	if revokedAt.Valid {
		s := revokedAt.String
		a.RevokedAt = &s
	}
	a.LimitsSnapshot = json.RawMessage(limitsRaw)
	return &a, nil
}

// Grant 写入一条授权记录。
//
// # 为什么用「先撤销旧的再插入」而不是纯 INSERT
//
// schema 有部分唯一索引 `uq_auth_active ON (channel, account_ref) WHERE revoked_at IS NULL`：
// 同一 (channel, account_ref) 同时只能有一条未撤销的授权。
// 用户重复授权（比如先授 live_whitelist 再升 live_manual）时，
// 必须先撤销旧的，否则 INSERT 直接撞索引。
//
// 两步在同一事务里，避免「撤销成功但插入失败」留下无授权的空档。
func (s *ExecAuthorizationStore) Grant(in ExecAuthorizationInput) (int64, error) {
	channel := strings.TrimSpace(in.Channel)
	accountRef := strings.TrimSpace(in.AccountRef)
	mode := strings.TrimSpace(in.Mode)
	fromMode := strings.TrimSpace(in.FromMode)

	if !validExecChannels[channel] {
		return 0, fmt.Errorf("%w: 不支持的通道 %q", ErrOrderInvalid, channel)
	}
	if accountRef == "" {
		return 0, fmt.Errorf("%w: account_ref 不能为空", ErrOrderInvalid)
	}
	if !validAuthModes[mode] {
		return 0, fmt.Errorf("%w: 授权目标模式必须是 live_whitelist / live_manual / live_auto（不能授权 paper）", ErrOrderInvalid)
	}
	if !validAuthFromModes[fromMode] {
		return 0, fmt.Errorf("%w: 不支持的 from_mode %q", ErrOrderInvalid, fromMode)
	}
	if strings.TrimSpace(in.DisclaimerVersion) == "" {
		return 0, fmt.Errorf("%w: disclaimer_version 不能为空（授权必须绑定告知版本）", ErrOrderInvalid)
	}
	if strings.TrimSpace(in.ConfirmPhrase) == "" {
		return 0, fmt.Errorf("%w: confirm_phrase 不能为空（授权必须有用户确认的短语）", ErrOrderInvalid)
	}
	// rules_revision <= 0 会让「授权时规则是第几版」无法追溯 ——
	// 而这个字段存在的全部意义就是追溯。
	if in.RulesRevision <= 0 {
		return 0, fmt.Errorf("%w: rules_revision 必须为正数", ErrOrderInvalid)
	}
	snapshot := in.LimitsSnapshot
	if len(snapshot) == 0 {
		snapshot = json.RawMessage(`{}`)
	}

	grantedBy := strings.TrimSpace(in.GrantedBy)
	if grantedBy == "" {
		grantedBy = "local"
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	// 撤销同一账户的旧活跃授权（若有）。撤销原因写清楚，
	// 否则审计时看到「revoked_at 有值但 reason 空」无法判断是升档还是手动撤销。
	if _, err := tx.Exec(`UPDATE exec_authorizations
SET revoked_at = now(), revoked_reason = $3
WHERE channel = $1 AND account_ref = $2 AND revoked_at IS NULL`,
		channel, accountRef, "重新授权（升级为 "+mode+"）"); err != nil {
		return 0, fmt.Errorf("撤销旧授权失败: %w", err)
	}

	var id int64
	if err := tx.QueryRow(`
INSERT INTO exec_authorizations
    (channel, account_ref, mode, from_mode, disclaimer_version,
     confirm_phrase, limits_snapshot, rules_revision, granted_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING id`,
		channel, accountRef, mode, fromMode, strings.TrimSpace(in.DisclaimerVersion),
		strings.TrimSpace(in.ConfirmPhrase), []byte(snapshot), in.RulesRevision, grantedBy,
	).Scan(&id); err != nil {
		return 0, fmt.Errorf("写入授权失败: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

// ActiveFor 取某账户当前有效的授权（未撤销的那条）。
//
// 没有有效授权时返回 ErrAuthorizationRequired —— **不返回 nil, nil**。
// 让调用方必须显式处理「没授权」这个分支,而不是忘记判空就放行。
func (s *ExecAuthorizationStore) ActiveFor(channel, accountRef string) (*ExecAuthorization, error) {
	row := s.db.QueryRow(`SELECT `+authColumns+` FROM exec_authorizations
WHERE channel = $1 AND account_ref = $2 AND revoked_at IS NULL`,
		strings.TrimSpace(channel), strings.TrimSpace(accountRef))
	a, err := scanAuth(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrAuthorizationRequired
		}
		return nil, err
	}
	return a, nil
}

// ListActive 列出全部未撤销的授权。
//
// # 什么时候用它（而不是 ActiveFor）
//
// ActiveFor 是**精确**匹配 (channel, account_ref)，用于正常的授权查找。
// 但 account_ref 是自由文本（用户在授权对话框里手填），历史数据可能与
// 账户表的 channel_account_id 不一致 —— 那时精确匹配查不到，用户会看到
// 「需要先授权实盘」而明明已经授权过。
//
// ListActive 给调用方一个「按通道找」的回退能力。调用方必须自己保证
// 不会把一个通道的授权用到另一个通道上（见 anyActiveAuthForChannel）。
//
// 按 id 降序：最新的授权优先（用户重新授权后期望新的那条生效）。
func (s *ExecAuthorizationStore) ListActive() ([]*ExecAuthorization, error) {
	rows, err := s.db.Query(`SELECT ` + authColumns + ` FROM exec_authorizations
WHERE revoked_at IS NULL ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ExecAuthorization
	for rows.Next() {
		a, err := scanAuth(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetByID 按主键取授权（含已撤销的 —— 审计需要读历史）。
func (s *ExecAuthorizationStore) GetByID(id int64) (*ExecAuthorization, error) {
	row := s.db.QueryRow(`SELECT `+authColumns+` FROM exec_authorizations WHERE id=$1`, id)
	a, err := scanAuth(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrAuthorizationNotFound
		}
		return nil, err
	}
	return a, nil
}

// Revoke 撤销某账户的活跃授权。
//
// 没有活跃授权时返回 ErrAuthorizationNotFound（而不是静默成功）——
// 调用方需要知道「我撤销的东西本来就不存在」，否则审计里会出现
// 「用户点了撤销、界面说成功、但实际什么都没有」。
func (s *ExecAuthorizationStore) Revoke(channel, accountRef, reason string) error {
	res, err := s.db.Exec(`UPDATE exec_authorizations
SET revoked_at = now(), revoked_reason = $3
WHERE channel = $1 AND account_ref = $2 AND revoked_at IS NULL`,
		strings.TrimSpace(channel), strings.TrimSpace(accountRef), strings.TrimSpace(reason))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrAuthorizationNotFound
	}
	return nil
}

// ExpiryHint 返回授权距今已过去多久（用于界面提示「授权于 N 天前」）。
//
// # 为什么没有「过期」判定
//
// 实际表里**没有 expires_at 列** —— 授权不过期，只被撤销。
// 这与「授权是一次有意识的仪式」一致：撤销是显式动作，而不是时间到了自动失效
// （自动失效会让用户在不知情的情况下失去实盘能力，比不失效更危险）。
//
// 所以这里只提供「多久以前授权」这个信息，让用户自己判断是否需要重新确认。
func (s *ExecAuthorizationStore) AgeOf(a *ExecAuthorization) (time.Duration, error) {
	if a == nil {
		return 0, ErrAuthorizationNotFound
	}
	// 用统一的 ParseDBTime：granted_at 是 xxx_at 读出来的 PG 格式，
	// 不是 RFC3339（见 db/timestamps.go 的说明）。
	t, err := ParseDBTime(a.GrantedAt)
	if err != nil {
		return 0, fmt.Errorf("解析 granted_at 失败: %w", err)
	}
	return time.Since(t), nil
}
