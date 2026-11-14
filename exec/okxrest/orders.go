package okxrest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// PlaceOrder 提交一笔订单。
//
// 返回的 *OrderAck 只代表「交易所受理了」。data[0].sCode 非 "0" 时
// 按错误返回 —— 顶层 code=0 但 sCode!=0 是 OKX 的常见形态
// （请求合法、单笔被拒），只看顶层会把拒单当成功。
func (c *Client) PlaceOrder(ctx context.Context, req OrderRequest) (*OrderAck, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("okx: 序列化下单请求失败: %w", err)
	}
	var acks []OrderAck
	if err := c.c.do(ctx, "POST", "/api/v5/trade/order", body, &acks); err != nil {
		return nil, err
	}
	if len(acks) == 0 {
		return nil, &APIError{Msg: "下单响应为空数组", Kind: ErrRetryable}
	}
	ack := &acks[0]
	if ack.SCode != "" && ack.SCode != "0" {
		return nil, &APIError{SCode: ack.SCode, Msg: ack.SMsg, Kind: classifyOrderSCode(ack.SCode)}
	}
	if ack.OrdID == "" {
		return nil, &APIError{Msg: "下单响应缺少 ordId", Detail: ack.SMsg, Kind: ErrRetryable}
	}
	return ack, nil
}

// SetLeverage 设置杠杆。幂等：重复设同一个值返回成功。
//
// 杠杆是账户级设置，不属于订单体；开仓前必须单独调一次，
// 否则第一次下单会用交易所默认杠杆（通常 3x），与计划的仓位不符。
func (c *Client) SetLeverage(ctx context.Context, instID, lever, mgnMode string) error {
	body, err := json.Marshal(map[string]string{
		"instId": instID, "lever": lever, "mgnMode": mgnMode,
	})
	if err != nil {
		return err
	}
	return c.c.do(ctx, "POST", "/api/v5/account/set-leverage", body, nil)
}

// PlaceAlgoOCO 挂一组止盈止损（ordType=oco，closeFraction=1 即全平）。
//
// 用于「开仓时 attachAlgoOrds 失败」的补挂路径。
func (c *Client) PlaceAlgoOCO(ctx context.Context, req AlgoRequest) (string, error) {
	if req.OrdType == "" {
		req.OrdType = "oco"
	}
	if req.CloseFraction == "" {
		req.CloseFraction = "1"
	}
	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("okx: 序列化算法单请求失败: %w", err)
	}
	var out []struct {
		AlgoID string `json:"algoId"`
		SCode  string `json:"sCode"`
		SMsg   string `json:"sMsg"`
	}
	if err := c.c.do(ctx, "POST", "/api/v5/trade/order-algo", body, &out); err != nil {
		return "", err
	}
	if len(out) == 0 {
		return "", &APIError{Msg: "算法单响应为空数组", Kind: ErrRetryable}
	}
	if out[0].SCode != "" && out[0].SCode != "0" {
		return "", &APIError{SCode: out[0].SCode, Msg: out[0].SMsg, Kind: classifyOrderSCode(out[0].SCode)}
	}
	return out[0].AlgoID, nil
}

// CancelAlgo 撤销一条算法单。
func (c *Client) CancelAlgo(ctx context.Context, instID, algoID string) error {
	body, err := json.Marshal([]map[string]string{{"instId": instID, "algoId": algoID}})
	if err != nil {
		return err
	}
	return c.c.do(ctx, "POST", "/api/v5/trade/cancel-algos", body, nil)
}

