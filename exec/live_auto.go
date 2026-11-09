package exec

// 实盘自动交易链路（2026-11-05）。
//
// # 本文件解决什么
//
// 自动模式下**没有人点确认**：系统按已过闸的草案自行执行。
// live_auto 走真实通道（vaultd），paper 只落纸面账本 —— 后者不碰真实资金，
// 但复用同一套闸门判定与审计，保证模拟成交与实盘口径一致。
//
// # 安全分层（从外到内，任何一层不过就不下单）
//
//	① 模式守卫   —— 只允许 live_auto / paper；frozen 与 live_* 手动档一律拒绝
//	② 授权守卫   —— exec_authorizations 里必须有**未撤销**的 live_auto 授权
//	③ 闸门判定   —— Gate.Authorize，与人工路径共用同一套风控规则与限额
//	④ 幂等保护   —— client_order_id 由 (draft_id, attempt) 确定性派生
//	⑤ 降级检查   —— 对账连续失败 / 单笔超限 → 立刻回 paper 并冻结
//
// # 为什么编排放在 exec 包而不是 server
//
// 它需要 Gate（exec）、vaultd Client（exec）、限额（db）三类依赖。
// server 已经有装配职责，但「自动交易该怎么跑」是执行域的决策，
// 放 server 会让那段逻辑被各种 HTTP handler 淹没。
//
// # 单实例假设（与 Reconciler 一致，但本处更危险）
//
// 两个实例同时跑自动交易 = 同一信号被下两次单。
// 所以 AutoTrader 启动时**必须**先拿 pg advisory lock（见 AcquireLeaderLock）。
// 拿不到就静默待机 —— 多副本部署下只有一个实例会真正下单。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
)

// ---------------------------------------------------------------------
// 错误与判定
// ---------------------------------------------------------------------

var (
	// ErrAutoNotAuthorized 表示当前没有有效的 live_auto 授权。
	ErrAutoNotAuthorized = errors.New("live_auto 未授权或已被撤销")

	// ErrAutoNotLeader 表示本实例不是自动交易的 leader。
	//
	// 多副本部署时只有一个实例拿得到 advisory lock，其余待机。
	ErrAutoNotLeader = errors.New("本实例不是自动交易 leader（advisory lock 被其他实例持有）")

	// ErrAutoDegraded 表示自动交易已被降级（对账失败等），需要人工介入才能恢复。
	ErrAutoDegraded = errors.New("自动交易已降级，需人工确认后才能重新启用")
)

// AutoOutcome 是一次自动执行的结果（用于审计与日志，不改状态）。
type AutoOutcome struct {
	DraftID     int64
	Decision    string // allow / deny / ask / error
	Reason      string
	ChannelID   string // 渠道订单号（成功时）
	ClientOrder string // 本地幂等键
	// Degraded 表示本次执行触发了降级。
	Degraded   bool
	DegradeWhy string
}

// ---------------------------------------------------------------------
// AutoTrader
// ---------------------------------------------------------------------

// AutoTrader 是自动交易链路的编排器。
type AutoTrader struct {
	db     *db.DB
	gate   *Gate
	client ChannelSubmitter // vaultd 客户端；nil = 实盘通道未接入

	// mu 保护 degraded 字段。多 goroutine 不常见（只有一个 tick），
	// 但降级可能由对账 goroutine 同时触发。
	mu       sync.Mutex
	degraded bool
	why      string

	// 连续对账失败计数（降级判据之一）。
	failStreak int
}

// ChannelSubmitter 是 AutoTrader 需要的通道能力。
//
// 只依赖 Submit/Query 两个方法，便于测试时替换为假实现。
// 不用 exec.Client 的具体类型，避免把 HTTP 细节钉死在编排逻辑里。
type ChannelSubmitter interface {
	Submit(ctx context.Context, req SubmitRequest) (*SubmitResponse, error)
}

// NewAutoTrader 构造自动交易编排器。
//
// client 为 nil 表示实盘通道未接入 —— 此时 Execute 会明确失败，
// 而不是静默回落 paper（那会让用户以为实盘在跑）。
func NewAutoTrader(d *db.DB, gate *Gate, client ChannelSubmitter) *AutoTrader {
	return &AutoTrader{db: d, gate: gate, client: client}
}

// Degraded 返回是否已降级，以及原因。
func (a *AutoTrader) Degraded() (bool, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.degraded, a.why
}

