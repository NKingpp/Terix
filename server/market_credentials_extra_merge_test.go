package server

// extra 的**字段级合并**语义测试。
//
// # 为什么要有这组测试
//
// 后端原来对 extra 是「整体替换」：传 {} 清空全部，传 {a:"x"} 也清空全部再把
// a 设成 x（因为 SQL 是 COALESCE($::jsonb, extra)）。
//
// 那让「清空 a 保留 b」无法表达 —— 用户只能两个一起清。而现在 fosun 只有一个
// extra 字段时看不出问题，等第二个字段出现时这个 bug 才会暴露，
// **而那时没人记得这个决定**。
//
// 所以这四条测试覆盖完整语义矩阵，不只是「能存能读」：
//   nil      → 保留
//   {}       → 全清
//   {a:"X"}  → 只改 a，其余保留
//   {a:""}   → 只删 a，其余保留

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// createFosunWithExtra 建一条带 extra 的 fosun 凭证，返回其 id。
func createFosunWithExtra(t *testing.T, s *Server, label string, extra map[string]string) int64 {
	t.Helper()
	rec := postJSON(t, s.saveMarketCredential, "/api/market/credentials", map[string]any{
		"provider": "fosun", "account_label": label,
		"api_key": "-----PEM-PUB-----", "api_secret": "-----PEM-PRIV-----",
		"extra": extra,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var c db.MarketCredential
	if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil {
		t.Fatalf("解析创建响应失败: %v", err)
	}
	return c.ID
}

// TestMarketCredentialExtraMergeKeepsOthers 改一个 key，其余保留。
func TestMarketCredentialExtraMergeKeepsOthers(t *testing.T) {
	s, done := credTestServer(t)
	defer done()
	pg := s.m.pg
	label := uniqueLabel(t)
	defer cleanupLabelCreds(t, s, label)

	id := createFosunWithExtra(t, s, label, map[string]string{"a": "1", "b": "2"})
	defer func() { _ = pg.DeleteMarketCredential(id) }()

	rec := postJSON(t, s.saveMarketCredential, "/api/market/credentials", map[string]any{
		"provider": "fosun", "account_label": label,
		"extra": map[string]string{"a": "X"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
	var after db.MarketCredential
	_ = json.Unmarshal(rec.Body.Bytes(), &after)

	if after.Extra["a"] != "X" {
		t.Errorf("a 应改成 X，实际 %q", after.Extra["a"])
	}
	if after.Extra["b"] != "2" {
		t.Errorf("b 应保留 2，实际 %q（整体替换会丢掉它）", after.Extra["b"])
	}
}

// TestMarketCredentialExtraFieldClear 传空串 = 删除该 key，其余保留。
//
// 这是本次改动的核心 —— 原来做不到「清空 a 保留 b」。
func TestMarketCredentialExtraFieldClear(t *testing.T) {
	s, done := credTestServer(t)
	defer done()
	pg := s.m.pg
	label := uniqueLabel(t)
	defer cleanupLabelCreds(t, s, label)

	id := createFosunWithExtra(t, s, label, map[string]string{"a": "1", "b": "2"})
	defer func() { _ = pg.DeleteMarketCredential(id) }()

	rec := postJSON(t, s.saveMarketCredential, "/api/market/credentials", map[string]any{
		"provider": "fosun", "account_label": label,
		"extra": map[string]string{"a": ""},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
	var after db.MarketCredential
	_ = json.Unmarshal(rec.Body.Bytes(), &after)

	if _, exists := after.Extra["a"]; exists {
		t.Errorf("a 应被删除（空串 = 清空该字段），实际仍在: %v", after.Extra)
	}
	if after.Extra["b"] != "2" {
		t.Errorf("b 应保留 2，实际 %v", after.Extra)
	}
}

// TestMarketCredentialExtraNilKeeps 不传 extra = 完全不动。
func TestMarketCredentialExtraNilKeeps(t *testing.T) {
	s, done := credTestServer(t)
	defer done()
	pg := s.m.pg
	label := uniqueLabel(t)
	defer cleanupLabelCreds(t, s, label)

	id := createFosunWithExtra(t, s, label, map[string]string{"a": "1"})
	defer func() { _ = pg.DeleteMarketCredential(id) }()

	// 只改 base_url，不传 extra
	rec := postJSON(t, s.saveMarketCredential, "/api/market/credentials", map[string]any{
		"provider": "fosun", "account_label": label,
		"base_url": "https://changed.test",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
	var after db.MarketCredential
	_ = json.Unmarshal(rec.Body.Bytes(), &after)

	if after.Extra["a"] != "1" {
		t.Errorf("不传 extra 时旧值应保留，实际 %v", after.Extra)
	}
	if after.BaseURL != "https://changed.test" {
		t.Errorf("base_url 应更新，实际 %q", after.BaseURL)
	}
}

// TestMarketCredentialExtraEmptyMapClearsAll 传 {} = 清空全部。
func TestMarketCredentialExtraEmptyMapClearsAll(t *testing.T) {
	s, done := credTestServer(t)
	defer done()
	pg := s.m.pg
	label := uniqueLabel(t)
	defer cleanupLabelCreds(t, s, label)

	id := createFosunWithExtra(t, s, label, map[string]string{"a": "1", "b": "2"})
	defer func() { _ = pg.DeleteMarketCredential(id) }()

	rec := postJSON(t, s.saveMarketCredential, "/api/market/credentials", map[string]any{
		"provider": "fosun", "account_label": label,
		"extra": map[string]string{},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
	var after db.MarketCredential
	_ = json.Unmarshal(rec.Body.Bytes(), &after)

	if len(after.Extra) != 0 {
		t.Errorf("传空 map 应清空全部，实际 %v", after.Extra)
	}
}

// TestMarketCredentialExtraMergeByIDUpdate 按 id 更新时也是字段级合并。
//
// 两条路径（Upsert / UpdateByID）各有独立的 SQL，容易只改一条漏另一条。
func TestMarketCredentialExtraMergeByIDUpdate(t *testing.T) {
	s, done := credTestServer(t)
	defer done()
	pg := s.m.pg
	label := uniqueLabel(t)
	defer cleanupLabelCreds(t, s, label)

	id := createFosunWithExtra(t, s, label, map[string]string{"a": "1", "b": "2"})
	defer func() { _ = pg.DeleteMarketCredential(id) }()

	rec := postJSON(t, s.saveMarketCredential, "/api/market/credentials", map[string]any{
		"id": id, "provider": "fosun", "account_label": label,
		"extra": map[string]string{"b": ""}, // 按 id 路径清 b
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("update by id: %d %s", rec.Code, rec.Body.String())
	}
	var after db.MarketCredential
	_ = json.Unmarshal(rec.Body.Bytes(), &after)

	if _, exists := after.Extra["b"]; exists {
		t.Errorf("b 应被删除，实际 %v", after.Extra)
	}
	if after.Extra["a"] != "1" {
		t.Errorf("a 应保留 1，实际 %v", after.Extra)
	}
}
