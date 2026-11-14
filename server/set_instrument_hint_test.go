package server

import (
	"strings"
	"testing"
)

// SetInstrumentHint 是「选品员选了个不能下单的标的」的唯一提示出口。
// 纯函数化是为了能在无 DB 的情况下验四种分支。
func TestInstrumentExecutabilityHint(t *testing.T) {
	cases := []struct {
		symbol, market string
		wantContains   string // "" = 不该有提示
	}{
		{"BTC-USDT-SWAP", "crypto", ""},
		{"AAPL-USDT-SWAP", "crypto", ""},
		{"BTC-USDT-SWAP", "us", "market 建议填 crypto"},
		{"BTC-USDT", "crypto", "不是永续 instId"},
		{"AAPL", "us", "只能分析、不能自动执行"},
		{"600519", "cn", "只能分析、不能自动执行"},
	}
	for _, c := range cases {
		got := instrumentExecutabilityHint(c.symbol, c.market)
		if c.wantContains == "" {
			if got != "" {
				t.Errorf("%s/%s 不该有提示，得到 %q", c.symbol, c.market, got)
			}
			continue
		}
		if !strings.Contains(got, c.wantContains) {
			t.Errorf("%s/%s 提示应含 %q，得到 %q", c.symbol, c.market, c.wantContains, got)
		}
	}
}
