package server

import (
	"net/http"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// TestResolveInstrumentFromTaskTextE2E 端到端：文本 → 行情验证 → 登记。
//
// 用真实 sidecar（127.0.0.1:8791）。没有 sidecar 时跳过 —— 这条测的是
// 「真能查到 MSFT」而不是「错误处理对不对」（后者由纯函数单测覆盖）。
func TestResolveInstrumentFromTaskTextE2E(t *testing.T) {
	s, _, _ := newAuthServer(t)

	// sidecar 不在就跑不了
	cli := s.marketClientFor("us")
	if cli == nil {
		t.Skip("market client 未装配")
	}
	if _, _, err := cli.Health(t.Context()); err != nil {
		t.Skipf("akshare sidecar 不可用（%v），跳过端到端", err)
	}

	r, _ := http.NewRequest("POST", "/api/tasks", nil)
	inst := s.resolveInstrumentFromTaskText(r, "", "给出 msft 交易策略")
	if inst == nil {
		t.Fatal("应能从「给出 msft 交易策略」识别并登记 MSFT")
	}
	if inst.Symbol != "MSFT" {
		t.Errorf("识别到 %q，want MSFT", inst.Symbol)
	}
	if inst.Market != "us" {
		t.Errorf("market = %q, want us", inst.Market)
	}
	if inst.AssetClass != db.AssetClassStock {
		t.Errorf("asset_class = %q, want stock", inst.AssetClass)
	}
	t.Cleanup(func() {
		_, _ = s.m.pg.Exec(`DELETE FROM instruments WHERE id=$1`, inst.ID)
	})
}
