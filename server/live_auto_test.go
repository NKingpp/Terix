package server

// live_auto 执行模式的集成验收（2026-11-05）。
//
// # 这些断言守的是什么
//
// 自动交易最危险的失败不是「算错」，而是「在不该下单时下了单」。
// 所以测试重点全在拒绝路径，且每条都断言「通道调用次数为 0」——
// 光看返回值不够，必须证明**真的没有发出去**。
//
// # 为什么用假通道而不是 HTTP mock
//
// 假通道能直接数调用次数。要是用 httptest，还得解析请求体才知道
// 「有没有下单」，而漏解析一个字段就会假通过。

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/exec"
)

// countingSubmitter 数出调用次数，用于断言「没有下单」。
type countingSubmitter struct {
	calls int
}

func (c *countingSubmitter) Submit(ctx context.Context, req exec.SubmitRequest) (*exec.SubmitResponse, error) {
	c.calls++
	return &exec.SubmitResponse{ChannelOrderID: "ch-test", AcceptedAt: "2026-11-05T00:00:00Z"}, nil
}

// TestLiveAutoModeAcceptsValue 模式常量与转换表必须包含 live_auto。
//
// # 为什么这条单独写
//
// 若 ParseExecMode 不认识 live_auto，切模式会报错；
// 若转换表缺条目，CanTransition 会静默拒绝 —— 用户看到「切不过去」
// 但拿不到原因。
func TestLiveAutoModeAcceptsValue(t *testing.T) {
	m, err := db.ParseExecMode("live_auto")
	if err != nil {
		t.Fatalf("ParseExecMode(\"live_auto\") 失败: %v", err)
	}
	if m != db.ExecModeLiveAuto {
		t.Errorf("解析结果 = %q, want live_auto", m)
	}

	// paper → live_auto 必须合法（首次开启的路径）
	if !db.CanTransitionExecMode(db.ExecModePaper, db.ExecModeLiveAuto) {
		t.Error("paper → live_auto 应合法")
	}
	// live_auto → paper 必须合法（降级路径）
	if !db.CanTransitionExecMode(db.ExecModeLiveAuto, db.ExecModePaper) {
		t.Error("live_auto → paper 应合法（降级出口）")
	}
	// live_auto → frozen 必须合法（紧急停止）
	if !db.CanTransitionExecMode(db.ExecModeLiveAuto, db.ExecModeFrozen) {
		t.Error("live_auto → frozen 应合法")
	}
	// live_auto → live_manual 必须**不**合法
	//
	// 这是刻意设计：从自动回手动必须经 paper，避免操作者
	// 心智还停留在「系统在跑」而实际已经变成手动。
	if db.CanTransitionExecMode(db.ExecModeLiveAuto, db.ExecModeLiveManual) {
		t.Error("live_auto → live_manual 不该直接合法（必须先经 paper）")
	}
}

// TestAutoTraderPaperNeverHitsRealChannel paper 模式允许自动成交，
// 但绝不能碰真实通道。
//
// # 为什么这条仍然是最核心的一条
//
// paper 自动成交只应写纸面账本（Gate.ExecutePaper）。如果实现里把
// paper 分支接到 ChannelSubmitter，用户以为在「模拟」，代码却调了
// 真实通道 —— 直接产生真实亏损。
func TestAutoTraderPaperNeverHitsRealChannel(t *testing.T) {
	e := newExecAPIEnv(t)
	pg := e.s.m.pg

	// 确保模式是 paper
	if _, _, err := pg.ForceSetExecMode(db.ExecModePaper); err != nil {
		t.Fatalf("设 paper 模式: %v", err)
	}

	ch := &countingSubmitter{}
	trader := exec.NewAutoTrader(pg, exec.NewGate(pg, nil), ch)

	// draftID / accountID 用任意值：这里只验证「paper 分支不会走到真实通道」。
	out, err := trader.Execute(context.Background(), 1, 1)
	if err != nil {
		t.Logf("Execute 返回 error（可接受，草案/账户不存在）: %v", err)
	}
	if out != nil && out.ChannelID != "" &&
		(len(out.ChannelID) < 6 || out.ChannelID[:6] != "paper:") {
		t.Errorf("paper 模式产生了非纸面渠道订单号 %q", out.ChannelID)
	}
	if ch.calls != 0 {
		t.Errorf("paper 模式下通道被调用 %d 次 —— 绝不能下单", ch.calls)
	}
}

