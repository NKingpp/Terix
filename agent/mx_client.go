package agent

// 东方财富妙想（MX Skills）HTTP 客户端。
//
// # 为什么是独立类型，不进 MarketClient 接口
//
// MarketClient（market_client.go）是「quote/kline/macro」三件套的抽象，
// akshare 与 okx 都要实现它。妙想的能力面完全不同 —— 它是**自然语言问句**
// 接口（POST 一段中文问句，返回若干张数据表 / 一批资讯），没有 symbol+period
// 这种结构化入参，也拿不到 quote/kline 的结构化字段。
//
// 把它塞进 MarketClient 会让 AkshareClient 与 OkxClient 都多出两个永远返回
// CHANNEL_NOT_APPLICABLE 的方法 —— 一个类型被迫实现它做不到的方法，说明
// 接口切错了。所以新能力只挂在具体类型上，与 FetchNews/FetchFundamental
// （market_news.go）同一处理方式。
//
// # 认证
//
// HTTP Header `apikey: <MX_APIKEY>`。**apiKey 绝不出现在任何日志/错误串里** ——
// 见 scrubKey。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	// envMXAPIKey 是妙想 API Key 的环境变量名。空 = 该通道未启用。
	envMXAPIKey = "MX_APIKEY"
	// defaultMXBaseURL 是两个端点的共同前缀。
	defaultMXBaseURL = "https://mkapi2.dfcfs.com/finskillshub/api/claw"
	// mxTimeout 是单次调用超时。与 marketToolMetas 里两条 meta 的 Timeout 一致。
	mxTimeout = 30 * time.Second
)

// 妙想通道的错误码。与 AKSHARE_* / NEWS_NO_DATA 同一层级，供上层分类处理。
//
//   - MX_NO_DATA           查得到接口，但没有返回任何数据（问句没命中标的，
//     或该指标妙想不提供）。**重试无用**，是合法结论。
//   - MX_AUTH_FAILED       apiKey 缺失/失效/无权限。配置问题，重试无用。
//   - MX_UPSTREAM_ERROR    上游故障/超时/返回结构不认识。可重试。
//   - MX_NOT_CONFIGURED    MX_APIKEY 未配置。与 AUTH_FAILED 分开：一个是
//     「没配」一个是「配了但错」，排查方向不同。
const (
	ErrMXNoData        = "MX_NO_DATA"
	ErrMXAuthFailed    = "MX_AUTH_FAILED"
	ErrMXUpstream      = "MX_UPSTREAM_ERROR"
	ErrMXNotConfigured = "MX_NOT_CONFIGURED"
)

// MXClient 调妙想的两个端点。
//
// 零值不可用（apiKey 空），必须经 NewMXClient 构造。
type MXClient struct {
	apiKey  string
	baseURL string
	hc      *http.Client
}

// NewMXClient 构造客户端。apiKey 为空时从环境变量 MX_APIKEY 读。
//
// **不在这里 panic / 返回 nil**：未配置是一种可诊断状态（tools_mx.go 会
// 给出「未启用」的明确错误），而不是构造期异常。
func NewMXClient(apiKey string) *MXClient {
	if strings.TrimSpace(apiKey) == "" {
		apiKey = strings.TrimSpace(os.Getenv(envMXAPIKey))
	}
	return &MXClient{
		apiKey:  apiKey,
		baseURL: defaultMXBaseURL,
		// 客户端的 HTTP 超时是兜底；真正的截止时间由 handler 的 context 控制
		// （与 AkshareClient 同一取舍）。
		hc: &http.Client{Timeout: mxTimeout},
	}
}

// Configured 报告 apiKey 是否已配置。
func (c *MXClient) Configured() bool {
	return c != nil && strings.TrimSpace(c.apiKey) != ""
}

// Timeout 返回建议超时（供 handler 建 context）。
func (c *MXClient) Timeout() time.Duration { return mxTimeout }

// ---------------------------------------------------------------------
// ctx 传递（与 WithMarketClient 同一模式）
// ---------------------------------------------------------------------

type mxClientKey struct{}

