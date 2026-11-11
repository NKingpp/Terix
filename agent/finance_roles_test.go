package agent

// finance_roles 的验收测试。
//
// # 这组测试守的是什么
//
// planner 能不能给意图指定 role，取决于「合法角色清单」与「有 prompt 模板」是否
// 一致。两者一旦漂移，后果是**静默**的：
//
//   - 清单里多一个没模板的 role → planner 派了它，renderRolePrompt 返回 false，
//     节点静默退回通用 worker，跑错身份且事后无从判断；
//   - 清单里少一个真角色 → 新加的角色 planner 永远派不到，只能等建图时才有。
//
// 所以这里做**双向**校验，任何一侧增删而忘了另一侧就立刻红。

import (
	"strings"
	"testing"
)

// TestFinancialRoleKeysMatchPromptSeeds 双向锁住「可指派」与「有模板」不漂移。
//
// 这是本设计的唯一不变量：IsFinancialRole 为 true 的 key，renderRolePrompt
// 必须能成功。
func TestFinancialRoleKeysMatchPromptSeeds(t *testing.T) {
	seeds := BuiltinPromptSeeds()
	keys := FinancialRoleKeys()

	if len(keys) == 0 {
		t.Fatal("FinancialRoleKeys() 为空 —— planner 将无法指派任何角色")
	}

	// ① 每个可指派角色都必须有 prompt 模板且被识别为金融角色
	for _, k := range keys {
		if seeds[k] == "" {
			t.Errorf("%q 在 FinancialRoleKeys 里但没有 prompt 模板 —— renderRolePrompt 会退回 worker", k)
		}
		if !isFinancialPrompt(k) {
			t.Errorf("%q 必须被 isFinancialPrompt 识别（否则会拿到 Bash/Write 全套工具）", k)
		}
	}

	// ② 反过来：BuiltinPromptSeeds 里所有金融角色都必须在清单里
	//    （否则新加的角色 planner 永远派不到）
	in := make(map[string]bool, len(keys))
	for _, k := range keys {
		in[k] = true
	}
	for k := range seeds {
		if isFinancialPrompt(k) && !in[k] {
			t.Errorf("%q 是金融角色但不在 FinancialRoleKeys 里 —— planner 派不到它", k)
		}
	}
}

// TestFinancialRoleKeysExcludesUnregisteredSentiment 股票版情绪分析师没有数据源、
// 未注册 prompt，绝不能进 enum —— 派了它只会产出 unavailable。
//
// 这条同时证明「不能用 isFinancialPrompt 的前缀判断当白名单」：
// analyst_sentiment 命中 analyst_ 前缀，isFinancialPrompt 返回 true，但它没有模板。
func TestFinancialRoleKeysExcludesUnregisteredSentiment(t *testing.T) {
	if IsFinancialRole(RoleAnalystSentiment) {
		t.Error("analyst_sentiment 无可靠数据源、未注册 prompt，不该可指派")
	}
	// 反证：它确实会被前缀判断识别，说明前缀不能替代模板检查
	if !isFinancialPrompt(RoleAnalystSentiment) {
		t.Log("注：isFinancialPrompt 已不再识别 analyst_sentiment —— 前缀规则变了，注释该更新")
	}
}

// TestFinancialRoleKeysOrderIsBusinessFlow 顺序是业务流，不是字母序。
//
// 清单会直接渲染进 planner prompt，顺序混乱会让人（和模型）看不出阶段先后。
func TestFinancialRoleKeysOrderIsBusinessFlow(t *testing.T) {
	keys := FinancialRoleKeys()
	if keys[0] != RoleScreener {
		t.Errorf("首个角色应为 screener（它是唯一能定标的的角色），实际 %q", keys[0])
	}
	if keys[len(keys)-1] != RolePortfolioManager {
		t.Errorf("末个角色应为 portfolio_manager（流程终点），实际 %q", keys[len(keys)-1])
	}
	// 交易员必须排在风控之前：先有提案才有可风控的对象
	idx := func(r string) int {
		for i, k := range keys {
			if k == r {
				return i
			}
		}
		return -1
	}
	if idx(RoleTrader) > idx(RoleRiskNeutral) {
		t.Error("trader 必须排在风控角色之前 —— 风控评审的是 trader 的提案")
	}
}

