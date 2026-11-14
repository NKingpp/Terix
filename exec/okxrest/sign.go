package okxrest

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"time"
)

// okxTimeFormat 是 OKX 要求的毫秒精度 ISO8601。
//
// 用固定布局而不是 RFC3339：RFC3339 会省略末尾零，OKX 会以
// 50102（timestamp expired）拒绝 —— 这类错误看起来像时钟问题，实则格式问题。
const okxTimeFormat = "2006-01-02T15:04:05.000Z"

// sign 计算 OKX 的请求签名。
//
// prehash = timestamp + METHOD + requestPath + body，HMAC-SHA256 后 base64。
// requestPath 必须**包含 query string** —— 漏掉它会让带参数的 GET 全部 401，
// 而错误文案是「Invalid Sign」，看不出是漏了 query。
func sign(secret, ts, method, path, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + method + path + body))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// setAuthHeaders 写四个鉴权头 + demo 头。
//
// 公开行情接口（/api/v5/public/*）不签名：每多一个收到凭证的端点，
// 就多一个可能记录它的服务。
//
// # x-simulated-trading 必须显式设置（2026-11-14 实测踩过）
//
// 模拟盘凭据走的是**同一个域名**，靠这个头区分环境。漏掉它时 OKX 返回
// 50101「APIKey does not match current environment」—— 文案像是 key 无效，
// 实际是没告诉对方「我在模拟盘」。结果是模拟盘根本没法用这个客户端下单，
// 而错误信息会把人引去查 key。
func setAuthHeaders(req *http.Request, c Credentials, body []byte) {
	if isPublicPath(req.URL.Path) {
		return
	}
	ts := time.Now().UTC().Format(okxTimeFormat)
	req.Header.Set("OK-ACCESS-KEY", c.APIKey)
	req.Header.Set("OK-ACCESS-SIGN", sign(c.SecretKey, ts, req.Method, req.URL.RequestURI(), string(body)))
	req.Header.Set("OK-ACCESS-TIMESTAMP", ts)
	req.Header.Set("OK-ACCESS-PASSPHRASE", c.Passphrase)
	if c.Demo {
		req.Header.Set("x-simulated-trading", "1")
	}
}

func isPublicPath(p string) bool {
	const prefix = "/api/v5/public/"
	return len(p) >= len(prefix) && p[:len(prefix)] == prefix
}
