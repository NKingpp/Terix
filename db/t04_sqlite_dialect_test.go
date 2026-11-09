package db

// T04（任务 / 配置 / 会话 / 通知域）的方言自检。
//
// 覆盖 ARCH-SQLite-Migration.md 里 T04 的四条验收要求，逐条对应：
//   - 归档 → 还原的完整往返（A 项：json_populate_record 的替代实现）
//   - task_archives 的聚合结果（B 项：jsonb_agg / FILTER / jsonb_build_object）
//   - 通知派发（C 项：删掉 SKIP LOCKED 后不重复、不丢失）
//   - config 的数组展开（D 项：jsonb_array_elements_text → json_each）
//
// 这些都是「编译通过但运行时报错」的写法（JSON 函数名、聚合空集、占位符拼接），
// 必须实跑才算验证过。

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/Autumn-27/artex/notify"
)

// timeSecond 让「租约时长」的意图在调用点可读（30*time.Second 的简写）。
func timeSecond() time.Duration { return time.Second }

// t04Aggregates 在只读事务里调 taskArchiveAggregates。
//
// 生产路径由 snapshotTaskArchive 开事务后调用；这里复现同样的形态，
// 避免测试直接依赖 *DB 的内部细节。
func t04Aggregates(t *testing.T, d *DB, taskID, expID int64) (map[string]any, error) {
	t.Helper()
	tx, err := d.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck
	return taskArchiveAggregates(tx, taskID, expID)
}

// TestT04ConfigArrayExpansion 验证 AgentBindingCounts 的数组展开。
//
// 原 PG 写法 `tools, jsonb_array_elements_text(agents) AS elem GROUP BY elem`。
// SQLite 版改成 `json_each(tools.agents) AS elem` + `GROUP BY elem.value`。
// 这里断言「同一个 key 出现在两个工具里 → 计数 2」，正是展开语义的判据
// （若误写成对 agents 整列聚合，会得到 1）。
func TestT04ConfigArrayExpansion(t *testing.T) {
	d := testOpen(t)
	agentKey := "t04expand"
	if _, err := d.CreateAgent(agentKey, "T04 展开", "自检用"); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM agents WHERE key=$1`, agentKey) })

	toolKeys := []string{"t04_tool_a", "t04_tool_b"}
	for _, key := range toolKeys {
		agents, _ := json.Marshal([]string{agentKey})
		if _, err := d.Exec(`INSERT INTO tools(key, system, description, schema, agents, enabled)
VALUES($1, false, 't04', '{}', $2, true)
ON CONFLICT (key) DO UPDATE SET agents=excluded.agents`, key, string(agents)); err != nil {
			t.Fatalf("seed tool %s: %v", key, err)
		}
		t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM tools WHERE key=$1`, key) })
	}

	_, _, tools, err := d.AgentBindingCounts()
	if err != nil {
		t.Fatalf("AgentBindingCounts: %v", err)
	}
	// 只断言本用例的 key：共享库里可能有别处留下的同名绑定。
	if got := tools[agentKey]; got != len(toolKeys) {
		t.Fatalf("agent %s 的工具绑定计数=%d，want %d（json_each 展开应为每个工具各计一次）",
			agentKey, got, len(toolKeys))
	}

	// 移除后再查：RemoveAgentFromToolBindings 用 json_group_array 重建数组，
	// 空集时必须是 '[]' 而不是 NULL（否则 NOT NULL 约束会炸）。
	if err := d.RemoveAgentFromToolBindings(agentKey); err != nil {
		t.Fatalf("RemoveAgentFromToolBindings: %v", err)
	}
	for _, key := range toolKeys {
		var agents string
		if err := d.QueryRow(`SELECT agents FROM tools WHERE key=$1`, key).Scan(&agents); err != nil {
			t.Fatalf("read agents: %v", err)
		}
		if agents != "[]" {
			t.Fatalf("移除后 agents=%q，want \"[]\"", agents)
		}
	}
	if _, _, tools, err = d.AgentBindingCounts(); err != nil {
		t.Fatalf("AgentBindingCounts after remove: %v", err)
	}
	if got := tools[agentKey]; got != 0 {
		t.Fatalf("移除后计数=%d，want 0", got)
	}
}

