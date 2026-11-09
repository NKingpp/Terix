package db

// 闸门依赖的 5 条内置草案规则的 seed（WE-8）。
//
// # 为什么这些规则必须在代码里而不是 schema.sql 里
//
// 它们原先只存在于开发库（手工 psql 插入），代码里零种子逻辑。
// 结果：新环境即使表建全了，`intercept_rules` 也是空的 ——
// 闸门的所有 matcher 读不到规则，**每一张草案都被 EXEC_GATE_UNAVAILABLE 拒绝**。
//
// 这和「18 张表不在 schema.sql 里」是同一类缺失，但更隐蔽：
// 表在、查询不报错，只是返回空。运维看到全拒会以为「风控很严」，
// 实际是「规则一条都没有」。
//
// # 为什么不用 ON CONFLICT
//
// `intercept_rules.name` 没有 UNIQUE 约束（只有 pkey），
// 所以 `ON CONFLICT (name)` 会直接报 SQL 错误。
//
// 而且即使加了 UNIQUE 也有个更深的问题：用户可能创建同名规则
// （他不懂「内置」这个概念），那时 UNIQUE 会阻止用户的合法操作。
//
// 所以用 `WHERE NOT EXISTS`：name 精确匹配，不存在才插入。
//
// # 已知限制：改过名的内置规则会被重新插入
//
// 用户把 `[内置] 草案-单笔名义金额上限` 改名后，下次启动会重新插入一条原名规则，
// 于是有两条相似的。这个代价比加 UNIQUE 小 —— 改名内置规则是罕见操作，
// 且改名之后行为本来就变了，需要人工检查。
//
// # 为什么 5 条规则里只有 2 条是 ask
//
// deny = 「无条件拒绝，无人可绕过」。ask = 「需要人工确认」。
//
// 只有「判据本身是近似的、或阈值是经验值」的规则才用 ask：
//
//   - 日内下单次数：数草案条数即可（判据可靠），但「20 笔」是**经验阈值**
//   - 单标的集中度：**判据不可靠** —— 它只数草案，系统未读取真实持仓
//     （规则参数里的 disclosure 字段就是为此存在的）
//
// 其余 3 条不可 ask：
//
//   - 单笔名义金额上限：超过就是超过，没有讨论空间
//   - 标的允许名单：不在名单里就是不允许交易
//   - 交易时段：非交易时段下单必然失败，让人确认毫无意义
//
// ⚠️ **这个分配是设计决定**。改它之前先想清楚「这条规则的判据可靠吗」——
// 对不可靠的判据用 deny 会误杀（用户真实情况可能完全不同），
// 对可靠的判据用 ask 会让「确认」变成机械点击（点多了就没人看了）。
//
// # 为什么这段注释必须存在
//
// 当前 DB 里的 action 与代码曾经不一致（DB=ask、代码=deny），而 seed 策略是
// 「已存在就不覆盖」—— 所以代码改了、DB 是旧值时只有一条 slog.Warn，没有测试会红。
//
// 而「ask vs deny」的区别是「人工确认 vs 无人可绕过」，那是**安全语义**。
// 不一致不该静默存在 —— 由 TestBuiltinDraftRulesActionMatchesDB 守护。
//
// 更危险的是：如果有人看到代码是 ask、以为是笔误而改回 deny，
// 那「为什么是 ask」的推理就丢了。所以理由写在规则本身的注释里。
//
// # 不覆盖用户修改
//
// 用 WHERE NOT EXISTS 而不是 DO UPDATE：用户可能把上限从 10000 改成自己想要的，
// 每次启动重置掉是错的。
//
// 代价是「内置规则升级不生效」（比如默认上限从 10000 改成 8000，
// 已部署环境不会更新）。所以 seed 之后做一次指纹比对，不一致就打 warn ——
// 不自动修，但让用户知道他的规则与当前版本不同。

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
)

// builtinDraftRule 是一条内置草案规则的定义。
type builtinDraftRule struct {
	Name      string
	Priority  int
	MatchType string
	// PatternParams 是结构化 matcher 的参数（JSON 对象）。
	// string/regex 类规则用不到，这里 5 条都是结构化 matcher。
	PatternParams string
	Action        string
	Message       string
}

