package agent

// 快照接线的端到端验证：真调工具 → 快照真落库 → snapshot_id 真返回。
//
// # 为什么需要这个测试
//
// snapshot_capture_test.go 的 11 个测试验证的是「捕获函数本身对」，
// 但验证不了「工具真的调了捕获函数」。
//
// 这个测试走完整链路：
//   get_quote(defer) → MarketClient → captureQuoteSnapshot → snapshots 表
// 并断言返回的 snapshot_id 能在库里查到。
//
// 依赖 sidecar（akshare）与 PG。任一不可用则 skip。

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// TestGetQuoteActuallyCapturesSnapshot 验证 getQuote 真的落快照。
func TestGetQuoteActuallyCapturesSnapshot(t *testing.T) {
	if os.Getenv("AKSHARE_SIDECAR_ADDR") == "" {
		// 默认地址也试一次，但 sidecar 未跑时 skip 而不是 fail
		t.Log("未设 AKSHARE_SIDECAR_ADDR，用默认 127.0.0.1:8791")
	}

	d := testDB(t)
	cli := NewAkshareClient("")
	if ok, msg, err := cli.Health(context.Background()); ok == false || err != nil {
		t.Skipf("sidecar 不可用（%v / %s）—— 跳过真实取数验证", err, msg)
	}

	mts := NewMarketToolSet(cli)
	mts.SetDB(d)

	// 记快照 id 上界，cleanup 时删掉超过它的一切。
	//
	// 为什么用 id 上界而不是「按 source/symbol 删」：
	//   params_hash 的去重逻辑意味着同一份数据可能命中已存在的快照
	//   （返回旧 id），此时「按 source 删」会误删别人的数据。
	//   而 id 上界只删本测试之后产生的，不会碰到历史记录。
	var snapMax int64
	if err := d.QueryRow(`SELECT COALESCE(max(id),0) FROM snapshots`).Scan(&snapMax); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := d.Exec(`DELETE FROM snapshots WHERE id > $1`, snapMax); err != nil {
			t.Logf("清理快照失败（残留守卫会报）: %v", err)
		}
	})

	tool := mts.getQuote()
	before := countSnapshots(t, d)

	raw, err := tool.Call(context.Background(), []byte(`{"symbol":"QQQ","market":"us"}`), nil)
	if err != nil {
		t.Fatalf("get_quote 调用失败: %v", err)
	}

	var out map[string]any
	for _, cb := range raw.Content {
		if err := json.Unmarshal([]byte(cb.Text), &out); err != nil {
			t.Fatalf("解析 get_quote 返回失败: %v\n原文: %s", err, cb.Text)
		}
		break
	}

	// 关键断言 1：返回里有 snapshot_id
	snapVal, ok := out["snapshot_id"]
	if !ok {
		// 可能是 is_partial（未收盘）→ 有 snapshot_absent_reason 是合法的
		if reason, hasReason := out["snapshot_absent_reason"]; hasReason {
			t.Skipf("本次数据未收盘，未落快照（合法）：reason=%v", reason)
		}
		t.Fatalf("既没有 snapshot_id 也没有 snapshot_absent_reason —— 接线没生效\n返回: %v", out)
	}

	snapID := int64(0)
	switch v := snapVal.(type) {
	case float64:
		snapID = int64(v)
	case string:
		t.Fatalf("snapshot_id 是字符串 %q，应为数字", v)
	}
	if snapID <= 0 {
		t.Fatalf("snapshot_id=%d，应为正数", snapID)
	}
	t.Logf("✓ get_quote 返回 snapshot_id=%d", snapID)

	// 关键断言 2：这条快照真在库里
	var found bool
	if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM snapshots WHERE id=$1)`, snapID).Scan(&found); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatalf("snapshot_id=%d 在 snapshots 表里查不到 —— 返回了不存在的 id", snapID)
	}
	t.Logf("✓ snapshots 表里确有 id=%d 的记录", snapID)

	// 关键断言 3：快照确实存在（这是「真的写入了」的判据）。
	//
	// # 为什么不再断言「数量增加」
	//
	// snapshot_capture 用 ON CONFLICT DO NOTHING 去重（同标的同秒同参数只留一条，
	// 见该文件 L3 已知限制）。同一台机器上重复跑测试时，第二次会**复用**
	// 上一条快照 —— 数量不变但行为完全正确。
	//
	// 原断言 `after <= before` 会因此误报。断言 2 已经证明「id 在库里存在」，
	// 那才是「写入成功」的直接证据；计数只反映「本次是否新建了行」，
	// 而新建与否取决于去重键，与功能正确性无关。
	//
	// 保留 before/after 的读取并在日志里打出，便于排查「为什么没新建」。
	after := countSnapshots(t, d)
	t.Logf("✓ 快照计数 before=%d after=%d（未增加属正常：去重键命中）", before, after)

	// 关键断言 4：旧字段都还在（验证「只加不改」）
	for _, k := range []string{"symbol", "market", "last", "is_partial", "channel"} {
		if _, ok := out[k]; !ok {
			t.Errorf("旧字段 %q 丢失 —— snapshot_id 应该是新增而非替换", k)
		}
	}
	t.Logf("✓ 所有旧字段保留")
}

func countSnapshots(t *testing.T, d *db.DB) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT count(*) FROM snapshots`).Scan(&n); err != nil {
		t.Fatalf("count snapshots: %v", err)
	}
	return n
}
