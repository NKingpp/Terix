package agent

// 跨包测试钩子。
//
// # 为什么这些导出在生产代码里（而不是 _test.go）
//
// server 包的测试需要断言 agent 包内的 prompt 分叉逻辑（emitToolFor /
// renderIntentTask / constraintTailFor）。Go 的 `_test.go` 里的导出**只能被
// 同包的测试使用**，跨包不可见。
//
// 替代方案都更差：
//   - 把逻辑复制到 server 包 → 测的是复制品，不是真代码
//   - 改成导出 API → 污染生产命名空间（但这些函数本来就没有外部调用方）
//
// 所以这里是**有意的折中**：导出 + 名字带 ForTest 后缀 + 注释说明用途。

import "github.com/Autumn-27/artex/db"

// EmitToolForTest 导出 emitToolFor，供 server 包的一致性测试断言。
func EmitToolForTest(agentKey string) string { return emitToolFor(agentKey) }

// RenderIntentTaskForTest 用给定 role 与 intent id 渲染意图块。
func RenderIntentTaskForTest(agentKey string, intentID int64) string {
	return renderIntentTask(agentKey, &db.Node{ID: intentID, Payload: []byte(`{"summary":"测试意图"}`)})
}

// ConstraintBlockTailForTest 导出约束块的结尾段。
func ConstraintBlockTailForTest(agentKey string) string { return constraintTailFor(agentKey) }
