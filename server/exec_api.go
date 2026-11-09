package server

// 执行层 API（WE-12）：模式 / 授权 / 解冻 / 执行列表 / 限额，共 6 个端点。
//
// # 为什么这些端点不自己实现状态机
//
// 模式切换的唯一合法路径是 `exec.ApplyTransition` —— 它已包含：
//   - 转换合法性（frozen→live_* 拒绝）
//   - 授权三重一致（channel + account_ref + rules_revision）
//   - 限额完整性
//   - 审计写入（失败即转换失败）
//
// API 层重新实现一遍 = 多一个可能漏掉某条规则的入口。
// 这与「deny 无旁路」是同一条原则：**唯一入口**。
//
// # 错误码与 HTTP 状态的映射
//
// 400 = 参数问题（类型错 / 缺字段 / 值不合法 / confirm_phrase 不匹配）
// 401 = 未认证（由 requireAuth 中间件处理）
// 404 = 资源不存在
// 409 = 状态冲突（当前状态不允许这个操作）—— 客户端刷新后可重试
// 500 = 内部错误
//
// 409 的语义很重要：它表示「请求本身合法，但当前状态不允许」。
// 用 400 会让客户端以为要改请求参数，而实际是要先做别的动作。

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/exec"
)

// 确认短语。**三件事用三个不同的短语** —— 它们确认的内容不同。

// disclaimerPhrases 按版本号映射「授权实盘」的确认短语。
//
// # 为什么是 map 而不是单个常量
//
// 版本升级时旧版仍要能验证 —— 已授权的用户不受影响。
// 只留一个常量的话，改文案会让所有旧版授权失效。
var disclaimerPhrases = map[string]string{
	"v1": "我已知晓实盘交易风险",
}

// liveAutoConfirmPhrase 是「开启实盘自动交易」的确认短语。
//
// # 为什么要与 disclaimerPhrases 分开
//
// 授权 live_manual = 「我接受逐笔确认」
// 授权 live_auto   = 「我接受**无人值守**下单」
//
// 后者放弃了每一笔的人工把关，风险性质不同。用同一句话会让用户
// 以为只是「再确认一次实盘」，而实际是授权系统自己下单。
//
// # 为什么这句话要写明「不再询问」
//
// 用户最容易误判的是「系统会不会还问我一下」。
// 短语本身就把这个预期说死，避免事后争议。
const liveAutoConfirmPhrase = "我确认开启实盘自动交易并接受不再逐笔询问"

// unfreezeConfirmPhrase 是「解除冻结」的确认短语。
//
// 与授权短语分开：解冻只是「恢复可用」，不是「接受实盘风险」。
const unfreezeConfirmPhrase = "我确认解除冻结"

// limitsConfirmPhrase 是「修改交易限额」的确认短语。
//
// # 为什么不复用授权的 disclaimerPhrases
//
// 授权实盘 = 「我接受可能亏钱的后果」
// 改限额   = 「我把单笔上限从 1 万改成 5 万」
//
// 用同一句话会让用户不清楚自己在确认哪一件 ——
// 而「不清楚在确认什么」正是确认短语要避免的：它存在的意义就是让用户看清。
const limitsConfirmPhrase = "我确认修改交易限额"

// execModeDTO 是 GET /api/exec/mode 的响应。
type execModeDTO struct {
	Mode             string `json:"mode"`
	Frozen           bool   `json:"frozen"`
	RulesFingerprint string `json:"rules_fingerprint"`
	LimitsRevision   int64  `json:"limits_revision"`

	// AccountsSummary 只给计数，不给明细。
	//
	// 「有几个账户被冻结」是模式总览的一部分；
	// 「哪个被冻结」是另一个问题（刷新节奏不同 —— 账户状态需要高频轮询，
	// 模式是低频。合成一个请求会让前端为了刷新账户而重查模式）。
	AccountsSummary struct {
		Total   int `json:"total"`
		Blocked int `json:"blocked"`
		Enabled int `json:"enabled"`
	} `json:"accounts_summary"`

	// DefaultAccount 是「授权时该填哪个 account_ref」的权威答案。
	//
	// # 2026-11-05 加：修「授权了却匹配不上」
	//
	// account_ref 是授权表的自然键，必须是账户的真实标识
	// （exec_accounts.channel_account_id）。让用户在输入框手填，
	// 就会填出 "1" 这种与真实标识不符的值 —— 授权落库成功，
	// 但切模式时按真实标识精确匹配，永远查不到。
	//
	// 由后端给出这个值，前端只读展示，从根本上消除手填错误。
	// 没有可用账户时为空（此时授权会被 execAccountRefMatches 放行）。
	DefaultAccount struct {
		Channel string `json:"channel"`
		// AccountRef 是 channel_account_id（券商侧的账户标识）。
		AccountRef string `json:"account_ref"`
		// Label 是给人看的名字，仅用于展示。
		Label string `json:"label"`
	} `json:"default_account"`
}