// builtinDraftRules 是闸门依赖的全部内置草案规则。
//
// 顺序与 priority 一致（高的在前），但插入顺序不影响行为 ——
// matcher 按 priority DESC 求值。
var builtinDraftRules = []builtinDraftRule{
	{
		Name:          "[内置] 草案-单笔名义金额上限",
		Priority:      100,
		MatchType:     "numeric_lte",
		PatternParams: `{"max":"10000","field":"notional"}`,
		Action:        "deny",
		Message:       "单笔名义金额超过上限",
	},
	{
		Name:      "[内置] 草案-标的允许名单",
		Priority:  90,
		MatchType: "list",
		// 默认名单只含最主流的 4 个标的。用户要放宽必须自己改这条规则 ——
		// 默认严格比默认宽松安全（漏放一个单比误拦一个单危险得多）。
		PatternParams: `{"mode":"include","field":"symbol","values":["SPY","QQQ","AAPL","BTC-USDT"]}`,
		Action:        "deny",
		Message:       "标的不在允许名单内",
	},
	{
		Name:          "[内置] 草案-交易时段",
		Priority:      80,
		MatchType:     "time_window",
		PatternParams: `{"source":"market_session"}`,
		Action:        "deny",
		Message:       "当前不在交易时段",
	},
	{
		Name:          "[内置] 草案-日内下单次数",
		Priority:      70,
		MatchType:     "daily_count",
		PatternParams: `{"max":"20","field":"symbol","scope":"instrument"}`,
		// ask 而非 deny —— 理由见包级注释「为什么 5 条里只有 2 条是 ask」。
		//
		// 判据本身可靠（数草案条数即可），但「20 笔」是经验阈值不是硬约束。
		// 用户可能确实想在一天内分 25 笔建仓，那时该由他确认而不是系统拒绝。
		Action:  "ask",
		Message: "该标的当日下单次数超限",
	},
	{
		Name:      "[内置] 草案-单标的集中度近似",
		Priority:  60,
		MatchType: "daily_count",
		// disclosure 是必填的：这条只数草案、不读真实持仓，
		// 用户必须被告知「这是近似」。少了这句话，用户会以为系统知道他的仓位。
		PatternParams: `{"max":"3","field":"symbol","scope":"instrument","disclosure":"基于草案数近似，系统未读取你的实际持仓"}`,
		// ask 而非 deny —— 这条的判据**本身不可靠**：
		// 它只数草案条数，系统从未读取用户的真实持仓（见上面的 disclosure）。
		//
		// 用不可靠的判据做硬拒绝（deny）会误杀：
		// 用户可能真实持仓已经很集中、但草案只有 1 条 —— 那不该被拦；
		// 反之用户可能只是分多笔建同一仓位 —— 那拦下来是错杀。
		//
		// 所以它的正确语义是「提示用户复核」而不是「系统替用户决定」。
		Action:  "ask",
		Message: "单标的集中度超限",
	},
}

// ruleFingerprint 算一条内置规则的定义指纹，用于检测「DB 里的定义与代码不同」。
//
// 只覆盖会改变判定行为的字段（match_type / pattern_params / action）。
// name 不参与 —— 它就是查找键；priority / message 也不参与 ——
// 改它们不影响「放不放行」，只是排序和文案。
func (r builtinDraftRule) fingerprint() string {
	h := sha256.New()
	// 用 \x00 分隔，避免「ab|c」和「a|bc」撞出同一个哈希。
	h.Write([]byte(r.MatchType))
	h.Write([]byte{0})
	// params 必须先归一化：PG 的 jsonb 会重排空白
	// （代码里 `{"max":"10000"}` 读回来是 `{"max": "10000"}`），
	// 直接对原始文本算哈希会把同一个定义误判为不同。
	h.Write([]byte(normalizeJSONText(r.PatternParams)))
	h.Write([]byte{0})
	h.Write([]byte(r.Action))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// normalizeJSONText 把 JSON 文本归一化成紧凑格式（key 排序、无多余空白）。
//
// JSON 解析失败时原样返回 —— 宁可让指纹可能误报，
// 也不要因为一个格式问题让 seed 直接失败（那会让整个 Open() 挂掉）。
func normalizeJSONText(s string) string {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return s
	}
	out, err := json.Marshal(v)
	if err != nil {
		return s
	}
	return string(out)
}

// seedDraftRules 插入闸门依赖的 5 条内置草案规则（幂等）。
//
// 在 Open() 里 seedBuiltins() 之后调用。
//
// 已存在同名规则时不动它（用户可能改过），只打一条 warn 说明
// 「DB 里的定义与当前代码不同」—— 不自动覆盖。
func seedDraftRules(d execer) error {
	for _, r := range builtinDraftRules {
		fp := r.fingerprint()

		// 先查是否已存在，并拿到它的指纹用于比对。
		var existingParams, existingType, existingAction string
		err := d.QueryRow(`SELECT match_type, pattern_params, action
FROM intercept_rules WHERE name = $1 LIMIT 1`, r.Name).
			Scan(&existingType, &existingParams, &existingAction)
		if err == nil {
			// 已存在 —— 不覆盖，但比对指纹
			existing := builtinDraftRule{
				MatchType:     existingType,
				PatternParams: existingParams,
				Action:        existingAction,
			}
			if existing.fingerprint() != fp {
				// 不自动修：用户可能是有意改的。
				// 但必须告知，否则「内置规则升级不生效」是静默的。
				slog.Warn("内置草案规则与当前版本定义不一致（不会自动覆盖）",
					"rule", r.Name,
					"db_match_type", existingType,
					"code_match_type", r.MatchType,
					"hint", "如果你改过这条规则，这是预期的；如果是版本升级，请手工核对")
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		// 不存在 —— 插入。
		//
		// pattern 列对结构化 matcher 无意义（matcher 只读 pattern_params），
		// 但它是 NOT NULL，所以给空串。
		if _, err := d.Exec(`INSERT INTO intercept_rules
(name, enabled, priority, match_target, match_type, pattern, pattern_params, action, message)
VALUES ($1, true, $2, 'draft_field', $3, '', $4, $5, $6)`,
			r.Name, r.Priority, r.MatchType, r.PatternParams, r.Action, r.Message); err != nil {
			return err
		}
	}
	return nil
}

// CountBuiltinDraftRules 返回 DB 里的内置草案规则条数。
//
// 给自检用：全新库建完后应该等于 len(builtinDraftRules)。
// test-fresh-db.sh 通过它（间接）判断 seed 是否执行过。
func (d *DB) CountBuiltinDraftRules() (int, error) {
	var n int
	err := d.QueryRow(`SELECT count(*) FROM intercept_rules
WHERE match_target = 'draft_field' AND name LIKE '[内置]%'`).Scan(&n)
	return n, err
}
