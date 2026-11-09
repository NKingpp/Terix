package server

// `GET /api/market/credentials` 的**响应形状**验收测试。
//
// # 这组测试守的是什么
//
// 2026-10-31 实测：凭证页的「新增凭据」按钮永远是灰的，渠道下拉是空的。
//
// 根因是两侧契约在同一次提交里被改得不一致：
//   - 后端 `listMarketCredentials` 返回裸数组 `[]`
//   - 前端 `api.marketCredentials()` 按 `{credentials, meta:{providers}}` 解析
//
// 于是 `r.meta?.providers` 恒为 undefined → `providers` 数组为空 →
// `openAdd` 的 `disabled={providers.length === 0}` 永远成立。
//
// **没有任何测试跨这句话的边界** —— 后端测试查「返回了几条凭据」，
// 前端测试查「给定 providers 怎么渲染」，而「后端到底给的那个对象里
// 有没有 meta」这层谁都没测。
//
// 所以这里测的是**响应体的字段路径本身**，不是「函数返回了数据」。
// 那种测试在接口形状改变时不会红。

import (
	"encoding/json"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// TestListMarketCredentialsResponseShape 断言响应是可被前端解析的对象。
//
// 三个必须存在的路径：
//   - `.credentials` 数组（哪怕是空的）
//   - `.meta.providers` 数组
//   - `.meta.providers[].purpose`（前端按 provider+purpose 复合键查找）
func TestListMarketCredentialsResponseShape(t *testing.T) {
	_, _, request := newAuthServer(t)

	w := request("GET", "/api/market/credentials", "")
	if w.Code != 200 {
		t.Fatalf("GET /api/market/credentials = %d: %s", w.Code, w.Body.String())
	}

	// ① 顶层必须是对象，不能是裸数组。
	//
	// 用 json.Unmarshal 到 any 再判类型：直接 Unmarshal 到 struct 的话，
	// 裸数组会报错 —— 那也算抓到，但错误信息是「cannot unmarshal array」，
	// 看不出「我们期望的是对象」。这里显式判一次，让失败信息能自己解释。
	var raw any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("响应顶层是 %T，前端期望对象 {credentials, meta}。body=%s",
			raw, truncateForTest(w.Body.String()))
	}

	// ② credentials 必须存在且是数组
	creds, ok := obj["credentials"]
	if !ok {
		t.Fatalf("响应缺 credentials 字段: %s", truncateForTest(w.Body.String()))
	}
	if _, ok := creds.([]any); !ok {
		t.Fatalf("credentials 是 %T，期望数组", creds)
	}

	// ③ meta.providers —— 这条是本次 bug 的直接原因
	meta, ok := obj["meta"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺 meta 对象（前端读 r.meta?.providers，缺了就永远拿到 undefined）: %s",
			truncateForTest(w.Body.String()))
	}
	providers, ok := meta["providers"].([]any)
	if !ok {
		t.Fatalf("meta 缺 providers 数组: %s", truncateForTest(w.Body.String()))
	}
	if len(providers) == 0 {
		t.Fatal("meta.providers 是空数组 —— 渠道下拉会没有可选项，用户无法新增凭据")
	}

	// ④ 每一项都必须有 provider + purpose（前端用复合键查找）
	for i, p := range providers {
		m, ok := p.(map[string]any)
		if !ok {
			t.Fatalf("providers[%d] 不是对象", i)
		}
		if _, ok := m["provider"].(string); !ok {
			t.Errorf("providers[%d] 缺 provider 字符串", i)
		}
		if _, ok := m["purpose"].(string); !ok {
			t.Errorf("providers[%d] 缺 purpose 字符串 —— 前端按 (provider,purpose) 复合键查找", i)
		}
		// fields 必须存在：前端 buildFieldSpecs 按它渲染表单。
		// 缺了会退化成「只渲染 secret_fields」，而 6 家数据源的 auth_token
		// 虽然是 secret，但 endpoint 不是 —— 那栏会消失。
		if _, ok := m["fields"].([]any); !ok {
			t.Errorf("providers[%d] (%v) 缺 fields 数组", i, m["provider"])
		}
	}

	// ⑤ 六家数据源必须在列表里（本轮的产出）
	wantProviders := []string{"ifind", "fosun_data", "tdx", "neodata", "kuaicha", "westock"}
	got := map[string]bool{}
	for _, p := range providers {
		m := p.(map[string]any)
		if name, _ := m["provider"].(string); name != "" {
			got[name] = true
		}
	}
	for _, want := range wantProviders {
		if !got[want] {
			t.Errorf("meta.providers 缺 %q —— 该数据源无法在凭证页配置", want)
		}
	}
}

// TestListMarketCredentialsIncludesSavedRows 建的凭据必须出现在列表里。
//
// 与上一条互补：上一条只测形状（列表可以是空的），这一条测「数据和形状同时对」。
// 只测形状会漏掉「返回了正确的空对象」这种实现 —— 那比裸数组更难发现，
// 因为前端不报错、只是永远看不到已保存的凭据。
func TestListMarketCredentialsIncludesSavedRows(t *testing.T) {
	s, _, request := newAuthServer(t)

	// 直接写库（绕过 handler），只为验证读路径
	if _, err := s.m.pg.UpsertMarketCredential(db.MarketCredentialInput{
		Provider: "tdx", Kind: "data", AccountLabel: "shape-test",
	}); err != nil {
		t.Fatalf("建测试凭据: %v", err)
	}
	t.Cleanup(func() {
		if _, err := s.m.pg.Exec(
			`DELETE FROM market_credentials WHERE account_label='shape-test'`); err != nil {
			t.Errorf("清理测试凭据: %v", err)
		}
	})

	w := request("GET", "/api/market/credentials", "")
	if w.Code != 200 {
		t.Fatalf("GET = %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Credentials []struct {
			Provider     string `json:"provider"`
			AccountLabel string `json:"account_label"`
			Kind         string `json:"kind"`
		} `json:"credentials"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	found := false
	for _, c := range resp.Credentials {
		if c.AccountLabel == "shape-test" {
			found = true
			if c.Provider != "tdx" {
				t.Errorf("provider = %q, want tdx", c.Provider)
			}
			if c.Kind != "data" {
				t.Errorf("kind = %q, want data", c.Kind)
			}
		}
	}
	if !found {
		t.Fatalf("刚建的 shape-test 没出现在 credentials 里（共 %d 条）", len(resp.Credentials))
	}
}

// truncateForTest 截断响应体用于报错信息。
//
// 不直接打印全文：凭据列表里可能有 api_key_hint（脱敏后的前缀），
// 虽然不含明文，但失败信息会进 CI 日志，没必要带上一整页。
func truncateForTest(s string) string {
	const max = 500
	if len(s) <= max {
		return s
	}
	return s[:max] + "…(截断)"
}
