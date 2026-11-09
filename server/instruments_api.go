package server

// 标的入库 API（POST /api/instruments/sync）。
//
// # 为什么需要这个端点
//
// `instruments` 表原本只有 seedBenchmarkInstruments 写入的 4 行基准（QQQ/^NDX/^TNX/^VIX），
// server 层从未写过它 —— 所以用户无法「选一只股票」来做金融任务是这个缺口的直接后果。
//
// # 为什么叫 sync 而不是 create
//
// 语义是「确保这些标的在库里」，不是「插入一条记录」。已存在时更新描述性字段
// 而不是报错（用户可能想补充 name/sector）。
//
// # 为什么 asset_class 必填且不可变
//
// 它直接决定 agent.Expand 用哪个阶段图模板（stock 17 节点含 fundamental /
// crypto 13 节点无 fundamental）。猜错不只是「分类不准」，是**整个分析流程走错**
// （stock 模板套到 crypto 上会去找财报，找不到后返回 unavailable，整条链白跑）。
// 所以：不推断、不给默认值、已存在时不允许改。

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// maxSyncInstruments 单次 sync 的标的数上限。
//
// 为什么限 100：这个端点是「手工录入选中的标的」，不是批量导入通道。
// 批量导入应该走离线脚本（skip_verify=true 直连 DB），不该用 HTTP。
const maxSyncInstruments = 100

// syncInstrumentItem 是单条标的入参。
type syncInstrumentItem struct {
	Symbol     string `json:"symbol"`      // 必填
	AssetClass string `json:"asset_class"` // 必填：stock/etf/index/crypto/future/option
	Name       string `json:"name,omitempty"`
	Currency   string `json:"currency,omitempty"`
	Exchange   string `json:"exchange,omitempty"`
	Sector     string `json:"sector,omitempty"`
	Industry   string `json:"industry,omitempty"`
}

// syncInstrumentsReq 是 POST /api/instruments/sync 的入参。
type syncInstrumentsReq struct {
	Market string               `json:"market"` // 必填：us/hk/cn/crypto
	Items  []syncInstrumentItem `json:"items"`
	// SkipVerify 跳过行情验证。默认 false（会调 get_quote 确认 symbol 真实存在）。
	// true 用于离线批量导入（不想每个都打网络）。
	SkipVerify bool `json:"skip_verify,omitempty"`
}

// syncInstrumentResult 是单条标的的处理结果。
type syncInstrumentResult struct {
	ID         int64  `json:"id,omitempty"`
	Market     string `json:"market,omitempty"`
	Symbol     string `json:"symbol"`
	AssetClass string `json:"asset_class,omitempty"`
}

// syncInstrumentFailure 是单条标的的失败结果。
//
// ErrorCode 必须区分两种原因，因为它们对用户的指示完全不同：
//   - INSTRUMENT_NOT_FOUND → 符号不存在（用户拼错，或市场选错）→ 「检查拼写」
//   - UPSTREAM_BLOCKED / UPSTREAM_TIMEOUT → 源故障（符号可能是对的）→
//     「暂时无法验证，可加 skip_verify=true 强制入库」
//
// 混成一个错误会让用户误改一个本来正确的代码，越改越错。
type syncInstrumentFailure struct {
	Symbol    string `json:"symbol"`
	ErrorCode string `json:"error_code"`
	Error     string `json:"error"`
}

// syncInstrumentsResp 是 POST /api/instruments/sync 的响应。
type syncInstrumentsResp struct {
	Created []syncInstrumentResult  `json:"created"`
	Updated []syncInstrumentResult  `json:"updated"`
	Failed  []syncInstrumentFailure `json:"failed"`
	// Verified 报告本次是否做了行情验证（skip_verify=true 时为 false）。
	Verified bool `json:"verified"`
}