// pgExecMode 是 GET /api/exec/mode。
func (s *Server) pgExecMode(w http.ResponseWriter, r *http.Request) {
	var out execModeDTO

	mode, err := s.m.pg.GetExecMode()
	if err != nil {
		writeErrCode(w, 500, "读取执行模式失败", "EXEC_MODE_READ_FAILED")
		return
	}
	out.Mode = string(mode)
	out.Frozen = mode == db.ExecModeFrozen

	// 规则指纹：前端用它判断「草案的判定基于哪套规则」。
	//
	// 算不出来时返回错误而不是空串 —— 空串恰好可能是合法值
	// （规则集为空时），前端会误解成「指纹就是空的」。
	if fp, fErr := s.cachedRulesFingerprint(); fErr == nil {
		out.RulesFingerprint = fp
	} else {
		writeErrCode(w, 500,
			fmt.Sprintf("计算规则指纹失败：%v", fErr), "EXEC_FINGERPRINT_FAILED")
		return
	}

	// 限额版本：未配置时留 0。
	if l, lErr := s.m.pg.ExecutionLimits().GetLimits("global"); lErr == nil || errors.Is(lErr, db.ErrLimitNotConfigured) {
		if l != nil {
			out.LimitsRevision = l.Revision
		}
	}

	accts, aErr := s.m.pg.ExecAccounts().ListAccounts()
	if aErr != nil {
		writeErrCode(w, 500, "读取账户列表失败", "EXEC_ACCOUNTS_READ_FAILED")
		return
	}
	// 权威的「授权该填什么」
	if list, lErr := s.m.pg.ExecAccounts().ListAccounts(); lErr == nil {
		for _, a := range list {
			if !a.Active || a.BlockedAt != nil {
				continue
			}
			ref := strings.TrimSpace(a.ChannelAcctID)
			if ref == "" {
				continue
			}
			out.DefaultAccount.Channel = a.Channel
			out.DefaultAccount.AccountRef = ref
			out.DefaultAccount.Label = a.Label
			break
		}
	}

	out.AccountsSummary.Total = len(accts)
	for _, a := range accts {
		if a.BlockedAt != nil {
			out.AccountsSummary.Blocked++
			continue
		}
		if a.Active {
			out.AccountsSummary.Enabled++
		}
	}

	writeJSON(w, 200, out)
}

// pgExecSetMode 是 POST /api/exec/mode。
//
// 请求：{"to": "paper|live_whitelist|live_manual|frozen"}
//
// # 为什么委托给 exec.ApplyTransition
//
// 它是模式切换的唯一入口，已含全部五条硬规则。
// API 层只做「解析请求 + 映射错误码」。
func (s *Server) pgExecSetMode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		To         string `json:"to"`
		Reason     string `json:"reason"`
		Channel    string `json:"channel"`     // 切 live 时必填（用于找授权）
		AccountRef string `json:"account_ref"` // 同上
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErrCode(w, 400, "请求体格式错误", "EXEC_BAD_REQUEST")
		return
	}
	to := strings.TrimSpace(body.To)
	if to == "" {
		writeErrCode(w, 400, "缺少目标模式（to）", "EXEC_BAD_REQUEST")
		return
	}

	cur, err := s.m.pg.GetExecMode()
	if err != nil {
		writeErrCode(w, 500, "读取当前模式失败", "EXEC_MODE_READ_FAILED")
		return
	}

	// 直接切换：不再要求「授权实盘」仪式或完整限额。
	//
	// 2026-11-08 按需求调整：执行模式页选好目标模式点「应用」即生效。
	// ApplyTransition 仍保留合法转换校验（如 frozen → live 拒绝）与审计写入，
	// SkipLiveGuards 只跳过「升级 live 需有效授权 + 限额完整」两条前置。
	// 请求里的 channel / account_ref 仅为兼容旧调用保留，不再参与判定。
	req := exec.TransitionRequest{
		From:           cur,
		To:             db.ExecMode(to),
		SkipLiveGuards: true,
	}

	auditor := &execAuditor{pg: s.m.pg, actor: s.execActor(r)}
	reason := strings.TrimSpace(body.Reason)
	if reason == "" {
		reason = fmt.Sprintf("API 切换模式 %s → %s", cur, to)
	}

	if tErr := exec.ApplyTransition(s.m.pg, auditor, req, reason); tErr != nil {
		code, status := mapExecErr(tErr)
		writeErrCode(w, status, tErr.Error(), code)
		return
	}

	// 返回新状态（让前端不用再请求一次）。
	writeJSON(w, 200, map[string]any{"mode": to})
}