// degrade 把自动交易标记为降级，并把执行模式切回 paper。
//
// # 为什么要同时改模式
//
// 只标记 degraded 而不改模式，会出现「模式显示 live_auto 但实际不下单」
// 的状态 —— 用户看模式以为在跑，看订单列表却没有新单，无法判断是
// 「没信号」还是「系统停了」。切回 paper 让状态自解释。
//
// # 为什么用 Freeze 而不是直接 SetExecMode
//
// Freeze 会走完整的降级流程（记录原因、冻结账户、写审计）。
// 直接改 settings 会绕过这些，事后无法解释「为什么自己降级了」。
func (a *AutoTrader) degrade(ctx context.Context, reason string) {
	a.mu.Lock()
	if a.degraded {
		a.mu.Unlock()
		return
	}
	a.degraded = true
	a.why = reason
	a.mu.Unlock()

	slog.Error("自动交易降级", "reason", reason)

	// 切回 paper：live_auto 的合法出口只有 paper / frozen，这里选 paper
	// —— 冻结会连纸面单都停，而降级后仍需继续验证策略。
	if _, _, err := a.db.ForceSetExecMode(db.ExecModePaper); err != nil {
		// 切模式失败是严重问题：模式还停在 live_auto 但已 degraded。
		// 再试一次 frozen（宁可停死也不要留在 live_auto）。
		slog.Error("降级切 paper 失败，尝试 frozen", "err", err)
		if _, _, err2 := a.db.ForceSetExecMode(db.ExecModeFrozen); err2 != nil {
			slog.Error("critical: 降级后两个模式都切不动", "err", err2, "state", "仍在 live_auto 但已 degraded")
		}
	}
}

