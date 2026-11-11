package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// Worker is an LLM work agent (docs §4.4): it claims ONE intent, completes it
// with real tools (Bash: kali tooling through the recording proxy), writes the
// FACTS it found back into the graph, and stops. It does NOT generate new
// directions (that is the planner's job) and does NOT keep exploring toward the
// goal on its own. Multiple workers run concurrently as goroutines.
// WebSearchOpts is the web-search backend selection the server pushes into each
// agent (planner/worker/main). Enabled=false leaves the web_search tool off.
// Backend is "ddgs" (no key), "brave-free" (BraveKey required), "tavily"
// (TavilyKey required), or "deepseek" (DeepSeek* required, filled from the
// active LLM profile). It maps directly onto agentcore.Options.
// Proxy is a dedicated egress proxy for the search request (http/https/socks5),
// independent of the traffic-recording MITM proxy — set it when the search endpoint
// is only reachable via a VPN/SOCKS proxy. Empty = direct.
//
// 注意 deepseek 后端与其它三个的性质不同：DeepSeek 没有可直接调用的搜索接口，
// 搜索只存在于其 Anthropic 兼容 messages 接口内部(web_search_20250305 server
// tool)，因此每次搜索会消耗一次模型调用，且搜索请求由 DeepSeek 服务端发出——
// 不经过本机 Proxy，也不会进流量留痕。
type WebSearchOpts struct {
	Enabled   bool
	Backend   string
	BraveKey  string
	TavilyKey string
	Proxy     string
	// DeepSeek* 来自当前激活的 LLM 配置(仅 anthropic 格式的 DeepSeek 官方端点)，
	// 不单独配置，随 LLM 配置切换而变。
	DeepSeekBaseURL string
	DeepSeekAPIKey  string
	DeepSeekModel   string
}

type Worker struct {
	findingRecorder FindingRecorder
	prov            llm.Provider
	model           string
	workDir         string
	proxyAddr       string
	proxyCACert     string            // recording proxy's CA cert path (for WebFetch HTTPS verify)
	webSearch       WebSearchOpts     // web_search tool backend selection (off by default)
	tx              *transcript.Store // raw LLM conversation persistence (nil = off)
	window          int               // context window in tokens (for compaction)
	windowFn        func() int        // optional dynamic task-chain minimum
	maxTurns        int               // max agent turns per run (0 = unlimited)
	// runTimeout is the wall-clock budget for the main exploration of one intent
	// (0 = unlimited). When it fires, the run is cut and a settlement round is
	// forced so already-identified facts get written back instead of being lost.
	runTimeout time.Duration
	// extraTools are host-provided tools (e.g. traffic query, oast) appended to
	// the worker's graph write-back tools.
	extraTools []actool.CoreTool
	// injectConstraints resolves whether this task's operation constraints get
	// injected into the worker system prompt. Read per run so the settings toggle
	// takes effect without rebuilding the agent. nil = inject (default).
	injectConstraints func() bool
	// nonStreamingFn resolves whether this run uses the non-streaming (Complete)
	// path. Read per run so a profile/task toggle takes effect without rebuilding
	// the agent. nil = streaming (default).
	nonStreamingFn func() bool
	// noaEnabledFn resolves whether this run uses the experimental noa context-
	// compression mechanism. Read per run, like nonStreaming. nil = off (built-in
	// compaction).
	noaEnabledFn func() bool
	// maxTokensFn resolves the per-reply output cap in tokens, on the same
	// per-run basis. nil or 0 = send no cap and let the endpoint decide.
	maxTokensFn func() int
}

// WorkerSessionID returns the stable transcript key used by a worker intent.
// Worker slots are reusable, so the intent id (rather than work#N) is the
// session identity. Keep this helper public so the Worker message API and UI
// can refer to exactly the conversation that will be resumed.
func WorkerSessionID(explorationID, intentID int64) string {
	return fmt.Sprintf("exp%d-worker-i%d", explorationID, intentID)
}

const workerChatMarkerPrefix = "<!-- ARTEX_WORKER_CHAT:"

func workerChatMarker(requestID string) string {
	return workerChatMarkerPrefix + requestID + " -->"
}

func hasWorkerChatMessage(messages []llm.Message, requestID string) bool {
	marker := workerChatMarker(requestID)
	for _, message := range messages {
		if message.Role == llm.RoleUser && strings.Contains(message.Text(), marker) {
			return true
		}
	}
	return false
}

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default). Read per
// run so a profile or task-chain toggle takes effect without rebuilding.
func (w *Worker) SetNonStreaming(fn func() bool) { w.nonStreamingFn = fn }

func (w *Worker) nonStreaming() bool { return w.nonStreamingFn != nil && w.nonStreamingFn() }

// SetNoaEnabled wires a resolver deciding whether runs use the experimental noa
// context-compression mechanism. nil/unset = off (built-in compaction). Read per
// run so the settings toggle takes effect without rebuilding the agent.
func (w *Worker) SetNoaEnabled(fn func() bool) { w.noaEnabledFn = fn }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (w *Worker) SetMaxTokens(fn func() int) { w.maxTokensFn = fn }

func (w *Worker) maxTokens() int {
	if w.maxTokensFn == nil {
		return 0
	}
	return w.maxTokensFn()
}

