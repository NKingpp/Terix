package server

// 金融任务判定（2026-11-05）。
//
// # 为什么需要它
//
// 用户明确要求：**允许不指定标的，让模型自行选品**。
// 但原实现把「instrument_id 非空」当作金融路径的唯一判据
// （goals prompt / 阶段图 / 行情取数三条链都看它），于是：
//
//	任务「在 OKX 上做交易，标的自己挑」
//	  → 识别不出具体代码 → instrument_id = NULL
//	  → 判定为渗透任务 → 走 goalsDefaultTmpl（渗透措辞）
//	  → 模型拿不到金融语境，也不知道可以用 get_quote 自己选品
//
// # 判据怎么定
//
// 两级，前一级优先：
//
//	① 有标的（instrument_id 非空）→ 必然是金融任务，无需看文本
//	② 没标的 → 看任务文本的**市场/交易语义**
//
// ② 用关键词而不是「调 LLM 分类」：分类要额外一次模型调用
// （冷启动 + 费用 + 延迟），而这个判据只在任务创建时用一次，
// 且误判代价低 —— 判错只是 prompt 措辞不同，不会产生交易。
//
// # 为什么不加 tasks.kind 列
//
// 加列意味着「kind 与 instrument_id 可能互相矛盾」（kind=finance 但没标的）。
// 文本判定没有这个不一致风险，且不需要迁移历史数据。
// 若将来需要用户显式指定领域，再加一个可空列并由它优先即可。

import (
	"strings"

	"github.com/Autumn-27/artex/agent"
)

// financeKeywords 是「这是金融任务」的强信号词。
//
// # 为什么这些词而不是更宽的集合
//
// 误判方向要对称考虑：
//   - 漏判（金融任务当渗透跑）→ 模型用错措辞，但工具绑的是两个域的交集，
//     用户能看到「分析员在讲漏洞」并重开任务。
//   - 误判（渗透任务当金融跑）→ 模型被要求拆「分析结论」，
//     而目标是打漏洞 —— 任务会跑偏但不会产生交易（exec 链路另有闸门）。
//
// 所以关键词取「明确是市场/交易语义」的那些，不追求覆盖所有说法。
var financeKeywords = []string{
	// 标的与市场
	"股票", "美股", "港股", "a股", "A股", "加密", "加密货币", "数字货币",
	"比特币", "以太坊", "币", "合约", "永续",
	// 交易动作
	"交易", "下单", "买入", "卖出", "开仓", "平仓", "持仓", "仓位",
	"止损", "止盈", "建仓", "清仓", "加仓", "减仓",
	// 分析框架
	"策略", "回测", "估值", "评级", "多空", "看多", "看空", "套利", "对冲",
	"资产配置", "组合", "收益", "盈亏", "净值",
	// 数据源与平台
	"行情", "K线", "k线", "财报", "市盈率", "市值", "指数", "ETF", "etf",
	"OKX", "okx", "复星", "星财富", "同花顺", "通达信", "akshare",
	// 资金
	"USDT", "usdt", "美元", "人民币", "收益率", "杠杆",
}

// taskIsFinance 判断任务是否属于金融领域。
//
// instrumentID 非空直接判 true —— 绑了标的就必然是金融任务，
// 不必再看文本（文本可能是「帮我看看这个」这种没有关键词的短句）。
//
// texts 传入任务的 Description 与 Goal，两段都参与匹配：
// 用户常把标的信息写在描述里、把动作写在目标里。
func taskIsFinance(instrumentID *int64, texts ...string) bool {
	if instrumentID != nil && *instrumentID > 0 {
		return true
	}
	blob := strings.ToLower(strings.Join(texts, " "))
	for _, kw := range financeKeywords {
		if strings.Contains(blob, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}

// assetClassHintForTask 从任务文本推断资产类别，供**无标**金融任务建阶段图时用。
//
// # 为什么需要（2026-11-11）
//
// 无标任务原来恒用 AssetClassStock 展开阶段图（stageGraphMaterializer），
// 于是 screener 节点的 payload 带着 asset_class=stock —— 模型看到的第一手信息
// 就是「这是美股票」，于是一个写满「加密永续」的任务被选成了 NVDA。
//
// 注释说选 stock 是因为「三套模板里覆盖最广」。但覆盖广不等于猜对：
// stock 模板含 analyst_fundamental（crypto 没有财报源），真做加密时会写一堆
// unavailable；而且它对选品方向有实质误导。
//
// # 判据为什么是「关键词计数」而不是「有 crypto 词就是 crypto」
//
// 任务可能同时提到两类（「比较 BTC 和美股科技股」）。取出现次数多的一侧，
// 平局则回到 stock（保守兜底，与原来行为一致）。
//
// 返回 agent.AssetClass* 之一。
func assetClassHintForTask(texts ...string) string {
	blob := strings.ToLower(strings.Join(texts, " "))
	cryptoScore := 0
	for _, kw := range cryptoHintKeywords {
		cryptoScore += strings.Count(blob, strings.ToLower(kw))
	}
	// 美股/A股不加权：它们在 stock 模板下都能跑，只有 crypto 需要换模板。
	if cryptoScore > 0 {
		return agent.AssetClassCrypto
	}
	return agent.AssetClassStock
}

// cryptoHintKeywords 是「这是加密任务」的市场信号词（比 financeKeywords 更专）。
//
// 不含 "币" 这种过宽的词：用户说「资金」也会命中。
var cryptoHintKeywords = []string{
	"加密", "加密货币", "数字货币", "比特币", "以太坊",
	"永续", "合约", "usdt", "okx",
	"btc", "eth", "sol", "doge", "xrp", "bnb",
	"-swap",
}