// WithMXClient 把妙想客户端放进 ctx。
//
// # 为什么不复用 WithMarketClient
//
// 那个位置放的是 MarketClient 接口（生产是 *AkshareClient），而妙想不是
// MarketClient 的实现（理由见文件头）。混在一起只能靠类型断言区分，
// 一旦装配把两者搞反，行情会静默走错通道。
func WithMXClient(ctx context.Context, c *MXClient) context.Context {
	if !c.Configured() {
		return ctx // 注入未配置的客户端等于不注入
	}
	return context.WithValue(ctx, mxClientKey{}, c)
}

// MXClientFrom 取回妙想客户端（ok=false 表示 ctx 里没有）。
func MXClientFrom(ctx context.Context) (*MXClient, bool) {
	c, ok := ctx.Value(mxClientKey{}).(*MXClient)
	if !ok || !c.Configured() {
		return nil, false
	}
	return c, true
}

// mxForVars 取 vars 构造器可用的客户端：先看 ctx，再退回环境变量。
//
// # 为什么要退回环境变量
//
// 装配点（server/engine.go 的 WithMarketClient 那一行）只注入 MarketClient。
// 若必须要 server 侧再加一行 WithMXClient 才能用，那么「配好 MX_APIKEY 就能用」
// 这个承诺就不成立 —— 环境变量本身就是这条通道的开关（见 NewMXClient）。
// 所以 ctx 没注入时按环境变量自建一个，行为与显式注入完全一致。
//
// 返回 nil 表示未配置 —— 调用方必须把它变成 data_gaps 而不是静默跳过。
func mxForVars(ctx context.Context) *MXClient {
	if c, ok := MXClientFrom(ctx); ok {
		return c
	}
	if strings.TrimSpace(os.Getenv(envMXAPIKey)) == "" {
		return nil
	}
	return NewMXClient("")
}

// ---------------------------------------------------------------------
// 响应信封
// ---------------------------------------------------------------------

