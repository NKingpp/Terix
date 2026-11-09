package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// =====================================================================
// 自选清单(watchlists + watchlist_items)数据层
// =====================================================================

var (
	// ErrWatchlistNameConflict 表示清单名已存在(name 全局唯一)。
	ErrWatchlistNameConflict = errors.New("watchlist name already exists")
	// ErrWatchlistNotFound 表示清单不存在。
	ErrWatchlistNotFound = errors.New("watchlist not found")
	// ErrWatchlistItemExists 表示该标的已在此清单中。
	ErrWatchlistItemExists = errors.New("watchlist item already exists")
	// ErrWatchlistItemNotFound 表示清单里没有这个标的。
	ErrWatchlistItemNotFound = errors.New("watchlist item not found")
)

// Watchlist is a row in the watchlists table.
type Watchlist struct {
	ID        int64   `json:"id"`
	Name      string  `json:"name"`
	Note      *string `json:"note,omitempty"`
	CreatedAt string  `json:"created_at"`
}

// WatchlistItem is a watchlist_items row joined with its instrument.
// symbol/name 直接带出来,前端渲染清单不必二次查 instruments。
type WatchlistItem struct {
	ID           int64   `json:"id"`
	WatchlistID  int64   `json:"watchlist_id"`
	InstrumentID int64   `json:"instrument_id"`
	Note         *string `json:"note,omitempty"`
	Market       string  `json:"market"`
	Symbol       string  `json:"symbol"`
	Name         *string `json:"name,omitempty"`
	AssetClass   string  `json:"asset_class"`
	CreatedAt    string  `json:"created_at"`
}

// WatchlistStore operates on watchlists + watchlist_items.
type WatchlistStore struct{ db *DB }

// Watchlists returns the watchlist store associated with this DB.
func (d *DB) Watchlists() *WatchlistStore { return &WatchlistStore{db: d} }

// watchlistUniqueViolation 判断错误是否撞了指定唯一键。
//
// PostgreSQL 用 ConstraintName 区分具体是哪个唯一键；SQLite 没有约束名
// （实测 UNIQUE 报错只带列名），所以 fragment 改成匹配 message 里的列名。
// 传空 fragment 表示"任意唯一冲突"。
// watchlistUniqueViolation 判断唯一冲突是否来自某个指定约束。
//
// # 为什么按列名片段匹配（SQLite 迁移）
//
// PG 的 PgError 带 ConstraintName（如 watchlist_items_watchlist_id_instrument_id_key），
// 而 SQLite 的 UNIQUE 报错**只有列名，没有约束名**（实测）：
//
//	constraint failed: UNIQUE constraint failed: watchlist_items.watchlist_id, watchlist_items.instrument_id (2067)
//
// 所以片段要写成列名形态（"watchlist_items.watchlist_id"）而不是约束名。
// 调用方原本传的约束名片段已随之调整 —— 见 AddWatchlistItem。
func watchlistUniqueViolation(err error, columnFragment string) bool {
	if !IsUniqueViolation(err) {
		return false
	}
	return columnFragment == "" || strings.Contains(err.Error(), columnFragment)
}

// CreateWatchlist 新建清单。重名返回 ErrWatchlistNameConflict。
func (s *WatchlistStore) CreateWatchlist(name, note string) (int64, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, fmt.Errorf("%w: name 不能为空", ErrWatchlistNameConflict)
	}
	var id int64
	err := s.db.QueryRow(`
INSERT INTO watchlists(name, note) VALUES ($1, $2) RETURNING id`,
		name, normalizeOptText(note),
	).Scan(&id)
	if err != nil {
		if watchlistUniqueViolation(err, "watchlists.name") {
			return 0, ErrWatchlistNameConflict
		}
		return 0, err
	}
	return id, nil
}

// GetWatchlist 按名字取清单,查不到返回 ErrWatchlistNotFound。
func (s *WatchlistStore) GetWatchlist(name string) (*Watchlist, error) {
	var w Watchlist
	err := s.db.QueryRow(`
SELECT id, name, note, created_at FROM watchlists WHERE name = $1`,
		strings.TrimSpace(name)).Scan(&w.ID, &w.Name, &w.Note, &w.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrWatchlistNotFound
		}
		return nil, err
	}
	return &w, nil
}

// AddWatchlistItem 把标的加入清单。重复加入返回 ErrWatchlistItemExists。
func (s *WatchlistStore) AddWatchlistItem(watchlistID, instrumentID int64, note string) (int64, error) {
	if watchlistID <= 0 || instrumentID <= 0 {
		return 0, fmt.Errorf("%w: watchlist_id / instrument_id 必须为正", ErrWatchlistItemNotFound)
	}
	var id int64
	err := s.db.QueryRow(`
INSERT INTO watchlist_items(watchlist_id, instrument_id, note) VALUES ($1, $2, $3) RETURNING id`,
		watchlistID, instrumentID, normalizeOptText(note),
	).Scan(&id)
	if err != nil {
		if watchlistUniqueViolation(err, "watchlist_items.watchlist_id") {
			return 0, ErrWatchlistItemExists
		}
		return 0, err
	}
	return id, nil
}

// ListWatchlistItems 列出清单内标的,按 market/symbol 排序保证稳定。
// 结果为空返回空切片而非 nil。
func (s *WatchlistStore) ListWatchlistItems(watchlistID int64) ([]*WatchlistItem, error) {
	if watchlistID <= 0 {
		return nil, fmt.Errorf("%w: watchlist_id 必须为正", ErrWatchlistNotFound)
	}
	rows, err := s.db.Query(`
SELECT wi.id, wi.watchlist_id, wi.instrument_id, wi.note,
       i.market, i.symbol, i.name, i.asset_class, wi.created_at
FROM watchlist_items wi
JOIN instruments i ON i.id = wi.instrument_id
WHERE wi.watchlist_id = $1
ORDER BY i.market, i.symbol`, watchlistID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]*WatchlistItem, 0, 16)
	for rows.Next() {
		var it WatchlistItem
		if err := rows.Scan(
			&it.ID, &it.WatchlistID, &it.InstrumentID, &it.Note,
			&it.Market, &it.Symbol, &it.Name, &it.AssetClass, &it.CreatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, &it)
	}
	return out, rows.Err()
}

// RemoveWatchlistItem 从清单移除标的。本来就不在时返回 ErrWatchlistItemNotFound,
// 让调用方能区分「移除成功」与「本来没有」。
func (s *WatchlistStore) RemoveWatchlistItem(watchlistID, instrumentID int64) error {
	res, err := s.db.Exec(`
DELETE FROM watchlist_items WHERE watchlist_id = $1 AND instrument_id = $2`,
		watchlistID, instrumentID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrWatchlistItemNotFound
	}
	return nil
}
