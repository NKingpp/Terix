package server

// 执行层调度（WE-11）：每 60s 驱动一次对账与超时扫描。
//
// # 为什么独立于 Scheduler（5s tick）
//
// 现有 `Scheduler` 的 tick 是 5 秒，服务的是「触发器的响应速度」——
// 用户建了触发器，希望几秒内生效。
//
// 而执行层的节奏完全不同：
//   - 对账要查通道（有速率限制，且通道回执本身是分钟级）
//   - 超时阈值是 30 分钟，60s 一次的精度绰绰有余
//
// 把两者塞进一个 ticker 会迫使其中一方妥协：要么触发器变慢，要么通道被打爆。
// 所以独立一个 60s ticker。
//
// # 单实例假设（与 exec.Reconciler / exec.TimeoutScanner 一致）
//
// 多实例部署时，每个实例都会跑这个 tick —— 同一单被对账多次。
// 后果：重复写 exec_reconciliations（条件更新让状态推进幂等，但记录会重复）。
//
// 升级路径：pg_try_advisory_lock(<固定 key>) 选主，锁随连接释放。
//
// # 为什么对账可以「没有 querier 就不跑」，而超时扫描不依赖通道
//
// 对账的本质是「拿通道的事实修正本地」—— 没有通道就无从对起。
// 超时扫描只按本地时间判断「等太久了」，不需要通道。
//
// 所以渠道未接入时：对账跳过（log 一次说明原因），超时扫描照常跑。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/exec"
)

// execTickInterval 是执行层调度的周期。
//
// 60s：对账要查通道（有速率限制），且超时阈值是 30 分钟 ——
// 60s 的精度远超需要。再快只会浪费通道配额。
const execTickInterval = 60 * time.Second

// execTicker 驱动对账与超时扫描。
type execTicker struct {
	pg         *db.DB
	reconciler *exec.Reconciler // 可能为 nil（渠道未接入）
	timeout    *exec.TimeoutScanner
	// auto 是自动交易编排器（paper 模拟成交 / live_auto 实盘下发）。
	//
	// 装了它也不代表会下单 —— 每轮都会先检查执行模式是否为 paper 或
	// live_auto，其它模式直接跳过。「装配」与「启用」仍是两件事。
	auto     *exec.AutoTrader
	interval time.Duration
}

// newExecTicker 构造执行层调度。
//
// querier 为 nil 时只跑超时扫描（对账跳过）。
func newExecTicker(pg *db.DB, querier exec.ChannelQuerier, auto *exec.AutoTrader) *execTicker {
	t := &execTicker{
		pg:       pg,
		timeout:  exec.NewTimeoutScanner(pg, 0), // 0 = 用默认 30min
		interval: execTickInterval,
		auto:     auto,
	}
	if querier != nil {
		t.reconciler = exec.NewReconciler(pg, querier)
	}
	return t
}

// Run 循环直到 ctx 取消。
//
// 启动时打一行日志说明单实例假设 ——
// **每次都打（不是首次）**：每次启动都是一次「重新确认部署形态」的机会。
func (t *execTicker) Run(ctx context.Context) {
	if t.pg == nil {
		return
	}
	slog.Info("执行层调度启动",
		"interval", t.interval.String(),
		"reconcile_enabled", t.reconciler != nil,
		"单实例假设", "多实例部署需加 pg advisory lock 选主，否则同一单会被对账多次")

	tk := time.NewTicker(t.interval)
	defer tk.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			t.step(ctx)
		}
	}
}