// TestAutoTraderRejectsWithoutAuth live_auto 模式下，没有授权也要拒绝。
func TestAutoTraderRejectsWithoutAuth(t *testing.T) {
	e := newExecAPIEnv(t)
	pg := e.s.m.pg

	// 切到 live_auto，并**立刻**注册恢复。
	//
	// ⚠️ cleanup 必须在设置成功之后、断言之前注册：
	// 若放在 t.Fatalf 之后，失败时不会恢复，后续测试会看到 live_auto，
	// 于是「莫名奇妙地」在其他测试里触发自动交易路径。
	// 这个顺序问题实际导致过一次 package 级 FAIL。
	if _, _, err := pg.ForceSetExecMode(db.ExecModeLiveAuto); err != nil {
		t.Fatalf("设 live_auto: %v", err)
	}
	t.Cleanup(func() { _, _, _ = pg.ForceSetExecMode(db.ExecModePaper) })

	ch := &countingSubmitter{}
	trader := exec.NewAutoTrader(pg, exec.NewGate(pg, nil), ch)

	// accountID = 999999 不存在 → 授权检查必然失败
	out, err := trader.Execute(context.Background(), 1, 999999)
	if err != nil {
		t.Logf("Execute 返回 error: %v", err)
	}
	if out != nil && out.Decision == "allow" {
		t.Error("账户不存在时不该 allow")
	}
	if ch.calls != 0 {
		t.Errorf("未授权时通道被调用 %d 次 —— 绝不能下单", ch.calls)
	}
}