// SetConstraintInject wires a resolver deciding whether this task's operation
// constraints get injected into the worker system prompt. nil = inject (default).
func (w *Worker) SetConstraintInject(fn func() bool) { w.injectConstraints = fn }

// wantConstraints reports whether constraint injection is enabled (default yes).
func (w *Worker) wantConstraints() bool { return w.injectConstraints == nil || w.injectConstraints() }

// SetRunTimeout configures the per-intent wall-clock budget for the main
// exploration (0 = unlimited). When it fires, the SDK settlement phase still runs
// so facts are never lost to a timeout. Safe to call before Execute.
func (w *Worker) SetRunTimeout(run time.Duration) {
	w.runTimeout = run
}

// settleWrapUpPrompt is injected by the SDK settlement phase when a worker hits its
// turn/time budget: stop fetching, write back what was found, then end with a
// plain-text one-liner (which becomes this run's displayed result).
//
// 2026-11-03 金融化：旧文案把 insert_assets 说成登记资产、把 report_finding 说成
// 上报单条缺陷，都是上一代领域措辞。金融 worker 的写回口是 record_fact（分析结论）
// + report_finding（已核实结论/风险点，带证据）。insert_assets 仍保留 —— 它登记的是
// 标的/数据源这类可复用资源，金融也需要。
const settleWrapUpPrompt = "你即将因预算耗尽被终止。不要再运行任何取数/查询。请依次：(1) 把你上面已识别但还没写回的内容逐条写回——新标的/数据源用 insert_assets、分析与结论用 record_fact、已核实且有可引用证据的结论用 report_finding；(2) **最后单独用一句话纯文本**总结你做了什么、得到哪些关键结论（这句会作为本次运行的结果展示，务必输出）。"

func NewWorker(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int, extra ...actool.CoreTool) *Worker {
	return &Worker{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns, extraTools: extra}
}

// defaultToolsExcept returns actool.DefaultTools() minus the named tools (by
// CoreTool.Name()). Used to trim SDK default tools an agent shouldn't have.
func defaultToolsExcept(exclude ...string) []actool.CoreTool {
	drop := make(map[string]bool, len(exclude))
	for _, n := range exclude {
		drop[n] = true
	}
	all := actool.DefaultTools()
	out := make([]actool.CoreTool, 0, len(all))
	for _, t := range all {
		if !drop[t.Name()] {
			out = append(out, t)
		}
	}
	return out
}

func (w *Worker) SetCompactionWindowResolver(fn func() int) { w.windowFn = fn }

func (w *Worker) compactionWindow() int {
	if w.windowFn != nil {
		return w.windowFn()
	}
	return w.window
}