// pgSyncInstruments 处理 POST /api/instruments/sync。
func (s *Server) pgSyncInstruments(w http.ResponseWriter, r *http.Request) {
	var req syncInstrumentsReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "请求体解析失败："+err.Error())
		return
	}
	market := strings.TrimSpace(req.Market)
	if market == "" {
		writeErr(w, 400, "market 不能为空（us/hk/cn/crypto）")
		return
	}
	if len(req.Items) == 0 {
		writeErr(w, 400, "items 不能为空")
		return
	}
	if len(req.Items) > maxSyncInstruments {
		writeErr(w, 400, fmt.Sprintf("单次最多 %d 个标的，收到 %d 个", maxSyncInstruments, len(req.Items)))
		return
	}
	store := s.m.pg.Instruments()

	resp := syncInstrumentsResp{
		Created:  []syncInstrumentResult{},
		Updated:  []syncInstrumentResult{},
		Failed:   []syncInstrumentFailure{},
		Verified: !req.SkipVerify,
	}

	for _, it := range req.Items {
		symbol := strings.TrimSpace(it.Symbol)
		if symbol == "" {
			resp.Failed = append(resp.Failed, syncInstrumentFailure{
				Symbol: "", ErrorCode: "INVALID_INPUT", Error: "symbol 不能为空",
			})
			continue
		}
		assetClass := strings.TrimSpace(it.AssetClass)
		if assetClass == "" {
			resp.Failed = append(resp.Failed, syncInstrumentFailure{
				Symbol: symbol, ErrorCode: "INVALID_INPUT",
				Error: "asset_class 必填（stock/etf/index/crypto/future/option）—— " +
					"它决定阶段图模板，不能推断",
			})
			continue
		}

		// 行情验证：确认 symbol 真实存在。失败按原因分类。
		if !req.SkipVerify {
			if code, msg, ok := s.verifyInstrumentSymbol(r, market, symbol); !ok {
				resp.Failed = append(resp.Failed, syncInstrumentFailure{
					Symbol: symbol, ErrorCode: code, Error: msg,
				})
				continue
			}
		}

		id, created, err := store.UpsertInstrument(db.InstrumentInput{
			Market:     market,
			Symbol:     symbol,
			Name:       it.Name,
			AssetClass: assetClass,
			Currency:   it.Currency,
			Exchange:   it.Exchange,
			Sector:     it.Sector,
			Industry:   it.Industry,
		})
		if err != nil {
			code := "INTERNAL"
			msg := err.Error()
			switch {
			case errors.Is(err, db.ErrInstrumentAssetClassConflict):
				code = "ASSET_CLASS_CONFLICT"
			case errors.Is(err, db.ErrInstrumentInvalid):
				code = "INVALID_INPUT"
			}
			resp.Failed = append(resp.Failed, syncInstrumentFailure{
				Symbol: symbol, ErrorCode: code, Error: msg,
			})
			continue
		}

		out := syncInstrumentResult{ID: id, Market: market, Symbol: symbol, AssetClass: assetClass}
		if created {
			resp.Created = append(resp.Created, out)
		} else {
			resp.Updated = append(resp.Updated, out)
		}
	}

	// 逐项结果总是 HTTP 200：部分失败是正常结果，不是请求错误。
	// 只有「请求本身不合法」（缺 market / items 空 / 超限）才 400。
	writeJSON(w, 200, resp)
}

