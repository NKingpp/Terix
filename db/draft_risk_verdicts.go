package db

// 草案风控求值记录（draft_risk_verdicts）数据层。
//
// # 这张表是审计链的锚
//
// schema.sql:1753 的原话：
//
//	「rules_snapshot 存命中规则的当时定义 —— 否则规则被改后就无法复现当时判定」
//
// 所以它不是「结论的备份」，是**「当时依据什么判的」的唯一证据**。
// 结论（pass/deny/ask）在 orders_draft.risk_verdict 里也有一份，
// 但那份只有结论，没有依据 —— 事后无法回答「为什么」。
//
// # 与 orders_draft.risk_verdict 的权威关系
//
//	orders_draft.risk_verdict   当前结论（快照）
//	  用途：列表页展示、快速筛选
//	  特征：单值，每次求值覆盖
//	  权威性：**派生值，可重建**
//
//	draft_risk_verdicts         历史记录（事件流）
//	  用途：审计、复现「为什么这么判」
//	  特征：每次求值追加一条
//	  权威性：**唯一事实来源**
//
// 两者不一致是 bug，不是正常状态。InsertDraftRiskVerdict 在**同一事务**里
// 同时写两边，保证不会「写了一边忘了另一边」。

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrDraftVerdictNotFound 表示该草案没有求值记录。
var ErrDraftVerdictNotFound = errors.New("draft risk verdict not found")

// validVerdictActions 是结论白名单，与 schema 的 CHECK 一致。
//
// 注意 schema 用的是 pass/deny/ask，而 intercept.Decision 用 allow/deny/ask。
// 两者措辞不同！这里做归一：allow → pass。
// 归一放在写入层而不是调用方 —— 否则每个调用方都要记得转。
var validVerdictActions = map[string]string{
	"pass":  "pass",
	"allow": "pass", // intercept 用 allow，表里用 pass
	"deny":  "deny",
	"ask":   "ask",
}

// DraftRiskVerdict 是 draft_risk_verdicts 的一行。
type DraftRiskVerdict struct {
	ID             int64           `json:"id"`
	DraftID        int64           `json:"draft_id"`
	Verdict        string          `json:"verdict"`
	MatchedRuleIDs []int64         `json:"matched_rule_ids"`
	RuleResults    json.RawMessage `json:"rule_results"`
	Reason         string          `json:"reason"`
	RulesRevision  int64           `json:"rules_revision"`
	RulesSnapshot  json.RawMessage `json:"rules_snapshot"`
	EvaluatedBy    string          `json:"evaluated_by"`
	EvaluatedAt    string          `json:"evaluated_at"`
}

// DraftRiskVerdictInput 是 InsertDraftRiskVerdict 的入参。
type DraftRiskVerdictInput struct {
	DraftID        int64
	Verdict        string
	MatchedRuleIDs []int64
	RuleResults    json.RawMessage
	Reason         string
	RulesRevision  int64
	RulesSnapshot  json.RawMessage
	EvaluatedBy    string
}

