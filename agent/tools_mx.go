package agent

// mx_query / mx_news：东方财富妙想（MX Skills）的两个取数工具。
//
// # 与 get_quote / get_kline / get_news / get_fundamentals 的分工
//
// 已有工具是**结构化**取数：给定 symbol + 参数，返回确定字段。它们是可复现的
// 证据，也是快照/回测的基础。妙想是**自然语言**问句：问什么答什么，字段形态
// 由上游决定（问一次返回一张动态表）。
//
// 所以两个工具描述里必须写死「这是自然语言查询，不是结构化参数」——
// 否则模型会照着 get_quote 的形状传 {"symbol":"600519","field":"price"}，
// 得到一个语义完全不同的查询（实测：传一段 JSON 当问句，妙想仍然会解析，
// 但会回一张意图漂移的表，比报错更危险：**看起来成功了**）。
//
// # 为什么不落快照
//
// 妙想返回的表格没有稳定的业务时间：一张表可能一行是 2026-10-30、另一行是
// 2025-09-30（多标的对比时更乱）。snapshots.collected_at NOT NULL，硬填
// 「今天」会让去重粒度失真（同一份数据每天一个新快照），填首行日期又会在
// 多标的表上写出错误的时间语义。
//
// 代价：mx_* 的结论无法带 snapshot_id 进证据链。所以工具描述里要求模型在
// emit_fact 里把「数据来自妙想 + 问句原文」写进 evidence ——
// 可追溯性靠问句原文，不靠快照 id。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	actool "github.com/Autumn-27/norma/tool"
)

// 默认/上限条数。资讯每条进 prompt 都花 token（content 实测可达数千字）。
const (
	mxNewsDefaultLimit = 10
	mxNewsMaxLimit     = 30
)

// mxSource 取可用的妙想客户端。
//
// 三个来源按优先级：注入的字段 → ctx → 环境变量。
//
// # 为什么 ctx 与环境变量两条退路都要留
//
// 工具实例是 server 装配时构造的那个（buildDomainReg），而**种子壳**
// （builtinToolsByAgent 里的 NewMarketToolSet(nil)）也会被问到名称/描述，
// 但它永远不会被 Call。真正被 Call 的实例来自 buildDomainReg —
// 它在 server 侧可以用 SetMXClient 注入。
//
// 但装配点不止一个（domainreg 的构造在 3 处，测试里还有），漏掉一处就会
// 出现「配了 MX_APIKEY 却报未启用」。所以 SetMXClient 没设时按环境变量兜底 ——
// 环境变量本身就是这条通道的开关（见 NewMXClient）。
func (m *MarketToolSet) mxSource() (*MXClient, error) {
	if cli := m.mx.Load(); cli != nil {
		if !cli.Configured() {
			return nil, fmt.Errorf(
				"mx 通道未启用：MX_APIKEY 为空（设置环境变量后重启；已注入的客户端没有 apiKey）")
		}
		return cli, nil
	}
	if c := NewMXClient(""); c.Configured() {
		return c, nil
	}
	return nil, fmt.Errorf(
		"mx 通道未启用：MX_APIKEY 未配置（这是**配置缺失**，不是「没有数据」——" +
			"请写进 data_gaps 并改用 get_quote/get_kline/get_news）")
}

// ---------------------------------------------------------------------
// mx_query
// ---------------------------------------------------------------------

