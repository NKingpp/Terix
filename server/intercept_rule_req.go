package server

// 拦截规则 kind/pattern 的校验。
//
// # 为什么这个文件存在
//
// 这段逻辑原先在 server/asset_intercept.go（全局资产拦截规则的 CRUD，
// 渗透功能，已删）。但 server/task_intercept.go 的「任务级拦截规则」
// 复用了它做参数归一化 —— 而任务级规则是 GET/POST /api/tasks/{id}/intercept-rules
// 的服务对象，前端 task overview 仍在调用。
//
// 判据同 query_params.go / findings_query.go：helper 有保留的引用方 → 搬出来。
//
// # 为什么搬的时候改成了「两个参数」而不是搬整个 struct
//
// 原实现取 *assetInterceptRuleReq，但只读 Kind 和 Pattern。见函数注释。

import (
	"fmt"
	"net"
	"strings"
)

// validateInterceptRule 校验 kind + pattern。
//
// # 为什么只取两个值而不是整个 request struct
//
// 原来有两套 request 类型（资产级 assetInterceptRuleReq / 任务级 taskInterceptRuleReq），
// 字段 4/5 相同，只差 Action。而校验逻辑只用到 kind + pattern ——
// 原实现从头到尾没读过 Enabled 和 Note。
//
// 旧做法是「造一个 assetInterceptRuleReq 把字段对上去，校验，再把 trim 后的
// Pattern 拷回来」—— 那个中间 struct 的存在只为了「字段对得上」，没有语义。
//
// 而它有个隐患：如果将来给 assetInterceptRuleReq 加字段，task_intercept.go 那处
// 构造不会传它 —— 编译器不报错（字段有零值），于是静默丢值。
//
// 所以改成直接收 kind + pattern：签名即契约，加字段也不会静默丢。
func validateInterceptRule(kind string, pattern *string) error {
	*pattern = strings.TrimSpace(*pattern)
	if *pattern == "" {
		return fmt.Errorf("pattern 不能为空")
	}
	switch kind {
	case "exact_domain", "exact_url", "fuzzy_domain", "fuzzy_ip", "fuzzy_url":
		// free-form, no format check
	case "exact_ip":
		if net.ParseIP(*pattern) == nil {
			return fmt.Errorf("exact_ip 不是有效 IP 地址：%s", *pattern)
		}
	case "cidr":
		if _, _, err := net.ParseCIDR(*pattern); err != nil {
			return fmt.Errorf("cidr 不是有效网段（形如 192.168.0.0/16）：%s", *pattern)
		}
	default:
		return fmt.Errorf("kind 无效：%s", kind)
	}
	return nil
}
