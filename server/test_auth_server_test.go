package server

// 通用的「带鉴权的 Server」测试 fixture。
//
// # 为什么这个文件存在
//
// 它原先叫 trafficEvidenceServer，定义在 server/finding_traffic_test.go 里。
// 而那个文件随渗透功能删除（2026-10-29），但 server/notify_api_test.go 仍在用这个 fixture ——
// 它只需要「一个能跑 HTTP handler 的 Server + 一个签名用的 token」，
// 与流量证据无关。
//
// 所以取回核心部分、去掉渗透专有配置（m.traffic / traffic.Open / 证据写入），
// 改名为通用的 newAuthServer。
//
// 判据同 query_params.go：fixture 有保留的引用方 → 搬出来，不能随定义它的文件删。

import (
	"context"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// newAuthServer 建一个「带 PG + 任务 + JWT 鉴权」的 Server，
// 返回它、它造的那条任务、以及一个发请求的 helper。
func newAuthServer(t *testing.T) (*Server, *Task, func(method, path, body string) *httptest.ResponseRecorder) {
	t.Helper()
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	task, err := m.CreateTask("auth server fixture", "local fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	tid, _ := strconv.ParseInt(task.ID, 10, 64)
	t.Cleanup(func() {
		m.pg.Exec(`DELETE FROM task_archives WHERE task_id=$1`, tid)
		m.pg.DeleteTask(tid)
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := New(ctx, m, t.TempDir(), t.TempDir(), t.TempDir())
	s.archiveWG.Wait()
	// 服务的后台任务在 cancelled ctx 下会立即退出；等它们结束再复用这个 fixture。
	<-s.side.done
	token, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	return s, task, request
}

// 说明：本 fixture 不再返回 *db.RecordedFinding（原 trafficEvidenceServer 的第二个返回值）。
// notify_api_test.go 自己 CreateTask 造数据，不需要预置的 finding。
var _ = db.RecordedFinding{}
