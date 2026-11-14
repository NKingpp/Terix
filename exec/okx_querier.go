package exec

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/exec/okxrest"
)

// OkxQuerier 是 ChannelQuerier 的 OKX 实现（对账用）。
//
// # 为什么需要它
//
// exec/reconcile.go 定义了 ChannelQuerier 接口，但全仓没有任何实现，
// server 装配时传的是 nil —— 对账从未真正运行过。没有对账，
// 「本地记着 submitted、交易所其实已成交」这种分叉就永远不会被发现。
type OkxQuerier struct {
	creds  func() (*db.OkxTradeCred, error)
	client *okxrest.Client
	credID int64
}

// NewOkxQuerier 构造对账查询器。creds 为空时返回 nil（调用方据此跳过对账）。
func NewOkxQuerier(creds func() (*db.OkxTradeCred, error)) *OkxQuerier {
	if creds == nil {
		return nil
	}
	return &OkxQuerier{creds: creds}
}

// QueryByClientOrderID 按我方幂等键查通道侧状态。
//
// 走 orders-history 而不是 GET /trade/order：后者必须带 instId，而对账时
// 手里只有 clientOrderId。多查一次换取「不猜 instId 导致误判订单丢失」。
func (q *OkxQuerier) QueryByClientOrderID(ctx context.Context, clOrdID string) (ChannelAck, error) {
	if q == nil || clOrdID == "" {
		return ChannelAck{}, fmt.Errorf("对账查询：client_order_id 不能为空")
	}
	cred, err := q.creds()
	if err != nil {
		return ChannelAck{}, err
	}
	if q.client == nil || q.credID != cred.ID {
		q.client = okxrest.NewClient(okxrest.Credentials{
			APIKey: cred.APIKey, SecretKey: cred.SecretKey, Passphrase: cred.Passphrase,
			Demo: cred.Demo, BaseURL: cred.BaseURL,
		})
		q.credID = cred.ID
	}

	now := time.Now().UTC()
	qv := url.Values{}
	qv.Set("instType", "SWAP")
	qv.Set("begin", fmt.Sprintf("%d", now.Add(-7*24*time.Hour).UnixMilli()))
	qv.Set("end", fmt.Sprintf("%d", now.UnixMilli()))
	qv.Set("limit", "100")

	var rows []struct {
		OrdID     string `json:"ordId"`
		ClOrdID   string `json:"clOrdId"`
		State     string `json:"state"`
		AccFillSz string `json:"accFillSz"`
		AvgPx     string `json:"avgPx"`
	}
	if err := q.client.GetJSON(ctx, "/api/v5/trade/orders-history?"+qv.Encode(), &rows); err != nil {
		return ChannelAck{}, err
	}
	for _, r := range rows {
		if r.ClOrdID != clOrdID {
			continue
		}
		raw, _ := json.Marshal(r)
		return ChannelAck{
			Found:     true,
			State:     mapOkxState(r.State),
			FilledQty: r.AccFillSz,
			AvgPrice:  r.AvgPx,
			Raw:       raw,
		}, nil
	}
	return ChannelAck{Found: false}, nil
}

// mapOkxState 把 OKX 的订单状态映射到 orders_execution 的 state 枚举。
func mapOkxState(s string) string {
	switch s {
	case "live":
		return db.ExecStateSubmitted
	case "partially_filled":
		return db.ExecStatePartial
	case "filled":
		return db.ExecStateFilled
	case "canceled", "mmp_canceled":
		return db.ExecStateCancelled
	default:
		return db.ExecStateSubmitted
	}
}
