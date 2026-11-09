// Package intercept implements the user-configurable tool-call interception layer.
// Rules are loaded from the database, cached in memory, and evaluated in priority
// order (highest first) on every PreToolUse event. Three actions are supported:
//
//   - allow: immediately permits the call, skipping lower-priority rules.
//   - deny:  blocks the call and returns a message to the model.
//   - ask:   blocks the call, creates an intercept_pending record, writes an
//     activity to the active conversation, then waits for the user to approve or
//     deny via the /api/intercept/pending/{id}/decide endpoint.
//
// The timeout behaviour is configurable at runtime via SetTimeoutConfig.
package intercept

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
)

// ctxKey is the unexported context key type to avoid collisions.
type ctxKey int

// ConvIDKey stores the active conversation ID in a context.Context so the
// interceptor can associate "ask" pending records with the right conversation.
const ConvIDKey ctxKey = 0

// WithConvID returns a child context carrying convID.
func WithConvID(ctx context.Context, convID int64) context.Context {
	return context.WithValue(ctx, ConvIDKey, convID)
}

// ConvIDFromContext extracts the conversation ID (0 if absent).
func ConvIDFromContext(ctx context.Context) int64 {
	v, _ := ctx.Value(ConvIDKey).(int64)
	return v
}

// taskCtxKey is a separate unexported key type for task context values.
type taskCtxKey int

const (
	taskInfoCtxKey taskCtxKey = 1
	taskEmitCtxKey taskCtxKey = 2
)

type taskCtxInfo struct{ taskID, agentName string }

// WithTaskContext injects task metadata and an emit function into ctx so that
// HandleAsk can tag pending records and write intercept_request activities to
// the task's exploration stream (making them appear inline in session transcripts).
func WithTaskContext(ctx context.Context, taskID, agentName string, emit func(db.Activity)) context.Context {
	ctx = context.WithValue(ctx, taskInfoCtxKey, taskCtxInfo{taskID, agentName})
	if emit != nil {
		ctx = context.WithValue(ctx, taskEmitCtxKey, emit)
	}
	return ctx
}

func taskInfoFromCtx(ctx context.Context) (taskID, agentName string) {
	if v, ok := ctx.Value(taskInfoCtxKey).(taskCtxInfo); ok {
		return v.taskID, v.agentName
	}
	return "", ""
}

func taskEmitFromCtx(ctx context.Context) func(db.Activity) {
	f, _ := ctx.Value(taskEmitCtxKey).(func(db.Activity))
	return f
}

// compiledRule is an InterceptRule with the regex pre-compiled (nil for string rules).
type compiledRule struct {
	db.InterceptRule
	re *regexp.Regexp
}

// pendingManager tracks in-flight "ask" requests via per-request channels.
type pendingManager struct {
	mu sync.Mutex
	ch map[int64]chan bool
}

func newPendingManager() *pendingManager { return &pendingManager{ch: map[int64]chan bool{}} }

func (p *pendingManager) add(id int64) chan bool {
	ch := make(chan bool, 1)
	p.mu.Lock()
	p.ch[id] = ch
	p.mu.Unlock()
	return ch
}

func (p *pendingManager) resolve(id int64, allowed bool) {
	p.mu.Lock()
	ch, ok := p.ch[id]
	delete(p.ch, id)
	p.mu.Unlock()
	if ok {
		ch <- allowed
	}
}

func (p *pendingManager) remove(id int64) {
	p.mu.Lock()
	delete(p.ch, id)
	p.mu.Unlock()
}

// Reviewer runs the LLM fallback judge for one tool call and returns its verdict
// as a Decision (Action ∈ allow|ask|deny; empty Action means the reply could not
// be parsed). It is injected by the server layer so the intercept package stays
// free of any llm dependency. profileID == 0 means "use the active/default profile".
type Reviewer func(ctx context.Context, profileID int64, prompt string, input ReviewInput) (Decision, error)

// Interceptor loads intercept rules from the database and evaluates them on
// tool calls. It is safe for concurrent use.
type Interceptor struct {
	db           *db.DB
	mu           sync.RWMutex
	cached       []compiledRule  // sorted by priority DESC; nil means not loaded yet
	enabledTools map[string]bool // nil means not loaded yet
	pending      *pendingManager
	reviewer     Reviewer // nil = LLM fallback judge not wired

	// 草案路径(EvaluateDraft)的注入点。全部可为 nil,
	// 为 nil 时对应 matcher 报错 → 草案路径 fail-closed。
	// 工具路径(Match)不使用这些字段,行为不受影响。
	nowFn     func() time.Time
	loc       *time.Location
	countFn   CountFunc
	sessionFn SessionFunc
}

// SetReviewer installs the LLM fallback judge callback. Passing nil disables it.
func (i *Interceptor) SetReviewer(r Reviewer) {
	i.mu.Lock()
	i.reviewer = r
	i.mu.Unlock()
}

