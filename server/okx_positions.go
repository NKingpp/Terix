package server

import (
	"context"
	"sync"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/exec/okxrest"
)

// okxPositionsProvider 给 agent 提供「当前持仓」只读快照。
//
// # 为什么要缓存
//
// 每轮 planner/worker 都会构造 vars；若每个 intent 都打一次 OKX，
// 一个任务几十个意图就会撞限频（OKX 的 account/positions 是 10 次/2s 量级）。
// 缓存 10 秒对「该不该平仓」这个决策完全够用 —— 持仓不会在 10 秒内
// 变成另一笔仓。
//
// # 为什么读失败不缓存成「空持仓」
//
// 空持仓会让 trader 认为「没有仓可以平」，从而不发 exit。读失败必须
// 作为错误上抛，prompt 里会明确写「读不到 ≠ 没有」。
type okxPositionsProvider struct {
	db    *db.DB
	creds func() (*db.OkxTradeCred, error)

	mu       sync.Mutex
	cachedAt time.Time
	cached   []agent.Position
	lastErr  error
	ttl      time.Duration
	timeout  time.Duration
}

func newOkxPositionsProvider(d *db.DB, creds func() (*db.OkxTradeCred, error)) *okxPositionsProvider {
	return &okxPositionsProvider{db: d, creds: creds, ttl: 10 * time.Second, timeout: 5 * time.Second}
}

func (p *okxPositionsProvider) Positions(ctx context.Context) ([]agent.Position, error) {
	if p == nil || p.db == nil || p.creds == nil {
		return nil, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.cachedAt.IsZero() && time.Since(p.cachedAt) < p.ttl {
		return p.cached, p.lastErr
	}
	cred, err := p.creds()
	if err != nil {
		p.cachedAt, p.cached, p.lastErr = time.Now(), nil, err
		return nil, err
	}
	client := okxrest.NewClient(okxrest.Credentials{
		APIKey: cred.APIKey, SecretKey: cred.SecretKey, Passphrase: cred.Passphrase,
		Demo: cred.Demo, BaseURL: cred.BaseURL,
	})
	cctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	raw, err := client.GetPositions(cctx, "SWAP")
	if err != nil {
		p.cachedAt, p.cached, p.lastErr = time.Now(), nil, err
		return nil, err
	}
	out := make([]agent.Position, 0, len(raw))
	for _, r := range raw {
		out = append(out, agent.Position{
			InstID: r.InstID, PosSide: r.PosSide, Pos: r.Pos,
			AvgPx: r.AvgPx, Upl: r.Upl, Lever: r.Lever, MgnMode: r.MgnMode,
		})
	}
	p.cachedAt, p.cached, p.lastErr = time.Now(), out, nil
	return out, nil
}

// positionsProvider 是 Engine 上懒加载的单例：缓存必须跨 intent 共享，
// 每次新建 provider 等于没有缓存。
func (e *Engine) positionsProvider() agent.PositionsProvider {
	e.posProviderOnce.Do(func() {
		var creds func() (*db.OkxTradeCred, error)
		if e.m != nil && e.m.pg != nil {
			creds = e.m.pg.ActiveOkxTradeCredential
		}
		e.posProvider = newOkxPositionsProvider(e.m.pg, creds)
	})
	return e.posProvider
}