func (m *MarketToolSet) mxQuery() actool.CoreTool {
	meta := marketToolMetas["mx_query"]
	return readTool(
		"mx_query",
		"用**自然语言问句**查询东方财富妙想数据库的行情与财务数据。这是东财的权威数据库，\n"+
			"覆盖 A股 / 港股 / 美股 / 基金 / 债券。\n"+
			"⚠ 入参是**一句中文问句**，不是结构化参数。不要传 {symbol, field, period} 这种对象。\n"+
			"  正确：「贵州茅台最近5个交易日收盘价」「宁德时代2026年三季度营业收入和净利润」\n"+
			"        「比亚迪近三年毛利率」「上证指数最新点位」「腾讯控股港股通持股比例」\n"+
			"  错误：{\"symbol\":\"600519\",\"field\":\"收盘价\"}（会被当成一段奇怪的问句解析）\n"+
			"返回 {tables:[{name, code, entity, head_label, columns, units, rows}], table_count, note}：\n"+
			"  - 每张表一个 code/entity（一次问句可能返回多张表，如 A股与港股同名公司）\n"+
			"  - head_label 是首列语义：「日期」（时间序列）或「标的」（多标的对比）\n"+
			"  - columns 首列是 head_label，其余是**指标名**；rows 每行一个取值，值是字符串\n"+
			"  - units 是「指标名 → 单位」（如 收盘价→元）。**没给单位时不要自己假设**\n"+
			"  - note 非空表示部分指标上游没给名称、列名是原始 id —— 这类列**不要凭 id 猜含义**\n"+
			"⚠ MX_NO_DATA = 「问句没命中数据」的合法结论，重试无用。换更具体的问句\n"+
			"  （带公司名 + 指标名 + 时间范围），或改用 get_quote/get_kline 取结构化行情。\n"+
			"⚠ 没有加密数据 —— crypto 标的请用 get_quote / get_kline / get_crypto_flow。\n"+
			"⚠ 引用它的结论时，evidence 里请写明「妙想 + 问句原文 + 表名」，便于复核。",
		obj(map[string]any{
			"query": str("必填。一句中文问句（自然语言）。含公司名/标的+指标名+时间范围时解析最准"),
		}, "query"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Query string `json:"query"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf(fmt.Sprintf("入参解析失败: %v", err)), nil
			}
			query := strings.TrimSpace(a.Query)
			if query == "" {
				return actool.Errorf("缺少 query（这里是自然语言问句，不是 symbol）"), nil
			}
			cli, err := m.mxSource()
			if err != nil {
				return actool.Errorf(fmt.Sprintf("mx_query 不可用：%v", err)), nil
			}
			cctx, cancel := context.WithTimeout(ctx, meta.Timeout)
			defer cancel()

			res, err := cli.Query(cctx, query)
			if err != nil {
				return actool.Errorf(mxErrText("mx_query", err)), nil
			}
			out := map[string]any{
				"query":       query,
				"tables":      res.Tables,
				"table_count": res.TableCount,
				"channel":     string(ChannelMX),
				"note":        res.Note,
				"data_note": "值取自上游 rawTable（纯数字，无单位）；单位见各表 units。" +
					"没有单位时不要假设，也不要跨表混用不同口径的指标",
			}
			if res.QuestionID != "" {
				out["question_id"] = res.QuestionID
			}
			return jsonResult(out)
		},
	)
}

// ---------------------------------------------------------------------
// mx_news
// ---------------------------------------------------------------------

func (m *MarketToolSet) mxNews() actool.CoreTool {
	meta := marketToolMetas["mx_news"]
	return readTool(
		"mx_news",
		"用**自然语言问句**检索东方财富妙想的资讯库。这是东财的权威资讯源，一次检索混合返回：\n"+
			"  新闻(type=INV_NEWS/NEWS) / 公告(NOTICE) / 券商研报(REPORT，带 rating 与 institution)。\n"+
			"⚠ 入参是**一句中文问句**，不是结构化参数（不要传 {symbol, from, to}）。\n"+
			"  正确：「贵州茅台最新消息」「宁德时代 业绩预告」「比亚迪 研报 评级」\n"+
			"返回 {items:[{title, content, date, source, type, rating, institution, entity, url}], count}。\n"+
			"⚠ 三类资讯的**可信度与用法不同**，必须分开对待：\n"+
			"  公告(NOTICE) 是事实；研报(REPORT) 是**别人**的观点，只能当观点引用；\n"+
			"  新闻(NEWS/INV_NEWS) 是二手转述，含雪球/股吧等散户内容。\n"+
			"⚠ 上游会返回**与问句无关的垃圾条目**（实测有「涉案物品信息表」这类），\n"+
			"  必须逐条判断相关性，无关的丢弃并在 rationale 写明排除了几条。\n"+
			"⚠ content 可能很长或是表格垃圾；date 可能为空（当 > 20 交易日处理）。\n"+
			"⚠ MX_NO_DATA = 「该问句没有资讯」，是合法结论，不是源故障。\n"+
			"⚠ 引用时 evidence 里写明「妙想资讯 + 标题 + date + type」。",
		obj(map[string]any{
			"query": str("必填。一句中文问句（自然语言），如「贵州茅台最新消息」"),
			"limit": intp(fmt.Sprintf("返回条数，默认 %d，范围 1..%d（每条都花 token）",
				mxNewsDefaultLimit, mxNewsMaxLimit)),
		}, "query"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Query string `json:"query"`
				Limit int    `json:"limit"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf(fmt.Sprintf("入参解析失败: %v", err)), nil
			}
			query := strings.TrimSpace(a.Query)
			if query == "" {
				return actool.Errorf("缺少 query（这里是自然语言问句，不是 symbol）"), nil
			}
			limit := a.Limit
			if limit <= 0 {
				limit = mxNewsDefaultLimit
			}
			if limit > mxNewsMaxLimit {
				limit = mxNewsMaxLimit
			}
			cli, err := m.mxSource()
			if err != nil {
				return actool.Errorf(fmt.Sprintf("mx_news 不可用：%v", err)), nil
			}
			cctx, cancel := context.WithTimeout(ctx, meta.Timeout)
			defer cancel()

			items, err := cli.NewsSearch(cctx, query)
			if err != nil {
				return actool.Errorf(mxErrText("mx_news", err)), nil
			}
			total := len(items)
			if total > limit {
				items = items[:limit]
			}
			return jsonResult(map[string]any{
				"query":   query,
				"count":   len(items),
				"total":   total,
				"items":   items,
				"channel": string(ChannelMX),
				"note": "count 是本次返回条数，total 是上游命中的去重后总数。" +
					"公告=事实、研报=观点、新闻=二手转述，引用时不要混为一类证据",
			})
		},
	)
}

