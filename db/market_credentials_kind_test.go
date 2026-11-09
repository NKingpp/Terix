package db

// market_credentials 的 kind 语义测试。
//
// # 为什么这些测试必须存在
//
// 加 kind 的核心目的是「同一个 provider 可以有两条用途不同的凭据」。
// 而这件事依赖三处配合，任何一处漏掉都会静默出错：
//  ① DDL 的唯一约束是 (provider, kind, account_label) —— 少 kind 会撞约束
//  ② Upsert 的 ON CONFLICT 也要带 kind —— 少了会把第二条覆盖掉第一条
//  ③ Update 必须拒绝改 kind —— 否则字段布局会错配
//
// ② 是最隐蔽的：约束允许两条，但 ON CONFLICT 不带 kind 会**静默覆盖**
// （因为 PostgreSQL 找不到冲突目标会报错，但如果写成 (provider, account_label)
// 就变成「第二条覆盖第一条」，不报错）。

import (
	"errors"
	"testing"
)

// cleanCredentials 清掉**本测试文件造的**凭据，保证测试间互不影响。
//
// # 为什么不 `DELETE FROM market_credentials`（2026-11-05 事故）
//
// 这张表存用户的真实交易密钥。全表删除会让用户失去 OKX/复星凭据，
// 且没有任何提示 —— 用户只看到「开发完凭据没了」。
//
// 事故现场：跑完 `go test ./...` 后 market_credentials 变成 0 行。
//
// # 为什么按 label 前缀而不是精确 label
//
// 本文件各测试用了 "主账户" / "acc" 等散落的 label，「主账户」这类值
// 与真实用户可能重名。加测试专用前缀后，与真实数据永久隔离。
//
// ⚠️ 新增测试时 label 必须以 testKindPrefix 开头，否则会被其他测试清掉。
const testKindPrefix = "kittest-"

