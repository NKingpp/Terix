package agent

import (
	"context"

	"github.com/Autumn-27/artex/db"
)

// RunInfo identifies WHICH run a tool call belongs to. Tool assembly only receives
// (ctx, agentKey) — the task/exploration ids live in the caller's arguments, not the
// ctx — so anything wired at assembly time (currently the Skill ledger in
// server/assembly.go) has no way to attribute a call to a task. Each run attaches
// its own RunInfo before calling AugmentTools; the wiring closure reads it once and
// captures it, so per-run attribution stays correct without threading parameters
// through the tool layer. Same pattern as TaskClock (see taskclock.go).
//
// Zero value = attribution unknown; every consumer must treat it as optional.
type RunInfo struct {
	TaskID        int64  // task registry id; 0 for non-task runs (chat sessions)
	ExplorationID int64  // exploration id; 0 when unknown
	IntentID      int64  // worker's intent node; 0 for planner/mainagent/chat
	SessionID     string // chat conversation id; empty for task runs
}

// explorationID reads a store's exploration id, tolerating a nil store (planner and
// worker runs can be driven without one in tests).
func explorationID(ts *db.ExplorationStore) int64 {
	if ts == nil {
		return 0
	}
	return ts.ID()
}

type runInfoKey struct{}

// WithRunInfo attaches run attribution to ctx.
func WithRunInfo(ctx context.Context, ri RunInfo) context.Context {
	return context.WithValue(ctx, runInfoKey{}, ri)
}

// RunInfoFrom reads the RunInfo (zero value if none attached).
func RunInfoFrom(ctx context.Context) RunInfo {
	if v, ok := ctx.Value(runInfoKey{}).(RunInfo); ok {
		return v
	}
	return RunInfo{}
}

// ── 市场数据客户端（ctx 传递）───────────────────────────────────────

type marketClientKey struct{}

// WithMarketClient 把市场数据客户端放进 ctx。
//
// # 为什么用 ctx 而不是 Worker 字段
//
//   - 不改 NewWorker 签名与全部构造点
//   - 与 WithRunInfo / RunInfoFrom 是同一模式（已有先例）
//   - 天然按「一次执行」作用域（ctx 随 run 结束失效，不会跨任务泄漏）
//
// # 为什么 vars 构造器必须自己取数（而不是让 LLM 调工具）
//
// 如果 .Bars 由 vars 构造器取、而 snapshot_id 由 LLM 调 get_kline 拿到，
// 两者会是**两个不同来源** —— 可能取到同一份数据却产生两个 snapshot，
// 或者 vars 里的 Bars 与工具返回的 bars 不一致。那么 emit_fact 的
// snapshot_ids 就指向了与它引用的数据不匹配的快照。
//
// 所以取数和拿 snapshot_id 必须**在同一次调用里完成**：
// fetchBars 内部调 MarketClient + captureKlineSnapshot，一次拿到数据与 id。
func WithMarketClient(ctx context.Context, c MarketClient) context.Context {
	if c == nil {
		return ctx // 注入 nil 等于不注入，避免调用方误以为「注入了但为空」
	}
	return context.WithValue(ctx, marketClientKey{}, c)
}

// MarketClientFrom 取回市场数据客户端。
//
// 返回 (client, ok) 而不是只返回 client：调用方需要区分
// 「没注入」（ok=false，走 stub 分支）与「注入了但取数失败」（ok=true，err 非空）。
// 只返回 nil 会让这两种情况无法区分。
func MarketClientFrom(ctx context.Context) (MarketClient, bool) {
	c, ok := ctx.Value(marketClientKey{}).(MarketClient)
	if !ok || c == nil {
		return nil, false
	}
	return c, true
}

type explorationStoreKey struct{}

// WithExplorationStore 把 ExplorationStore 放进 ctx。
//
// 用途：vars 构造器需要落 snapshot（captureKlineSnapshot 要 *db.DB），
// 而 execute 的签名里 ts 是参数不是 ctx —— 变量构造器拿不到它。
// 走 ctx 是零签名改动的做法（与 WithRunInfo / WithMarketClient 同一模式）。
func WithExplorationStore(ctx context.Context, ts *db.ExplorationStore) context.Context {
	if ts == nil {
		return ctx
	}
	return context.WithValue(ctx, explorationStoreKey{}, ts)
}

// ExplorationStoreFrom 取回 ExplorationStore（ok=false 表示未注入）。
func ExplorationStoreFrom(ctx context.Context) (*db.ExplorationStore, bool) {
	ts, ok := ctx.Value(explorationStoreKey{}).(*db.ExplorationStore)
	if !ok || ts == nil {
		return nil, false
	}
	return ts, true
}

// ---------------------------------------------------------------------
// 执行账户（交易员用）
// ---------------------------------------------------------------------

type execAccountKey struct{}

// ExecAccount 是一次执行可用的下单账户。
type ExecAccount struct {
	ID  int64  // exec_accounts.id —— authorize_order / execute_paper_order 需要
	Ref string // 可读标识（如 execdemo-account），仅用于提示文案
	// MinOrderAmount 是该通道的最小下单金额（计价币）。0 = 无限制。
	MinOrderAmount float64
}

// WithExecAccount 把下单账户放进 ctx。
//
// # 为什么用 ctx 而不是加进 TraderVars 的构造参数
//
// buildTraderVars 的签名是 (ctx, intent) —— 事件驱动、无额外参数。
// 加参数要改 5 个调用点（含测试），而账户是「本次执行的装配决定」，
// 与 WithMarketClient / WithExplorationStore 同一性质。
//
// # 为什么不给 agent 一个「列账户」工具
//
// 那会让交易员在运行时猜「该用哪个账户」。账户是**执行配置**，
// 由 server 装配时决定（默认账户），不该让模型自选 —— 选错就是打错账户。
func WithExecAccount(ctx context.Context, acct ExecAccount) context.Context {
	if acct.ID <= 0 {
		return ctx // 无效账户等于不注入
	}
	return context.WithValue(ctx, execAccountKey{}, acct)
}

// ExecAccountFrom 取回下单账户。
//
// 返回 (账户, ok)。ok=false 表示未配置 —— trader 应当只产信号、不建草案，
// 而不是猜一个 account_id。
func ExecAccountFrom(ctx context.Context) (ExecAccount, bool) {
	a, ok := ctx.Value(execAccountKey{}).(ExecAccount)
	if !ok || a.ID <= 0 {
		return ExecAccount{}, false
	}
	return a, true
}
