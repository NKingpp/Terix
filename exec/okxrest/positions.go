package okxrest

import (
	"context"
	"net/url"
)

// GetPositions 读取当前持仓（只返回 pos != "0" 的）。
//
// 过滤在客户端做：OKX 会把「曾持仓、现在已平」的记录也返回（pos="0"），
// 不过滤会让「有仓不开新仓」的判断永远为真。
func (c *Client) GetPositions(ctx context.Context, instType string) ([]Position, error) {
	q := url.Values{}
	q.Set("instType", instType)
	var out []Position
	if err := c.c.do(ctx, "GET", "/api/v5/account/positions?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	live := make([]Position, 0, len(out))
	for _, p := range out {
		if p.Pos == "" || p.Pos == "0" || p.Pos == "0.0" {
			continue
		}
		live = append(live, p)
	}
	return live, nil
}

// GetAccountConfig 读账户配置（uid / posMode）。
//
// posMode 决定 posSide 怎么填：net_mode → "net"，long_short_mode → long/short。
// 这填错了会被 51000 拒，且错误文案只有「Parameter posSide error」。
func (c *Client) GetAccountConfig(ctx context.Context) (*AccountConfig, error) {
	var out []AccountConfig
	if err := c.c.do(ctx, "GET", "/api/v5/account/config", nil, &out); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, &APIError{Msg: "account/config 返回空数组（凭证无该账户权限）", Kind: ErrPermanent}
	}
	return &out[0], nil
}
