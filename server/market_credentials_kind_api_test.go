package server

// market_credentials 的 kind API 测试。
//
// # 为什么必须在 API 层再测一次
//
// db 层的测试证明了「同 provider 同 label 不同 kind 能存两条」。
// 但那不覆盖两件事：
//  ① HTTP 层的 kind 字段有没有真的从 JSON 透传到 db（漏了就永远存 trade）
//  ② ?kind= 过滤是否生效
//
// 而 ① 是这个改动最容易漏的地方 —— db 层全绿但 handler 没接 body.Kind，
// 那所有凭据都会变成 trade，而**没有任何测试会红**。

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// credAPITestPrefix 是本文件所有测试凭据的 label 前缀。
//
// ⚠️ 清理只能删带这个前缀的行。
//
// # 2026-11-05 事故
//
// 原实现是 `DELETE FROM market_credentials`（全表）。这张表存用户的
// **真实交易密钥**（OKX / 复星）。跑一次 `go test ./server` 就把它们删光了，
// 用户只看到「开发完凭据没了」，没有任何日志说明是测试干的。
//
// 现在按 label 前缀隔离：真实数据的 label 是「1」「主账户」这类，
// 永远不会以 cred-api-test- 开头。
const credAPITestPrefix = "cred-api-test-"

// credentialCleanup 清掉本文件造的测试凭据并注册清理。
func credentialCleanup(t *testing.T, s *Server) {
	t.Helper()
	cleans := func() {
		_, _ = s.m.pg.Exec(`DELETE FROM market_credentials WHERE account_label LIKE $1`,
			credAPITestPrefix+"%")
	}
	cleans()
	t.Cleanup(cleans)
}