// mxErrText 把妙想的错误翻译成模型可据此行动的说明。
//
// # 为什么按错误码分支而不是直接 %v
//
// 三个错误码要求的**后续动作完全不同**：鉴权失败要人改配置、无数据要换问句、
// 上游故障可以稍后重试。笼统抛一句「mx_query 失败」会让模型三者都当成
// 「源不可用」，然后要么无脑重试（浪费 30s 超时），要么把结论写成
// stance=unavailable（而真相可能只是问句太模糊）。
func mxErrText(tool string, err error) string {
	me, ok := err.(*MarketError)
	if !ok {
		return fmt.Sprintf("%s 失败: %v", tool, err)
	}
	switch me.Code {
	case ErrMXNoData:
		return fmt.Sprintf("%s：%v —— 这是「问句没命中数据」的**合法结论**，"+
			"重试同一个问句无用。请换更具体的问句（公司名+指标名+时间范围），"+
			"或改用结构化工具（get_quote/get_kline/get_news/get_fundamentals）。"+
			"若确实没有该数据，写进 data_gaps", tool, err)
	case ErrMXAuthFailed:
		return fmt.Sprintf("%s：%v —— 这是 **apiKey 配置问题**（不是数据缺失，也不是源故障），"+
			"需要人工检查 MX_APIKEY。不要重试，把这条写进 data_gaps，改用其他通道取数", tool, err)
	case ErrMXNotConfigured:
		return fmt.Sprintf("%s：%v —— 这是**配置缺失**（MX_APIKEY 未设置），"+
			"不是「没有数据」。请写进 data_gaps 并改用其他通道", tool, err)
	default:
		return fmt.Sprintf("%s 失败: %v —— 上游故障，可稍后重试；若持续失败改用其他通道", tool, err)
	}
}