// pgExecAuthorize 是 POST /api/exec/authorize。
//
// 请求：{"channel","account_ref","mode","disclaimer_version","confirm_phrase"}
func (s *Server) pgExecAuthorize(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Channel           string `json:"channel"`
		AccountRef        string `json:"account_ref"`
		Mode              string `json:"mode"`
		DisclaimerVersion string `json:"disclaimer_version"`
		ConfirmPhrase     string `json:"confirm_phrase"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErrCode(w, 400, "请求体格式错误", "EXEC_BAD_REQUEST")
		return
	}

	channel := strings.TrimSpace(body.Channel)
	accountRef := strings.TrimSpace(body.AccountRef)
	mode := strings.TrimSpace(body.Mode)
	version := strings.TrimSpace(body.DisclaimerVersion)

	// ---- 1. channel 合法 ----
	if channel != "fosunwealth" && channel != "okx" {
		writeErrCode(w, 400, `channel 必须是 "fosunwealth" 或 "okx"`, "EXEC_CHANNEL_INVALID")
		return
	}

	// ---- 2. 免责声明版本在白名单 ----
	//
	// live_auto 用独立短语（见 liveAutoConfirmPhrase 的注释）：
	// 授权它等于「授权无人值守」，不是「再确认一次实盘」。
	wantPhrase, ok := disclaimerPhrases[version]
	if mode == string(db.ExecModeLiveAuto) {
		wantPhrase, ok = liveAutoConfirmPhrase, true
	}
	if !ok {
		writeErrCode(w, 400,
			fmt.Sprintf("未知的免责声明版本 %q（当前支持：v1）", version),
			"EXEC_DISCLAIMER_VERSION_UNKNOWN")
		return
	}

	// ---- 3. confirm_phrase 精确匹配 ----
	//
	// # 为什么不 trim / 不忽略空格 / 不用 contains
	//
	// 它的作用是「让用户主动输入一句有语义的话」，而不是「点一下同意」。
	// 允许模糊匹配就退化成复选框了。
	if body.ConfirmPhrase != wantPhrase {
		writeErrCode(w, 400,
			fmt.Sprintf("确认短语不匹配（需要精确输入「%s」）", wantPhrase),
			"EXEC_CONFIRM_MISMATCH")
		return
	}

	// ---- 4. mode 必须是 live_* ----
	if mode != string(db.ExecModeLiveWhitelist) &&
		mode != string(db.ExecModeLiveManual) &&
		mode != string(db.ExecModeLiveAuto) {
		writeErrCode(w, 400,
			"授权只对 live_whitelist / live_manual / live_auto 有意义（paper 不需要授权）",
			"EXEC_AUTH_MODE_INVALID")
		return
	}

	// ---- 5. 限额必须完整 ----
	limits, lErr := s.m.pg.ExecutionLimits().RequireLiveReady("global")
	if lErr != nil {
		writeErrCode(w, 400,
			fmt.Sprintf("交易限额不完整，无法授权实盘：%v", lErr),
			"EXEC_LIMITS_INCOMPLETE")
		return
	}

	// ---- 5b. account_ref 必须与该通道的实际账户标识一致 ----
	//
	// # 为什么放在最后（而不是最前）
	//
	// 前面的校验（channel / 短语 / mode / 限额）都是**更基础**的问题，
	// 用户应当先修它们。若 account_ref 校验放最前，用户输错短语时会看到
	// 「账户标识不匹配」—— 而真正该改的是短语，提示会把人带偏。
	//
	// 排序原则：越靠前的检查，越是「改了才能继续」的前置。
	//
	// # 2026-11-05 修 bug：授权写入时没校验，导致授权永远匹配不上
	//
	// 实测数据：账户表 channel_account_id = "execdemo-account"，
	// 而授权表 account_ref = "1"（用户在输入框手填的）。
	// 后果：
	//   ① 授权写入成功，用户看到「授权完成」
	//   ② 但切模式时用 channel_account_id 精确匹配 → 查不到授权
	//   ③ 报 EXEC_AUTH_STALE / EXEC_AUTH_REQUIRED
	//
	// 「明明授权了却说没授权」是 2026-11-02 线上事故的同款问题，
	// 那时只修了切模式路径（defaultExecAccountRef 回退），
	// **授权写入路径没修** —— 于是新授权继续写错。
	//
	// # 为什么是「拒绝」而不是「静默改成正确的」
	//
	// 授权是一次法律意义上的确认动作。若后端悄悄替换用户输入的账户标识，
	// 用户以为授权给 A，实际授权给 B —— 那是比报错严重得多的问题。
	// 所以这里明确报错，把正确值告诉他，让他重新确认。
	if !s.execAccountRefMatches(channel, accountRef) {
		want, ok := s.canonicalExecAccountRef(channel)
		hint := ""
		if ok {
			hint = fmt.Sprintf("（该通道的正确 account_ref 是 %q）", want)
		}
		writeErrCode(w, 400,
			fmt.Sprintf("account_ref %q 与该通道的实际账户标识不匹配%s", accountRef, hint),
			"EXEC_ACCOUNT_REF_MISMATCH")
		return
	}

	// ---- 6. 写授权（含限额快照 + 规则版本）----
	// 「授权在某个限额版本下生效」—— 限额变了授权就该失效。
	//
	// # from_mode 为什么必须从 settings 读真实值，不能留空
	//
	// 这个字段记录「授权时系统处在哪个执行模式」—— 它是「升档前是什么状态」
	// 的唯一证据。审计时看到 `from_mode=paper` 才能证明这是首次升实盘；
	// 看到 `from_mode=live_whitelist` 才知道是在已授权状态下扩权。
	//
	// 原实现硬编码 `FromMode: ""`，注释写「由调用方在切模式时填」——
	// 但**没有任何调用方填过**（授权是独立动作，不经过切模式路径）。
	// 结果是每次授权都在 Grant 的白名单校验上失败，实盘授权完全不可用：
	//   写授权失败：invalid paper order: 不支持的 from_mode ""
	//
	// 读不到模式时不能退回空串（那会重新踩同一个坑），也不能猜 paper
	// （可能在 live_whitelist 上扩权）。所以用当前真实值，读失败就报错
	// 让用户知道「系统状态读不出来，先别授权」。
	curMode, mErr := s.m.pg.GetExecMode()
	if mErr != nil {
		writeErrCode(w, 500,
			fmt.Sprintf("读取当前执行模式失败，无法记录授权来源：%v", mErr),
			"EXEC_AUTH_MODE_READ_FAILED")
		return
	}

	// 限额快照：必须在此刻固化成 JSON 存进授权行。
	//
	// # 为什么不能留空（2026-11-02 线上事故）
	//
	// 原实现 `LimitsSnapshot: nil`，db 层写进 `{}`。而升级实盘时
	// `LimitsSnapshotMatches({}, 当前限额)` 必然返回 false ——
	// 空快照表示「授权时没配限额」，当前却有序额，判为不一致。
	//
	// 后果：用户授权成功（200），切实盘却永远报
	// `EXEC_AUTH_STALE: 限额配置在授权后被修改过` —— 而用户从没改过限额。
	// 这个流程当时**完全不可用**，且错误信息把责任指向用户。
	//
	// 语义上快照的意义就是「授权在那套限额下生效」。留空等于放弃这层保护，
	// 同时又让校验必然失败 —— 两种坏处都占了。
	snap, sErr := exec.BuildLimitsSnapshot(limits)
	if sErr != nil {
		writeErrCode(w, 500,
			fmt.Sprintf("生成限额快照失败：%v", sErr),
			"EXEC_AUTH_SNAPSHOT_FAILED")
		return
	}

	authID, gErr := s.m.pg.ExecAuthorizations().Grant(db.ExecAuthorizationInput{
		Channel:           channel,
		AccountRef:        accountRef,
		Mode:              mode,
		DisclaimerVersion: version,
		RulesRevision:     limits.Revision,
		ConfirmPhrase:     body.ConfirmPhrase,
		FromMode:          string(curMode),
		LimitsSnapshot:    snap,
		GrantedBy:         s.execActor(r),
	})
	if gErr != nil {
		writeErrCode(w, 500, fmt.Sprintf("写授权失败：%v", gErr), "EXEC_AUTH_WRITE_FAILED")
		return
	}

	// ---- 7. 审计 ----
	if aErr := s.m.pg.WriteExecAudit(nil, db.AuditExecAuthorize, s.execActor(r), map[string]any{
		"channel":            channel,
		"account_ref":        accountRef,
		"mode":               mode,
		"disclaimer_version": version,
		"limits_revision":    limits.Revision,
		"authorization_id":   authID,
	}); aErr != nil {
		// # 为什么授权不能像冻结那样「审计失败就回滚」
		//
		// 冻结有事务（FreezeAccountTx），能回滚；授权是单条 INSERT，已提交。
		//
		// 判据不是「有没有事务」，是「能不能回滚」：
		//   能回滚 → 必须回滚（否则「有状态变更但无痕迹」）
		//   不能回滚 → 把「不完整」明确告诉用户，而不是静默成功
		//
		// # 为什么响应要带 partial + written_id
		//
		// 前端看到 500 通常显示「操作失败，请重试」—— 而那正好是错的
		// （重试会产生第二条授权）。partial:true 让它能显示不同的提示，
		// written_id 让用户能核对到底写进去了哪一条。
		writeJSON(w, 500, map[string]any{
			"error": fmt.Sprintf(
				"授权已写入（id=%d）但审计记录失败：%v —— 请检查审计表，不要重复授权",
				authID, aErr),
			"code":       "EXEC_AUDIT_WRITE_FAILED",
			"partial":    true,
			"written_id": authID,
		})
		return
	}

	writeJSON(w, 200, map[string]any{"authorization_id": authID, "mode": mode})
}

// pgExecUnfreeze 是 POST /api/exec/accounts/{id}/unfreeze。
//
// 路径必须用文档原文 `POST /api/exec/accounts/{id}/unfreeze`。
func (s *Server) pgExecUnfreeze(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	accountID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || accountID <= 0 {
		writeErrCode(w, 400, "账户 id 不合法", "EXEC_BAD_REQUEST")
		return
	}

	var body struct {
		Reason        string `json:"reason"`
		ConfirmPhrase string `json:"confirm_phrase"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErrCode(w, 400, "请求体格式错误", "EXEC_BAD_REQUEST")
		return
	}

	reason := strings.TrimSpace(body.Reason)
	if reason == "" {
		writeErrCode(w, 400, "解冻必须填写原因（reason 不能为空）", "EXEC_REASON_REQUIRED")
		return
	}

	if body.ConfirmPhrase != unfreezeConfirmPhrase {
		writeErrCode(w, 400,
			fmt.Sprintf("确认短语不匹配（需要精确输入「%s」）", unfreezeConfirmPhrase),
			"EXEC_CONFIRM_MISMATCH")
		return
	}

	// ---- 当前必须是 frozen ----
	cur, mErr := s.m.pg.GetExecMode()
	if mErr != nil {
		writeErrCode(w, 500, "读取当前模式失败", "EXEC_MODE_READ_FAILED")
		return
	}
	if cur != db.ExecModeFrozen {
		// 409 而不是 400：请求本身合法，只是当前状态不允许 ——
		// 客户端刷新状态后会看到「本来就没冻结」。
		writeErrCode(w, 409,
			fmt.Sprintf("当前模式是 %s，不是 frozen —— 无需解冻", cur),
			"EXEC_NOT_FROZEN")
		return
	}

	acct, aErr := s.m.pg.ExecAccounts().GetAccount(accountID)
	if aErr != nil {
		if errors.Is(aErr, db.ErrExecAccountNotFound) {
			writeErrCode(w, 404, "账户不存在", "EXEC_ACCOUNT_NOT_FOUND")
			return
		}
		writeErrCode(w, 500, fmt.Sprintf("读取账户失败：%v", aErr), "EXEC_ACCOUNT_READ_FAILED")
		return
	}

	// ---- 先降模式，再解账户 ----
	//
	// # 为什么这个顺序（有安全意义，不只是「语义清晰」）
	//
	// 两步之间失败的中间态：
	//   先降模式后解账户 → 中间态「模式 paper，账户仍 blocked」→ 更保守
	//   先解账户后降模式 → 中间态「账户可用，系统仍 live」   → 更激进
	//
	// 只有「更激进」的中间态需要事务；「更保守」的重试一次就好
	// （用户看到「模式已回 paper 但账户仍 blocked」，再点一次解冻）。
	//
	// # 为什么不加 UnfreezeAccountTx
	//
	// 因为中间态在保守方向 —— 加事务的收益（省一次重试）小于成本
	// （多一个事务函数 + 测试）。
	if _, _, fErr := s.m.pg.ForceSetExecMode(db.ExecModePaper); fErr != nil {
		writeErrCode(w, 500, fmt.Sprintf("降模式失败：%v", fErr), "EXEC_MODE_SET_FAILED")
		return
	}
	if uErr := s.m.pg.ExecAccounts().Unblock(accountID); uErr != nil {
		// 这里已经降了模式但没解账户 —— 中间态在保守方向，可重试。
		writeErrCode(w, 500,
			fmt.Sprintf("模式已降为 paper，但解冻账户失败：%v —— 可重试", uErr),
			"EXEC_UNFREEZE_PARTIAL")
		return
	}

	// ---- 审计 ----
	if wErr := s.m.pg.WriteExecAudit(nil, db.AuditExecUnfreeze, s.execActor(r), map[string]any{
		"account_id":  accountID,
		"channel":     acct.Channel,
		"account_ref": acct.ChannelAcctID,
		"reason":      reason,
	}); wErr != nil {
		// 解冻已生效（模式和账户都改了），审计失败只报错不回滚 ——
		// 因为中间态已过，回滚反而制造「模式 paper 但账户仍 blocked」的不一致。
		writeErrCode(w, 500,
			fmt.Sprintf("解冻已生效但审计失败：%v —— 请人工补记", wErr),
			"EXEC_AUDIT_WRITE_FAILED")
		return
	}

	writeJSON(w, 200, map[string]any{"mode": "paper", "account_id": accountID, "unblocked": true})
}

