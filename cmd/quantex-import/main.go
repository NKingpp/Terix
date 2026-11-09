// Command quantex-import 一次性导入 backtest_data/ 下的雅虎原始响应到 bars 表。
//
// 用法:
//
//	ARTEX_PG_DSN=... go run ./cmd/quantex-import -dir C:/.../backtest_data
//
// 设计要点（详见《ARCH-QuantEx》§10.1，docs 仓库：quantex/ARCH-QuantEx.md）:
//   - 选型 c:一个文件 = 一条 snapshots 行(coverage_start..coverage_end),
//     明细落 bars,通过 snapshot_id 关联。不按日切快照(会与去重唯一索引冲突)。
//   - collected_at 取源数据里的行情时间戳,不是导入时间。否则「何时可知」
//     会被误标成导入时刻,point-in-time 语义就废了。
//   - 幂等:按校验和判重。同一文件重跑不增行。
//   - 全 NULL bar 入表并标 is_partial,OHLC 保持 NULL。禁止补 0。
package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
)

// yahooResp 是雅虎 chart API 的响应子集。
type yahooResp struct {
	Chart struct {
		Result []struct {
			Meta struct {
				Symbol               string `json:"symbol"`
				Currency             string `json:"currency"`
				ExchangeName         string `json:"exchangeName"`
				InstrumentType       string `json:"instrumentType"`
				RegularMarketTime    int64  `json:"regularMarketTime"`
				ExchangeTimezoneName string `json:"exchangeTimezoneName"`
			} `json:"meta"`
			Timestamp  []int64 `json:"timestamp"`
			Indicators struct {
				Quote []struct {
					Open   []*float64 `json:"open"`
					High   []*float64 `json:"high"`
					Low    []*float64 `json:"low"`
					Close  []*float64 `json:"close"`
					Volume []*float64 `json:"volume"`
				} `json:"quote"`
				AdjClose []struct {
					AdjClose []*float64 `json:"adjclose"`
				} `json:"adjclose"`
			} `json:"indicators"`
		} `json:"result"`
		Error *struct {
			Code        string `json:"code"`
			Description string `json:"description"`
		} `json:"error"`
	} `json:"chart"`
}

func main() {
	dir := flag.String("dir", "", "backtest_data 目录路径")
	flag.Parse()
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "必须指定 -dir")
		os.Exit(2)
	}
	// 走 db.Open：它会拼 SQLite DSN（WAL / foreign_keys / busy_timeout /
	// _txlock=immediate）并应用 schema + seed。裸 sql.Open 会漏掉这些
	// 连接级 pragma（foreign_keys 尤其关键：不设则全部外键静默失效）。
	d, err := db.Open(mustDBPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "连接数据库失败: %v\n", err)
		os.Exit(1)
	}
	defer d.Close()
	sqlDB := d.DB

	entries, err := os.ReadDir(*dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取目录失败: %v\n", err)
		os.Exit(1)
	}

	var imported, skipped int
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		path := filepath.Join(*dir, e.Name())
		n, err := importFile(sqlDB, path, e.Name())
		if err != nil {
			fmt.Printf("跳过：%s —— %v\n", e.Name(), err)
			skipped++
			continue
		}
		fmt.Printf("导入：%s —— %d 根 bar\n", e.Name(), n)
		imported++
	}
	fmt.Printf("\n完成：导入 %d 个文件，跳过 %d 个\n", imported, skipped)
}

