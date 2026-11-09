package db

// 执行账户（exec_accounts）数据层。
//
// # 这张表的实际语义与常见想象力不同
//
// 它**没有** provider / mode 两列：
//   - 通道叫 `channel`，且只有 'fosunwealth' / 'okx' 两个合法值（CHECK 约束）
//   - **模式不在这里** —— 模式是订单级（orders_execution.mode）的，
//     账户级只有「是否被拉黑」（blocked_at / block_reason）
//
// 为什么模式不挂在账户上：
// 同一个账户可以同时跑纸面与实盘（paper / live_whitelist / live_manual
// 是订单属性）。把模式挂到账户上会让「同一账户先跑纸面验证策略、
// 确认后切实盘」变成两次账户配置，而它本质上是同一账户的不同运行方式。
// 账户级只需要一个「能不能用」的开关 —— 那就是 blocked_at。

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrExecAccountNotFound 表示账户不存在。
var ErrExecAccountNotFound = errors.New("exec account not found")

// ErrExecAccountBlocked 表示账户已被拉黑，不允许下单。
var ErrExecAccountBlocked = errors.New("exec account is blocked")

// validExecChannels 是通道白名单，与 schema.sql 的 CHECK 约束一致。
//
// 为什么在这里也列一遍（DB 已有 CHECK）：DB 约束报错是 500 级错误文本
// （"violates check constraint"），而这里能给出「不支持的通道: xxx」。
// 两层都留不是冗余 —— DB 那层防脏数据，这层给人看。
var validExecChannels = map[string]bool{
	"fosunwealth": true,
	"okx":         true,
}

// validExecMarkets 是市场白名单，与 CHECK 约束一致。
var validExecMarkets = map[string]bool{
	"us": true, "hk": true, "cn": true, "crypto": true,
}

// ExecAccount 是 exec_accounts 的一行。
//
// MarketSupport 是 text[]，用 []string 承载。
// BlockedAt 为 nil 表示账户可用；非 nil 表示被拉黑（附 block_reason）。
type ExecAccount struct {
	ID            int64    `json:"id"`
	Channel       string   `json:"channel"`
	ChannelAcctID string   `json:"channel_account_id"`
	Label         string   `json:"label"`
	Market        string   `json:"market"`
	MarketSupport []string `json:"market_support"`
	Active        bool     `json:"active"`
	BlockedAt     *string  `json:"blocked_at,omitempty"`
	BlockReason   string   `json:"block_reason"`
	CreatedAt     string   `json:"created_at"`
}

// ExecAccountInput 是 UpsertAccount 的入参。
type ExecAccountInput struct {
	Channel       string
	ChannelAcctID string
	Label         string
	Market        string
	MarketSupport []string
	Active        bool
}

// ExecAccountStore 操作 exec_accounts 表。
type ExecAccountStore struct{ db *DB }

// ExecAccounts 返回该 DB 关联的账户仓储。
func (d *DB) ExecAccounts() *ExecAccountStore { return &ExecAccountStore{db: d} }

// execAccountColumns 与 scanExecAccount 的扫描顺序必须一致。
//
// SQLite 侧：market_support 已经是 JSON 文本（schema 里 TEXT NOT NULL
// DEFAULT '{}'），不需要 to_json(...)::text；时间列也不需要 ::text。
// 历史行可能是 NULL，所以仍用 COALESCE 兜底。
//
// 注意 market_support 的**默认值**是 PG 语法遗留的 '{}'（空 JSON 对象）而不是
// '[]'：decodeStringArray 对 '{}' 的 json.Unmarshal 失败。这里在 SELECT 侧把
// 非数组值统一折成 '[]'，让扫描不依赖列默认值。
const execAccountColumns = `id, channel, channel_account_id, label, market,
	CASE WHEN COALESCE(market_support,'') IN ('','{}') THEN '[]' ELSE market_support END,
	active, blocked_at, COALESCE(block_reason, ''), created_at`

// scanExecAccount 从一行结果读出一条 ExecAccount。
func scanExecAccount(row interface{ Scan(...any) error }) (*ExecAccount, error) {
	var (
		a         ExecAccount
		blockedAt sql.NullString
		msRaw     sql.NullString
	)
	if err := row.Scan(&a.ID, &a.Channel, &a.ChannelAcctID, &a.Label, &a.Market,
		&msRaw, &a.Active, &blockedAt, &a.BlockReason, &a.CreatedAt); err != nil {
		return nil, err
	}
	if blockedAt.Valid {
		s := blockedAt.String
		a.BlockedAt = &s
	}
	ms, err := decodeStringArray(msRaw)
	if err != nil {
		return nil, fmt.Errorf("解析 market_support 失败: %w", err)
	}
	a.MarketSupport = ms
	return &a, nil
}

// GetAccount 按 id 取一个账户。
func (s *ExecAccountStore) GetAccount(id int64) (*ExecAccount, error) {
	row := s.db.QueryRow(`SELECT `+execAccountColumns+` FROM exec_accounts WHERE id=$1`, id)
	a, err := scanExecAccount(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrExecAccountNotFound
		}
		return nil, err
	}
	return a, nil
}