// pgExecOrders 是 GET /api/exec/orders。
//
// 查询参数：state（逗号分隔）、limit（默认 50，上限 200）。
func (s *Server) pgExecOrders(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	stateParam := strings.TrimSpace(q.Get("state"))
	limit := atoiDefault(q.Get("limit"), 50)
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	// 解析 state 列表。
	var states []string
	if stateParam != "" {
		for _, p := range strings.Split(stateParam, ",") {
			if p = strings.TrimSpace(p); p != "" {
				states = append(states, p)
			}
		}
	}

	var orders []*db.ExecOrder
	var err error
	if len(states) == 0 {
		// 不指定状态：按时间顺序遍历全部在途/终态状态。
		//
		// # 为什么遍历所有状态而不是一条 SQL
		//
		// db 层没有 ListRecent(limit)，所以遍历 7 个状态逐个查再合并。
		// 代价：7 次查询（每次走 idx_exec_state 索引，不是全表扫）。
		//
		// ponytail: 当前量级（单账户订单数十级）下 7 次查询无感。
		//   ⚠️ 前提是「订单数是十级」—— 如果涨到上千，这里会莫名变慢。
		// 升级路径：给 db 层加 ListRecent(limit)，SQL 是
		//   `ORDER BY state_at DESC LIMIT $1`。
		allStates := []string{
			db.ExecStateSubmitted, db.ExecStatePartial, db.ExecStateFilled,
			db.ExecStateCancelled, db.ExecStateRejected, db.ExecStateSettled,
			db.ExecStateCancelRequested,
		}
		for _, st := range allStates {
			list, lErr := s.m.pg.ExecOrders().ListByState(st, limit)
			if lErr != nil {
				err = lErr
				break
			}
			orders = append(orders, list...)
		}
	} else if len(states) == 1 {
		orders, err = s.m.pg.ExecOrders().ListByState(states[0], limit)
	} else {
		// 多状态：逐个查再合并。
		//
		// ponytail: 当前用多次查询 + Go 侧合并（在途单最多几百，够用）。
		// 升级路径：给 db 层加 ListByStates(states []string, limit int)，
		// 用 `WHERE state = ANY($1)` 一次查完。
		for _, st := range states {
			list, lErr := s.m.pg.ExecOrders().ListByState(st, limit)
			if lErr != nil {
				err = lErr
				break
			}
			orders = append(orders, list...)
		}
	}
	if err != nil {
		writeErrCode(w, 500, fmt.Sprintf("查询执行订单失败：%v", err), "EXEC_ORDERS_READ_FAILED")
		return
	}
	if orders == nil {
		orders = []*db.ExecOrder{}
	}

	writeJSON(w, 200, map[string]any{"orders": orders, "count": len(orders)})
}

