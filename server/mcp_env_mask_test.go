package server

// MCP env 脱敏的验收测试。
//
// # 这组测试守的是什么
//
// 2026-10-31 实测发现：行情凭证同步到 MCP 后，`env` 里存的是
// `{"Authorization":"Bearer <真实 token>"}`，而 `GET /api/mcp` 把它
// **原样返回**。任何持有登录态的人都能读到数据源密钥 ——
// 绕开了凭证页「只回显 hint」的脱敏设计。
//
// 这类泄漏的失败方式是「没有人会报错，因为每一步都成功了」：
// 保存凭证成功、同步成功、列表接口 200 —— 只有人肉看返回值才发现。
//
// 所以这里逐字段守住：敏感的遮掉，非敏感的保留。

import (
	"encoding/json"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// TestMaskMCPEnvHidesAuthorization Authorization 必须被遮掉。
//
// 这是最要紧的一条：行情凭证同步的正是这个 header。
func TestMaskMCPEnvHidesAuthorization(t *testing.T) {
	raw := json.RawMessage(`{"Authorization":"Bearer sk-live-secret-value"}`)
	out := maskMCPEnv(raw)

	var got map[string]string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("脱敏后不是合法 JSON: %v（原始: %s）", err, out)
	}
	if v, ok := got["Authorization"]; !ok {
		t.Fatal("Authorization 这个 key 不该消失 —— 用户需要看到「配过」")
	} else if v == "Bearer sk-live-secret-value" {
		t.Fatalf("Authorization 的明文被原样返回: %q", v)
	}
	// 原始值不能以任何形式出现在输出里
	if bytesContain(out, "sk-live-secret-value") {
		t.Errorf("输出里仍含原始 token: %s", out)
	}
}

// TestMaskMCPEnvKeepsNonSensitiveKeys 非敏感 key 保留原值。
//
// # 为什么这条也重要
//
// 如果一律遮成 `***`，用户看不出自己配的 `LOG_LEVEL=debug` 到底是什么，
// 排查 MCP 连不上时会失去线索。所以脱敏必须**按 key 名判断**，不是一刀切。
func TestMaskMCPEnvKeepsNonSensitiveKeys(t *testing.T) {
	raw := json.RawMessage(`{"LOG_LEVEL":"debug","NODE_ENV":"production"}`)
	out := maskMCPEnv(raw)

	var got map[string]string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("脱敏后不是合法 JSON: %v", err)
	}
	for k, want := range map[string]string{"LOG_LEVEL": "debug", "NODE_ENV": "production"} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q（非敏感 key 不该被遮）", k, got[k], want)
		}
	}
}

// TestMaskMCPEnvHandlesMalformed 畸形输入整体遮空，不回显原文。
//
// # 为什么这条是安全要求而不是健壮性
//
// 解析失败时若 `return raw`，那段「解析不了的内容」很可能正是
// 敏感串的某种畸形形态（比如未闭合引号）—— 回显等于泄漏。
// 所以失败路径必须返回空对象。
func TestMaskMCPEnvHandlesMalformed(t *testing.T) {
	cases := []json.RawMessage{
		json.RawMessage(`{"Authorization":"Bearer leaked-token"`), // 未闭合
		json.RawMessage(`not-json-leaked-token`),
		json.RawMessage(`["leaked-token"]`), // 数组不是 map
	}
	for _, raw := range cases {
		out := maskMCPEnv(raw)
		if bytesContain(out, "leaked-token") {
			t.Errorf("畸形输入 %s 的输出仍含原始内容: %s", raw, out)
		}
	}
}

// TestMaskMCPEnvEmpty 空输入原样返回（不产生 `{}` 噪音）。
func TestMaskMCPEnvEmpty(t *testing.T) {
	if out := maskMCPEnv(nil); len(out) != 0 {
		t.Errorf("nil 输入应原样返回，实际: %s", out)
	}
}

// bytesContain 是字节级子串判断。
//
// 不用 strings.Contains 是因为 out 是 json.RawMessage（[]byte），
// 转成 string 只为比较会让人误以为这里有字符串处理逻辑。
//
// ⚠️ 名字不能叫 contains —— server 包里已有一个同名的 []string 辅助函数
// （assembly.go:316 附近），重名会编译失败。
func bytesContain(b []byte, sub string) bool {
	if len(sub) == 0 || len(b) < len(sub) {
		return false
	}
	for i := 0; i+len(sub) <= len(b); i++ {
		if string(b[i:i+len(sub)]) == sub {
			return true
		}
	}
	return false
}

// TestListMCPHandlerMasksEnv 走真实 HTTP handler，断言列表接口不泄漏。
//
// # 为什么必须有这一条（而不是只测 maskMCPEnv）
//
// 变异验证发现的盲区：把 pgListMCP 里那三行脱敏循环**整段删掉**后，
// TestMaskMCPEnvHidesAuthorization 仍然是绿的 —— 因为它只测函数本身，
// 从没验证「handler 到底调没调这个函数」。
//
// 「函数是安全的」与「调用方用了那个安全函数」是两件事。
// 这层脱敏的价值全在调用点上，所以必须测调用点。
func TestListMCPHandlerMasksEnv(t *testing.T) {
	s, _, request := newAuthServer(t)

	// 建一个带凭据 header 的 MCP 行（模拟行情凭证同步后的状态）
	rawEnv := json.RawMessage(`{"Authorization":"Bearer super-secret-token-xyz"}`)
	if _, err := s.m.pg.SaveMCP(&db.MCPServer{
		Name: "mask-e2e", Transport: "http",
		URL: "https://example.invalid/mcp", Env: rawEnv, Enabled: true,
	}); err != nil {
		t.Fatalf("建 MCP 行: %v", err)
	}
	t.Cleanup(func() {
		if _, err := s.m.pg.Exec(`DELETE FROM mcp_servers WHERE name='mask-e2e'`); err != nil {
			t.Errorf("清理 MCP 行: %v", err)
		}
	})

	w := request("GET", "/api/mcp", "")
	if w.Code != 200 {
		t.Fatalf("GET /api/mcp = %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// ① 原始 token 不能在响应里
	if bytesContain([]byte(body), "super-secret-token-xyz") {
		t.Fatalf("GET /api/mcp 泄漏了 MCP env 的明文 token: %s", body)
	}
	// ② Authorization 这个 key 仍要在（用户要能看出「配过」）
	var resp struct {
		Servers []struct {
			Name string            `json:"name"`
			Env  map[string]string `json:"env"`
		} `json:"servers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	found := false
	for _, m := range resp.Servers {
		if m.Name != "mask-e2e" {
			continue
		}
		found = true
		if _, ok := m.Env["Authorization"]; !ok {
			t.Fatal("Authorization key 消失了 —— 用户看不出这个 MCP 配过凭据")
		}
		if m.Env["Authorization"] == "Bearer super-secret-token-xyz" {
			t.Fatal("Authorization 值仍是明文")
		}
	}
	if !found {
		t.Fatal("响应里没找到刚建的 mask-e2e —— handler 可能没返回它")
	}
}
