package okxrest

import (
	"context"
	"fmt"
	"net/url"
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

// ListFillsByInst 拉某标的最近窗口的成交（便捷封装）。
//
// 窗口默认 7 天：OKX 的 fills 保留期远大于此，但把窗口放大到 30 天会让
// 一次调用就撞上 100 条上限而静默截断。要更长的历史必须自己切片。
func (c *Client) ListFillsByInst(ctx context.Context, instID string, days int) ([]Fill, error) {
	if days <= 0 {
		days = 7
	}
	now := time.Now().UTC()
	q := url.Values{}
	q.Set("instType", "SWAP")
	q.Set("instId", instID)
	q.Set("begin", fmt.Sprintf("%d", now.AddDate(0, 0, -days).UnixMilli()))
	q.Set("end", fmt.Sprintf("%d", now.UnixMilli()))
	q.Set("limit", "100")
	var out []Fill
	if err := c.c.do(ctx, "GET", "/api/v5/trade/fills?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}
