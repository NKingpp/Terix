package exec

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/exec/okxrest"
)

// FillsSyncer 把交易所侧的成交明细同步进本地 trade_fills。
//
// # 为什么要单独同步而不是只靠订单状态
//
// orders_execution 只有均价与总量，没有手续费、没有分批成交的价格与时间。
// 而「平仓后到底赚了多少」必须逐笔算 —— 均价 × 总量算不出手续费，
// 也算不出分批平仓的实际成本。
//
// # 幂等
//
// trade_fills.fill_id 有 UNIQUE 约束，重复拉同一窗口只会命中冲突。
// 所以可以无脑每轮拉最近窗口，不需要游标。
type FillsSyncer struct {
	db    *db.DB
	creds func() (*db.OkxTradeCred, error)
	// window 是每轮回溯的天数。7 天足够覆盖「开仓到平仓」的常规周期，
	// 又不会让单次查询撞上 100 条上限。
	window int
	client *okxrest.Client
	credID int64
}

// NewFillsSyncer 构造同步器。
func NewFillsSyncer(d *db.DB, creds func() (*db.OkxTradeCred, error)) *FillsSyncer {
	return &FillsSyncer{db: d, creds: creds, window: 7}
}

func (s *FillsSyncer) clientFor(cred *db.OkxTradeCred) *okxrest.Client {
	if s.client == nil || s.credID != cred.ID {
		s.client = okxrest.NewClient(okxrest.Credentials{
			APIKey: cred.APIKey, SecretKey: cred.SecretKey, Passphrase: cred.Passphrase,
			Demo: cred.Demo, BaseURL: cred.BaseURL,
		})
		s.credID = cred.ID
	}
	return s.client
}

// SyncAccount 拉某账户所有「我方下过单的标的」的成交，写入本地。
//
// 返回新插入的条数。单个标的失败不中断其余（一次限流不该让整轮白跑）。
func (s *FillsSyncer) SyncAccount(ctx context.Context, accountID int64) (int, error) {
	if s == nil || s.db == nil || s.creds == nil {
		return 0, nil
	}
	instIDs, err := s.db.DistinctInstIDsForAccount(accountID)
	if err != nil {
		return 0, fmt.Errorf("列出待同步标的失败: %w", err)
	}
	if len(instIDs) == 0 {
		return 0, nil
	}
	cred, err := s.creds()
	if err != nil {
		return 0, fmt.Errorf("读取 OKX 交易凭据失败: %w", err)
	}
	client := s.clientFor(cred)

	now := time.Now().UTC()
	total := 0
	var firstErr error
	for _, inst := range instIDs {
		fills, fErr := client.ListFillsByInst(ctx, inst, s.window)
		if fErr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("拉 %s 成交失败: %w", inst, fErr)
			}
			continue
		}
		// 历史遗留行的归属回填（新逻辑之前插入的成交 draft_id 为空）。
		// 逐条持仓窗口回填（同一标的会反复开平，只补最新一条会让更早的漏掉）。
		if dids, dErr := s.db.DraftIDsForInst(accountID, inst); dErr == nil {
			for _, did := range dids {
				if n, bErr := s.db.BackfillFillDraftID(accountID, inst, did); bErr == nil && n > 0 {
					slog.Info("成交归属回填", "account_id", accountID, "inst", inst, "draft_id", did, "rows", n)
				}
			}
		}
		for _, f := range fills {
			// 去重键：tradeId 是成交维度唯一；billId 是账单维度（可能多条）。
			// 两者都空说明响应结构变了 —— 跳过并记错，绝不写空键进库
			// （空键会撞唯一约束，导致整批成交静默丢弃）。
			key := strings.TrimSpace(f.TradeID)
			if key == "" {
				key = strings.TrimSpace(f.BillID)
			}
			if key == "" {
				if firstErr == nil {
					firstErr = fmt.Errorf("成交缺少 tradeId/billId（OKX 响应结构可能已变）: inst=%s ord=%s", f.InstID, f.OrdID)
				}
				continue
			}
			fillID := f.InstID + ":" + key // instId 前缀防跨标的 tradeId 撞车
			_, inserted, insErr := s.db.InsertFill(db.TradeFill{
				FillID: fillID, AccountID: accountID, InstID: f.InstID,
				Side: f.Side, PosSide: f.PosSide, FillPx: f.FillPx, FillSz: f.FillSz,
				Fee: f.Fee, FeeCcy: f.FeeCcy, OrdID: f.OrdID, ClOrdID: f.ClOrdID,
				DraftID:  s.resolveDraftID(accountID, f.InstID, f.ClOrdID),
				FilledAt: msToRFC3339(f.Ts, now),
			})
			if insErr != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("写入成交 %s 失败: %w", fillID, insErr)
				}
				continue
			}
			if inserted {
				total++
			}
		}
	}
	return total, firstErr
}

// msToRFC3339 把 OKX 的毫秒时间戳转成 ISO8601。
//
// 解析失败时用 fallback（当前时间）而不是丢掉这条成交 —— 少记一笔成交
// 会让盈亏偏小，而「时间不准」只影响排序（同一毫秒内的两笔顺序，
// 对加权平均成本无影响）。
func msToRFC3339(ms string, fallback time.Time) string {
	var n int64
	if _, err := fmt.Sscanf(ms, "%d", &n); err != nil || n <= 0 {
		return fallback.UTC().Format(time.RFC3339)
	}
	return time.UnixMilli(n).UTC().Format(time.RFC3339)
}

// resolveDraftID 把一笔成交归属到草案。
//
// 两条路径：
//  1. cl_ord_id 是我方派生的（artex…）→ 直接反查 orders_execution
//  2. 不是 → 交易所 OCO 触发的平仓（clOrdId 形如 O4098…），
//     只能靠「同账户同标的最近一条持仓跟踪」归属
//
// 路径 2 不能省：没有它，平仓成交不归属任何任务 → 任务的模拟/实盘盈亏
// 里永远只看到开仓腿，持仓永远显示未平（实测任务 23）。
func (s *FillsSyncer) resolveDraftID(accountID int64, instID, clOrdID string) int64 {
	if id := s.db.DraftIDByClOrdID(clOrdID); id > 0 {
		return id
	}
	return s.db.DraftIDByInstRecently(accountID, instID)
}
