package server

// 代理 URL 的校验。
//
// # 为什么这个文件存在
//
// 它原先在 traffic 包（traffic.ValidateProxyURL）。流量捕获功能删了
// （2026-10-29），但这个校验仍是「设置全局出网代理」的必要一环 ——
// 它拦的是一批拼错的 URL，与是否录像无关。
//
// 判据同 query_params.go：helper 有保留的引用方 → 搬出来，不随包删。

import (
	"fmt"
	"net/url"
	"strings"
)

// ValidateProxyURL 校验并解析出网代理地址（http/https/socks5，可选 user:pass）。
//
// 空串合法吗？不 —— 调用方（SetGlobalProxy）在空串时直接走「清空代理」分支，
// 不调用本函数。所以这里对空串会返回「缺少协议」错误。
func ValidateProxyURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("解析代理地址 %q: %w", raw, err)
	}
	switch u.Scheme {
	case "http", "https", "socks5":
	case "":
		return nil, fmt.Errorf("代理 %q 缺少协议(用 http://、https:// 或 socks5://)", raw)
	default:
		return nil, fmt.Errorf("不支持的代理协议 %q(用 http、https 或 socks5)", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("代理 %q 缺少主机地址", raw)
	}
	return u, nil
}
