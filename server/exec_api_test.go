package server

// 执行层 API 的验收测试（WE-12）。
//
// # 为什么直接调 handler 而不走完整 HTTP
//
// handler 只依赖 `s.m.pg` 和请求体 —— 走完整 mux 需要构造认证 token、
// 中间件链等无关组件。直接调 handler 让测试聚焦在「这个端点的行为」。
//
// 认证由 requireAuth 中间件保证（另有测试覆盖），这里是它的下游。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// execAPIEnv 准备一个带 pg 的 Server（不启动 engine/scheduler）。
type execAPIEnv struct {
	t *testing.T
	s *Server
	m *Manager
}

func newExecAPIEnv(t *testing.T) *execAPIEnv {
	t.Helper()
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { m.Close() })

	e := &execAPIEnv{t: t, s: &Server{m: m}, m: m}

	// 每个测试用独立账户，清理时按 id 删。
	t.Cleanup(func() {
		_, _ = m.pg.Exec(`DELETE FROM exec_audit WHERE actor='owner'`)
	})

	// 从干净的 paper 模式开始（有些测试会改）。
	_, _, _ = m.pg.ForceSetExecMode(db.ExecModePaper)

	return e
}

// ensureGlobalLimits 确保 global 限额齐全（RequireLiveReady 的前置条件）。
//
// # 为什么要这个 helper 而不是在每个测试里写 INSERT
//
// execution_limits 是**共享的全局表**（key='global'），而多个测试会
// DELETE 它来验证「限额缺失时拒绝」。测试之间因此有隐式依赖：
// 前面某条删了，后面调 RequireLiveReady 的测试就会意外失败，
// 而错误信息（EXEC_LIMITS_INCOMPLETE）看起来像「业务逻辑不对」。
//
// 用幂等 upsert 让每条需要的测试自己声明依赖，而不是靠执行顺序。
// ensureExecAccount 确保某 (channel, accountRef) 的执行账户存在。
//
// # 2026-11-05 为什么需要它
//
// 授权接口新增了 account_ref 校验：它必须是 exec_accounts.channel_account_id
// 里的真实标识，否则授权落库后切模式永远匹配不上（真实发生过的 bug）。
// 而早期测试用的是 auth-ok-1 这类**不存在的**假账户名，于是全被拒绝。
//
// 这不是放宽校验，而是让测试造的账户与它要授权的标识一致 ——
// 真实用户也是先在执行页建账户、再拿那个标识去授权。
func (e *execAPIEnv) ensureExecAccount(accountRef string) {
	e.t.Helper()
	if _, err := e.m.pg.Exec(`
INSERT INTO exec_accounts (channel, channel_account_id, label, market, market_support, active)
VALUES ('okx', $1, $1, 'crypto', '["crypto"]', true)
ON CONFLICT DO NOTHING`, accountRef); err != nil {
		e.t.Fatalf("造执行账户 %q: %v", accountRef, err)
	}
	e.t.Cleanup(func() {
		_, _ = e.m.pg.Exec(`DELETE FROM exec_accounts WHERE channel_account_id=$1`, accountRef)
	})
}

func (e *execAPIEnv) ensureGlobalLimits() {
	e.t.Helper()
	if _, err := e.m.pg.Exec(`
INSERT INTO execution_limits (key, mode, max_single_amount, max_daily_amount, max_daily_orders)
VALUES ('global', 'paper', 10000, 50000, 20)
ON CONFLICT (key) DO UPDATE SET
    max_single_amount = COALESCE(execution_limits.max_single_amount, 10000),
    max_daily_amount  = COALESCE(execution_limits.max_daily_amount, 50000),
    max_daily_orders  = COALESCE(execution_limits.max_daily_orders, 20)`); err != nil {
		e.t.Fatalf("确保 global 限额: %v", err)
	}
	e.t.Cleanup(func() {
		_, _ = e.m.pg.Exec(`DELETE FROM execution_limits WHERE key='global'`)
	})
}

