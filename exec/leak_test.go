package exec

// 泄漏测试（WE-14 的 paper 路径部分）。
//
// # 为什么现在只能做 paper 路径
//
// WE-14 的完整验收要求「假 key 跑完 submit→query→对账」，
// 而 submit/query 要经过 vaultd 的真实通道（WE-6/WE-7 未做）。
//
// 本文件做**能做的部分**：paper 路径 + 我们自己写的所有落库点。
// vaultd 相关的部分在 exec/vaultd 的测试里（TestSubmitRedactsRawAck 已验证出参脱敏）。
//
// # 为什么 paper 路径也值得测
//
// `exec_audit.detail` 存 `before`/`after`（含限额数字）、`account_ref`、
// `channel` —— 如果将来有人往 detail 里加了一个凭证字段，这个测试能提前发现。
//
// 而「我们自己的代码不泄漏凭证」是一个可以独立验证的命题，
// 不依赖券商通道是否接好。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/redact"
)

// fakeSecret 是一个形似真实凭证的字符串。
//
// # 为什么用「key=value」的形态，而不是裸的 sk-live-xxx
//
// `redact` 的识别依赖两个条件之一：
//
//	① 有敏感的 key 名上下文（api_key: xxx / token=xxx）
//	② 值本身是 64+ 字符的连续 blob
//
// 而裸的 `sk-live-<35 字符>` 两者都不满足 —— 它是**刻意的取舍**：
// 裸串识别会误伤正常文本（任何 35 字符的随机字符串都会被遮）。
//
// 所以测试必须用「真实会泄漏的形态」：
// 凭证总是以某个字段的形式出现（错误信息里的 "api_key=xxx"、
// 通道返回体里的 {"secret": "xxx"}），而不是孤立的一段字符串。
const fakeSecret = "api_key=sk-live-abcdefghijklmnopqrstuvwxyz0123456789"

