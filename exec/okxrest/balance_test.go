package okxrest

import "testing"

// 纸面仓位校验：这笔 10000 张 NVDA（名义 160 万）在 8.7 万权益的账户上
// 必须被拒 —— 2026-11-15 的荒谬纸面盈亏就是因为它被放过了。
func TestFitsMargin(t *testing.T) {
	cases := []struct {
		name                       string
		notional, equity, leverage string
		want                       bool
	}{
		{"小仓正常放行", "1000", "87000", "3", true},
		{"正好等于上限", "261000", "87000", "3", true},
		{"超过上限拒绝", "1600000", "87000", "3", false},
		{"杠杆无效按 1 倍", "100000", "87000", "", false},
		{"杠杆为 1 时可支撑", "80000", "87000", "1", true},
		{"权益为零一律拒绝", "1", "0", "3", false},
		{"非法数字拒绝", "abc", "87000", "3", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, why := FitsMargin(c.notional, c.equity, c.leverage)
			if got != c.want {
				t.Errorf("FitsMargin(%s,%s,%s) = %v, want %v（%s）",
					c.notional, c.equity, c.leverage, got, c.want, why)
			}
			if !got && why == "" {
				t.Error("拒绝时必须给原因")
			}
		})
	}
}

// 权益取值优先级：availEq > eq > availBal；没有该币种返回 "0"。
func TestEquityOf(t *testing.T) {
	b := &AccountBalance{Details: []map[string]string{
		{"ccy": "USDT", "avail_eq": "5000", "eq": "9000", "avail_bal": "4000"},
		{"ccy": "BTC", "eq": "1"},
		{"ccy": "ETH", "avail_eq": "0", "eq": "2"},
	}}
	if got := b.EquityOf("usdt"); got != "5000" {
		t.Errorf("USDT = %q, want 5000（优先 avail_eq）", got)
	}
	if got := b.EquityOf("BTC"); got != "1" {
		t.Errorf("BTC = %q, want 1（回退 eq）", got)
	}
	if got := b.EquityOf("ETH"); got != "2" {
		t.Errorf("ETH = %q, want 2（avail_eq 为 0 时回退 eq）", got)
	}
	if got := b.EquityOf("DOGE"); got != "0" {
		t.Errorf("不存在的币种应为 0，得到 %q", got)
	}
}