// pgExecGetLimits 是 GET /api/exec/limits。
func (s *Server) pgExecGetLimits(w http.ResponseWriter, r *http.Request) {
	l, err := s.m.pg.ExecutionLimits().GetLimits("global")
	if err != nil && !errors.Is(err, db.ErrLimitNotConfigured) {
		if errors.Is(err, db.ErrExecutionLimitsNotFound) {
			// 从未配置过：返回空结构而不是 404 ——
			// 前端要能渲染「未配置」的编辑表单。
			writeJSON(w, 200, map[string]any{
				"key": "global", "revision": 0,
				"max_single_amount": nil, "max_daily_amount": nil, "max_daily_orders": nil,
			})
			return
		}
		writeErrCode(w, 500, fmt.Sprintf("读取限额失败：%v", err), "EXEC_LIMITS_READ_FAILED")
		return
	}
	// ExecutionLimits 的 Mode / WhitelistSymbols 已加 json:"-"，
	// 所以这里直接序列化不会泄露废弃字段。
	writeJSON(w, 200, l)
}

// pgExecSetLimits 是 POST /api/exec/limits。
func (s *Server) pgExecSetLimits(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MaxSingleAmount *string `json:"max_single_amount"`
		MaxDailyAmount  *string `json:"max_daily_amount"`
		MaxDailyOrders  *int64  `json:"max_daily_orders"`
		ConfirmPhrase   string  `json:"confirm_phrase"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErrCode(w, 400, "请求体格式错误", "EXEC_BAD_REQUEST")
		return
	}

	if body.ConfirmPhrase != limitsConfirmPhrase {
		writeErrCode(w, 400,
			fmt.Sprintf("确认短语不匹配（需要精确输入「%s」）", limitsConfirmPhrase),
			"EXEC_CONFIRM_MISMATCH")
		return
	}

	// ---- 金额合法性 ----
	if code, msg := validateLimits(body.MaxSingleAmount, body.MaxDailyAmount, body.MaxDailyOrders); code != "" {
		writeErrCode(w, 400, msg, code)
		return
	}

	// ---- 读旧值（before）----
	before, _ := s.m.pg.ExecutionLimits().GetLimits("global")

	// ---- 写入（内部 revision++）----
	// Mode 是 DEPRECATED 字段，但 db 层仍要求合法值（NOT NULL + CHECK）。
	// 传当前执行模式 —— 它不会被使用，只是满足约束。
	curMode, _ := s.m.pg.GetExecMode()
	after, uErr := s.m.pg.ExecutionLimits().UpsertLimits(db.ExecutionLimitsInput{
		Key:             "global",
		Mode:            string(curMode),
		MaxSingleAmount: derefStr(body.MaxSingleAmount),
		MaxDailyAmount:  derefStr(body.MaxDailyAmount),
		MaxDailyOrders:  body.MaxDailyOrders,
	})
	if uErr != nil {
		writeErrCode(w, 500, fmt.Sprintf("写入限额失败：%v", uErr), "EXEC_LIMITS_WRITE_FAILED")
		return
	}

	// ---- 审计：before + after ----
	//
	// # 为什么存 before + after 而不是只存 after
	//
	// 审计的价值在「差异」。只存 after 的话，「上个月是多少」要按时间倒推 N 次
	// 才能回到某个时点；存 before + after 让每次变更自解释。
	detail := map[string]any{
		"before":          limitsSnapshot(before),
		"after":           limitsSnapshot(after),
		"revision_before": revisionOf(before),
		"revision_after":  revisionOf(after),
	}
	if aErr := s.m.pg.WriteExecAudit(nil, db.AuditExecLimitsChange, s.execActor(r), detail); aErr != nil {
		writeErrCode(w, 500,
			fmt.Sprintf("限额已更新（revision=%d）但审计失败：%v —— 请人工补记",
				revisionOf(after), aErr),
			"EXEC_AUDIT_WRITE_FAILED")
		return
	}

	writeJSON(w, 200, after)
}

// ---- 辅助 ----

// validateLimits 校验限额的合法性与自洽性。
//
// 返回 (错误码, 错误信息)；都为空表示通过。
func validateLimits(single, daily *string, orders *int64) (code, msg string) {
	parseAmount := func(s *string) (float64, bool, bool) {
		// 返回 (值, 是否有值, 是否合法)
		if s == nil || strings.TrimSpace(*s) == "" {
			return 0, false, true
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(*s), 64)
		if err != nil {
			return 0, true, false
		}
		return v, true, true
	}

	sv, sHas, sOK := parseAmount(single)
	if !sOK {
		return "EXEC_LIMITS_INVALID", "max_single_amount 不是合法数字"
	}
	dv, dHas, dOK := parseAmount(daily)
	if !dOK {
		return "EXEC_LIMITS_INVALID", "max_daily_amount 不是合法数字"
	}

	// ① 必须有值（live 模式下空值会让 RequireLiveReady 拒绝，但那是在切换时；
	//    这里提前拒绝能给出更早的反馈）。
	if !sHas || !dHas {
		return "EXEC_LIMITS_INCOMPLETE", "max_single_amount 与 max_daily_amount 都必须填写"
	}

	// ② 必须是正数。
	if sv <= 0 {
		return "EXEC_LIMITS_INVALID", "max_single_amount 必须大于 0"
	}
	if dv <= 0 {
		return "EXEC_LIMITS_INVALID", "max_daily_amount 必须大于 0"
	}

	// ③ 单笔不能超日累计。
	//
	// # 为什么必须校验
	//
	// 如果单笔上限 > 日累计上限，那「一笔就超日限」——
	// 用户会以为「单笔能下 10 万」，实际下一笔就被拒。
	// 这不是「配置错了」，是「配置自相矛盾」，必须拒绝。
	if sv > dv {
		return "EXEC_LIMITS_CONTRADICTORY",
			fmt.Sprintf("max_single_amount（%s）不能大于 max_daily_amount（%s）—— 否则一笔就超日限",
				*single, *daily)
	}

	// ④ 日单数必须 >= 1。
	if orders == nil || *orders < 1 {
		return "EXEC_LIMITS_INVALID", "max_daily_orders 必须 >= 1"
	}

	return "", ""
}

// limitsSnapshot 把限额转成审计用的精简快照。
func limitsSnapshot(l *db.ExecutionLimits) map[string]any {
	if l == nil {
		return map[string]any{"configured": false}
	}
	return map[string]any{
		"configured":        true,
		"max_single_amount": l.MaxSingleAmount,
		"max_daily_amount":  l.MaxDailyAmount,
		"max_daily_orders":  l.MaxDailyOrders,
		"revision":          l.Revision,
	}
}

func revisionOf(l *db.ExecutionLimits) int64 {
	if l == nil {
		return 0
	}
	return l.Revision
}

// mapExecErr 把 exec 的结构化错误映射成 (错误码, HTTP 状态)。
func mapExecErr(err error) (code string, status int) {
	var me *exec.ModeError
	if errors.As(err, &me) {
		switch me.Code {
		case exec.CodeModeFrozenRejectsLive:
			return me.Code, 409 // 冻结态不允许切 live —— 状态冲突
		case exec.CodeLimitsIncomplete, exec.CodeAuthRequired, exec.CodeAuthStale,
			exec.CodeModeTransitionInvalid:
			return me.Code, 400 // 参数/前置条件问题
		default:
			return me.Code, 400
		}
	}
	// 非结构化错误 = 内部问题。
	return "EXEC_INTERNAL", 500
}

// execActor 返回操作者标识（用于审计）。
//
// # 为什么是固定值
//
// 本系统是单用户模型：`requireAuth` 只校验一个共享 token，
// 请求里没有「哪个用户」的信息。
//
// 审计里记 "owner" 比记空串好 —— 空串会让「谁做的」这个问题无解，
// 而 "owner" 至少说明了「本机唯一用户」。
//
// ponytail: 将来加多用户时，改成从 context 读用户名。
//
//	那时 requireAuth 要把解析出的用户注入 ctx。
func (s *Server) execActor(_ *http.Request) string {
	return "owner"
}

// anyActiveAuthForChannel 找某个通道下的任一有效授权。
//
// # 安全边界：只在「该通道没有别的歧义」时才用
//
// 最终仍要求 auth.Channel == 请求的 channel —— 所以不会出现
// 「拿 okx 的授权去开 fosunwealth 的实盘」。账号层面放宽是刻意的：
// 一个通道下多账户共用同一份「我已知晓实盘风险」的确认是合理的
// （风险告知与账户无关，与通道有关）。
func (s *Server) anyActiveAuthForChannel(channel string) (*db.ExecAuthorization, bool) {
	if s == nil || s.m == nil || s.m.pg == nil {
		return nil, false
	}
	auths, err := s.m.pg.ExecAuthorizations().ListActive()
	if err != nil {
		return nil, false
	}
	// 只在**该通道恰好一条**有效授权时才回退。
	//
	// # 为什么不能「取第一条匹配的」
	//
	// 踩过：`demo-account` 与 `e2e-acct` 同属 okx，回退拿到的是前者
	// （rev=3），而请求实际针对后者（rev=1）—— 于是报
	// 「授权时 rev=3，当前 rev=1」，用户在两个账户都没改的情况下被判失效。
	//
	// 精确匹配已经在前一步做过了，走到这里说明 account_ref 对不上。
	// 此时若有多条候选，**猜哪一条都是错的** —— 宁可拒绝（让用户重新授权，
	// 那会写一条对齐当前状态的记录），也不要拿着别人的授权放行。
	var hit *db.ExecAuthorization
	n := 0
	for _, a := range auths {
		if a.Channel == channel {
			n++
			if hit == nil {
				hit = a
			}
		}
	}
	if n == 1 {
		return hit, true
	}
	return nil, false
}

// defaultExecAccountRef 推导「当前该用哪个账户」的 (channel, account_ref)。
//
// # 这个函数存在的唯一理由
//
// 授权表里的 account_ref 是**自由文本**（无外键约束），而切模式时需要用它
// 精确匹配。请求没带时，原实现留空 → 必然查不到授权 → 误报「需要先授权」。
//
// 推导规则：取第一条「启用且未冻结」的账户，用它的 channel_account_id
// 作为 account_ref。
//
// # 为什么用 channel_account_id 而不是 id
//
// account_ref 在 Schema 里是 text，语义是「券商侧的账户标识」——
// 就是 channel_account_id（如 OKX 的子账户名、券商的账户号）。
// 用自增 id 会让审计记录失去可读性（"1" 是谁？），且换库后失效。
//
// # 单个都推不出来时返回 false
//
// 让调用方保持「无授权」的原语义 —— 未经授权就该拒绝，不能悄悄放行。
func (s *Server) defaultExecAccountRef() (channel, accountRef string, ok bool) {
	if s == nil || s.m == nil || s.m.pg == nil {
		return "", "", false
	}
	list, err := s.m.pg.ExecAccounts().ListAccounts()
	if err != nil {
		return "", "", false
	}
	for _, a := range list {
		// 冻结的账户不能作为默认目标（它本来就该被拦下）
		if !a.Active || a.BlockedAt != nil {
			continue
		}
		ref := strings.TrimSpace(a.ChannelAcctID)
		if ref == "" {
			continue
		}
		return a.Channel, ref, true
	}
	return "", "", false
}

// derefStr 解引用可空字符串，nil 时返回空串（db 层用它表示 SQL NULL）。
func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(*p)
}

// execAuditor 是 exec.Auditor 的实现，写 exec_audit 表。
//
// # 为什么不在 exec 包里实现
//
// exec 包不该依赖 db 的具体表（它只依赖 db 的仓储方法）。
// 而 WriteExecAudit 是 db 的方法 —— 所以适配器放 server 层。
type execAuditor struct {
	pg    *db.DB
	actor string
}

// WriteModeChange 记录一次模式变更。
func (a *execAuditor) WriteModeChange(from, to db.ExecMode, reason string) error {
	return a.pg.WriteExecAudit(nil, db.AuditExecModeChange, a.actor, map[string]any{
		"from":   string(from),
		"to":     string(to),
		"reason": reason,
	})
}

// fpCache 是规则指纹的短 TTL 缓存（handler 层专享）。
//
// # 为什么需要缓存
//
// 前端每 60s 轮询 GET /api/exec/mode，每次都调 RulesFingerprint，
// 而它要查一次 intercept_rules（可能几十条规则）。
//
// 5 个浏览器标签页 × 每分钟 = 5 次查询/分钟，结果完全一样。
//
// # 为什么 TTL 是 30s 而不是 60s
//
// 60s 会和轮询周期对齐 —— 如果轮询恰好在缓存过期的同一秒，每次都 miss。
// 30s 保证一次轮询至少命中一次缓存。
//
// # ⚠️ 缓存只用于「展示」，不用于判定
//
// 真正做风控判定时（Gate.Authorize），**必须用实时指纹**，不能用缓存。
// 原因：指纹的作用是「检测规则变了吗」。如果用缓存，规则刚改完的 30s 内，
// 闸门会用旧指纹判断「没变」—— 那是安全漏洞。
//
// 所以这里的缓存只影响「GET /mode 返回什么」，不影响任何判定。
type fingerprintCache struct {
	mu      sync.Mutex
	value   string
	expires time.Time
}

const fpCacheTTL = 30 * time.Second

// cachedRulesFingerprint 读指纹，30s 内复用上次的结果。
func (s *Server) cachedRulesFingerprint() (string, error) {
	s.fpMu.Lock()
	defer s.fpMu.Unlock()

	if time.Now().Before(s.fpCache.expires) {
		return s.fpCache.value, nil
	}

	fp, err := exec.NewGate(s.m.pg, nil).RulesFingerprint()
	if err != nil {
		// 失败不写缓存 —— 否则一个瞬时错误会被缓存 30s。
		return "", err
	}
	s.fpCache = fingerprintCache{value: fp, expires: time.Now().Add(fpCacheTTL)}
	return fp, nil
}

// pgExecDraftExecute 是第 7 个端点：执行一条草案（闸门 + 纸面执行）。
//
// # 为什么这是独立于前 6 个的「执行面」
//
// 前 6 个是**管理面**（看模式、切模式、授权、解冻、看单、改限额）——
// 它们改变「系统允许做什么」。
//
// 本端点是**执行面** —— 它真的产生一张执行单（当前是 paper_orders）。
// 两者混在一起会让「管理」和「执行」的权限边界模糊。
//
// # 为什么 account_id 必须显式传，不接受默认
//
// 这是真实资金路径，不该有隐含默认。
// CLI（cmd/execdemo）里可以省 account_id，因为那是开发者工具、本机单账户；
// 而 API 面向真实调用方，默认账户会让「调错了账户」变成静默事故。
//
// # 流程
//
//  1. Gate.Authorize → 拿判定
//  2. allow → ExecutePaper（真正建执行单）
//  3. deny  → 400 + 具体错误码（GateResult.ErrCode 直接透传）
//  4. ask   → 409 + 「需人工确认」
func (s *Server) pgExecDraftExecute(w http.ResponseWriter, r *http.Request) {
	draftID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || draftID <= 0 {
		writeErrCode(w, 400, "draft id 必须是正整数", "EXEC_DRAFT_INVALID_ID")
		return
	}

	var body struct {
		AccountID int64 `json:"account_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErrCode(w, 400, "请求体格式错误："+err.Error(), "EXEC_BAD_BODY")
		return
	}
	// account_id 必填 —— 理由见函数注释。
	if body.AccountID <= 0 {
		writeErrCode(w, 400,
			"account_id 必填且必须为正（执行路径不接受默认账户：调错账户是静默事故）",
			"EXEC_ACCOUNT_REQUIRED")
		return
	}

	// 读当前模式 —— 闸门需要它判断「paper 还是 live」。
	mode, err := s.m.pg.GetExecMode()
	if err != nil {
		writeErrCode(w, 500, "读取执行模式失败："+err.Error(), "EXEC_MODE_READ_FAILED")
		return
	}

	gate := exec.NewGate(s.m.pg, nil)
	res, err := gate.Authorize(r.Context(), exec.GateRequest{
		DraftID:   draftID,
		AccountID: body.AccountID,
		Mode:      mode,
	})
	if err != nil {
		// authorizeInner 只在系统错误时返回 err（判定结果都是 (result, nil)）。
		writeErrCode(w, 500, "闸门判定失败："+err.Error(), "EXEC_GATE_ERROR")
		return
	}

	switch res.Decision {
	case exec.DecisionDeny:
		// 400 + 具体错误码：让调用方能按码分支，而不是匹配文案。
		code := res.ErrCode
		if code == "" {
			code = "EXEC_GATE_DENIED"
		}
		writeJSON(w, 400, map[string]any{
			"error":    res.Reason,
			"code":     code,
			"decision": string(res.Decision),
		})
		return
	case exec.DecisionAsk:
		// 409 —— 「需要人工确认」是状态冲突，不是参数错误。
		//
		// # 为什么是 409 而不是 400
		//
		// 400 意味着「你的请求有问题」，而 409 意味着「请求没问题，但当前状态不允许」。
		// 调用方对两者的处置不同：400 该改参数，409 该等人工或改状态。
		code := res.ErrCode
		if code == "" {
			code = "EXEC_GATE_NEEDS_MANUAL"
		}
		writeJSON(w, 409, map[string]any{
			"error":    res.Reason,
			"code":     code,
			"decision": string(res.Decision),
			"hint":     "该单需人工确认后才能执行",
		})
		return
	case exec.DecisionAllow:
		// 继续往下 —— 真正执行。
	default:
		writeErrCode(w, 500,
			"闸门返回未知判定："+string(res.Decision), "EXEC_GATE_UNKNOWN_DECISION")
		return
	}

	paperID, err := gate.ExecutePaper(r.Context(), draftID, body.AccountID)
	if err != nil {
		// 执行失败不是「判定失败」—— 判定已经 allow 了，是执行的某条前置条件不满足
		// （或 DB 错误）。用 409 而非 500，因为多数是「状态已变」（如草案被别处改了）。
		writeErrCode(w, 409, "执行失败："+err.Error(), "EXEC_EXECUTE_FAILED")
		return
	}

	writeJSON(w, 200, map[string]any{
		"paper_order_id":    paperID,
		"draft_id":          draftID,
		"account_id":        body.AccountID,
		"mode":              string(mode),
		"decision":          string(res.Decision),
		"rules_fingerprint": res.RulesFingerprint,
		"limits_revision":   res.LimitsRevision,
	})
}