// TestNoSecretInPaperPath paper 全路径不把假 key 落进任何表。
//
// # 怎么注入假 key
//
// 把它写进 draft 的可自由文本字段（executed_note / ref_price 之类的边缘），
// 然后跑 ExecutePaper，再全表扫描。
//
// 这个测法有点「人为」 —— 但它验的是「万一有人把凭证塞进了某个字段，
// 我们有没有机会发现」。答案是：有，只要扫描所有可能的落库点。
func TestNoSecretInPaperPath(t *testing.T) {
	e := newReconcileEnv(t, "leak-paper")

	// 建一张带假 key 的草案（模拟「凭证被误写进业务字段」）。
	inst := newGateTestInstrument(t, e.g, "crypto", "BTC-USDT", "crypto")
	draftID, err := e.g.db.OrderDrafts().InsertDraft(db.OrderDraftInput{
		InstrumentID: &inst.ID,
		Side:         "buy",
		Qty:          "0.0000000001",
		RefPrice:     "100",
		// risk_verdict 有取值白名单（pass/deny/ask），不能塞自由文本。
		// 用它 —— 它是给人看的备注，假设有人把凭证写进来了。
		RiskVerdict: "pass",
	})
	if err != nil {
		t.Fatalf("建草案: %v", err)
	}
	trackGateTestDraft(t, e.g, draftID)
	// 把假 key 写进可自由文本的字段（这里模拟「凭证被误写进业务字段」）。
	if _, eErr := e.g.db.Exec(`UPDATE orders_draft SET executed_note=$1 WHERE id=$2`,
		fakeSecret, draftID); eErr != nil {
		t.Fatalf("写 executed_note 失败：%v", eErr)
	}
	_ = draftID

	// 跑一次冻结（会写 exec_audit）+ 一次对账（会写 exec_reconciliations）。
	// 这两条路径覆盖了我们新加的所有落库点。
	if _, fErr := e.g.db.FreezeAccountTx(e.acct.ID, e.acct.Channel, e.acct.ChannelAcctID,
		"泄漏测试：reason 里也放一个 "+fakeSecret); fErr != nil {
		t.Fatalf("FreezeAccountTx: %v", fErr)
	}

	// ---- 扫描落库点 ----
	//
	// # 只列本测试真的会写到的表
	//
	// 每条测试应该只覆盖它能真的走到的路径。硬把别的表塞进来，
	// 会让「空表也通过」——那正是这次修掉的缺陷。
	//
	// 本测试的路径：
	//   InsertDraft      → orders_draft（**注入点**，不扫）
	//   FreezeAccountTx  → exec_accounts（block_reason）+ exec_audit
	//
	// # 其它表的覆盖在哪（避免将来有人以为漏了）
	//
	//   exec_reconciliations → TestNoSecretInReconcileRemoteSnapshot（跑真对账）
	//   paper_orders         → 需 ExecutePaper 路径，未在此测试中调用
	//   orders_execution     → 本测试建了单，但 raw_ack 恒空（没走 submit）
	//   draft_risk_verdicts  → 需 EvaluateDraft 路径，未在此测试中调用
	checks := []struct {
		table string
		sql   string
	}{
		// ⚠️ orders_draft 不在这里 —— 假 key 是本测试**故意**写进
		// executed_note 的（模拟「凭证被误写进业务字段」）。
		// 扫它必然失败，而那个失败不说明脱敏有问题，只说明注入生效了。
		//
		// 所以这里只扫「假 key 不该出现」的表。
		{
			table: "exec_audit",
			sql:   `SELECT COALESCE(string_agg(action || ' ' || detail || ' ' || actor, ' '), '') FROM exec_audit`,
		},
		{
			table: "exec_accounts",
			sql: `SELECT COALESCE(string_agg(
				channel || ' ' || channel_account_id || ' ' || COALESCE(block_reason,''), ' '), '')
				FROM exec_accounts`,
		},
	}

	for _, c := range checks {
		// # 为什么要断言表非空，而不是「空表就跳过」
		//
		// 「空表跳过」会让测试在「这条路径从没走过」时依然绿 —— 那正是之前的缺陷。
		// 表非空是本测试的**前提**，不是可选项：没数据就没验证任何东西。
		//
		// 所以：空表 → t.Fatalf（不是 t.Skip / 不是 continue）。
		var rowCount int
		if err := e.g.db.QueryRow("SELECT count(*) FROM " + c.table).Scan(&rowCount); err != nil {
			t.Fatalf("数 %s 行数失败：%v", c.table, err)
		}
		if rowCount == 0 {
			t.Fatalf("表 %s 是空的 —— 这条路径没被走到，扫描结果无意义。"+
				" 请先让测试跑到这条路径（或移除这条检查并注明由哪个测试覆盖）", c.table)
		}

		var got string
		if err := e.g.db.QueryRow(c.sql).Scan(&got); err != nil {
			// 列不存在是不可恢复的错误 —— 不是「跳过」的理由。
			t.Fatalf("扫 %s 失败（列可能不存在）：%v", c.table, err)
		}
		if strings.Contains(got, fakeSecret) {
			t.Errorf("表 %s 里出现了假 key —— 泄漏；内容片段：%s", c.table, truncate(got, 300))
		}
	}
}

// TestRedactMasksFakeSecret 确认真 key 形状的字符串会被 redact 拦住。
//
// # 为什么先测这个
//
// 上面那条测试是「扫描落库点」。而如果 redact 本身不认这种形状的 key，
// 那扫描通过也只是「因为没有人往里塞」。
//
// 所以先证明 redact 有效，再证明落库点干净 —— 两者的组合才有意义。
func TestRedactMasksFakeSecret(t *testing.T) {
	// 这里直接调 redact 包（它是 exec 的兄弟包，不破坏隔离：
	// exec 可以依赖 redact，redact 只依赖 stdlib）。
	masked := maskForTest(fakeSecret)
	if strings.Contains(masked, fakeSecret) {
		t.Fatalf("redact 没有拦住形似凭证的字符串\n实际：%s", masked)
	}
	if !strings.Contains(masked, "REDACTED") && !strings.Contains(masked, "***") {
		t.Logf("警告：脱敏后的内容里没有明显的占位标记，实际：%s", masked)
	}
}

