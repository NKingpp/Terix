package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// testRecorder 是 FindingRecorder 的最小实现，直接走 db 层事务。
//
// # 为什么需要它而不是 evidence.New(...)
//
// 这两个测试原本用 evidence.New(...) 作为 FindingRecorder 的唯一实现。
// 而 evidence 包的 Record（行锁 + 流量证据准备）随渗透功能删除（2026-10-29）。
//
// 但这两个测试验证的是**通用契约**：
//  1. 不带 traffic_refs 的 report_finding 也能落库（金融侧同样适用）
//  2. 通知必须在事务提交**之后**发出，不能在事务内
//
// 所以不能删测试 —— 而是换一个「只做通用部分」的 recorder：
// 直接用 ExplorationStore.RecordFinding（它有 WithEvidenceTx 事务 + RecordFindingTx 通知挂点），
// 只是少了 LockTaskEvidenceTx 行锁。
//
// ⚠️ 本测试无并发断言，所以不用行锁不损失覆盖。
// 如果将来给这里加并发断言，要改用带行锁的实现 —— 否则会漏测竞态。
type testRecorder struct{ d *db.DB }

func (r testRecorder) Record(ctx context.Context, in db.RecordFindingInput, _ []db.TrafficRef) (*db.RecordedFinding, error) {
	// refs 被丢弃：ExplorationStore.RecordFinding 不接 traffic refs（prepared=nil）。
	// 本测试只传空 refs（""/[]/null），所以丢弃后行为与原来的 evidence.Store.Record 一致。
	return r.d.Exploration(in.ExplorationID).RecordFinding(ctx, in)
}

type failingFindingRecorder struct{}

func (failingFindingRecorder) Record(context.Context, db.RecordFindingInput, []db.TrafficRef) (*db.RecordedFinding, error) {
	return nil, errors.New("fixture persistence failed")
}

func TestReportFindingOptionalTrafficWithoutCapture(t *testing.T) {
	d := testDB(t)
	// 不用 defer d.Close()：它比 t.Cleanup 先执行，会让 testDB 里注册的
	// notification_events 清理跑在已关闭的连接上（失败且无声）。
	// testDB 已用 t.Cleanup 管收尾，这里不重复关。
	task, err := d.CreateTask("TCP evidence", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// 先删 findings —— DeleteTask 不删它（finding.task_id 是 ON DELETE SET NULL，
		// 故意保留 finding 做审计），测试必须自己清。
		_, _ = d.Exec(`DELETE FROM findings WHERE task_id=$1`, task.ID)
		_ = d.DeleteTask(task.ID)
	}()
	ts := NewToolSet(nil, "")
	ts.ts, ts.taskID = d.Exploration(task.ExplorationID), task.ID
	ts.SetFindingRecorder(testRecorder{d: d})
	notices := 0
	ts.notifyFinding = func(int64, string) { notices++ }
	for _, refs := range []string{"", `,"traffic_refs":[]`, `,"traffic_refs":null`} {
		res, err := ts.addFinding().Call(t.Context(), json.RawMessage(`{"vulnclass":"TCP","severity":"low","summary":"verified TCP fixture","evidence":"command output proves the finding"`+refs+`}`), nil)
		if err != nil || !strings.HasPrefix(res.Flatten(), "finding recorded: ") {
			t.Fatalf("optional traffic rejected: %v %s", err, res.Flatten())
		}
		var out db.RecordedFinding
		if err := json.Unmarshal([]byte(strings.SplitN(res.Flatten(), "\n", 2)[1]), &out); err != nil {
			t.Fatal(err)
		}
		f, err := d.GetFinding(out.FindingID)
		if err != nil || f == nil || len(out.Traffic.Bindings) != 0 || f.Evidence != "command output proves the finding" {
			t.Fatalf("lost non-HTTP evidence: %+v %v", f, err)
		}
	}
	if notices != 3 {
		t.Fatalf("successful reports notified %d times", notices)
	}
}

func TestReportFindingAtomicContract(t *testing.T) {
	old := FindingTrafficBindingEnabled
	FindingTrafficBindingEnabled = func() bool { return true }
	t.Cleanup(func() { FindingTrafficBindingEnabled = old })
	d := testDB(t)
	// 不用 defer d.Close()：它比 t.Cleanup 先执行，会让 testDB 里注册的
	// notification_events 清理跑在已关闭的连接上（失败且无声）。
	// testDB 已用 t.Cleanup 管收尾，这里不重复关。
	task, err := d.CreateTask("report contract", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// 先删 findings —— DeleteTask 不删它（finding.task_id 是 ON DELETE SET NULL，
		// 故意保留 finding 做审计），测试必须自己清。
		_, _ = d.Exec(`DELETE FROM findings WHERE task_id=$1`, task.ID)
		_ = d.DeleteTask(task.ID)
	}()
	ts := NewToolSet(nil, "")
	ts.ts = d.Exploration(task.ExplorationID)
	ts.taskID = task.ID
	ts.worker = "fixture"
	notices := 0
	ts.notifyFinding = func(int64, string) { notices++ }
	call := func(body string) string {
		t.Helper()
		res, err := ts.addFinding().Call(t.Context(), json.RawMessage(body), nil)
		if err != nil {
			t.Fatal(err)
		}
		return res.Flatten()
	}
	ts.SetFindingRecorder(failingFindingRecorder{})
	if got := call(`{"vulnclass":"TEST","summary":"fail","severity":"low","traffic_refs":[{"traffic_id":"x"}]}`); !strings.Contains(got, "persistence failed") {
		t.Fatal(got)
	}
	if notices != 0 || ts.writes.Findings != 0 {
		t.Fatal("notified before commit")
	}
	ts.SetFindingRecorder(nil)
	got := call(`{"vulnclass":"TEST","summary":"legacy report","severity":"low"}`)
	lines := strings.SplitN(got, "\n", 2)
	if len(lines) != 2 {
		t.Fatal(got)
	}
	var out db.RecordedFinding
	if err = json.Unmarshal([]byte(lines[1]), &out); err != nil {
		t.Fatal(err)
	}
	if lines[0] != fmt.Sprintf("finding recorded: %d", out.NodeID) || out.FindingID <= 0 || notices != 1 {
		t.Fatal(got)
	}
	f, err := d.GetFinding(out.FindingID)
	if err != nil || f == nil || f.NodeID == nil || *f.NodeID != out.NodeID {
		t.Fatalf("wrong finding/node mapping: %+v %v", f, err)
	}
	if got = call(`{"vulnclass":"TEST","summary":"no storage","severity":"low","traffic_refs":[{"traffic_id":"x"}]}`); !strings.Contains(got, "未登记") {
		t.Fatal(got)
	}
	if notices != 1 {
		t.Fatal("failed tool triggered reporter")
	}
}
