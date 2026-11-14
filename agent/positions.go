package agent

import "context"

// Position 是一笔当前持仓（只读视图）。
//
// # 为什么这个类型在 agent 包而不是 exec 包
//
// agent 不得 import exec（scripts/check-vaultd-isolation.sh 第 3 组断言：
// 决策进程不能碰执行层）。持仓读取由 server 侧实现，通过 ctx 注入 ——
// 与 WithMarketClient / WithExecAccount 同一套模式。
type Position struct {
	InstID  string `json:"instId"`
	PosSide string `json:"posSide"`
	Pos     string `json:"pos"`   // 净数量，负数 = 空头
	AvgPx   string `json:"avgPx"`
	Upl     string `json:"upl"`
	Lever   string `json:"lever"`
	MgnMode string `json:"mgnMode"`
}

// PositionsProvider 提供当前持仓快照。
//
// 实现方（server）负责超时与缓存 —— 每个 intent 都打一次交易所会撞限频。
type PositionsProvider interface {
	Positions(ctx context.Context) ([]Position, error)
}

type positionsKey struct{}

// WithPositionsProvider 把持仓来源注入 ctx。
func WithPositionsProvider(ctx context.Context, p PositionsProvider) context.Context {
	if p == nil {
		return ctx
	}
	return context.WithValue(ctx, positionsKey{}, p)
}

// PositionsFrom 取出 ctx 里的持仓来源。
func PositionsFrom(ctx context.Context) (PositionsProvider, bool) {
	p, ok := ctx.Value(positionsKey{}).(PositionsProvider)
	return p, ok
}
