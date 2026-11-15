package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/exec/okxrest"
)

// 执行账户的人工管理接口。
//
// # 为什么必须显式创建
//
// 「建一条执行账户」等于「允许这个服务对真实资金下单」，不存在合理的自动
// 创建路径 —— ensurePaperAutoAccount 那种兜底只配用于纸面账本。所以这里
// 只提供人工入口，且 POST 时会先用真实凭证打一次 account/config：
// 拿不到 uid 就说明凭证或网络有问题，此时建档只会制造一个「看起来能用」的账户。

// execAccountDTO 是账户 + 账户参数 + 未平仓跟踪数的展示结构。
type execAccountDTO struct {
	*db.ExecAccount
	PosMode     string `json:"pos_mode"`
	MgnMode     string `json:"mgn_mode"`
	Lever       string `json:"lever"`
	OpenTracked int    `json:"open_tracked"`
}

func (s *Server) listExecAccounts(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	accts, err := pg.ExecAccounts().ListAccounts()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out := make([]execAccountDTO, 0, len(accts))
	for _, a := range accts {
		params, _ := pg.GetExecAccountParams(a.ID)
		open, _ := pg.ListOpenLivePositions(a.ID)
		out = append(out, execAccountDTO{
			ExecAccount: a, PosMode: params.PosMode, MgnMode: params.MgnMode,
			Lever: params.Lever, OpenTracked: len(open),
		})
	}
	writeJSON(w, 200, map[string]any{"accounts": out})
}

