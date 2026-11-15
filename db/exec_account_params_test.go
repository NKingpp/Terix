package db

import "testing"

// 无账户参数记录时，默认杠杆必须是 1 倍。
//
// # 为什么钉住这条
//
// 2026-11-15：任务 34 目标写明「使用 10x 杠杆」，但模型没传 leverage
// 参数（当时描述写的是「可选」），于是落到账户默认值 3。
// 台账记 leverage=3，与目标不符。
//
// 把默认值设成 1 是最保守的兜底：**不给杠杆就不放大敞口**。
// 要放大必须是显式动作（开仓传 leverage 或改账户参数）。
func TestExecAccountParamsDefaultLeverIsOne(t *testing.T) {
	d := testOpen(t)

	p, err := d.GetExecAccountParams(999999) // 不存在的账户 → 走默认值分支
	if err != nil {
		t.Fatalf("读默认参数失败: %v", err)
	}
	if p.Lever != "1" {
		t.Errorf("默认杠杆 = %q，want \"1\" —— 默认值不该放大敞口", p.Lever)
	}
	if p.PosMode != "net" || p.MgnMode != "cross" {
		t.Errorf("默认模式 = %s/%s，want net/cross", p.PosMode, p.MgnMode)
	}
}

// Upsert 时 lever 为空也应该是 1，而不是写入空串或旧默认值。
func TestUpsertExecAccountParamsEmptyLeverDefaultsToOne(t *testing.T) {
	d := testOpen(t)

	res, err := d.Exec(`INSERT INTO exec_accounts(channel, channel_account_id, label, market, active)
VALUES('okx','lever-default-test','杠杆默认测试','crypto',1)`)
	if err != nil {
		t.Fatalf("建账户: %v", err)
	}
	id, _ := res.LastInsertId()
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM exec_accounts WHERE id=$1`, id) })

	if err := d.UpsertExecAccountParams(ExecAccountParams{AccountID: id}); err != nil {
		t.Fatalf("写参数: %v", err)
	}
	p, err := d.GetExecAccountParams(id)
	if err != nil {
		t.Fatalf("读参数: %v", err)
	}
	if p.Lever != "1" {
		t.Errorf("lever 为空时默认 = %q，want \"1\"", p.Lever)
	}
}