// InsertDraftRiskVerdict 写入一条求值记录，并同步更新 orders_draft.risk_verdict。
//
// # 为什么两者在同一事务里写
//
// 两套记录描述同一件事。分开写会出现「verdicts 有新记录但 orders_draft 还是旧结论」——
// 而 orders_draft.risk_verdict 是列表页读的，用户会看到过期结论。
//
// # Verdict 的措辞归一
//
// 入参接受 allow / pass 两种（intercept 用 allow，表里用 pass），
// 写入前统一成表里的值。这层归一放在这里而不是调用方：
// 让调用方传什么都不会写进脏值。
func (d *DB) InsertDraftRiskVerdict(in DraftRiskVerdictInput) (int64, error) {
	if in.DraftID <= 0 {
		return 0, fmt.Errorf("%w: draft_id 必须为正", ErrDraftInvalid)
	}
	verdict, ok := validVerdictActions[strings.ToLower(strings.TrimSpace(in.Verdict))]
	if !ok {
		return 0, fmt.Errorf("%w: verdict %q 不在 pass/deny/ask（或 allow）之内", ErrDraftInvalid, in.Verdict)
	}

	ruleResults := in.RuleResults
	if len(ruleResults) == 0 {
		ruleResults = json.RawMessage(`[]`)
	}
	snapshot := in.RulesSnapshot
	if len(snapshot) == 0 {
		snapshot = json.RawMessage(`{}`)
	}
	matched := in.MatchedRuleIDs
	if matched == nil {
		matched = []int64{}
	}
	// matched_rule_ids 是 TEXT 列且存 JSON 数组：SQLite 不接受 []int64 参数
	// （unsupported type），必须自己序列化。rule_results / rules_snapshot
	// 同样走字符串绑定 —— 绑 []byte 会落成 BLOB，读写两侧的类型就不一致了。
	matchedJSON, err := json.Marshal(matched)
	if err != nil {
		return 0, fmt.Errorf("序列化 matched_rule_ids 失败: %w", err)
	}
	by := strings.TrimSpace(in.EvaluatedBy)
	if by == "" {
		by = "local"
	}

	tx, err := d.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	var id int64
	if err := tx.QueryRow(`
INSERT INTO draft_risk_verdicts
    (draft_id, verdict, matched_rule_ids, rule_results, reason,
     rules_revision, rules_snapshot, evaluated_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id`,
		in.DraftID, verdict, string(matchedJSON), string(ruleResults),
		strings.TrimSpace(in.Reason), in.RulesRevision, string(snapshot), by,
	).Scan(&id); err != nil {
		return 0, fmt.Errorf("写入草案风控记录失败: %w", err)
	}

	// 同步更新当前结论快照。
	if _, err := tx.Exec(`UPDATE orders_draft SET risk_verdict = $2 WHERE id = $1`,
		in.DraftID, verdict); err != nil {
		return 0, fmt.Errorf("同步 orders_draft.risk_verdict 失败: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

// verdictColumns 与 scanDraftVerdict 的扫描顺序必须一致。
//
// matched_rule_ids 在 SQLite 里本来就是 JSON 数组文本（TEXT 列），不需要 PG 的
// `to_json(bigint[])::text` 转换 —— 直接读出来反序列化即可。
// evaluated_at 同理：列声明是 TIMESTAMP（Affinity NUMERIC，实际存 ISO 文本），
// 不需要 `::text` 转型（SQLite 没有 `::` 语法）。
const verdictColumns = `id, draft_id, verdict, matched_rule_ids, rule_results,
	COALESCE(reason, ''), rules_revision, rules_snapshot,
	COALESCE(evaluated_by, ''), evaluated_at`

// scanDraftVerdict 从一行结果读出一条 DraftRiskVerdict。
func scanDraftVerdict(row interface{ Scan(...any) error }) (*DraftRiskVerdict, error) {
	var (
		v           DraftRiskVerdict
		matchedRaw  sql.NullString
		ruleResults []byte
		snapshot    []byte
	)
	if err := row.Scan(&v.ID, &v.DraftID, &v.Verdict, &matchedRaw, &ruleResults,
		&v.Reason, &v.RulesRevision, &snapshot, &v.EvaluatedBy, &v.EvaluatedAt); err != nil {
		return nil, err
	}
	// matched_rule_ids 是 JSON 数组文本（见 verdictColumns 注释），反序列化回 []int64。
	// NULL / 空串 / PG 遗留的 '{}' 都归一成空切片。
	matched := []int64{}
	if matchedRaw.Valid && strings.TrimSpace(matchedRaw.String) != "" && strings.TrimSpace(matchedRaw.String) != "{}" {
		if err := json.Unmarshal([]byte(matchedRaw.String), &matched); err != nil {
			return nil, fmt.Errorf("解析 matched_rule_ids 失败: %w", err)
		}
	}
	v.MatchedRuleIDs = matched
	v.RuleResults = normalizeJSON(ruleResults)
	v.RulesSnapshot = normalizeJSON(snapshot)
	return &v, nil
}

// LatestDraftRiskVerdict 取某草案最新一次求值记录。
//
// 没有记录时返回 ErrDraftVerdictNotFound —— **不返回 nil, nil**。
// 让调用方必须显式处理「没求值过」这个分支。
func (d *DB) LatestDraftRiskVerdict(draftID int64) (*DraftRiskVerdict, error) {
	row := d.QueryRow(`SELECT `+verdictColumns+`
FROM draft_risk_verdicts WHERE draft_id = $1
ORDER BY evaluated_at DESC, id DESC LIMIT 1`, draftID)
	v, err := scanDraftVerdict(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrDraftVerdictNotFound
		}
		return nil, err
	}
	return v, nil
}

// ListDraftRiskVerdicts 列出某草案的全部求值历史（新→旧）。
//
// 用于审计：用户问「这单为什么被判 deny 过」，要看的是完整历史，
// 不是最后一次 —— 中间可能有一次 ask 后人工放行。
func (d *DB) ListDraftRiskVerdicts(draftID int64) ([]*DraftRiskVerdict, error) {
	rows, err := d.Query(`SELECT `+verdictColumns+`
FROM draft_risk_verdicts WHERE draft_id = $1
ORDER BY evaluated_at DESC, id DESC`, draftID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*DraftRiskVerdict, 0, 4)
	for rows.Next() {
		v, err := scanDraftVerdict(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