// do 发一个请求到指定 handler。
func (e *execAPIEnv) do(h func(http.ResponseWriter, *http.Request), method, path, body string, pathVals map[string]string) *httptest.ResponseRecorder {
	e.t.Helper()
	var rd *bytes.Buffer
	if body == "" {
		rd = bytes.NewBufferString("{}")
	} else {
		rd = bytes.NewBufferString(body)
	}
	req := httptest.NewRequest(method, path, rd)
	for k, v := range pathVals {
		req.SetPathValue(k, v)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// decode 解响应体到 map。
func (e *execAPIEnv) decode(rec *httptest.ResponseRecorder) map[string]any {
	e.t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		e.t.Fatalf("响应不是合法 JSON: %v\nbody=%s", err, rec.Body.String())
	}
	return out
}

// mkAccount 建一个可用账户。
func (e *execAPIEnv) mkAccount(tag string) *db.ExecAccount {
	e.t.Helper()
	a, err := e.m.pg.ExecAccounts().UpsertAccount(db.ExecAccountInput{
		Channel:       "okx",
		ChannelAcctID: tag + "-" + strconv.FormatInt(int64(len(tag)), 10),
		Label:         tag,
		Market:        "crypto",
		MarketSupport: []string{"crypto"},
		Active:        true,
	})
	if err != nil {
		e.t.Fatalf("建账户: %v", err)
	}
	e.t.Cleanup(func() {
		_, _ = e.m.pg.Exec(`DELETE FROM exec_accounts WHERE id=$1`, a.ID)
	})
	return a
}

// ---- ① GET /api/exec/mode ----

func TestExecAPIGetModeHappyPath(t *testing.T) {
	e := newExecAPIEnv(t)
	e.mkAccount("mode-happy")

	rec := e.do(e.s.pgExecMode, http.MethodGet, "/api/exec/mode", "", nil)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := e.decode(rec)
	if body["mode"] != string(db.ExecModePaper) {
		t.Errorf("mode 应为 paper，实际 %v", body["mode"])
	}
	if body["frozen"] != false {
		t.Error("frozen 应为 false")
	}
	sum, ok := body["accounts_summary"].(map[string]any)
	if !ok {
		t.Fatalf("缺少 accounts_summary: %v", body)
	}
	if sum["total"].(float64) < 1 {
		t.Errorf("accounts_summary.total 应 >= 1，实际 %v", sum["total"])
	}
}

func TestExecAPIGetModeSummaryCountsBlocked(t *testing.T) {
	e := newExecAPIEnv(t)
	a := e.mkAccount("mode-blocked")
	if err := e.m.pg.ExecAccounts().Block(a.ID, "测试冻结"); err != nil {
		t.Fatalf("冻结: %v", err)
	}

	rec := e.do(e.s.pgExecMode, http.MethodGet, "/api/exec/mode", "", nil)
	body := e.decode(rec)
	sum := body["accounts_summary"].(map[string]any)
	if sum["blocked"].(float64) < 1 {
		t.Errorf("有冻结账户时 blocked 应 >= 1，实际 %v", sum["blocked"])
	}
}

// ---- ② POST /api/exec/mode ----

func TestExecAPISetModeFrozenToLiveRejected(t *testing.T) {
	e := newExecAPIEnv(t)

	// 先切到 frozen
	if rec := e.do(e.s.pgExecSetMode, http.MethodPost, "/api/exec/mode",
		`{"to":"frozen"}`, nil); rec.Code != 200 {
		t.Fatalf("切 frozen 失败: %d %s", rec.Code, rec.Body.String())
	}
	t.Cleanup(func() { _, _, _ = e.m.pg.ForceSetExecMode(db.ExecModePaper) })

	// frozen → live_manual 必须被拒（409，不是 400 —— 状态冲突）
	rec := e.do(e.s.pgExecSetMode, http.MethodPost, "/api/exec/mode",
		`{"to":"live_manual","channel":"okx","account_ref":"x"}`, nil)
	if rec.Code != 409 {
		t.Errorf("frozen→live 应返回 409，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	body := e.decode(rec)
	if body["code"] != "EXEC_MODE_FROZEN" {
		t.Errorf("错误码应为 EXEC_MODE_FROZEN，实际 %v", body["code"])
	}
}

// 2026-11-08：执行模式页改为「点应用直接切换」，授权仪式与限额完整性
// 不再作为切换前置。这条测试锁定新行为：清空限额后切 live_manual 仍应成功。
func TestExecAPISetModeLiveWithoutLimitsAllowed(t *testing.T) {
	e := newExecAPIEnv(t)
	t.Cleanup(func() { _, _, _ = e.m.pg.ForceSetExecMode(db.ExecModePaper) })

	if _, err := e.m.pg.Exec(`DELETE FROM execution_limits WHERE key='global'`); err != nil {
		t.Fatalf("清限额: %v", err)
	}

	rec := e.do(e.s.pgExecSetMode, http.MethodPost, "/api/exec/mode",
		`{"to":"live_manual","channel":"okx","account_ref":"x"}`, nil)
	if rec.Code != 200 {
		t.Errorf("直接切换应 200，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	body := e.decode(rec)
	if body["mode"] != "live_manual" {
		t.Errorf("模式应为 live_manual，实际 %v", body["mode"])
	}
}

// ---- ③ POST /api/exec/authorize ----

func TestExecAPIAuthorizeConfirmPhraseMismatch(t *testing.T) {
	e := newExecAPIEnv(t)

	rec := e.do(e.s.pgExecAuthorize, http.MethodPost, "/api/exec/authorize",
		`{"channel":"okx","account_ref":"a","mode":"live_manual",
		  "disclaimer_version":"v1","confirm_phrase":"错的短语"}`, nil)
	if rec.Code != 400 {
		t.Fatalf("确认短语不匹配应 400，实际 %d", rec.Code)
	}
	body := e.decode(rec)
	if body["code"] != "EXEC_CONFIRM_MISMATCH" {
		t.Errorf("错误码应为 EXEC_CONFIRM_MISMATCH，实际 %v", body["code"])
	}
}

func TestExecAPIAuthorizeUnknownDisclaimerVersion(t *testing.T) {
	e := newExecAPIEnv(t)

	rec := e.do(e.s.pgExecAuthorize, http.MethodPost, "/api/exec/authorize",
		`{"channel":"okx","account_ref":"a","mode":"live_manual",
		  "disclaimer_version":"v99","confirm_phrase":"我已知晓实盘交易风险"}`, nil)
	if rec.Code != 400 {
		t.Fatalf("未知版本应 400，实际 %d", rec.Code)
	}
	body := e.decode(rec)
	if body["code"] != "EXEC_DISCLAIMER_VERSION_UNKNOWN" {
		t.Errorf("错误码应为 EXEC_DISCLAIMER_VERSION_UNKNOWN，实际 %v", body["code"])
	}
}

func TestExecAPIAuthorizeLimitsIncomplete(t *testing.T) {
	e := newExecAPIEnv(t)
	if _, err := e.m.pg.Exec(`DELETE FROM execution_limits WHERE key='global'`); err != nil {
		t.Fatalf("清限额: %v", err)
	}

	rec := e.do(e.s.pgExecAuthorize, http.MethodPost, "/api/exec/authorize",
		`{"channel":"okx","account_ref":"a","mode":"live_manual",
		  "disclaimer_version":"v1","confirm_phrase":"我已知晓实盘交易风险"}`, nil)
	if rec.Code != 400 {
		t.Fatalf("限额不完整应 400，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	body := e.decode(rec)
	if body["code"] != "EXEC_LIMITS_INCOMPLETE" {
		t.Errorf("错误码应为 EXEC_LIMITS_INCOMPLETE，实际 %v", body["code"])
	}
}

// TestExecAPIAuthorizeHappyPath 授权成功路径。
//
// # 为什么必须补这一条（2026-11-02 线上事故）
//
// 用户点「授权实盘」报：
//
//	写授权失败：invalid paper order: 不支持的 from_mode ""
//
// 根因是 pgExecAuthorize 把 FromMode 硬编码成 ""，注释写「由调用方在切模式时填」
// 但那个调用方从来不存在。而**原有的四条授权测试全都只测失败路径**
// （短语不匹配 / 版本未知 / 限额不完整），每一条都在 FromMode 校验之前就
// return 了 —— 于是这个 bug 藏了整轮开发都没被发现。
//
// 教训：一个 handler 只有失败路径测试时，"成功路径是否可达"是完全没有保障的。
func TestExecAPIAuthorizeHappyPath(t *testing.T) {
	e := newExecAPIEnv(t)
	// 限定测试不污染真实授权表
	t.Cleanup(func() {
		_, _ = e.m.pg.Exec(`DELETE FROM exec_authorizations WHERE account_ref LIKE 'auth-ok-%'`)
	})
	// 前面的 TestExecAPISetModeLiveWithoutLimitsRejected 会删掉 global 限额
	// 且不恢复 —— 这里必须自己确保齐全，否则测的是「限额缺失」而不是授权本身。
	e.ensureGlobalLimits()

	acct := "auth-ok-1"
	e.ensureExecAccount(acct)
	rec := e.do(e.s.pgExecAuthorize, http.MethodPost, "/api/exec/authorize",
		`{"channel":"okx","account_ref":"`+acct+`","mode":"live_manual",
		  "disclaimer_version":"v1","confirm_phrase":"我已知晓实盘交易风险"}`, nil)

	if rec.Code != 200 {
		t.Fatalf("授权应成功，实际 %d body=%s", rec.Code, rec.Body.String())
	}

	// 核心断言：from_mode 必须落成当前真实模式（此夹具起始是 paper），
	// 不能是空串、也不能是猜的值。
	var fromMode, mode string
	if err := e.m.pg.QueryRow(
		`SELECT from_mode, mode FROM exec_authorizations
		 WHERE account_ref=$1 AND revoked_at IS NULL`, acct,
	).Scan(&fromMode, &mode); err != nil {
		t.Fatalf("查授权行: %v", err)
	}
	if fromMode != string(db.ExecModePaper) {
		t.Errorf("from_mode = %q, want %q（应记录授权时的真实模式）",
			fromMode, db.ExecModePaper)
	}
	if mode != string(db.ExecModeLiveManual) {
		t.Errorf("mode = %q, want live_manual", mode)
	}
}

// TestExecAPIAuthorizeFromModeFollowsCurrent 从 live_whitelist 扩权时 from_mode 应跟着变。
//
// 这一条防的是「修 bug 时图省事把 from_mode 写死成 paper」——
// 那样首次升实盘的记录对了，但「已授权状态下扩权」会留下错误的来源证据，
// 审计时无法区分「首次升实盘」和「从白名单扩到全手动」。
func TestExecAPIAuthorizeFromModeFollowsCurrent(t *testing.T) {
	e := newExecAPIEnv(t)
	t.Cleanup(func() {
		_, _ = e.m.pg.Exec(`DELETE FROM exec_authorizations WHERE account_ref LIKE 'auth-ok-%'`)
		_, _, _ = e.m.pg.ForceSetExecMode(db.ExecModePaper)
	})

	e.ensureGlobalLimits()
	if _, _, err := e.m.pg.ForceSetExecMode(db.ExecModeLiveWhitelist); err != nil {
		t.Fatalf("置 live_whitelist: %v", err)
	}

	acct := "auth-ok-2"
	e.ensureExecAccount(acct)
	rec := e.do(e.s.pgExecAuthorize, http.MethodPost, "/api/exec/authorize",
		`{"channel":"okx","account_ref":"`+acct+`","mode":"live_manual",
		  "disclaimer_version":"v1","confirm_phrase":"我已知晓实盘交易风险"}`, nil)
	if rec.Code != 200 {
		t.Fatalf("授权应成功，实际 %d body=%s", rec.Code, rec.Body.String())
	}

	var fromMode string
	if err := e.m.pg.QueryRow(
		`SELECT from_mode FROM exec_authorizations
		 WHERE account_ref=$1 AND revoked_at IS NULL`, acct,
	).Scan(&fromMode); err != nil {
		t.Fatalf("查授权行: %v", err)
	}
	if fromMode != string(db.ExecModeLiveWhitelist) {
		t.Errorf("from_mode = %q, want live_whitelist（不能写死成 paper）", fromMode)
	}
}

// TestExecAPIAuthorizeWritesLimitsSnapshot 授权必须固化限额快照。
//
// # 为什么这条最要紧（2026-11-02 线上事故）
//
// 原实现 `LimitsSnapshot: nil` → db 层写进 `{}`。而升级实盘时
// `LimitsSnapshotMatches({}, 当前限额)` **必然**返回 false（空快照意为
// 「授权时没配限额」，当前有序额即视为不一致）。
//
// 结果是完整死锁：
//  1. 用户授权 → 200 成功
//  2. 用户切实盘 → 400 `EXEC_AUTH_STALE: 限额配置在授权后被修改过`
//     而用户从没改过限额
//  3. 重新授权 → 仍是 {} → 还是失败
//
// 快照留空同时占了两种坏处：放弃了「授权绑定某套限额」的保护，
// 又让校验必然失败。
func TestExecAPIAuthorizeWritesLimitsSnapshot(t *testing.T) {
	e := newExecAPIEnv(t)
	e.ensureGlobalLimits()
	t.Cleanup(func() {
		_, _ = e.m.pg.Exec(`DELETE FROM exec_authorizations WHERE account_ref LIKE 'snap-%'`)
	})

	acct := "snap-1"
	e.ensureExecAccount(acct)
	rec := e.do(e.s.pgExecAuthorize, http.MethodPost, "/api/exec/authorize",
		`{"channel":"okx","account_ref":"`+acct+`","mode":"live_whitelist",
		  "disclaimer_version":"v1","confirm_phrase":"我已知晓实盘交易风险"}`, nil)
	if rec.Code != 200 {
		t.Fatalf("授权应成功，实际 %d body=%s", rec.Code, rec.Body.String())
	}

	var snapJSON []byte
	if err := e.m.pg.QueryRow(
		`SELECT limits_snapshot FROM exec_authorizations
		 WHERE account_ref=$1 AND revoked_at IS NULL`, acct,
	).Scan(&snapJSON); err != nil {
		t.Fatalf("查授权行: %v", err)
	}
	// 核心断言：快照不能是空的 `{}`
	if len(snapJSON) == 0 || string(snapJSON) == "{}" {
		t.Fatalf("limits_snapshot 是空的 %q —— 切实盘时会被判 EXEC_AUTH_STALE，"+
			"而用户从没改过限额", string(snapJSON))
	}
	// 必须含实际限额值，否则「一致」是碰巧成立
	for _, k := range []string{"max_single_amount", "max_daily_amount", "max_daily_orders"} {
		if !strings.Contains(string(snapJSON), k) {
			t.Errorf("快照缺 %q: %s", k, snapJSON)
		}
	}
}

// TestExecAPIAuthorizeThenSwitchLive 授权后切实盘必须成功（端到端）。
//
// 这一条同时覆盖三个修复：
//   - from_mode 不再是空串（否则授权就写不进去）
//   - 限额快照被固化（否则切换时报 STALE）
//   - channel/account_ref 缺省时能推导（否则报 AUTH_REQUIRED）
func TestExecAPIAuthorizeThenSwitchLive(t *testing.T) {
	e := newExecAPIEnv(t)
	e.ensureGlobalLimits()
	t.Cleanup(func() {
		_, _ = e.m.pg.Exec(`DELETE FROM exec_authorizations WHERE account_ref LIKE 'e2e-%'`)
		_, _, _ = e.m.pg.ForceSetExecMode(db.ExecModePaper)
	})

	// 建一个可用账户 —— 切换时若不带 channel/ref，后端要靠它推导
	if _, err := e.m.pg.Exec(`
INSERT INTO exec_accounts (channel, channel_account_id, label, market, market_support, active)
VALUES ('okx', 'e2e-acct', 'e2e', 'crypto', '["crypto"]', true)
ON CONFLICT (channel, channel_account_id) DO UPDATE SET active=true, blocked_at=NULL`); err != nil {
		t.Fatalf("建账户: %v", err)
	}
	t.Cleanup(func() {
		_, _ = e.m.pg.Exec(`DELETE FROM exec_accounts WHERE channel_account_id='e2e-acct'`)
	})

	// 清掉该通道的其他有效授权 —— 回退逻辑只在「唯一一条」时生效，
	// 残留的旧授权会让它正确地拒绝（见 anyActiveAuthForChannel 的注释）。
	_, _ = e.m.pg.Exec(`UPDATE exec_authorizations SET revoked_at=now(), revoked_reason='test setup'
		WHERE channel='okx' AND revoked_at IS NULL`)

	acct := "e2e-1"
	e.ensureExecAccount(acct)
	recA := e.do(e.s.pgExecAuthorize, http.MethodPost, "/api/exec/authorize",
		`{"channel":"okx","account_ref":"`+acct+`","mode":"live_whitelist",
		  "disclaimer_version":"v1","confirm_phrase":"我已知晓实盘交易风险"}`, nil)
	if recA.Code != 200 {
		t.Fatalf("授权失败 %d: %s", recA.Code, recA.Body.String())
	}

	// 刻意**不传** channel/account_ref —— 复现前端 `api.setExecMode(to)` 的形态。
	// 后端应靠 anyActiveAuthForChannel 回退找到授权。
	//
	// ⚠️ 让当前 revision 与授权时一致。
	//
	// 这是**测试数据污染**的补救，不是实现问题：execution_limits 是共享全局表，
	// 前面的 TestExecAPISetModeLiveWithoutLimitsRejected 会 DELETE 它，
	// 重建后 revision 从 1 重新开始 —— 与授权时记下的值不同，被判 STALE。
	// 线上 revision 单调递增，不会出现这种回退。
	//
	// 用 UPSERT 而不是 UPDATE：行可能已被删，UPDATE 会静默 0 rows。
	if _, err := e.m.pg.Exec(`
INSERT INTO execution_limits (key, mode, max_single_amount, max_daily_amount, max_daily_orders, revision)
SELECT 'global', 'paper', 10000, 50000, 20,
       (SELECT rules_revision FROM exec_authorizations WHERE account_ref='` + acct + `')
ON CONFLICT (key) DO UPDATE SET
    revision = (SELECT rules_revision FROM exec_authorizations WHERE account_ref='` + acct + `')`); err != nil {
		t.Fatalf("对齐 revision: %v", err)
	}
	rec := e.do(e.s.pgExecSetMode, http.MethodPost, "/api/exec/mode",
		`{"to":"live_whitelist"}`, nil)
	if rec.Code != 200 {
		t.Fatalf("切实盘失败 %d: %s（若 code=EXEC_AUTH_REQUIRED 说明授权回退没生效）",
			rec.Code, rec.Body.String())
	}

	mode, err := e.m.pg.GetExecMode()
	if err != nil {
		t.Fatalf("读模式: %v", err)
	}
	if string(mode) != string(db.ExecModeLiveWhitelist) {
		t.Errorf("mode = %q, want live_whitelist", mode)
	}
}

// ---- ④ POST /api/exec/accounts/{id}/unfreeze ----

func TestExecAPIUnfreezeNotFrozenReturns409(t *testing.T) {
	e := newExecAPIEnv(t)
	a := e.mkAccount("unfreeze-notfrozen")
	// 模式是 paper（不是 frozen）
	rec := e.do(e.s.pgExecUnfreeze, http.MethodPost,
		"/api/exec/accounts/1/unfreeze",
		`{"reason":"测试","confirm_phrase":"我确认解除冻结"}`,
		map[string]string{"id": strconv.FormatInt(a.ID, 10)})

	if rec.Code != 409 {
		t.Fatalf("非 frozen 时解冻应 409，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	body := e.decode(rec)
	if body["code"] != "EXEC_NOT_FROZEN" {
		t.Errorf("错误码应为 EXEC_NOT_FROZEN，实际 %v", body["code"])
	}
}

func TestExecAPIUnfreezeEmptyReasonRejected(t *testing.T) {
	e := newExecAPIEnv(t)
	a := e.mkAccount("unfreeze-noreason")

	rec := e.do(e.s.pgExecUnfreeze, http.MethodPost,
		"/api/exec/accounts/1/unfreeze",
		`{"reason":"  ","confirm_phrase":"我确认解除冻结"}`,
		map[string]string{"id": strconv.FormatInt(a.ID, 10)})

	if rec.Code != 400 {
		t.Fatalf("空 reason 应 400，实际 %d", rec.Code)
	}
	body := e.decode(rec)
	if body["code"] != "EXEC_REASON_REQUIRED" {
		t.Errorf("错误码应为 EXEC_REASON_REQUIRED，实际 %v", body["code"])
	}
}

func TestExecAPIUnfreezeHappyPath(t *testing.T) {
	e := newExecAPIEnv(t)
	a := e.mkAccount("unfreeze-happy")

	// 先进 frozen 并冻结该账户
	if _, err := e.m.pg.Exec(`UPDATE exec_accounts SET blocked_at=now(), block_reason='test' WHERE id=$1`, a.ID); err != nil {
		t.Fatalf("冻结账户: %v", err)
	}
	if _, _, err := e.m.pg.ForceSetExecMode(db.ExecModeFrozen); err != nil {
		t.Fatalf("切 frozen: %v", err)
	}
	t.Cleanup(func() { _, _, _ = e.m.pg.ForceSetExecMode(db.ExecModePaper) })

	rec := e.do(e.s.pgExecUnfreeze, http.MethodPost,
		"/api/exec/accounts/1/unfreeze",
		`{"reason":"问题已处理","confirm_phrase":"我确认解除冻结"}`,
		map[string]string{"id": strconv.FormatInt(a.ID, 10)})

	if rec.Code != 200 {
		t.Fatalf("解冻应成功，实际 %d body=%s", rec.Code, rec.Body.String())
	}

	// 断言副作用：模式回 paper + 账户解除冻结 + 审计有一行
	mode, _ := e.m.pg.GetExecMode()
	if mode != db.ExecModePaper {
		t.Errorf("模式应回 paper，实际 %s", mode)
	}
	got, _ := e.m.pg.ExecAccounts().GetAccount(a.ID)
	if got.BlockedAt != nil {
		t.Error("账户应已解冻")
	}
	var n int
	_ = e.m.pg.QueryRow(`SELECT count(*) FROM exec_audit WHERE action='exec_unfreeze'`).Scan(&n)
	if n != 1 {
		t.Errorf("应有 1 条 exec_unfreeze 审计，实际 %d", n)
	}
}

// ---- ⑤ GET /api/exec/orders ----

func TestExecAPIListOrdersStateFilter(t *testing.T) {
	e := newExecAPIEnv(t)
	a := e.mkAccount("orders-filter")

	// 建两张单：submitted + filled
	instID, _, _ := e.m.pg.Instruments().UpsertInstrument(db.InstrumentInput{
		Market: "crypto", Symbol: "BTC-USDT", AssetClass: "crypto",
	})
	draftID, err := e.m.pg.OrderDrafts().InsertDraft(db.OrderDraftInput{
		InstrumentID: &instID, Side: "buy", Qty: "1", RefPrice: "100",
	})
	if err != nil {
		t.Fatalf("建草案: %v", err)
	}
	for i, st := range []string{db.ExecStateSubmitted, db.ExecStateFilled} {
		_, iErr := e.m.pg.ExecOrders().InsertOrder(db.ExecOrderInput{
			DraftID: draftID, AccountID: a.ID, Mode: string(db.ExecModePaper),
			State: st, ClientOrderID: "ord-" + strconv.Itoa(i) + "-" + strconv.FormatInt(a.ID, 10),
			Qty: "1",
		})
		if iErr != nil {
			t.Fatalf("建执行单: %v", iErr)
		}
	}
	t.Cleanup(func() {
		_, _ = e.m.pg.Exec(`DELETE FROM orders_execution WHERE account_id=$1`, a.ID)
		_, _ = e.m.pg.Exec(`DELETE FROM orders_draft WHERE id=$1`, draftID)
	})

	// 过滤 submitted
	rec := e.do(e.s.pgExecOrders, http.MethodGet, "/api/exec/orders?state=submitted", "", nil)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := e.decode(rec)
	list, _ := body["orders"].([]any)
	if len(list) == 0 {
		t.Fatal("应至少返回 1 张 submitted 单")
	}
	for _, o := range list {
		m := o.(map[string]any)
		if m["state"] != db.ExecStateSubmitted {
			t.Errorf("过滤生效失败：出现了 state=%v 的单", m["state"])
		}
	}
}

// ---- ⑥ GET/POST /api/exec/limits ----

func TestExecAPIGetLimitsExcludesDeprecatedFields(t *testing.T) {
	e := newExecAPIEnv(t)
	if _, err := e.m.pg.ExecutionLimits().UpsertLimits(db.ExecutionLimitsInput{
		Key: "global", Mode: string(db.ExecModePaper),
		MaxSingleAmount: "1000", MaxDailyAmount: "5000", MaxDailyOrders: int64Ptr(10),
	}); err != nil {
		t.Fatalf("seed 限额: %v", err)
	}

	rec := e.do(e.s.pgExecGetLimits, http.MethodGet, "/api/exec/limits", "", nil)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	raw := rec.Body.String()

	// 反向：废弃字段不该出现
	for _, gone := range []string{`"mode"`, `"whitelist_symbols"`} {
		if strings.Contains(raw, gone) {
			t.Errorf("GET /limits 不该返回废弃字段 %s\n实际: %s", gone, raw)
		}
	}
	// 正向：有效字段必须出现
	for _, want := range []string{`"max_single_amount"`, `"revision"`} {
		if !strings.Contains(raw, want) {
			t.Errorf("GET /limits 缺少有效字段 %s\n实际: %s", want, raw)
		}
	}
}

func TestExecAPISetLimitsContradictoryRejected(t *testing.T) {
	e := newExecAPIEnv(t)

	// max_single(10000) > max_daily(5000) —— 自相矛盾
	rec := e.do(e.s.pgExecSetLimits, http.MethodPost, "/api/exec/limits",
		`{"max_single_amount":"10000","max_daily_amount":"5000",
		  "max_daily_orders":10,"confirm_phrase":"我确认修改交易限额"}`, nil)
	if rec.Code != 400 {
		t.Fatalf("单笔>日累计应 400，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	body := e.decode(rec)
	if body["code"] != "EXEC_LIMITS_CONTRADICTORY" {
		t.Errorf("错误码应为 EXEC_LIMITS_CONTRADICTORY，实际 %v", body["code"])
	}
}

func TestExecAPISetLimitsWritesBeforeAfterAudit(t *testing.T) {
	e := newExecAPIEnv(t)
	// 先有一版限额
	if _, err := e.m.pg.ExecutionLimits().UpsertLimits(db.ExecutionLimitsInput{
		Key: "global", Mode: string(db.ExecModePaper),
		MaxSingleAmount: "1000", MaxDailyAmount: "5000", MaxDailyOrders: int64Ptr(10),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var beforeRev int64
	_ = e.m.pg.QueryRow(`SELECT revision FROM execution_limits WHERE key='global'`).Scan(&beforeRev)

	rec := e.do(e.s.pgExecSetLimits, http.MethodPost, "/api/exec/limits",
		`{"max_single_amount":"2000","max_daily_amount":"8000",
		  "max_daily_orders":20,"confirm_phrase":"我确认修改交易限额"}`, nil)
	if rec.Code != 200 {
		t.Fatalf("改限额应成功，实际 %d body=%s", rec.Code, rec.Body.String())
	}

	// 断言副作用 1：revision +1
	var afterRev int64
	_ = e.m.pg.QueryRow(`SELECT revision FROM execution_limits WHERE key='global'`).Scan(&afterRev)
	if afterRev != beforeRev+1 {
		t.Errorf("revision 应 +1（%d → %d），实际 %d", beforeRev, beforeRev+1, afterRev)
	}

	// 断言副作用 2：审计含 before + after
	var detail string
	if err := e.m.pg.QueryRow(`SELECT detail FROM exec_audit
WHERE action='exec_limits_change' ORDER BY id DESC LIMIT 1`).Scan(&detail); err != nil {
		t.Fatalf("读审计: %v", err)
	}
	for _, want := range []string{"before", "after", "revision_before", "revision_after"} {
		if !strings.Contains(detail, want) {
			t.Errorf("审计 detail 应含 %q，实际 %s", want, detail)
		}
	}
}

func int64Ptr(v int64) *int64 { return &v }
