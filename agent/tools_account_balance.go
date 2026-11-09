package agent

// get_account_balance：读取交易账户资产（需要有效凭证）。
//
// # 为什么这个工具必须存在（2026-11-09 任务 #4 翻车）
//
// 任务目标是「使用全部账户余额，赚取 1000 USDT」。agent 需要知道余额才能算仓位，
// 但系统里**没有任何余额工具** —— 它只能开 shell 手摸 `okx` CLI，然后在
// `env` 里看到 OKX_API_KEY 为空（注入只给 CLI 子进程，不进 server 进程环境），
// 于是去跑 `okx-auth status` 拿到 not_logged_in，得出「交易账户未登录」的
// **错误结论**。而同期意图 #194 用 quote_okx + get_crypto_flow 就「验证通过」
// 了凭据 —— 那两个接口是公开行情，根本不需要凭证。
//
// 所以这个工具同时解决两件事：
//  1. 给 agent 一个结构化取余额的入口（不需要 shell）；
//  2. 成为**唯一**真凭据校验手段：取得到余额 = 凭证必然有效。
//
// 它走 AccountClient 而不是 MarketClient，理由见 market_client.go 里的接口注释。

import (
	"context"
	"encoding/json"
	"fmt"

	actool "github.com/Autumn-27/norma/tool"
)

func (m *MarketToolSet) getAccountBalance() actool.CoreTool {
	meta := marketToolMetas["get_account_balance"]
	return readTool(
		"get_account_balance",
		"读取**交易账户**的资产与余额（需要有效的 API 凭证）。\n"+
			"这是唯一能验证交易凭证是否可用的接口 —— 行情类工具（quote_okx 等）\n"+
			"走的是公开接口，不需要凭证，**不能**用来判断凭证是否有效。\n\n"+
			"返回：total_eq（账户总权益，USD）、currencies[]（按币种明细：\n"+
			"  ccy/eq/cash_bal/avail_bal/frozen_bal/eq_usd）。\n"+
			"只返回**余额非零**的币种 —— 空数组表示账户确实没有资产，不是查询失败。\n\n"+
			"⚠ 金额全部是**字符串**（精度铁律），不要当 float 解析。\n"+
			"⚠ 返回的是**当下快照**，没有历史版本。\n\n"+
			"失败时看错误码：\n"+
			"  UNAUTHORIZED  → 凭证无效/未配置（这是**凭证问题**，不是网络）\n"+
			"  OKX_CLI_NOT_FOUND → CLI 未安装（环境问题）",
		obj(map[string]any{}),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			// 无入参：账户是凭证绑定的，没有「查哪个账户」的概念。
			// 仍要读一次 input 以保持 readTool 的约定（可能是空对象或空字节）。
			_ = in

			if m.account == nil {
				return actool.Errorf(
					"余额通道未配置（OKX AccountClient 未注入）。" +
						"这通常意味着 server 启动时没有构造 OKX 客户端。"), nil
			}

			cctx, cancel := context.WithTimeout(ctx, meta.Timeout)
			defer cancel()

			bal, err := m.account.FetchBalance(cctx)
			if err != nil {
				// 凭证类错误单独给一句人话 —— agent 最容易在这里误判：
				// 它会把「unauthorized」当成「需要跑 okx-auth login」，
				// 而 okx-auth 是 OAuth 登录态，与 API key 凭证是两套东西。
				if me, ok := err.(*MarketError); ok && me.Code == "UNAUTHORIZED" {
					return actool.Errorf(fmt.Sprintf(
						"交易账户凭证不可用：%s\n"+
							"排查顺序：① 系统 → 凭证页是否填了 OKX 交易（trade）凭证且已启用；"+
							"② 该 key 与「模拟盘/实盘」开关是否与 key 的环境一致（错配报 50101，容易被误读成 key 无效）；"+
							"③ 注意 okx-auth 的 OAuth 登录态与本凭证**无关**，不要用 okx-auth status 判断。",
						me.Message)), nil
				}
				return actool.Errorf(fmt.Sprintf("读取账户余额失败: %v", err)), nil
			}

			out := map[string]any{
				"total_eq":    bal.TotalEq,
				"adjusted_eq": bal.AdjustedEq,
				"upl":         bal.Upl,
				"update_time": bal.UpdateTime,
				"currencies":  bal.Currencies,
				"price_kind":  string(meta.SourceKind), // realtime：快照，非历史
			}
			if len(bal.Currencies) == 0 {
				// 显式说明，避免模型把「没有币种」当成「工具坏了」。
				out["note"] = "账户存在但各币种余额均为 0（不是查询失败）"
			}
			return jsonResult(out)
		},
	)
}