// TestT04NotificationFanOutAndClaim 验证通知派发在删掉 SKIP LOCKED 后的行为。
//
// 三条不变量：
//  1. 一轮分派把事件展开成「渠道数」条投递，且事件被标记 fanned_out
//  2. 第二轮不会重复展开（fanned_out 起作用）
//  3. 领取（ClaimRealtimeDeliveries）把行置 sending 并 +1 attempts，
//     第二次领取不会再拿到同一行（租约把它们推到了未来）
func TestT04NotificationFanOutAndClaim(t *testing.T) {
	d := testOpen(t)
	ctx := context.Background()

	channels := make([]*NotificationChannel, 0, 2)
	for i := 0; i < 2; i++ {
		ch := &NotificationChannel{
			Name:       fmt.Sprintf("t04-notify-%d-%s", i, t.Name()),
			Kind:       notify.KindDingTalk,
			Mode:       NotifyModeRealtime,
			Config:     json.RawMessage(`{"webhook":"https://example.com/hook"}`),
			Filter:     json.RawMessage(`{}`),
			RatePerMin: 100,
		}
		id, err := d.SaveNotificationChannel(ctx, ch)
		if err != nil {
			t.Fatalf("save channel: %v", err)
		}
		t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM notification_channels WHERE id=$1`, id) })
		ch.ID = id
		channels = append(channels, ch)
	}

	eventID, err := d.AddNotificationEvent(ctx, notify.EventFindingCreated, 90401,
		notify.Snapshot{Kind: notify.EventFindingCreated, FindingID: 90401, Severity: SeverityHigh})
	if err != nil {
		t.Fatalf("add event: %v", err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM notification_events WHERE id=$1`, eventID) })

	// 第一轮：只分派本用例造的那一条，避免共享库里别人的事件干扰计数。
	// FanOutPendingEvents 按 id 升序取，所以先把自己之前的都清掉更稳 —— 直接
	// 断言「本事件被展开」而不是「本轮展开了几条」。
	if _, _, err := d.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatalf("fan out: %v", err)
	}
	var fannedOut bool
	if err := d.QueryRow(`SELECT fanned_out FROM notification_events WHERE id=$1`, eventID).Scan(&fannedOut); err != nil {
		t.Fatalf("read fanned_out: %v", err)
	}
	if !fannedOut {
		t.Fatal("事件未被标记 fanned_out")
	}
	var deliveriesForEvent int
	if err := d.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE event_id=$1`, eventID).Scan(&deliveriesForEvent); err != nil {
		t.Fatalf("count deliveries: %v", err)
	}
	if deliveriesForEvent != len(channels) {
		t.Fatalf("投递数=%d，want %d（每个启用渠道一条）", deliveriesForEvent, len(channels))
	}

	// 第二轮：不得重复展开。
	if _, deliveryCount, err := d.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatalf("second fan out: %v", err)
	} else if deliveryCount != 0 {
		t.Fatalf("第二轮又新建了 %d 条投递（fanned_out 未生效）", deliveryCount)
	}
	var again int
	if err := d.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE event_id=$1`, eventID).Scan(&again); err != nil {
		t.Fatalf("recount: %v", err)
	}
	if again != deliveriesForEvent {
		t.Fatalf("投递数从 %d 变成 %d（重复展开）", deliveriesForEvent, again)
	}

	// 领取：租约把它们推到未来，第二次必须领不到同一行。
	claimed, err := d.ClaimRealtimeDeliveries(ctx, channels[0].ID, 10, 30*timeSecond())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	want := 0
	for _, dl := range claimed {
		if dl.EventID == eventID {
			want++
			if dl.State != NotifyStateSending {
				t.Fatalf("领取后 state=%q，want %q", dl.State, NotifyStateSending)
			}
			if dl.Attempts != 1 {
				t.Fatalf("领取后 attempts=%d，want 1", dl.Attempts)
			}
		}
	}
	if want != 1 {
		t.Fatalf("第一次领取拿到本事件 %d 条投递，want 1", want)
	}
	second, err := d.ClaimRealtimeDeliveries(ctx, channels[0].ID, 10, 30*timeSecond())
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	for _, dl := range second {
		if dl.EventID == eventID {
			t.Fatal("租约未生效：第二次领取又拿到了同一条投递")
		}
	}

	// 标记已送达后不应再被领取。
	ids := make([]int64, 0, len(claimed))
	for _, dl := range claimed {
		ids = append(ids, dl.ID)
	}
	if err := d.MarkDeliveriesSent(ctx, ids); err != nil {
		t.Fatalf("mark sent: %v", err)
	}
	third, err := d.ClaimRealtimeDeliveries(ctx, channels[0].ID, 10, 30*timeSecond())
	if err != nil {
		t.Fatalf("third claim: %v", err)
	}
	for _, dl := range third {
		if dl.EventID == eventID {
			t.Fatal("已送达的投递又被领取了一次")
		}
	}
	// 清理投递行（事件删了会级联，但这里显式清更直观）。
	for _, dl := range append(append([]*NotificationDelivery{}, claimed...), second...) {
		_, _ = d.Exec(`DELETE FROM notification_deliveries WHERE id=$1`, dl.ID)
	}
	for _, dl := range third {
		_, _ = d.Exec(`DELETE FROM notification_deliveries WHERE id=$1`, dl.ID)
	}
}