// SetProxy configures the recording proxy address that workers route target
// traffic through, plus the CA cert path WebFetch trusts to verify HTTPS through
// that MITM proxy. Empty addr disables the hint.
func (w *Worker) SetProxy(addr, caCert string) { w.proxyAddr, w.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for this worker (off by default).
func (w *Worker) SetWebSearch(o WebSearchOpts) { w.webSearch = o }

// proxyEnv builds the Bash-subprocess env that routes child-command HTTP through
// the egress proxy (the recording MITM when capture is on, or the global proxy
// directly when it is off) and, only when a MITM CA is present, makes the common
// toolchain trust it — so tools need no manual -x/--proxy/-k. Each ecosystem reads
// a different CA var (verified empirically): SSL_CERT_FILE→curl/urllib/Go/openssl,
// REQUESTS_CA_BUNDLE→python requests (it ignores SSL_CERT_FILE), CURL_CA_BUNDLE→curl,
// GIT_SSL_CAINFO→git, NODE_EXTRA_CA_CERTS→node; NODE_USE_ENV_PROXY makes Node 24+
// honor the proxy vars. ALL_PROXY is set too so a socks5 egress proxy (which curl
// only reads from ALL_PROXY, not HTTP(S)_PROXY) works in the capture-off path.
// Empty proxyAddr → nil (direct, unchanged env).
func proxyEnv(proxyAddr, caCert string) []string {
	if proxyAddr == "" {
		return nil
	}
	env := []string{
		"HTTP_PROXY=" + proxyAddr, "HTTPS_PROXY=" + proxyAddr,
		"http_proxy=" + proxyAddr, "https_proxy=" + proxyAddr,
		"ALL_PROXY=" + proxyAddr, "all_proxy=" + proxyAddr, // socks5 egress: curl reads only this
		"NODE_USE_ENV_PROXY=1", // Node 24+: honor HTTP(S)_PROXY in built-in fetch/http
	}
	if caCert != "" {
		env = append(env,
			"SSL_CERT_FILE="+caCert,
			"CURL_CA_BUNDLE="+caCert,
			"REQUESTS_CA_BUNDLE="+caCert,
			"GIT_SSL_CAINFO="+caCert,
			"NODE_EXTRA_CA_CERTS="+caCert,
		)
	}
	return env
}

// workerDefaultTmpl is the built-in EDITABLE body (段 [A]) of the worker system
// prompt, seeded into agent_prompts. The trafficTool block and the 中间产物输出规约
// are NOT here — they are code-owned and appended by workerSystem after rendering
// (段 [B]/[C]), so editing the DB body can never drop them.
const workerDefaultTmpl = `你是金融交易平台 Terix 的"执行者"(work agent)。你领到【一条意图】(intent：一句话分析方向)，唯一职责：**完成这一条意图、把发现写回知识图谱、然后停止返回。**

**边界（红线）**：
1. **只做你领到的这一条意图**。**执行本意图时若瞥见本意图之外值得深挖的线索**（财报科目的异常口径、与其它标的联动的点、疑似另一条逻辑链的入口），**在 fact 的 summary 里点一句交给规划者**。
2. 初次受阻（接口限流 / 数据缺失 / 字段口径不一致 / 取数报错）不代表已探透——把本意图的替代数据源与替代口径都试完再输出结论；
3. 只在授权范围内操作。系统提示顶部若附【操作约束】，那是最高优先级红线：每次取数/查询执行前先自检，违反即不做（哪怕它落在你领到的意图里）。

**边发现边写回**（写进图才算数，脑子/文字里的不算；每得一个结果立刻写，别攒到最后被步数耗尽丢掉）。三种写回，别串图：
- **新标的/资源 → insert_assets（标的图）**：标的(instrument) / 行情数据源 / 财报科目 / 新闻源 等一切可复用资源【本身】。**这里只登记资源；分析结论/判断不写这里，用 record_fact。**
- **分析结论/事实 → record_fact（探索图，传 intent_id）**：都用它。**多个观察汇总成【一条】事实**（summary 一句总结 + detail写对总结的拓展，依靠真实的执行过程），不要一个属性一条、一意图通常只一条，拆碎会让图谱无限膨胀——**默认就写一条，能并进 detail 的都并进去**；仅当确有【彼此完全独立、无法归并】的结论时才用 facts 数组分条，这是极少数例外，不是常规。**只写增量**：只记这次【新得到】的，别把已有事实换措辞重记（只印证已有、无新增就不必记）。**只写真实看到的**：给 evidence（一行：取数命令/查询+最能证明的一两行结果，简洁，细节在 detail）、标 confidence（observed=直接看到 / inferred=据现象推断）。
- **已核实的分析结论/风险点 → report_finding（探索图，含证据，传 intent_id）**：**只有你本次真实取到过、拿到可引用证据（行情与财报数值、公告原文、来源链接）才用**。严禁把"指标看起来偏离""字段口径看起来异常""外部研报/新闻推断"当已确认，也不要用第三方结论或历史数值替代本次实际取数。验证不了但有嫌疑 → 用 record_fact 记一条 inferred 事实（嫌疑点+为何未验证）交规划者，别硬记成 finding。


完成本意图后用一句话总结你做了什么、写回了哪些事实。`

// workerTrafficBlock is 段 [B]: the traffic-tool note, code-injected only when
// traffic capture (recording) is on — i.e. the traffic_* tools actually exist.
// Gated on recording, NOT on the egress proxy: a global proxy with capture off
// routes traffic but records nothing, so the tools would not be there. Not stored,
// not editable.
//
// # 金融场景为何仍保留
//
// 这是上一代领域（网络侧）的流量录制功能，金融分析用的是结构化行情
// （get_quote/get_kline）。但代码没删：某些自定义工具（HTTP 抓取类）仍可能产出
// 可回看的请求记录，且删它会牵动 tools 表的 traffic_* 绑定。
//
// 实际不会注入 —— 调用点（renderWorkerSystem 段 [B]）的门槛是 caCert != ""，
// 金融部署不配 MITM 证书。所以这里的措辞影响不到金融 agent，保持原样。
func workerTrafficBlock(recording bool) string {
	if !recording {
		return ""
	}
	return "\n\n**流量工具**：\n- traffic_search / traffic_get / traffic_blob：回看响应、找已访问过的资源，**先查流量、不要重复 curl 同一 URL**。traffic_search **必须指定 host**、默认只回 3 条极轻量索引(id/method/url/status/resp_len，无响应内容)，需要更多显式调大 limit；可用 body_contains 在请求/响应正文里做全文搜索(至少 3 字符，支持子串和中文，如找密码/密钥/报错/内网地址)；要看某条原文用 traffic_get(id)，其中超大正文显示为 @blob sha256:<hash>，用 traffic_blob(hash) 分段取全文。"
}

// artifactSpec is 段 [C]: the code-owned, non-editable tail appended to every
// agent's prompt — intermediate artifacts must land in the shared work dir,
// never /tmp. Guaranteed present regardless of how the DB body is edited.
//
// 2026-11-03 金融化：原文举例是「脚本、抓到的响应体」这类抓取侧产物。
// 金融 worker 的中间产物是 K 线数据、财报原文、抓取结果，改成本领域的例子。
func artifactSpec(dir string) string {
	return "\n\n**中间产物输出规约**：脚本、抓取的数据、行情/财报原文、临时结果等一切中间产物，**一律写到本任务工作目录 " + dir + "**（相对路径即写在这里，也可用该绝对路径）——**不要写 /tmp、不要用其它绝对路径**。"
}

// workerArtifactSpec is the worker's 段 [C]: its per-intent run dir is pre-created
// by the engine (ensureRunDir), so it just writes relative paths there — no manual
// mkdir, no cross-worker name collisions.
func workerArtifactSpec(runDir string) string {
	return "\n\n**中间产物输出规约**：脚本、抓取的数据、行情/财报原文、临时结果等一切中间产物，**一律写到本次意图的专属工作目录 " + runDir + "**（已自动建好，直接用相对路径写在这里即可，无需再手动建目录）——**不要写 /tmp、不要用其它绝对路径**。"
}

// ensureRunDir builds and creates an agent's working directory under base:
// <base>/tasks/<taskID> for planner/main; <base>/tasks/<taskID>/i<intentID> for a
// worker (intentID<=0 → task dir only). The "tasks/" segment groups per-task dirs
// symmetrically with the chat agent's "sessions/<sessionID>". Best-effort mkdir — on
// failure, writes fail the same way an unwritable CWD would.
func ensureRunDir(base string, taskID, intentID int64) string {
	dir := filepath.Join(base, "tasks", strconv.FormatInt(taskID, 10))
	if intentID > 0 {
		dir = filepath.Join(dir, "i"+strconv.FormatInt(intentID, 10))
	}
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// cmdOutDir is the SDK large-tool-output spill dir under an agent's run dir.
func cmdOutDir(dir string) string { return filepath.Join(dir, "cmd-output") }

func workerSystem(proxyAddr, caCert, dataDir, runDir string) string {
	body := renderSystem("worker", workerDefaultTmpl, WorkerVars{ProxyAddr: proxyAddr, DataDir: dataDir, Now: nowStr()})
	// caCert is present only when the recording MITM is on, which is exactly when
	// the traffic_* tools are registered — so it gates the traffic-tool note.
	// Optional finding guidance is added for every role after tool resolution.
	return body + workerTrafficBlock(caCert != "") + workerArtifactSpec(runDir)
}

// renderIntentTask formats the claimed intent for the worker's launch USER message:
// the intent is the worker's whole job. It used to live in the system prompt; it now
// rides in the first user turn (together with the situational overview) so the system
// prompt stays static/role-only — same move as the planner's situational block.
// intentAssetIDs pulls the intent's target instrument (asset) ids out of its payload
// (planner's add_intent stores them as a numeric asset_ids array). nil on absence
// or malformed payload.
func intentAssetIDs(intent *db.Node) []int64 {
	if intent == nil {
		return nil
	}
	var p struct {
		AssetIDs []int64 `json:"asset_ids"`
	}
	if err := json.Unmarshal(intent.Payload, &p); err != nil {
		return nil
	}
	return p.AssetIDs
}

// renderIntentTask 渲染「本次唯一的意图」块。
//
// # 为什么按 agentKey 分叉
//
// 这段文案**点名具体工具**。点名一个该角色没有的工具，会让 LLM 尝试调用
// 然后困惑 —— 那是「prompt 与工具面不一致」的最坏形态（工具不在，但
// prompt 说它在，模型没有替代信息可用）。
//
// 而且原文案写的是 record_fact —— **这个工具名根本不存在**
// （recordFact() 是 Go 方法名，注册的工具名是 emit_fact）。
// 无角色路径一直靠「工具列表里有 emit_fact」自己纠正，所以没人发现。
//
// # 为什么通用路径保留两个工具
//
// 未识别角色的语义下两者都用得到：
//   - report_finding = 报告已核实的结论（有严重程度分级、可复核）
//   - emit_fact      = 记录事实（无分级，只是认知）
//
// 金融角色只有一个写回口（emit_fact/emit_thesis/emit_signal/emit_rating），
// 走上面的 emitToolFor 分支，不会到这里。
func renderIntentTask(agentKey string, intent *db.Node) string {
	payload := string(intent.Payload)
	if emitTool := emitToolFor(agentKey); emitTool != "" {
		return fmt.Sprintf(
			"\n\n【你领到的意图（本次唯一任务：只做这一条、做完即停）】：\n%s\n意图 id: %d（写回用 %s，传 intent_id=%d）",
			payload, intent.ID, emitTool, intent.ID)
	}
	// 未识别角色 / 宿生路径：仍给一条通用写回指引。
	//
	// 2026-11-03 金融化：旧文案把 report_finding 说成上报单条缺陷，那是上一代
	// 领域语义。改成「report_finding（已核实的结论/风险点）」—— 底层 finding
	// 记录机制未变（仍是独立表 + 证据字段），只是语义从「缺陷」变为「结论」。
	return fmt.Sprintf(
		"\n\n【你领到的意图（本次唯一任务：只做这一条、只产生事实、做完即停）】：\n%s\n意图 id: %d（写回用 emit_fact（事实）或 report_finding（已核实结论），传 intent_id=%d）",
		payload, intent.ID, intent.ID)
}

// emitToolFor 返回该角色用来写回产出的工具名；未识别角色（无 emit_* 绑定）返回空串。
//
// 与 financialToolBindings[role].Tools 里声明的 emit_* 必须一致 ——
// TestEmitToolMentionedInPromptIsBound 交叉校验两者。
func emitToolFor(agentKey string) string {
	switch {
	case strings.HasPrefix(agentKey, "analyst_"):
		return "emit_fact"
	case strings.HasPrefix(agentKey, "debate_"):
		return "emit_thesis"
	case agentKey == RoleResearchManager:
		return "emit_thesis"
	case strings.HasPrefix(agentKey, "risk_"):
		return "emit_thesis"
	case agentKey == RoleTrader:
		return "emit_signal"
	case agentKey == RolePortfolioManager:
		return "emit_rating"
	}
	return ""
}

// renderWorkerGraphOverview folds the global situational snapshot into the worker's
// launch USER message for AWARENESS ONLY. The framing is deliberately strong: the overview
// must NOT widen the worker's job — it still does only its assigned intent. Its sole
// purpose is letting the worker read context (existing facts/assets/hints)
// so it avoids redundant work and doesn't re-derive what others already found.
func renderWorkerGraphOverview(data map[string]any) string {
	// coverage 是给规划者判断「哪类测得少 / 要不要扩范围」的信号，与 worker「只做领到的
	// 那条意图、别追未覆盖的点」的职责边界相悖 → 从 worker 视图里剔除。data 是本次 worker
	// 专属的新 map，删键不影响 planner。
	delete(data, "coverage")
	b, err := json.Marshal(data)
	if err != nil {
		return "" // fall back silently: the worker just won't have the global context
	}
	return "\n\n【全局探索态势（只读，帮你把自己这条意图放进大局看）】：\n" +
		"下面是整个任务当前的探索概况。用途有两个：一是知道别人已发现什么，别重复；二是让你执行自己这条意图时，能联想到它和全局的关系。\n" +
		"**发散是好事**：执行本意图时尽管深想、多联想。唯一的界线是——别真的动手去执行别的意图（那是别的 worker 的事，由规划者调度）。但凡你联想到有价值的线索（跨标的的联动、疑似另一条逻辑链的入口、全局层面的可疑点），**务必写进 fact 交规划者**——这是你重要的产出，不是可有可无。宁可多报一条让规划者判断，也别自己咽下去。\n" +
		string(b)
}

// Execute runs one intent. hooks (the per-task Guard) gates every tool call; may
// be nil. emit, if non-nil, receives one ActivityRecord per execution step.
// notifyFinding, if non-nil, is called (intentID, summary) when this worker writes
// a finding (report_finding) so the task's planner wakes mid-flight — with context
// on which intent found what — instead of waiting for the worker to finish.
// Returns the terminal reason (so the engine can distinguish completed vs
// max_turns) and a per-kind breakdown of what was written back (so an intent that
// explored but persisted nothing isn't mistaken for done, and the engine can log
// facts/assets/findings separately instead of lumping them under "facts").
func (w *Worker) Execute(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), notifyFinding func(int64, string)) (harness.TerminalReason, WriteCounts, error) {
	return w.execute(ctx, name, taskID, as, ts, intent, hooks, emit, notifyFinding, "", "")
}

// ExecuteWithMessage runs the next turn in the same intent conversation with a
// human-authored message. The HTTP handler does not edit the transcript;
// agentcore records the message as a normal user turn when this Worker starts.
// This keeps Worker continuation identical to the regular agent chat flow.
func (w *Worker) ExecuteWithMessage(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), notifyFinding func(int64, string), requestID, message string) (harness.TerminalReason, WriteCounts, error) {
	return w.execute(ctx, name, taskID, as, ts, intent, hooks, emit, notifyFinding, strings.TrimSpace(requestID), strings.TrimSpace(message))
}

