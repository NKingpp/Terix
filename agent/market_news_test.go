package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 用假 sidecar 验证三条关键语义：正常解析、无数据映射、市场非法。
// 真 sidecar 的集成验证靠 curl（见交付报告），这里只锁契约。
func TestFetchNewsAndFundamental(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/news":
			if r.URL.Query().Get("symbol") == "NONE" {
				w.Write([]byte(`{"ok":true,"data":{"matched":0,"items":[]}}`))
				return
			}
			w.Write([]byte(`{"ok":true,"data":{"matched":1,"items":[` +
				`{"title":"t","summary":"s","url":"u","source":"caixin","published":"2026-10-30"}]}}`))
		case "/fundamental":
			if r.URL.Query().Get("market") == "us" {
				w.WriteHeader(200)
				w.Write([]byte(`{"ok":false,"error":{"code":"AKSHARE_NO_DATA","message":"待接入"}}`))
				return
			}
			w.Write([]byte(`{"ok":true,"data":{"symbol":"600519","market":"cn",` +
				`"report_type":"abstract","currency":"CNY","periods":["2026-09-30"],` +
				`"columns":["指标","2026-09-30"],"rows":[{"指标":"净利润","2026-09-30":"1.0"}]}}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c := NewAkshareClient(srv.Listener.Addr().String())
	ctx := context.Background()

	items, err := c.FetchNews(ctx, "", "cn", 5)
	if err != nil || len(items) != 1 || items[0].Source != "caixin" {
		t.Fatalf("FetchNews 正常路径失败: %v %+v", err, items)
	}

	// 空结果是 NEWS_NO_DATA，而不是 nil,nil —— 上层要靠它区分「没新闻」与「源故障」
	if _, err := c.FetchNews(ctx, "NONE", "us", 5); err == nil || !IsNoDataError(err) {
		t.Fatalf("空新闻应返回 NO_DATA，实际: %v", err)
	}

	f, err := c.FetchFundamental(ctx, "600519", "cn", "abstract")
	if err != nil || f.Symbol != "600519" || len(f.Rows) != 1 {
		t.Fatalf("FetchFundamental 正常路径失败: %v %+v", err, f)
	}

	// 美股财报 = 能力缺口，必须可识别为 no-data（不重试、不告警）
	if _, err := c.FetchFundamental(ctx, "MSFT", "us", "abstract"); err == nil || !IsNoDataError(err) {
		t.Fatalf("美股财报应返回 NO_DATA，实际: %v", err)
	}

	if _, err := c.FetchNews(ctx, "", "jp", 5); err == nil {
		t.Fatal("非法 market 应报错")
	}
}
