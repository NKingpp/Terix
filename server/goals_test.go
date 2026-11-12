package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestEditGoalAudit(t *testing.T) {
	s, task, request := newAuthServer(t)
	goal, err := task.Store.AddGoal(map[string]any{"text": "至少1000个结果", "custom": "保留"}, "human")
	if err != nil {
		t.Fatal(err)
	}
	_, rootBefore, err := task.Store.Root()
	if err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/tasks/%s/goals/%d", task.ID, goal)
	for i, text := range []string{"至少100个结果", "至少10个结果"} {
		raw, _ := json.Marshal(map[string]string{"text": text})
		rec := request("PATCH", path, string(raw))
		if rec.Code != 200 {
			t.Fatalf("修改目标返回 %d: %s", rec.Code, rec.Body.String())
		}
		items, _, err := task.Store.ActivityList(&goal, 0, 100)
		if err != nil || len(items) != i+1 || items[i].Kind != "goal_updated" {
			t.Fatalf("编辑审计缺失: %+v err=%v", items, err)
		}
	}
	_, rootAfter, err := task.Store.Root()
	if err != nil || rootAfter != rootBefore {
		t.Fatalf("编辑改变 root goal: %q => %q err=%v", rootBefore, rootAfter, err)
	}
	triggers := task.drainTriggers()
	if len(triggers) != 2 || triggers[0].OldGoal != "至少1000个结果" || triggers[0].NewGoal != "至少100个结果" ||
		triggers[1].OldGoal != "至少100个结果" || triggers[1].NewGoal != "至少10个结果" {
		t.Fatalf("通知前后值不是提交链: %+v", triggers)
	}
	rec := request("PATCH", path, `{"text":"至少10个结果"}`)
	if rec.Code != 200 || len(task.drainTriggers()) != 0 {
		t.Fatalf("无变化不应发通知: %d %s", rec.Code, rec.Body.String())
	}
	other, err := s.m.CreateTask("其他探索", "root", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	otherID, err := strconv.ParseInt(other.ID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.m.pg.DeleteTask(otherID) })
	rec = request("PATCH", fmt.Sprintf("/api/tasks/%s/goals/%d", other.ID, goal), `{"text":"越界"}`)
	if rec.Code != 404 {
		t.Fatalf("跨 exploration 应返回404: %d %s", rec.Code, rec.Body.String())
	}
	proof, err := task.Store.AddNode(db.KindFact, map[string]any{"text": "证据"}, 0, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	rec = request("PATCH", fmt.Sprintf("/api/tasks/%s/goals/%d", task.ID, proof), `{"text":"非目标"}`)
	if rec.Code != 404 {
		t.Fatalf("非 goal 应返回404: %d %s", rec.Code, rec.Body.String())
	}
}

func TestEditGoalAuditFailure(t *testing.T) {
	s, task, request := newAuthServer(t)
	goal, err := task.Store.AddGoal(map[string]any{"text": "旧目标"}, "human")
	if err != nil {
		t.Fatal(err)
	}
	trigger := fmt.Sprintf("fail_api_goal_audit_%d", task.ExpID)
	if _, err := s.m.pg.Exec(fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON activity
		WHEN NEW.kind='goal_updated' AND NEW.exploration_id=%d
		BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END`, trigger, task.ExpID)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.m.pg.Exec(`DROP TRIGGER ` + trigger) })
	rec := request("PATCH", fmt.Sprintf("/api/tasks/%s/goals/%d", task.ID, goal), `{"text":"新目标"}`)
	if rec.Code != 500 {
		t.Fatalf("审计失败应返回500: %d %s", rec.Code, rec.Body.String())
	}
	node, err := task.Store.GetNode(goal)
	if err != nil || goalDTO(node).Text != "旧目标" || len(task.drainTriggers()) != 0 {
		t.Fatalf("审计失败修改未回滚或已发通知: node=%+v err=%v", node, err)
	}
}

func TestCreateGoalsNilTask(t *testing.T) {
	if got := (&Server{}).createGoals(context.Background(), nil, nil); len(got) != 0 {
		t.Fatalf("nil task produced goals: %+v", got)
	}
}
