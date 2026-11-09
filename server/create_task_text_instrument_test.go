package server

import (
	"encoding/json"
	"strconv"
	"testing"
)

// TestCreateTaskFromTextBindsInstrument 走真实 HTTP handler：
// 不传 instrument_id，只在 goal 里写 msft，验证任务绑上了标的。
func TestCreateTaskFromTextBindsInstrument(t *testing.T) {
	s, _, request := newAuthServer(t)

	cli := s.marketClientFor("us")
	if cli != nil {
		if _, _, err := cli.Health(t.Context()); err != nil {
			t.Skipf("akshare sidecar 不可用（%v），跳过", err)
		}
	} else {
		t.Skip("market client 未装配")
	}

	w := request("POST", "/api/tasks", `{"name":"msft","description":"","goal":"给出 msft 交易策略"}`)
	if w.Code != 201 {
		t.Fatalf("createTask: %d %s", w.Code, w.Body.String())
	}

	// ⚠️ 必须断言**响应体**，不能只看库。
	// 库里有值但响应缺失，是本 bug 的实际形态：CreateTaskWithOptions 写对了，
	// 而 taskFromPG 漏拷 instrument_id → 内存对象 nil → DTO 里没这个字段。
	// 只查库的测试会放行，而前端拿到的是「没有标的」的任务。
	var created struct {
		ID           string `json:"id"`
		InstrumentID *int64 `json:"instrument_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if created.InstrumentID == nil {
		t.Fatalf("响应里没有 instrument_id（库里有值也不行 —— 前端看不到）: %s", w.Body.String())
	}
	wantInstID := *created.InstrumentID

	var instID *int64
	var taskID int64
	if err := s.m.pg.QueryRow(
		`SELECT id, instrument_id FROM tasks WHERE goal='给出 msft 交易策略' ORDER BY id DESC LIMIT 1`,
	).Scan(&taskID, &instID); err != nil {
		t.Fatalf("查不到刚建的任务: %v", err)
	}
	if instID == nil {
		t.Fatalf("任务 %d 的 instrument_id 仍是 NULL —— 文本解析没生效", taskID)
	}
	if *instID != wantInstID {
		t.Errorf("响应 instrument_id=%d 与库里 %d 不一致", wantInstID, *instID)
	}
	var symbol string
	if err := s.m.pg.QueryRow(`SELECT symbol FROM instruments WHERE id=$1`, *instID).Scan(&symbol); err != nil {
		t.Fatalf("查标的: %v", err)
	}
	if symbol != "MSFT" {
		t.Errorf("绑定标的 = %q, want MSFT", symbol)
	}

	// 阶段图必须建了 —— 否则 engine 无活可派。
	var nodes int
	if err := s.m.pg.QueryRow(
		`SELECT count(*) FROM exploration_nodes n
		 JOIN tasks t ON t.exploration_id = n.exploration_id WHERE t.id=$1`, taskID,
	).Scan(&nodes); err != nil {
		t.Fatalf("数节点: %v", err)
	}
	if nodes == 0 {
		t.Error("阶段图 0 节点 —— 建图没触发")
	}
	t.Logf("任务 %d 绑定 MSFT，阶段图 %d 节点", taskID, nodes)

	// 顺序不能反：tasks.instrument_id 是 → instruments 的外键，
	// 先删标的会被 tasks 挡住（23503）。必须先走 Manager.DeleteTask
	// （而非裸 DELETE —— 裸删绕过 exploration subgraph 级联，
	// 残留守卫会抓到「explorations/nodes 涨了但 tasks 没变」），再删标的。
	t.Cleanup(func() {
		if _, err := s.m.DeleteTask(strconv.FormatInt(taskID, 10), DeleteTaskOptions{}); err != nil {
			t.Errorf("清理任务 %d: %v", taskID, err)
		}
		if _, err := s.m.pg.Exec(`DELETE FROM instruments WHERE id=$1`, *instID); err != nil {
			t.Errorf("清理标的 %d: %v", *instID, err)
		}
	})
}