// step 跑一轮：先对账，再超时扫描。
//
// # 为什么对账在前
//
// 对账可能把单推进到 filled（事实），而超时只是因为「等太久了」。
// 先对账能让超时扫描看到最新的状态 —— 已成交的单就不会被误判超时。
//
// 但注意：这不是**正确性**的保证（两个 tick 之间仍可能冲突），
// 只是「减少无谓的抢跑」。正确性由超时扫描的条件 UPDATE 保证
// （`WHERE state=$from`），见 exec/timeout.go 的说明。
func (t *execTicker) step(ctx context.Context) {
	if t.reconciler != nil {
		// 对账按账户跑。当前只有「列出所有账户」这一种方式。
		accts, err := t.pg.ExecAccounts().ListAccounts()
		if err != nil {
			slog.Error("对账：列账户失败", "err", err)
		} else {
			for _, a := range accts {
				if !a.Active || a.BlockedAt != nil {
					// 停用/冻结的账户不必对账 —— 它们不会再有新单。
					continue
				}
				res, rErr := t.reconciler.Run(ctx, a.ID)
				if t.auto != nil {
					// 把对账结果告诉自动交易器 —— 它是降级判据之一。
					//
					// 判据是「连续 N 次失败」而不是单次：券商 API 偶发超时是常态，
					// 单次失败就降级会让自动交易几乎跑不起来。
					t.auto.NoteReconcileResult(rErr == nil)
				}
				if rErr != nil {
					slog.Error("对账失败", "account_id", a.ID, "err", rErr)
					continue
				}
				// 日志分级：全一致用 Debug（不刷屏），有差异用 Info。
				if res.Checked > 0 && res.NoDiff == res.Checked {
					slog.Debug("对账：全部一致", "account_id", a.ID, "checked", res.Checked)
				} else if res.Checked > 0 {
					slog.Info("对账发现差异",
						"account_id", a.ID,
						"checked", res.Checked, "no_diff", res.NoDiff,
						"state_only", res.StateOnly, "amount", res.Amount,
						"missing_remote", res.MissingRem,
						"query_fail", res.QueryFail, "frozen", res.Frozen)
				}
			}
		}
	}

	res, err := t.timeout.Run(ctx)
	if err != nil {
		slog.Error("超时扫描失败", "err", err)
		return
	}
	if res.TimedOut > 0 || res.Skipped > 0 {
		slog.Info("超时扫描完成",
			"scanned", res.Scanned, "timed_out", res.TimedOut,
			"skipped", res.Skipped, "by_state", res.ByState)
	} else if res.Scanned > 0 {
		// 扫了但都没超时 —— Debug 级，避免每 60s 一条 Info。
		slog.Debug("超时扫描：无超时单", "scanned", res.Scanned)
	}

	// 自动交易放在最后：它依赖对账/超时先跑完（先修正本地状态，再决定下什么单）。
	t.runAuto(ctx)
}

// runAuto 跑一轮自动交易：找出待执行的草案，逐条过闸下单。
//
// # 为什么每轮都先查模式
//
// 模式可能在任何时刻被降级（对账失败 / 人工切换）。缓存模式值会让
// 「已经降级了但还在下单」发生 —— 那是自动交易最危险的 bug。
//
// # 为什么逐条串行而不是并发
//
// ① 限额是共享的：并发下单会让「当日已用额度」判断互相踩踏，
//
//	可能整体超限；
//
// ② 券商通道几乎都有速率限制，并发只会被限流；
// ③ 概览页规模下（个位数待执行单）串行完全够快。
func (t *execTicker) runAuto(ctx context.Context) {
	if t.auto == nil {
		return
	}
	mode, err := t.pg.GetExecMode()
	if err != nil {
		slog.Error("自动交易：读执行模式失败", "err", err)
		return
	}
	// paper = 模拟盘自动成交（只落纸面账本），live_auto = 实盘自动下发。
	// 其余模式一律不碰 —— 特别是 live_manual（要人工确认，不能被自动流程绕过）。
	if mode != db.ExecModePaper && mode != db.ExecModeLiveAuto {
		return
	}

	// 找「已判定 pass、还没导出、还没执行」的草案。
	//
	// 口径与仪表盘的「待决策草案」不同：那个数的是「还没拍板的」（含未判定），
	// 这里要的是「已判定可执行但还没发出去的」。两者互补，不能混用。
	drafts, err := t.pg.OrderDrafts().ListAutoExecutable(maxAutoBatch)
	if err != nil {
		slog.Error("自动交易：列待执行草案失败", "err", err)
		return
	}
	if len(drafts) == 0 {
		return
	}

	// 用哪个账户下单？
	//
	// 当前取「第一个活跃且未冻结的账户」。多账户场景需要草案显式指定
	// 或按市场路由 —— 那是后续工作，这里先保证单账户可用且行为可预测。
	acct, err := t.firstActiveAccount()
	if err != nil {
		slog.Error("自动交易：找不到可用账户", "err", err)
		return
	}

	slog.Info("自动交易：开始执行", "drafts", len(drafts), "account_id", acct.ID)

	for _, d := range drafts {
		out, err := t.auto.Execute(ctx, d.ID, acct.ID)
		if err != nil {
			slog.Error("自动交易：执行失败",
				"draft_id", d.ID, "err", err)
			// 降级错误不继续跑后面的单 —— 状态已经不可信。
			if errors.Is(err, exec.ErrAutoDegraded) ||
				errors.Is(err, exec.ErrAutoNotAuthorized) {
				slog.Error("自动交易：已停止本轮", "reason", err)
				return
			}
			continue
		}
		if out == nil {
			continue
		}
		if out.Degraded {
			// 已降级 → 本轮不再继续（后续单的限额判断也不可信了）。
			slog.Error("自动交易：因降级停止本轮", "reason", out.DegradeWhy)
			return
		}
		slog.Info("自动交易：草案已处理",
			"draft_id", out.DraftID,
			"decision", out.Decision,
			"channel_order_id", out.ChannelID,
			"client_order_id", out.ClientOrder,
			"reason", out.Reason)
	}
}

// maxAutoBatch 是单轮自动执行的上限。
//
// 20 单/分钟远低于任何券商限流阈值，同时能在积压时几轮内消化完。
// 更重要：批次上限让「一次故障最多影响 20 单」，而不是全库草案。
const maxAutoBatch = 20

