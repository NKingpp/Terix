package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// =====================================================================
// 标的(instruments)数据层
// =====================================================================

// Market 与 AssetClass 的合法取值,与 schema.sql 的 CHECK 约束一一对应。
// 校验放在这一层而非只靠 CHECK:调用方能拿到可读的错误,而不是原始 SQL 错误。
const (
	MarketUS     = "us"
	MarketHK     = "hk"
	MarketCN     = "cn"
	MarketCrypto = "crypto"
)

// AssetClass 取值。
const (
	AssetClassStock  = "stock"
	AssetClassETF    = "etf"
	AssetClassIndex  = "index"
	AssetClassCrypto = "crypto"
	AssetClassFuture = "future"
	AssetClassOption = "option"
)

var (
	// ErrInstrumentExists 表示 (market, symbol) 已存在。
	ErrInstrumentExists = errors.New("instrument already exists")
	// ErrInstrumentNotFound 表示按 (market, symbol) 查不到标的。
	ErrInstrumentNotFound = errors.New("instrument not found")
	// ErrInstrumentInvalid 包装字段校验失败。
	ErrInstrumentInvalid = errors.New("invalid instrument")
)

var (
	validMarkets = map[string]bool{
		MarketUS: true, MarketHK: true, MarketCN: true, MarketCrypto: true,
	}
	validAssetClasses = map[string]bool{
		AssetClassStock: true, AssetClassETF: true, AssetClassIndex: true,
		AssetClassCrypto: true, AssetClassFuture: true, AssetClassOption: true,
	}
)

// Instrument is a row in the instruments table.
// 可空列用指针表示,JSON 输出时省略,避免前端把 "" 与 NULL 混为一谈。
type Instrument struct {
	ID         int64   `json:"id"`
	Market     string  `json:"market"`
	Symbol     string  `json:"symbol"`
	Name       *string `json:"name,omitempty"`
	AssetClass string  `json:"asset_class"`
	Currency   *string `json:"currency,omitempty"`
	Exchange   *string `json:"exchange,omitempty"`
	Sector     *string `json:"sector,omitempty"`
	Industry   *string `json:"industry,omitempty"`
	Active     bool    `json:"active"`
	CreatedAt  string  `json:"created_at"`
}

// InstrumentInput 是 InsertInstrument 的入参。
// 可选文本用 string 而非指针:空字符串统一归一化为 NULL,调用方不必区分。
type InstrumentInput struct {
	Market     string
	Symbol     string
	Name       string
	AssetClass string
	Currency   string
	Exchange   string
	Sector     string
	Industry   string
	Active     *bool // nil 走 schema 默认 true
}

// InstrumentFilter 是 ListInstruments 的过滤条件。
// 零值表示不过滤;Market 与 Active 同时给出时取交集。
type InstrumentFilter struct {
	Market string
	Active *bool
	Limit  int // <=0 时用 defaultInstrumentListLimit
}

const (
	defaultInstrumentListLimit = 200
	maxInstrumentListLimit     = 1000
)

// InstrumentStore operates on the instruments table.
type InstrumentStore struct{ db *DB }

// Instruments returns the instrument store associated with this DB.
func (d *DB) Instruments() *InstrumentStore { return &InstrumentStore{db: d} }

// normalizeOptText 把空白字符串折叠为 NULL。
func normalizeOptText(s string) any {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return s
}

// validateInstrumentInput 校验必填字段与枚举取值。
func validateInstrumentInput(in InstrumentInput) error {
	market := strings.TrimSpace(in.Market)
	if !validMarkets[market] {
		return fmt.Errorf("%w: market %q 不在 us/hk/cn/crypto 之内", ErrInstrumentInvalid, in.Market)
	}
	if strings.TrimSpace(in.Symbol) == "" {
		return fmt.Errorf("%w: symbol 不能为空", ErrInstrumentInvalid)
	}
	if !validAssetClasses[strings.TrimSpace(in.AssetClass)] {
		return fmt.Errorf("%w: asset_class %q 不在 stock/etf/index/crypto/future/option 之内",
			ErrInstrumentInvalid, in.AssetClass)
	}
	return nil
}

// insertInstrumentUniqueViolation 判断错误是否为 (market, symbol) 唯一键冲突。
//
// PostgreSQL 靠 ConstraintName 区分是哪个唯一键；SQLite 没有这个信息
// （实测 UNIQUE 报错里只有列名，没有约束名），所以改用 message 片段匹配。
// 实测 message 形如：
//
//	constraint failed: UNIQUE constraint failed: instruments.market, instruments.symbol (2067)
//
// 见 docs/quantex/ARCH-SQLite-Migration.md §4.2 / §8.3。
func insertInstrumentUniqueViolation(err error) bool {
	if !IsUniqueViolation(err) {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "instruments.market, instruments.symbol") ||
		strings.Contains(msg, "instruments.id")
}

