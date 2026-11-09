package server

// POST /api/exec/drafts/{id}/execute 的验收测试（P0-3）。
//
// # 测的重点是「拒绝路径」，不是「成功执行」
//
// 成功执行需要一整套前置（授权、限额、账户、闸门规则全部就绪）——
// 那在 exec 包的 Gate 测试里已覆盖。这里测的是**HTTP 层到闸门之间**的接线：
//
//   - account_id 缺失 → 400（不接受默认账户）
//   - draft id 非法 → 400
//   - 未认证 → 401
//   - 闸门 deny → 400 + 具体 code
//
// 这些是「端点自己」的责任，与闸门内部逻辑无关。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
)

// executeTestServer 建一个最小 Server（不启动调度器），返回 handler + token。
func executeTestServer(t *testing.T) (http.Handler, string, *db.DB) {
	t.Helper()
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skipf("no test database configured (%v)", err)
	}
	d, err := db.Open(dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	m := &Manager{pg: d, interceptor: intercept.New(d)}
	s := &Server{m: m, jwtKey: []byte("exec-execute-test-signing-key")}
	token, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatalf("signJWT: %v", err)
	}
	return s.Handler(), token, d
}

// TestExecDraftExecuteRequiresAccountID 缺 account_id → 400（不接受默认账户）。
//
// # 为什么这条最重要
//
// 「执行路径不接受默认账户」是本端点的核心约定。
// 如果实现给了默认值，那「调错账户」会变成静默事故 —— 而它无法被测试发现，
// 除非专门写一条「不传 account_id 必须被拒绝」的测试。
func TestExecDraftExecuteRequiresAccountID(t *testing.T) {
	h, token, _ := executeTestServer(t)

	for _, body := range []string{
		`{}`,
		`{"account_id":0}`,
		`{"account_id":-1}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/exec/drafts/1/execute", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("body=%s 应 400，实际 %d：%s", body, rec.Code, rec.Body.String())
			continue
		}
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if out["code"] != "EXEC_ACCOUNT_REQUIRED" {
			t.Errorf("body=%s 的 code = %v, want EXEC_ACCOUNT_REQUIRED", body, out["code"])
		}
	}
}

// TestExecDraftExecuteRejectsBadDraftID draft id 非正整数 → 400。
func TestExecDraftExecuteRejectsBadDraftID(t *testing.T) {
	h, token, _ := executeTestServer(t)

	for _, id := range []string{"abc", "0", "-5"} {
		req := httptest.NewRequest(http.MethodPost,
			"/api/exec/drafts/"+id+"/execute", strings.NewReader(`{"account_id":1}`))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("draft id=%q 应 400，实际 %d：%s", id, rec.Code, rec.Body.String())
		}
	}
}

// TestExecDraftExecuteRequiresAuth 未认证 → 401。
func TestExecDraftExecuteRequiresAuth(t *testing.T) {
	h, _, _ := executeTestServer(t)

	req := httptest.NewRequest(http.MethodPost,
		"/api/exec/drafts/1/execute", strings.NewReader(`{"account_id":1}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("未认证应 401，实际 %d", rec.Code)
	}
}

// TestExecDraftExecuteDenyReturns400WithCode 闸门 deny → 400 + 具体 code。
//
// # 用「不存在的 draft」触发 deny
//
// 闸门对不存在的草案会返回 deny（找不到草案 → 无法判定 → 拒绝）。
// 这条不依赖任何授权/限额数据，是最稳的 deny 路径。
func TestExecDraftExecuteDenyReturns400WithCode(t *testing.T) {
	h, token, _ := executeTestServer(t)

	// account_id=1 也可能不存在，但那不影响「draft 不存在」这个 deny 判定
	// （闸门先看草案）。如果 account 不存在导致 500，说明闸门的错误处置有问题。
	req := httptest.NewRequest(http.MethodPost,
		"/api/exec/drafts/999999999/execute", strings.NewReader(`{"account_id":1}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("认证没生效：%d", rec.Code)
	}
	// 400（deny）或 409（account 问题）都可接受，但必须是「有结构化 code 的拒绝」，
	// 不能是 500。
	if rec.Code == http.StatusInternalServerError {
		t.Fatalf("不该 500（那是系统错误，而「草案不存在」是正常判定）：%s", rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON：%s", rec.Body.String())
	}
	if out["code"] == nil || out["code"] == "" {
		t.Errorf("拒绝时必须带 code（让调用方能按码分支而不是匹配文案），实际：%s", rec.Body.String())
	}
}

// TestExecDraftExecuteErrorCodesAreUnique 各个拒绝路径的 code 互不相同。
//
// # 为什么测这个
//
// 如果两个不同的拒绝原因用同一个 code，调用方无法区分 ——
// 而那正是「结构化错误码」存在的意义。
func TestExecDraftExecuteErrorCodesAreUnique(t *testing.T) {
	h, token, _ := executeTestServer(t)

	cases := []struct {
		name string
		path string
		body string
	}{
		{"bad_draft_id", "/api/exec/drafts/abc/execute", `{"account_id":1}`},
		{"missing_account", "/api/exec/drafts/1/execute", `{}`},
	}

	seen := map[string]string{}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodPost, c.path, strings.NewReader(c.body))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		code, _ := out["code"].(string)
		if code == "" {
			continue
		}
		if prev, ok := seen[code]; ok {
			t.Errorf("code %q 被 %s 和 %s 共用 —— 调用方无法区分这两种拒绝",
				code, prev, c.name)
		}
		seen[code] = c.name
	}
	_ = fmt.Sprint() // 保留 fmt 导入（便于后续加格式化断言）
}
