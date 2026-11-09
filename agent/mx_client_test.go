package agent

// 妙想（MX）客户端的单元测试 + 一个真实调用冒烟测试。
//
// # 测试分层
//
//   - 纯离线测试（TestMX*）：用 httptest 起假上游，验证解析/错误码/脱敏。
//     这些必须无条件通过 —— 它们是 CI 的基础。
//   - 真实调用（TestMXLive*）：需要 MX_APIKEY，缺 key 时 **skip** 而不是 fail。
//     真实上游随时可能改结构，把它设成硬断言会让 CI 变成随机红。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// mxTestClient 构造一个指向假上游的客户端。
func mxTestClient(t *testing.T, h http.HandlerFunc) *MXClient {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &MXClient{apiKey: "test-key-1234", baseURL: srv.URL, hc: &http.Client{Timeout: 5 * time.Second}}
}

// TestMXQueryParsesTables 锁定「数字 id 指标 key → 可读列名」的解析。
//
// 这是整个落地最容易出错的地方：指标 key 是 `325898` 这种数字 id，
// 拿不到 nameMap 时如果「猜」成「收盘价」，分析师会把别的指标当收盘价用。
// 所以断言三条：有 nameMap 时用中文名、无 nameMap 时保留 id、note 提示不可猜。
func TestMXQueryParsesTables(t *testing.T) {
	withNameMap := `{"success":true,"status":0,"code":0,"message":"ok",
	 "data":{"data":{"searchDataResultDTO":{"questionId":"q1","dataTableDTOList":[
	   {"code":"600519.SH","entityName":"贵州茅台(600519.SH)","title":"贵州茅台(600519.SH)的收盘价",
	    "table":{"325898":["1392.05元","1354元"],"headName":["2026-10-30(日)","2026-10-29(日)"]},
	    "rawTable":{"325898":["1392.05","1354.0"],"headName":["2026-10-30","2026-10-29"]},
	    "nameMap":{"325898":"收盘价"},"indicatorOrder":["325898"],
	    "fieldSet":[{"returnCode":"325898","returnName":"收盘价","unitName":"元"}]}
	 ]}}}}`

	c := mxTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("apikey"); got != "test-key-1234" {
			t.Errorf("apikey header = %q, want test-key-1234", got)
		}
		fmt.Fprint(w, withNameMap)
	})
	res, err := c.Query(context.Background(), "贵州茅台最近5个交易日收盘价")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.TableCount != 1 {
		t.Fatalf("TableCount = %d, want 1", res.TableCount)
	}
	tb := res.Tables[0]
	if tb.Code != "600519.SH" || tb.Entity != "贵州茅台(600519.SH)" {
		t.Errorf("code/entity = %q/%q", tb.Code, tb.Entity)
	}
	if tb.HeadLabel != "日期" {
		t.Errorf("HeadLabel = %q, want 日期", tb.HeadLabel)
	}
	want := []string{"日期", "收盘价"}
	if len(tb.Columns) != 2 || tb.Columns[0] != want[0] || tb.Columns[1] != want[1] {
		t.Fatalf("Columns = %v, want %v", tb.Columns, want)
	}
	if tb.Units["收盘价"] != "元" {
		t.Errorf("Units[收盘价] = %q, want 元", tb.Units["收盘价"])
	}
	// 值取 rawTable（无单位），单位单独在 Units 里。
	if got := tb.Rows[0]["收盘价"]; got != "1392.05" {
		t.Errorf("首行收盘价 = %q, want 1392.05（rawTable 优先）", got)
	}
	if len(tb.Rows) != 2 || tb.Rows[1]["日期"] != "2026-10-29" {
		t.Errorf("Rows = %v", tb.Rows)
	}
	if res.Note != "" {
		t.Errorf("Note 应为空（指标名都解析出来了），实际 %q", res.Note)
	}
}

// TestMXQueryUnnamedIndicatorKeepsID 指标没有名字时**保留 id 并报警**，绝不猜。
func TestMXQueryUnnamedIndicatorKeepsID(t *testing.T) {
	body := `{"success":true,"status":0,"data":{"data":{"searchDataResultDTO":{"dataTableDTOList":[
	   {"code":"X","entityName":"X","title":"X 的表",
	    "table":{"999":["1"],"headName":["2026-10-30"]}}]}}}}`
	c := mxTestClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
	res, err := c.Query(context.Background(), "某个没有名字的指标")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.Tables[0].Columns[1] != "999" {
		t.Errorf("未命名指标列名 = %q, want 原始 id \"999\"（不得猜测含义）", res.Tables[0].Columns[1])
	}
	if !strings.Contains(res.Note, "999") {
		t.Errorf("Note 应提示 999 指标名缺失，实际 %q", res.Note)
	}
}

