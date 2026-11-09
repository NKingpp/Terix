package db

// 行情渠道凭证的存取。
//
// # 读写分离的核心
//
// 三列敏感字段（api_key / api_secret / passphrase）**只写不读**：
//   - 写路径（Upsert）接收全量值，落库
//   - 读路径（List / Get）用 credentialCols（不含敏感列），只用 api_key_hint 展示
//
// 这与 llm_profiles 的 profileCols / profileColsKey 是同一套模式。
// 唯一会 SELECT 全量 key 的是 SeparateAPIKeyForUse（供 server 内部调 CLI 用，
// 结果永不进 HTTP 响应）。
//
// # 为什么不复用 llm_profiles
//
// 1. 生命周期不同：LLM 配置是「当前用哪个模型」，行情凭证是「这个渠道能不能用」。
// 2. 字段不同：OKX 需要 passphrase（三元组）。
// 3. 语义不同：llm_profiles 有 is_default（唯一激活），行情凭证可多个并行启用。

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrCredentialNotFound 表示凭证不存在。
var ErrCredentialNotFound = errors.New("行情凭证不存在")

// MarketCredential 是一条行情渠道凭证（**不含敏感字段**）。
//
// 注意：这个结构体刻意没有 APIKey / APISecret / Passphrase 字段 ——
// 让「读出来就带着明文 key」这件事在类型层面不可能发生。
// 需要全量 key 时必须走 SeparateAPIKeyForUse，那个函数的名字就是一个警告。
type MarketCredential struct {
	ID       int64  `json:"id"`
	Provider string `json:"provider"`
	// Kind 是凭据用途：'data'（行情数据源）| 'trade'（交易通道）。
	//
	// # 为什么同一个 provider 会有两条凭据
	//
	// OKX 的行情走公开接口（可免凭证，填 key 只为提高限频配额），
	// 而交易必须用 API Key。两者是不同的密钥、不同的权限。
	//
	// # 为什么不可修改
	//
	// 它决定「这条凭据的字段布局」（trade 要 passphrase，data 不要）。
	// 改 kind 等于「用 A 类型的字段去填 B 类型的凭据」——
	// 校验会通过（字段都满足新 kind 的 required），但数据是错的。
	// 所以改 kind 必须先删再建（见 UpdateMarketCredentialByID 的注释）。
	Kind         string     `json:"kind"`
	AccountLabel string     `json:"account_label"`
	APIKeyHint   string     `json:"api_key_hint"`
	BaseURL      string     `json:"base_url"`
	Enabled      bool       `json:"enabled"`
	VerifiedAt   *time.Time `json:"verified_at"`
	VerifyError  string     `json:"verify_error"`
	// Extra 是 provider 特有的**非敏感**字段（如 fosun 的 apply_account_id）。
	//
	// # 为什么敏感字段不能放这里
	//
	// Extra 是**整体返回给前端**的（不像 api_key 有独立的 hint 列做读写分离）。
	// 一旦把敏感值放进 Extra，就绕过了 credentialCols 那个「读路径不查敏感列」
	// 的保护 —— 而那是这套设计最强的防线（数据根本不进内存）。
	//
	// 如果真的需要在 Extra 里放敏感值，不要偷偷放：那时应该加
	// extra_secret + extra_hint 两列，或者按 secrets/secret_hints 重构。
	Extra     map[string]string `json:"extra,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}

// credentialCols 是读路径的列清单（**不含敏感三列**）。
const credentialCols = `id,provider,kind,account_label,api_key_hint,COALESCE(base_url,''),enabled,verified_at,COALESCE(verify_error,''),COALESCE(extra,'{}'),created_at,updated_at`

func scanCredential(sc interface{ Scan(...any) error }, c *MarketCredential) error {
	// 时间列在 SQLite 里是 TEXT（ISO8601 UTC），驱动不会自动转 time.Time ——
	// 实测直接扫进 sql.NullTime 报 "unsupported Scan, storing driver.Value type
	// string into type *time.Time"。所以先取字符串，再走统一的 ParseDBTime。
	var (
		verifiedAt sql.NullString
		createdAt  string
		updatedAt  string
		extraRaw   []byte
	)
	if err := sc.Scan(&c.ID, &c.Provider, &c.Kind, &c.AccountLabel, &c.APIKeyHint, &c.BaseURL,
		&c.Enabled, &verifiedAt, &c.VerifyError, &extraRaw, &createdAt, &updatedAt); err != nil {
		return err
	}
	if len(extraRaw) > 0 {
		if err := json.Unmarshal(extraRaw, &c.Extra); err != nil {
			c.Extra = nil // 解析失败不影响主流程（extra 是附加信息）
		}
	}
	if verifiedAt.Valid && verifiedAt.String != "" {
		if t, err := ParseDBTime(verifiedAt.String); err == nil {
			c.VerifiedAt = &t
		}
	}
	// 解析失败保留零值：updated_at 只用于展示，不值得让整条读取失败。
	if t, err := ParseDBTime(createdAt); err == nil {
		c.CreatedAt = t
	}
	if t, err := ParseDBTime(updatedAt); err == nil {
		c.UpdatedAt = t
	}
	return nil
}

// ListMarketCredentials 列出全部凭证（只含 hint）。
func (d *DB) ListMarketCredentials() ([]*MarketCredential, error) {
	rows, err := d.Query(`SELECT ` + credentialCols + ` FROM market_credentials ORDER BY provider, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*MarketCredential
	for rows.Next() {
		var c MarketCredential
		if err := scanCredential(rows, &c); err != nil {
			return nil, err
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// GetMarketCredential 按 id 取一条（只含 hint）。
func (d *DB) GetMarketCredential(id int64) (*MarketCredential, error) {
	row := d.QueryRow(`SELECT `+credentialCols+` FROM market_credentials WHERE id=$1`, id)
	var c MarketCredential
	if err := scanCredential(row, &c); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrCredentialNotFound
		}
		return nil, err
	}
	return &c, nil
}

// MarketCredentialInput 是新增/更新的入参。
//
// # 空值语义（与 server 的 setLLM 一致）
//
//   - 空字符串 ""    = **保留旧值**（用户只想改 base_url 不想重打 key）
//   - ClearMarker    = **显式清空**（给用户一个清空手段，否则永远改不掉旧 key）
//   - 其他           = 新值
//
// 为什么需要 ClearMarker 而不是「空串=清空」：
// 因为空串必须表示「保留」—— 前端不回显 key，用户只改其他字段时那次提交里
// key 字段就是空的。如果空串表示清空，改任何字段都会把 key 清掉。
const ClearMarker = "<clear>"

// MarketCredentialInput 是 upsert 的入参（含全量敏感值）。
type MarketCredentialInput struct {
	Provider string
	// Kind 是凭据用途：'data' | 'trade'。空串时按 'trade'（兼容既有调用方）。
	Kind         string
	AccountLabel string
	APIKey       string
	APISecret    string
	Passphrase   string
	BaseURL      string
	Enabled      *bool // nil = 保留旧值（新建时默认 true）

	// Extra 是 provider 特有的非敏感字段（如 fosun 的 apply_account_id）。
	//
	// # 语义与三个敏感字段不同
	//
	// 敏感字段用 keepSentinel 表达「保留旧值」（因为空串是合法输入）。
	// Extra 用 nil 表达「保留旧值」——**调用方不传就是不改**。
	// 传空 map 表示「显式清空所有 extra 键」。
	//
	// 为什么允许清空：provider 的字段集合可能变（如 fosun 加了新必填字段，
	// 用户想重置）。清空后由前端按最新 meta 重新填。
	Extra map[string]string

	// CreateOnly=true 时，(provider, account_label) 已存在则返回
	// ErrCredentialExists 而不是覆盖。
	//
	// 为什么需要这个开关：
	//   - 前端「新增」流程 → create_only=true → 撞名报 409（用户确实想新建）
	//   - 前端「编辑」流程 → 不传 → 覆盖（用户就是想改这条）
	//
	// 默认 false = 覆盖，符合「编辑」的常见预期。
	CreateOnly bool
}

// ErrCredentialExists 表示 create_only=true 但记录已存在。
var ErrCredentialExists = errors.New("该 provider + 账户标签已存在")

// UpsertMarketCredential 按 (provider, account_label) 新增或更新。
//
// # 空值处理在 SQL 里做，不在 Go 里做
//
// 因为需要区分「保留旧值」和「显式清空」，而后者要写空串 ——
// 在 Go 里先读出旧值再决定写什么会有 TOCTOU 竞态（两个并发请求互相覆盖）。
// 用 CASE WHEN 一次原子完成。
func (d *DB) UpsertMarketCredential(in MarketCredentialInput) (*MarketCredential, error) {
	provider := strings.TrimSpace(in.Provider)
	if provider == "" {
		return nil, fmt.Errorf("provider 不能为空")
	}
	// kind 归一：空串按 'trade'。
	//
	// 为什么允许空串：既有调用方（测试、内部脚本）不传 kind，
	// 而它们建的都是交易凭据。归一在这里比要求每个调用方都传更省事。
	kind := strings.TrimSpace(in.Kind)
	if kind == "" {
		kind = "trade"
	}
	if kind != "data" && kind != "trade" {
		return nil, fmt.Errorf("kind 必须是 data 或 trade，收到 %q", kind)
	}
	label := in.AccountLabel // label 允许空串（「默认账户」）

	// create_only 预检：要在写之前判断，否则 UPSERT 已经覆盖了。
	if in.CreateOnly {
		var exists bool
		if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM market_credentials
WHERE provider=$1 AND kind=$2 AND account_label=$3)`, provider, kind, label).Scan(&exists); err != nil {
			return nil, err
		}
		if exists {
			return nil, ErrCredentialExists
		}
	}

	// 三个敏感字段各自独立翻译成「要写入的值，或 sentinel 表示保留」。
	//
	// 用 sentinel 而不是「空串=保留」的原因：空串是一个**合法的用户输入**
	// （虽然对 key 没意义，但对 base_url 有）。把「保留」编码成一个绝不会
	// 出现在真实凭证里的字符串，SQL 只需判断 `= sentinel`，语义无歧义。
	keyVal, keyKeep := secretUpdateExpr(in.APIKey)
	secVal, _ := secretUpdateExpr(in.APISecret)
	passVal, _ := secretUpdateExpr(in.Passphrase)

	if keyKeep {
		keyVal = keepSentinel
	}
	if _, k := secretUpdateExpr(in.APISecret); k {
		secVal = keepSentinel
	}
	if _, k := secretUpdateExpr(in.Passphrase); k {
		passVal = keepSentinel
	}

	// hint：key 被改写时重算；保留时用 sentinel 让 SQL 保留旧 hint；
	// 显式清空时 hint 也要清空。
	hint := keepSentinel
	if !keyKeep {
		hint = MaskAPIKey(keyVal) // keyVal 此时是真实值或 ""（显式清空）
	}

	// extra：先读旧值再按字段级语义合并。
	//
	// # 为什么在 Go 里合并而不是 SQL
	//
	// SQL 的 jsonb 合并（`||`）表达不了「空串 = 删除该键」这个语义 ——
	// 它只会把空串当成一个值存进去。而前端「清除单个字段」的交互需要这个语义
	// （见 mergeExtra 的注释）。
	//
	// 代价是多一次 SELECT。这条路径不在热路径上（保存凭证是低频操作），
	// 换来的是语义清晰 + 可单测（mergeExtra 是纯函数）。
	var extraJSON []byte
	if in.Extra != nil {
		stored, sErr := d.storedExtraFor(provider, kind, label)
		if sErr != nil {
			return nil, fmt.Errorf("读取已有 extra 失败: %w", sErr)
		}
		merged := mergeExtra(stored, in.Extra)
		raw, mErr := marshalExtra(merged, true)
		if mErr != nil {
			return nil, mErr
		}
		extraJSON = raw
	}

	var id int64
	err := d.QueryRow(`
INSERT INTO market_credentials
    (provider, kind, account_label, api_key, api_secret, passphrase, api_key_hint, base_url, enabled, extra)
VALUES ($1, $11, $2,
        CASE WHEN $3 = $7 THEN '' ELSE $3 END,
        CASE WHEN $4 = $7 THEN '' ELSE $4 END,
        CASE WHEN $5 = $7 THEN '' ELSE $5 END,
        CASE WHEN $6 = $7 THEN '' ELSE $6 END,
        COALESCE($8,''), COALESCE($9,true), COALESCE($10,'{}'))
ON CONFLICT (provider, kind, account_label) DO UPDATE SET
    -- sentinel（$7=keepSentinel）表示「保留旧值」。见 secretUpdateExpr 的说明。
    api_key     = CASE WHEN $3 = $7 THEN market_credentials.api_key      ELSE $3 END,
    api_secret  = CASE WHEN $4 = $7 THEN market_credentials.api_secret   ELSE $4 END,
    passphrase  = CASE WHEN $5 = $7 THEN market_credentials.passphrase   ELSE $5 END,
    api_key_hint= CASE WHEN $6 = $7 THEN market_credentials.api_key_hint ELSE $6 END,
    base_url    = COALESCE(NULLIF($8,''), market_credentials.base_url),
    enabled     = COALESCE($9, market_credentials.enabled),
    -- extra：NULL（=nil 未传）表示保留旧值；传了就是**已合并好的完整对象**（见 mergeExtra）。
    extra       = COALESCE($10, market_credentials.extra)
RETURNING id`,
		provider, label, keyVal, secVal, passVal, hint, keepSentinel,
		strings.TrimSpace(in.BaseURL), in.Enabled, extraJSON,
		kind,
	).Scan(&id)
	if err != nil {
		return nil, fmt.Errorf("保存行情凭证失败: %w", err)
	}
	return d.GetMarketCredential(id)
}

// keepSentinel 是「保留旧值」的哨兵值。
//
// # 为什么用哨兵而不是「空串=保留」
//
// 空串是一个**合法的用户输入**。虽然对 api_key 没意义，但对其他字段有
// （例如 base_url 传空串可能表示「用默认」）。把「保留」编码成一个
// 绝不会出现在真实凭证里的字符串，SQL 只需判断 `= sentinel`，语义无歧义。
//
// 选用一个绝不可能出现在真实凭证里的长串，是为了让它在日志/调试输出里
// 一眼看出「这是元指令，不是数据」。
const keepSentinel = "__ARCTEX_KEEP_OLD_VALUE__"

// secretUpdateExpr 把入参翻译成「要写入的值」+「是否保留旧值」。
//
// 返回值语义：
//   - 空串 ""        → ("", true)   —— 保留旧值
//   - ClearMarker    → ("", false)  —— 显式清空
//   - 其他           → (v, false)   —— 写入新值
//
// 调用方把 keep=true 的返回值替换成 keepSentinel 再传给 SQL。
func secretUpdateExpr(v string) (value string, keep bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", true
	}
	if v == ClearMarker {
		return "", false // 显式清空
	}
	return v, false
}

// DeleteMarketCredential 物理删除（用户要求删就是要删干净，不留审计残留）。
func (d *DB) DeleteMarketCredential(id int64) error {
	res, err := d.Exec(`DELETE FROM market_credentials WHERE id=$1`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrCredentialNotFound
	}
	return nil
}

// SetCredentialVerifyResult 记录一次验证结果。
//
// verifyError 必须**已经脱敏**（调用方负责），这里不做二次处理 ——
// 因为把「脱敏」放在 db 层会让它成为可选，而放在调用方紧邻 CLI 输出的位置
// 才是唯一能保证不漏的地方。
func (d *DB) SetCredentialVerifyResult(id int64, ok bool, verifyError string) error {
	if ok {
		_, err := d.Exec(`UPDATE market_credentials
SET verified_at=now(), verify_error='' WHERE id=$1`, id)
		return err
	}
	// 失败时不动 verified_at（保留「上次成功」时间），只记错误。
	_, err := d.Exec(`UPDATE market_credentials SET verify_error=$2 WHERE id=$1`, id, verifyError)
	return err
}

// SeparateAPIKeyForUse 返回某条凭证的**全量**敏感字段，供 server 内部调 CLI 用。
//
// # 为什么名字这么长
//
// 这是唯一会读出明文 key 的函数。名字本身就是警告：**分离出来是为了「用」，
// 不是为了「展示」**。任何把它的返回值放进 HTTP 响应 / 日志 / 错误的调用都是 bug。
//
// 命名参考：`Separate` 强调「从只读模型里分离出敏感部分」这个动作。
func (d *DB) SeparateAPIKeyForUse(id int64) (apiKey, apiSecret, passphrase, baseURL string, err error) {
	row := d.QueryRow(`SELECT COALESCE(api_key,''),COALESCE(api_secret,''),COALESCE(passphrase,''),COALESCE(base_url,'')
FROM market_credentials WHERE id=$1`, id)
	err = row.Scan(&apiKey, &apiSecret, &passphrase, &baseURL)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", "", "", ErrCredentialNotFound
	}
	return apiKey, apiSecret, passphrase, baseURL, err
}

// GetMarketCredentialSecrets 取出一条凭证的**全量**敏感字段 + extra。
//
// # ⚠️ 安全约束：只能在服务端内部用，绝不能出现在任何 HTTP 响应里
//
// 这个函数与 SeparateAPIKeyForUse 同级别危险，但更强：它把三个敏感列
// **和 extra 一起**交出去，返回值是一个完整的凭据。
//
// ## 为什么禁止它进 handler 响应
//
// 表的读路径（ListMarketCredentials / GetMarketCredential）刻意用
// credentialCols 排除了 api_key/api_secret/passphrase —— 那是这套设计里
// **最强的防线：敏感值根本不进内存**，因此不存在「忘了脱敏」的可能。
//
// 一旦某个 handler 把本函数的返回值 writeJSON 出去，防线就退化成
// 「靠调用方记得脱敏」，而那类防线在实际项目里必然被突破一次
// （新增导出、调试日志、错误包装、前端缓存……每条路径都要重新记一遍）。
//
// ## 为什么还要它
//
// 没有它就无法建连：MCP 客户端要从 env map 拿 Authorization header，
// 而那个 token 只存在于 api_key 列。调用方（server/mcp_credential_sync.go）
// 拿到后**只**用于构造 header，绝不回传。
//
// ## 允许的调用形态（白名单）
//
//	SHA256 校验校验用 → 建连（MCP/CLI）→ 立刻丢弃
//	任何「返回给调用方看」的用途都属于 bug
//
// ## extra 为什么也算敏感输出
//
// extra 目前只放非敏感字段，但它与交易参数（如 fosun 的 apply_account_id）
// 同源，且调用方拿到后若顺手整体回传就等于绕过了「extra 单独有读路径」的设计。
// 这里一并返回是为了省一次查询 —— 使用面越小越好。
func (d *DB) GetMarketCredentialSecrets(id int64) (apiKey, apiSecret, passphrase string, extra map[string]string, err error) {
	var extraRaw []byte
	row := d.QueryRow(`SELECT COALESCE(api_key,''),COALESCE(api_secret,''),COALESCE(passphrase,''),COALESCE(extra,'{}')
FROM market_credentials WHERE id=$1`, id)
	err = row.Scan(&apiKey, &apiSecret, &passphrase, &extraRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", "", nil, ErrCredentialNotFound
	}
	if err != nil {
		return "", "", "", nil, err
	}
	if len(extraRaw) > 0 {
		// 解析失败不阻断建连：extra 是附加信息，凭据本身（token）才是关键。
		_ = json.Unmarshal(extraRaw, &extra)
	}
	return apiKey, apiSecret, passphrase, extra, nil
}

// MaskAPIKey 生成脱敏 hint（前 4 + *** + 后 4；长度 <= 8 时全 ***）。
//
// # 为什么保留前后 4 位
//
// 用户要能区分「我填的是哪个 key」—— 只显示 *** 无法分辨两个不同的 key。
// 前后各 4 位足够辨认，同时不足以还原。
//
// 长度 <= 8 时全打码：短 key 露出首尾 4 位就等于露出大半。
func MaskAPIKey(k string) string {
	if k == "" {
		return ""
	}
	if len(k) <= 8 {
		return "***"
	}
	return k[:4] + "***" + k[len(k)-4:]
}

// ErrCredentialLabelConflict 表示改标签时撞上了另一条记录。
var ErrCredentialLabelConflict = errors.New("该 provider + 账户标签已被另一条凭证占用")

// ErrCredentialProviderImmutable 表示试图改 provider。
//
// # 为什么 provider 不可改
//
// 换 provider 等于换一条记录：字段集合不同（okx 三元组 vs fosun 两个 PEM）、
// 验证方式不同（okx 有 CLI 验证，fosun 没有）、甚至存储的语义都不同
// （api_key 在 okx 是 API Key，在 fosun 是 PEM 公钥）。
//
// 「改 provider」在用户语义里其实是「删掉这条，新建一条」。
// 允许原地改会让已存的密码学材料与新的字段语义错配 —— 那是数据损坏。
var ErrCredentialProviderImmutable = errors.New("凭证的 provider 不可修改（请删除后重建）")

// ErrCredentialKindImmutable 表示试图修改凭证用途（kind）。
//
// # 为什么 kind 也不可改（与 provider 同理但理由更具体）
//
// kind 决定这条凭据的**字段布局**：trade 要 passphrase，data 不要。
// 「改 kind」等于「用 A 类型的字段去填 B 类型的凭据」——
// 而校验会通过（字段都满足新 kind 的 required），但数据是错的。
//
// 用户语义里的「换用途」其实是「删掉这条，按新用途重建」。
var ErrCredentialKindImmutable = errors.New("凭证的用途（kind）不可修改 —— 如需更换用途，请删除后重建")

// UpdateMarketCredentialByID 按主键更新，允许改 account_label。
//
// # 为什么需要它（而不是只靠 Upsert）
//
// Upsert 用 (provider, account_label) 定位。用户「重命名」时传的是**新 label**，
// 按 label 查会找不到原记录 → 新建一条，旧的还在。
// 用户以为在重命名，结果多出一条重复的、旧的还占着位置。
//
// 按 id 更新解决这个问题：id 是不变的身份，label 只是它的一个属性。
//
// # 三种冲突情形
//
//  1. id 不存在          → ErrCredentialNotFound (404)
//  2. 新 label 撞另一条   → ErrCredentialLabelConflict (409)
//  3. id 就是那条记录的   → 正常更新（label 改成新值）
//
// 第 3 种靠 `id <> $1` 排除自己。
func (d *DB) UpdateMarketCredentialByID(id int64, in MarketCredentialInput) (*MarketCredential, error) {
	if id <= 0 {
		return nil, ErrCredentialNotFound
	}
	provider := strings.TrimSpace(in.Provider)
	if provider == "" {
		return nil, fmt.Errorf("provider 不能为空")
	}
	label := in.AccountLabel

	// provider / kind 不可改：先查原值，不同就拒。
	//
	// 必须在 UPDATE 之前查 —— 否则 UPDATE 已经把值改掉了。
	var oldProvider, oldKind string
	if err := d.QueryRow(`SELECT provider, kind FROM market_credentials WHERE id=$1`, id).
		Scan(&oldProvider, &oldKind); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrCredentialNotFound
		}
		return nil, err
	}
	if oldProvider != provider {
		return nil, ErrCredentialProviderImmutable
	}
	// kind 只在调用方显式传了且不同的时候拒。
	//
	// ⚠️ 为什么不「空串当不同」：空串表示「没提这件事」，
	// 而 Update 的调用方（前端编辑表单）不该被要求重新传 kind。
	// 静默接受空串是安全的 —— 因为 UPDATE 语句里根本没有 kind 列。
	if k := strings.TrimSpace(in.Kind); k != "" && k != oldKind {
		return nil, ErrCredentialKindImmutable
	}

	// 冲突预检：同 provider 下是否已有**另一条**用了这个 label。
	// 不靠 UNIQUE 约束报错的原因是：要给出明确的 409 而不是 500。
	var exists bool
	err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM market_credentials
WHERE provider=$1 AND kind=$2 AND account_label=$3 AND id <> $4)`,
		provider, oldKind, label, id).Scan(&exists)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, ErrCredentialLabelConflict
	}

	keyVal, keyKeep := secretUpdateExpr(in.APIKey)
	secVal, secKeep := secretUpdateExpr(in.APISecret)
	passVal, passKeep := secretUpdateExpr(in.Passphrase)
	if keyKeep {
		keyVal = keepSentinel
	}
	if secKeep {
		secVal = keepSentinel
	}
	if passKeep {
		passVal = keepSentinel
	}
	hint := keepSentinel
	if !keyKeep {
		hint = MaskAPIKey(keyVal)
	}

	// extra：与 Upsert 同一套字段级合并语义（见 mergeExtra 的注释）。
	var extraJSON []byte
	if in.Extra != nil {
		stored, sErr := d.storedExtraByID(id)
		if sErr != nil {
			return nil, sErr
		}
		merged := mergeExtra(stored, in.Extra)
		raw, mErr := marshalExtra(merged, true)
		if mErr != nil {
			return nil, mErr
		}
		extraJSON = raw
	}

	res, err := d.Exec(`
UPDATE market_credentials SET
    provider      = $2,
    account_label = $3,
    api_key       = CASE WHEN $4 = $9 THEN api_key      ELSE $4 END,
    api_secret    = CASE WHEN $5 = $9 THEN api_secret   ELSE $5 END,
    passphrase    = CASE WHEN $6 = $9 THEN passphrase   ELSE $6 END,
    api_key_hint  = CASE WHEN $7 = $9 THEN api_key_hint ELSE $7 END,
    base_url      = COALESCE(NULLIF($8,''), base_url),
    enabled       = COALESCE($10, enabled),
    extra         = COALESCE($11, extra)
WHERE id = $1`,
		id, provider, label, keyVal, secVal, passVal, hint,
		strings.TrimSpace(in.BaseURL), keepSentinel, in.Enabled, extraJSON)
	if err != nil {
		return nil, fmt.Errorf("更新行情凭证失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrCredentialNotFound
	}
	return d.GetMarketCredential(id)
}

