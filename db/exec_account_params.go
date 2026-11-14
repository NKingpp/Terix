package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// OkxTradeCred 是一条可用的 OKX 交易凭据（含全量敏感字段）。
//
// # 为什么单独一个类型
//
// MarketCredential 刻意不含敏感字段（读路径不查敏感列）。下单需要三元组，
// 所以必须有一个显式命名为「拿得到明文」的类型 —— 用到它的地方一眼能看出
// 这是敏感路径。
type OkxTradeCred struct {
	ID         int64
	APIKey     string
	SecretKey  string
	Passphrase string
	Demo       bool
	BaseURL    string
}

// ActiveOkxTradeCredential 取最后一条启用的 okx/trade 凭据。
//
// 口径与 server.injectOkxCredential 完全一致（倒序 + enabled），抽到一处是为了
// 避免「注入给 CLI 的 key」与「下单用的 key」在用户禁用/新建后分叉 ——
// 那种分叉的报错指向 OKX 侧（50119 key 不存在），极难排查。
func (d *DB) ActiveOkxTradeCredential() (*OkxTradeCred, error) {
	list, err := d.ListMarketCredentials()
	if err != nil {
		return nil, err
	}
	for i := len(list) - 1; i >= 0; i-- {
		c := list[i]
		if c.Provider != "okx" || c.Kind != "trade" || !c.Enabled {
			continue
		}
		key, secret, pass, extra, kErr := d.GetMarketCredentialSecrets(c.ID)
		if kErr != nil {
			if errors.Is(kErr, ErrCredentialNotFound) {
				continue
			}
			return nil, fmt.Errorf("读取 okx 交易凭据 %d 失败: %w", c.ID, kErr)
		}
		if strings.TrimSpace(key) == "" {
			continue
		}
		return &OkxTradeCred{
			ID:         c.ID,
			APIKey:     strings.TrimSpace(key),
			SecretKey:  strings.TrimSpace(secret),
			Passphrase: strings.TrimSpace(pass),
			Demo:       strings.EqualFold(strings.TrimSpace(extra["demo"]), "true"),
			BaseURL:    strings.TrimSpace(c.BaseURL),
		}, nil
	}
	return nil, ErrCredentialNotFound
}

// ExecAccountParams 是账户级的 swap 交易参数（侧表 exec_account_params）。
type ExecAccountParams struct {
	AccountID int64
	PosMode   string // net | long_short
	MgnMode   string // cross | isolated
	Lever     string
}

// GetExecAccountParams 读账户参数；无记录时返回默认值（net / cross / 3），
// 与「加列前的历史账户」语义一致。
func (d *DB) GetExecAccountParams(accountID int64) (ExecAccountParams, error) {
	out := ExecAccountParams{AccountID: accountID, PosMode: "net", MgnMode: "cross", Lever: "3"}
	var pos, mgn, lever sql.NullString
	err := d.QueryRow(`SELECT pos_mode, mgn_mode, lever FROM exec_account_params WHERE account_id=$1`, accountID).
		Scan(&pos, &mgn, &lever)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if pos.Valid && pos.String != "" {
		out.PosMode = pos.String
	}
	if mgn.Valid && mgn.String != "" {
		out.MgnMode = mgn.String
	}
	if lever.Valid && lever.String != "" {
		out.Lever = lever.String
	}
	return out, nil
}

// UpsertExecAccountParams 写账户参数（幂等）。
func (d *DB) UpsertExecAccountParams(p ExecAccountParams) error {
	if p.PosMode == "" {
		p.PosMode = "net"
	}
	if p.MgnMode == "" {
		p.MgnMode = "cross"
	}
	if p.Lever == "" {
		p.Lever = "3"
	}
	if p.PosMode != "net" && p.PosMode != "long_short" {
		return fmt.Errorf("pos_mode 只能是 net 或 long_short，收到 %q", p.PosMode)
	}
	if p.MgnMode != "cross" && p.MgnMode != "isolated" {
		return fmt.Errorf("mgn_mode 只能是 cross 或 isolated，收到 %q", p.MgnMode)
	}
	_, err := d.Exec(`INSERT INTO exec_account_params(account_id, pos_mode, mgn_mode, lever, updated_at)
VALUES ($1,$2,$3,$4,now())
ON CONFLICT (account_id) DO UPDATE SET pos_mode=excluded.pos_mode, mgn_mode=excluded.mgn_mode,
  lever=excluded.lever, updated_at=now()`, p.AccountID, p.PosMode, p.MgnMode, p.Lever)
	return err
}
