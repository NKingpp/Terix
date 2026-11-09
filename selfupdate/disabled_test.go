package selfupdate

// 自更新禁用开关的守护测试。
//
// # 为什么这个隐患必须由测试守
//
// 它的失败方式是「静默的」：
//
//	用户点「检查更新」→ 读 Autumn-27/artex 的 latest release
//	→ 下载渗透版二进制 → SHA256 校验**通过**（SUMS 也是那个 release 的）
//	→ 原子替换自己 → 重启后是另一个软件
//
// 全程没有任何错误。没有一条日志会说「你刚刚把 Terix 换成了 ARTEX」。
//
// 所以只有测试能守 —— 而且必须守的是「开关本身」，不是「某个调用路径」。

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// TestSelfUpdateDisabledByDefault 断言自更新默认关闭。
func TestSelfUpdateDisabledByDefault(t *testing.T) {
	if Enabled {
		t.Fatal("Terix 不应启用自更新：上游 Repo 是渗透测试平台（AI 渗透测试控制台），\n" +
			"启用会把 Terix 换成 ARTEX —— 而且 SHA256 校验会通过，全程无错误信号。\n" +
			"恢复条件：Terix 建立自己的发布源，并**同时**把 Repo 指向自己的仓库。")
	}
}

// TestFetchLatestRefusesWhenDisabled 禁用时 FetchLatest 必须返回 ErrDisabled，且不联网。
//
// # 为什么断言 errors.Is 而不是只看 err != nil
//
// 「不联网」这件事无法直接断言，但可以间接证明：
// 传入一个必然 panic 的 http.Client（Transport 为 nil 会 panic），
// 如果它真的尝试发请求就会崩；没崩 = 未发请求。
func TestFetchLatestRefusesWhenDisabled(t *testing.T) {
	if Enabled {
		t.Skip("自更新已启用，本测试的前提不成立")
	}

	// 一个「一旦被使用就崩」的 client —— 证明代码没有走到发请求那一步。
	poison := &http.Client{Transport: poisonTransport{}}

	_, err := FetchLatest(context.Background(), poison)
	if err == nil {
		t.Fatal("禁用时 FetchLatest 必须返回错误，不能静默尝试联网")
	}
	if !errors.Is(err, ErrDisabled) {
		t.Fatalf("应返回 ErrDisabled（让调用方能区分「被下掉」与「网络失败」），实际: %v", err)
	}
}

// TestStageRefusesWhenDisabled 禁用时 Stage 不写文件、不发请求。
func TestStageRefusesWhenDisabled(t *testing.T) {
	if Enabled {
		t.Skip("自更新已启用，本测试的前提不成立")
	}

	poison := &http.Client{Transport: poisonTransport{}}
	rel := &Release{TagName: "v99.0.0"}

	err := Stage(context.Background(), poison, rel, "v1.0.0", nil)
	if err == nil {
		t.Fatal("禁用时 Stage 必须返回错误")
	}
	if !errors.Is(err, ErrDisabled) {
		t.Fatalf("应返回 ErrDisabled，实际: %v", err)
	}
}

// TestRollbackStillAvailable 回滚能力必须保留（禁用只针对「拉取/下载/应用」）。
//
// # 为什么这条也要测
//
// 「禁用自更新」的正确边界是「不换种」，而不是「删掉整个包」。
// Rollback 是运维手动替换二进制后的救命能力 —— 如果它被一起禁掉，
// 那这个改动就从「安全修复」变成了「功能退步」。
func TestRollbackStillAvailable(t *testing.T) {
	// Rollback 不联网、不下载（它只动本地暂存文件），所以禁用时也应可用。
	// 这里只断言「函数存在且签名未变」—— 真正的回滚行为需要真实文件系统，
	// 由 bootstrap 的测试覆盖。
	var _ func() error = Rollback
}

// poisonTransport 一旦被调用就 panic —— 用于证明「没发请求」。
type poisonTransport struct{}

func (poisonTransport) RoundTrip(*http.Request) (*http.Response, error) {
	panic("自更新禁用时不应发起任何 HTTP 请求（已到达 RoundTrip）")
}