// defaultEnabledTools is the hard-coded set of tools that enter the intercept
// rule system when no intercept_enabled_tools setting has been saved.
var defaultEnabledTools = []string{
	"Bash", "WebFetch", "web_search",
	"shell_open", "shell_send",
	"Write", "Edit", "MultiEdit",
}

// New creates an Interceptor backed by d. The rule cache is lazy-loaded on
// first use.
func New(d *db.DB) *Interceptor {
	return &Interceptor{db: d, pending: newPendingManager()}
}

// Invalidate clears the in-memory rule cache and the enabled-tools cache.
// The next call to Match or IsToolEnabled will reload from the database.
// Call this after any CRUD operation on rules or tool config.
func (i *Interceptor) Invalidate() {
	i.mu.Lock()
	i.cached = nil
	i.enabledTools = nil
	i.mu.Unlock()
}

func (i *Interceptor) loadLocked() error {
	rules, err := i.db.ListInterceptRules()
	if err != nil {
		return err
	}
	var out []compiledRule
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		cr := compiledRule{InterceptRule: r}
		switch r.MatchType {
		case "regex":
			re, err := regexp.Compile(r.Pattern)
			if err != nil {
				continue // skip rules with bad regex rather than crashing
			}
			cr.re = re
		}
		out = append(out, cr)
	}
	i.cached = out

	// Load enabled-tools set from settings, falling back to hard-coded defaults.
	val, ok, _ := i.db.GetSetting("intercept_enabled_tools")
	if !ok {
		m := make(map[string]bool, len(defaultEnabledTools))
		for _, n := range defaultEnabledTools {
			m[n] = true
		}
		i.enabledTools = m
	} else {
		var names []string
		if json.Unmarshal([]byte(val), &names) != nil {
			i.enabledTools = map[string]bool{}
		} else {
			m := make(map[string]bool, len(names))
			for _, n := range names {
				m[n] = true
			}
			i.enabledTools = m
		}
	}
	return nil
}

func (i *Interceptor) rules() ([]compiledRule, error) {
	i.mu.RLock()
	if i.cached != nil {
		out := i.cached
		i.mu.RUnlock()
		return out, nil
	}
	i.mu.RUnlock()

	i.mu.Lock()
	defer i.mu.Unlock()
	if i.cached != nil {
		return i.cached, nil
	}
	if err := i.loadLocked(); err != nil {
		return nil, err
	}
	return i.cached, nil
}

// IsToolEnabled returns true if the named tool is in the intercept-enabled set
// (i.e. it should enter the rule-matching path). Uses the same double-check lock
// pattern as rules().
func (i *Interceptor) IsToolEnabled(name string) bool {
	i.mu.RLock()
	if i.enabledTools != nil {
		v := i.enabledTools[name]
		i.mu.RUnlock()
		return v
	}
	i.mu.RUnlock()

	i.mu.Lock()
	defer i.mu.Unlock()
	if i.enabledTools == nil {
		_ = i.loadLocked()
	}
	return i.enabledTools[name]
}

// GetEnabledTools returns the ordered list of tool names that are currently
// configured to enter the intercept rule system. When the setting has never been
// saved the hard-coded default list is returned.
func (i *Interceptor) GetEnabledTools() ([]string, error) {
	val, ok, err := i.db.GetSetting("intercept_enabled_tools")
	if err != nil {
		return nil, err
	}
	if !ok {
		out := make([]string, len(defaultEnabledTools))
		copy(out, defaultEnabledTools)
		return out, nil
	}
	var names []string
	if err := json.Unmarshal([]byte(val), &names); err != nil {
		return []string{}, nil
	}
	return names, nil
}

// SetEnabledTools persists the list of tool names that should enter the intercept
// rule system, then invalidates the cache so the next call picks up the new list.
func (i *Interceptor) SetEnabledTools(tools []string) error {
	b, err := json.Marshal(tools)
	if err != nil {
		return err
	}
	if err := i.db.SetSetting("intercept_enabled_tools", string(b)); err != nil {
		return err
	}
	i.Invalidate()
	return nil
}

// Decision is the outcome of a successful rule match.
type Decision struct {
	ModelInput       json.RawMessage
	ModelInputDigest string
	ModelFallback    bool
	RuleName         string
	ConfigDigest     string
	ProfileID        int64
	Action           string // "allow" | "deny" | "ask"
	Message          string
	RuleID           int64
	TimeoutEnabled   bool
	TimeoutSeconds   int
	TimeoutAction    string // "deny" | "allow"
}

// --- LLM fallback judge ---

