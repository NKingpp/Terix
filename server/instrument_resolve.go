package server

// 标的推断：从「分析背景 + 分析目标」的自由文本里识别出标的。
//
// # 为什么需要它
//
// 用户不想在下拉框里选标的 —— 他想在描述里直接写「给出 msft 交易策略」。
// 而 tasks.instrument_id 是三个关键分叉的**唯一**判据：
//
//	① goals prompt：instrumentID>0 → goalsFinanceTmpl，否则渗透 prompt
//	② 阶段图：只有 instrument 非空才 agent.Expand 建图
//	③ 行情取数：fetchBarsForVars 从 intent payload 读 symbol / market
//
// 所以「不选标的」不能等于「instrument_id 恒 NULL」—— 那会退回渗透路径。
// 必须在文本里把标的认出来。
//
// # 为什么是「查库匹配」而不是「正则猜代码」
//
// 正则无法区分 `MSFT`（股票）与 `msft`（小写，用户随手打的）。
// 而库里有 symbol 与 name 两列 —— 直接拿文本去比对已有标的，
// 命中即用它的 market / asset_class。这比猜更可靠，且不需要维护正则。
//
// # 认不出来时的行为
//
// 返回 nil，任务照建。但此时 instrument_id 为空 → 走的是「无标的」路径。
// ⚠️ 这条路径当前会退回渗透 prompt（见 goalsBodyFor 的注释）。
// 所以 createTask 在认不出来时要**明确提示用户**写上代码，而不是静默降级。

import (
	"log"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/Autumn-27/artex/db"
)

// ResolveInstrumentFromText 从文本中识别标的。
//
// 依次尝试：
//  1. 显式代码（区分大小写地先试原文，再试大写）—— 用户写 "MSFT" 时最准
//  2. 标的名称（中文名如「贵州茅台」）—— 用户常写「分析一下贵州茅台」
//
// 多个命中时取**最先出现**的那个：用户通常把主语放句首
// （「给出 msft 交易策略」→ msft 在最前）。
//
// # 为什么去掉长度 < 2 的 token
//
// 单字符 "a" / "1" 会命中 id 之类的噪声。标的代码最短是 2 位（如美股 "GE"），
// 而 2 位已足够长，不会误伤。
func ResolveInstrumentFromText(store *db.InstrumentStore, texts ...string) (*db.Instrument, bool) {
	if store == nil {
		return nil, false
	}
	blob := strings.TrimSpace(strings.Join(texts, "\n"))
	if blob == "" {
		return nil, false
	}

	// 一次拉全量标的做本地匹配。
	//
	// # 为什么不逐 token 查库
	//
	// 一段话可能有几十个 token，逐 token 查库就是几十次往返。
	// 而标的表是**参考数据**（几百条量级，不是业务数据），全量拉一次更快。
	// 将来若涨到全市场几千条，再改成「先按 token 查 (market,symbol) 索引」。
	list, err := store.ListInstruments(db.InstrumentFilter{})
	if err != nil || len(list) == 0 {
		return nil, false
	}

	// token 化，并按出现位置保留先后顺序。
	type hit struct {
		pos  int
		inst *db.Instrument
	}
	var hits []hit
	seen := map[int64]bool{}

	record := func(pos int, inst *db.Instrument) {
		if seen[inst.ID] {
			return
		}
		seen[inst.ID] = true
		hits = append(hits, hit{pos: pos, inst: inst})
	}

	// ① 先按 symbol 匹配（最长优先，避免 "BTC" 抢走 "BTC-USDT"）。
	//
	// 从长到短遍历标的，找到即停 —— 长代码是短代码的超集，
	// 先处理长的能避免 "600519.SH" 被 "600519" 提前命中而错误归类。
	sorted := make([]*db.Instrument, len(list))
	copy(sorted, list)
	sort.SliceStable(sorted, func(i, j int) bool {
		return len(sorted[i].Symbol) > len(sorted[j].Symbol)
	})
	for _, inst := range sorted {
		sym := strings.TrimSpace(inst.Symbol)
		if len(sym) < 2 {
			continue
		}
		// 大小写不敏感定位 —— 用户可能写 msft / Msft / MSFT。
		// 但**要求边界**：MSFT 不该在 "MSFTX" 里命中。
		if pos, ok := findToken(blob, sym); ok {
			record(pos, inst)
		}
	}

	// ② 再按 name 匹配（中文名 / 英文全称）。
	for _, inst := range list {
		if inst.Name == nil {
			continue
		}
		name := strings.TrimSpace(*inst.Name)
		if len(name) < 2 {
			continue
		}
		if pos := strings.Index(blob, name); pos >= 0 {
			record(pos, inst)
		}
	}

	if len(hits) == 0 {
		return nil, false
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].pos < hits[j].pos })
	return hits[0].inst, true
}

