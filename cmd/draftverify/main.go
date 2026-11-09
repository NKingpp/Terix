package main

// 手动验证 live_auto 的自动下单链路：建草案 → 过闸 → 等 AutoTrader 执行。
//
// # 为什么要这个程序
//
// 阶段图的编排时序还有问题（debate 早于 analyst fact 落库，导致裁决恒为
// inconclusive → trader 不建草案）。但那**不是下单链路的问题**。
//
// 这个程序绕开编排，直接走 trader 会走的同一条路：
//
//	InsertDraft  → Gate.Authorize  → （AutoTrader 轮询捡到）→ vaultd → OKX
//
// 如果这条跑通了，说明「下单链路」本身是好的，剩余问题全在编排。

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/Autumn-27/artex/config"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/exec"
)

func main() {
	path, _, err := config.DBPath()
	if err != nil {
		fmt.Println("解析库路径:", err)
		return
	}
	d, err := db.Open(path)
	if err != nil {
		fmt.Println("open:", err)
		return
	}
	defer d.Close()

	ctx := context.Background()

	// 参数：symbol market side qty  （qty 是币量；用 quote_ccy 时是金额）
	if len(os.Args) < 5 {
		fmt.Println("用法: draft-verify <symbol> <market> <side> <qty>")
		return
	}
	symbol, market, side, qty := os.Args[1], os.Args[2], os.Args[3], os.Args[4]

	// 1) 找标的
	inst, err := d.Instruments().GetInstrumentByMarketSymbol(market, symbol)
	if err != nil || inst == nil {
		fmt.Printf("标的 %s/%s 不存在，先登记\n", market, symbol)
		ac := "stock"
		if market == "crypto" {
			ac = "crypto"
		}
		id, _, uErr := d.Instruments().UpsertInstrument(db.InstrumentInput{
			Symbol: symbol, Name: symbol, Market: market, AssetClass: ac, Currency: "USDT",
		})
		if uErr != nil {
			fmt.Println("登记标的失败:", uErr)
			return
		}
		inst, _ = d.Instruments().GetInstrumentByID(id)
	}
	fmt.Printf("标的: id=%d %s\n", inst.ID, inst.Symbol)

	// 2) 参考价：取最新快照里的收盘价，没有就用固定值
	refPx := "78000"
	if sym, ok := queryLastPrice(d, inst.Symbol); ok {
		refPx = sym
	}
	fmt.Printf("参考价: %s\n", refPx)

	// 3) 建草案
	in := db.OrderDraftInput{
		InstrumentID: &inst.ID,
		Side:         side,
		Qty:          qty,
		RefPrice:     refPx,
	}
	draftID, err := d.OrderDrafts().InsertDraft(in)
	if err != nil {
		fmt.Println("建草案失败:", err)
		return
	}
	fmt.Printf("草案已建: id=%d\n", draftID)

	// 4) 过闸
	mode, _ := d.GetExecMode()
	fmt.Printf("执行模式: %s\n", mode)
	acct, err := firstActiveAccount(d)
	if err != nil {
		fmt.Println("找账户失败:", err)
		return
	}
	fmt.Printf("账户: id=%d ref=%s\n", acct.ID, acct.ChannelAcctID)

	gate := exec.NewGate(d, nil)
	res, err := gate.Authorize(ctx, exec.GateRequest{
		DraftID: draftID, AccountID: acct.ID, Mode: mode,
	})
	if err != nil {
		fmt.Println("过闸出错:", err)
		return
	}
	fmt.Printf("闸门判定: %s | %s\n", res.Decision, res.Reason)
	if string(res.Decision) != "allow" {
		fmt.Println("未放行，停止")
		return
	}

	fmt.Println("\n草案已放行。AutoTrader 会在下一轮轮询（≤60s）捡到它并下单。")
	fmt.Println("观察: docker exec artex-pg psql -U artex -d artex -c \"select * from orders_execution order by id desc limit 3;\"")
	_ = strconv.Itoa(int(draftID))
	_ = time.Second
}

func firstActiveAccount(d *db.DB) (*db.ExecAccount, error) {
	accts, err := d.ExecAccounts().ListAccounts()
	if err != nil {
		return nil, err
	}
	for _, a := range accts {
		if a.Active {
			return a, nil
		}
	}
	return nil, fmt.Errorf("没有 active 账户")
}

// queryLastPrice 取标的的最新收盘价（没有就返回 false）。
//
// # 为什么改查 bars
//
// 原实现查 `snapshots.body->'last'`，但 snapshots 表**没有 body 列**
// （PG 与 SQLite 都只有 body_ref），所以那条 SQL 一直报错、永远回退到固定价 ——
// 静默失效。既然 server/rating_draft.go 的参考价来源就是 bars.close，
// 这里保持一致，不再依赖一个不存在的列。
func queryLastPrice(d *db.DB, symbol string) (string, bool) {
	var px string
	err := d.QueryRow(`
SELECT COALESCE(close, '')
FROM bars
WHERE instrument_id = (SELECT id FROM instruments WHERE symbol = $1 ORDER BY id LIMIT 1)
  AND close IS NOT NULL
ORDER BY ts DESC LIMIT 1`, symbol).Scan(&px)
	if err != nil || px == "" {
		return "", false
	}
	return px, true
}