// TestMXNoDataIsNoDataError 空 DTO 列表必须变成 MX_NO_DATA，且被 IsNoDataError 识别。
func TestMXNoDataIsNoDataError(t *testing.T) {
	body := `{"success":true,"status":0,"data":{"data":{"searchDataResultDTO":{"dataTableDTOList":[]}}}}`
	c := mxTestClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
	_, err := c.Query(context.Background(), "不存在的公司 xyz 的收盘价")
	if err == nil {
		t.Fatal("空表应返回错误")
	}
	me, ok := err.(*MarketError)
	if !ok || me.Code != ErrMXNoData {
		t.Fatalf("err = %v, want MarketError{MX_NO_DATA}", err)
	}
	if me.Retryable {
		t.Error("MX_NO_DATA 应不可重试（重试同一个问句无用）")
	}
	if !IsNoDataError(err) {
		t.Error("MX_NO_DATA 必须被 IsNoDataError 识别")
	}
}

// TestMXAuthFailedAndNoKeyLeak 鉴权失败要归到 MX_AUTH_FAILED，且错误串里不能出现 key。
//
// 实测上游：HTTP 200 + status=114「API密钥不存在或已失效」。
// 只判 HTTP 会把这条最需要人说清楚的配置错误归成「上游故障」。
func TestMXAuthFailedAndNoKeyLeak(t *testing.T) {
	const key = "super-secret-key-abcdef"
	// 故意在 message 里回显 key —— 上游解析失败时真的会这样。
	body := `{"success":false,"status":114,"code":114,` +
		`"message":"API密钥不存在或已失效: ` + key + `","data":null}`
	c := mxTestClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
	c.apiKey = key

	_, err := c.Query(context.Background(), "贵州茅台收盘价")
	if err == nil {
		t.Fatal("鉴权失败应返回错误")
	}
	me, ok := err.(*MarketError)
	if !ok || me.Code != ErrMXAuthFailed {
		t.Fatalf("err = %v, want MarketError{MX_AUTH_FAILED}", err)
	}
	if me.Retryable {
		t.Error("MX_AUTH_FAILED 应不可重试")
	}
	if strings.Contains(err.Error(), key) {
		t.Fatalf("错误串里泄漏了 apiKey: %s", err.Error())
	}
	if IsNoDataError(err) {
		t.Error("鉴权失败不是「无数据」—— 两者的后续动作完全不同")
	}

	// 未携带 key 的变体（status=115）同样归鉴权问题。
	body115 := `{"success":false,"status":115,"code":115,"message":"请求未携带API密钥","data":null}`
	c2 := mxTestClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body115) })
	_, err = c2.Query(context.Background(), "x")
	if me, ok := err.(*MarketError); !ok || me.Code != ErrMXAuthFailed {
		t.Fatalf("status=115 err = %v, want MX_AUTH_FAILED", err)
	}
}

// TestMXNotConfigured 没有 apiKey 时必须明确报错，不 panic、不发请求。
func TestMXNotConfigured(t *testing.T) {
	called := false
	c := mxTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		fmt.Fprint(w, `{}`)
	})
	c.apiKey = "" // 抹掉 key，模拟 MX_APIKEY 未配置
	_, err := c.Query(context.Background(), "贵州茅台收盘价")
	me, ok := err.(*MarketError)
	if !ok || me.Code != ErrMXNotConfigured {
		t.Fatalf("err = %v, want MarketError{MX_NOT_CONFIGURED}", err)
	}
	if called {
		t.Error("未配置时不该发出真实请求")
	}
}