// TestFinancialRoleKeysReturnsCopy 调用方改返回值不能污染内部状态。
func TestFinancialRoleKeysReturnsCopy(t *testing.T) {
	a := FinancialRoleKeys()
	if len(a) == 0 {
		t.Fatal("清单为空")
	}
	orig := a[0]
	a[0] = "被调用方改掉了"
	if b := FinancialRoleKeys(); b[0] != orig {
		t.Error("FinancialRoleKeys 返回了内部切片 —— 调用方的修改泄漏进全局状态")
	}
}

// TestExpectedStageRolesDedupes 多轮辩论里 bull/bear 会出现两次，缺失检测要的是去重集合。
func TestExpectedStageRolesDedupes(t *testing.T) {
	roles := ExpectedStageRoles(AssetClassStock)
	if len(roles) == 0 {
		t.Fatal("stock 阶段图应有执行角色")
	}
	seen := map[string]bool{}
	for _, r := range roles {
		if r == "" {
			t.Error("不该出现空 role")
		}
		if seen[r] {
			t.Errorf("%q 重复出现 —— ExpectedStageRoles 必须去重", r)
		}
		seen[r] = true
	}
	// 关键角色必须在（这几个是阶段主轴）
	for _, want := range []string{RoleAnalystMarket, RoleTrader, RolePortfolioManager} {
		if !seen[want] {
			t.Errorf("stock 阶段图应含 %q，实际: %v", want, roles)
		}
	}
	// 不含 screener：那由「任务是否绑标的」判断，不靠这张表
	if seen[RoleScreener] {
		t.Error("ExpectedStageRoles 不该含 screener（InstrumentID>0 时不展开选品阶段）")
	}
}

// TestExpectedStageRolesCryptoUsesSentiment crypto 用加密情绪分析师，不用股票版。
func TestExpectedStageRolesCryptoUsesSentiment(t *testing.T) {
	roles := ExpectedStageRoles(AssetClassCrypto)
	if len(roles) == 0 {
		t.Fatal("crypto 阶段图应有执行角色")
	}
	seen := map[string]bool{}
	for _, r := range roles {
		seen[r] = true
	}
	if !seen[RoleAnalystSentimentCrypto] {
		t.Errorf("crypto 阶段图应含 analyst_sentiment_crypto，实际: %v", roles)
	}
	if seen[RoleAnalystNews] {
		t.Log("注：crypto 不再包含 analyst_news —— 若这是刻意的（无加密新闻源），注释该更新")
	}
}

// TestExpectedStageRolesUnknownAssetClass 未知资产类别返回 nil，不报假缺失。
func TestExpectedStageRolesUnknownAssetClass(t *testing.T) {
	if roles := ExpectedStageRoles("future"); roles != nil {
		t.Errorf("未知资产类别应返回 nil（表示「没有期望」），实际 %v", roles)
	}
}

// TestIsFinancialRole 边界：空串、前缀、大小写。
func TestIsFinancialRole(t *testing.T) {
	cases := map[string]bool{
		RoleTrader:        true,
		RoleScreener:      true,
		"":                false,
		"analyst_":        false, // 前缀本身不是角色
		"analysts_market": false, // 常见拼写错误
		"TRADER":          false, // 大小写敏感 —— role 值原样进 payload
		"worker":          false, // 通用执行者不是「可指派角色」
	}
	for in, want := range cases {
		if got := IsFinancialRole(in); got != want {
			t.Errorf("IsFinancialRole(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestFinancialRoleKeysJoinable 清单能直接拼进错误文案（模型据此自纠）。
func TestFinancialRoleKeysJoinable(t *testing.T) {
	joined := strings.Join(FinancialRoleKeys(), "、")
	if strings.Contains(joined, "analyst_sentiment、") {
		t.Error("错误文案里不该出现不可指派的 analyst_sentiment")
	}
	if !strings.Contains(joined, RoleTrader) {
		t.Error("清单应含 trader")
	}
}