// findToken 在 blob 里查找 sym（大小写不敏感），要求两侧是边界。
//
// # 为什么要判边界
//
// 没有边界时 `SPY` 会在 `SPYDER` 里命中 —— 用户写「像 spyder 一样」会被
// 识别成 SPY 标的。边界定义为「非字母数字」或字符串首尾。
func findToken(blob, sym string) (int, bool) {
	lower := strings.ToLower(blob)
	target := strings.ToLower(sym)
	from := 0
	for {
		i := strings.Index(lower[from:], target)
		if i < 0 {
			return 0, false
		}
		i += from
		end := i + len(target)
		leftOK := i == 0 || !isWordByte(lower[i-1])
		rightOK := end == len(lower) || !isWordByte(lower[end])
		if leftOK && rightOK {
			return i, true
		}
		from = i + 1
		if from >= len(lower) {
			return 0, false
		}
	}
}

// isWordByte 判断是否属于「代码/名称的组成字符」。
//
// 下划线与点算 word（`600519.SH` 是一体），连字符算 word（`BTC-USDT` 是一体）——
// 所以 `BTC-USDT` 里查 `BTC` 时右侧是 `-`，判为 word 内 → 不命中（正确：
// 应该由更长的 `BTC-USDT` 命中）。
func isWordByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c == '_', c == '.', c == '-', c == '^':
		return true
	}
	return false
}

// candidatePattern 切出「像标的代码」的片段，用于库中不存在时自动登记。
//
// 与 findToken 的分工：findToken 判「是不是**已有**标的」，本正则判
// 「长得像不像一个代码」。后者必须更保守 —— 它会导致**建库**，
// 把 "the" 当成美股代码建进去就是脏数据。
//
//	[a-z]{2,10}-[a-z]{2,10} → BTC-USDT（任意大小写）
//	[a-z]{1,5}              → MSFT / SPY（任意大小写，用户常写 msft）
//	\d{6}                   → 600519（A 股）
//	\d{5}                   → 00700（港股）
//
// # 为什么允许小写
//
// 用户实际就写「给出 msft 交易策略」。只认全大写会让这个最常见的写法
// 完全失效 —— 而那种失效是静默的（任务照建，只是不绑标的）。
//
// # 那 "the" / "and" 这类词怎么办
//
// 它们会成为候选，但**只有行情源验证通过才登记**（见 resolveInstrumentFromTaskText）。
// 行情源对 "THE" 查不到报价 → 不登记 → 无脏数据。这是「宁可多问一次行情源，
// 也不要静默丢标的」的取舍：误判的代价只是一次 HTTP 请求。
//
// # 为什么词频高的短词要额外排掉
//
// 一次任务可能切出十几个候选，每个都是一次行情往返。所以用 commonWords
// 先砍掉最明显的英文虚词 —— 它们不可能命中，问了也是白问。
var candidatePattern = regexp.MustCompile(`\b(?:[a-zA-Z]{2,10}-[a-zA-Z]{2,10}|[a-zA-Z]{2,5}|\d{6}|\d{5})\b`)

// commonWords 是「绝不可能是标的、但会被 candidatePattern 切出来」的英文虚词。
//
// # 为什么是黑名单而不是白名单
//
// 白名单（「只允许已知标的」）等于要求用户先 sync —— 那就回到了选器的
// 老问题。黑名单是**性能优化**而不是正确性保证：漏掉的词会让行情源多被问
// 一次，但不会产生错误结果（验证失败就不登记）。
var commonWords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "this": true,
	"that": true, "from": true, "have": true, "will": true, "can": true,
	"not": true, "are": true, "was": true, "but": true, "you": true,
	"all": true, "any": true, "how": true, "why": true, "what": true,
	"when": true, "about": true, "into": true, "over": true, "than": true,
	"then": true, "there": true, "these": true, "they": true, "more": true,
	"most": true, "some": true, "such": true, "only": true, "also": true,
	"its": true, "it": true, "is": true, "of": true, "on": true, "or": true,
	"as": true, "at": true, "be": true, "by": true, "do": true, "if": true,
	"in": true, "no": true, "so": true, "to": true, "up": true, "us": true,
	"we": true, "an": true, "my": true, "me": true,
}

