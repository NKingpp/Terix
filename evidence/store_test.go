package evidence

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

func evidenceFixture(t *testing.T) (*Store, db.RecordFindingInput, string) {
	t.Helper()
	pg, err := db.Open(evidenceTestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.Close() })
	task, err := pg.CreateTask("evidence integration "+t.Name(), "local fixtures", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.DeleteFindingsByTask(task.ID); pg.DeleteTask(task.ID) })
	dir := t.TempDir()
	// 不再开 traffic.Open(...)：Store 的 Traffic 字段已随渗透功能删除，
	// 而现在快照由 seedSnapshot 直接构造，不经过读流量那一步。
	store := New(pg, filepath.Join(dir, "evidence"))
	in := db.RecordFindingInput{TaskID: task.ID, ExplorationID: task.ExplorationID, Worker: "test", VulnClass: "TEST", Name: "Evidence fixture", Severity: "low", Summary: "local test"}
	return store, in, dir
}

// seedSnapshot 直接构造「一条被 finding 引用的证据快照」：
// DB 行 + 绑定行 + 磁盘 blob 三处一起造。
//
// # 为什么不用 s.Record(...)
//
// Record 随渗透功能删除。而它做的「写漏洞 + 读流量 + 写 blob + 插行 + 绑定」中，
// 本测试只需要后三步 —— 所以拆开手工做。
//
// 三步各自都有现成函数可用（所以不是「复刻生产逻辑」，是「拼接现成零件」）：
//   - 写 blob  → s.writeBody（包内私有，本测试是同包）
//   - 插行     → db.InsertEvidenceSnapshotTx
//   - 绑定     → db.AddFindingTrafficTx
func seedSnapshot(t *testing.T, s *Store, findingID int64, trafficID string, respBody []byte) db.TrafficEvidenceSnapshot {
	t.Helper()
	ctx := context.Background()
	reqBody := []byte("{}")
	snap := db.TrafficEvidenceSnapshot{
		SourceTrafficID: trafficID,
		CapturedAt:      time.Now().Unix(),
		URL:             "https://fixture.local/" + trafficID,
		Method:          "POST",
		Status:          200,
		ContentType:     "application/octet-stream",
		ReqHead:         "POST /test HTTP/1.1\nHost: fixture.local\n",
		RespHead:        "HTTP 200\n",
		ReqLen:          int64(len(reqBody)),
		RespLen:         int64(len(respBody)),
	}
	// writeBody 会算 hash 并落到 blobs/<hash[:2]>/<hash>.bin。
	// expectedHash 传 "" 表示「算但不校验」；expectedLength 必须准确。
	var reqHash, respHash string
	if err := s.DB.WithEvidenceTx(ctx, func(*sql.Tx) error {
		var err error
		if reqHash, err = s.writeBody(bytes.NewReader(reqBody), snap.ReqLen, ""); err != nil {
			return err
		}
		respHash, err = s.writeBody(bytes.NewReader(respBody), snap.RespLen, "")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	snap.ReqHash, snap.RespHash = reqHash, respHash
	snap = snap.Normalize()
	snap.ID = db.TrafficSnapshotID(snap)
	if err := s.DB.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		if err := db.InsertEvidenceSnapshotTx(tx, snap); err != nil {
			return err
		}
		return db.AddFindingTrafficTx(tx, findingID,
			[]db.PreparedTrafficEvidence{{Ref: db.TrafficRef{TrafficID: trafficID}, Snapshot: snap}})
	}); err != nil {
		t.Fatal(err)
	}
	return snap
}

// TestEvidenceGCGraceAndActiveRestore 测快照 GC 的宽限期与活跃恢复。
//
// # 为什么不再用 s.Record(...) 造数据
//
// Record 随渗透功能删除（2026-10-29）—— 它是「写漏洞 + 准备流量证据」。
// 而本测试只需要两样东西：
//  1. DB 里一条被 finding_traffic_bindings 引用的 traffic_evidence_snapshots 行
//  2. 磁盘上对应的 blob 文件
//
// 所以改成直接构造（seedSnapshot），不经过 Record。
//
// ⚠️ 这三处状态必须一起造对，少一处会测出假行为：
//   - 漏了 DB 行 → Collect 的 refs 里没这个 hash → blob 会被当孤儿删掉
//   - 漏了绑定行 → Collect 把它标为 unreferenced → 宽限期断言假通过
//   - 漏了 blob   → WalkDir 扫不到文件 → "grace ignored" 断言假通过
func TestEvidenceGCGraceAndActiveRestore(t *testing.T) {
	s, in, _ := evidenceFixture(t)
	ctx := context.Background()
	f, err := s.DB.Exploration(in.ExplorationID).RecordFinding(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	snap := seedSnapshot(t, s, f.FindingID, "gc", []byte("unique unreferenced gc body"))
	archive := t.TempDir()
	if err = s.CopySnapshots(ctx, []db.TrafficEvidenceSnapshot{snap}, archive); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.DeleteFinding(f.FindingID); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err = s.Collect(ctx, now); err != nil {
		t.Fatal(err)
	}
	path, err := hashPath(s.Dir, snap.RespHash)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Collect(ctx, now.Add(23*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(path); err != nil {
		t.Fatal("GC grace ignored", err)
	}
	if err = s.Collect(ctx, now.Add(25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("orphan was not collected", err)
	}
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		finished <- s.WithInstalledSnapshots(ctx, []db.TrafficEvidenceSnapshot{snap}, archive, func() error { close(entered); <-release; return nil })
	}()
	<-entered
	short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	err = s.Collect(short, now.Add(72*time.Hour))
	cancel()
	close(release)
	if err == nil {
		t.Fatal("GC entered during active restore")
	}
	if err = <-finished; err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(path); err != nil {
		t.Fatal("active restore lost body", err)
	}
	// A portable package cannot substitute metadata while retaining its old ID.
	bad := snap
	bad.URL = "http://tampered.local/"
	if err = s.InstallSnapshots(ctx, []db.TrafficEvidenceSnapshot{bad}, archive); err == nil {
		t.Fatal("accepted tampered snapshot")
	}
}
