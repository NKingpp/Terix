package server

// marketProviders 元数据的自洽性测试。
//
// # 为什么要测「元数据自洽」
//
// providerMeta 里有三处重复描述同一件事，任何一处漏改都会让 UI 和后端校验分叉：
//   - RequiredFields（key 清单，给程序判断）
//   - Fields[].Required（字段自身属性，给前端渲染星号）
//
// 加 purpose 之后又多了一组：marketProviders 里同一个 provider 出现两次。
// 如果两处的 Fields 不一致，用户会在「交易」Tab 看到一套字段、
// 在「数据源」Tab 看到另一套 —— 而错的那套不会有任何报错。

import (
	"testing"
)

// TestMarketProvidersHavePurpose 验证每项都有合法的 purpose。
func TestMarketProvidersHavePurpose(t *testing.T) {
	if len(marketProviders) == 0 {
		t.Fatal("marketProviders 为空")
	}
	for _, p := range marketProviders {
		if p.Purpose != "data" && p.Purpose != "trade" {
			t.Errorf("provider %q 的 purpose = %q，必须是 data 或 trade", p.Provider, p.Purpose)
		}
		if p.Label == "" {
			t.Errorf("provider %q purpose %q 没有 label", p.Provider, p.Purpose)
		}
	}
}

// TestMarketProvidersUniqueProviderPurpose 验证 (provider, purpose) 唯一。
//
// 前端按 provider+purpose 查元数据，重复会让 find 取到第一个。
func TestMarketProvidersUniqueProviderPurpose(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range marketProviders {
		key := p.Provider + "/" + p.Purpose
		if seen[key] {
			t.Errorf("重复的 (provider, purpose) = %s", key)
		}
		seen[key] = true
	}
}

// TestMarketProvidersRequiredFieldsMatchFields 验证 RequiredFields 与 Fields[].Required 一致。
//
// 两者描述同一件事（哪些字段必填），只是用途不同：
//   - RequiredFields：key 清单，后端校验用
//   - Fields[].Required：字段属性，前端渲染星号用
//
// 不一致会让「前端标了星号但后端不校验」或反之。
func TestMarketProvidersRequiredFieldsMatchFields(t *testing.T) {
	for _, p := range marketProviders {
		wantRequired := map[string]bool{}
		for _, f := range p.Fields {
			if f.Required {
				wantRequired[f.Key] = true
			}
		}
		gotRequired := map[string]bool{}
		for _, k := range p.RequiredFields {
			gotRequired[k] = true
		}
		for k := range wantRequired {
			if !gotRequired[k] {
				t.Errorf("%s/%s: 字段 %q 标了 Required 但不在 RequiredFields 里",
					p.Provider, p.Purpose, k)
			}
		}
		for k := range gotRequired {
			if !wantRequired[k] {
				t.Errorf("%s/%s: RequiredFields 含 %q 但该字段没标 Required",
					p.Provider, p.Purpose, k)
			}
		}
	}
}

// TestOkxDataPurposeAllFieldsOptional 验证 OKX 的行情凭据全部可选。
//
// 这是「加 purpose」这条需求的核心设计点之一：公开行情免凭证，
// 填 key 只为提高限频配额。如果这里变成必填，用户会被迫提供一个
// 他不需要的 key 才能保存 —— 而那时「不填」这个合法状态就无法表达。
func TestOkxDataPurposeAllFieldsOptional(t *testing.T) {
	var found bool
	for _, p := range marketProviders {
		if p.Provider != "okx" || p.Purpose != "data" {
			continue
		}
		found = true
		if len(p.RequiredFields) != 0 {
			t.Errorf("okx/data 的 RequiredFields 应为空（公开行情免凭证），实际: %v", p.RequiredFields)
		}
		for _, f := range p.Fields {
			if f.Required {
				t.Errorf("okx/data 的字段 %q 不应标 Required", f.Key)
			}
		}
	}
	if !found {
		t.Fatal("marketProviders 里没有 okx/data 项")
	}
}

// TestOkxTradePurposeRequiresAllSecrets 验证 OKX 的交易凭据三个字段都必填。
func TestOkxTradePurposeRequiresAllSecrets(t *testing.T) {
	var found bool
	for _, p := range marketProviders {
		if p.Provider != "okx" || p.Purpose != "trade" {
			continue
		}
		found = true
		required := map[string]bool{}
		for _, k := range p.RequiredFields {
			required[k] = true
		}
		for _, k := range []string{"api_key", "api_secret", "passphrase"} {
			if !required[k] {
				t.Errorf("okx/trade 的 RequiredFields 缺 %q", k)
			}
		}
	}
	if !found {
		t.Fatal("marketProviders 里没有 okx/trade 项")
	}
}

// TestFosunNotVerifiable 验证 fosun 两种 purpose 都不支持在线验证。
//
// # 为什么这条要测
//
// 缺 fosun OpenAPI 的签名规范（openapi-sdk/ 不存在），所以无法用表里的密钥
// 真的调一次只读接口。而「假装能验证」比「明确说不支持」更糟 ——
// 前者会让用户以为密钥是对的。
func TestFosunNotVerifiable(t *testing.T) {
	for _, p := range marketProviders {
		if p.Provider != "fosun" {
			continue
		}
		if p.Verifiable {
			t.Errorf("fosun/%s 不应标 Verifiable —— 缺 OpenAPI 文档，无法真验证", p.Purpose)
		}
	}
}
