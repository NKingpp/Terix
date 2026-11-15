package okxrest

// 账户权益读取：纸面模式的仓位校验需要它。
//
// # 为什么纸面模式需要真实权益（2026-11-15 荒谬仓位事故）
//
// 纸面模式原来对仓位**零校验**：模型按「用全部余额」算仓位，算出
// 10000 张 NVDA 永续（名义约 160 万 USD），而账户权益只有 8.7 万。
// 引擎照单全收记进纸面账本，于是 dashboard 显示「模拟盘赚 2766 USDT」。
//
// 那个数字没有任何决策含义：它来自一笔在现实中被交易所必然拒绝的单。
// 纸面模拟的价值就在于「价格与保证金约束都是真的，只是不花真钱」——
// 允许不可能成交的单，模拟结果就是纯噪声。
//
// 所以这里读真实权益，让纸面与实盘受同一套约束。

import (
	"context"
	"fmt"
	"math/big"
	"strings"
)

// balanceResp 是 GET /api/v5/account/balance 的响应。
type balanceResp struct {
	TotalEq string `json:"totalEq"`
	AdjEq   string `json:"adjEq"`
	// Details 按币种明细，纸面校验时用来找计价币可用量。
	Details []struct {
		Ccy       string `json:"ccy"`
		Eq        string `json:"eq"`
		AvailBal  string `json:"availBal"`
		AvailEq   string `json:"availEq"`
		EqUsd     string `json:"eqUsd"`
		FrozenBal string `json:"frozenBal"`
	} `json:"details"`
	UTime string `json:"uTime"`
}

// AccountBalance 是账户权益快照（金额全部保持字符串，不转 float）。
type AccountBalance struct {
	// TotalEq 是账户总权益（USD 计价）。
	TotalEq string `json:"total_eq"`
	// AdjEq 是有效保证金（部分场景为空）。
	AdjEq string `json:"adj_eq"`
	// Details 是币种明细的原文，供调用方按需取用。
	Details []map[string]string `json:"details"`
	UTime   string              `json:"u_time"`
}

// FetchAccountBalance 读账户权益（需要签名，即需要有效凭证）。
//
// 与 LastPrice 不同，这个接口**必须**用私钥签名 —— 所以它同时是一次
// 隐式的凭证校验：签名错会返回 50113/50101，能立刻区分「key 错」
// 与「网络不通」。
func (c *Client) FetchAccountBalance(ctx context.Context) (*AccountBalance, error) {
	var out []balanceResp
	if err := c.c.do(ctx, "GET", "/api/v5/account/balance", nil, &out); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("okx: 余额响应为空")
	}
	b := out[0]
	res := &AccountBalance{TotalEq: b.TotalEq, AdjEq: b.AdjEq, UTime: b.UTime}
	for _, d := range b.Details {
		res.Details = append(res.Details, map[string]string{
			"ccy": d.Ccy, "eq": d.Eq, "avail_bal": d.AvailBal,
			"avail_eq": d.AvailEq, "eq_usd": d.EqUsd, "frozen_bal": d.FrozenBal,
		})
	}
	return res, nil
}

// EquityOf 从余额快照里取某计价币的可用权益。
//
// 优先 availEq（可用保证金），回退 eq（总持仓），再回退 equal 的
// eq_usd（USDT 计价）。三者都空说明该币种没有资产 —— 返回 "0"。
func (b *AccountBalance) EquityOf(ccy string) string {
	if b == nil {
		return "0"
	}
	want := strings.ToUpper(strings.TrimSpace(ccy))
	for _, d := range b.Details {
		if strings.ToUpper(strings.TrimSpace(d["ccy"])) != want {
			continue
		}
		for _, k := range []string{"avail_eq", "eq", "avail_bal"} {
			if v := strings.TrimSpace(d[k]); v != "" && v != "0" {
				return v
			}
		}
		return "0"
	}
	return "0"
}

// FitsMargin 判断一笔名义金额能否被该权益支撑。
//
// leverage 为空或非法时按 1 倍（不放大购买力）—— 保守方向总是安全的。
// 返回 (能否, 说明)。
func FitsMargin(notional, equity, leverage string) (bool, string) {
	n, ok1 := new(big.Rat).SetString(strings.TrimSpace(notional))
	e, ok2 := new(big.Rat).SetString(strings.TrimSpace(equity))
	if !ok1 || !ok2 {
		return false, "金额/权益不是合法数字，无法校验"
	}
	if e.Sign() <= 0 {
		return false, fmt.Sprintf("账户权益为 %s，无法支撑任何仓位", equity)
	}
	lev := new(big.Rat).SetInt64(1)
	if v, ok := new(big.Rat).SetString(strings.TrimSpace(leverage)); ok && v.Cmp(lev) > 0 {
		lev = v
	}
	limit := new(big.Rat).Mul(e, lev)
	if n.Cmp(limit) > 0 {
		return false, fmt.Sprintf(
			"名义金额 %s 超过可承受上限 %s（权益 %s × %s 倍杠杆）",
			notional, limit.FloatString(2), equity, lev.FloatString(0))
	}
	return true, ""
}
