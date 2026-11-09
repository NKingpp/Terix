package server

// findings 列表查询的共用解析。
//
// # 为什么这个文件存在
//
// 这两个函数原先定义在 server/findings_groups.go（渗透分组功能）里，
// 但它们的消费者是 GET /api/exploration/findings（保留的路由，
// dashboard 和 task overview 都在调）。
//
// 判据同 query_params.go：一个 helper 若有「不在删除清单里」的引用方，
// 就要搬出来，不能随定义它的文件一起删。

import (
	"net/url"
	"strings"

	"github.com/Autumn-27/artex/db"
)

// findingPaginationParam 解析分页参数，带默认值与上限。
func findingPaginationParam(raw string, fallback, upperBound int) int {
	value := atoiDefault(raw, fallback)
	if value <= 0 {
		value = fallback
	}
	if upperBound > 0 && value > upperBound {
		value = upperBound
	}
	return value
}

// findingFilterFromQuery builds the shared findings filter from a request's
// query string. 新增筛选项只改这里。
func findingFilterFromQuery(q url.Values) db.FindingFilter {
	return db.FindingFilter{
		Severity:  normFilter(q.Get("severity")),
		Status:    normFilter(q.Get("status")),
		VulnClass: normFilter(q.Get("vulnclass")),
		// task_id(独立于会切到「按任务节点」分支的 task 参数):全局表按任务筛选。
		TaskID:     normFilter(q.Get("task_id")),
		Query:      q.Get("q"),
		Sort:       q.Get("sort"),
		AssetScope: strings.TrimSpace(q.Get("asset_scope")),
	}
}