// canonicalExecAccountRef 返回某通道第一个可用账户的 channel_account_id。
//
// 用于「授权的 account_ref 该写什么」——它必须与切模式时
// defaultExecAccountRef 推导出来的值一致，否则授权了也匹配不上。
func (s *Server) canonicalExecAccountRef(channel string) (string, bool) {
	if s == nil || s.m == nil || s.m.pg == nil {
		return "", false
	}
	list, err := s.m.pg.ExecAccounts().ListAccounts()
	if err != nil {
		return "", false
	}
	for _, a := range list {
		if a.Channel != channel || !a.Active || a.BlockedAt != nil {
			continue
		}
		if ref := strings.TrimSpace(a.ChannelAcctID); ref != "" {
			return ref, true
		}
	}
	return "", false
}

// execAccountRefMatches 校验 account_ref 是否与该通道的实际账户标识一致。
//
// # 为什么必须精确匹配而不是「包含」或「忽略大小写」
//
// account_ref 是授权表的自然键之一（另一个是 channel）。
// 宽松匹配会让「授权给 A 账户」在改名后仍然生效 —— 那是隐蔽的串账户风险。
//
// 没有任何账户配置时返回 true（放行）：
// 那种情况下用户还没建账户，授权先落下是合理的，
// 后续建账户时应该用同样的标识。
func (s *Server) execAccountRefMatches(channel, accountRef string) bool {
	if s == nil || s.m == nil || s.m.pg == nil {
		return true
	}
	list, err := s.m.pg.ExecAccounts().ListAccounts()
	if err != nil {
		return true // 查不到账户表时不阻断授权（fail-open 在这里安全：授权本身不产生交易）
	}
	hasAny := false
	for _, a := range list {
		if a.Channel != channel {
			continue
		}
		hasAny = true
		if strings.TrimSpace(a.ChannelAcctID) == accountRef {
			return true
		}
	}
	// 该通道下一个账户都没有 → 放行（允许先授权后建账户）
	return !hasAny
}