func (s *Server) createExecAccount(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var body struct {
		Channel          string   `json:"channel"`
		ChannelAccountID string   `json:"channel_account_id"`
		Label            string   `json:"label"`
		Market           string   `json:"market"`
		MarketSupport    []string `json:"market_support"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, "请求体格式错误："+err.Error())
		return
	}
	channel := strings.TrimSpace(body.Channel)
	if channel != "okx" {
		writeErr(w, 400, "当前只支持创建 okx 执行账户")
		return
	}
	uid := strings.TrimSpace(body.ChannelAccountID)
	posMode := "net"
	if uid == "" {
		// 探测一次真实账户：拿不到 uid 说明凭证/网络有问题，此时建档只会
		// 制造一个「看起来能用但下不了单」的账户。
		cred, err := s.okxTradeCred()
		if err != nil {
			writeErr(w, 400, "还没有可用的 OKX 交易凭据（先在行情凭据页配置 okx/trade）")
			return
		}
		client := okxrest.NewClient(okxrest.Credentials{
			APIKey: cred.APIKey, SecretKey: cred.SecretKey, Passphrase: cred.Passphrase,
			Demo: cred.Demo, BaseURL: cred.BaseURL,
		})
		cfg, err := client.GetAccountConfig(r.Context())
		if err != nil {
			writeErr(w, 400, "探测 OKX 账户失败："+err.Error())
			return
		}
		uid = strings.TrimSpace(cfg.UID)
		if cfg.PosMode != "" {
			posMode = strings.TrimSpace(strings.TrimSuffix(cfg.PosMode, "_mode"))
		}
	}
	if uid == "" {
		writeErr(w, 400, "无法确定 OKX uid，请手工填写 channel_account_id")
		return
	}
	market := strings.TrimSpace(body.Market)
	if market == "" {
		market = "crypto"
	}
	support := body.MarketSupport
	if len(support) == 0 {
		// 美股永续在 instruments 里记 market=crypto（OKX 的 GATE 校验只认
		// us/hk/cn/crypto），所以默认给 crypto + us 两个，避免开仓被市场过滤挡住。
		support = []string{"crypto", "us"}
	}
	acct, err := pg.ExecAccounts().UpsertAccount(db.ExecAccountInput{
		Channel: channel, ChannelAcctID: uid, Label: strings.TrimSpace(body.Label),
		Market: market, MarketSupport: support, Active: true,
	})
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := pg.UpsertExecAccountParams(db.ExecAccountParams{
		AccountID: acct.ID, PosMode: posMode, MgnMode: "cross", Lever: "1",
	}); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	params, _ := pg.GetExecAccountParams(acct.ID)
	writeJSON(w, 200, execAccountDTO{
		ExecAccount: acct, PosMode: params.PosMode, MgnMode: params.MgnMode, Lever: params.Lever,
	})
}

func (s *Server) patchExecAccount(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok || id <= 0 {
		writeErr(w, 400, "id 无效")
		return
	}
	var body struct {
		Label         *string   `json:"label"`
		MarketSupport *[]string `json:"market_support"`
		Active        *bool     `json:"active"`
		PosMode       *string   `json:"pos_mode"`
		MgnMode       *string   `json:"mgn_mode"`
		Lever         *string   `json:"lever"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, "请求体格式错误："+err.Error())
		return
	}
	found := false
	accts, err := pg.ExecAccounts().ListAccounts()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	for _, a := range accts {
		if a.ID != id {
			continue
		}
		found = true
		if body.Label != nil {
			a.Label = strings.TrimSpace(*body.Label)
		}
		if body.MarketSupport != nil {
			a.MarketSupport = *body.MarketSupport
		}
		if body.Active != nil {
			a.Active = *body.Active
		}
		if _, err := pg.ExecAccounts().UpsertAccount(db.ExecAccountInput{
			Channel: a.Channel, ChannelAcctID: a.ChannelAcctID, Label: a.Label,
			Market: a.Market, MarketSupport: a.MarketSupport, Active: a.Active,
		}); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
	}
	if !found {
		writeErr(w, 404, "执行账户不存在")
		return
	}
	if body.PosMode != nil || body.MgnMode != nil || body.Lever != nil {
		cur, err := pg.GetExecAccountParams(id)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if body.PosMode != nil {
			cur.PosMode = strings.TrimSpace(*body.PosMode)
		}
		if body.MgnMode != nil {
			cur.MgnMode = strings.TrimSpace(*body.MgnMode)
		}
		if body.Lever != nil {
			cur.Lever = strings.TrimSpace(*body.Lever)
		}
		if err := pg.UpsertExecAccountParams(cur); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// execLoopStatus 返回执行层状态（前端卡片唯一数据源）。
//
// # 2026-11-14 起没有「交易循环」
//
// 旧实现是一个每 15 秒扫草案的后台 goroutine（TradeLoop）。它的问题不是
// 性能而是**控制权**：模型写完草案就结束，之后成交与否、仓位被套怎么办
// 全由循环按固定规则决定，模型既看不见也无法干预。
//
// 现在下单是 LLM 直接调工具（open_position / close_position），同步拿回执。
// 所以这个端点只回答「执行层是否可用」——保留字段是为了让前端不必改版：
// running 恒 true（执行能力就绪即视为在服务），positions 是交易所真实持仓数。
func (s *Server) execLoopStatus(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{
		"running": true, "mode": "", "positions": 0,
		"driver": "tools", // 下单由工具驱动，不再有后台循环
	}
	mode, err := s.m.pg.GetExecMode()
	if err == nil {
		out["mode"] = string(mode)
	}
	// 持仓数与「是否裸仓」是模型最关心的两件事，顺手带上，
	// 免得前端为了一个数字再打一次交易所。
	if cred, cErr := s.okxTradeCred(); cErr == nil {
		client := okxrest.NewClient(okxrest.Credentials{
			APIKey: cred.APIKey, SecretKey: cred.SecretKey, Passphrase: cred.Passphrase,
			Demo: cred.Demo, BaseURL: cred.BaseURL,
		})
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		if ps, pErr := client.GetPositions(ctx, "SWAP"); pErr == nil {
			n := 0
			for _, p := range ps {
				if q := strings.TrimSpace(p.Pos); q != "" && q != "0" {
					n++
				}
			}
			out["positions"] = n
		}
		if algos, aErr := client.ListAlgoOrders(ctx, "SWAP", "oco"); aErr == nil {
			out["protection_orders"] = len(algos)
		}
	}
	writeJSON(w, 200, out)
}