// Judge settings keys (stored in the settings KV table). See docs §3.
const (
	settingJudgeEnabled          = "llm_judge_enabled"
	settingJudgeProfileID        = "llm_judge_profile_id"
	settingJudgePrompt           = "llm_judge_prompt"
	settingJudgeTimeoutSecs      = "llm_judge_timeout_seconds"
	settingJudgeFailAction       = "llm_judge_fail_action"
	settingJudgeAskTimeoutSecs   = "llm_judge_ask_timeout_seconds"
	settingJudgeAskTimeoutAction = "llm_judge_ask_timeout_action"
)

// Judge default values.
const (
	defaultJudgeTimeoutSecs      = 15
	defaultJudgeFailAction       = "allow"
	defaultJudgeAskTimeoutSecs   = 300
	defaultJudgeAskTimeoutAction = "deny"
)

// JudgeConfig is the resolved LLM-fallback-judge configuration. Prompt is always
// non-empty (falls back to DefaultJudgePrompt).
type JudgeConfig struct {
	Enabled           bool   `json:"enabled"`
	ProfileID         int64  `json:"profile_id"` // 0 = follow active/default
	Prompt            string `json:"prompt"`
	TimeoutSeconds    int    `json:"timeout_seconds"`
	FailAction        string `json:"fail_action"` // allow|ask|deny
	AskTimeoutSeconds int    `json:"ask_timeout_seconds"`
	AskTimeoutAction  string `json:"ask_timeout_action"` // allow|deny
}

// judgeConfig reads the judge configuration from settings, applying defaults for
// missing/invalid keys. Read fresh on each fallback judgement — the LLM call that
// follows dwarfs a few KV reads, and freshness avoids a cache-invalidation path.
func (i *Interceptor) judgeConfig() JudgeConfig {
	c := JudgeConfig{
		Enabled:           i.db.GetBool(settingJudgeEnabled, false),
		ProfileID:         int64(i.getSettingInt(settingJudgeProfileID, 0)),
		TimeoutSeconds:    i.getSettingInt(settingJudgeTimeoutSecs, defaultJudgeTimeoutSecs),
		FailAction:        i.getSettingChoice(settingJudgeFailAction, defaultJudgeFailAction, "allow", "ask", "deny"),
		AskTimeoutSeconds: i.getSettingInt(settingJudgeAskTimeoutSecs, defaultJudgeAskTimeoutSecs),
		AskTimeoutAction:  i.getSettingChoice(settingJudgeAskTimeoutAction, defaultJudgeAskTimeoutAction, "allow", "deny"),
	}
	// Prompt: stored value if non-empty, else the built-in template.
	if v, ok, _ := i.db.GetSetting(settingJudgePrompt); ok && strings.TrimSpace(v) != "" {
		c.Prompt = v
	} else {
		c.Prompt = DefaultJudgePrompt
	}
	if c.TimeoutSeconds <= 0 {
		c.TimeoutSeconds = defaultJudgeTimeoutSecs
	}
	if c.AskTimeoutSeconds <= 0 {
		c.AskTimeoutSeconds = defaultJudgeAskTimeoutSecs
	}
	return c
}