func (w *Worker) execute(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), notifyFinding func(int64, string), requestID, message string) (harness.TerminalReason, WriteCounts, error) {
	tsx := NewToolSet(ts, name)
	tsx.SetFindingRecorder(w.findingRecorder)
	tsx.SetTaskID(taskID)
	coverageEnabled := as == nil || as.CoverageEnabled(taskID)
	tsx.SetCoverageEnabled(coverageEnabled)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetOwnerNode(intent.ID)         // assets this worker discovers anchor to its intent → visible to the task
	tsx.SetNotifyFinding(notifyFinding) // report_finding 落库时当场唤醒 planner，带上「哪个意图+finding」
	// base = built-in worker tools ∪ host tools (traffic) ∪ default tools (incl. Bash);
	// then augment with the agent's visible skills/MCP. During the SDK settlement
	// phase, Bash is hidden via Settlement.DisabledTools (no local gating needed).
	base := append(tsx.WorkerTools(), w.extraTools...)

	// agentKey 决定「加载哪个 prompt」与「过滤哪些工具」。
	//
	// 提到工具组装之前：下面要按它决定给不给 Bash/Write。
	// agentKeyForIntent 是纯函数（读 intent.Payload 里的 role），提前无副作用。
	agentKey := agentKeyForIntent(intent)

	// 判据是「**任务**是否金融」，而不是「**角色**是否金融」。
	//
	// # 2026-11-05 二次事故：只按 role 判断是残缺的
	//
	// 第一次修复只看 isFinancialPrompt(agentKey)，但 planner 自建的意图
	// payload 里**没有 role**（只有 summary）：
	//
	//	{"summary": "打通 OKX 账户通路…安装/定位 OKX CLI…"}
	//
	// 于是 agentKey 落回 "worker" → isFinancialPrompt 为 false →
	// 拿到完整工具集（含 Bash），继续跑 `okx` CLI、继续自己写脚本。
	// 实测该意图的命令：$env:OKX_API_KEY; ... Get-ChildItem Env: | ...
	//
	// 所以判据换成**任务级**：图里任何一个节点带 asset_class，这任务就是金融任务，
	// 它的所有意图（包括 planner 临时加的）都不该有 Bash/Write。
	// asset_class 由 Expand 写入每个阶段节点（pipeline.go 的 basePayload），
	// 是任务级别的稳定事实 —— 比「这条意图有没有 role」可靠得多。
	financeTask := isFinancialPrompt(agentKey) || explorationIsFinancial(ts)

	if financeTask {
		// 金融任务**不给** Bash / Write / Edit。
		//
		// # 为什么（2026-11-05 实测）
		//
		// 通用工具与专用工具并存时，模型会选自由度更高的那个：
		//
		//	work#3 | Write {"content":"import json,urllib.request ... okx.com/api/v5/market/candles"}
		//	work#3 | Bash  {"command":"okx --profile agent account balance USDT --json"}
		//	→ "Error: Not logged in — run okx-auth login first"
		//
		// 第一个后果：自取的数据**不进 snapshots**，emit_fact 的 snapshot_ids
		// 是空的 —— 下游辩论/风控引用的 fact 无法回溯。
		//
		// 第二个后果：`okx` CLI 优先用 ~/.okx/config.toml 的 OAuth 登录，
		// 而我们的 API key 走环境变量注入（SetCredential），两条认证路径不通。
		// 模型撞上 OAuth 报错后会误判「整个 OKX 不可用」，进而放弃或乱试。
		// 实测该错误把 work#3 引去读 .okx/logs 调查了十几轮。
		//
		// # 为什么保留 Read / LS / Sleep
		//
		// Read 读已有报告与笔记，不涉及取数，不破坏证据链。
		// LS 只是一次列目录，Sleep 用于等长任务。三者都无法绕过 get_kline。
		base = append(base, defaultToolsExcept("MultiEdit", "Glob", "Grep", "Bash", "Write", "Edit")...)
	} else {
		// worker 刻意不给 MultiEdit/Glob/Grep：文件精改用 Edit、检索走 Bash(grep/find)，
		// 收敛工具面、减少低价值调用。其余 SDK 默认工具(Read/Write/Edit/LS/Bash/Sleep)照常。
		base = append(base, defaultToolsExcept("MultiEdit", "Glob", "Grep")...)
	}
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts), IntentID: intent.ID})
	// vars 构造器要落 snapshot（captureKlineSnapshot 需要 *db.DB），
	// 而 ts 是 execute 的参数不是 ctx —— 走 ctx 传是零签名改动的做法。
	ctx = WithExplorationStore(ctx, ts)
	// agentKey 已在上面（工具组装前）算出 —— 金融角色拿不到 Bash/Write/Edit。
	//
	// 无 role 的 intent（手工建的 / 旧数据）→ agentKey = "worker" → 走通用
	// worker 路径。金融角色会拿到 §4.3.2 定义的子集。
	tools, def, cleanup := AugmentTools(ctx, agentKey, base)
	defer cleanup()

	// 意图是 worker 的【唯一职责、贯穿整个 run 的不变量】→ 连同启动指令、意图锚定的目标资产
	// 原始数据一起放进 system prompt：system 每次 run 都重新拼一遍、绝不会被 compaction 压掉，
	// 长 run 里意图永远在场，续跑时也不依赖 transcript 历史是否留住那条首消息。代价是 system
	// 混入 per-intent 易变数据、失去跨意图缓存复用；这是刻意的取舍（意图丢失比省 token 严重得多）。
	// 与 planner「态势块放 user turn」分叉是有意的：planner 本身是产意图的那个、没有单一 mandate，
	// worker 有。仅【全局态势 overview】留在启动 user 消息里——它可降级、容忍 stale，压掉无碍。
	// 本次意图的专属工作目录 <workDir>/tasks/<taskID>/i<intentID>，引擎侧先建好。
	runDir := ensureRunDir(w.workDir, taskID, intent.ID)
	// The run-wide intent is not the current tool action. Do not forward it or
	// inherit a parent run's background into the action reviewer.
	ctx = intercept.WithReviewContext(ctx, runDir, intercept.ReviewBackground{})
	sysBody, isRolePrompt := renderRolePrompt(ctx, agentKey, intent)
	// 全局探索态势块只给通用 worker。
	//
	// 金融角色不需要它，而且它会主动误导：
	//   - 内容是探索图概况（标的/事实/意图计数），对金融角色是噪音
	//   - 文案是「疑似另一条线索的入口」「务必写进 fact 交规划者」这类探索语境，
	//     而金融任务没有 planner 在跑（意图是 MaterializeGraph 建好的）
	//   - 它的事实来源是 .AnalystFacts/.Bars，已经在角色 prompt 里了
	var overview string
	if !isRolePrompt {
		overview = renderWorkerGraphOverview(tsx.graphOverviewData())
		sysBody = workerSystem(w.proxyAddr, w.proxyCACert, w.workDir, runDir)
	}
	if w.wantConstraints() {
		// 操作约束(若有)注入系统提示。结尾段的措辞按角色分叉（通用路径说「新标的/
		// 派生方向」，金融角色说「分析维度/data_gap」）。
		sysBody += constraintBlock(ts, agentKey)
	}
	// 意图块 → 意图锚定资产块 → 启动指令，依次追加到 system 尾部（与 constraintBlock 同一套追加法）。
	sysBody += renderIntentTask(agentKey, intent)
	if as != nil {
		if ids := intentAssetIDs(intent); len(ids) > 0 {
			if assets, err := as.GetByIDs(ids); err == nil && len(assets) > 0 {
				if b, err := json.Marshal(assets); err == nil {
					sysBody += "\n\n本意图 asset_ids 对应的目标资产：\n" + string(b)
				}
				// 意图明确针对的这些资产 → 自动纳入任务测试范围（与 insertAssets 同一套
				// 保守粒度）。upsertTaskScope 的 ON CONFLICT DO NOTHING + uq_task_scope
				// 唯一索引保证不会重复添加；重跑/重试同样是幂等 no-op。
				// 资产覆盖度功能关闭时不再累积测试范围(分母)。
				if coverageEnabled {
					for _, a := range assets {
						_ = as.AddAutoScope(taskID, a.Type, a.Domain, a.URL, a.IP)
					}
				}
			}
		}
	}
	sysBody += "\n\n开始执行上面这条意图：只做它、只产生事实、assets、finding、做完即停。"
	system, boundary := deferredSystem(sysBody, def)
	// 任务级 deadline(经 ctx 注入)夹逼本 run 的墙钟预算 + 决定收尾词(见 taskclock.go)。
	tc := taskClockFrom(ctx)
	maxDur, clamped := clampMaxDuration(tc.DeadlineUnix, w.runTimeout)
	settle := wrapupSettlement("worker", []string{"Bash"})
	if tc.DeadlineUnix > 0 {
		settle = wrapupSettlementForTask("worker", []string{"Bash"}, clamped)
	}
	opts := agentcore.Options{
		Provider:        w.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		// WebFetch 走记录代理，其 HTTP 与 curl 一样被留痕；载入代理 CA 让经 MITM
		// 重签的 HTTPS 证书能【正常校验通过】（而非关掉校验）。proxy 空则直连。
		EnableWebFetch: true,
		WebFetchProxy:  w.proxyAddr,
		WebFetchCACert: w.proxyCACert,
		// 联网搜索(可选)。ddgs 无需 key；brave-free 需 BraveKey；tavily 需 TavilyKey。
		// WebSearchProxy 是独立的出口代理(http/https/socks5)，与记录流量的 MITM 代理无关；空则直连。
		EnableWebSearch:       w.webSearch.Enabled,
		WebSearchBackend:      w.webSearch.Backend,
		BraveSearchAPIKey:     w.webSearch.BraveKey,
		TavilySearchAPIKey:    w.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: w.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  w.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   w.webSearch.DeepSeekModel,
		WebSearchProxy:        w.webSearch.Proxy,
		// Bash 子命令的 HTTP 默认走记录代理 + 信任其 CA（工具无需 -x/-k）。
		BashEnv:    proxyEnv(w.proxyAddr, w.proxyCACert),
		WorkingDir: runDir,
		MaxTurns:   w.maxTurns, // 0 = unlimited (configurable in agent management)
		// 墙钟预算,轮边界判,不打断半路;0 = 不限。有任务级 deadline 时夹逼到 min(自身预算,
		// 距 deadline 剩余),让本 run 在任务到点时自然进收尾(见 taskclock.go)。
		MaxDuration: maxDur,
		// 命中预算(轮次 OR 时长)→ SDK 跑一轮收尾(隐藏 Bash),把已识别的写回,避免烂尾。
		// clamped(被任务 deadline 夹逼)时用 PromptByReason:因超时=任务到点→任务超时词,
		// 因步数=夹逼窗口内步数先耗尽→回落 per-run 词。非 clamped 维持纯 per-run。
		Settlement: settle,
		// large tool output spills to cmd-output/ with a head + pointer (SDK tool.Capture);
		// full output preserved on disk. 截断上限用 SDK 默认(30000 字符)。
		ToolOutputDir: cmdOutDir(runDir),
		Compaction:    compactionConfig(w.compactionWindow()), // long tool-heavy runs stay within the window
		Todos:         actool.NewTodoStore(),                  // 会话级临时待办（TodoWrite），纯规划用，退出即丢
		NonStreaming:  w.nonStreaming(),                       // 该 profile 选非流式时走 Provider.Complete
		MaxTokens:     w.maxTokens(),                          // 0 = 不发上限,由服务端默认值决定
	}
	if hooks != nil { // typed-nil guard: only set when concrete (avoids harness panic)
		opts.Hooks = hooks
	}
	if w.tx != nil { // persist raw LLM conversation; one file per worked intent
		opts.Transcript = w.tx
		opts.SessionID = WorkerSessionID(ts.ID(), intent.ID)
	}
	intentID := intent.ID
	emitWrap := func(r db.Activity) {
		if emit != nil {
			r.NodeID, r.Worker = &intentID, name
			emit(r)
		}
	}
	// 意图 / 启动指令 / 意图锚定资产已随 system prompt 下发（见上方 sysBody 组装）。
	// 这条启动 user 消息只承载【全局态势 overview】——可降级的了解大局信息，压掉无碍。
	// overview 罕见地 marshal 失败为空时，回退一句启动词，避免首轮出现空 user 消息。
	input := overview
	if strings.TrimSpace(input) == "" {
		input = "开始执行 system 里领到的意图：只做它、只产生事实、assets、finding、做完即停。"
	}

	// 实验功能:开启后由 noa 接管上下文压缩(归档集中在 <workDir>/noa/<SessionID> 下,持久)。
	noaSession := WorkerSessionID(ts.ID(), intent.ID)
	enableNoa(&opts, w.noaEnabledFn, w.workDir, noaSession, noaWarn(noaSession))
	ctx = attachSideCapture(ctx, &opts)
	s := agentcore.NewSession(opts)
	defer s.Close() // release the session's background-task manager (temp dir + processes)

	// Resume prior conversation if this intent was paused/blocked/exhausted and is
	// being re-run. The transcript ID is deterministic per intent, so if a prior
	// session exists the worker continues from where it left off instead of
	// restarting from scratch.
	alreadyRecorded := false
	if w.tx != nil {
		_ = s.Resume(opts.SessionID)
		alreadyRecorded = requestID != "" && hasWorkerChatMessage(s.Messages(), requestID)
		if len(s.Messages()) > 0 && message == "" {
			seedUnlockFromHistory(s.Messages(), def.UnlockSkill)
			input = "继续执行。"
		} else if len(s.Messages()) > 0 {
			seedUnlockFromHistory(s.Messages(), def.UnlockSkill)
		}
	}
	if message != "" {
		if alreadyRecorded {
			input = "继续执行上一次人工对话输入的新意图。不要重复已经完成的动作。"
		} else if len(s.Messages()) > 0 {
			input = workerChatMarker(requestID) + "\n【人工对话输入的新意图】\n" + message +
				"\n\n请立即按这条人工输入执行，完成后再根据上下文决定原任务是否需要继续。"
		} else {
			input += "\n\n" + workerChatMarker(requestID) + "\n【人工对话输入的新意图】\n" + message +
				"\n\n请优先执行这条人工输入。"
		}
	}

	// Budgets + settlement are owned by the SDK (MaxTurns/MaxDuration + Settlement):
	// on hit it runs a wrap-up turn and finishes with ReasonMaxTurns/ReasonTimeout.
	// MaxDuration now interrupts an in-flight tool at the wall-clock deadline and
	// enters the wrap-up phase on the live ctx, so a run whose tool overran the budget
	// still settles (no external hard-timeout backstop needed). ctx itself carries only
	// pause / planner kill / shutdown, which the engine distinguishes and re-queues/stops.
	_, reason, err := captureRunSession(ctx, s, input, emitWrap)
	return reason, tsx.Writes(), err
}