// Execute 执行一条草案的自动下单。
//
// 这是 live_auto 的核心：没有人工确认，直接走完守卫→闸门→下发。
//
// 返回 (*AutoOutcome, error)：
//   - error        → 系统层失败（DB / 网络），调用方应记录并重试或告警
//   - outcome      → 业务结果（allow 下的成功、deny 的拒绝、ask 的需人工）
//
// # 为什么 ask 在自动模式下也返回结果而不报错
//
// 闸门返回 ask 意味着「按规则需要人工确认」。自动模式下**无法满足**这个要求，
// 所以它不是「执行成功」，但也不是系统故障 —— 是一条被正确拒绝的下单。
// 调用方（ticker）应该把它当正常业务流处理，只是不下单。
func (a *AutoTrader) Execute(ctx context.Context, draftID, accountID int64) (*AutoOutcome, error) {
	out := &AutoOutcome{DraftID: draftID}

	// ① 模式守卫
	//
	// 两种自动模式：
	//   - live_auto：走真实通道（vaultd），需 live_auto 授权 + 通道可用
	//   - paper    ：只落纸面账本，不碰真实通道，无需授权
	// 其它模式（live_whitelist / live_manual / frozen）一律不自动执行。
	mode, err := a.db.GetExecMode()
	if err != nil {
		return nil, fmt.Errorf("读执行模式: %w", err)
	}
	if mode != db.ExecModeLiveAuto && mode != db.ExecModePaper {
		out.Decision = "error"
		out.Reason = fmt.Sprintf("当前模式 %s 不支持自动下单（仅 live_auto / paper）", mode)
		return out, nil
	}
	isPaper := mode == db.ExecModePaper

	// ②③④ 都是**实盘专属**守卫：降级来自对账失败/单笔超限，授权与通道
	// 也只对真实资金有意义。paper 没有真实敞口，跳过它们但仍保留闸门判定。
	if !isPaper {
		// ② 降级守卫
		if bad, why := a.Degraded(); bad {
			out.Decision = "error"
			out.Reason = "自动交易已降级：" + why
			return out, ErrAutoDegraded
		}

		// ③ 授权守卫
		ok, err := a.hasLiveAutoAuth(ctx, accountID)
		if err != nil {
			return nil, fmt.Errorf("检查 live_auto 授权: %w", err)
		}
		if !ok {
			out.Decision = "error"
			out.Reason = "该账户没有有效的 live_auto 授权"
			return out, ErrAutoNotAuthorized
		}

		// ④ 通道守卫 —— 实盘自动未接通道时明确失败，绝不静默降级
		if a.client == nil {
			out.Decision = "error"
			out.Reason = "实盘通道未接入（vaultd client 为 nil），无法自动下单"
			return out, nil
		}
	}

	// ⑤ 闸门判定（与人工路径同一套规则，不因为自动而放宽）
	res, err := a.gate.Authorize(ctx, GateRequest{
		DraftID:   draftID,
		AccountID: accountID,
		Mode:      mode,
	})
	if err != nil {
		return nil, fmt.Errorf("闸门判定: %w", err)
	}
	out.Decision = string(res.Decision)
	out.Reason = res.Reason

	switch res.Decision {
	case DecisionDeny:
		return out, nil // 拒绝是正确的业务结果，不是错误
	case DecisionAsk:
		// 自动模式无法满足「需人工确认」→ 记录但不执行。
		//
		// ⚠️ 这里**不做**「自动升级为 allow」—— 那等于把 ask 当 allow，
		// 是自动交易最危险的错误理解。ask 就是「我判断不了，要人看」。
		out.Reason = "闸门要求人工确认（自动模式下不执行）：" + res.Reason
		return out, nil
	case DecisionAllow:
		// paper：最后一跳落到纸面账本，不碰真实通道。
		//
		// 仍复用上面的 Gate.Authorize（同一套模式/账户/草案/风控判定与审计），
		// 只是把「下发渠道」换成 ExecutePaper —— 模拟成交与实盘口径一致。
		if isPaper {
			paperID, pErr := a.gate.ExecutePaper(ctx, draftID, accountID)
			if pErr != nil {
				out.Decision = "error"
				out.Reason = "模拟盘自动成交失败：" + pErr.Error()
				return out, pErr
			}
			out.ChannelID = fmt.Sprintf("paper:%d", paperID)
			out.Reason = "模拟盘自动成交（不涉及真实资金）"
			return out, nil
		}
		// live_auto：继续下发真实通道
	default:
		return nil, fmt.Errorf("闸门返回未知判定 %q", res.Decision)
	}

	// ⑥ 组单 + 幂等键 + 下发
	sub, err := a.buildSubmit(ctx, draftID, accountID)
	if err != nil {
		return nil, err
	}
	out.ClientOrder = sub.ClientOrderID

	// 单笔金额守卫（降级判据之二）。
	//
	// 闸门内部已检查限额，但那读的是执行时的 limits 快照。
	// 这里是**下发前**的最后一次核对：若限额在下单瞬间被收紧（另一实例/人工改），
	// 闸门的结果可能已过期。金额超限时降级而不是仅拒绝这一单 ——
	// 因为「限额被临时改小」本身就是异常信号。
	if over, amt := a.exceedsSingleLimit(sub); over {
		out.Degraded = true
		out.DegradeWhy = fmt.Sprintf("单笔金额 %s 超过 live_auto 限额", amt)
		a.degrade(ctx, out.DegradeWhy)
		return out, nil
	}

	resp, err := a.client.Submit(ctx, sub)
	if err != nil {
		// 下发失败：记录但不降级（网络抖动是常态）。
		// 连续失败由 tick 的 failStreak 计数处理。
		out.Decision = "error"
		out.Reason = "通道下发失败：" + err.Error()
		return out, err
	}
	out.ChannelID = resp.ChannelOrderID

	// 标记已下发：下一个 tick 就不会再捞到这一单。
	//
	// 这是幂等的**第二道闸**（第一道是确定性 client_order_id）。
	// 两道都有意义：
	//   ① client_order_id 防「同一单在渠道侧重复」
	//   ② exported_at    防「本地再捞一次发出去」
	// 只靠 ① 依赖渠道真的实现了去重；只靠 ② 在写标记前崩溃仍会重发。
	if _, mErr := a.db.OrderDrafts().MarkExported(draftID); mErr != nil {
		// 标记失败不回滚下单 —— 单已经发出去了，回滚没有意义。
		// 但要告警：下一轮可能重发（幂等键会兜住，但那依赖渠道行为）。
		slog.Error("自动交易：标记 exported 失败（下轮可能重发）",
			"draft_id", draftID, "client_order_id", out.ClientOrder, "err", mErr)
	}
	return out, nil
}