// TestMXNewsParsesAndDedupes 资讯解析：字段归一 + 去重 + 空标题用正文首句。
func TestMXNewsParsesAndDedupes(t *testing.T) {
	body := `{"status":0,"data":{"data":{"llmSearchResponse":{"data":[
	  {"title":"贵州茅台：第三季度净利润199.51亿元","content":"...","date":"2026-10-30 18:43:00",
	   "informationType":"INV_NEWS","source":"东财","jumpUrl":"http://x"},
	  {"title":"贵州茅台：第三季度净利润199.51亿元","content":"...","date":"2026-10-30 18:43:00",
	   "informationType":"INV_NEWS"},
	  {"title":"","content":"贵州茅台发布董事会决议公告。第二句无关。","date":"2026-10-31",
	   "informationType":"NOTICE"},
	  {"title":"强烈推荐：以动销定投放","content":"...","date":"2026-09-21",
	   "informationType":"REPORT","emRatingName":"强烈推荐","insName":"诚通证券"},
	  {"title":"","content":""}
	]}}}}`
	c := mxTestClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
	items, err := c.NewsSearch(context.Background(), "贵州茅台最新消息")
	if err != nil {
		t.Fatalf("NewsSearch: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("去重后条数 = %d, want 3（1 条完全重复 + 1 条全空应被丢）", len(items))
	}
	if items[1].Type != "NOTICE" {
		t.Errorf("第 2 条 type = %q, want NOTICE", items[1].Type)
	}
	if items[1].Title != "贵州茅台发布董事会决议公告" {
		t.Errorf("空标题应取正文首句，实际 %q", items[1].Title)
	}
	if items[2].Rating != "强烈推荐" || items[2].Institution != "诚通证券" {
		t.Errorf("研报字段 = %q/%q", items[2].Rating, items[2].Institution)
	}
}

// TestMXNewsNoData 零条目 → MX_NO_DATA（不是空切片 + nil）。
func TestMXNewsNoData(t *testing.T) {
	body := `{"status":0,"data":{"data":{"llmSearchResponse":{"data":[]}}}}`
	c := mxTestClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
	_, err := c.NewsSearch(context.Background(), "zzzqqq 最新消息")
	me, ok := err.(*MarketError)
	if !ok || me.Code != ErrMXNoData || !IsNoDataError(err) {
		t.Fatalf("err = %v, want MarketError{MX_NO_DATA} 且被 IsNoDataError 识别", err)
	}
}

// TestMXToolsRegistered 两个工具必须注册且 meta 齐备（与 TestMarketToolMetasCoverAllTools 互补）。
func TestMXToolsRegistered(t *testing.T) {
	mts := NewMarketToolSet(nil)
	var names []string
	for _, tl := range mts.Tools() {
		names = append(names, tl.Name())
	}
	for _, want := range []string{"mx_query", "mx_news"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Errorf("工具 %q 未注册（Tools() 里没有）", want)
		}
		meta, ok := marketToolMetas[want]
		if !ok {
			t.Fatalf("marketToolMetas 缺 %q", want)
		}
		if meta.Channels[0] != ChannelMX {
			t.Errorf("%q 的 Channel = %v, want [mx]", want, meta.Channels)
		}
		for _, ac := range meta.AssetClasses {
			if ac == "crypto" {
				t.Errorf("%q 的 AssetClasses 含 crypto —— 妙想没有加密数据", want)
			}
		}
	}
}

// TestMXToolDescriptionWarnsNaturalLanguage 工具描述必须写明「自然语言，不是结构化参数」。
//
// 没有这句话，模型会照着 get_quote 的形状传 {"symbol":"600519"}，
// 而妙想会把它当问句解析出一张意图漂移的表 —— **看起来成功了**，
// 比报错危险得多。
func TestMXToolDescriptionWarnsNaturalLanguage(t *testing.T) {
	mts := NewMarketToolSet(nil)
	for _, name := range []string{"mx_query", "mx_news"} {
		var desc string
		for _, tl := range mts.Tools() {
			if tl.Name() == name {
				desc = tl.Description()
			}
		}
		if desc == "" {
			t.Fatalf("%s 没有描述", name)
		}
		if !strings.Contains(desc, "自然语言") {
			t.Errorf("%s 的描述必须说明「自然语言查询」，实际:\n%s", name, desc)
		}
	}
}

