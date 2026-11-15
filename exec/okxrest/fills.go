package okxrest

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Fill 是一笔成交明细（GET /api/v5/trade/fills 的一项）。
//
// # 为什么需要它（而不是只用订单的 avgPx / accFillSz）
//
// 订单级的均价与总量算不出「净盈亏」：手续费只在 fills 里，
// 而且分批成交时每笔的价格不同 —— 用均价 × 总量会漏掉手续费、
// 也无法解释「为什么这笔单的净收益比预期少」。
//
// # 字段名的坑（2026-11-14 实测踩过）
//
// OKX 的成交明细**没有 `fillId`**。去重键只能是 `tradeId`
// （`billId` 是账单维度，同一笔成交可能对应多条账单）。
// 按文档想当然写成 fillId 的后果：解析出空串 → 落库被唯一键校验拒绝
// → 每轮同步静默丢全部成交，而日志只说「fill_id 不能为空」。
type Fill struct {
	// TradeID 是成交 ID，本地的去重键。
	TradeID  string `json:"tradeId"`
	BillID   string `json:"billId"`
	OrdID    string `json:"ordId"`
	ClOrdID  string `json:"clOrdId"`
	InstID   string `json:"instId"`
	InstType string `json:"instType"`
	Side     string `json:"side"`    // buy | sell
	PosSide  string `json:"posSide"` // long | short | net
	FillPx   string `json:"fillPx"`
	FillSz   string `json:"fillSz"`
	// Fee 是负数（OKX 用负号表示支出）。计算净盈亏时直接相加即可。
	Fee    string `json:"fee"`
	FeeCcy string `json:"feeCcy"`
	// ExecType 是流动性类型：T=taker（吃单，手续费通常更高）| M=maker。
	ExecType string `json:"execType"`
	// Ts 是毫秒时间戳字符串。
	Ts string `json:"ts"`
}

// ListFills 按时间区间拉成交明细。
//
// OKX 该接口单次最多返回 100 条（limit 上限），且必须给 begin/end（ms）。
// 调用方按天/按小时切片，避免「一次拉一个月」漏数据 —— 漏数据在盈亏
// 计算里是静默错误（数字看起来合理但偏小）。
func (c *Client) ListFills(ctx context.Context, instType string, begin, end time.Time) ([]Fill, error) {
	q := url.Values{}
	q.Set("instType", instType)
	q.Set("begin", fmt.Sprintf("%d", begin.UnixMilli()))
	q.Set("end", fmt.Sprintf("%d", end.UnixMilli()))
	q.Set("limit", "100")
	var out []Fill
	if err := c.c.do(ctx, "GET", "/api/v5/trade/fills?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListFillsByInst 拉某标的最近窗口的**全部**成交（自动分页）。
//
// # 为什么必须分页（2026-11-15 数据不一致事故）
//
// 原来这里硬编码 limit=100 且只发一次请求，注释写的是「7 天足够，
// 不会撞上 100 条上限」。这个假设是错的：
//
//	一笔市价单在盘口薄的时候会被撮合拆成**上百笔微成交**。
//	实测 NVDA-USDT-SWAP 一笔 150 张的卖单产生 150+ 条 fill，
//	我们只拿到前 100 条（68.95 张）。
//
// 后果不是「少几条数字偏小」这种可容忍的误差，而是**净持仓符号都错**：
// 台账算出 net=-68.95，而交易所实际是 0。基于这个净持仓做的所有
// 盈亏、敞口、对账结论全部失真，且没有任何报错。
//
// # 分页机制
//
// OKX 用 billId 游标：`after=<billId>` 返回**比该条更早**的记录。
// 响应按时间倒序，所以取本页最后一条的 billId 作为下一页游标，
// 直到某页不足 pageSize（说明到底了）。
//
// 去重按 tradeId（fill 维度唯一）。billId 在 fill 接口里足够唯一，
// 但 tradeId 更严格 —— 万一 OKX 某天返回重复 billId，游标不前进会被
// 下面的 last.BillID == cursor 检查挡住，不会死循环。
func (c *Client) ListFillsByInst(ctx context.Context, instID string, days int) ([]Fill, error) {
	if days <= 0 {
		days = 7
	}
	now := time.Now().UTC()
	begin := fmt.Sprintf("%d", now.AddDate(0, 0, -days).UnixMilli())
	end := fmt.Sprintf("%d", now.UnixMilli())

	const pageSize = 100 // OKX 上限
	// 上限保护：6000 笔。正常一圈远小于此；这个值只为防「后端异常时无限循环」。
	const maxPages = 60

	var all []Fill
	seen := make(map[string]struct{}, 256)
	cursor := ""
	for page := 0; page < maxPages; page++ {
		q := url.Values{}
		q.Set("instType", "SWAP")
		q.Set("instId", instID)
		q.Set("begin", begin)
		q.Set("end", end)
		q.Set("limit", fmt.Sprintf("%d", pageSize))
		if cursor != "" {
			q.Set("after", cursor)
		}
		var out []Fill
		if err := c.c.do(ctx, "GET", "/api/v5/trade/fills?"+q.Encode(), nil, &out); err != nil {
			// 返回已取到的部分 + 错误：让调用方既能落库已有的，
			// 又知道这一轮不完整（下一轮会补）。
			return all, err
		}
		added := 0
		for _, f := range out {
			k := strings.TrimSpace(f.TradeID)
			if k == "" {
				k = strings.TrimSpace(f.BillID)
			}
			if k == "" {
				continue
			}
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
			all = append(all, f)
			added++
		}
		if len(out) < pageSize {
			break // 到底了
		}
		last := out[len(out)-1]
		if last.BillID == "" || last.BillID == cursor {
			break // 没游标可用 / 游标不前进，防死循环
		}
		cursor = last.BillID
		if added == 0 {
			break // 整页都重复 → 后端在重复返回同一批，再问也没用
		}
	}
	return all, nil
}