// mergeExtra 按**字段级**语义合并 extra 更新。
//
// # 四条规则（完整语义矩阵）
//
//	incoming == nil      → 返回 stored（保留，不动）
//	len(incoming) == 0   → 返回空 map（清空全部）
//	incoming 某 key 非空  → 覆盖该 key，其余保留
//	incoming 某 key 空串  → **从结果里删除该 key**，其余保留
//
// # 为什么是「合并」而不是「整体替换」
//
// 前端有「清除单个字段」的交互。整体替换会让「清空 a 保留 b」无法表达 ——
// 用户只能两个一起清。而现在 fosun 只有一个 extra 字段时看不出问题，
// 等第二个字段出现时这个 bug 才会暴露，而那时没人记得这个决定。
//
// # 为什么空串表示删除，而不是存一个空值
//
// 存入 `""` 与「不存这个键」在读取时都是「没有值」，但后者让 GET 返回的
// extra 更干净（不会出现 `{"apply_account_id": ""}` 这种半残状态）。
// 而且若某天某个 provider 需要「空串是合法值」，那时该用显式标记而不是
// 复用空串语义 —— 混淆两者会让语义再也说不清。
func mergeExtra(stored, incoming map[string]string) map[string]string {
	if incoming == nil {
		return stored
	}
	if len(incoming) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(stored)+len(incoming))
	for k, v := range stored {
		out[k] = v
	}
	for k, v := range incoming {
		if strings.TrimSpace(v) == "" {
			delete(out, k)
		} else {
			out[k] = v
		}
	}
	return out
}

