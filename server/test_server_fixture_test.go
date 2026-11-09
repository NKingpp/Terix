package server

// 通用测试 fixture：一个「最小可用」的 Server（带 pg 连接 + 临时目录 + 空 map）。
//
// # 为什么这个文件存在
//
// 原先有两个通用的测试 fixture 定义在已删除的渗透测试文件里：
//   - server/finding_retests_test.go 的 newRetestServer
//
// 而它们的消费者是**非渗透**的测试：
//   - server/chat_mentions_test.go     的 TestChatMentionPagination
//   - server/conversation_status_test.go 的 TestConversationListRunningState
//
// 两者都只需要「一个带 pg + 临时目录 + 空 map 的 Server」，与复测功能无关。
// 所以把那个 fixture 的核心部分搬到这里，去掉复测专有的注入
// （seedFindingRetester / findingRetestTools / RetesterDefaultPrompt）。
//
// 判据同 query_params.go：fixture 有保留的引用方 → 搬出来，不能随定义它的文件删。

import (
	"context"
	"errors"
	"iter"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
)

// newTestServer 建一个最小的 Server 实例，返回它和一条已入库的 finding id。
//
// finding 用于需要「至少一条漏洞记录」的测试（如提及解析的分页）。
func newTestServer(t *testing.T) (*Server, int64) {
	t.Helper()
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skip("test postgres not configured")
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	td := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{ctx: ctx, m: &Manager{pg: pg, dir: td, tasks: map[string]*Task{}},
		chatBusy: map[string]bool{}, chatCancel: map[string]context.CancelCauseFunc{}}
	// Evidence 的值必须是 "original proof" —— chat_mentions_test.go 的两个测试
	// 断言它出现在提及上下文中。这是原 newRetestServer fixture 的值，别改。
	fid, err := pg.AddFinding(0, 0, "test-server", "测试用漏洞", "high", "summary", "original proof", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		_, _ = pg.DeleteFinding(fid)
		pg.Close()
	})
	return s, fid
}

// scriptedProvider 是 llm.Provider 的测试桩：由调用方提供 complete 函数。
//
// # 为什么改这个名字
//
// 原名 retestProvider —— 名字带「复测」，但实现完全通用（只是把 Complete 转发给
// 调用方给的函数）。它原先定义在已删除的 finding_retests_test.go 里，
// 而消费者是非渗透的 TestChatMentionWorkerReceivesServerDetails。
//
// 改名是为了让「谁还能用」这件事不再被名字误导。
type scriptedProvider struct {
	complete func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error)
}

func (p scriptedProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	return p.complete(ctx, req)
}

func (p scriptedProvider) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		yield(llm.StreamEvent{}, errors.New("test expects non-streaming"))
	}
}

// setChatProvider 给 Server 装一个 scripted chat agent。
//
// 原名 setRetestProvider —— 名字带「复测」，但实现只是「装 chatAgent + 关流式」，
// 与复测无关。消费者是非渗透测试。
func setChatProvider(s *Server, p scriptedProvider) {
	s.chatAgent = agent.NewChatAgent(p, "test", s.m.dir, nil, 10000)
	s.chatAgent.SetNonStreaming(func() bool { return true })
}

// waitChatIdle 等 Server 的对话全部空闲（chatBusy 空）。
//
// 原名 waitRetestIdle —— 它只是在等 chatBusy 清空，与复测无关。
func waitChatIdle(t *testing.T, s *Server) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.chatMu.Lock()
		n := len(s.chatBusy)
		s.chatMu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("conversation did not become idle")
}

// pathValueRequest 建一个带 {id} 路径变量的请求。
//
// 原名 retestRequest —— 只是 httptest 的构造 helper，与复测无关。
func pathValueRequest(handler http.HandlerFunc, method string, id int64, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/", strings.NewReader(body))
	r.SetPathValue("id", strconv.FormatInt(id, 10))
	w := httptest.NewRecorder()
	handler(w, r)
	return w
}