// scanInstrument 从一行结果读出一条 Instrument。
// 可空文本列直接扫进 *string:database/sql 会把 NULL 写成 nil。
func scanInstrument(row interface{ Scan(...any) error }) (*Instrument, error) {
	var inst Instrument
	if err := row.Scan(
		&inst.ID, &inst.Market, &inst.Symbol, &inst.Name, &inst.AssetClass,
		&inst.Currency, &inst.Exchange, &inst.Sector, &inst.Industry,
		&inst.Active, &inst.CreatedAt,
	); err != nil {
		return nil, err
	}
	return &inst, nil
}

// instrumentColumns 与 scanInstrument 的扫描顺序必须一致。
const instrumentColumns = `id, market, symbol, name, asset_class, currency, exchange,
	sector, industry, active, created_at`

// InsertInstrument 插入一条标的并返回新 id。
// (market, symbol) 重复时返回 ErrInstrumentExists;字段非法时返回 ErrInstrumentInvalid。
func (s *InstrumentStore) InsertInstrument(in InstrumentInput) (int64, error) {
	if err := validateInstrumentInput(in); err != nil {
		return 0, err
	}
	var active any
	if in.Active != nil {
		active = *in.Active
	}
	var id int64
	err := s.db.QueryRow(`
INSERT INTO instruments(market, symbol, name, asset_class, currency, exchange, sector, industry, active)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, COALESCE($9, true))
RETURNING id`,
		strings.TrimSpace(in.Market),
		strings.TrimSpace(in.Symbol),
		normalizeOptText(in.Name),
		strings.TrimSpace(in.AssetClass),
		normalizeOptText(in.Currency),
		normalizeOptText(in.Exchange),
		normalizeOptText(in.Sector),
		normalizeOptText(in.Industry),
		active,
	).Scan(&id)
	if err != nil {
		if insertInstrumentUniqueViolation(err) {
			return 0, ErrInstrumentExists
		}
		return 0, err
	}
	return id, nil
}

// GetInstrumentByMarketSymbol 按 (market, symbol) 取一条标的。
// 查不到返回 ErrInstrumentNotFound。
func (s *InstrumentStore) GetInstrumentByMarketSymbol(market, symbol string) (*Instrument, error) {
	row := s.db.QueryRow(`
SELECT `+instrumentColumns+`
FROM instruments WHERE market = $1 AND symbol = $2`,
		strings.TrimSpace(market), strings.TrimSpace(symbol))
	inst, err := scanInstrument(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrInstrumentNotFound
		}
		return nil, err
	}
	return inst, nil
}