// explorationIsFinancial 判断这个探索图是不是金融任务。
//
// # 判据
//
// 金融任务的阶段图由 agent.Expand 展开，它给**每个**阶段节点都写了
// payload.asset_class（见 pipeline.go 的 basePayload）。渗透任务没有这个概念，
// 它的意图 payload 里只有 summary 与 asset_ids。
//
// 所以「图里存在 asset_class 非空的 intent 节点」⇔ 金融任务。
//
// # 为什么不用 intent 自己的 payload
//
// planner 自建的意图没有 role，也没有 asset_class —— 它只写 summary。
// 拿意图判断会漏掉整整一类（2026-11-05 实测：planner 加的第 15-19 条意图
// 全部落回 worker 路径，拿到 Bash 后去装 OKX CLI）。
//
// # 失败方向
//
// 查库出错时返回 false（保持旧行为，给完整工具集）。
// 宁可漏判一次（人工看得见 Bash 调用），也不要因为一次查询失败
// 让所有金融任务突然失去必要工具。
func explorationIsFinancial(ts *db.ExplorationStore) bool {
	return assetClassOfGraph(ts) != ""
}

// assetClassOfGraph 返回本探索阶段图的资产类别；空 = 非金融任务。
//
// 判据与 explorationIsFinancial 同源（那函数现在就是它的包装）：
// Expand 把 asset_class 写进**每个**阶段节点的 payload，那是任务级稳定事实。
// 比「这条意图有没有 role」可靠得多 —— planner 自建的意图两者都没有。
//
// # 为什么抽出来
//
// graph_overview 要靠它判断「本任务是不是金融任务、该有哪些角色」，
// 从而报出 stage_graph_missing。两处各写一份解析逻辑必然分叉。
//
// 只扫前 50 条：阶段图节点在任务创建时集中插入、id 靠前，扫太多是浪费。
// 失败返回空串（当作非金融），保持 explorationIsFinancial 原有的失败方向。
func assetClassOfGraph(ts *db.ExplorationStore) string {
	if ts == nil {
		return ""
	}
	nodes, err := ts.ListByKind(db.KindIntent, 50)
	if err != nil {
		return ""
	}
	for _, n := range nodes {
		if len(n.Payload) == 0 {
			continue
		}
		var p map[string]any
		if err := json.Unmarshal(n.Payload, &p); err != nil {
			continue
		}
		if ac, _ := p["asset_class"].(string); strings.TrimSpace(ac) != "" {
			return strings.TrimSpace(ac)
		}
	}
	return ""
}