func (i *Interceptor) getSettingInt(key string, def int) int {
	v, ok, err := i.db.GetSetting(key)
	if err != nil || !ok {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return n
}

func (i *Interceptor) getSettingChoice(key, def string, allowed ...string) string {
	v, ok, err := i.db.GetSetting(key)
	if err != nil || !ok {
		return def
	}
	v = strings.TrimSpace(v)
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	return def
}

// GetJudgeConfig returns the resolved judge configuration for the API/UI. Prompt
// is the effective prompt (built-in template when unset), so the UI can prefill.
func (i *Interceptor) GetJudgeConfig() JudgeConfig { return i.judgeConfig() }

// SetJudgeConfig persists the judge configuration. An empty Prompt clears the
// override (the built-in template is used again).
func (i *Interceptor) SetJudgeConfig(c JudgeConfig) error {
	if err := i.db.SetBool(settingJudgeEnabled, c.Enabled); err != nil {
		return err
	}
	if err := i.db.SetSetting(settingJudgeProfileID, strconv.FormatInt(c.ProfileID, 10)); err != nil {
		return err
	}
	// Store the prompt only when it differs from the built-in template, so version
	// updates to DefaultJudgePrompt flow through for users who never customized it.
	promptToStore := ""
	if strings.TrimSpace(c.Prompt) != "" && strings.TrimSpace(c.Prompt) != strings.TrimSpace(DefaultJudgePrompt) {
		promptToStore = c.Prompt
	}
	if err := i.db.SetSetting(settingJudgePrompt, promptToStore); err != nil {
		return err
	}
	if err := i.db.SetSetting(settingJudgeTimeoutSecs, strconv.Itoa(c.TimeoutSeconds)); err != nil {
		return err
	}
	if err := i.db.SetSetting(settingJudgeFailAction, c.FailAction); err != nil {
		return err
	}
	if err := i.db.SetSetting(settingJudgeAskTimeoutSecs, strconv.Itoa(c.AskTimeoutSeconds)); err != nil {
		return err
	}
	if err := i.db.SetSetting(settingJudgeAskTimeoutAction, c.AskTimeoutAction); err != nil {
		return err
	}
	return nil
}

// Judge runs the LLM fallback judge for a tool call that matched no rule. It
// returns (Decision, true) when the judge produced a terminal verdict, or
// (Decision{}, false) when the fallback is disabled or not wired (caller then
// keeps the current behavior: allow). On model error or an unparseable reply it
// falls back to the configured FailAction. Ask verdicts carry the human-approval
// timeout so the existing HandleAsk consumes them unchanged.
func (i *Interceptor) Judge(ctx context.Context, tool string, arguments json.RawMessage) (Decision, bool) {
	cfg := i.judgeConfig()
	i.mu.RLock()
	rv := i.reviewer
	i.mu.RUnlock()
	if !cfg.Enabled || rv == nil {
		return Decision{}, false
	}

	cctx := ctx
	if cfg.TimeoutSeconds > 0 {
		var cancel context.CancelFunc
		cctx, cancel = context.WithTimeout(ctx, time.Duration(cfg.TimeoutSeconds)*time.Second)
		defer cancel()
	}

	input, contextErr := BuildReviewInput(cctx, tool, arguments)
	cfg.Prompt = EffectiveJudgePrompt(cfg.Prompt)
	var out Decision
	var err error
	var modelInput []byte
	if contextErr != nil {
		// Invalid current arguments cannot be reviewed faithfully, regardless of
		// the configured model-failure strategy. A human must resolve the input.
		out = Decision{Action: "ask", ModelFallback: true, Message: "审查上下文不完整，需要人工确认：" + contextErr.Error()}
	} else {
		modelInput, _ = json.Marshal(input)
		out, err = rv(cctx, cfg.ProfileID, cfg.Prompt, input)
	}
	if err != nil {
		out = Decision{ProfileID: out.ProfileID, ModelFallback: true, Action: cfg.FailAction, Message: "模型审批失败,按失败策略处理: " + err.Error()}
	}
	switch out.Action {
	case "allow", "ask", "deny":
		// valid verdict
	default:
		out = Decision{ProfileID: out.ProfileID, ModelFallback: true, Action: cfg.FailAction, Message: "模型输出无法解析,按失败策略处理"}
	}
	// A model verdict never carries a rule; keep RuleID 0 (→ NULL) for history.
	out.RuleID = 0
	if len(modelInput) > 0 {
		out.ModelInput = modelInput
		out.ModelInputDigest = digestInput(modelInput)
	}
	if out.ProfileID != 0 {
		cfg.ProfileID = out.ProfileID
	}
	out.ProfileID = cfg.ProfileID
	configJSON, _ := json.Marshal(cfg)
	out.ConfigDigest = digestInput(configJSON)
	if out.Message == "" {
		out.Message = "[模型] " + judgeActionLabel(out.Action)
	} else if !strings.HasPrefix(out.Message, "[模型]") {
		out.Message = "[模型] " + out.Message
	}
	if out.Action == "ask" {
		out.TimeoutEnabled = true
		out.TimeoutSeconds = cfg.AskTimeoutSeconds
		out.TimeoutAction = cfg.AskTimeoutAction
	}
	return out, true
}

func judgeActionLabel(action string) string {
	switch action {
	case "allow":
		return "放行"
	case "deny":
		return "拦截"
	case "ask":
		return "转人工审批"
	default:
		return action
	}
}

// Match evaluates the rule list (priority DESC) against a tool call.
// Returns (Decision, true) for the first matching enabled rule, or
// (Decision{}, false) if no rule matches.
func (i *Interceptor) Match(toolName string, input []byte) (Decision, bool) {
	rules, err := i.rules()
	if err != nil || len(rules) == 0 {
		return Decision{}, false
	}
	for _, r := range rules {
		// 工具路径 fail-open:matcher 报「无法判定」时按未命中处理,继续试下一条。
		// 这是刻意的 —— 渗透场景误拦会打断探索,存量行为不能改。
		// 与之相反,草案路径(EvaluateDraft)必须 fail-closed,见 matchers.go 注释。
		res, mErr := ruleMatches(context.Background(), r, &Subject{ToolName: toolName, ToolInput: input})
		if mErr != nil || !res.Matched {
			continue
		}
		msg := r.Message
		if msg == "" {
			msg = defaultMessage(r.Action, r.Name)
		}
		configJSON, _ := json.Marshal(r.InterceptRule)
		return Decision{
			RuleName:       r.Name,
			ConfigDigest:   digestInput(configJSON),
			Action:         r.Action,
			Message:        msg,
			RuleID:         r.ID,
			TimeoutEnabled: timeoutEnabledFor(r),
			TimeoutSeconds: timeoutSecondsFor(r),
			TimeoutAction:  r.TimeoutAction,
		}, true
	}
	return Decision{}, false
}

// timeoutEnabledFor 决定一条命中规则是否应带超时。
//
// # 为什么 ask 必须强制 true（而不信任 DB 里的 timeout_enabled）
//
// HandleAsk 在 `!TimeoutEnabled` 时进入**无限期** select（见 intercept.go 的
// "No timeout: wait indefinitely"），唯一能解除的是 pending channel 的决策。
// 而审批 UI（/system/intercept/approvals）已不存在 —— 所以 ask 规则一旦
// timeout_enabled=false，Agent 的工具调用会**永久卡死**，直到任务被取消。
//
// 实测（2026-10-30）：库里 [内置] 草案-日内下单次数 / 单标的集中度近似
// 两条启用中的 ask 规则都是 timeout_enabled=false，且 timeout_seconds=60
// 形同虚设 —— 因为那个分支根本不会读取它。用户在真实下单时就会撞上。
//
// # 为什么与模型判定分支一致
//
// 模型判定的 ask 分支（见上面的 cfg.AskTimeout 处理）无条件置
// TimeoutEnabled = true。没有理由认为「规则拦截」比「模型拦截」更不需要超时。
//
// # 为什么 allow/deny 仍用 DB 值
//
// 那两种动作 HandleAsk 不会被调用（只有 ask 会阻塞等决策），
// 所以它们的 timeout 字段无实际作用 —— 保持透传，不改变既有语义。
func timeoutEnabledFor(r compiledRule) bool {
	if r.Action == "ask" {
		return true
	}
	return r.TimeoutEnabled
}

// timeoutSecondsFor 与 timeoutEnabledFor 配套：ask 在没有正数秒数时兜底 60。
//
// 兜底不能是 0 —— `if secs <= 0 { secs = 60 }` 在 HandleAsk 里也有，
// 但那是「已决定要超时」之后的分支；这里补一层是为了让 Decision 本身自洽
// （读 Decision 的人看到 TimeoutSeconds=0 会以为「不超时」）。
func timeoutSecondsFor(r compiledRule) int {
	if r.Action == "ask" && r.TimeoutSeconds <= 0 {
		return 60
	}
	return r.TimeoutSeconds
}

// targetApplies 判定规则的 match_target 是否适用于本次求值的路径。
//
// 返回 false 的语义是「这条规则对本次求值不适用」—— 跳过它,不是命中它。
// 未知 target 不在这里判错:那是 errUnknownMatchTarget 的事,由 ruleMatches
// 的 switch 兜底报错,保证「忘记加 case」不会被静默忽略。
func targetApplies(target string, subj *Subject) bool {
	switch target {
	case "tool_name", "tool_input":
		return subj != nil && (subj.ToolName != "" || len(subj.ToolInput) > 0)
	case "draft_field":
		return subj != nil && subj.Draft != nil
	default:
		// 未知 target:交给 ruleMatches 报 errUnknownMatchTarget。
		// 这里返 true 而不是 false,否则规则会在进入 switch 前就被跳过,
		// 错误永远不会暴露 —— 正是要消灭的静默失效。
		return true
	}
}

// plainSubject 提取 string/regex matcher 的目标文本(保持现有语义)。
func plainSubject(target string, subj *Subject) string {
	if subj == nil {
		return ""
	}
	switch target {
	case "tool_name":
		return subj.ToolName
	case "tool_input":
		return string(subj.ToolInput)
	default:
		return ""
	}
}

// ruleMatches 是规则求值的唯一实现。
//
// G12:matchType 与 matchTarget 两层都必须是 switch。任一层的兜底走
// 子串匹配 / 返 nil,都会让新增类型静默失效(编译通过、不报错、
// 单测测 matcher 函数还全绿,但集成路径根本没被调用)。
//
// 返回值语义:
//   - (res, nil)  → 判定完成,看 res.Matched
//   - (res, err)  → **无法判定**,上层按场景处理:
//     工具路径(Match)fail-open 当作未命中;草案路径(EvaluateDraft)fail-closed 一律 deny。
// subjectForTarget 取出该 match_target 对应的文本输入。
//
// 已知 target 恒返回 (文本, nil);未知 target 返回 errUnknownMatchTarget ——
// 这是「无法判定」而非「未命中」,由 ruleMatches 决定按场景 fail-open/closed。
func subjectForTarget(target string, subj *Subject) (string, error) {
	switch target {
	case "tool_name", "tool_input":
		return plainSubject(target, subj), nil
	case "draft_field":
		// 草案字段没有单一"文本"可比较:string/regex 无法直接作用于它,
		// 只有结构化 matcher 能读 Draft 的具体字段。这里返回空串,
		// 让 ruleMatches 的 matchType switch 决定 —— string/regex 会得到
		// 空串比较(自然不命中),结构化类型走 matchStructured 读 Draft。
		return "", nil
	default:
		return "", fmt.Errorf("%w: %q (rule=%s)", errUnknownMatchTarget, target, ruleNameForErr(subj, target))
	}
}

// ruleNameForErr 仅用于错误文案:把 target 原样带出便于定位。
// 独立成函数是为了让 subjectForTarget 的错误信息不必依赖 compiledRule。
func ruleNameForErr(_ *Subject, target string) string { return target }

func ruleMatches(ctx context.Context, r compiledRule, subj *Subject) (matchResult, error) {
	if !targetApplies(r.MatchTarget, subj) {
		return matchResult{}, nil
	}
	subject, err := subjectForTarget(r.MatchTarget, subj)
	if err != nil {
		return matchResult{}, fmt.Errorf("%w: rule=%s", err, r.Name)
	}
	// 显式 switch,结构上不可能误落"未知类型按 string 处理"。
	switch r.MatchType {
	case "string", "":
		// 空 match_type 是历史数据的默认语义(子串匹配),保留兼容。
		// 这是兼容路径,不是推荐用法 —— 新规则请显式写 match_type。
		return matchResult{Matched: strings.Contains(subject, r.Pattern)}, nil
	case "regex":
		return matchResult{Matched: r.re != nil && r.re.MatchString(subject)}, nil
	case "numeric_lte", "list", "time_window", "daily_count":
		// 新增 matcher 类型在此显式分派。绝不允许把未知类型静默按 "string"
		// 处理 —— 那是 (a) 静默错配。
		params, perr := ParseParams(r.PatternParams)
		if perr != nil {
			return matchResult{}, fmt.Errorf("rule %d params: %w", r.ID, perr)
		}
		return matchStructured(ctx, r.MatchType, params, subj)
	default:
		// 未知 match_type:无法判定,交给调用方按场景处理。
		// 不 panic(DB 里可能存了未来版本的规则),但必须报错而非静默 false。
		return matchResult{}, fmt.Errorf("%w: 未知 match_type=%q (rule=%s)",
			errMatcherNotImplemented, r.MatchType, r.Name)
	}
}

func defaultMessage(action, name string) string {
	switch action {
	case "deny":
		return "拦截规则 [" + name + "] 禁止执行此工具"
	case "ask":
		return "拦截规则 [" + name + "] 要求用户审批，请等待"
	default:
		return ""
	}
}

// Log records an allow/deny rule or model decision into intercept_pending as an ALREADY-decided
// row (status = "allowed" | "denied"), for observability. Unlike HandleAsk it does NOT
// block and needs no user action — it makes explicit review decisions visible on the history
// page (GET /api/intercept/history) and the task's intercept list. Best-effort: a DB
// error is swallowed so logging never changes the tool call's outcome. The pending list
// (status='pending') is unaffected, so it still shows only asks awaiting a decision.
func (i *Interceptor) Log(ctx context.Context, convID int64, dec Decision, toolName string, input []byte, status string) {
	taskID, agentName := taskInfoFromCtx(ctx)
	audit := auditFor(ctx, dec, input, status)
	id, err := i.db.CreateDecidedIntercept(dec.RuleID, convID, taskID, agentName, toolName, input, status, dec.Message, audit)
	if err == nil {
		i.bindResult(ctx, id, audit)
	}
}

// HandleAsk creates a pending approval record and blocks until the user decides
// (via /api/intercept/pending/{id}/decide) or the per-rule timeout elapses.
// Returns true if the user approved.
//
// convID == 0 means no active conversation (background pentest task). The
// pending record is still created (conversation_id = NULL) so the approvals
// page shows it and the sidebar badge lights up. The worker thread blocks just
// like in a chat session — the user must visit the approvals page to unblock it.
func (i *Interceptor) HandleAsk(ctx context.Context, convID int64, dec Decision, toolName string, input []byte) bool {
	ruleID := dec.RuleID
	taskID, agentName := taskInfoFromCtx(ctx)
	taskEmit := taskEmitFromCtx(ctx)

	audit := auditFor(ctx, dec, input, "pending")
	pendingID, err := i.db.CreateInterceptPending(ruleID, convID, taskID, agentName, toolName, input, dec.Message, audit)
	if err != nil {
		return false
	}

	i.bindResult(ctx, pendingID, audit)
	ch := i.pending.add(pendingID)
	defer i.pending.remove(pendingID)
	// Polling clients can decide after INSERT commits but before the channel is
	// registered. Re-read after registration so that decision cannot be lost;
	// later decisions will be delivered through ch.
	if saved, err := i.db.GetInterceptDetail(pendingID); err == nil && saved != nil && saved.Status != "pending" {
		return saved.Status == "allowed" || (saved.Audit != nil && saved.Audit.EffectiveAction == "allow")
	}

	detail, _ := json.Marshal(map[string]any{
		"pending_id": pendingID,
		"tool":       toolName,
		"input":      json.RawMessage(input),
	})
	activity := db.Activity{
		Kind:    "intercept_request",
		Summary: fmt.Sprintf("工具 %s 请求审批 (#%d)", toolName, pendingID),
		Detail:  string(detail),
	}

	if convID != 0 {
		// Chat session: write inline card to the conversation stream.
		_, _ = i.db.AppendConvActivity(convID, activity)
	} else if taskEmit != nil {
		// Task worker: emit to the exploration activity stream so it appears
		// inline in the session transcript (the emit fn stamps NodeID + Worker).
		taskEmit(activity)
	}

	if !dec.TimeoutEnabled {
		// No timeout: wait indefinitely until user decides or worker stops.
		select {
		case allowed := <-ch:
			return allowed
		case <-ctx.Done():
			_, _ = i.db.ResolveIntercept(pendingID, "denied", "deny", "工作已取消")
			_ = i.db.CompleteIntercept(pendingID, audit.RunID, audit.ToolUseID, "not_executed", "执行前工作已取消", false)
			return false
		}
	}

	secs := dec.TimeoutSeconds
	if secs <= 0 {
		secs = 60
	}
	timer := time.NewTimer(time.Duration(secs) * time.Second)
	defer timer.Stop()
	select {
	case allowed := <-ch:
		return allowed
	case <-timer.C:
		allowed := dec.TimeoutAction == "allow"
		action := "deny"
		verb := "拒绝"
		if allowed {
			action = "allow"
			verb = "放行"
		}
		resolved, err := i.db.ResolveIntercept(pendingID, "timeout", action, "审批超时，按超时策略处理")

		// 超时必须留一条用户可见的记录 —— 否则它会变成「静默否决」。
		//
		// # 为什么这比「Agent 卡死」更危险
		//
		// 请求审批时上面已经 emit 了一条 intercept_request，所以用户知道
		// 「有个东西在等我」。
		//
		// 而超时后如果不再 emit，用户看到的是那条「请求审批」**还在**，
		// 于是以为「还在等我」—— 而实际已按 timeout_action 处理完了。
		//
		// 对金融交易，「还在等」和「已拒绝」的用户动作完全不同：
		// 前者等着，后者要重新下单。
		//
		// ⚠️ 当前两条 ask 规则的 timeout_action 都是 deny，
		// 所以每次超时都是「静默拒绝一笔交易」。
		//
		// ⚠️ Kind 用 intercept_timeout 而不是复用 intercept_request ——
		// 前端可能按 Kind 渲染不同样式（超时需要更醒目）。
		timeoutDetail, _ := json.Marshal(map[string]any{
			"pending_id":      pendingID,
			"tool":            toolName,
			"input":           json.RawMessage(input),
			"timeout_seconds": secs,
			"decision":        action,
		})
		timeoutActivity := db.Activity{
			Kind: "intercept_timeout",
			Summary: fmt.Sprintf("审批超时（%d 秒），工具 %s 已按策略%s (#%d)",
				secs, toolName, verb, pendingID),
			Detail: string(timeoutDetail),
		}
		if convID != 0 {
			_, _ = i.db.AppendConvActivity(convID, timeoutActivity)
		} else if taskEmit != nil {
			taskEmit(timeoutActivity)
		}

		if err != nil {
			return false
		}
		if !resolved {
			detail, err := i.db.GetInterceptDetail(pendingID)
			return err == nil && detail != nil && (detail.Status == "allowed" || (detail.Audit != nil && detail.Audit.EffectiveAction == "allow"))
		}
		return allowed
	case <-ctx.Done():
		_, _ = i.db.ResolveIntercept(pendingID, "denied", "deny", "工作已取消")
		_ = i.db.CompleteIntercept(pendingID, audit.RunID, audit.ToolUseID, "not_executed", "执行前工作已取消", false)
		return false
	}
}

var ErrAlreadyDecided = errors.New("审批已处理或不存在，请刷新记录")

// Decide resolves a pending request. Called by the HTTP decide endpoint.
func (i *Interceptor) Decide(pendingID int64, allowed bool) error {
	status := "denied"
	if allowed {
		status = "allowed"
	}
	action, reason := "deny", "人工拒绝执行"
	if allowed {
		action, reason = "allow", "人工允许执行"
	}
	resolved, err := i.db.ResolveIntercept(pendingID, status, action, reason)
	if err != nil {
		return err
	}
	if !resolved {
		return ErrAlreadyDecided
	}
	i.pending.resolve(pendingID, allowed)
	return nil
}

// =====================================================================
// 草案路径求值(EvaluateDraft)—— 与工具路径失败语义相反
// =====================================================================
//
// Match        —— 规则加载失败 / 零规则 / matcher 报错 → 返「未命中」→ 上层放行。
//                 工具路径(渗透场景)需要:误拦会打断探索,试错成本低。
// EvaluateDraft —— 任一步失败 → deny。金融场景放行一个本该拦住的单不可逆。
//
// 这两条**不能统一**。统一到任何一侧都会破坏另一侧。

// ErrNoDraftRules 表示草案规则集为空。
// 空规则 = 规则可能被误清空/误全关 = 不能放行。
var ErrNoDraftRules = errors.New("wind control: 草案规则集为空(draft_field 规则 0 条)")

// EvaluateDraft 是草案路径的唯一求值入口(ADR-05 的 G11 落点)。
//
// 三态聚合:任一 deny → 立即短路 deny;有 ask → 暂存(不短路,要继续收集
// 其他命中项好给人完整理由);全 pass 才 pass。
func (i *Interceptor) EvaluateDraft(ctx context.Context, d *DraftSubject) (Decision, error) {
	rules, err := i.rules()
	if err != nil {
		// 规则集读不到 = 不知道规则是什么 = 不能放行。
		return Decision{
			Action:  "deny",
			Message: "风控规则集加载失败,无法判定,拒绝放行",
		}, fmt.Errorf("evaluate draft: load rules: %w", err)
	}

	// 只取 draft_field 规则。工具类规则(tool_name/tool_input)不适用于草案。
	var applicable []compiledRule
	for _, r := range rules {
		if r.MatchTarget == "draft_field" {
			applicable = append(applicable, r)
		}
	}
	if len(applicable) == 0 {
		return Decision{
			Action:  "deny",
			Message: "风控规则集为空(draft_field 规则 0 条),拒绝放行。请检查规则配置是否被误清空",
		}, ErrNoDraftRules
	}

	subj := &Subject{
		Draft:   d,
		Now:     i.clock(),
		Loc:     i.loc,
		Count:   i.countFn,
		Session: i.sessionFn,
	}

	dec := Decision{Action: "allow"} // 三态聚合起点
	for _, r := range applicable {
		hit, mErr := ruleMatches(ctx, r, subj)
		if mErr != nil {
			// 单条规则无法判定 → 整体 deny(不是跳过这条继续算别的)。
			return Decision{
				Action:  "deny",
				Message: fmt.Sprintf("风控规则 [%s] 求值失败,拒绝放行:%v", r.Name, mErr),
			}, fmt.Errorf("evaluate draft: rule %s: %w", r.Name, mErr)
		}
		if !hit.Matched {
			continue
		}
		// 任一 deny 立即短路。
		if r.Action == "deny" {
			return Decision{
				RuleID: r.ID, RuleName: r.Name, Action: "deny",
				Message: ruleMessage(r),
			}, nil
		}
		// ask 不短路:继续收集,最后若无 deny 则 ask。
		if r.Action == "ask" {
			dec = Decision{RuleID: r.ID, RuleName: r.Name, Action: "ask", Message: ruleMessage(r)}
		}
	}
	return dec, nil
}

// ruleMessage 返回规则的消息;为空时用默认文案。
// 结构化规则可在 pattern_params.disclosure 里补强制披露文案。
func ruleMessage(r compiledRule) string {
	if r.Message != "" {
		return r.Message
	}
	if p, err := ParseParams(r.PatternParams); err == nil && p.Disclosure != "" {
		return p.Disclosure
	}
	return defaultMessage(r.Action, r.Name)
}

// clock 返回注入的时钟;未注入时退回 time.Now。
// 生产环境应通过 SetClock 注入,让测试可确定。
func (i *Interceptor) clock() time.Time {
	if i.nowFn != nil {
		return i.nowFn()
	}
	return time.Now()
}

// SetClock 注入时钟(测试用)。传 nil 恢复 time.Now。
func (i *Interceptor) SetClock(fn func() time.Time) {
	i.mu.Lock()
	i.nowFn = fn
	i.mu.Unlock()
}

// SetLocation 注入草案求值使用的时区。传 nil 时 time_window / daily_count 会报错。
func (i *Interceptor) SetLocation(loc *time.Location) {
	i.mu.Lock()
	i.loc = loc
	i.mu.Unlock()
}

// SetCountFunc 注入 daily_count 的计数来源。传 nil 时该 matcher 报错(fail-closed)。
func (i *Interceptor) SetCountFunc(fn CountFunc) {
	i.mu.Lock()
	i.countFn = fn
	i.mu.Unlock()
}

// SetSessionFunc 注入 time_window(source=market_session) 的时段来源。
// 通常传 agent.SessionAt 的包装。传 nil 时该 matcher 报错(fail-closed)。
func (i *Interceptor) SetSessionFunc(fn SessionFunc) {
	i.mu.Lock()
	i.sessionFn = fn
	i.mu.Unlock()
}