// ListInstruments 按可选条件列出标的,按 (market, symbol) 排序保证结果稳定。
// 结果为空时返回空切片而非 nil,调用方不必判空。
func (s *InstrumentStore) ListInstruments(filter InstrumentFilter) ([]*Instrument, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = defaultInstrumentListLimit
	}
	if limit > maxInstrumentListLimit {
		limit = maxInstrumentListLimit
	}

	// 参数化拼接 WHERE:每段额外拼一个 $N 占位符,不做字符串插值。
	where := make([]string, 0, 2)
	args := make([]any, 0, 3)
	if m := strings.TrimSpace(filter.Market); m != "" {
		if !validMarkets[m] {
			return nil, fmt.Errorf("%w: market %q 不在 us/hk/cn/crypto 之内", ErrInstrumentInvalid, filter.Market)
		}
		args = append(args, m)
		where = append(where, fmt.Sprintf("market = $%d", len(args)))
	}
	if filter.Active != nil {
		args = append(args, *filter.Active)
		where = append(where, fmt.Sprintf("active = $%d", len(args)))
	}
	args = append(args, limit)

	query := `SELECT ` + instrumentColumns + ` FROM instruments`
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, ` AND `)
	}
	query += fmt.Sprintf(` ORDER BY market, symbol LIMIT $%d`, len(args))

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]*Instrument, 0, 16)
	for rows.Next() {
		inst, err := scanInstrument(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, inst)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ErrInstrumentAssetClassConflict 表示已存在同 (market,symbol) 但 asset_class 不同。
//
// 为什么这是错误而不是可更新的字段：asset_class 决定 Expand 用哪个阶段图模板
// （stock 17 节点含 fundamental / crypto 13 节点无 fundamental）。已建任务的图
// 是按当时的 asset_class 展开的，改了会让「图」与「标的分类」不一致 ——
// 那种不一致没有任何代码能检测出来，只会表现为「分析师找一个不存在的维度」。
var ErrInstrumentAssetClassConflict = errors.New("instrument asset_class 冲突")

// UpsertInstrument 插入或更新标的（sync 语义）。
//
// 与 InsertInstrument 的区别：冲突时不报错，而是更新**可变字段**。
//
//   - 可更新：name / currency / exchange / sector / industry / active（描述性字段）
//   - **不可更新：asset_class** —— 冲突时返回 ErrInstrumentAssetClassConflict
//
// 返回 created 表示是新建还是命中已有记录，供 handler 分入 created/updated 两组。
//
// # 为什么改成「先查再写」（SQLite 迁移）
//
// PG 版靠 `RETURNING id, (xmax = 0) AS inserted` 判断是否新行 —— xmax 是 PG 的
// MVCC 系统列，SQLite 没有等价物，也没有「RETURNING 里告诉我这次是插还是改」
// 的能力（`last_insert_rowid()` 在 DO UPDATE 分支不可靠）。
//
// 所以改成事务里「先 SELECT 再 INSERT 或 UPDATE」：
//   - BEGIN IMMEDIATE 已经拿到全局写锁，两次语句之间不可能插进别的写者
//   - 单写者模型下这才是**正确**的读-判-写，不存在原注释担心的竞态
//
// 语义与 PG 版逐条对齐：
//   - 已存在且 asset_class 不同 → ErrInstrumentAssetClassConflict
//   - 已存在且相同 → 只更新非空的可变字段（COALESCE 语义保留）
//   - 不存在 → 插入
func (s *InstrumentStore) UpsertInstrument(in InstrumentInput) (id int64, created bool, err error) {
	if err := validateInstrumentInput(in); err != nil {
		return 0, false, err
	}
	market := strings.TrimSpace(in.Market)
	symbol := strings.TrimSpace(in.Symbol)
	assetClass := strings.TrimSpace(in.AssetClass)

	var active *bool
	if in.Active != nil {
		v := *in.Active
		active = &v
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback() //nolint:errcheck // 提交成功后是 no-op

	var (
		existingID    int64
		existingClass string
	)
	scanErr := tx.QueryRow(`SELECT id, asset_class FROM instruments
WHERE market=$1 AND symbol=$2`, market, symbol).Scan(&existingID, &existingClass)
	switch {
	case scanErr == nil:
		// 已存在。asset_class 不可变：不同就报冲突，让调用方决定怎么处理。
		if existingClass != assetClass {
			return 0, false, fmt.Errorf(
				"%w: %s/%s 已存在且 asset_class=%q，不能改为 %q（阶段图模板已按原分类展开）",
				ErrInstrumentAssetClassConflict, market, symbol, existingClass, assetClass)
		}
		// 只覆盖**本次给了值**的字段（COALESCE 语义）。
		if _, err := tx.Exec(`
UPDATE instruments SET
    name     = COALESCE($2, name),
    currency = COALESCE($3, currency),
    exchange = COALESCE($4, exchange),
    sector   = COALESCE($5, sector),
    industry = COALESCE($6, industry),
    active   = COALESCE($7, active)
WHERE id = $1`,
			existingID,
			normalizeOptText(in.Name),
			normalizeOptText(in.Currency),
			normalizeOptText(in.Exchange),
			normalizeOptText(in.Sector),
			normalizeOptText(in.Industry),
			active,
		); err != nil {
			return 0, false, err
		}
		if err := tx.Commit(); err != nil {
			return 0, false, err
		}
		return existingID, false, nil
	case errors.Is(scanErr, sql.ErrNoRows):
		// 不存在 → 插入。
		var insertedID int64
		if err := tx.QueryRow(`
INSERT INTO instruments(market, symbol, name, asset_class, currency, exchange, sector, industry, active)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, COALESCE($9, true))
RETURNING id`,
			market, symbol,
			normalizeOptText(in.Name),
			assetClass,
			normalizeOptText(in.Currency),
			normalizeOptText(in.Exchange),
			normalizeOptText(in.Sector),
			normalizeOptText(in.Industry),
			active,
		).Scan(&insertedID); err != nil {
			return 0, false, err
		}
		if err := tx.Commit(); err != nil {
			return 0, false, err
		}
		return insertedID, true, nil
	default:
		return 0, false, scanErr
	}
}

// GetInstrumentByID 按 id 取标的。查不到返回 ErrInstrumentNotFound。
func (s *InstrumentStore) GetInstrumentByID(id int64) (*Instrument, error) {
	if id <= 0 {
		return nil, fmt.Errorf("%w: id 必须为正", ErrInstrumentInvalid)
	}
	row := s.db.QueryRow(`SELECT `+instrumentColumns+` FROM instruments WHERE id = $1`, id)
	inst, err := scanInstrument(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrInstrumentNotFound
		}
		return nil, err
	}
	return inst, nil
}