// storedExtraFor 读出某条凭证当前的 extra。
//
// 找不到记录时返回 nil（调用方按「无旧值」处理，等于新建）。
func (d *DB) storedExtraFor(provider, kind, label string) (map[string]string, error) {
	var raw []byte
	err := d.QueryRow(`SELECT COALESCE(extra,'{}') FROM market_credentials
WHERE provider=$1 AND kind=$2 AND account_label=$3`, provider, kind, label).Scan(&raw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		// 旧值解析失败时当作「没有旧值」—— 不阻断保存。
		// 这会让该次保存的 extra 变成「仅含本次传入的键」，是有损的，
		// 但比让用户完全存不进去好（且正常路径下不会发生：extra 由我们自己写入）。
		return nil, nil
	}
	return m, nil
}

// storedExtraByID 按主键读 extra（UpdateByID 用）。
func (d *DB) storedExtraByID(id int64) (map[string]string, error) {
	var raw []byte
	err := d.QueryRow(`SELECT COALESCE(extra,'{}') FROM market_credentials WHERE id=$1`, id).Scan(&raw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrCredentialNotFound
		}
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, nil
	}
	return m, nil
}

// marshalExtra 把合并结果序列化成 JSONB 参数；nil 表示「不传」（保留旧值）。
func marshalExtra(merged map[string]string, incomingProvided bool) ([]byte, error) {
	if !incomingProvided {
		// 调用方完全没提供 extra → 传 NULL 让 SQL 的 COALESCE 保留旧值。
		// 注意这与「合并结果为空」不同：后者要写入 {}（清空）。
		return nil, nil
	}
	raw, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("序列化 extra 失败: %w", err)
	}
	return raw, nil
}
