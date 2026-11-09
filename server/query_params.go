package server

// 从 HTTP 请求读查询参数的通用 helper。
//
// # 为什么这个文件存在
//
// 这些函数原先定义在 server/sync_scopesentry.go（渗透功能）里，
// 但它们的消费者是通用的 —— 删那个文件时不能连带删掉。
//
// 判据：grep 一个 helper 的引用方时，若引用方里有「不在删除清单里」的文件，
// 那个 helper 就要搬出来，而不是随定义它的文件一起删。

import (
	"net/http"
	"strconv"
)

// queryInt reads an int query param with a default fallback.
func queryInt(r *http.Request, key string, def int) int {
	if v := r.URL.Query().Get(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}