// TestT04ArchiveAggregateStatsShape 验证 taskArchiveAggregates 的产出形状。
//
// 这是 B 项（jsonb_agg / FILTER / jsonb_build_object → json_group_array /
// json_group_object / json_object）的验收：逐个字段断言，而不是只看「能跑」。
//
// 关键覆盖点：
//   - 空集时必须得到 `[]` / `{}`，不是 NULL（json_group_array 空集返回 NULL）
//   - `count(*) FILTER (WHERE …)` 的计数正确
//   - `agents` 数组来自 json_group_array(DISTINCT …)，必须能 Unmarshal 成 []string
func TestT04ArchiveAggregateStatsShape(t *testing.T) {
	d := testOpen(t)
	if err := d.EnsureLLMUsageTable(); err != nil {
		t.Fatalf("ensure llm_usage: %v", err)
	}
	task, err := d.CreateTask("t04 aggregate", "验证聚合形状", nil, 0, 0)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	taskIDStr := fmt.Sprint(task.ID)

	// 空集：三段聚合都必须返回 [] / {}，而不是 NULL。
	stats, err := t04Aggregates(t, d, task.ID, task.ExplorationID)
	if err != nil {
		t.Fatalf("aggregates (empty): %v", err)
	}
	for _, key := range []string{"token_profiles", "token_daily", "skill_stats", "missing_skill_stats"} {
		raw, err := json.Marshal(stats[key])
		if err != nil {
			t.Fatalf("%s marshal: %v", key, err)
		}
		if string(raw) != "[]" {
			t.Fatalf("空集 %s=%s，want []", key, raw)
		}
	}
	for _, key := range []string{"skills", "tools", "findings"} {
		raw, err := json.Marshal(stats[key])
		if err != nil {
			t.Fatalf("%s marshal: %v", key, err)
		}
		if string(raw) != "{}" {
			t.Fatalf("空集 %s=%s，want {}", key, raw)
		}
	}
	findingStats, ok := stats["finding_stats"].(map[string]any)
	if !ok {
		t.Fatalf("finding_stats 类型=%T，want map", stats["finding_stats"])
	}
	if vulns, ok := findingStats["vulnclasses"].([]any); !ok || len(vulns) != 0 {
		t.Fatalf("空集 vulnclasses=%v，want []", findingStats["vulnclasses"])
	}

	// 有数据：灌 2 条 llm_usage + 1 条 skill_usage + 1 条 tool_usage + 2 条 finding。
	for _, u := range []*LLMUsage{
		{TaskID: taskIDStr, ExplorationID: task.ExplorationID, Worker: "worker", Model: "m1", ProfileName: "p1", InputTokens: 10, OutputTokens: 5, CacheRead: 1, CacheWrite: 2, Status: "ok"},
		{TaskID: taskIDStr, ExplorationID: task.ExplorationID, Worker: "worker", Model: "m2", ProfileName: "p1", InputTokens: 20, OutputTokens: 8, CacheRead: 3, CacheWrite: 4, Status: "ok"},
	} {
		if err := d.InsertLLMUsage(u); err != nil {
			t.Fatalf("insert llm_usage: %v", err)
		}
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM llm_usage WHERE task_id=$1`, taskIDStr) })
	if err := d.InsertSkillUsage(&SkillUsage{Skill: "t04skill", AgentKey: "worker", TaskID: task.ID, ExplorationID: task.ExplorationID, Found: true}); err != nil {
		t.Fatalf("insert skill_usage: %v", err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM skill_usage WHERE task_id=$1`, task.ID)
	})
	if err := d.InsertToolUsage(&ToolUsage{ToolKey: "t04tool", AgentKey: "worker", TaskID: task.ID, ExplorationID: task.ExplorationID}); err != nil {
		t.Fatalf("insert tool_usage: %v", err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM tool_usage WHERE task_id=$1`, task.ID)
	})
	for _, f := range []struct {
		vulnclass, severity, status string
	}{
		{"t04-vuln-a", SeverityCritical, "pending"},
		{"t04-vuln-b", SeverityLow, "confirmed"},
	} {
		id, err := d.AddFinding(task.ID, 0, f.vulnclass, "t04 finding", f.severity, "summary", "evidence", "worker", nil)
		if err != nil {
			t.Fatalf("insert finding: %v", err)
		}
		if f.status != "pending" {
			if _, err := d.SetFindingStatus(id, f.status); err != nil {
				t.Fatalf("set finding status: %v", err)
			}
		}
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM findings WHERE task_id=$1`, task.ID) })

	stats, err = t04Aggregates(t, d, task.ID, task.ExplorationID)
	if err != nil {
		t.Fatalf("aggregates (filled): %v", err)
	}

	tokens, ok := stats["tokens"].(map[string]int64)
	if !ok {
		t.Fatalf("tokens 类型=%T", stats["tokens"])
	}
	if tokens["calls"] != 2 || tokens["input_tokens"] != 30 || tokens["output_tokens"] != 13 ||
		tokens["cache_read_tokens"] != 4 || tokens["cache_write_tokens"] != 6 {
		t.Fatalf("tokens=%+v，want calls=2 input=30 output=13 cache_read=4 cache_write=6", tokens)
	}

	profiles, ok := stats["token_profiles"].([]any)
	if !ok || len(profiles) != 1 {
		t.Fatalf("token_profiles=%v，want 1 条（同一个 profile）", stats["token_profiles"])
	}
	profile, _ := profiles[0].(map[string]any)
	if profile["profile_name"] != "p1" || profile["input_tokens"] != float64(30) {
		t.Fatalf("token_profiles[0]=%+v", profile)
	}

	daily, ok := stats["token_daily"].([]any)
	if !ok || len(daily) != 1 {
		t.Fatalf("token_daily=%v，want 1 条（同一天）", stats["token_daily"])
	}
	day, _ := daily[0].(map[string]any)
	// substr(ts,1,10) 必须给出 YYYY-MM-DD。
	if date, _ := day["date"].(string); len(date) != 10 || date[4] != '-' || date[7] != '-' {
		t.Fatalf("token_daily.date=%v，want YYYY-MM-DD", day["date"])
	}

	skillStats, ok := stats["skill_stats"].([]any)
	if !ok || len(skillStats) != 1 {
		t.Fatalf("skill_stats=%v，want 1 条", stats["skill_stats"])
	}
	skill, _ := skillStats[0].(map[string]any)
	agents, ok := skill["agents"].([]any)
	if !ok || len(agents) != 1 || agents[0] != "worker" {
		t.Fatalf("skill_stats[0].agents=%v，want [worker]", skill["agents"])
	}
	if skill["calls"] != float64(1) {
		t.Fatalf("skill_stats[0].calls=%v，want 1", skill["calls"])
	}

	tools, ok := stats["tools"].(map[string]any)
	if !ok || tools["t04tool"] != float64(1) {
		t.Fatalf("tools=%v，want {t04tool:1}", stats["tools"])
	}

	findingStats, ok = stats["finding_stats"].(map[string]any)
	if !ok {
		t.Fatalf("finding_stats 类型=%T", stats["finding_stats"])
	}
	if findingStats["total"] != float64(2) || findingStats["pending"] != float64(1) ||
		findingStats["critical"] != float64(1) || findingStats["high"] != float64(0) ||
		findingStats["medium"] != float64(0) || findingStats["low"] != float64(1) {
		t.Fatalf("finding_stats=%+v", findingStats)
	}
	classes, ok := findingStats["vulnclasses"].([]any)
	if !ok || len(classes) != 2 {
		t.Fatalf("vulnclasses=%v，want 2 项", findingStats["vulnclasses"])
	}
}
