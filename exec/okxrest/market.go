package okxrest

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// Ticker 是一个标的的最新价快照（GET /api/v5/market/ticker）。
//
// 只取模拟盘判断止盈止损要用的字段；其余字段不解析 —— 少一个字段
// 就少一处与文档不同步的风险。
type Ticker struct {
	InstID string `json:"instId"`
	Last   string `json:"last"`
	BidPx  string `json:"bidPx"`
	AskPx  string `json:"askPx"`
	Ts     string `json:"ts"`
}

// LastPrice 返回该标的最新成交价。
//
// 走公共接口（不签名）：模拟盘判断止盈止损只需要公开行情，
// 而这条路径在凭证失效时仍应可用（否则模拟盘会因为实盘 key 过期而停摆）。
func (c *Client) LastPrice(ctx context.Context, instID string) (string, error) {
	instID = strings.TrimSpace(instID)
	if instID == "" {
		return "", fmt.Errorf("okx: instId 不能为空")
	}
	q := url.Values{}
	q.Set("instId", instID)
	var out []Ticker
	if err := c.c.doPublic(ctx, "/api/v5/market/ticker?"+q.Encode(), &out); err != nil {
		return "", err
	}
	if len(out) == 0 || strings.TrimSpace(out[0].Last) == "" {
		return "", fmt.Errorf("okx: %s 无最新价", instID)
	}
	return strings.TrimSpace(out[0].Last), nil
}
