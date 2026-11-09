package server

// 妙想凭证热更新的验收测试。
//
// # 这组测试守的是什么
//
// 2026-11-09 线上问题：用户在凭证页新建了 mx key，验证通过、页面显示已保存，
// 但 agent 一直报「mx 通道未启用：MX_APIKEY 未配置」，只有重启容器才好。
//
// 根因：loadMXAPIKey → mts.SetMXClient 只在 server.New() 跑一次。保存凭证的
// handler 没回头刷 MarketToolSet 里的 client。
//
// 所以这里钉住：保存**之后**同一个 MarketToolSet 实例必须立刻可用，
// 删除之后必须立刻失效。

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/Autumn-27/artex/agent"
)

// mxReady 就是 agent 视角的「妙想能不能用」（domainReg 装配时用的同一个判断）。
func mxReady(s *Server) bool { return s.mts != nil && s.mts.AvailableSourceTool("quote_mx") }

// TestSaveMXCredentialHotInjects 保存 mx 凭证后，运行中的工具集立刻可用。
func TestSaveMXCredentialHotInjects(t *testing.T) {
	s, _, request := newAuthServer(t)
	const label = "hot-mx"
	cleanup := func() {
		_, _ = s.m.pg.Exec(`DELETE FROM market_credentials WHERE provider='mx' AND account_label=$1`, label)
	}
	cleanup()
	t.Cleanup(cleanup)

	// 前置：没有 key 时不可用。把它当基线，否则后面的「可用」可能只是
	// 别的测试漏进来的 key 造成的假通过。
	if mxReady(s) {
		t.Fatal("前置不成立：无 mx 凭证时不该可用")
	}

	w := request("POST", "/api/market/credentials", `{"provider":"mx","kind":"data","account_label":"`+label+`","auth_token":"mx-hot-inject-key"}`)
	if w.Code != 200 {
		t.Fatalf("保存凭证: %d %s", w.Code, w.Body.String())
	}

	// 关键断言：**不重启**，同一个工具集实例就该拿到这把 key。
	if !mxReady(s) {
		t.Fatal("保存后仍未启用 —— 正是「配了 key 却要重启」那个 bug")
	}
}

// TestDeleteMXCredentialDisables 删除最后一条 mx 凭证后立刻失效。
//
// 不刷新的话旧 client 会继续用已删除的 key，报错指向远端而非「key 没了」。
func TestDeleteMXCredentialDisables(t *testing.T) {
	s, _, request := newAuthServer(t)
	const label = "hot-mx-del"
	cleanup := func() {
		_, _ = s.m.pg.Exec(`DELETE FROM market_credentials WHERE provider='mx' AND account_label=$1`, label)
	}
	cleanup()
	t.Cleanup(cleanup)

	w := request("POST", "/api/market/credentials", `{"provider":"mx","kind":"data","account_label":"`+label+`","auth_token":"mx-del-key"}`)
	if w.Code != 200 {
		t.Fatalf("保存凭证: %d %s", w.Code, w.Body.String())
	}
	var saved struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil || saved.ID == 0 {
		t.Fatalf("解析凭证 id: %v %s", err, w.Body.String())
	}
	if !mxReady(s) {
		t.Fatal("保存后应可用")
	}

	w = request("DELETE", "/api/market/credentials/"+strconv.FormatInt(saved.ID, 10), "")
	if w.Code != 200 {
		t.Fatalf("删除凭证: %d %s", w.Code, w.Body.String())
	}
	if mxReady(s) {
		t.Fatal("删除后仍可用 —— 旧 key 会继续被用")
	}
}

// TestMXClientConcurrentSet 并发 set/read 不 panic（竞态由 -race 抓）。
func TestMXClientConcurrentSet(t *testing.T) {
	m := agent.NewMarketToolSet(nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			m.SetMXClient(agent.NewMXClient("k"))
			m.SetMXClient(nil)
		}
	}()
	for i := 0; i < 200; i++ {
		_ = m.AvailableSourceTool("quote_mx")
	}
	<-done
}

// TestOkxCredentialClearedAfterDelete 删掉最后一条 okx/trade 凭证后，
// 内存里那把 key 必须被清掉。
//
// 不清的后果：CLI 继续用已删除的 key，报 50119「API key doesn't exist」，
// 排查时会以为 key 填错了，而不是「本地缓存还留着旧的」。
func TestOkxCredentialClearedAfterDelete(t *testing.T) {
	s, _, request := newAuthServer(t)
	const label = "hot-okx-del"
	cleanup := func() {
		_, _ = s.m.pg.Exec(`DELETE FROM market_credentials WHERE provider='okx' AND account_label=$1`, label)
	}
	cleanup()
	t.Cleanup(cleanup)

	w := request("POST", "/api/market/credentials",
		`{"provider":"okx","kind":"trade","account_label":"`+label+`","api_key":"okx-del-key","api_secret":"s","passphrase":"p"}`)
	if w.Code != 200 {
		t.Fatalf("保存凭证: %d %s", w.Code, w.Body.String())
	}
	var saved struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil || saved.ID == 0 {
		t.Fatalf("解析凭证 id: %v %s", err, w.Body.String())
	}
	if c := s.okxCli.CredentialForTest(); c == nil || c.APIKey != "okx-del-key" {
		t.Fatalf("保存后应注入该 key，实际 %+v", c)
	}

	w = request("DELETE", "/api/market/credentials/"+strconv.FormatInt(saved.ID, 10), "")
	if w.Code != 200 {
		t.Fatalf("删除凭证: %d %s", w.Code, w.Body.String())
	}
	if c := s.okxCli.CredentialForTest(); c != nil {
		t.Fatalf("删除后仍注入着 %q —— CLI 会继续用已删除的 key", c.APIKey)
	}
}