// TestSaveCredentialPassesKindThrough 验证 HTTP 层的 kind 真的透传到 db。
//
// 这是本改动最易漏的一环：db 层做对了，但 handler 没接 body.Kind ——
// 那时所有凭据都会落成 trade（DEFAULT），而 db 层测试全绿。
func TestSaveCredentialPassesKindThrough(t *testing.T) {
	s, _, request := newAuthServer(t)
	credentialCleanup(t, s)

	w := request("POST", "/api/market/credentials",
		`{"provider":"okx","kind":"data","account_label":"cred-api-test-行情账户","api_key":"k1"}`)
	if w.Code != 200 {
		t.Fatalf("保存 data 凭据: %d %s", w.Code, w.Body.String())
	}
	var saved struct {
		ID   int64  `json:"id"`
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil {
		t.Fatalf("解析响应: %v（body=%s）", err, w.Body.String())
	}
	if saved.Kind != "data" {
		t.Fatalf("kind 应为 data，实际 %q —— handler 可能没把 body.Kind 传给 db", saved.Kind)
	}

	// 再查一次库（确认不是响应体造假）
	var dbKind string
	if err := s.m.pg.QueryRow(`SELECT kind FROM market_credentials WHERE id=$1`, saved.ID).Scan(&dbKind); err != nil {
		t.Fatal(err)
	}
	if dbKind != "data" {
		t.Fatalf("库里 kind 应为 data，实际 %q", dbKind)
	}
}

// TestSaveTwoKindsSameLabel 验证同 provider 同 label 的两个 kind 能各自保存。
func TestSaveTwoKindsSameLabel(t *testing.T) {
	s, _, request := newAuthServer(t)
	credentialCleanup(t, s)

	w1 := request("POST", "/api/market/credentials",
		`{"provider":"okx","kind":"trade","account_label":"cred-api-test-主账户","api_key":"k","api_secret":"s","passphrase":"p"}`)
	if w1.Code != 200 {
		t.Fatalf("保存 trade: %d %s", w1.Code, w1.Body.String())
	}
	w2 := request("POST", "/api/market/credentials",
		`{"provider":"okx","kind":"data","account_label":"cred-api-test-主账户","api_key":"k2"}`)
	if w2.Code != 200 {
		t.Fatalf("保存 data（同 label）: %d %s", w2.Code, w2.Body.String())
	}

	var n int
	if err := s.m.pg.QueryRow(`SELECT count(*) FROM market_credentials`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("应有 2 条，实际 %d 条", n)
	}
}

// TestListCredentialsFilterByKind 验证 ?kind= 过滤。
func TestListCredentialsFilterByKind(t *testing.T) {
	s, _, request := newAuthServer(t)
	credentialCleanup(t, s)

	request("POST", "/api/market/credentials",
		`{"provider":"okx","kind":"trade","account_label":"cred-api-test-t","api_key":"k","api_secret":"s","passphrase":"p"}`)
	request("POST", "/api/market/credentials",
		`{"provider":"okx","kind":"data","account_label":"cred-api-test-d","api_key":"k2"}`)

	all := request("GET", "/api/market/credentials", "")
	dataOnly := request("GET", "/api/market/credentials?kind=data", "")
	tradeOnly := request("GET", "/api/market/credentials?kind=trade", "")
	for _, w := range []struct {
		name string
		code int
	}{{"all", all.Code}, {"data", dataOnly.Code}, {"trade", tradeOnly.Code}} {
		if w.code != 200 {
			t.Fatalf("%s 状态码 %d", w.name, w.code)
		}
	}

	// 响应是 {credentials, meta} 对象（2026-10-31 起的契约，见
	// market_credentials_resp_test.go 的说明）。原先这里按裸数组解析，
	// 接口形状改了之后这条就成了「测试假设过期」而非「实现坏了」。
	type credList struct {
		Credentials []struct {
			Kind string `json:"kind"`
		} `json:"credentials"`
	}
	parse := func(body []byte) []struct {
		Kind string `json:"kind"`
	} {
		var v credList
		if err := json.Unmarshal(body, &v); err != nil {
			t.Fatal(err)
		}
		return v.Credentials
	}
	allList := parse(all.Body.Bytes())
	dataList := parse(dataOnly.Body.Bytes())
	tradeList := parse(tradeOnly.Body.Bytes())

	if len(allList) != 2 {
		t.Fatalf("全部应有 2 条，实际 %d", len(allList))
	}
	if len(dataList) != 1 || dataList[0].Kind != "data" {
		t.Fatalf("?kind=data 应返回 1 条 data，实际 %+v", dataList)
	}
	if len(tradeList) != 1 || tradeList[0].Kind != "trade" {
		t.Fatalf("?kind=trade 应返回 1 条 trade，实际 %+v", tradeList)
	}
}

// TestSaveRejectsKindChange 验证按 id 更新时不能改 kind，且返回明确的错误码。
func TestSaveRejectsKindChange(t *testing.T) {
	s, _, request := newAuthServer(t)
	credentialCleanup(t, s)

	w := request("POST", "/api/market/credentials",
		`{"provider":"okx","kind":"trade","account_label":"cred-api-test-acc","api_key":"k","api_secret":"s","passphrase":"p"}`)
	if w.Code != 200 {
		t.Fatalf("建 trade: %d %s", w.Code, w.Body.String())
	}
	var saved struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}

	// 带 id 更新，把 kind 改成 data
	w2 := request("POST", "/api/market/credentials", fmt.Sprintf(
		`{"id":%d,"provider":"okx","kind":"data","account_label":"cred-api-test-acc"}`, saved.ID))
	if w2.Code != 400 {
		t.Fatalf("改 kind 应返回 400，实际 %d %s", w2.Code, w2.Body.String())
	}
	if !strings.Contains(w2.Body.String(), "KIND_IMMUTABLE") {
		t.Fatalf("应返回 KIND_IMMUTABLE 错误码，实际: %s", w2.Body.String())
	}
}

// TestListRejectsInvalidKindFilter 验证非法的 ?kind= 被拒而不是静默返回空。
func TestListRejectsInvalidKindFilter(t *testing.T) {
	s, _, request := newAuthServer(t)
	credentialCleanup(t, s)

	w := request("GET", "/api/market/credentials?kind=bogus", "")
	if w.Code != 400 {
		t.Fatalf("非法 kind 过滤应返回 400，实际 %d %s", w.Code, w.Body.String())
	}
}
