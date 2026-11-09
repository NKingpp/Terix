package db

// 「测试删除了真实数据」检测的验收测试（2026-11-05）。
//
// # 事故背景
//
// 用户在多次开发后反馈「llm 和交易凭据都没了」。核查发现
// 三个测试文件里有不带 WHERE 的 `DELETE FROM market_credentials`：
//
//	db/market_credentials_cols_test.go:25
//	db/market_credentials_kind_test.go:25（cleanCredentials）
//	server/market_credentials_kind_api_test.go:25（credentialCleanup）
//
// 这三处会把用户的真实 OKX / 复星密钥删光，且**没有任何日志**。
//
// 而原有的残留守卫只比对总行数 —— 测试「删 2 行真实凭据 + 插 2 行假凭据」
// 净变化为 0，守卫完全看不见。所以这里加 findDecreased，逐表比对。

import "testing"

// TestFindDecreasedDetectsLoss 行数减少必须被发现。
func TestFindDecreasedDetectsLoss(t *testing.T) {
	before := ResidueCounts{ExecAccounts: 5, Signals: 3, Ratings: 2}
	after := ResidueCounts{ExecAccounts: 3, Signals: 3, Ratings: 2}

	has, detail := findDecreased(before, after)
	if !has {
		t.Fatal("exec_accounts 从 5 减到 3，findDecreased 应返回 true")
	}
	if detail == "" {
		t.Error("描述不能为空 —— 否则用户不知道该查哪张表")
	}
	// 描述里必须含表名与两个数字，用户才能定位。
	for _, want := range []string{"exec_accounts", "5", "3"} {
		if !containsStr(detail, want) {
			t.Errorf("描述缺 %q：%s", want, detail)
		}
	}
}

// TestFindDecreasedIgnoresGrowth 行数增加不是「删除」，不该被这条规则判失败。
//
// 增加由原有的残留检查负责，两者是不同的病，提示也应不同。
func TestFindDecreasedIgnoresGrowth(t *testing.T) {
	before := ResidueCounts{Tasks: 10}
	after := ResidueCounts{Tasks: 12}

	has, _ := findDecreased(before, after)
	if has {
		t.Error("行数增加不该被 findDecreased 判为删除")
	}
}

// TestFindDecreasedDetectsZeroing 清空表是最严重的情形。
func TestFindDecreasedDetectsZeroing(t *testing.T) {
	before := ResidueCounts{ExecAccounts: 2}
	after := ResidueCounts{ExecAccounts: 0}

	has, detail := findDecreased(before, after)
	if !has {
		t.Fatal("表被清空必须被发现")
	}
	if detail == "" {
		t.Error("描述不能为空")
	}
}

// TestFindDecreasedNoChange 完全没变化时不该报警。
func TestFindDecreasedNoChange(t *testing.T) {
	c := ResidueCounts{Tasks: 1, ExecAccounts: 1}
	if has, _ := findDecreased(c, c); has {
		t.Error("无变化时不该报警")
	}
}
