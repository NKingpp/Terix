package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// 总览「目标管理」的人工 CRUD 接口。与 agent 侧的 set_goals 工具写同一批 goal 节点,
// 但入口是人类在 UI 上直接增删改;新增/修改后复用「复活任务」逻辑(admitTask resume:
// 终态→running、解除暂停、必要时排队),删除不复活(按产品决策)。每个变更 handler 都走
// beginTaskOperation/decInflight,避免与任务删除竞态(与意图 CRUD 一致)。

// listGoals 返回本任务的全部目标(text/vulnclass/state 拆好),供目标管理卡片渲染。
func (s *Server) listGoals(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	goals, err := t.Store.ListByKind(db.KindGoal, 10000)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	dtos := goalDTOs(goals)
	s.fillGoalAcceptMeta(t, dtos)
	writeJSON(w, 200, map[string]any{"goals": dtos})
}

// fillGoalAcceptMeta 给目标补 completed_at 与最近一次人工验收的验收人/理由。
//
// 单独两条轻查询而不动 nodeCols/scanNode：后者被 20+ 处复用，不为一屏 UI 改公共扫描。
func (s *Server) fillGoalAcceptMeta(t *Task, dtos []GoalDTO) {
	if len(dtos) == 0 || t.Store == nil || t.Store.DB() == nil {
		return
	}
	byID := make(map[int64]*GoalDTO, len(dtos))
	for i := range dtos {
		if id, err := strconv.ParseInt(dtos[i].ID, 10, 64); err == nil {
			byID[id] = &dtos[i]
		}
	}
	if rows, err := t.Store.DB().Query(`SELECT id, COALESCE(completed_at,'') FROM exploration_nodes
		WHERE exploration_id=$1 AND kind='goal'`, t.Store.ID()); err == nil {
		for rows.Next() {
			var id int64
			var ca string
			if rows.Scan(&id, &ca) == nil {
				if g, ok := byID[id]; ok {
					g.CompletedAt = ca
				}
			}
		}
		rows.Close()
	}
	if rows, err := t.Store.DB().Query(`SELECT node_id, COALESCE(worker,''), COALESCE(json_extract(metadata,'$.reason'),'')
		FROM activity WHERE exploration_id=$1 AND kind='goal_accepted' ORDER BY id DESC LIMIT 500`, t.Store.ID()); err == nil {
		seen := make(map[int64]bool, len(dtos))
		for rows.Next() {
			var id int64
			var worker, reason string
			if rows.Scan(&id, &worker, &reason) != nil || seen[id] {
				continue
			}
			seen[id] = true
			if g, ok := byID[id]; ok {
				g.AcceptedBy = worker
				g.AcceptedReason = reason
			}
		}
		rows.Close()
	}
}

// addGoal 人工新增一个目标:落库(挂到任务根 spawns 下)→ 记一条「新增了目标」触发唤醒
// planner → 复活任务,让规划者据新目标重判是否达成。
func (s *Server) addGoal(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "任务正在删除,无法新增目标")
		return
	}
	defer s.engine.decInflight(t.ID)

	var body struct {
		Text      string `json:"text"`
		VulnClass string `json:"vulnclass"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeErr(w, 400, "目标内容不能为空")
		return
	}
	payload := map[string]any{"text": text}
	if vc := strings.TrimSpace(body.VulnClass); vc != "" {
		payload["vulnclass"] = vc
	}
	id, err := t.Store.AddGoal(payload, "human")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if of, _ := t.Store.OriginFactID(); of > 0 && id > 0 {
		_ = t.Store.Link(of, db.RelSpawns, id) // goal descends from the task root (origin fact)
	}
	t.NotifyGoal([]string{text}) // 记「人新增了目标:…」触发并唤醒 planner
	s.reviveTask(t)              // 把已完成/暂停的任务拉回运行态继续跑
	node, _ := t.Store.GetNode(id)
	if node == nil {
		writeErr(w, 500, "目标写入后读取失败")
		return
	}
	writeJSON(w, 200, goalDTO(node))
}

// editGoal 人工修改一个目标文本(及 vulnclass):改库 → 记「用户修改了目标由 old 变为 new」
// 触发唤醒 planner → 复活任务,让规划者据新目标调整方向。
func (s *Server) editGoal(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "任务正在删除,无法修改目标")
		return
	}
	defer s.engine.decInflight(t.ID)

	gid, err := strconv.ParseInt(r.PathValue("gid"), 10, 64)
	if err != nil || gid <= 0 {
		writeErr(w, 400, "bad goal id")
		return
	}
	var body struct {
		Text      string `json:"text"`
		VulnClass string `json:"vulnclass"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeErr(w, 400, "目标内容不能为空")
		return
	}
	change, err := t.Store.UpdateGoalPayloadWithAudit(gid, text, strings.TrimSpace(body.VulnClass))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, 404, "目标不存在")
		} else {
			writeErr(w, 500, err.Error())
		}
		return
	}
	if change.Changed {
		t.NotifyGoalEdited(change.OldText, change.NewText)
		s.reviveTask(t)
	}
	updated, _ := t.Store.GetNode(gid)
	if updated == nil {
		writeErr(w, 500, "目标更新后读取失败")
		return
	}
	writeJSON(w, 200, goalDTO(updated))
}

