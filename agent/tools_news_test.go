package agent

// get_news / get_fundamentals 的自检：只验**不依赖网络**的契约。
//
// 取数本身靠 sidecar 集成验证（见交付报告的 curl），这里锁住三件容易写错的事：
//  1. crypto 必须被**显式拒绝**（不是返回空结果 —— 空结果会被读成「无消息」）
//  2. 非法 report_type 必须报错（不是静默退回 abstract）
//  3. 两个工具都必须在 marketToolMetas 里、且 AssetClasses 不含 crypto
//
// 第 3 条与 TestMarketToolMetasCoverAllTools 互补：那个验「数量与超时」，
// 这个验「能力边界写对了」。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
)

// TestGetNewsRejectsCrypto crypto 不在 get_news 的 AssetClasses 里，必须报错。
func TestGetNewsRejectsCrypto(t *testing.T) {
	mts := NewMarketToolSet(nil)
	res, err := mts.getNews().Call(context.Background(),
		json.RawMessage(`{"symbol":"BTC-USDT","market":"crypto"}`), nil)
	if err != nil {
		t.Fatalf("Call 返回 error: %v", err)
	}
	msg := resultText(t, res)
	if !strings.Contains(msg, "不支持 market=\"crypto\"") {
		t.Errorf("crypto 应被显式拒绝并说明原因，实际: %s", msg)
	}
}

// TestGetFundamentalsRejectsCrypto 同上。
func TestGetFundamentalsRejectsCrypto(t *testing.T) {
	mts := NewMarketToolSet(nil)
	res, err := mts.getFundamentals().Call(context.Background(),
		json.RawMessage(`{"symbol":"BTC-USDT","market":"crypto"}`), nil)
	if err != nil {
		t.Fatalf("Call 返回 error: %v", err)
	}
	if msg := resultText(t, res); !strings.Contains(msg, "不支持 market=\"crypto\"") {
		t.Errorf("crypto 应被显式拒绝，实际: %s", msg)
	}
}

// TestGetFundamentalsRejectsBadReportType 非法报表类型必须报错。
//
// 静默退回 abstract 会让分析师以为「我要的利润表就是这些摘要指标」，
// 从而把摘要当成三大报表下结论。
func TestGetFundamentalsRejectsBadReportType(t *testing.T) {
	mts := NewMarketToolSet(nil)
	res, err := mts.getFundamentals().Call(context.Background(),
		json.RawMessage(`{"symbol":"600519","report_type":"利润"}`), nil)
	if err != nil {
		t.Fatalf("Call 返回 error: %v", err)
	}
	if msg := resultText(t, res); !strings.Contains(msg, "report_type 非法") {
		t.Errorf("非法报表类型应报错，实际: %s", msg)
	}
}

// TestNewsAndFundamentalMetaExcludeCrypto AssetClasses 不含 crypto。
//
// 这条比 handler 的校验更根本：AssetClasses 决定「哪些资产类别能用该工具」，
// 写错会让 crypto 分析师拿到一个永远空的工具。
func TestNewsAndFundamentalMetaExcludeCrypto(t *testing.T) {
	for _, key := range []string{"get_news", "get_fundamentals"} {
		meta, ok := marketToolMetas[key]
		if !ok {
			t.Fatalf("marketToolMetas 缺 %q", key)
		}
		for _, ac := range meta.AssetClasses {
			if ac == AssetClassCrypto {
				t.Errorf("%q 的 AssetClasses 含 crypto —— 新闻/财报没有加密源，"+
					"列进来会让空结果被读成「无消息」", key)
			}
		}
		if len(meta.Channels) == 0 {
			t.Errorf("%q 必须有 Channels（它走外部 akshare 请求）", key)
		}
	}
}

// resultText 取 actool.Result 的纯文本（Flatten 拼全部 Content 块）。
func resultText(t *testing.T, res actool.Result) string {
	t.Helper()
	return res.Flatten()
}
