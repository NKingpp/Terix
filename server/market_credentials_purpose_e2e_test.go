package server

// 凭据 kind 的端到端行为验证（对应 Step 4e 的手动验证清单 ③④⑤）。
//
// # 为什么要把它写成测试而不是「手动点一遍」
//
// 因为手动验证的三条结论（「保存成功了」「布局对了」）在下次改动后不会被重跑，
// 而它们恰好覆盖了「RequiredFields 为空」与「metaFor 带 purpose」这两个易错点。

import (
	"encoding/json"
	"fmt"
	"testing"
)

// TestDataPurposeCredentialSavesWithEmptyFields 对应手动验证 ③：
// 数据源 Tab 保存一条 OKX 凭据，字段全空只填 label —— 应该成功。
//
// 因为 okx/data 的 RequiredFields 是空的（公开行情免凭证）。
func TestDataPurposeCredentialSavesWithEmptyFields(t *testing.T) {
	s, _, request := newAuthServer(t)
	// ⚠️ 清理**必须按 label 限定**，绝不能 `DELETE FROM market_credentials`。
	//
	// 2026-11-02 事故：这里原来是无条件清空全表，把用户真实的 OKX 交易凭证
	// 删掉了。测试跑在**生产同库**上（ARTEX_PG_DSN 指向 artex），
	// 一次 `go test ./server` 就能抹掉用户配置。
	//
	// 判据：测试只该删自己创建的行。用唯一的 label 前缀标识。
	cleanCreds := func() {
		_, _ = s.m.pg.Exec(`DELETE FROM market_credentials WHERE account_label LIKE 'test-empty-%'`)
	}
	cleanCreds()
	t.Cleanup(cleanCreds)

	// 只有 provider/kind/label，三个敏感字段全空
	w := request("POST", "/api/market/credentials",
		`{"provider":"okx","kind":"data","account_label":"test-empty-行情"}`)
	if w.Code != 200 {
		t.Fatalf("okx/data 应允许空字段保存，实际 %d %s", w.Code, w.Body.String())
	}
}

// TestTradeAndDataSameLabelBothSave 对应手动验证 ④：
// 同 label 在交易通道和数据结构各存一条 —— 都应成功。
func TestTradeAndDataSameLabelBothSave(t *testing.T) {
	s, _, request := newAuthServer(t)
	// 同上：只删自己创建的行，不用无条件清空。
	cleanCreds := func() {
		_, _ = s.m.pg.Exec(`DELETE FROM market_credentials WHERE account_label LIKE 'test-samelabel-%'`)
	}
	cleanCreds()
	t.Cleanup(cleanCreds)

	w1 := request("POST", "/api/market/credentials",
		`{"provider":"okx","kind":"trade","account_label":"test-samelabel-同一标签","api_key":"k","api_secret":"s","passphrase":"p"}`)
	if w1.Code != 200 {
		t.Fatalf("trade 保存: %d %s", w1.Code, w1.Body.String())
	}
	w2 := request("POST", "/api/market/credentials",
		`{"provider":"okx","kind":"data","account_label":"test-samelabel-同一标签"}`)
	if w2.Code != 200 {
		t.Fatalf("data 保存（同 label）: %d %s", w2.Code, w2.Body.String())
	}

	// 只统计本测试创建的行 —— 表里可能还有用户真实凭证。
	//
	// 原来的写法是 `SELECT count(*) FROM market_credentials` 断言 ==2，
	// 一旦库里有真实凭证就必然失败，而且失败信息（"应有 2 条，实际 3"）
	// 完全指不出问题在测试还是实现 —— 这正是它实际发生时的表现。
	var n int
	if err := s.m.pg.QueryRow(
		`SELECT count(*) FROM market_credentials WHERE account_label LIKE 'test-samelabel-%'`,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("本测试应有 2 条（trade + data 同 label），实际 %d", n)
	}
}

// TestMetaLookupByPurposeReturnsDistinctFields 对应手动验证 ⑤：
// 按 provider+purpose 取 meta，trade 与 data 应返回**不同的字段集合**。
//
// # 为什么这条最关键
//
// 如果前端 lookup 只按 provider（不带 purpose），它会取到数组里第一个
// （trade 版），于是编辑一条 data 凭据时显示的是 trade 的字段布局
// （passphrase 标了必填星号）。
//
// 那个错误不报错、不崩 —— 只是「表单显示的字段不对」。
// 所以必须断言「两个 purpose 的 required 集合不同」。
func TestMetaLookupByPurposeReturnsDistinctFields(t *testing.T) {
	_, _, request := newAuthServer(t)

	w := request("GET", "/api/market/credentials/meta", "")
	if w.Code != 200 {
		t.Fatalf("meta: %d %s", w.Code, w.Body.String())
	}
	var body struct {
		Providers []struct {
			Provider       string   `json:"provider"`
			Purpose        string   `json:"purpose"`
			RequiredFields []string `json:"required_fields"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}

	pick := func(provider, purpose string) []string {
		for _, p := range body.Providers {
			if p.Provider == provider && p.Purpose == purpose {
				return p.RequiredFields
			}
		}
		return nil
	}

	trade := pick("okx", "trade")
	data := pick("okx", "data")
	if trade == nil || data == nil {
		t.Fatalf("缺 okx 的 trade 或 data 项: trade=%v data=%v", trade, data)
	}
	if len(data) != 0 {
		t.Errorf("okx/data 的 RequiredFields 应为空，实际 %v", data)
	}
	if len(trade) != 3 {
		t.Errorf("okx/trade 的 RequiredFields 应有 3 项，实际 %v", trade)
	}
	// 核心断言：两者不同。相同时说明按 purpose 区分没生效。
	if fmt.Sprint(trade) == fmt.Sprint(data) {
		t.Fatal("okx 的 trade 与 data 的 required 集合相同 —— 按 purpose 区分没生效")
	}
}