// deleteGoal 人工删除一个目标(硬删除,级联删边/锚点):删库 → 记「用户删除了该目标 X」
// 触发唤醒 planner 据此重判剩余目标。按产品决策,删除【不】复活任务。
func (s *Server) deleteGoal(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "任务正在删除,无法删除目标")
		return
	}
	defer s.engine.decInflight(t.ID)

	gid, err := strconv.ParseInt(r.PathValue("gid"), 10, 64)
	if err != nil || gid <= 0 {
		writeErr(w, 400, "bad goal id")
		return
	}
	node, err := t.Store.GetNode(gid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if node == nil || node.Kind != db.KindGoal {
		writeErr(w, 404, "目标不存在")
		return
	}
	text := goalDTO(node).Text
	if err := t.Store.DeleteGoal(gid); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	t.NotifyGoalDeleted(text) // 记「人删除了该目标:…」触发并唤醒 planner(不复活任务)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// acceptGoal 人工验收一个目标：先跑机器核验（口径见 server/goal_accept.go），通过后把
// 目标置 met 并写审计；全部目标达成时把任务落 done。
//
// 这是金融任务唯一的收敛路径 —— agent 侧 prove_goal/goal_met 在金融域被硬拦截
// (agent/tools.go:1510/1543)，engine 的 goalless 自动 done 也显式排除金融任务。
// 与 editGoal 的区别：不复活任务（验收只会让任务进入终态，不该把已结束的任务拉回）。
func (s *Server) acceptGoal(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "任务正在删除,无法验收目标")
		return
	}
	defer s.engine.decInflight(t.ID)

	gid, err := strconv.ParseInt(r.PathValue("gid"), 10, 64)
	if err != nil || gid <= 0 {
		writeErr(w, 400, "bad goal id")
		return
	}
	var body struct {
		Reason     string             `json:"reason"`
		EvidenceID int64              `json:"evidence_id"`
		Acceptance *db.GoalAcceptance `json:"acceptance"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	body.Reason = strings.TrimSpace(body.Reason)
	if body.Reason == "" {
		writeErr(w, 400, "验收理由必填")
		return
	}
	node, err := t.Store.GetNode(gid)
	if err != nil || node == nil || node.Kind != db.KindGoal {
		writeErr(w, 404, "目标不存在")
		return
	}
	if node.State == "met" {
		allMet, _ := t.Store.AllGoalsMet()
		writeJSON(w, 200, map[string]any{"goal": goalDTO(node), "all_met": allMet, "changed": false})
		return
	}

	var stored struct {
		Acceptance *db.GoalAcceptance `json:"acceptance"`
	}
	_ = json.Unmarshal(node.Payload, &stored)
	if body.Acceptance != nil && stored.Acceptance != nil {
		writeErr(w, 400, "该目标已配置验收口径，不允许在验收时改写")
		return
	}
	effective := db.GoalAcceptance{Kind: "flat", Source: "evidence"}
	switch {
	case stored.Acceptance != nil:
		effective = *stored.Acceptance
	case body.Acceptance != nil:
		effective = *body.Acceptance
	}
	if err := effective.Normalize(); err != nil {
		writeErr(w, 400, err.Error())
		return
	}

	passed, observed, why := verifyGoalAcceptance(r.Context(), t, node, effective, body.EvidenceID)
	if !passed {
		writeJSON(w, 409, map[string]any{
			"error":      "机器核验未通过：" + why,
			"code":       "GOAL_ACCEPT_VERIFY_FAILED",
			"observed":   observed,
			"acceptance": effective,
		})
		return
	}
	res, err := t.Store.AcceptGoalWithAudit(gid, body.Reason, body.EvidenceID, body.Acceptance, observed)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	allMet, _ := t.Store.AllGoalsMet()
	if allMet {
		if won, err := s.m.SetTaskStatusGuarded(t.ID, "done"); err == nil && won {
			s.engine.cancelExec(t.ID, agent.AbortGoalMet)
			s.engine.emitActivity(t, db.Activity{
				Worker: "human", Kind: "text", Summary: "人工验收通过，全部目标达成，任务结束",
			})
		}
	}
	goal := res.Goal
	if goal == nil {
		goal = node
	}
	one := []GoalDTO{goalDTO(goal)}
	s.fillGoalAcceptMeta(t, one)
	writeJSON(w, 200, map[string]any{
		"goal": one[0], "all_met": allMet, "changed": res.Changed, "verification": observed,
	})
}
