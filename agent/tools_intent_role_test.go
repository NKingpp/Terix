package agent

// add_intent 的 role 参数验收测试。
//
// # 这组测试守的是什么
//
// planner 能指定 role 之后，最大的风险是**静默失败**：role 写错、role 生效了但
// 意图拿不到标的、或者空 role 的旧行为被无意改掉。三者都不会报错，只会让结论变差。
//
// 所以这里逐条钉住：
//   1. 非法 role 必须**拒绝**并给出可纠正的清单（不是静默落库）；
//   2. 合法 role 必须同时写 payload.role 与 owner 列（两者由不同代码路径派生）；
//   3. 角色意图必须继承任务标的，否则角色判「标的未知」→ unavailable；
//   4. 空 role 的 payload 必须与改动前逐字节一致（零回归）。

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// newIntentToolSet 建一个挂在临时 exploration 上的 ToolSet。
func newIntentToolSet(t *testing.T) (*ToolSet, *db.DB, int64) {
	t.Helper()
	d := testDB(t)
	expID := mustExploration(t, d)
	ts := d.Exploration(expID)
	return NewToolSet(ts, "planner"), d, expID
}

// readIntentPayload 读回某个 intent 节点的 payload 与 owner 列。
func readIntentPayload(t *testing.T, d *db.DB, id int64) (map[string]any, string) {
	t.Helper()
	var raw []byte
	var owner *string
	err := d.QueryRow(`SELECT payload, owner FROM exploration_nodes WHERE id=$1`, id).Scan(&raw, &owner)
	if err != nil {
		t.Fatalf("读回 intent %d: %v", id, err)
	}
	var p map[string]any
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("解析 payload: %v", err)
	}
	o := ""
	if owner != nil {
		o = *owner
	}
	return p, o
}

// TestAddIntentRejectsUnknownRole 拼错的 role 必须被拒绝，且错误里带合法清单。
//
// # 为什么必须是「拒绝」而不是「纠正」
//
// 自动把 analysts_market 改成 analyst_market 会让模型以为它拼对了，下次继续拼错。
// 拒绝 + 清单是它唯一能自我纠正的信号。
func TestAddIntentRejectsUnknownRole(t *testing.T) {
	ts, _, _ := newIntentToolSet(t)

	_, err := ts.addOneIntent(intentItem{Summary: "补跑技术面分析", Role: "analysts_market"})
	if err == nil {
		t.Fatal("拼错的 role 必须被拒绝 —— 否则节点会静默退回通用 worker")
	}
	msg := err.Error()
	if !strings.Contains(msg, "analysts_market") {
		t.Errorf("错误应回显收到的值，实际: %s", msg)
	}
	// 必须带合法清单：这是模型唯一能自纠的输入
	for _, want := range []string{RoleAnalystMarket, RoleTrader} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误文案必须含合法清单（缺 %q），实际: %s", want, msg)
		}
	}
}

// TestAddIntentWritesRoleAndOwner 合法 role 同时落 payload.role 与 owner 列。
//
// 两者由不同路径派生（payload 由本函数写；owner 由 AddNode 从 payload 抄，
// 或由 schema 触发器兜底），只测一个会漏掉另一半。
func TestAddIntentWritesRoleAndOwner(t *testing.T) {
	ts, d, _ := newIntentToolSet(t)

	id, err := ts.addOneIntent(intentItem{Summary: "出交易信号", Role: RoleTrader})
	if err != nil {
		t.Fatalf("合法 role 不该被拒绝: %v", err)
	}
	p, owner := readIntentPayload(t, d, id)
	if got, _ := p["role"].(string); got != RoleTrader {
		t.Errorf("payload.role = %q, want %q", got, RoleTrader)
	}
	if owner != RoleTrader {
		t.Errorf("owner 列 = %q, want %q —— agentKeyForIntent 与前端徽标都读它", owner, RoleTrader)
	}
}

// TestAddIntentEmptyRoleIsUnchanged 空 role 的 payload 必须与改动前一致。
//
// 这是零回归的锚点：存量 planner 不传 role，落库形状不能因为本次改动而变化
// （多出 role:"" 或 instrument_id 都算回归 —— 下游按 key 存在性判定的地方会误读）。
func TestAddIntentEmptyRoleIsUnchanged(t *testing.T) {
	ts, d, _ := newIntentToolSet(t)

	id, err := ts.addOneIntent(intentItem{Summary: "开放式侦察一下"})
	if err != nil {
		t.Fatalf("不传 role 不该失败: %v", err)
	}
	p, _ := readIntentPayload(t, d, id)
	if _, ok := p["role"]; ok {
		t.Error("空 role 不该写入 payload（会污染按 key 存在性判定的下游）")
	}
	if _, ok := p["instrument_id"]; ok {
		t.Error("空 role 不该写 instrument_id —— 那是角色执行者才需要的")
	}
	if p["summary"] != "开放式侦察一下" {
		t.Errorf("summary 应原样保留，实际 %v", p["summary"])
	}
}