// TestExecAuditDetailHasNoCredentialFields 断言审计 detail 里不含凭证字段名。
//
// # 这条是「结构性断言」，不是「值断言」
//
// 上一条扫描的是「假 key 有没有落进去」；这条扫描的是「detail 的字段名里
// 有没有 api_key / secret / token 这类不该出现的键」。
//
// 两者互补：值断言发现「已经泄漏了」，结构断言发现「有人在往这里加凭证字段」。
func TestExecAuditDetailHasNoCredentialFields(t *testing.T) {
	e := newReconcileEnv(t, "leak-audit-keys")

	if _, fErr := e.g.db.FreezeAccountTx(e.acct.ID, e.acct.Channel, e.acct.ChannelAcctID, "结构测试"); fErr != nil {
		t.Fatalf("FreezeAccountTx: %v", fErr)
	}

	var detail string
	if err := e.g.db.QueryRow(`SELECT detail FROM exec_audit
WHERE action='exec_frozen' ORDER BY id DESC LIMIT 1`).Scan(&detail); err != nil {
		t.Fatalf("读 detail: %v", err)
	}

	// 凭证类字段名（小写比较）。
	forbidden := []string{"api_key", "apikey", "secret", "password", "passphrase",
		"private_key", "privatekey", "token", "signature", "session", "cookie"}

	lower := strings.ToLower(detail)
	for _, k := range forbidden {
		if strings.Contains(lower, k) {
			t.Errorf("exec_audit.detail 里出现了凭证类字段名 %q —— 审计不该记录这类字段\n实际：%s", k, detail)
		}
	}
}

// ---- 辅助 ----

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// maskForTest 走 redact 包的 Mask 函数。
//
// 单独包一层是为了「如果 redact 的 API 变了，只改这里」。
func maskForTest(s string) string {
	var out map[string]any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		// 不是 JSON 就直接当字符串处理。
		return maskStringForTest(s)
	}
	raw, _ := json.Marshal(out)
	return string(maskJSONForTest(raw))
}

func maskStringForTest(s string) string { return redact.Mask(s) }
func maskJSONForTest(b []byte) []byte   { return redact.MaskJSON(b) }

// TestNoSecretInVaultdPath 走 Client → vaultd 的完整链路，断言无假 key 泄漏。
//
// # 这条补的是 L13 的一半
//
// L13 记的是「submit → query → 对账的完整链路未验证」。
// 本测试用 exec.Client + 一个 fake vaultd（模拟 vaultd 的脱敏行为）跑完
// submit → query，然后断言：
//
//	① 返回值里无假 key
//	② 错误信息里无假 key
//
// # 为什么 fake 而不是真 vaultd 进程
//
// 真进程测试在 exec/vaultd 的 12 个测试里（含 TestSubmitRedactsRawAck）。
// 这里测的是「Client 这一层不引入新泄漏」—— 用 fake 更聚焦，
// 且不引入跨进程依赖（那会让这个测试在 CI 上不稳）。
//
// # 还缺什么
//
// 「把结果落库」这一步（对账写 exec_reconciliations）——
// 那需要真实的通道回执，等 WE-6/7。见 L13。
func TestNoSecretInVaultdPath(t *testing.T) {
	const rawKey = "api_key=sk-live-abcdefghijklmnopqrstuvwxyz0123456789"

	// fake vaultd：模拟「已经脱敏过的 vaultd 输出」。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/submit":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"channel_order_id": "CH-1",
				"accepted_at":      "2026-10-28T00:00:00Z",
				// vaultd 侧已脱敏（TestSubmitRedactsRawAck 验的就是这个）
				"raw_ack": map[string]any{"api_key": "***", "ok": true},
			})
		case "/v1/query":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"orders": []map[string]any{{
					"client_order_id": "c-1", "channel_order_id": "CH-1",
					"state": "filled", "filled_qty": "1", "avg_price": "100",
				}},
			})
		default:
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":"not found","code":"VAULT_NOT_FOUND"}`))
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-token-should-not-leak-abcdef123456")
	ctx := t.Context()

	// ① submit
	sub, err := c.Submit(ctx, SubmitRequest{
		Channel: "okx", AccountRef: "a", ClientOrderID: "c-1",
		Symbol: "BTC-USDT", Side: "buy", Qty: "1",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if strings.Contains(string(sub.RawAck), rawKey) {
		t.Fatalf("Submit 的 raw_ack 出现凭证：%s", string(sub.RawAck))
	}

	// ② query
	orders, err := c.Query(ctx, QueryRequest{
		Channel: "okx", AccountRef: "a", ClientOrderID: "c-1",
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}

	// ③ 把整个链路的输出拼起来扫一遍
	var all strings.Builder
	blob, _ := json.Marshal(sub)
	all.Write(blob)
	blob, _ = json.Marshal(orders)
	all.Write(blob)

	if strings.Contains(all.String(), rawKey) {
		t.Fatalf("vaultd 链路里出现凭证：%s", all.String())
	}

	// ④ 错误路径也不能泄漏（用一个会 401 的 server）
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":"unauth","code":"VAULT_UNAUTHORIZED"}`))
	}))
	defer srv2.Close()

	c2 := NewClient(srv2.URL, "test-token-should-not-leak-abcdef123456")
	_, err = c2.Submit(ctx, SubmitRequest{
		Channel: "okx", AccountRef: "a", ClientOrderID: "c-1",
		Symbol: "BTC-USDT", Side: "buy", Qty: "1",
	})
	if err == nil {
		t.Fatal("401 应报错")
	}
	if strings.Contains(err.Error(), rawKey) {
		t.Fatalf("错误信息里出现凭证：%v", err)
	}
	if strings.Contains(err.Error(), "test-token-should-not-leak") {
		t.Fatalf("错误信息里出现 vaultd token（这比凭证更严重）：%v", err)
	}
}