// TestMXToolCallWithFakeUpstream 工具 handler 端到端（假上游）：
// 确认 mx_query 真能出表、mx_news 真能出条目，且 apiKey 不出现在任何返回里。
func TestMXToolCallWithFakeUpstream(t *testing.T) {
	const key = "tool-key-secret-9"
	queryBody := `{"success":true,"status":0,"data":{"data":{"searchDataResultDTO":{"dataTableDTOList":[
	   {"code":"600519.SH","entityName":"贵州茅台","title":"收盘价",
	    "table":{"1":["1392.05元"],"headName":["2026-10-30(日)"]},
	    "rawTable":{"1":["1392.05"],"headName":["2026-10-30"]},
	    "nameMap":{"1":"收盘价"}}]}}}`
	newsBody := `{"status":0,"data":{"data":{"llmSearchResponse":{"data":[
	   {"title":"净利+3.78%","date":"2026-10-30","informationType":"NOTICE","content":"..."}]}}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/query"):
			fmt.Fprint(w, queryBody)
		case strings.HasSuffix(r.URL.Path, "/news-search"):
			fmt.Fprint(w, newsBody)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	mts := NewMarketToolSet(nil)
	mts.SetMXClient(&MXClient{apiKey: key, baseURL: srv.URL, hc: &http.Client{Timeout: 5 * time.Second}})

	var queryTool, newsTool = mts.mxQuery(), mts.mxNews()
	res, err := queryTool.Call(context.Background(), json.RawMessage(`{"query":"贵州茅台收盘价"}`), nil)
	if err != nil {
		t.Fatalf("mx_query Call: %v", err)
	}
	if got := resultText(t, res); !strings.Contains(got, "1392.05") {
		t.Fatalf("mx_query 返回里没有数据: %s", got)
	}
	if strings.Contains(resultText(t, res), key) {
		t.Fatal("mx_query 返回里泄漏了 apiKey")
	}

	res2, err := newsTool.Call(context.Background(), json.RawMessage(`{"query":"贵州茅台","limit":5}`), nil)
	if err != nil {
		t.Fatalf("mx_news Call: %v", err)
	}
	if !strings.Contains(resultText(t, res2), "净利+3.78%") {
		t.Fatalf("mx_news 返回里没有条目: %s", resultText(t, res2))
	}
}

// TestMXLiveSmoke 真实调用冒烟（需 MX_APIKEY，缺则 skip）。
//
// 断言只要求「非空且结构对」—— 不锁定具体数值，上游数据每天在变。
func TestMXLiveSmoke(t *testing.T) {
	if strings.TrimSpace(os.Getenv("MX_APIKEY")) == "" {
		t.Skip("MX_APIKEY 未配置，跳过真实调用冒烟")
	}
	c := NewMXClient("")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	res, err := c.Query(ctx, "贵州茅台最近5个交易日收盘价")
	if err != nil {
		t.Fatalf("live mx_query: %v", err)
	}
	if res.TableCount == 0 || len(res.Tables[0].Rows) == 0 {
		t.Fatalf("live mx_query 返回空表: %+v", res)
	}
	t.Logf("live mx_query: %d 张表，首表 %q 列=%v 首行=%v",
		res.TableCount, res.Tables[0].Name, res.Tables[0].Columns, res.Tables[0].Rows[0])

	items, err := c.NewsSearch(ctx, "贵州茅台最新消息")
	if err != nil {
		t.Fatalf("live mx_news: %v", err)
	}
	if len(items) < 5 {
		t.Fatalf("live mx_news 只返回 %d 条，期望 >= 5", len(items))
	}
	t.Logf("live mx_news: %d 条，首条 %q (%s %s)", len(items), items[0].Title, items[0].Type, items[0].Date)
}

// TestIsStructuralGarbage 拦住「表格原始文本当资讯」的脏数据。
//
// # 为什么需要这条
//
// 实测（2026-10-31）：问句「贵州茅台最新消息」的首条返回
// `|涉案物品信息表||||||||||||||||||||` —— 与标的毫无关系，
// 却排在第 1 位。分析师按顺序读，首条就是垃圾会直接影响判断。
func TestIsStructuralGarbage(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"|涉案物品信息表||||||||||||||||||||", true},
		{",,,,表格,,,", true},
		{"_______", true},
		// 正常标题不能误杀 —— 误杀的代价是「明明有资讯却说没有」
		{"贵州茅台：推进“全面向C”市场化改革", false},
		{"贵州茅台酒股份有限公司 - MBA智库百科", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isStructuralGarbage(c.in); got != c.want {
			t.Errorf("isStructuralGarbage(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
