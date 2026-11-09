package db

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"unicode/utf8"
)

// =====================================================================
// 公司主体层
// =====================================================================

// Company is a row in the companies table.
type Company struct {
	ID        int64   `json:"id"`
	Name      string  `json:"name"`
	NKey      string  `json:"nkey"`
	Logo      *string `json:"logo,omitempty"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
}

// CompanyWithScope extends Company with its scope rules and asset count.
type CompanyWithScope struct {
	Company
	Scope      []ScopeRule `json:"scope"`
	AssetCount int         `json:"asset_count"`
}

// ScopeRule is one company_scope row.
type ScopeRule struct {
	ID        int64  `json:"id"`
	CompanyID int64  `json:"company_id"`
	Kind      string `json:"kind"`
	Domain    string `json:"domain,omitempty"`
	Net       string `json:"net,omitempty"`
	Value     string `json:"value,omitempty"`
	Raw       string `json:"raw"`
	Reason    string `json:"reason,omitempty"`
}

// CompanyStore operates on the companies + company_scope tables.
type CompanyStore struct{ db *DB }

var (
	ErrCompanyNameConflict = errors.New("company name already exists")
	ErrCompanyNotFound     = errors.New("company not found")
)

const (
	// 企业范围不限制规则条数:逐个 IP / 域名录入的范围动辄上千条,封顶只会逼用户
	// 拆成多个企业。请求体大小(server 侧 maxCompanyMutationBodyBytes)仍然兜底。
	//
	// Raw and normalized textual scope payloads are bounded by Unicode rune
	// count so multi-byte input is treated consistently by the API and DB layer.
	MaxCompanyScopeRawRunes   = 1024
	MaxCompanyScopeValueRunes = 1024
)

// CompanyScopeValidationError identifies a client-correctable scope error.
// Storage and transaction failures are returned as ordinary errors instead.
type CompanyScopeValidationError struct{ Message string }

func (e *CompanyScopeValidationError) Error() string { return e.Message }

// ValidateCompanyScopeInputBounds applies request-wide limits before parsing.
// Store methods call it again so non-HTTP callers cannot bypass the limits.
// 只约束单条规则的长度,不限制条数。
func ValidateCompanyScopeInputBounds(inputs []ScopeInput) error {
	for i, input := range inputs {
		if utf8.RuneCountInString(input.Value) > MaxCompanyScopeRawRunes {
			return &CompanyScopeValidationError{Message: fmt.Sprintf(
				"企业范围第 %d 条原始值过长: 最多 %d 个字符", i+1, MaxCompanyScopeRawRunes,
			)}
		}
	}
	return nil
}

// Scope writes rebuild derived asset ownership globally, so serialize them to
// ensure the committed attribution always reflects the latest committed rules.
// This key is reserved for company mutations; 7337741001 is the schema lock and
// 7337741002 is the cross-package test-suite lock.
const companyScopeMutationLock int64 = 7337741003

// Companies returns the company store.
func (d *DB) Companies() *CompanyStore { return &CompanyStore{db: d} }

// companyNKey normalises a company name: lowercase + trim + collapse whitespace.
func companyNKey(name string) string {
	return strings.Join(strings.Fields(strings.ToLower(name)), " ")
}

// UpsertCompany creates or updates a company by name. Returns the id and whether
// a new row was created.
//
// PG 版靠 `RETURNING id, (xmax = 0)` 判断"是否新行"—— xmax 是 PG 的 MVCC 系统列，
// SQLite 没有。改成"先查后写"：同事务内先 SELECT 一次，再 upsert。BEGIN IMMEDIATE
// 事务已持全局写锁，中间不会被别的连接插进来，所以两步等价。
func (s *CompanyStore) UpsertCompany(name, logo string) (id int64, created bool, err error) {
	nkey := companyNKey(name)
	var logoVal any
	if logo != "" {
		logoVal = logo
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback() //nolint:errcheck
	var existingID int64
	switch err := tx.QueryRow(`SELECT id FROM companies WHERE nkey = $1`, nkey).Scan(&existingID); {
	case err == nil:
	case errors.Is(err, sql.ErrNoRows):
		created = true
	default:
		return 0, false, err
	}
	if err := tx.QueryRow(`
INSERT INTO companies(name, nkey, logo)
VALUES ($1, $2, $3)
ON CONFLICT (nkey) DO UPDATE SET
    name = EXCLUDED.name,
    logo = COALESCE(EXCLUDED.logo, companies.logo),
    updated_at = now()
RETURNING id`, name, nkey, logoVal).Scan(&id); err != nil {
		return 0, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	return id, created, nil
}

// CreateCompanyWithScope creates a company without updating an existing row.
// The company, its valid initial scope rules, and derived asset attribution are
// committed atomically. Invalid inputs retain the legacy partial-validation
// contract and are reported without preventing valid rules from being stored.
func (s *CompanyStore) CreateCompanyWithScope(name, logo string, inputs []ScopeInput, reason string) (
	id int64, added, skipped, invalid int, validationErrors []string, err error,
) {
	if err := ValidateCompanyScopeInputBounds(inputs); err != nil {
		return 0, 0, 0, 0, nil, err
	}
	rules, invalid, validationErrors := parseScopeInputs(inputs)
	if err := validateParsedScopeBounds(rules); err != nil {
		return 0, 0, 0, invalid, validationErrors, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, 0, invalid, validationErrors, err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := lockCompanyScopeMutation(tx); err != nil {
		return 0, 0, 0, invalid, validationErrors, err
	}

	nkey := companyNKey(name)
	var logoVal any
	if logo != "" {
		logoVal = logo
	}
	if err := tx.QueryRow(`
INSERT INTO companies(name, nkey, logo)
VALUES ($1, $2, $3)
ON CONFLICT (nkey) DO NOTHING
RETURNING id`, name, nkey, logoVal).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, 0, 0, invalid, validationErrors, ErrCompanyNameConflict
		}
		return 0, 0, 0, invalid, validationErrors, err
	}

	added, skipped, needsAttribution, err := insertScopeRulesTx(tx, id, rules, reason)
	if err != nil {
		return 0, 0, 0, invalid, validationErrors, err
	}
	if needsAttribution {
		warning, err := recomputeAttributionTx(tx)
		if err != nil {
			return 0, 0, 0, invalid, validationErrors, err
		}
		logAttributionWarning(warning)
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, 0, invalid, validationErrors, err
	}
	return id, added, skipped, invalid, validationErrors, nil
}

// GetCompany returns one company by id (nil if not found).
func (s *CompanyStore) GetCompany(id int64) (*Company, error) {
	c := &Company{}
	err := s.db.QueryRow(`
SELECT id, name, nkey, logo, created_at, updated_at
FROM companies WHERE id = $1`, id).Scan(
		&c.ID, &c.Name, &c.NKey, &c.Logo, &c.CreatedAt, &c.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return c, err
}

// GetCompanyByName returns one company by normalized name (nil if not found).
func (s *CompanyStore) GetCompanyByName(name string) (*Company, error) {
	nkey := companyNKey(name)
	c := &Company{}
	err := s.db.QueryRow(`
SELECT id, name, nkey, logo, created_at, updated_at
FROM companies WHERE nkey = $1`, nkey).Scan(
		&c.ID, &c.Name, &c.NKey, &c.Logo, &c.CreatedAt, &c.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return c, err
}

// UpsertByName creates the company if it doesn't exist, then returns its id.
func (s *CompanyStore) UpsertByName(name string) (int64, error) {
	id, _, err := s.UpsertCompany(name, "")
	return id, err
}

// DeleteCompany deletes a company and re-evaluates automatic ownership against
// the remaining companies in the same transaction. Explicitly-owned assets are
// detached by the FK and may then fall back to a remaining scope match.
func (s *CompanyStore) DeleteCompany(id int64) error {
	_, err := s.DeleteCompanyWithAssets(id, false)
	return err
}

// DeleteCompanyWithAssets deletes a company and optionally all of its assets in
// one transaction, then re-evaluates ownership against the remaining companies.
func (s *CompanyStore) DeleteCompanyWithAssets(id int64, deleteAssets bool) (assetsDeleted int64, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := lockCompanyScopeMutation(tx); err != nil {
		return 0, err
	}
	if deleteAssets {
		res, err := tx.Exec(`DELETE FROM assets WHERE company_id = $1`, id)
		if err != nil {
			return 0, err
		}
		assetsDeleted, err = res.RowsAffected()
		if err != nil {
			return 0, err
		}
	}
	res, err := tx.Exec(`DELETE FROM companies WHERE id = $1`, id)
	if err != nil {
		return 0, err
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if deleted == 0 {
		return 0, ErrCompanyNotFound
	}
	// This path has no per-request warning channel, so the log is the only place
	// the operator can learn about unparseable ip rows here.
	warning, err := recomputeAttributionTx(tx)
	if err != nil {
		return 0, err
	}
	logAttributionWarning(warning)
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return assetsDeleted, nil
}

// ListCompanies returns all companies with scope and asset count.
func (s *CompanyStore) ListCompanies() ([]*CompanyWithScope, error) {
	rows, err := s.db.Query(`
SELECT c.id, c.name, c.nkey, c.logo, c.created_at, c.updated_at,
       COUNT(DISTINCT a.id) AS asset_count
FROM companies c
LEFT JOIN assets a ON a.company_id = c.id
GROUP BY c.id
ORDER BY c.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*CompanyWithScope
	for rows.Next() {
		cws := &CompanyWithScope{}
		if err := rows.Scan(&cws.ID, &cws.Name, &cws.NKey, &cws.Logo,
			&cws.CreatedAt, &cws.UpdatedAt, &cws.AssetCount); err != nil {
			return nil, err
		}
		out = append(out, cws)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// fetch scope rules for each company
	for _, cws := range out {
		cws.Scope, err = s.GetScope(cws.ID)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// GetScope returns all scope rules for a company.
func (s *CompanyStore) GetScope(companyID int64) ([]ScopeRule, error) {
	rows, err := s.db.Query(`
SELECT id, company_id, kind,
       COALESCE(domain,''), COALESCE(net,''), COALESCE("value",''), raw, COALESCE(reason,'')
FROM company_scope
WHERE company_id = $1
ORDER BY id`, companyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ScopeRule, 0)
	for rows.Next() {
		var r ScopeRule
		if err := rows.Scan(&r.ID, &r.CompanyID, &r.Kind, &r.Domain, &r.Net, &r.Value, &r.Raw, &r.Reason); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AddScope parses and inserts scope lines for a company, then reattributes assets.
// Returns counts of added, skipped, and invalid lines.
func (s *CompanyStore) AddScope(companyID int64, lines []string, reason string) (added, skipped, invalid int, errors []string) {
	inputs := make([]ScopeInput, 0, len(lines))
	for _, line := range lines {
		inputs = append(inputs, ScopeInput{Value: line})
	}
	return s.AddScopeInputs(companyID, inputs, reason)
}

// AddScopeInputs inserts structured scope rules. Empty kinds use the automatic
// CIDR/IP/ICP/domain/keyword classification used by AddScope.
func (s *CompanyStore) AddScopeInputs(companyID int64, inputs []ScopeInput, reason string) (added, skipped, invalid int, errors []string) {
	added, skipped, invalid, validationErrors, err := s.AddScopeInputsChecked(companyID, inputs, reason)
	if err != nil {
		validationErrors = append(validationErrors, err.Error())
	}
	return added, skipped, invalid, validationErrors
}

// AddScopeInputsChecked inserts structured scope rules while keeping input
// validation separate from storage and transaction errors.
func (s *CompanyStore) AddScopeInputsChecked(companyID int64, inputs []ScopeInput, reason string) (
	added, skipped, invalid int, validationErrors []string, err error,
) {
	if err := ValidateCompanyScopeInputBounds(inputs); err != nil {
		return 0, 0, 0, nil, err
	}
	rules, invalid, errors := parseScopeInputs(inputs)
	if err := validateParsedScopeBounds(rules); err != nil {
		return 0, 0, invalid, errors, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, invalid, errors, err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := lockCompanyScopeMutation(tx); err != nil {
		return 0, 0, invalid, errors, err
	}
	if err := ensureCompanyExistsTx(tx, companyID); err != nil {
		return 0, 0, invalid, errors, err
	}
	if len(rules) == 0 {
		if err := tx.Commit(); err != nil {
			return 0, 0, invalid, errors, err
		}
		return 0, 0, invalid, errors, nil
	}
	added, skipped, needsAttribution, err := insertScopeRulesTx(tx, companyID, rules, reason)
	if err != nil {
		return 0, 0, invalid, errors, err
	}
	if needsAttribution {
		warning, err := recomputeAttributionTx(tx)
		if err != nil {
			return 0, 0, invalid, errors, fmt.Errorf("重新计算企业归属失败: %w", err)
		}
		logAttributionWarning(warning)
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, invalid, errors, err
	}
	return added, skipped, invalid, errors, nil
}

func parseScopeInputs(inputs []ScopeInput) (rules []ParsedScope, invalid int, validationErrors []string) {
	rules = make([]ParsedScope, 0, len(inputs))
	for _, input := range inputs {
		rule, err := ParseScopeInput(input)
		if err != nil {
			invalid++
			validationErrors = append(validationErrors, fmt.Sprintf("%s: %v", input.Value, err))
			continue
		}
		rules = append(rules, rule)
	}
	return rules, invalid, validationErrors
}

func validateParsedScopeBounds(rules []ParsedScope) error {
	for i, rule := range rules {
		if utf8.RuneCountInString(rule.Raw) > MaxCompanyScopeRawRunes {
			return &CompanyScopeValidationError{Message: fmt.Sprintf(
				"企业范围第 %d 条原始值过长: 最多 %d 个字符", i+1, MaxCompanyScopeRawRunes,
			)}
		}
		if utf8.RuneCountInString(rule.Value) > MaxCompanyScopeValueRunes {
			return &CompanyScopeValidationError{Message: fmt.Sprintf(
				"企业范围第 %d 条规范化值过长: 最多 %d 个字符", i+1, MaxCompanyScopeValueRunes,
			)}
		}
	}
	return nil
}

func ensureCompanyExistsTx(tx *sql.Tx, companyID int64) error {
	var exists bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM companies WHERE id = $1)`, companyID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrCompanyNotFound
	}
	return nil
}

// lockCompanyScopeMutation 原本取 PG 的事务级 advisory lock（7337741003），
// 让"范围变更 + 全量重算归属"与其它范围变更是串行的。
//
// SQLite 下不需要：BEGIN 走 _txlock=immediate（见 db/sqlite_dsn.go），
// 事务一开始就拿 RESERVED 写锁，同进程/同文件的第二个写事务在 begin 阶段
// 就排队。保留函数壳与常量（companyScopeMutationLock 仍被
// db/company_lock_test.go 的"键值互不复用"断言使用），改动最小。
func lockCompanyScopeMutation(tx *sql.Tx) error {
	return nil
}

func insertScopeRulesTx(tx *sql.Tx, companyID int64, rules []ParsedScope, reason string) (
	added, skipped int, needsAttribution bool, err error,
) {
	for _, rule := range rules {
		inserted, insertErr := insertScopeRuleTx(tx, companyID, rule, reason)
		if insertErr != nil {
			return 0, 0, false, insertErr
		}
		if !inserted {
			skipped++
			continue
		}
		added++
		needsAttribution = needsAttribution || rule.Kind != "keyword"
	}
	return added, skipped, needsAttribution, nil
}

// insertScopeRuleTx inserts a scope rule. inserted=false means a duplicate was
// ignored by ON CONFLICT, not an error.
//
// SQLite 不支持 `ON CONFLICT ON CONSTRAINT <name>`，要写成列清单。三个分支的
// 冲突目标与 schema 里的唯一约束一一对应：
//   - uq_sv2_domain  → (company_id, domain)
//   - uq_sv2_net     → (company_id, net)
//   - uq_sv2_value   → (company_id, kind, value) WHERE kind IN ('icp','keyword')
//
// 判定"是否真插入"不能用 RowsAffected：实测 SQLite 的 `INSERT … DO NOTHING`
// 冲突时 changes 仍是 1。改走 `RETURNING id` —— DO NOTHING 命中冲突时不返回行，
// 于是 Scan 得到 sql.ErrNoRows，与 PG 的语义完全一致。
func insertScopeRuleTx(tx *sql.Tx, companyID int64, rule ParsedScope, reason string) (inserted bool, err error) {
	var id int64
	switch rule.Kind {
	case "domain":
		err = tx.QueryRow(`
INSERT INTO company_scope(company_id, kind, domain, raw, reason)
VALUES ($1, 'domain', $2, $3, $4)
ON CONFLICT (company_id, domain) DO NOTHING
RETURNING id`,
			companyID, rule.Domain, rule.Raw, reason).Scan(&id)
	case "ip", "cidr":
		err = tx.QueryRow(`
INSERT INTO company_scope(company_id, kind, net, raw, reason)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (company_id, net) DO NOTHING
RETURNING id`,
			companyID, rule.Kind, rule.Net, rule.Raw, reason).Scan(&id)
	case "icp", "keyword":
		err = tx.QueryRow(`
INSERT INTO company_scope(company_id, kind, value, raw, reason)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (company_id, kind, value) WHERE kind IN ('icp','keyword') DO NOTHING
RETURNING id`,
			companyID, rule.Kind, rule.Value, rule.Raw, reason).Scan(&id)
	default:
		return false, fmt.Errorf("unsupported company scope kind %q", rule.Kind)
	}
	if errors.Is(err, sql.ErrNoRows) {
		// ON CONFLICT DO NOTHING 命中已有行：不返回行，与 PG 一致。
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return id > 0, nil
}

// RecomputeAttribution rebuilds only scope-derived ownership. Explicit company
// links are immutable under scope edits. Precedence is domain, IP/CIDR, then
// normalized exact ICP; keyword rules never attribute assets.
func (s *CompanyStore) RecomputeAttribution() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := lockCompanyScopeMutation(tx); err != nil {
		return err
	}
	warning, err := recomputeAttributionTx(tx)
	if err != nil {
		return err
	}
	logAttributionWarning(warning)
	return tx.Commit()
}

// sqlNormalizeICPExpr 是"去空白 + 小写"的 SQL 表达式，等价 PG 的
// `lower(regexp_replace(x, '[[:space:]]+', '', 'g'))`。
//
// SQLite 没有 regexp_replace，也不想为它注册 UDF（T01 只给了 regexp_match）。
// 归属匹配用的 ICP 值在 Go 侧已经过 NormalizeICP（去掉所有 Unicode 空白），
// 而 ICP 字面值（如 京ICP备12345678号-1）里出现的空白只有空格和 Tab 两种，
// 所以 replace 两次足够。中文不是 case-sensitive 的，lower() 对 CJK 是 no-op，
// 与 PG 行为一致。
func sqlNormalizeICPExpr(col string) string {
	return `lower(replace(replace(COALESCE(` + col + `,''),' ',''),char(9),''))`
}

// recomputeAttributionTx rebuilds scope-derived ownership. It returns a warning
// for assets whose ip column cannot be parsed: cidr_contains skips them instead
// of aborting the statement, so without this they would silently never receive a
// network-based company. Callers surface the warning and it is always logged.
//
// 三处 DISTINCT ON 改成 `ROW_NUMBER() OVER (PARTITION BY a.id ORDER BY …)` 外包
// 一层取 rn=1 —— SQLite 没有 DISTINCT ON。PG 的 `UPDATE … FROM matched` 在
// SQLite 里叫 `UPDATE … FROM`，语法一致，只是不能给目标表起别名后又在 SET 里
// 用它（这里没用别名）。
func recomputeAttributionTx(tx *sql.Tx) (string, error) {
	// Only derived rows are cleared. Rows with explicit provenance are untouched.
	if _, err := tx.Exec(`
UPDATE assets
SET company_id = NULL, company_source = 'scope'
WHERE company_source = 'scope'`); err != nil {
		return "", err
	}

	// Domain-based attribution (root_domain exact match). Longest matching scope
	// domain wins, then lowest company id — same tie-break as the PG ORDER BY.
	if _, err := tx.Exec(`
WITH matched AS (
    SELECT asset_id, company_id FROM (
        SELECT a.id AS asset_id, cs.company_id,
               ROW_NUMBER() OVER (
                   PARTITION BY a.id
                   ORDER BY length(cs.domain) DESC, cs.company_id
               ) AS rn
        FROM assets a
        JOIN company_scope cs ON cs.kind = 'domain' AND a.root_domain = cs.domain
        WHERE a.company_id IS NULL
          AND a.type IN ('root_domain','subdomain','service','endpoint')
          AND a.root_domain IS NOT NULL
    ) WHERE rn = 1
)
UPDATE assets
SET company_id = (SELECT matched.company_id FROM matched WHERE matched.asset_id = assets.id),
    company_source = 'scope'
WHERE id IN (SELECT asset_id FROM matched)`); err != nil {
		return "", err
	}

	// IP/CIDR attribution for still-unowned assets. cidr_contains 是 T01 注册的
	// UDF（db/sqlite_udf.go），返回 1/0/NULL：脏 IP 与脏网段都返回 NULL，
	// WHERE 里 `= 1` 自然把它们过滤掉 —— 等价 PG 的 try_inet 跳过语义。
	// 最长前缀优先用 cidr_masklen 排序。
	if _, err := tx.Exec(`
WITH matched AS (
    SELECT asset_id, company_id FROM (
        SELECT a.id AS asset_id, cs.company_id,
               ROW_NUMBER() OVER (
                   PARTITION BY a.id
                   ORDER BY cidr_masklen(cs.net) DESC, cs.company_id
               ) AS rn
        FROM assets a
        JOIN company_scope cs ON cs.kind IN ('ip','cidr') AND cidr_contains(cs.net, a.ip) = 1
        WHERE a.company_id IS NULL
          AND a.type IN ('ip','subdomain','service','endpoint')
          AND a.ip IS NOT NULL
    ) WHERE rn = 1
)
UPDATE assets
SET company_id = (SELECT matched.company_id FROM matched WHERE matched.asset_id = assets.id),
    company_source = 'scope'
WHERE id IN (SELECT asset_id FROM matched)`); err != nil {
		return "", err
	}

	// Exact normalized ICP attribution after domain/network precedence.
	if _, err := tx.Exec(`
WITH matched AS (
    SELECT asset_id, company_id FROM (
        SELECT a.id AS asset_id, cs.company_id,
               ROW_NUMBER() OVER (PARTITION BY a.id ORDER BY cs.company_id) AS rn
        FROM assets a
        JOIN company_scope cs ON cs.kind = 'icp'
          AND (` + sqlNormalizeICPExpr("a.icp") + ` = cs."value"
            OR ` + sqlNormalizeICPExpr("a.app_icp") + ` = cs."value")
        WHERE a.company_id IS NULL
          AND (COALESCE(a.icp,'') <> '' OR COALESCE(a.app_icp,'') <> '')
    ) WHERE rn = 1
)
UPDATE assets
SET company_id = (SELECT matched.company_id FROM matched WHERE matched.asset_id = assets.id),
    company_source = 'scope'
WHERE id IN (SELECT asset_id FROM matched)`); err != nil {
		return "", err
	}
	return malformedIPAssetWarning(tx)
}

// malformedIPAssetsSampled bounds how many offending ids one warning names, so a
// large batch of bad rows stays readable in a toast and in the log.
const malformedIPAssetsSampled = 5

// logAttributionWarning records a recompute warning in the server log. Every
// recompute path calls it, so the warning is reported even for triggers with no
// per-request response (company deletion, scopesentry sync, agent asset writes).
func logAttributionWarning(warning string) {
	if warning != "" {
		log.Printf("[assets] %s", warning)
	}
}

// malformedIPAssetQueryer is satisfied by both *sql.Tx and *DB so the warning
// can be produced inside a recompute transaction or read standalone by the API.
type malformedIPAssetQueryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

// MalformedIPAssetWarning reports assets with an unparseable ip outside of any
// mutation, letting the API attach the warning to a scope response without
// widening the mutation signatures — an unrelated data problem is not one of
// this request's validation errors.
func (s *CompanyStore) MalformedIPAssetWarning() (string, error) {
	return malformedIPAssetWarning(s.db)
}

// malformedIPAssetWarning describes assets whose ip column is not a valid
// address. They are invisible to network attribution, so the operator has to be
// told which rows to fix — silently skipping them would look like scope rules
// that simply do not work.
//
// PG 用 `try_inet(ip) IS NULL` 判脏数据。SQLite 没有 inet 类型，这里用
// cidr_contains 反推：一个合法地址至少要能构成 /32 或 /128 的单机网段
// （cidr_contains(ip||'/32', ip) 返回 1）。两种形态都返回 NULL 即非法。
func malformedIPAssetWarning(q malformedIPAssetQueryer) (string, error) {
	rows, err := q.Query(`
SELECT id, ip, count(*) OVER () AS total
FROM assets
WHERE ip IS NOT NULL AND ip <> ''
  AND `+sqlIsMalformedIP("ip")+`
  AND type IN ('ip','subdomain','service','endpoint')
ORDER BY id
LIMIT $1`, malformedIPAssetsSampled)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var total int
	samples := make([]string, 0, malformedIPAssetsSampled)
	for rows.Next() {
		var id int64
		var ip string
		if err := rows.Scan(&id, &ip, &total); err != nil {
			return "", err
		}
		samples = append(samples, fmt.Sprintf("#%d %s", id, ip))
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if total == 0 {
		return "", nil
	}
	warning := fmt.Sprintf(
		"%d 条资产的 ip 字段不是合法 IP，已跳过 IP/CIDR 范围匹配（这些资产不会被网段规则归属到企业）：%s",
		total, strings.Join(samples, "、"),
	)
	if total > len(samples) {
		warning += fmt.Sprintf(" 等 %d 条", total)
	}
	return warning, nil
}

// sqlIsMalformedIP 返回"该列不是合法 IP"的 SQL 条件，等价 PG 的
// `try_inet(col) IS NULL`：既不是 IPv4（构不出 /32）也不是 IPv6（构不出 /128）。
func sqlIsMalformedIP(col string) string {
	return `(cidr_contains(` + col + `||'/32', ` + col + `) IS NULL` +
		` AND cidr_contains(` + col + `||'/128', ` + col + `) IS NULL)`
}

// UpdateScope replaces all scope rules for a company and reattributes.
func (s *CompanyStore) UpdateScope(companyID int64, lines []string, reason string) (added, invalid int, errs []string) {
	inputs := make([]ScopeInput, 0, len(lines))
	for _, line := range lines {
		inputs = append(inputs, ScopeInput{Value: line})
	}
	return s.UpdateScopeInputs(companyID, inputs, reason)
}

// UpdateScopeInputs replaces all rules with a structured set.
func (s *CompanyStore) UpdateScopeInputs(companyID int64, inputs []ScopeInput, reason string) (added, invalid int, errs []string) {
	added, invalid, validationErrors, err := s.UpdateScopeInputsChecked(companyID, inputs, reason)
	if err != nil {
		validationErrors = append(validationErrors, err.Error())
	}
	return added, invalid, validationErrors
}

// UpdateScopeInputsChecked replaces all rules while separating validation
// feedback from storage and transaction failures.
func (s *CompanyStore) UpdateScopeInputsChecked(companyID int64, inputs []ScopeInput, reason string) (
	added, invalid int, validationErrors []string, err error,
) {
	if err := ValidateCompanyScopeInputBounds(inputs); err != nil {
		return 0, 0, nil, err
	}
	rules, invalid, errs := parseScopeInputs(inputs)
	if invalid > 0 {
		return 0, invalid, errs, &CompanyScopeValidationError{Message: fmt.Sprintf(
			"企业范围包含 %d 条无效规则，未覆盖原有范围", invalid,
		)}
	}
	if err := validateParsedScopeBounds(rules); err != nil {
		return 0, invalid, errs, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, invalid, errs, err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := lockCompanyScopeMutation(tx); err != nil {
		return 0, invalid, errs, err
	}
	if err := ensureCompanyExistsTx(tx, companyID); err != nil {
		return 0, invalid, errs, err
	}
	if _, err := tx.Exec(`DELETE FROM company_scope WHERE company_id = $1`, companyID); err != nil {
		return 0, invalid, errs, err
	}
	added, _, _, err = insertScopeRulesTx(tx, companyID, rules, reason)
	if err != nil {
		return 0, invalid, errs, err
	}
	// Rebuild even for an empty replacement because removing the old rules may
	// detach scope-derived assets or expose a lower-precedence company match.
	warning, err := recomputeAttributionTx(tx)
	if err != nil {
		return 0, invalid, errs, fmt.Errorf("重新计算企业归属失败: %w", err)
	}
	logAttributionWarning(warning)
	if err := tx.Commit(); err != nil {
		return 0, invalid, errs, err
	}
	return added, invalid, errs, nil
}

// ResolveCompany returns the company_id for a given root_domain and/or ip, or nil
// if no scope rule matches. Mirrors the attribution logic used at asset insert time.
func (s *CompanyStore) ResolveCompany(rootDomain, ipStr string) (*int64, error) {
	return s.ResolveCompanyWithICP(rootDomain, ipStr, "")
}

// ResolveCompanyWithICP mirrors RecomputeAttribution for insert-time ownership.
// ICP is consulted only after domain and IP/CIDR fail to match.
func (s *CompanyStore) ResolveCompanyWithICP(rootDomain, ipStr, icp string) (*int64, error) {
	return resolveCompanyWithICP(s.db, rootDomain, ipStr, icp)
}
type companyScopeQueryer interface {
	QueryRow(query string, args ...any) *sql.Row
}

func resolveCompanyWithICP(q companyScopeQueryer, rootDomain, ipStr, icp string) (*int64, error) {
	if rootDomain != "" {
		var cid int64
		err := q.QueryRow(`
SELECT company_id FROM company_scope
WHERE kind = 'domain'
  AND domain = $1
ORDER BY length(domain) DESC, company_id
LIMIT 1`, rootDomain).Scan(&cid)
		if err == nil {
			return &cid, nil
		}
		if err != sql.ErrNoRows {
			return nil, err
		}
	}
	if ipStr != "" {
		if net.ParseIP(ipStr) != nil {
			var cid int64
			err := q.QueryRow(`
SELECT company_id FROM company_scope
WHERE kind IN ('ip','cidr')
  AND cidr_contains(net, $1) = 1
ORDER BY cidr_masklen(net) DESC, company_id
LIMIT 1`, ipStr).Scan(&cid)
			if err == nil {
				return &cid, nil
			}
			if err != sql.ErrNoRows {
				return nil, err
			}
		}
	}
	if normalized := NormalizeICP(icp); normalized != "" {
		var cid int64
		err := q.QueryRow(`
SELECT company_id FROM company_scope
WHERE kind = 'icp' AND "value" = $1
ORDER BY company_id
LIMIT 1`, normalized).Scan(&cid)
		if err == nil {
			return &cid, nil
		}
		if err != sql.ErrNoRows {
			return nil, err
		}
	}
	return nil, nil
}