// AmendAlgoOCO 原子修改一条 OCO 的止盈/止损触发价（POST /api/v5/trade/amend-algos）。
//
// # 为什么用 amend 而不是「撤旧单 + 挂新单」
//
// 撤挂之间有窗口期：旧保护已经没了、新保护还没生效。价格恰好在那几百毫秒
// 里穿过止损，这一仓就是裸的。实测这个窗口在弱网下能到秒级。
//
// 只有 amend 被拒（比如新价在现价错误一侧）时才回退到撤挂 —— 那时
// 窗口期无法避免，但至少是「已知会失败」的分支，不是默认路径。
//
// 传空串表示该项不改。
func (c *Client) AmendAlgoOCO(ctx context.Context, instID, algoID, tpTriggerPx, slTriggerPx string) error {
	req := map[string]string{"instId": instID, "algoId": algoID}
	if v := strings.TrimSpace(tpTriggerPx); v != "" {
		req["newTpTriggerPx"] = v
		// -1 = 触发后按市价成交。与 PlaceAlgoOCO 保持同一个口径，
		// 否则「改完止盈变成限价单挂不上」—— 成交行为在改价前后不一致。
		req["newTpOrdPx"] = "-1"
		req["newTpTriggerPxType"] = "last"
	}
	if v := strings.TrimSpace(slTriggerPx); v != "" {
		req["newSlTriggerPx"] = v
		req["newSlOrdPx"] = "-1"
		req["newSlTriggerPxType"] = "last"
	}
	if len(req) == 2 {
		return fmt.Errorf("okx: 改止盈止损至少要给一个价格")
	}
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("okx: 序列化改价请求失败: %w", err)
	}
	var out []struct {
		AlgoID string `json:"algoId"`
		SCode  string `json:"sCode"`
		SMsg   string `json:"sMsg"`
	}
	if err := c.c.do(ctx, "POST", "/api/v5/trade/amend-algos", body, &out); err != nil {
		return err
	}
	if len(out) == 0 {
		return &APIError{Msg: "改价响应为空数组", Kind: ErrRetryable}
	}
	if out[0].SCode != "" && out[0].SCode != "0" {
		return &APIError{SCode: out[0].SCode, Msg: out[0].SMsg, Kind: classifyOrderSCode(out[0].SCode)}
	}
	return nil
}

// ListAlgoOrders 列出未触发的算法单。
//
// 只返回 state ∈ {live, effective} 的记录 —— 已撤销/已触发的不算「保护还在」。
func (c *Client) ListAlgoOrders(ctx context.Context, instType, ordType string) ([]AlgoOrder, error) {
	q := url.Values{}
	q.Set("instType", instType)
	if ordType != "" {
		q.Set("ordType", ordType)
	}
	var out []AlgoOrder
	if err := c.c.do(ctx, "GET", "/api/v5/trade/orders-algo-pending?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	live := make([]AlgoOrder, 0, len(out))
	for _, a := range out {
		if a.State == "live" || a.State == "effective" {
			live = append(live, a)
		}
	}
	return live, nil
}

// QueryOrderByClOrdID 按我方幂等键查单。查不到返回 (nil, nil)。
func (c *Client) QueryOrderByClOrdID(ctx context.Context, instID, clOrdID string) (*OrderState, error) {
	q := url.Values{}
	q.Set("instId", instID)
	q.Set("clOrdId", clOrdID)
	var out []OrderState
	if err := c.c.do(ctx, "GET", "/api/v5/trade/order?"+q.Encode(), nil, &out); err != nil {
		// 51603/51400 之类的「查无此单」在 OKX 侧是 code=51600/51603 的 200 响应，
		// 语义是「没有这张单」，不是系统故障。
		var apiErr *APIError
		if asAPIError(err, &apiErr) && (apiErr.Code == "51600" || apiErr.Code == "51603") {
			return nil, nil
		}
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	return &out[0], nil
}

func asAPIError(err error, dst **APIError) bool {
	for err != nil {
		if e, ok := err.(*APIError); ok {
			*dst = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// GetJSON 发一次 GET 并把 data 解到 out。供对账等只需要原始数据的调用方使用。
func (c *Client) GetJSON(ctx context.Context, path string, out any) error {
	return c.c.do(ctx, "GET", path, nil, out)
}
