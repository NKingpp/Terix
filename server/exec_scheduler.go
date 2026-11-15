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
	auto *exec.AutoTrader
	// fills 把成交明细同步进 trade_fills（可能为 nil）。
	//
	// 与执行模式无关：切回 paper 之后仍要能看历史成交与盈亏，
	// 所以它不在 runAuto 的模式闸之后，而是在 step 开头就跑。
	fills    *exec.FillsSyncer
	interval time.Duration
}

// newExecTicker 构造执行层调度。
//
// querier 为 nil 时只跑超时扫描（对账跳过）。
func newExecTicker(pg *db.DB, querier exec.ChannelQuerier, auto *exec.AutoTrader, fills *exec.FillsSyncer) *execTicker {
	t := &execTicker{
		pg:       pg,
		timeout:  exec.NewTimeoutScanner(pg, 0), // 0 = 用默认 30min
		interval: execTickInterval,
		auto:     auto,
		fills:    fills,
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
	// 账户列表本轮只查一次（对账与成交同步共用）。
	accts, acctsErr := t.pg.ExecAccounts().ListAccounts()
	if acctsErr != nil {
		slog.Error("执行层：列账户失败", "err", acctsErr)
	}

	// 成交明细同步：放在最前面且不受模式限制 —— 交易记录是「已经发生过
	// 什么」的账，不因为当前切到 paper 就该停止记录。
	//
	// 停用/冻结账户也照同步：冻结是因为对账失败，那时更需要历史成交来核对。
	if t.fills != nil && acctsErr == nil {
		for _, a := range accts {
			n, sErr := t.fills.SyncAccount(ctx, a.ID)
			if sErr != nil {
				slog.Warn("成交同步部分失败", "account_id", a.ID, "inserted", n, "err", sErr)
				continue
			}
			if n > 0 {
				slog.Info("成交同步完成", "account_id", a.ID, "inserted", n)
			}
		}
	}

	if t.reconciler != nil {
		// 对账按账户跑。
		if acctsErr != nil {
			slog.Error("对账：列账户失败", "err", acctsErr)
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
	// ⚠️ 2026-11-14 起**草案不再有执行者**，本函数只保留状态巡检职责。
	//
	// # 为什么（这是架构变更的收尾，不是临时关掉）
	//
	// 旧设计有两条执行路径：TradeLoop 消费 live_auto 草案，runAuto 消费
	// paper 草案。TradeLoop 已下线（模型改用 open_position / close_position
	// 同步下单），于是草案在 live 模式下**永远没人执行** —— 而模型还按旧
	// prompt 以为「过闸就会自动下发」，报告「已完成」而实际什么都没发生。
	//
	// 留着 paper 这一半更糟：它让 paper 与 live 走不同的路径、不同的账本。
	// 实测任务 30 的纸面单就是这条路产生的 —— 交易记录页里 paper_orders
	// 有 1 笔，而 trade_records.action 是 0 条，盈亏归属对不上。
	//
	// 所以两条都停：**唯一的执行路径是工具**。纸面单也由 open_position 写，
	// 这样两种模式共用同一套台账与盈亏算法。
	//
	// 想做「保留旧草案自动成交」的话，正确做法是给 ExecutePaper 补写
	// trade_records，而不是恢复这条分支 —— 但那是多余的第二条路。
	_ = mode
	return
}

// maxAutoBatch 是单轮自动执行的上限。
//
// 20 单/分钟远低于任何券商限流阈值，同时能在积压时几轮内消化完。
// 更重要：批次上限让「一次故障最多影响 20 单」，而不是全库草案。
const maxAutoBatch = 20

// firstActiveAccount 返回第一个可用于自动执行的账户。
func (t *execTicker) firstActiveAccount() (*db.ExecAccount, error) {
	accts, err := t.pg.ExecAccounts().ListAccounts()
	if err != nil {
		return nil, err
	}
	for _, a := range accts {
		if !a.Active || a.BlockedAt != nil {
			continue
		}
		if strings.HasPrefix(a.ChannelAcctID, paperAutoAccountRef) {
			continue // 模拟账户不用于真实下单
		}
		return a, nil
	}
	return nil, errors.New("没有 active 且未冻结的执行账户")
}

// paperAutoAccountRef 是历史上的模拟账户标识，仅用于排掉遗留行。
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
// okxTradeCred 读当前可用的 OKX 交易凭据（下单 / 对账 / 持仓都从这里取）。
//
// 每次调用都重读表：用户在凭证页换 key 之后必须立即生效 ——
// 缓存住旧 key 的后果是所有下单返回 50119「key 不存在」，
// 而页面显示的是新 hint，最难查的那类「配置不生效」。
func (s *Server) okxTradeCred() (*db.OkxTradeCred, error) {
	if s.m == nil || s.m.pg == nil {
		return nil, db.ErrCredentialNotFound
	}
	return s.m.pg.ActiveOkxTradeCredential()
}

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
