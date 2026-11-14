package okxrest

import (
	"bytes"
	"net/http"
	"testing"
)

// 模拟盘必须带 x-simulated-trading 头。
//
// 漏掉它时 OKX 返回 50101「APIKey does not match current environment」，
// 文案会把人引去查 key，实际只是没声明环境。
func TestDemoHeader(t *testing.T) {
	body := []byte(`{}`)
	req, _ := http.NewRequest(http.MethodPost, "https://www.okx.com/api/v5/trade/order", bytes.NewReader(body))

	setAuthHeaders(req, Credentials{APIKey: "k", SecretKey: "s", Passphrase: "p", Demo: true}, body)
	if got := req.Header.Get("x-simulated-trading"); got != "1" {
		t.Fatalf("demo 凭据必须带 x-simulated-trading=1，得到 %q", got)
	}

	req2, _ := http.NewRequest(http.MethodPost, "https://www.okx.com/api/v5/trade/order", bytes.NewReader(body))
	setAuthHeaders(req2, Credentials{APIKey: "k", SecretKey: "s", Passphrase: "p", Demo: false}, body)
	if got := req2.Header.Get("x-simulated-trading"); got != "" {
		t.Fatalf("实盘凭据不该带 simulated 头，得到 %q", got)
	}
}

// 公开行情不签名（少一个收到凭证的端点）。
func TestPublicPathNoAuthHeaders(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://www.okx.com/api/v5/public/instruments", nil)
	setAuthHeaders(req, Credentials{APIKey: "k", SecretKey: "s", Passphrase: "p", Demo: true}, nil)
	if got := req.Header.Get("OK-ACCESS-KEY"); got != "" {
		t.Fatalf("公开接口不该带 key，得到 %q", got)
	}
	if got := req.Header.Get("x-simulated-trading"); got != "" {
		t.Fatalf("公开接口不该带 simulated 头，得到 %q", got)
	}
}

// 签名必须包含 query string：漏掉会让带参数的 GET 全部 401「Invalid Sign」。
func TestSignCoversQuery(t *testing.T) {
	a := sign("secret", "2026-11-14T00:00:00.000Z", "GET", "/api/v5/account/positions?instType=SWAP", "")
	b := sign("secret", "2026-11-14T00:00:00.000Z", "GET", "/api/v5/account/positions", "")
	if a == b {
		t.Fatal("带 query 与不带 query 的签名必须不同")
	}
}