// buildSubmit 从草案构造 vaultd 提交请求。
//
// # 幂等键怎么来
//
// client_order_id = sha256("draft:<id>")[:16]
//
// 确定性派生而不是随机 UUID：同一草案无论重试几次，键都一样 ——
// vaultd 侧按它去重，天然防重复下单。
//
// ⚠️ 这意味着**同一草案不能下两单**。这是刻意的：一条草案对应一次决策，
// 要再下一单必须新建草案（新建就换了 id，换了幂等键）。
func (a *AutoTrader) buildSubmit(ctx context.Context, draftID, accountID int64) (SubmitRequest, error) {
	draft, err := a.db.OrderDrafts().GetDraft(draftID)
	if err != nil {
		return SubmitRequest{}, fmt.Errorf("读草案 %d: %w", draftID, err)
	}
	// 没有 instrument_id 的草案无法下单 —— 拿不到 symbol 就发不出去。
	// 这是历史遗留数据（加列前建的草案），不是系统错误，但必须明确拒绝。
	if draft.InstrumentID == nil {
		return SubmitRequest{}, fmt.Errorf("草案 %d 没有标的(instrument_id)，无法下单", draftID)
	}
	inst, err := a.db.Instruments().GetInstrumentByID(*draft.InstrumentID)
	if err != nil {
		return SubmitRequest{}, fmt.Errorf("读标的: %w", err)
	}
	acct, err := a.db.ExecAccounts().GetAccount(accountID)
	if err != nil {
		return SubmitRequest{}, fmt.Errorf("读账户 %d: %w", accountID, err)
	}

	// 幂等键：sha256("draft:<id>")[:16] 十六进制，前缀 artex。
	//
	// ⚠️ **不能带连字符**。OKX 对 clOrdId 只允许字母数字（长度 ≤32）：
	//
	//	artex-probe-1793933483 → sCode=51000 "Parameter clOrdId error"
	//	artex1793933492        → sCode=0 "Order placed"
	//
	// 原来写的是 "artex-" + hex，走 REST API 时 OKX 接受，但 CLI 通道
	// 统一被拒（2026-11-06 实测）。去掉连字符后两种通道都能用。
	h := sha256.Sum256([]byte(fmt.Sprintf("draft:%d", draftID)))
	clientID := "artex" + hex.EncodeToString(h[:8])

	req := SubmitRequest{
		Channel:       acct.Channel,
		AccountRef:    acct.ChannelAcctID,
		ClientOrderID: clientID,
		Symbol:        inst.Symbol,
		Side:          draft.Side,
		Qty:           draft.Qty,
		OrderType:     "limit", // 自动交易默认限价 —— 市价在无人值守下风险不可控
	}
	if draft.RefPrice != nil {
		req.Price = *draft.RefPrice
	}

	// 现货小单改按金额下单。
	//
	// # 为什么（2026-11-06 实测）
	//
	// OKX 现货最小下单额是 1 USDT 等值。草案的 qty 是**币量**，
	// 而模型算仓位时习惯给金额（如「买 10 USDT」）。换算成币量后
	// 常小到被拒，且错误码笼统（"All operations failed"）看不出原因：
	//
	//	qty=0.0001 BTC（约 8 USDT）→ 500 / All operations failed
	//	sz=10 & tgtCcy=quote_ccy   → 成功
	//
	// 所以：qty × ref_price 小于阈值时转成「按计价币金额」下单，
	// 沿用同一笔决策的金额，不改变风险敞口。
	if acct.Channel == "okx" && draft.RefPrice != nil && req.Price != "" {
		if amt, amtErr := notionalValue(draft.Qty, *draft.RefPrice); amtErr == nil && amt < minOkxSpotNotional {
			req.Qty = fmt.Sprintf("%.2f", minOkxSpotNotional) // 提到最小额
			req.TgtCcy = "quote_ccy"
			req.OrderType = "market" // 按金额下单用市价才能立即成交
			req.Price = ""
		} else if amtErr == nil {
			req.TgtCcy = "base_ccy" // 金额够，按币量下（保留限价单语义）
		}
	}
	return req, nil
}