func cleanCredentials(t *testing.T, d *DB) {
	t.Helper()
	if _, err := d.Exec(`DELETE FROM market_credentials WHERE account_label LIKE $1`,
		testKindPrefix+"%"); err != nil {
		t.Fatalf("清理 market_credentials: %v", err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM market_credentials WHERE account_label LIKE $1`,
			testKindPrefix+"%")
	})
}

// TestCredentialSameLabelDifferentKind 验证同一 provider 同 label 的两个 kind 能共存。
//
// 这是加 kind 的核心价值：OKX 的「主账户」既要有交易凭据（下单）
// 也要有行情凭据（提高限频配额），两者是不同的密钥。
func TestCredentialSameLabelDifferentKind(t *testing.T) {
	d := testOpen(t)
	cleanCredentials(t, d)

	trade, err := d.UpsertMarketCredential(MarketCredentialInput{
		Provider: "okx", Kind: "trade", AccountLabel: testKindPrefix + "主账户",
		APIKey: "k-trade", APISecret: "s-trade", Passphrase: "p-trade",
	})
	if err != nil {
		t.Fatalf("建 trade 凭据: %v", err)
	}
	data, err := d.UpsertMarketCredential(MarketCredentialInput{
		Provider: "okx", Kind: "data", AccountLabel: testKindPrefix + "主账户",
		APIKey: "k-data",
	})
	if err != nil {
		t.Fatalf("建 data 凭据（同 provider 同 label）: %v", err)
	}

	if trade.ID == data.ID {
		t.Fatalf("两个 kind 落到了同一行（id=%d）—— Upsert 的定位键没带 kind", trade.ID)
	}
	if trade.Kind != "trade" || data.Kind != "data" {
		t.Fatalf("kind 读回不符: trade=%q data=%q", trade.Kind, data.Kind)
	}

	// 只数本测试的 label（表里还有用户真实凭据，那是正常数据）。
	var n int
	if err := d.QueryRow(
		`SELECT count(*) FROM market_credentials WHERE account_label LIKE $1`,
		testKindPrefix+"%").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("本测试的凭据应有 2 条（同 label 不同 kind），实际 %d 条", n)
	}
}

// TestCredentialUpsertSameKindOverwrites 验证「同 kind 同 label」是覆盖而不是新增。
func TestCredentialUpsertSameKindOverwrites(t *testing.T) {
	d := testOpen(t)
	cleanCredentials(t, d)

	first, err := d.UpsertMarketCredential(MarketCredentialInput{
		Provider: "okx", Kind: "trade", AccountLabel: testKindPrefix + "acc", APIKey: "k1",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := d.UpsertMarketCredential(MarketCredentialInput{
		Provider: "okx", Kind: "trade", AccountLabel: testKindPrefix + "acc", APIKey: "k2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("同 kind 同 label 应覆盖同一行：第一次 id=%d，第二次 id=%d", first.ID, second.ID)
	}
	// 只数**本测试的** label —— 表里还有用户的真实凭据，那是正常数据。
	//
	// 原来是 `SELECT count(*) FROM market_credentials` 并要求 =1，
	// 隐含假设「表是空的」。而这张表本就该有用户的生产密钥，
	// 那个断言只有在「测试可以清空全表」的前提下才成立 —— 而那正是要禁止的事。
	var n int
	if err := d.QueryRow(
		`SELECT count(*) FROM market_credentials WHERE account_label LIKE $1`,
		testKindPrefix+"%").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("本测试的凭据应只有 1 条（同 kind 同 label 应覆盖），实际 %d 条", n)
	}
}

// TestCredentialKindDefaultsToTrade 验证空串 kind 归一为 trade（兼容既有调用方）。
func TestCredentialKindDefaultsToTrade(t *testing.T) {
	d := testOpen(t)
	cleanCredentials(t, d)

	c, err := d.UpsertMarketCredential(MarketCredentialInput{
		Provider: "fosun", AccountLabel: testKindPrefix + "acc", // Kind 留空
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind != "trade" {
		t.Fatalf("Kind 应为 trade（空串归一），实际 %q", c.Kind)
	}
}

// TestCredentialRejectsInvalidKind 验证非法 kind 被拒 —— 而不是写进库后被 CHECK 拦成 500。
func TestCredentialRejectsInvalidKind(t *testing.T) {
	d := testOpen(t)
	cleanCredentials(t, d)

	_, err := d.UpsertMarketCredential(MarketCredentialInput{
		Provider: "okx", Kind: "bogus", AccountLabel: testKindPrefix + "acc",
	})
	if err == nil {
		t.Fatal("非法 kind 应报错")
	}
}

// TestUpdateRejectsKindChange 验证按 id 更新时不能改 kind。
func TestUpdateRejectsKindChange(t *testing.T) {
	d := testOpen(t)
	cleanCredentials(t, d)

	c, err := d.UpsertMarketCredential(MarketCredentialInput{
		Provider: "okx", Kind: "trade", AccountLabel: testKindPrefix + "acc", APIKey: "k",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = d.UpdateMarketCredentialByID(c.ID, MarketCredentialInput{
		Provider: "okx", Kind: "data", AccountLabel: testKindPrefix + "acc",
	})
	if !errors.Is(err, ErrCredentialKindImmutable) {
		t.Fatalf("改 kind 应返回 ErrCredentialKindImmutable，实际: %v", err)
	}
}

// TestUpdateAllowsEmptyKind 验证不传 kind 时正常更新（前端编辑表单不需要重传 kind）。
func TestUpdateAllowsEmptyKind(t *testing.T) {
	d := testOpen(t)
	cleanCredentials(t, d)

	c, err := d.UpsertMarketCredential(MarketCredentialInput{
		Provider: "okx", Kind: "data", AccountLabel: testKindPrefix + "old", APIKey: "k",
	})
	if err != nil {
		t.Fatal(err)
	}

	updated, err := d.UpdateMarketCredentialByID(c.ID, MarketCredentialInput{
		Provider: "okx", AccountLabel: testKindPrefix + "new", // Kind 留空 + 改 label
	})
	if err != nil {
		t.Fatalf("不传 kind 的更新应成功: %v", err)
	}
	if updated.Kind != "data" {
		t.Fatalf("kind 应保持 data，实际 %q", updated.Kind)
	}
	// 断言与写入值一致（写入的是 testKindPrefix+"new"，前缀用于隔离清理）。
	if want := testKindPrefix + "new"; updated.AccountLabel != want {
		t.Fatalf("label 应改成 %q，实际 %q", want, updated.AccountLabel)
	}
}