// ListAccounts 列出全部账户（含被拉黑的 —— 被拉黑的账户也需要在界面上可见，
// 否则用户不知道它为什么不出单）。
//
// 排序用 id 而不是 created_at：id 单调递增且不受时区/时钟回拨影响。
func (s *ExecAccountStore) ListAccounts() ([]*ExecAccount, error) {
	rows, err := s.db.Query(`SELECT ` + execAccountColumns + ` FROM exec_accounts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*ExecAccount, 0, 8)
	for rows.Next() {
		a, err := scanExecAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// UpsertAccount 按 (channel, channel_account_id) 新增或更新。
//
// 唯一键是这两列的组合（schema 的 UNIQUE 约束），label 只是展示名，
// 不参与身份判定 —— 用户改 label 不该变成两条记录。
func (s *ExecAccountStore) UpsertAccount(in ExecAccountInput) (*ExecAccount, error) {
	channel := strings.TrimSpace(in.Channel)
	if !validExecChannels[channel] {
		return nil, fmt.Errorf("%w: 不支持的通道 %q（支持: fosunwealth / okx）", ErrOrderInvalid, channel)
	}
	acctID := strings.TrimSpace(in.ChannelAcctID)
	if acctID == "" {
		return nil, fmt.Errorf("%w: channel_account_id 不能为空", ErrOrderInvalid)
	}
	market := strings.TrimSpace(in.Market)
	if !validExecMarkets[market] {
		return nil, fmt.Errorf("%w: 不支持的市场 %q（支持: us / hk / cn / crypto）", ErrOrderInvalid, market)
	}
	for _, m := range in.MarketSupport {
		if !validExecMarkets[strings.TrimSpace(m)] {
			return nil, fmt.Errorf("%w: market_support 含不支持的市场 %q", ErrOrderInvalid, m)
		}
	}
	// market_support 必须是**非空**数组。
	//
	// # 为什么校验在应用层（SQLite 迁移后 CHECK 退化）
	//
	// PG 版是 `market_support <@ ARRAY['us','hk','cn','crypto']`（「是这四个
	// 的子集」）。SQLite 的 CHECK 禁止子查询，schema 里只能降级成
	// `LIKE '%"us"%' OR ...`（见 ARCH-SQLite-Migration.md §2.8 / 任务书坑⑥）。
	//
	// 实测这个降级**两边都错**：
	//   - `[]`（合法：账户暂时不支持任何市场）被拒 —— 空数组不包含任何子串
	//   - `["us","bogus"]` 被放行 —— 只要含一个已知市场就过
	//
	// 上面的循环已经拦住「含未知市场」。这里补上「不能为空」，
	// 让 DB 的降级 CHECK 只作为最后一道防脏数据的网，语义以本函数为准。
	ms := normalizeMarketSupport(in.MarketSupport)
	if len(ms) == 0 {
		return nil, fmt.Errorf("%w: market_support 不能为空（至少声明一个市场）", ErrOrderInvalid)
	}
	supportJSON, err := json.Marshal(ms)
	if err != nil {
		return nil, fmt.Errorf("序列化 market_support 失败: %w", err)
	}

	var id int64
	err = s.db.QueryRow(`
INSERT INTO exec_accounts (channel, channel_account_id, label, market, market_support, active)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (channel, channel_account_id) DO UPDATE SET
    label          = EXCLUDED.label,
    market         = EXCLUDED.market,
    market_support = EXCLUDED.market_support,
    active         = EXCLUDED.active
RETURNING id`,
		channel, acctID, strings.TrimSpace(in.Label), market, string(supportJSON), in.Active,
	).Scan(&id)
	if err != nil {
		return nil, fmt.Errorf("保存执行账户失败: %w", err)
	}
	return s.GetAccount(id)
}

// Block 拉黑账户并记录原因。
//
// # 为什么拉黑是独立方法而不是 Upsert 的字段
//
// 「拉黑」是一次带原因的、有语义的操作（风控触发），不是一个配置字段。
// 让它走独立的 SQL 保证 blocked_at 只会被写成 now()，不会被某次
// Upsert 顺手带成别的值。
func (s *ExecAccountStore) Block(id int64, reason string) error {
	res, err := s.db.Exec(`UPDATE exec_accounts
SET blocked_at = now(), block_reason = $2 WHERE id = $1`, id, strings.TrimSpace(reason))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrExecAccountNotFound
	}
	return nil
}

// Unblock 解除拉黑（清除 blocked_at 与原因）。
func (s *ExecAccountStore) Unblock(id int64) error {
	res, err := s.db.Exec(`UPDATE exec_accounts
SET blocked_at = NULL, block_reason = '' WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrExecAccountNotFound
	}
	return nil
}

// RequireUsable 校验账户可用于下单。
//
// 返回 ErrExecAccountBlocked 时是**业务拒绝**（不是系统错误），
// 调用方应把订单标为 risk_denied 而不是重试。
func (s *ExecAccountStore) RequireUsable(id int64) (*ExecAccount, error) {
	a, err := s.GetAccount(id)
	if err != nil {
		return nil, err
	}
	if a.BlockedAt != nil {
		return a, fmt.Errorf("%w: %s", ErrExecAccountBlocked, a.BlockReason)
	}
	if !a.Active {
		return a, fmt.Errorf("%w: 账户已停用（active=false）", ErrExecAccountBlocked)
	}
	return a, nil
}

// normalizeMarketSupport 去空白、去重、保持稳定顺序。
//
// 空数组是合法输入（账户暂时不支持任何市场），由调用方决定是否接受 ——
// 见 UpsertAccount 里「不能为空」那条校验的注释。
func normalizeMarketSupport(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}