// verifyInstrumentSymbol 用 get_quote 确认 symbol 在该 market 真实存在。
//
// 返回 (errorCode, message, ok)。ok=false 时调用方把该标的记入 failed。
//
// 错误分类的依据是「用户该怎么改」：
//   - INSTRUMENT_NOT_FOUND：符号确实查不到 → 让用户检查拼写/market
//   - UPSTREAM_*：源故障 → 符号可能是对的，让用户用 skip_verify 试
//
// 区分这两种是必要的：如果源故障也报「符号不存在」，用户会去改一个
// 本来正确的代码，越改越错。
func (s *Server) verifyInstrumentSymbol(r *http.Request, market, symbol string) (code, msg string, ok bool) {
	// 走 server 装配的客户端（New() 里构造一次，全 server 复用连接池）。
	cli := s.marketClientFor(market)
	if cli == nil {
		// fallback：装配未注入时的兜底。正常路径应该走 marketClientFor。
		// 待 analyst.go 落地、所有调用点迁移到装配后，这段可以删。
		// 存在原因：既有测试可能用零值 Server（未经 New()），需要能跑。
		if market == "crypto" {
			cli = agent.NewOkxClient("")
		} else {
			cli = agent.NewAkshareClient("")
		}
	}
	q, err := cli.FetchQuote(r.Context(), symbol, market, "")
	if err != nil {
		// 用错误码区分「查不到」与「源故障」。
		var me *agent.MarketError
		if errors.As(err, &me) {
			switch me.Code {
			case "NO_DATA", "OKX_NO_DATA", "SYMBOL_NOT_FOUND":
				return "INSTRUMENT_NOT_FOUND",
					fmt.Sprintf("该 symbol 在 %s 市场查不到，请检查拼写或市场是否正确", market), false
			case "UPSTREAM_TIMEOUT", "TIMEOUT":
				return "UPSTREAM_TIMEOUT",
					"行情源超时，symbol 可能正确；可加 skip_verify=true 强制入库", false
			case "UPSTREAM_BLOCKED", "CHANNEL_NOT_APPLICABLE", "ALL_CHANNELS_FAILED":
				return "UPSTREAM_BLOCKED",
					"行情源暂时不可用，symbol 可能正确；可加 skip_verify=true 强制入库", false
			}
			// 其它已知错误码：仍按「可能是源问题」处理，不误伤正确代码。
			return "UPSTREAM_ERROR",
				fmt.Sprintf("行情校验失败（%s）；可加 skip_verify=true 强制入库", me.Code), false
		}
		return "UPSTREAM_ERROR",
			"行情校验失败，symbol 可能正确；可加 skip_verify=true 强制入库", false
	}
	if q == nil || strings.TrimSpace(q.Last) == "" {
		return "INSTRUMENT_NOT_FOUND",
			fmt.Sprintf("该 symbol 在 %s 市场查不到有效行情，请检查拼写", market), false
	}
	return "", "", true
}

// listInstruments GET /api/instruments
//
// # 为什么需要这条路由
//
// 原先只有 POST /api/instruments/sync —— 那是「从外部行情源拉数据入库」。
// 而「新建任务时让用户选已有标的」需要「列出库里已有的」，二者是不同操作：
// 前者写入、后者只读。没有只读路由，前端就只能从 404 里猜。
//
// # 为什么按 asset_class 分组交给前端做
//
// 后端只返回平铺列表 —— 分组是纯展示逻辑，而前端的标的选择器要按
// stock/etf/index/crypto 分节显示。放后端会让「换一种分组方式」需要改 API。
//
// # 为什么不默认过滤 active
//
// `active` 是软删除标记。测试建的标的 active=true，但将来若有人停用某标的，
// 「已停用但历史任务还在用」的标的仍需可查 —— 所以默认返回全部，
// 由调用方显式传 active=true 才过滤。
func (s *Server) listInstruments(w http.ResponseWriter, r *http.Request) {
	// s.pg 内部检查 s.m.pg 非 nil，为 nil 时自己写 503 并返回 nil。
	// ⚠️ 所以 nil 时只能 return —— 再写一次响应会 superfluous WriteHeader。
	pg := s.pg(w)
	if pg == nil {
		return
	}

	q := r.URL.Query()
	filter := db.InstrumentFilter{
		Market: strings.TrimSpace(q.Get("market")),
		Limit:  queryInt(r, "limit", 0), // <=0 时 ListInstruments 用默认 200
	}
	if v := q.Get("active"); v != "" {
		active := v == "true"
		filter.Active = &active
	}

	list, err := pg.Instruments().ListInstruments(filter)
	if err != nil {
		writeErr(w, 503, errDataSourceUnavailable)
		return
	}
	writeJSON(w, 200, map[string]any{"instruments": list})
}