// mxEnvelope 是两个端点共用的顶层信封。
//
// **业务状态在 status/code，不在 HTTP 状态码** —— 实测：apiKey 失效时
// HTTP 仍是 200，body 里 status=114。只判 HTTP 会把鉴权失败当成成功。
//
// Success 用 *bool 而不是 bool：`/news-search` 的部分响应**不带 success 字段**，
// 用 bool 会把它默认成 false 从而把正常响应当失败（实测踩过）。
// 缺字段时以 status/code 为准。
type mxEnvelope struct {
	Success *bool           `json:"success"`
	Status  int             `json:"status"`
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// post 发一次 POST 并返回 data 字段（已剥离信封）。
func (c *MXClient) post(ctx context.Context, path string, body any) (json.RawMessage, error) {
	if !c.Configured() {
		return nil, &MarketError{
			Code:      ErrMXNotConfigured,
			Message:   fmt.Sprintf("MX_APIKEY 未配置（设置环境变量 %s 后重启）", envMXAPIKey),
			Channel:   ChannelMX,
			Retryable: false,
		}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("序列化妙想请求失败: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("构造妙想请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("apikey", c.apiKey)

	resp, err := c.hc.Do(req)
	if err != nil {
		// 网络层失败（DNS/连接/超时）。**错误串来自 net 包，不含 header**，
		// 但仍过一遍 scrubKey —— 万一将来换了 transport 会带出请求头。
		return nil, &MarketError{
			Code:      ErrMXUpstream,
			Message:   c.scrubKey(err.Error()),
			Channel:   ChannelMX,
			Retryable: true,
		}
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, &MarketError{
			Code:      ErrMXUpstream,
			Message:   "读取妙想响应失败: " + c.scrubKey(err.Error()),
			Channel:   ChannelMX,
			Retryable: true,
		}
	}
	// 网关/代理返回 HTML 而不是 JSON —— 与 akshare 同一类故障，可重试。
	trimmed := bytes.TrimSpace(raw)
	if bytes.HasPrefix(trimmed, []byte("<!DOCTYPE")) || bytes.HasPrefix(trimmed, []byte("<html")) {
		return nil, &MarketError{
			Code:      ErrMXUpstream,
			Message:   "妙想返回 HTML 而非 JSON（网关/代理拦截）",
			Channel:   ChannelMX,
			Retryable: true,
		}
	}

	var env mxEnvelope
	if err := json.Unmarshal(trimmed, &env); err != nil {
		return nil, &MarketError{
			Code:      ErrMXUpstream,
			Message:   "解析妙想响应失败: " + c.scrubKey(err.Error()) + " (body=" + c.scrubKey(snippet(trimmed)) + ")",
			Channel:   ChannelMX,
			Retryable: true,
		}
	}
	if (env.Success != nil && !*env.Success) || env.Status != 0 || env.Code != 0 {
		return nil, c.classifyFailure(&env, resp.StatusCode)
	}
	return env.Data, nil
}

// classifyFailure 把业务失败映射成结构化错误。
//
// # 为什么 status 也算鉴权信号
//
// 实测：apiKey 失效 → status=114「API密钥不存在或已失效」；
// 未携带 → status=115「请求未携带API密钥」。两者 HTTP 都是 200。
// 只认 401/403 会把这两条最需要说清楚的配置错误归到「上游故障」，
// 让人去查网络而不是查 Key。
func (c *MXClient) classifyFailure(env *mxEnvelope, httpStatus int) *MarketError {
	msg := c.scrubKey(strings.TrimSpace(env.Message))
	if msg == "" {
		msg = "妙想返回失败但 message 为空"
	}
	if httpStatus == http.StatusUnauthorized || httpStatus == http.StatusForbidden {
		return &MarketError{Code: ErrMXAuthFailed, Message: msg, Channel: ChannelMX, Retryable: false}
	}
	if env.Status == 114 || env.Status == 115 || env.Code == 114 || env.Code == 115 {
		return &MarketError{Code: ErrMXAuthFailed, Message: msg, Channel: ChannelMX, Retryable: false}
	}
	if looksLikeMXAuthMessage(msg) {
		return &MarketError{Code: ErrMXAuthFailed, Message: msg, Channel: ChannelMX, Retryable: false}
	}
	return &MarketError{
		Code:      ErrMXUpstream,
		Message:   fmt.Sprintf("status=%d code=%d: %s", env.Status, env.Code, msg),
		Channel:   ChannelMX,
		Retryable: true,
	}
}

// looksLikeMXAuthMessage 兜底识别鉴权类文案（上游将来可能换 status 码）。
//
// 只匹配「密钥/key」这类明确词，不匹配泛化的「权限」—— 后者可能是
// 「该指标无权限」这种能力边界，误判成鉴权失败会掩盖真实原因。
func looksLikeMXAuthMessage(msg string) bool {
	l := strings.ToLower(msg)
	for _, p := range []string{"apikey", "api key", "api_key", "密钥", "秘钥"} {
		if strings.Contains(l, p) {
			return true
		}
	}
	return false
}

// scrubKey 从任意文本里抹掉 apiKey。
//
// # 为什么值得一个函数
//
// 错误串会被写进日志、返回给模型、最终出现在对话与报告里。上游若在
// message 或 body 里回显 key（解析失败时常见），一次泄漏就等于把凭证
// 写进历史记录 —— 撤销都收不回来。所以**所有**对外错误文本都过这一层，
// 而不是「我记得别打印」。
func (c *MXClient) scrubKey(s string) string {
	k := strings.TrimSpace(c.apiKey)
	if k == "" || len(k) < 6 {
		return s // 太短的值不可能是真 key，替换会造成误伤
	}
	return strings.ReplaceAll(s, k, "***")
}

// ---------------------------------------------------------------------
// 端点 1：行情/财务查询（自然语言问句）
// ---------------------------------------------------------------------

// MXTable 是妙想返回的一张数据表。
//
// # 为什么是「列名 + 行」而不是结构化字段
//
// 妙想的返回是**动态表**：问什么指标就返回什么指标，列数、列名都不固定
// （「收盘价」是一列，「营业收入/净利润/毛利率」是三列）。预定义结构不可能
// 穷举，所以用「列名 → 值」的通用形态，让分析师按列名自取。
type MXTable struct {
	// Name 是表名，取自上游 title（如「贵州茅台(600519.SH)的收盘价」）。
	Name string `json:"name"`
	// Code / Entity 是标的标识（可能为空：宏观类问句没有单一标的）。
	Code   string `json:"code,omitempty"`
	Entity string `json:"entity,omitempty"`
	// HeadLabel 是首列列名：时间轴为「日期」，多标的对比为「标的」。
	HeadLabel string   `json:"head_label"`
	Columns   []string `json:"columns"`
	// Units 是「指标名 → 单位」（如 收盘价→元）。缺失表示上游没给。
	Units map[string]string `json:"units,omitempty"`
	// Rows 每行一个 headName 值，key 与 Columns 一致。
	// **值一律 string** —— 与 Quote/Bar 同一套精度铁律，禁 float。
	Rows []map[string]string `json:"rows"`
}

// MXQueryResult 是一次问句查询的结果（若干张表）。
type MXQueryResult struct {
	Tables     []MXTable `json:"tables"`
	TableCount int       `json:"table_count"`
	QuestionID string    `json:"question_id,omitempty"`
	// Note 记录「指标名没能解析出来」这类降级，让调用方知道该怀疑什么。
	Note string `json:"note,omitempty"`
}

// Query 用自然语言问句查询行情/财务数据。
//
// toolQuery 示例：「贵州茅台最近5个交易日收盘价」「宁德时代主力资金流向」。
// 返回零张表时返回 MX_NO_DATA（**不是**空结果 + nil error）——
// 「查不到」是有效结论，必须能被 IsNoDataError 识别，才不会让上层
// 把「没数据」渲染成「数据是空的」。
func (c *MXClient) Query(ctx context.Context, toolQuery string) (*MXQueryResult, error) {
	q := strings.TrimSpace(toolQuery)
	if q == "" {
		return nil, &MarketError{
			Code:      ErrMXUpstream,
			Message:   "toolQuery 为空",
			Channel:   ChannelMX,
			Retryable: false,
		}
	}
	data, err := c.post(ctx, "/query", map[string]string{"toolQuery": q})
	if err != nil {
		return nil, err
	}

	var d struct {
		Data struct {
			SearchDataResultDTO struct {
				QuestionID       string       `json:"questionId"`
				DataTableDTOList []mxTableDTO `json:"dataTableDTOList"`
			} `json:"searchDataResultDTO"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, &MarketError{
			Code:      ErrMXUpstream,
			Message:   "解析妙想 /query 失败: " + c.scrubKey(err.Error()),
			Channel:   ChannelMX,
			Retryable: true,
		}
	}
	sr := d.Data.SearchDataResultDTO
	res := &MXQueryResult{QuestionID: sr.QuestionID}
	var unnamed []string
	for _, dto := range sr.DataTableDTOList {
		t, missing := dto.toTable()
		if len(t.Rows) == 0 {
			continue // 空表不占位：调用方看到一张 0 行的表只会困惑
		}
		res.Tables = append(res.Tables, t)
		unnamed = append(unnamed, missing...)
	}
	res.TableCount = len(res.Tables)
	if res.TableCount == 0 {
		return nil, &MarketError{
			Code: ErrMXNoData,
			Message: fmt.Sprintf(
				"妙想未返回任何数据表（问句：%q）。这是「查得到接口但没有这个数据」，重试无用；"+
					"换个更具体的问句（带标的名称与指标名）或改用 get_quote/get_kline", q),
			Channel:   ChannelMX,
			Retryable: false,
		}
	}
	if len(unnamed) > 0 {
		// 指标名不可知时**不猜** —— 猜错会让分析师把「净利润」当成「营业收入」。
		res.Note = "以下指标上游未给出名称，列名用原始指标 id 代替，不要凭 id 推测含义: " +
			strings.Join(uniqueStrings(unnamed), ", ")
	}
	return res, nil
}

// mxTableDTO 是上游单张表的原始结构（字段名与 API 一致）。
type mxTableDTO struct {
	Code           string              `json:"code"`
	EntityName     string              `json:"entityName"`
	Title          string              `json:"title"`
	Table          map[string][]string `json:"table"`
	RawTable       map[string][]string `json:"rawTable"`
	NameMap        map[string]string   `json:"nameMap"`
	IndicatorOrder []string            `json:"indicatorOrder"`
	Field          mxField             `json:"field"`
	FieldSet       []mxField           `json:"fieldSet"`
}

// mxField 是上游的指标定义（一个 returnCode 对应一个指标）。
type mxField struct {
	ReturnCode   string `json:"returnCode"`
	ReturnName   string `json:"returnName"`
	ReturnSource string `json:"returnSourceCode"`
	UnitName     string `json:"unitName"`
}

// headNameKey 是「行标签」这一列在上游 map 里的固定 key。
const headNameKey = "headName"

// toTable 把一张上游 DTO 转成 MXTable。
//
// # 取值用 rawTable 优先
//
// 上游同时给 table（带单位，如 "1392.05元"）与 rawTable（纯数字 "1392.05"）。
// 取 rawTable 让下游能直接算同比/涨跌幅而不用先剥单位；单位改由 Units 单独
// 承载，信息不丢。rawTable 缺失时退回 table（宁可值带单位也不能没值）。
//
// 返回第二值是「名不出来的指标 key」列表，供上层写进 Note。
func (d mxTableDTO) toTable() (MXTable, []string) {
	values := d.RawTable
	if len(values) == 0 {
		values = d.Table
	}
	t := MXTable{
		Name:      strings.TrimSpace(d.Title),
		Code:      d.Code,
		Entity:    d.EntityName,
		HeadLabel: d.headLabel(values[headNameKey]),
		Units:     map[string]string{},
	}
	head := values[headNameKey]

	// 指标 key 的顺序：先用上游 indicatorOrder（它反映模型提问的顺序），
	// 再把 map 里有、order 里没有的 key 按字典序补上 —— map 无序，
	// 不排序会让两次相同查询的列序不同，破坏「同一问句同一结果」的可复现性。
	var keys []string
	seen := map[string]bool{headNameKey: true}
	for _, k := range d.IndicatorOrder {
		if _, ok := values[k]; ok && !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	var rest []string
	for k := range values {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	keys = append(keys, rest...)

	var missing []string
	for _, k := range keys {
		name := d.indicatorName(k)
		if name == "" {
			// **绝不瞎猜**：用原始 id 当列名，并把 key 报给上层写进 Note。
			name = k
			missing = append(missing, k)
		}
		t.Columns = append(t.Columns, name)
		if u := strings.TrimSpace(d.unitFor(k)); u != "" {
			t.Units[name] = u
		}
	}
	if len(t.Columns) == 0 {
		return t, missing
	}
	t.Columns = append([]string{t.HeadLabel}, t.Columns...)

	for i, label := range head {
		row := make(map[string]string, len(t.Columns))
		row[t.HeadLabel] = label
		for j, k := range keys {
			col := t.Columns[j+1]
			row[col] = at(values[k], i)
		}
		t.Rows = append(t.Rows, row)
	}
	if t.Name == "" {
		// title 缺失时拼一个可读表名，而不是留空让调用方看到无名表。
		t.Name = strings.TrimSpace(d.EntityName + " " + strings.Join(t.Columns[1:], "/"))
	}
	return t, missing
}

// indicatorName 依次从 nameMap / field / fieldSet 找指标名，找不到返回 ""。
//
// nameMap 是实测最可靠的来源（{"325898":"收盘价"}）；field/fieldSet 是它的
// 结构化版本，两者通常同时存在。**三处都没有就返回空**，由调用方降级成 id，
// 不做任何基于数值大小/单位的猜测。
func (d mxTableDTO) indicatorName(key string) string {
	if v, ok := d.NameMap[key]; ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if d.Field.ReturnCode == key && strings.TrimSpace(d.Field.ReturnName) != "" {
		return strings.TrimSpace(d.Field.ReturnName)
	}
	for _, f := range d.FieldSet {
		if f.ReturnCode == key && strings.TrimSpace(f.ReturnName) != "" {
			return strings.TrimSpace(f.ReturnName)
		}
	}
	return ""
}

// unitFor 同上，找单位名。
func (d mxTableDTO) unitFor(key string) string {
	if d.Field.ReturnCode == key {
		return d.Field.UnitName
	}
	for _, f := range d.FieldSet {
		if f.ReturnCode == key {
			return f.UnitName
		}
	}
	return ""
}

// mxDateRE 匹配上游的日期列头（"2026-10-30(日)" / "2026-10-30"）。
var mxDateRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}`)

// headLabel 推断首列语义：全是日期 → 「日期」，否则 → 「标的」。
//
// # 为什么这里可以「猜」
//
// 它只影响**列名文案**，不影响任何数值。而指标名不同：猜错会让分析师
// 把 A 指标的数当成 B 指标用，所以那边必须严格（见 indicatorName）。
// 两种猜的代价差三个数量级，处理方式自然不同。
func (d mxTableDTO) headLabel(head []string) string {
	if len(head) == 0 {
		return "维度"
	}
	for _, h := range head {
		if !mxDateRE.MatchString(strings.TrimSpace(h)) {
			return "标的"
		}
	}
	return "日期"
}

// at 取切片的第 i 个元素（越界返回空串）。
//
// 上游各列长度可能不一致（某指标缺某期），越界返回空串而不是报错 ——
// 空串在下游语义是「该期无此值」，与缺列等价。
func at(s []string, i int) string {
	if i < 0 || i >= len(s) {
		return ""
	}
	return s[i]
}

// ---------------------------------------------------------------------
// 端点 2：资讯搜索
// ---------------------------------------------------------------------

// MXNewsItem 是一条东财妙想资讯。
//
// 字段含义（与 akshare 的 NewsItem 对照）：
//   - Type=INV_NEWS/NEWS 偏新闻，NOTICE 是公告，REPORT 是券商研报
//     （研报才有 Rating/Institution）。**这几类可信度与用法不同**：
//     公告是事实、研报是观点、NEWS 是二手转述，必须让分析师看得见。
//   - Content 通常是正文摘要，可能很长（实测有几千字的表格垃圾），
//     也**可能完全无关**（垃圾召回），所以相关性必须由分析师筛。
//   - URL 可能为空（部分条目是站内快照）。
type MXNewsItem struct {
	Title       string `json:"title"`
	Content     string `json:"content,omitempty"`
	Date        string `json:"date,omitempty"`
	Source      string `json:"source,omitempty"`
	Type        string `json:"type,omitempty"` // INV_NEWS | NEWS | NOTICE | REPORT
	Rating      string `json:"rating,omitempty"`
	Institution string `json:"institution,omitempty"`
	Entity      string `json:"entity,omitempty"`
	URL         string `json:"url,omitempty"`
}

// NewsSearch 检索资讯（新闻/公告/研报混合）。
//
// 返回**已去重**的条目：实测上游会返回完全重复的条目（同一条 NEWS 出现两次），
// 重复项进 prompt 只是白烧 token 并放大它在判断里的权重。
func (c *MXClient) NewsSearch(ctx context.Context, query string) ([]MXNewsItem, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, &MarketError{
			Code:      ErrMXUpstream,
			Message:   "query 为空",
			Channel:   ChannelMX,
			Retryable: false,
		}
	}
	data, err := c.post(ctx, "/news-search", map[string]string{"query": q})
	if err != nil {
		return nil, err
	}

	// 注意层级：/news-search 比 /query **多包一层** `data.data`。
	// 实测：body 是 {status,code,data:{requestId,message,status,code,data:{protocolType,llmSearchResponse}}}，
	// post() 已剥掉最外层信封，因此这里还要再进一层 data。
	// 两个端点的信封深度不同，写死注释避免下次又被「看起来一样」骗到。
	var d struct {
		Data struct {
			LLMSearchResponse struct {
				Data []mxNewsRaw `json:"data"`
			} `json:"llmSearchResponse"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, &MarketError{
			Code:      ErrMXUpstream,
			Message:   "解析妙想 /news-search 失败: " + c.scrubKey(err.Error()),
			Channel:   ChannelMX,
			Retryable: true,
		}
	}

	raw := d.Data.LLMSearchResponse.Data
	items := make([]MXNewsItem, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, r := range raw {
		it := r.toItem()
		if strings.TrimSpace(it.Title) == "" && strings.TrimSpace(it.Content) == "" {
			continue // 标题与正文都空 = 无信息条目
		}
		key := it.Title + "\x00" + it.Date
		if seen[key] {
			continue
		}
		seen[key] = true
		items = append(items, it)
	}
	if len(items) == 0 {
		return nil, &MarketError{
			Code: ErrMXNoData,
			Message: fmt.Sprintf(
				"妙想资讯未返回任何条目（问句：%q）。这是「查得到接口但没有资讯」，重试无用", q),
			Channel:   ChannelMX,
			Retryable: false,
		}
	}
	return items, nil
}

// mxNewsRaw 是上游单条资讯的原始结构。
type mxNewsRaw struct {
	Title           string `json:"title"`
	Content         string `json:"content"`
	Date            string `json:"date"`
	PublishDate     string `json:"publishDate"`
	Source          string `json:"source"`
	InformationType string `json:"informationType"`
	Rating          string `json:"rating"`
	EMRatingName    string `json:"emRatingName"`
	InsName         string `json:"insName"`
	EntityFullName  string `json:"entityFullName"`
	JumpURL         string `json:"jumpUrl"`
}

func (r mxNewsRaw) toItem() MXNewsItem {
	rating := strings.TrimSpace(r.Rating)
	if rating == "" {
		rating = strings.TrimSpace(r.EMRatingName)
	}
	date := strings.TrimSpace(r.Date)
	if date == "" {
		date = strings.TrimSpace(r.PublishDate)
	}
	it := MXNewsItem{
		Title:       strings.TrimSpace(r.Title),
		Content:     strings.TrimSpace(r.Content),
		Date:        date,
		Source:      strings.TrimSpace(r.Source),
		Type:        strings.TrimSpace(r.InformationType),
		Rating:      rating,
		Institution: strings.TrimSpace(r.InsName),
		Entity:      strings.TrimSpace(r.EntityFullName),
		URL:         strings.TrimSpace(r.JumpURL),
	}
	// 标题为空但有正文时用正文首句当标题 —— 没有标题的条目在 prompt 里
	// 只是一坨无法引用的文字，分析师无法指向它写 evidence。
	if it.Title == "" && it.Content != "" {
		it.Title = firstSentence(it.Content, 60)
	}
	return it
}

// isStructuralGarbage 判断标题是否只是表格/表单的原始文本。
//
// # 为什么需要它
//
// 实测（2026-10-31）：问句「贵州茅台最新消息」的首条返回
// `|涉案物品信息表||||||||||||||||||||` —— 那是某份涉案财物清单的表头，
// 与贵州茅台的资讯毫无关系。它排在第 1 位，而分析师会按顺序读 ——
// 首条就是垃圾会直接影响它对「今天有什么消息」的判断。
//
// # 判据为什么是「分隔符占比」而不是关键词
//
// 这类脏数据的形式不固定（表格用 `|`、CSV 用 `,`、有时是 `_`/`=` 填充）。
// 用关键词会漏，用「分隔符在标题里占比过高」能覆盖所有形态。
//
// 阈值 0.3：正常新闻标题里标点通常 < 10%（「贵州茅台：推进全面向C改革」约 4%）。
// 0.3 留了足够余量，同时能抓住上面那种 >80% 的。
func isStructuralGarbage(title string) bool {
	t := strings.TrimSpace(title)
	if t == "" {
		return false // 空标题由调用方按「无标题且无正文」处理
	}
	rs := []rune(t)
	sep := 0
	for _, r := range rs {
		switch r {
		case '|', ',', '，', '_', '=', ';', '；', '	':
			sep++
		}
	}
	return float64(sep)/float64(len(rs)) > 0.3
}

// firstSentence 取首句并截断（用于给无标题条目造一个可引用的标题）。
func firstSentence(s string, max int) string {
	s = strings.TrimSpace(s)
	cut := len(s)
	for _, sep := range []string{"。", "！", "？", "\n", "."} {
		if i := strings.Index(s, sep); i > 0 && i < cut {
			cut = i
		}
	}
	s = s[:cut]
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}
