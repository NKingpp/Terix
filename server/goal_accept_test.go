package server

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func acceptBody(reason string, evidence int64, acc *db.GoalAcceptance) string {
	b := map[string]any{"reason": reason}
	if evidence > 0 {
		b["evidence_id"] = evidence
	}
	if acc != nil {
		b["acceptance"] = acc
	}
	raw, _ := json.Marshal(b)
	return string(raw)
}

// 平台账本没有本任务成交时，ledger 口径必须拒绝 —— 否则「空账本 flat」会平凡通过。
func TestAcceptGoalVerifyFails409(t *testing.T) {
	_, task, request := newAuthServer(t)
	goal, err := task.Store.AddGoal(map[string]any{"text": "净盈利 10 USDT"}, "human")
	if err != nil {
		t.Fatal(err)
	}
	body := acceptBody("已达成", 0, &db.GoalAcceptance{Kind: "pnl_ge", Source: "ledger", Mode: "live", Threshold: "10"})
	rec := request("POST", fmt.Sprintf("/api/tasks/%s/goals/%d/accept", task.ID, goal), body)
	if rec.Code != 409 {
		t.Fatalf("核验未通过应 409，得到 %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Code     string            `json:"code"`
		Observed map[string]string `json:"observed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Code != "GOAL_ACCEPT_VERIFY_FAILED" {
		t.Fatalf("错误码应为 GOAL_ACCEPT_VERIFY_FAILED，得到 %q", resp.Code)
	}
	if resp.Observed["open_positions"] == "" || resp.Observed["source"] != "ledger" {
		t.Fatalf("应回显实测值，得到 %+v", resp.Observed)
	}
	node, err := task.Store.GetNode(goal)
	if err != nil || node.State != "open" {
		t.Fatalf("核验失败不得改状态: %+v err=%v", node, err)
	}
}

// 全部目标验收通过后任务落 done（金融任务没有其它收尾路径）。
func TestAcceptGoalAllMetClosesTask(t *testing.T) {
	s, task, request := newAuthServer(t)
	evidence, err := task.Store.AddNode(db.KindFact, map[string]any{"text": "QQQ round-trip 净 +11.0041880000，持仓 flat"}, 0, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	acc := &db.GoalAcceptance{Kind: "flat", Source: "evidence"}
	goals := make([]int64, 0, 3)
	for i := 0; i < 3; i++ {
		goal, err := task.Store.AddGoal(map[string]any{"text": fmt.Sprintf("目标 %d", i+1)}, "human")
		if err != nil {
			t.Fatal(err)
		}
		goals = append(goals, goal)
	}
	lastGoal := goals[len(goals)-1]
	for i, goal := range goals {
		rec := request("POST", fmt.Sprintf("/api/tasks/%s/goals/%d/accept", task.ID, goal), acceptBody("实盘已验证", evidence, acc))
		if rec.Code != 200 {
			t.Fatalf("第 %d 条验收失败 %d: %s", i+1, rec.Code, rec.Body.String())
		}
		if i < len(goals)-1 && s.taskStatus(task.ID) == "done" {
			t.Fatalf("第 %d 条后不应落 done", i+1)
		}
	}
	if got := s.taskStatus(task.ID); got != "done" {
		t.Fatalf("全部目标验收后任务应为 done，得到 %q", got)
	}
	node, err := task.Store.GetNode(lastGoal)
	if err != nil || node.State != "met" {
		t.Fatalf("最后一条目标应为 met: %+v err=%v", node, err)
	}
	items, _, err := task.Store.ActivityList(&lastGoal, 0, 10)
	if err != nil || len(items) == 0 || items[0].Kind != "goal_accepted" {
		t.Fatalf("应写 goal_accepted 审计: %+v err=%v", items, err)
	}
}

func (s *Server) taskStatus(id string) string {
	t, ok := s.m.Task(id)
	if !ok {
		return ""
	}
	return t.Status
}

func TestAcceptGoalIdempotentAndReasonRequired(t *testing.T) {
	_, task, request := newAuthServer(t)
	evidence, err := task.Store.AddNode(db.KindFact, map[string]any{"text": "结算证据"}, 0, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	goal, err := task.Store.AddGoal(map[string]any{"text": "已平仓"}, "human")
	if err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/tasks/%s/goals/%d/accept", task.ID, goal)
	if rec := request("POST", path, acceptBody("", evidence, nil)); rec.Code != 400 {
		t.Fatalf("理由为空应 400，得到 %d: %s", rec.Code, rec.Body.String())
	}
	acc := &db.GoalAcceptance{Kind: "flat", Source: "evidence"}
	if rec := request("POST", path, acceptBody("已平仓", evidence, acc)); rec.Code != 200 {
		t.Fatalf("首次验收应 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	rec := request("POST", path, acceptBody("重复点击", evidence, acc))
	if rec.Code != 200 {
		t.Fatalf("重复验收应 200 幂等，得到 %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Changed bool `json:"changed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Changed {
		t.Fatal("重复验收 changed 应为 false")
	}
}

// 目标已配置口径时不允许在验收时改写（否则等于事后改考卷）。
func TestAcceptGoalRejectAcceptanceOverride(t *testing.T) {
	_, task, request := newAuthServer(t)
	evidence, err := task.Store.AddNode(db.KindFact, map[string]any{"text": "证据"}, 0, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	goal, err := task.Store.AddGoal(map[string]any{
		"text":       "目标",
		"acceptance": map[string]any{"kind": "flat", "source": "evidence"},
	}, "human")
	if err != nil {
		t.Fatal(err)
	}
	override := &db.GoalAcceptance{Kind: "flat", Source: "evidence", Mode: "paper"}
	rec := request("POST", fmt.Sprintf("/api/tasks/%s/goals/%d/accept", task.ID, goal), acceptBody("改口径", evidence, override))
	if rec.Code != 400 {
		t.Fatalf("已配置口径时改写应 400，得到 %d: %s", rec.Code, rec.Body.String())
	}
}