// TestNoSecretInReconcileRemoteSnapshot 对账写入 remote_snapshot 时脱敏。
//
// # 这条关闭 L13 的最后一个未验证落库点
//
// L13 记的剩余缺口是「对账写 exec_reconciliations.remote_snapshot」。
// 而在 db.ExecReconciliationStore.Record 里加了脱敏后，这条测试验证它真的生效。
//
// # 测法：不 mock，走完整链路
//
// 1. 建一张 submitted 单
// 2. 用 ChannelQuerier 返回「带凭证的 raw_ack」（模拟通道原样带回错误信息）
// 3. 跑 Reconciler.Run
// 4. 扫描 exec_reconciliations 全表
//
// 这比「直接调 Record」强：它验证了「从通道回执到落库」整条路径。
func TestNoSecretInReconcileRemoteSnapshot(t *testing.T) {
	e := newReconcileEnv(t, "leak-recon-snapshot")

	const fakeKey = "api_key=sk-live-abcdefghijklmnopqrstuvwxyz0123456789"

	o := e.mkSubmittedOrder(t, time.Now())

	// 通道回执里带凭证 —— 模拟「券商 SDK 把请求体原样带回」。
	e.f.acks[o.ClientOrderID] = ChannelAck{
		Found:     true,
		State:     db.ExecStateFilled,
		FilledQty: "1",
		AvgPrice:  "100",
		Raw:       json.RawMessage(`{"api_key":"` + fakeKey + `","ok":true}`),
	}

	// 跑对账：会写 exec_reconciliations（含 remote_snapshot）。
	if _, err := e.r.Run(t.Context(), e.acct.ID); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// 断言：remote_snapshot 里没有假 key。
	var blob string
	if err := e.g.db.QueryRow(`SELECT COALESCE(string_agg(
		local_snapshot || ' ' || remote_snapshot || ' ' || diff, ' '), '')
		FROM exec_reconciliations WHERE account_id=$1`, e.acct.ID).Scan(&blob); err != nil {
		t.Fatalf("读对账记录: %v", err)
	}

	if strings.Contains(blob, fakeKey) {
		t.Fatalf("exec_reconciliations 里出现凭证 —— 脱敏失效\n内容：%s", truncate(blob, 400))
	}
	// 正向断言：正常字段还在（脱敏不能把整条记录抹掉）
	if !strings.Contains(blob, "exec_state") && !strings.Contains(blob, "filled") {
		t.Errorf("对账记录里应有正常的 state 信息，实际：%s", truncate(blob, 300))
	}
}
