package agent

// get_crypto_flow：加密永续合约的资金费率与持仓量。
//
// 多空比（long/short ratio）：OKX **无公开命令**。已实测确认：
//   - `okx smartmoney` 只有排行榜、持仓分析、共识信号，无多空比
//   - `okx market pair-spread` 是两个标的的价差统计，不是多空持仓比
//   - `okx swap/futures` 的 long/short 只是下单时的 posSide 参数
// 所以 P0 不做，响应里显式列在 unsupported_fields，不用 0/null 假装有数据。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	actool "github.com/Autumn-27/norma/tool"
)

func (m *MarketToolSet) getCryptoFlow() actool.CoreTool {
	meta := marketToolMetas["get_crypto_flow"]
	return readTool(
		"get_crypto_flow",
		"查询加密永续合约的资金流数据：**资金费率**与**持仓量**。\n"+
			"仅适用于 crypto 标的（A股/港股/美股没有资金费率概念，调用会明确报错）。\n"+
			"symbol 用 SPOT 代码即可（如 BTC-USDT），内部自动转成永续 BTC-USDT-SWAP。\n"+
			"两项数据独立获取：若只有一项成功，返回成功的那项 + unavailable 里列出失败项及原因，\n"+
			"  不会整体失败——但**必须看 unavailable**，不要以为缺失项是 0。\n"+
			"注意：返回的是**当下快照**（realtime），没有历史版本，不能用于回测重放。\n"+
			"多空比(long/short ratio)：OKX 无公开命令，P0 未实现。",
		obj(map[string]any{
			"symbol": str("标的代码，如 BTC-USDT（SPOT 代码即可，内部转 SWAP）"),
			// enum 是「成本优化」：LLM 看到 enum 就不会传 market:"cn"，省一次工具往返。
			// handler 里的硬校验仍是「安全边界」，两层各有作用，不互相替代。
			"market": map[string]any{
				"type": "string",
				"enum": []string{"crypto"},
				"description": "固定为 crypto。本工具仅支持加密标的——" +
					"A股/港股/美股无资金费率与持仓量数据。",
			},
		}, "symbol", "market"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Symbol string `json:"symbol"`
				Market string `json:"market"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf(fmt.Sprintf("入参解析失败: %v", err)), nil
			}
			symbol := strings.TrimSpace(a.Symbol)
			market := strings.ToLower(strings.TrimSpace(a.Market))

			// 1. 显式拒绝非 crypto —— **不返回空结果**。
			// 返回空结果会让 LLM 以为「这只股票没有资金费率数据」（即数据为 0），
			// 实际是「这个工具对 A 股根本不适用」。两者的下游行为完全不同。
			if market != "crypto" {
				return actool.Errorf(fmt.Sprintf(
					"get_crypto_flow 仅适用于 crypto 标的（当前 market=%q）。"+
						"A股/港股/美股无资金费率与持仓量数据。", market)), nil
			}
			if symbol == "" {
				return actool.Errorf("缺少 symbol"), nil
			}

			// 2. 通道检查
			if m.cryptoFlow == nil {
				return actool.Errorf("OKX 通道未配置，无法获取加密资金流数据"), nil
			}

			cctx, cancel := context.WithTimeout(ctx, meta.Timeout)
			defer cancel()

			// 3. 并行取两项（独立数据源，互不依赖；串行会让 10s 超时更易触发）
			type frRes struct {
				v   *FundingRate
				err error
			}
			type oiRes struct {
				v   *OpenInterest
				err error
			}
			frCh := make(chan frRes, 1)
			oiCh := make(chan oiRes, 1)
			go func() {
				v, err := m.cryptoFlow.FetchFundingRate(cctx, symbol)
				frCh <- frRes{v, err}
			}()
			go func() {
				v, err := m.cryptoFlow.FetchOpenInterest(cctx, symbol)
				oiCh <- oiRes{v, err}
			}()
			fr := <-frCh
			oi := <-oiCh

			// 4. 两个都失败 → 整体失败（没有部分数据可返回）
			if fr.err != nil && oi.err != nil {
				return actool.Errorf(fmt.Sprintf(
					"get_crypto_flow 全部失败: funding_rate: %v | open_interest: %v",
					fr.err, oi.err)), nil
			}

			// 5. 部分成功：返回成功的部分 + unavailable 明确列出失败项。
			// 理由：LLM 拿到部分数据比拿到空数据有用；但缺失项必须显式标注，
			// 否则它会以为持仓量是 0。
			type unavail struct {
				Field  string `json:"field"`
				Reason string `json:"reason"`
			}
			var unavailable []unavail

			out := map[string]any{
				"symbol":           symbol,
				"market":           market,
				"price_kind":       string(meta.SourceKind), // realtime：快照，非历史
				"funding_rate":     nil,
				"open_interest":    nil,
				"unavailable":      nil,
				"long_short_ratio": nil, // 显式置 null 并说明，避免被当成 0
			}
			if fr.err == nil {
				out["funding_rate"] = fr.v
			} else {
				unavailable = append(unavailable, unavail{"funding_rate", marketErrCode(fr.err)})
			}
			if oi.err == nil {
				out["open_interest"] = oi.v
			} else {
				unavailable = append(unavailable, unavail{"open_interest", marketErrCode(oi.err)})
			}
			if len(unavailable) > 0 {
				out["unavailable"] = unavailable
			}
			// 多空比：OKX 无公开命令。显式说明，不让字段静默缺失。
			out["unsupported_fields"] = []string{
				"long_short_ratio: OKX 无公开的命令（smartmoney 只有排行榜与持仓分析，market pair-spread 是价差统计）",
			}
			return jsonResult(out)
		},
	)
}

// marketErrCode 提取 MarketError 的 Code；非 MarketError 返回 "UNKNOWN"。
func marketErrCode(err error) string {
	var me *MarketError
	if errors.As(err, &me) {
		return me.Code
	}
	return "UNKNOWN"
}