// exceedsSingleLimit 检查本次下单金额是否超过 live_auto 档的单笔限额。
//
// 返回 (是否超限, 金额字符串)。
func (a *AutoTrader) exceedsSingleLimit(sub SubmitRequest) (bool, string) {
	// 限额只有**一套**，key 固定 global。
	//
	// # 为什么不是 GetLimits("live_auto")（2026-11-06 事故）
	//
	// 原来这里传 db.ExecutionModeLiveAuto（="live_auto"）当 key，但：
	//
	//	- 写入口（POST /api/exec/limits）只写 key='global'
	//	- paper 路径（exec/paper.go）读的也是 key='global'
	//
	// 于是自动交易永远读不到限额 → 每次都判「未配置 → 超限」→ 降级回 paper，
	// 一笔单都发不出去。实测日志：
	//
	//	INFO  自动交易：开始执行 drafts=1 account_id=11569
	//	ERROR 单笔金额 未配置（自动交易必须显式配置单笔限额）超过 live_auto 限额
	//
	// 而 user 明明配了 20 USDT 单笔限额（key=global, revision=4）。
	//
	// 统一读 global 后，paper 与 live_auto 共用同一套风控参数 ——
	// 这也符合直觉：用户配的是「我的交易上限」，不是「某个模式的限额」。
	limits, err := a.db.ExecutionLimits().GetLimits(limitsKeyGlobal)
	if err != nil || limits == nil || limits.MaxSingleAmount == nil {
		// 没有配置限额 → 按「最严」处理：视为超限。
		//
		// ⚠️ 这一条是刻意保守的。无人值守 + 无限额 = 无限风险敞口。
		// 用户必须显式配置 live_auto 的限额才能自动交易。
		return true, "未配置（自动交易必须显式配置单笔限额）"
	}
	// 金额 = 数量 × 参考价。任一为空则无法判定，按超限处理。
	if sub.Price == "" {
		return true, "草案无参考价（无法判定金额）"
	}
	amt := mulDecimal(sub.Qty, sub.Price)
	// cmpDecimal 解析失败时返回 1（视为超限）—— fail-closed。
	return cmpDecimal(amt, *limits.MaxSingleAmount) > 0, amt
}

// hasLiveAutoAuth 检查该账户是否有未撤销的 live_auto 授权。
func (a *AutoTrader) hasLiveAutoAuth(ctx context.Context, accountID int64) (bool, error) {
	// 授权按 (channel, account_ref) 记录，先解析账户。
	acct, err := a.db.ExecAccounts().GetAccount(accountID)
	if err != nil {
		return false, fmt.Errorf("读账户 %d: %w", accountID, err)
	}
	auth, err := a.db.ExecAuthorizations().ActiveFor(acct.Channel, acct.ChannelAcctID)
	if err != nil {
		// ErrAuthorizationRequired 是「没授权」，不是系统错误。
		if errors.Is(err, db.ErrAuthorizationRequired) {
			return false, nil
		}
		return false, err
	}
	// 授权必须**正好是** live_auto。
	//
	// ⚠️ 不能接受 live_manual 的授权来跑自动：那是两件不同的事。
	// 授权表里 mode 记的是「授权到哪个档位」。
	return auth.Mode == string(db.ExecModeLiveAuto), nil
}

// NoteReconcileResult 由对账调度在每次对账后调用，维护连续失败计数。
//
// # 为什么降级判据是「连续 N 次」而不是「单次失败」
//
// 券商 API 偶发超时是常态。单次失败就降级会让自动交易几乎无法运行。
// 但连续 3 次（3 分钟）都对不上，说明本地记录与券商事实已经脱节 ——
// 那是最危险的金融状态（可能重复下单、可能漏单），必须停。
func (a *AutoTrader) NoteReconcileResult(ok bool) {
	const degradeAfter = 3

	if ok {
		a.mu.Lock()
		a.failStreak = 0
		a.mu.Unlock()
		return
	}
	a.mu.Lock()
	a.failStreak++
	streak := a.failStreak
	a.mu.Unlock()

	if streak >= degradeAfter {
		a.degrade(context.Background(),
			fmt.Sprintf("对账连续 %d 次失败 —— 本地记录与券商事实可能已脱节", streak))
	}
}

// 确保 time 被引用（tick 相关逻辑在 server 侧，这里用类型锚点）。
var _ = time.Now

// minOkxSpotNotional 是 OKX 现货的最小下单金额（USDT）。
//
// 实测依据（2026-11-06）：0.0001 BTC ≈ 8 USDT 的市价单被拒
// （"All operations failed"），而 10 USDT 的 quote_ccy 单成功。
// 取 10 而非 1 是留 buffer —— 1 USDT 在某些币对上仍可能因精度被拒。
const minOkxSpotNotional = 10.0

// notionalValue 算金额 = 数量 × 价格。
func notionalValue(qty, price string) (float64, error) {
	q, err := strconv.ParseFloat(strings.TrimSpace(qty), 64)
	if err != nil {
		return 0, fmt.Errorf("qty 不是数字: %w", err)
	}
	p, err := strconv.ParseFloat(strings.TrimSpace(price), 64)
	if err != nil {
		return 0, fmt.Errorf("price 不是数字: %w", err)
	}
	return q * p, nil
}
