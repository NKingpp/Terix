package agent

// 从 exploration_nodes.payload（JSONB）读值的工具函数。
//
// # 为什么需要这一层
//
// PostgreSQL 的 JSONB 列经 `encoding/json` 反序列化到 `map[string]any` 后，
// **所有数字都变成 `float64`** —— 不管写入时是 `int`、`int64` 还是别的整数类型。
//
// 所以 `p["round"].(int)` 会类型断言失败，返回**零值 0**，而且**不报错**。
// 表现是「辩论永远显示第 0 轮」「instrument_id 永远查不到标的」这类静默故障。
//
// C1.9 记录了这个问题。本文件是唯一的转换入口 —— 新增任何从 payload
// 读数字的代码都必须走这里，不要另写一套开关。

import (
	"encoding/json"

	"github.com/Autumn-27/artex/db"
)

// int64FromAny 把 JSON 反序列化出的数字转成 int64。
//
// 处理 5 种类型而不是 3 种：除了 DB 读回的 `float64`，内存中直接构造 payload
// （测试、单元调用）时可能是任意整数类型。穷举的成本是几行 switch，
// 漏一个的成本是「静默归零 + 排查半天」。
func int64FromAny(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case float32:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0
		}
		return i
	}
	return 0
}

// intFromAny 是 int64FromAny 的 int 版本（模板变量多是 int）。
func intFromAny(v any) int {
	return int(int64FromAny(v))
}

// stringFromAny 安全取字符串（非 string 返回空串）。
func stringFromAny(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// stringSliceFromAny 安全取字符串切片（JSON 数组反序列化后是 []any）。
func stringSliceFromAny(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// decodePayload 反序列化节点 payload（失败返回空 map，不返回 nil —— 调用方
// 不用每次都判 nil）。
func decodePayload(intent *db.Node) map[string]any {
	if intent == nil || len(intent.Payload) == 0 {
		return map[string]any{}
	}
	var p map[string]any
	if err := json.Unmarshal(intent.Payload, &p); err != nil {
		return map[string]any{}
	}
	if p == nil {
		return map[string]any{}
	}
	return p
}