// firstActiveAccount 返回第一个可用于自动执行的账户。
//
// paper 模式下若一个账户都没有，自动建一个仅用于纸面账本的账户 ——
// 否则「模拟盘自动成交」会因拿不到 account_id 直接跑不起来（Gate 与
// ExecutePaper 都要求账户可用）。该模拟账户在实盘自动模式下会被排除。
func (t *execTicker) firstActiveAccount() (*db.ExecAccount, error) {
	mode, err := t.pg.GetExecMode()
	if err != nil {
		return nil, fmt.Errorf("读执行模式: %w", err)
	}
	accts, err := t.pg.ExecAccounts().ListAccounts()
	if err != nil {
		return nil, err
	}
	for _, a := range accts {
		if !a.Active || a.BlockedAt != nil {
			continue
		}
		if mode == db.ExecModeLiveAuto && strings.HasPrefix(a.ChannelAcctID, paperAutoAccountRef) {
			continue // 模拟账户绝不用于真实下单
		}
		return a, nil
	}
	if mode == db.ExecModePaper {
		return t.ensurePaperAccount()
	}
	return nil, errors.New("没有 active 且未冻结的执行账户")
}

// paperAutoAccountRef 是自动创建的模拟账户标识。
const paperAutoAccountRef = "paper-auto"

func (t *execTicker) ensurePaperAccount() (*db.ExecAccount, error) {
	a, err := ensurePaperAutoAccount(t.pg)
	if err != nil {
		return nil, err
	}
	slog.Info("自动交易：模拟盘纸面账户已就绪", "account_id", a.ID)
	return a, nil
}

// ensurePaperAutoAccount 建（或复用）模拟盘纸面账户。
//
// 幂等：唯一键是 (channel, channel_account_id)。两处调用共用它：
//   - Server 装配：给 trader 注入 account_id，否则它不会建草案；
//   - execTicker：paper 自动执行时兜底，保证 Gate/ExecutePaper 有账户。
//
// 实盘自动模式会按 paperAutoAccountRef 前缀排除该账户，绝不用于真实下单。
func ensurePaperAutoAccount(d *db.DB) (*db.ExecAccount, error) {
	a, err := d.ExecAccounts().UpsertAccount(db.ExecAccountInput{
		Channel:       "okx",
		ChannelAcctID: paperAutoAccountRef,
		Label:         "模拟盘自动账户（仅纸面账本）",
		Market:        "crypto",
		MarketSupport: []string{"crypto", "us", "hk", "cn"},
		Active:        true,
	})
	if err != nil {
		return nil, fmt.Errorf("创建模拟盘账户失败: %w", err)
	}
	return a, nil
}

// newAutoTrader 装配实盘自动交易器。
//
// # 通道未接入时返回 nil 而不是返回一个会失败的实例
//
// runAuto 对 nil 直接 return —— 那种情况下连日志都不打，
// 因为「通道未接入」是部署时的既有事实，不是运行时异常。
// 每 60 秒刷一条「通道未接入」只会淹没真正的告警。
//
// # vaultd 的地址与 token 从哪来
//
// vaultd 是**独立进程**（cmd/vaultd），启动时在 stdout 首行打印 token。
// 主进程通过两个环境变量取：
//
//	ARTEX_VAULTD_URL    —— 例 http://127.0.0.1:8792
//	ARTEX_VAULTD_TOKEN  —— vaultd stdout 首行拿到的 token
//
// 不从数据库读：凭证不进 DB 是本项目的既有约束（见 PRD 的硬约束）。
func (s *Server) newAutoTrader() *exec.AutoTrader {
	base := strings.TrimSpace(os.Getenv("ARTEX_VAULTD_URL"))
	token := strings.TrimSpace(os.Getenv("ARTEX_VAULTD_TOKEN"))
	gate := exec.NewGate(s.m.pg, nil)
	if base == "" || token == "" {
		// 通道未接入时**仍然装配**编排器（client=nil）：
		// paper 模式的自动成交不需要真实通道，只写纸面账本。
		// live_auto 会在 Execute 的通道守卫处明确失败，不会静默回落模拟盘。
		if os.Getenv("ARTEX_AUTO_EXPECT_VAULTD") == "1" {
			slog.Error("自动交易：ARTEX_AUTO_EXPECT_VAULTD=1 但 VAULTD 配置缺失",
				"has_url", base != "", "has_token", token != "")
		}
		slog.Info("自动交易：模拟盘自动成交已装配（实盘通道未接入）")
		return exec.NewAutoTrader(s.m.pg, gate, nil)
	}
	client := exec.NewClient(base, token)
	slog.Info("自动交易：通道已装配", "vaultd", base)
	return exec.NewAutoTrader(s.m.pg, gate, client)
}