// importFile 导入单个文件,返回写入的 bar 行数。
func importFile(sqlDB *sql.DB, path, name string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("读取失败: %w", err)
	}
	// 内容校验:3 个 CSV 实测是反爬 JS challenge 页,以 <!DOCTYPE 开头。
	// 这类内容不是行情数据,不能尝试解析。
	if isChallengePage(raw) {
		return 0, errors.New("跳过：内容非行情数据（反爬 JS challenge 页）")
	}
	var resp yahooResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return 0, fmt.Errorf("不是合法 JSON: %w", err)
	}
	if resp.Chart.Error != nil {
		return 0, fmt.Errorf("雅虎返回错误: %s %s", resp.Chart.Error.Code, resp.Chart.Error.Description)
	}
	if len(resp.Chart.Result) == 0 {
		return 0, errors.New("chart.result 为空")
	}
	r := resp.Chart.Result[0]
	if len(r.Timestamp) == 0 {
		return 0, errors.New("timestamp 为空")
	}
	if len(r.Indicators.Quote) == 0 {
		return 0, errors.New("indicators.quote 为空")
	}
	symbol := strings.TrimSpace(r.Meta.Symbol)
	if symbol == "" {
		return 0, errors.New("meta.symbol 为空")
	}

	// 时区:用交易所时区把 UTC 时间戳还原成当地交易日。
	loc, err := time.LoadLocation(r.Meta.ExchangeTimezoneName)
	if err != nil {
		return 0, fmt.Errorf("时区 %q 无法解析: %w", r.Meta.ExchangeTimezoneName, err)
	}

	// 标的必须已存在(由 seedBenchmarkInstruments 建好)。
	var instrumentID int64
	if err := sqlDB.QueryRow(
		`SELECT id FROM instruments WHERE market='us' AND symbol=$1`, symbol,
	).Scan(&instrumentID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("instruments 里没有 %q,请先运行服务完成 seed", symbol)
		}
		return 0, fmt.Errorf("查标的失败: %w", err)
	}

	// collected_at:用源数据里的行情时间戳,不是导入时间。
	// 这是 point-in-time 的根 —— 标错就等于「今天导入的历史数据在今天才可知」。
	collectedAt := time.Unix(r.Meta.RegularMarketTime, 0).UTC()
	sum := sha256.Sum256(raw)
	checksum := hex.EncodeToString(sum[:])

	q := r.Indicators.Quote[0]
	adj := []*float64(nil)
	if len(r.Indicators.AdjClose) > 0 {
		adj = r.Indicators.AdjClose[0].AdjClose
	}

	// 先算覆盖区间与总行数,写进 snapshots。
	firstDay := time.Unix(r.Timestamp[0], 0).In(loc).Format("2006-01-02")
	lastDay := time.Unix(r.Timestamp[len(r.Timestamp)-1], 0).In(loc).Format("2006-01-02")

	tx, err := sqlDB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck

	// 幂等:同一 (source, endpoint, params_hash, collected_at 秒) 已存在则复用。
	// params_hash 用文件名校验和 —— 原始请求参数已不可考,用文件名做稳定标识。
	const source = "yahoo_chart"
	const endpoint = "/v8/finance/chart"
	paramsHash := "file:" + name

	var snapID int64
	err = tx.QueryRow(`
INSERT INTO snapshots(source, endpoint, params_hash, collected_at, body_ref, checksum,
                      coverage_start, coverage_end, bar_count)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (source, endpoint, params_hash, substr(collected_at,1,19))
DO UPDATE SET coverage_start = EXCLUDED.coverage_start,
              coverage_end   = EXCLUDED.coverage_end,
              bar_count      = EXCLUDED.bar_count,
              checksum       = EXCLUDED.checksum
RETURNING id`,
		source, endpoint, paramsHash, collectedAt, filepath.Base(path), checksum,
		firstDay, lastDay, len(r.Timestamp),
	).Scan(&snapID)
	if err != nil {
		return 0, fmt.Errorf("写快照失败: %w", err)
	}

	written := 0
	for i, tsv := range r.Timestamp {
		ts := time.Unix(tsv, 0).UTC()
		tradingDate := ts.In(loc).Format("2006-01-02")

		o := pick(q.Open, i)
		h := pick(q.High, i)
		l := pick(q.Low, i)
		c := pick(q.Close, i)
		v := pick(q.Volume, i)
		a := pick(adj, i)

		// 全 NULL bar:入表 + is_partial,OHLC 保持 NULL。绝不补 0。
		partial := o == nil && h == nil && l == nil && c == nil

		var adjVal any
		if a != nil {
			adjVal = fmtNum(*a)
		}
		var oVal, hVal, lVal, cVal, vVal any
		if o != nil {
			oVal = fmtNum(*o)
		}
		if h != nil {
			hVal = fmtNum(*h)
		}
		if l != nil {
			lVal = fmtNum(*l)
		}
		if c != nil {
			cVal = fmtNum(*c)
		}
		if v != nil {
			vVal = fmtNum(*v)
		}

		res, err := tx.Exec(`
INSERT INTO bars(instrument_id, interval, ts, trading_date, open, high, low, close,
                 adj_close, volume, snapshot_id, is_partial, source)
VALUES ($1, '1d', $2, $3, $4, $5, $6, $7,
        $8, $9, $10, $11, $12)
ON CONFLICT (instrument_id, "interval", ts) DO NOTHING`,
			instrumentID, ts, tradingDate, oVal, hVal, lVal, cVal, adjVal, vVal,
			snapID, partial, source)
		if err != nil {
			return 0, fmt.Errorf("第 %d 根 bar 写入失败: %w", i, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			written++
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return written, nil
}

// isChallengePage 判断内容是否是反爬 JS challenge 页。
func isChallengePage(raw []byte) bool {
	head := strings.TrimSpace(string(raw))
	if len(head) > 512 {
		head = head[:512]
	}
	lower := strings.ToLower(head)
	return strings.HasPrefix(lower, "<!doctype") ||
		strings.HasPrefix(lower, "<html") ||
		strings.Contains(lower, "requires javascript")
}

// pick 安全取第 i 个元素;越界或 nil 返回 nil。
func pick(xs []*float64, i int) *float64 {
	if i < 0 || i >= len(xs) {
		return nil
	}
	return xs[i]
}

// fmtNum 用 big.Rat 把 float64 转成十进制字符串,避免 %f 引入新的舍入。
//
// float64 本身已是二进制近似,但 FormatFloat(-1) 输出的是「能唯一还原该
// float64 的最短十进制」,再交给 NUMERIC(28,10) 存储时不会二次损失。
func fmtNum(f float64) string {
	r := new(big.Rat).SetFloat64(f)
	if r == nil {
		return "0"
	}
	// 保留足够位数让 NUMERIC(28,10) 不截断有效数字。
	return r.FloatString(10)
}

// mustDBPath 返回库文件路径（配置优先级见 config.DBPath）。
//
// 单独一个函数是为了让 main 里的错误分支保持一句话：
// 「拿不到路径」和「连不上库」是两类失败，提示文案要区分。
func mustDBPath() string {
	path, _, err := db.DSN()
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取数据库路径失败: %v\n", err)
		os.Exit(1)
	}
	return path
}
