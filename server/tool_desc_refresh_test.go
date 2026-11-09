package server

// 按源行情工具「描述刷新进 DB」的验收测试（2026-11-03）。
//
// # 这条守的是什么
//
// 模型运行时看的描述来自 domainReg 的内存对象（Description + Prompt 拼接），
// 那是正确的。但 UI 与 /api/tools 读的是 tools 表的 description 列 ——
// 静态快照。
//
// SeedTool 是 ON CONFLICT DO NOTHING，所以描述一旦写进去就再也不更新。
// 后果：代码里给 quote_mx 加了「空值不是 0」的说明，界面上却看不到，
// 用户以为 agent 不知道这件事。
//
// refreshSourceQuoteToolDescriptions 就是修这个。测试必须证明
// 「跑一次之后，DB 描述里含关键契约」。

import (
	"strings"
	"testing"
)

// TestRefreshSourceQuoteDescriptionsReachesDB DB 描述必须含返回值契约。
func TestRefreshSourceQuoteDescriptionsReachesDB(t *testing.T) {
	s, _, _ := newAuthServer(t)

	// 先把描述故意改坏，模拟「旧版本写进 DB 的旧文案」
	if _, err := s.m.pg.Exec(
		`UPDATE tools SET description='旧描述无契约' WHERE key='quote_mx'`); err != nil {
		t.Fatalf("预置旧描述: %v", err)
	}

	s.refreshSourceQuoteToolDescriptions()

	var desc string
	if err := s.m.pg.QueryRow(
		`SELECT description FROM tools WHERE key='quote_mx'`).Scan(&desc); err != nil {
		t.Fatalf("读描述: %v", err)
	}

	// 关键契约必须在 DB 里（不只是内存里）
	for _, want := range []string{"空字符串", "不是 0", "quote_akshare"} {
		if !strings.Contains(desc, want) {
			t.Errorf("DB 里的 quote_mx 描述缺 %q —— UI 上看不到这条契约\n描述: %s", want, desc)
		}
	}
	if strings.Contains(desc, "旧描述无契约") {
		t.Error("描述未被刷新 —— 仍是旧文案")
	}
}

// TestRefreshPreservesAgentBindings 刷新描述不能动 agents 绑定。
//
// # 为什么单独断言
//
// RefreshToolDefaults 的 SQL 是 UPDATE description, schema —— 不含 agents。
// 但若将来有人图方便改成 UpsertToolForce（那个会重写 agents），
// 用户的绑定会被静默重置。这条测试就是那道闸。
func TestRefreshPreservesAgentBindings(t *testing.T) {
	s, _, _ := newAuthServer(t)

	// 给 quote_mx 绑一个非默认的 agent，证明刷新不会冲掉
	if _, err := s.m.pg.Exec(
		`UPDATE tools SET agents='["custom_agent"]' WHERE key='quote_mx'`); err != nil {
		t.Fatalf("预置绑定: %v", err)
	}

	s.refreshSourceQuoteToolDescriptions()

	var agents string
	if err := s.m.pg.QueryRow(
		`SELECT agents FROM tools WHERE key='quote_mx'`).Scan(&agents); err != nil {
		t.Fatalf("读 agents: %v", err)
	}
	if !strings.Contains(agents, "custom_agent") {
		t.Errorf("刷新把 agents 绑定冲掉了（实际 %s）—— 用户配置被静默重置", agents)
	}
}