// inferMarket 从代码形态推断市场。
//
// # 为什么可以推断（而 asset_class 不能）
//
// 市场的形态特征足够强（A 股 6 位数字、港股 5 位、加密带连字符），
// 而 asset_class 决定阶段图模板（stock 有财报维度，crypto 没有）——
// 猜错会让整条分析链问错问题。所以：market 可以推断，asset_class 一律
// 按「股票」登记并要求行情验证真实存在（ETF/指数 用户应显式走 sync 端点指定）。
func inferMarket(symbol string) string {
	if strings.Contains(symbol, "-") {
		return "crypto"
	}
	if len(symbol) == 6 && isAllDigits(symbol) {
		return "cn"
	}
	if len(symbol) == 5 && isAllDigits(symbol) {
		return "hk"
	}
	return "us"
}

func isAllDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return len(s) > 0
}

// CandidateSymbols 从文本里切出「像代码」的候选，按出现顺序去重。
//
// 导出给 createTask 在「库中认不出」时用：它需要把候选逐个交给行情源验证，
// 验证通过才登记 —— 不通过就不登记，避免脏数据。
//
// 输出统一**大写**：行情源与 instruments 表都按大写存代码，
// 用户写 msft 时不能拿着小写去查（akshare/OKX 会查不到）。
func CandidateSymbols(texts ...string) []string {
	blob := strings.Join(texts, "\n")
	matches := candidatePattern.FindAllString(blob, -1)
	seen := map[string]bool{}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		up := strings.ToUpper(m)
		if commonWords[strings.ToLower(m)] {
			continue
		}
		// 单字符 / 两字符纯字母极难是代码（美股最短 1 位但极少），
		// 且候选数越多行情往返越多。所以纯字母要求 ≥2 已在正则里保证。
		if seen[up] {
			continue
		}
		seen[up] = true
		out = append(out, up)
	}
	return out
}

// resolveInstrumentFromTaskText 从任务文本解析标的，必要时自动登记。
//
// 返回 nil 表示「认不出」—— 调用方照建任务（但不建阶段图、走通用 prompt）。
//
// # 为什么要自动登记
//
// 用户写「给出 msft 交易策略」，而库里只有 7 条标的时，光靠查库永远认不出
// MSFT。要求用户先去 sync 端点登记再建任务，等于把选标的的工作从下拉框
// 搬到了一个更隐蔽的地方 —— 那不比选器好用。
//
// 所以：文本里的候选 → 行情源验证 → 通过则登记（asset_class=stock）→ 用它。
// 验证不通过（代码不存在 / 源故障）就不登记，任务照建但不绑标的。
func (s *Server) resolveInstrumentFromTaskText(r *http.Request, texts ...string) *db.Instrument {
	if s.m == nil || s.m.pg == nil {
		return nil
	}
	store := s.m.pg.Instruments()

	// ① 库里已有 → 直接用。
	if inst, ok := ResolveInstrumentFromText(store, texts...); ok {
		return inst
	}

	// ② 库里没有 → 候选逐个验证 + 登记，第一个验过的胜出。
	//
	// # 为什么只登记第一个
	//
	// 一个任务分析一个标的（tasks.instrument_id 是单值）。多个候选都验证通过时
	// （如「比较 MSFT 和 AAPL」），取第一个是**有损的** —— 但比「一个都不绑」
	// 好：至少主线分析有标的可用，第二个可以由用户在目标里再描述。
	// 真正的多标的对比需要 tasks 支持一对多，那是独立改造。
	for _, symbol := range CandidateSymbols(texts...) {
		market := inferMarket(symbol)
		if code, msg, ok := s.verifyInstrumentSymbol(r, market, symbol); !ok {
			// INSTRUMENT_NOT_FOUND 是正常的「这不是代码」—— 静默跳过下一个候选。
			// 其余错误（源故障）也一样跳过：验证不了就不登记，避免脏数据。
			_ = code
			_ = msg
			continue
		}
		id, created, err := store.UpsertInstrument(db.InstrumentInput{
			Market: market, Symbol: symbol, AssetClass: db.AssetClassStock,
		})
		if err != nil {
			continue
		}
		if created {
			log.Printf("[instrument] 从任务文本自动登记标的 %s/%s (id=%d)", market, symbol, id)
		}
		inst, getErr := store.GetInstrumentByID(id)
		if getErr != nil {
			return nil
		}
		return inst
	}
	return nil
}