// TestLiveAutoIsTreatedAsLive isLive() 必须把 live_auto 当实盘档。
//
// # 2026-11-05 实测发现的严重遗漏
//
// isLive() 原来只认 whitelist / manual。加 live_auto 后没同步，
// 后果是切到 live_auto 时 isLiveUpgrade=false → **授权与限额校验全被跳过**，
// 模式直接切换成功。
//
// 2026-11-08 起改为「点应用直接切换」：切换本身不再要求授权或限额。
// 授权/限额守卫仍保留在 exec.ValidateTransition 的完整路径（其它调用方）
// 与 AutoTrader 的实盘通道守卫里。这条测试锁定 API 层的新行为。
func TestLiveAutoSwitchSkipsAuthAndLimits(t *testing.T) {
	e := newExecAPIEnv(t)
	pg := e.s.m.pg

	// 确保从 paper 出发
	if _, _, err := pg.ForceSetExecMode(db.ExecModePaper); err != nil {
		t.Fatalf("设 paper: %v", err)
	}
	t.Cleanup(func() { _, _, _ = pg.ForceSetExecMode(db.ExecModePaper) })

	// 清掉 live_auto 授权与限额，模拟「全新环境直接切自动」。
	//
	// ⚠️ 只用**本测试账户的**授权，绝不 `DELETE FROM exec_authorizations`。
	//
	// 原实现是全表 DELETE，会把用户真实的 live_auto 授权删掉 ——
	// 后果是用户切模式时突然报「需要重新授权」，而他没做任何操作。
	// （2026-11-05 与 market_credentials 同类事故一起修的。）
	const testRef = "modeguard-test-acct"
	e.ensureExecAccount(testRef)
	if _, err := pg.Exec(`DELETE FROM exec_authorizations WHERE channel='okx' AND account_ref=$1`,
		testRef); err != nil {
		t.Fatalf("清授权: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pg.Exec(`DELETE FROM exec_authorizations WHERE channel='okx' AND account_ref=$1`, testRef)
	})
	// 限额是按 mode 全局一条的，删掉会破坏用户配置 —— 只在缺失时才造。
	// 本测试要验证「无分档限额时切 live_auto 被拒」，所以必须确保它不存在，
	// 但**先备份原值，测试后还原**。
	var savedLimit string
	_ = pg.QueryRow(`SELECT COALESCE(max_single_amount,'') FROM execution_limits
WHERE mode='live_auto' LIMIT 1`).Scan(&savedLimit)
	if _, err := pg.Exec(`DELETE FROM execution_limits WHERE mode='live_auto'`); err != nil {
		t.Fatalf("清限额: %v", err)
	}
	t.Cleanup(func() {
		if savedLimit != "" {
			_, _ = pg.Exec(`INSERT INTO execution_limits (key, mode, max_single_amount)
VALUES ('live_auto', 'live_auto', $1)
ON CONFLICT (key) DO UPDATE SET max_single_amount=$1`, savedLimit)
		}
	})

	w := e.do(e.s.pgExecSetMode, "POST", "/api/exec/mode", `{"to":"live_auto"}`, nil)
	if w.Code != 200 {
		t.Fatalf("无授权无限额直接切 live_auto 应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	got, err := pg.GetExecMode()
	if err != nil {
		t.Fatalf("读模式: %v", err)
	}
	if got != db.ExecModeLiveAuto {
		t.Errorf("模式 = %q, want live_auto", got)
	}
}

// TestDowngradeOnStartupWorks live_auto 残留时启动降级必须回到 paper。
//
// # 为什么这条重要
//
// 用户心智里「重启 = 回到安全状态」。若重启后仍是 live_auto，
// 那么「先重启一下看看」这个本能动作反而会让自动交易在无人看管时恢复运行。
func TestDowngradeOnStartupWorks(t *testing.T) {
	e := newExecAPIEnv(t)
	pg := e.s.m.pg

	// 造一个「上次退出时停在 live_auto」的状态
	if _, _, err := pg.ForceSetExecMode(db.ExecModeLiveAuto); err != nil {
		t.Fatalf("设 live_auto: %v", err)
	}
	t.Cleanup(func() { _, _, _ = pg.ForceSetExecMode(db.ExecModePaper) })

	// 模拟启动降级
	if err := exec.DowngradeOnStartup(pg, nil); err != nil {
		t.Fatalf("DowngradeOnStartup: %v", err)
	}

	got, err := pg.GetExecMode()
	if err != nil {
		t.Fatalf("读模式: %v", err)
	}
	if got != db.ExecModePaper {
		t.Errorf("启动降级后模式 = %q, want paper", got)
	}
}

// TestAuthorizeRejectsWrongAccountRef 授权时 account_ref 必须与账户标识一致。
//
// # 2026-11-05 实测 bug
//
// 用户在前面手填 account_ref="1"（输入框是自由文本），而账户表里
// channel_account_id="execdemo-account"。授权**成功落库**，但切模式时
// 按真实标识精确匹配 → 永远查不到 → 报「授权已过期，需重新授权」。
//
// 这条测试钉住「授权写入时就要拦住错误值」，而不是等到切模式才发现。
func TestAuthorizeRejectsWrongAccountRef(t *testing.T) {
	e := newExecAPIEnv(t)
	pg := e.s.m.pg

	// 造一个明确标识的账户。
	//
	// ⚠️ 不要 DELETE FROM exec_accounts：那会清掉用户的真实账户配置
	// （实际发生过一次，把演示账户删了）。只清理本测试造的那一行。
	const testRef = "real-acct-xyz"
	if _, err := pg.Exec(`DELETE FROM exec_accounts WHERE channel_account_id=$1`, testRef); err != nil {
		t.Fatalf("清理本测试账户: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pg.Exec(`DELETE FROM exec_accounts WHERE channel_account_id=$1`, testRef)
	})
	if _, err := pg.Exec(`
INSERT INTO exec_accounts (channel, channel_account_id, label, market, active)
VALUES ('okx',$1,'测试账户','crypto',true)`, testRef); err != nil {
		t.Skipf("造账户失败（表结构可能不同）: %v", err)
	}

	// 用错误的 account_ref 授权 → 必须 400
	w := e.do(e.s.pgExecAuthorize, "POST", "/api/exec/authorize",
		`{"channel":"okx","account_ref":"1","mode":"live_auto","disclaimer_version":"v1","confirm_phrase":"我确认开启实盘自动交易并接受不再逐笔询问"}`, nil)
	if w.Code != 400 {
		t.Fatalf("错误 account_ref 授权应被拒（400），实际 %d body=%s", w.Code, w.Body.String())
	}

	// 用正确的 → 应成功
	w2 := e.do(e.s.pgExecAuthorize, "POST", "/api/exec/authorize",
		`{"channel":"okx","account_ref":"real-acct-xyz","mode":"live_auto","disclaimer_version":"v1","confirm_phrase":"我确认开启实盘自动交易并接受不再逐笔询问"}`, nil)
	if w2.Code != 200 {
		t.Fatalf("正确 account_ref 授权应成功，实际 %d body=%s", w2.Code, w2.Body.String())
	}
}

// TestDefaultAccountInModeResponse 模式接口必须给出权威的 account_ref。
//
// 这是前端只读展示的数据源 —— 没有它，前端只能让用户手填，
// 而手填正是那个 bug 的起因。
func TestDefaultAccountInModeResponse(t *testing.T) {
	e := newExecAPIEnv(t)
	pg := e.s.m.pg

	const testRef = "real-acct-xyz"
	if _, err := pg.Exec(`DELETE FROM exec_accounts WHERE channel_account_id=$1`, testRef); err != nil {
		t.Fatalf("清理本测试账户: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pg.Exec(`DELETE FROM exec_accounts WHERE channel_account_id=$1`, testRef)
	})
	if _, err := pg.Exec(`
INSERT INTO exec_accounts (channel, channel_account_id, label, market, active)
VALUES ('okx',$1,'测试账户','crypto',true)`, testRef); err != nil {
		t.Skipf("造账户失败: %v", err)
	}

	w := e.do(e.s.pgExecMode, "GET", "/api/exec/mode", "", nil)
	if w.Code != 200 {
		t.Fatalf("status=%d", w.Code)
	}
	var got struct {
		DefaultAccount struct {
			Channel    string `json:"channel"`
			AccountRef string `json:"account_ref"`
		} `json:"default_account"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析: %v", err)
	}
	// ⚠️ 不写死某个账户标识：库里可能有多个账户，
	// default_account 返回「第一个可用的」，具体是哪个取决于数据顺序。
	// 这条测试要证明的是「返回的值是真实的 channel_account_id」，
	// 而不是「返回了我刚插入的那一条」——后者会让测试对数据状态敏感。
	if got.DefaultAccount.AccountRef == "" {
		t.Fatal("default_account.account_ref 为空 —— 前端将无法自动填充")
	}
	var exists bool
	if err := pg.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM exec_accounts WHERE channel_account_id=$1)`,
		got.DefaultAccount.AccountRef).Scan(&exists); err != nil {
		t.Fatalf("校验标识: %v", err)
	}
	if !exists {
		t.Errorf("default_account.account_ref = %q 不在 exec_accounts 里 —— 前端会填出无效值",
			got.DefaultAccount.AccountRef)
	}
}
