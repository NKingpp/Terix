package server

// okxTrader 是交易工具实际用到的那部分 OKX 客户端能力。
//
// # 为什么要抽接口（2026-11-14）
//
// 加仓/反手的核心正确性在「撤旧 OCO → 等成交 → 重读持仓 → 挂新 OCO」
// 这个状态机里，它的价值几乎全在**失败分支**上：
//
//	撤旧单失败 → 两组 OCO 并存，可能按旧 TP 平掉全部仓
//	挂新单失败 → 该仓裸奔
//	成交确认超时 → 按未知数量挂保护
//
// 这些分支如果不可测，就只能上实盘验证「撤单失败时仓是不是真的裸的」——
// 那是所有验证方式里最贵的一种（要真钱、真行情、还要恰好赶上故障）。
//
// 直接依赖 *okxrest.Client 时，这些分支只能靠手工造故障。
// 抽成接口后，测试里用一个函数字段打桩的 fake 就能逐条钉死调用顺序
// 与失败后的 protection 状态。
//
// # 为什么用这种「窄接口」而不是 mock 整个 Client
//
// 接口只列交易工具真正调用的 9 个方法。*okxrest.Client 天然满足
// （方法签名现成），所以生产路径零改动 —— 不需要包装、不需要适配器。

import (
	"context"

	"github.com/Autumn-27/artex/exec/okxrest"
)

type okxTrader interface {
	GetPositions(ctx context.Context, instType string) ([]okxrest.Position, error)
	PlaceOrder(ctx context.Context, req okxrest.OrderRequest) (*okxrest.OrderAck, error)
	QueryOrderByClOrdID(ctx context.Context, instID, clOrdID string) (*okxrest.OrderState, error)
	ListAlgoOrders(ctx context.Context, instType, ordType string) ([]okxrest.AlgoOrder, error)
	PlaceAlgoOCO(ctx context.Context, req okxrest.AlgoRequest) (string, error)
	CancelAlgo(ctx context.Context, instID, algoID string) error
	AmendAlgoOCO(ctx context.Context, instID, algoID, tp, sl string) error
	SetLeverage(ctx context.Context, instID, lever, mgnMode string) error
	LastPrice(ctx context.Context, instID string) (string, error)
}

// 编译期锚点：*okxrest.Client 必须满足 okxTrader。
//
// 放在这里而不是调用点，是为了「Client 改了方法签名」能立刻在编译期暴露，
// 而不是等到某个工具运行时才 panic。
var _ okxTrader = (*okxrest.Client)(nil)