// TestAddIntentRoleInheritsInstrument 角色意图必须继承任务标的。
//
// # 为什么这条最关键
//
// 角色执行者（analyst_market 等）靠 payload.instrument_id 定位标的。不回填的话
// 它拿到 0 → vars 构造器判「标的未知」→ 直接写 unavailable。
// 那比「不派角色」还浪费：白白消耗一轮完整 LLM 调用，只换来一条 data_gap。
func TestAddIntentRoleInheritsInstrument(t *testing.T) {
	ts, d, expID := newIntentToolSet(t)

	// 先建一张带标的的阶段图（模拟任务创建时的 Expand）
	g, err := Expand(AssetClassStock, ExpandOpts{InstrumentID: 42})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if err := withTx(t, d, func(tx *sql.Tx) error {
		_, e := MaterializeGraph(tx, expID, g)
		return e
	}); err != nil {
		t.Fatalf("MaterializeGraph: %v", err)
	}

	id, err := ts.addOneIntent(intentItem{Summary: "补跑技术面", Role: RoleAnalystMarket})
	if err != nil {
		t.Fatalf("addOneIntent: %v", err)
	}
	p, _ := readIntentPayload(t, d, id)

	gotID, _ := p["instrument_id"].(float64) // JSON 反序列化成 float64
	if int64(gotID) != 42 {
		t.Errorf("payload.instrument_id = %v, want 42 —— 不回填会让分析师判「标的未知」", p["instrument_id"])
	}
	if ac, _ := p["asset_class"].(string); ac != AssetClassStock {
		t.Errorf("payload.asset_class = %q, want %q", ac, AssetClassStock)
	}
}

// TestAddIntentRoleWithoutInstrumentStillWorks 无标的任务（尚未选品）不报错。
//
// 这是合法状态：screener 就是靠「无标的」工作的。回填拿不到就跳过，
// 不能因此拒绝创建意图。
func TestAddIntentRoleWithoutInstrumentStillWorks(t *testing.T) {
	ts, d, _ := newIntentToolSet(t)

	id, err := ts.addOneIntent(intentItem{Summary: "筛选加密永续候选", Role: RoleScreener})
	if err != nil {
		t.Fatalf("无标的任务也该能派 screener: %v", err)
	}
	p, _ := readIntentPayload(t, d, id)
	if got, _ := p["role"].(string); got != RoleScreener {
		t.Errorf("payload.role = %q, want %q", got, RoleScreener)
	}
	if _, ok := p["instrument_id"]; ok {
		t.Error("无标的时不该写 instrument_id")
	}
}

// TestAddIntentSchemaHasRole schema 的 enum 必须与 FinancialRoleKeys() 逐项相等。
//
// 防止两处分叉：schema 里少一个 → 模型看不到，派不了；多一个 → 模型能填但会被
// handler 拒绝，白费一次往返。
func TestAddIntentSchemaHasRole(t *testing.T) {
	ts := &ToolSet{}
	schema := ts.addIntent().InputSchema()

	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema.properties 缺失或类型错误: %T", schema["properties"])
	}
	roleProp, ok := props["role"].(map[string]any)
	if !ok {
		t.Fatal("add_intent schema 缺 role 参数 —— 模型永远看不到它")
	}

	rawEnum, ok := roleProp["enum"].([]any)
	if !ok {
		// FinancialRoleKeys() 是 []string，obj() 可能原样透传
		if ss, ok2 := roleProp["enum"].([]string); ok2 {
			rawEnum = make([]any, len(ss))
			for i, s := range ss {
				rawEnum[i] = s
			}
		} else {
			t.Fatalf("role.enum 类型异常: %T", roleProp["enum"])
		}
	}

	want := FinancialRoleKeys()
	if len(rawEnum) != len(want) {
		t.Fatalf("enum 长度 %d != FinancialRoleKeys() 长度 %d", len(rawEnum), len(want))
	}
	for i, w := range want {
		if s, _ := rawEnum[i].(string); s != w {
			t.Errorf("enum[%d] = %q, want %q（顺序也要一致，清单会渲染进 prompt）", i, s, w)
		}
	}
}

// TestCompactIntentsEchoesRole planner 每次唤醒都是新 session，
// role 回显是它「上一轮派过什么角色」的唯一记忆。
func TestCompactIntentsEchoesRole(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{"summary": "出信号", "role": RoleTrader})
	n := &db.Node{ID: 7, State: "open", Payload: payload}
	out := compactIntents([]*db.Node{n}, nil, nil)
	if len(out) != 1 {
		t.Fatalf("应输出 1 条，实际 %d", len(out))
	}
	if got, _ := out[0]["role"].(string); got != RoleTrader {
		t.Errorf("回显 role = %v, want %q", out[0]["role"], RoleTrader)
	}

	// 无 role 的意图不该凭空多出 role 键（保持输出紧凑）
	plain, _ := json.Marshal(map[string]any{"summary": "查点东西"})
	out2 := compactIntents([]*db.Node{{ID: 8, State: "open", Payload: plain}}, nil, nil)
	if _, ok := out2[0]["role"]; ok {
		t.Error("无 role 的意图不该输出 role 键")
	}
}
