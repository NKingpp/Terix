// Package db 是 ARTEX 的数据访问层（SQLite）。
//
// 它打开库文件、应用 schema、seed 内置数据，并在写锁竞争时自动重试。
//
// # 与 PostgreSQL 版的三个关键差异
//
//  1. 没有"建库"步骤：SQLite 的库就是一个文件，只需建父目录。
//  2. schema 与 seed 在**同一个**事务里：单写者模型下，schema 事务持锁期间
//     任何走连接池的写入都会等满 busy_timeout 然后报 BUSY。
//  3. 没有 advisory lock：BEGIN IMMEDIATE 本身就是跨进程互斥。
//
// 并发模型、事务时长的约束与应对见 docs/quantex/ARCH-SQLite-Migration.md §6.4。
package db

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Autumn-27/artex/config"
	_ "modernc.org/sqlite" // driver name is "sqlite"
)

//go:embed schema_sqlite.sql
var schemaSQL string

const (
	// maxOpenConns 是连接池上限。
	//
	// 为什么不是 1：SQLite 虽是单写者，但 MaxOpenConns(1) + 持有一条 *sql.Conn
	// 再走池会**死锁**（实测：Go runtime 直接 "all goroutines are asleep"）。
	// 而启动期正是"握事务做 schema、seed 里再要连接"的模式。
	//
	// 为什么不是 2：schema 事务 + seed 各占一条刚好用满，任何嵌套就挂。
	// 取 8 留余量；WAL 下读并发不受限，写靠 busy_timeout 排队。
	maxOpenConns = 8

	// busyRetryDelays 是 BUSY 重试次数。
	busyRetryDelays = 4
)

// schemaBusyRetryDelays 是退避序列。
//
// busy_timeout 已经承担"等锁"的主要工作，这里的重试只作临界抖动兜底。
var schemaBusyRetryDelays = [...]time.Duration{
	100 * time.Millisecond,
	250 * time.Millisecond,
	500 * time.Millisecond,
	time.Second,
}

// execer 是 *DB 与 *sql.Tx 的公共子集。
//
// seed 函数与 schema 应用都通过它取连接 —— 这样"schema + seed 同一事务"才成立。
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// DB wraps the shared *sql.DB.
//
// SQLite 是单写者模型：同一时刻只有一个写事务。读不受限（WAL）。
// 事务要短、IO 移出事务、BUSY 要重试 —— 详见 ARCH-SQLite-Migration.md §6.4。
type DB struct {
	*sql.DB

	path string // 库文件绝对路径，供日志与备份用
}

// DSN 报告当前数据库位置。
//
// 保留函数名（原 PG 版返回连接串）以不动 server/manager.go 的调用点。
// SQLite 没有连接串，返回的是库文件路径；source 说明它从哪来。
func DSN() (dsn, source string, err error) {
	path, src, err := config.DBPath()
	if err != nil {
		return "", "", err
	}
	return path, src, nil
}

// Path 返回库文件绝对路径（供日志、备份、诊断）。
func (d *DB) Path() string { return d.path }

// Open 打开（必要时创建）SQLite 库，应用 schema，然后 seed。
//
// pathOrDSN 可以是库文件路径，也可以是已拼好的 DSN（BuildDSN 的产物）。
func Open(pathOrDSN string) (*DB, error) {
	dsn, path := resolveDSN(pathOrDSN)
	if err := ensureDataDir(path); err != nil {
		return nil, err
	}

	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开 sqlite（%s）: %w", path, err)
	}
	sqlDB.SetMaxOpenConns(maxOpenConns)
	sqlDB.SetMaxIdleConns(maxOpenConns)
	// 本地文件库没有中间件会断连，不需要生命周期上限。
	sqlDB.SetConnMaxLifetime(0)
	sqlDB.SetConnMaxIdleTime(0)

	if err := sqlDB.Ping(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("连接 sqlite（%s）: %w", path, err)
	}

	d := &DB{DB: sqlDB, path: path}
	if err := d.initSchemaAndSeed(); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return d, nil
}

// initSchemaAndSeed 在**一个** BEGIN IMMEDIATE 事务里跑完 schema + 全部 seed。
//
// 为什么必须同一个事务：
//   - 单写者下 schema 事务持有写锁期间，任何走连接池的写入都会等满
//     busy_timeout 然后失败（实测 5s 后 database is locked）。
//   - BEGIN IMMEDIATE 直接拿 RESERVED 锁，天然把并发启动的第二个实例挡在门外
//     （不需要 advisory lock）。
func (d *DB) initSchemaAndSeed() error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("开启 schema 事务: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // 提交成功后是 no-op

	if err := applySchemaWithRetry(context.Background(), tx); err != nil {
		return fmt.Errorf("应用 schema: %w", err)
	}

	// seed 顺序即依赖顺序：agents 必须先建，agent_prompt_vars 才有 agent_id。
	steps := []struct {
		name string
		fn   func(execer) error
	}{
		{"builtins", seedBuiltins},
		{"draft rules", seedDraftRules},
	}
	for _, s := range steps {
		if err := s.fn(tx); err != nil {
			return fmt.Errorf("seed %s: %w", s.name, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交 schema + seed: %w", err)
	}
	return nil
}

// applySchemaWithRetry 执行 schema 脚本，遇 BUSY 时退避重试。
//
// 注意必须由 tx 驱动：单次 Exec 可含多语句但**不在同一事务内**，
// 中途失败会留下半套表（实测）。所以 schema 由调用方开事务。
func applySchemaWithRetry(ctx context.Context, e execer) error {
	for attempt := 0; ; attempt++ {
		_, err := e.Exec(schemaSQL)
		if err == nil {
			return nil
		}
		if !isBusyFn(err) || attempt >= len(schemaBusyRetryDelays) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(schemaBusyRetryDelays[attempt]):
		}
	}
}

// coordinateWithSchemaMigration 是 PG 版遗留的"长事务与启动 DDL 互斥"钩子。
//
// SQLite 单写者模型下不需要：写事务之间本来就互斥。保留空实现让既有调用点
// （task_archives / task_archives_restore / commands / llm_usage）不用改。
func coordinateWithSchemaMigration(tx *sql.Tx) error { return nil }

// ---------------------------------------------------------------------------
// BUSY 重试包装
// ---------------------------------------------------------------------------

// Exec 在遇到 SQLITE_BUSY 时自动退避重试。
//
// 为什么需要：busy_timeout 只在"等锁"阶段生效；BEGIN IMMEDIATE 阶段撞锁
// 会直接返回 BUSY。现状代码没有重试，迁移后会偶发"随机失败"。
func (d *DB) Exec(query string, args ...any) (sql.Result, error) {
	var res sql.Result
	err := d.withBusyRetry(func() error {
		var e error
		res, e = d.DB.Exec(query, args...)
		return e
	})
	return res, err
}

// Query 在遇到 SQLITE_BUSY 时自动退避重试。
func (d *DB) Query(query string, args ...any) (*sql.Rows, error) {
	var rows *sql.Rows
	err := d.withBusyRetry(func() error {
		var e error
		rows, e = d.DB.Query(query, args...)
		return e
	})
	return rows, err
}

// QueryRow 在遇到 SQLITE_BUSY 时自动退避重试。
//
// QueryRow 的错误延迟到 Scan 才暴露，所以这里只能重试"取行句柄"阶段；
// Scan 阶段的 BUSY 实际几乎不会发生（取行时已拿到锁）。
func (d *DB) QueryRow(query string, args ...any) *sql.Row {
	var row *sql.Row
	_ = d.withBusyRetry(func() error {
		row = d.DB.QueryRow(query, args...)
		return nil
	})
	return row
}

// withBusyRetry 执行 fn，遇 BUSY 时按退避序列重试。
func (d *DB) withBusyRetry(fn func() error) error {
	var err error
	for attempt := 0; attempt <= busyRetryDelays; attempt++ {
		err = fn()
		if err == nil || !isBusyFn(err) {
			return err
		}
		if attempt == busyRetryDelays {
			break
		}
		idx := attempt
		if idx >= len(schemaBusyRetryDelays) {
			idx = len(schemaBusyRetryDelays) - 1
		}
		time.Sleep(schemaBusyRetryDelays[idx])
	}
	return err
}

var (
	_ = errors.Is
	_ = strings.TrimSpace
	_ = slog.Warn
)

// builtinAgent describes one of the fixed agents and its prompt-variable catalog.
type builtinAgent struct {
	key, name, role, desc string
	vars                  []promptVar
	interactiveShell      bool // 建行时的默认交互式 shell 开关；ON CONFLICT 不覆盖用户后续手动开关
	runSeconds            *int // 建行时的单次 run 墙钟上限(秒)；nil=用种子默认(1200)，0=不限时
}

type promptVar struct{ name, desc, example, source string }

// intp 返回 v 的指针，用于给 builtinAgent 可选字段(如 runSeconds)显式取值。
func intp(v int) *int { return &v }

// builtinAgents mirrors docs §5(a). 内置工具不入库；这里只 seed agent + 变量目录。
// 注：planner/worker/mainagent/auto 的交互式 shell 默认由下方 interactive_shell_default_v1
// 块统一置 true（尊重后续 toggle）；这里的 interactiveShell 只给需要「建行即默认开」的新 agent。
var builtinAgents = []builtinAgent{
	{"goals", "目标拆解", "goals", "把渗透任务目标拆解成若干独立、可验证的子目标。", []promptVar{
		{"EngagementDescription", "任务描述（测试对象/背景）", "测试 example.com 站点", "exploration"},
		// Now 是全局 runtime 变量(见 server.globalPromptVars),不再在各 agent 目录里
		// 重复定义,否则 withGlobalVars 追加时会与全局项撞名。
	}, false, nil},
	{"planner", "规划", "planner", "读取态势、判定目标，只在确有未覆盖的新方向时补充探索意图（每任务一个规划循环）。", []promptVar{
		{"Goal", "任务总目标", "拿下 example.com 的管理员权限", "exploration"},
		{"AssetSummary", "资产计数/类型分布摘要(可选)", "domain:3 ip:5 site:2", "distilled"},
	}, false, nil},
	{"mainagent", "主", "main", "人机接口：观察进展，把人的意图落成 hint 或高优先级意图。", []promptVar{
		{"Goal", "当前任务目标", "拿下 example.com 的管理员权限", "exploration"},
		{"AssetSummary", "开局态势摘要(可选)", "domain:3 ip:5", "distilled"},
		{"FindingsSummary", "已确认漏洞摘要(可选)", "high:1 medium:2", "distilled"},
	}, false, nil},
	{"worker", "执行", "worker", "领取一条意图执行，把发现的事实/漏洞写回知识图谱后停止。", []promptVar{
		{"ProxyAddr", "记录代理地址(驱动 if 双文案)", "127.0.0.1:8080", "runtime"},
		{"WorkerName", "worker 自我标识(可选)", "worker-1", "runtime"},
	}, false, nil},
	// Auto:内置「平台操作」agent。不参与渗透编排循环,经对话页驱动,用工具操作平台。
	{"auto", "Auto", "assistant", "平台操作助手：用工具管理任务(建/看/暂停/给提示)与资产，并可创建/修改 skill、自定义工具、MCP。", nil, false, nil},
}

// builtinFinancialAgents 是 W5 金融分析链的 12 个内置角色。
//
// # 为什么必须在这里 seed（而不是只靠 agent/promptcatalog.go）
//
// BuiltinPromptSeeds() 里有这些 key 的默认 prompt，但 server.seedPrompts 是
// 「先按 key 查 agent 行，查不到就跳过」——空库启动时 agents 表里没有这些行，
// 于是一次次打「agent 不存在」的日志跳过，金融链路在新环境根本跑不起来
// （节点有 intent.payload.role，但 GetAgentByKey 拿不到角色配置与 prompt）。
//
// # 为什么 role 是 assistant 而不是 worker
//
// 金融角色的分派靠 intent.payload.role（见 agent/agentKeyForIntent），与 agents.role
// 列无关；schema 的 agents_role_ck 只允许 goals/main/planner/worker/assistant，所以
// 用 assistant —— 它们既不是探索 worker 也不是 planner，是「按角色名分派的独立角色」。
//
// run_seconds=300：金融阶段都是单轮取数+一轮输出，用不到 worker 的 1200s 墙钟。
// 注意 ON CONFLICT 不覆盖 run_seconds / interactive_shell（尊重用户后续调整）。
//
// vars 必须在这里给全：prompt 模板用 {{if}} 包着可选段，但模板引擎是
// missingkey=error —— 目录里缺一个变量，渲染直接失败退回代码默认值，
// 用户在提示词编辑页也看不到/校验不过那个变量（validateTemplate 按目录判定）。
// 所以「模板里出现的变量」= 「这里必须有的一行」。
var builtinFinancialAgents = []builtinAgent{
	// screener 只在任务**未绑标的**时出现在阶段图里（pipeline.go 阶段 0）。
	//
	// 它的工具集与其他角色不同：没有 mx_query/mx_news（选品要批量筛，
	// 妙想的自然语言问句接口一次只能问一个标的，不适合这个场景），
	// 也没有 Write/Edit/Bash（金融角色统一收窄，见 agent/worker.go）。
	{"screener", "选品员", "assistant", "按策略从市场筛选候选标的，产出带数字理由的候选清单与首选。", []promptVar{
		{"DataGaps", "已知情况（无标的时的说明）", `["本任务未指定具体标的"]`, "runtime"},
	}, false, intp(300)},
	{"analyst_market", "技术面分析师", "assistant", "基于日线与技术指标判断趋势、动量、波动与量价配合。", []promptVar{
		{"Instrument", "标的信息（symbol/name/asset_class/market/currency/exchange）", "QQQ / Invesco QQQ / etf / us / USD", "exploration"},
		{"AsOf", "分析基准日（YYYY-MM-DD，之后的数据不可用）", "2026-10-26", "runtime"},
		{"Bars", "日线摘要（根数与覆盖区间）", "20 根日线（2026-09-28 ~ 2026-10-26）", "runtime"},
		{"BarsJSON", "日线全量（JSON 数组，升序）", `[{"trading_date":"2026-10-24","close":"580.12"}]`, "runtime"},
		{"Indicators", "技术指标 JSON（macd/rsi14/atr14/boll/kdj）", `{"macd":1.82,"rsi14":76.4,"atr14":18.62}`, "runtime"},
		{"SnapshotIDs", "证据快照 id 列表", `["42","43"]`, "runtime"},
		{"DataGaps", "本次数据缺口清单", `["news: SOURCE_UNAVAILABLE"]`, "runtime"},
		{"PastContext", "历史反思（W5-1 未实现，当前恒空）", "", "distilled"},
	}, false, intp(300)},
	{"analyst_news", "新闻与宏观分析师", "assistant", "从新闻与宏观数据识别影响标的事件，判断方向与时效。", []promptVar{
		{"Instrument", "标的信息（symbol/name/asset_class/market/currency）", "AAPL / Apple Inc / stock / us / USD", "exploration"},
		{"AsOf", "分析基准日（YYYY-MM-DD，之后发布的新闻不可用）", "2026-10-26", "runtime"},
		{"News", "新闻摘要（条数与来源分布）", "12 条新闻（财新 8 / 财联社 4）", "runtime"},
		{"NewsJSON", "新闻全量（JSON 数组，放末尾完整数据节）", `[{"title":"...","source":"caixin","published":"2026-10-23"}]`, "runtime"},
		{"MXNews", "妙想资讯摘要（条数与类型分布：公告/研报/新闻）", "8 条（公告 2 / 研报 3 / 新闻 3）", "runtime"},
		{"MXNewsJSON", "妙想资讯全量（JSON 数组，含 type/rating/institution，放末尾完整数据节）", `[{"title":"...","type":"NOTICE","date":"2026-10-30"}]`, "runtime"},
		{"Macro", "宏观序列摘要（系列名/单位/最新一期）", "居民消费价格指数（%）最新一期 2026-09 = 3.2", "runtime"},
		{"MacroJSON", "宏观序列全量（JSON，放末尾完整数据节）", `{"series_name":"CPI","points":[{"period":"2026-09","value":"3.2"}]}`, "runtime"},
		{"SnapshotIDs", "证据快照 id 列表", `["61","62"]`, "runtime"},
		{"DataGaps", "本次数据缺口清单", `["个股新闻：NEWS_NO_DATA"]`, "runtime"},
		{"PastContext", "历史反思（reflector 未实现，当前恒空）", "", "distilled"},
	}, false, intp(300)},
	{"analyst_fundamental", "基本面分析师", "assistant", "基于财报数据评估估值与经营质量。", []promptVar{
		{"Instrument", "标的信息（symbol/name/asset_class/market/currency）", "600519 / 贵州茅台 / stock / cn / CNY", "exploration"},
		{"AsOf", "分析基准日（YYYY-MM-DD，之后报告期的财报不可用）", "2026-10-26", "runtime"},
		{"Fundamentals", "财报摘要（行数/期数/最新报告期）", "40 行 × 4 期（最新报告期 2026-09-30）", "runtime"},
		{"FundamentalsJSON", "财报全量（JSON：rows 科目行 × periods 报告期，放末尾完整数据节）", `{"columns":["指标","2026-09-30"],"rows":[{"指标":"净利润","2026-09-30":"1230000000"}]}`, "runtime"},
		{"MXData", "妙想财务数据摘要（张数与标的，东财权威库）", "2 张表（问句：贵州茅台最近三期营业收入…）· 贵州茅台(600519.SH)", "runtime"},
		{"MXDataJSON", "妙想财务全量（JSON：tables[name/entity/columns/units/rows]，放末尾完整数据节）", `{"tables":[{"name":"...","columns":["日期","营业收入"],"rows":[{"日期":"2026-09-30","营业收入":"1652亿元"}]}],"question":"..."}`, "runtime"},
		{"Filings", "申报文件摘要（通道未接入时为显式说明，不是空串）", "（申报文件通道未接入：SEC EDGAR / 巨潮公告均未实现）", "runtime"},
		{"FilingsJSON", "申报文件全量（JSON 数组；未接入时为 []）", "[]", "runtime"},
		{"SnapshotIDs", "证据快照 id 列表", `["71"]`, "runtime"},
		{"DataGaps", "本次数据缺口清单", `["财务数据不可用：FUNDAMENTAL_NO_DATA"]`, "runtime"},
		{"PastContext", "历史反思（reflector 未实现，当前恒空）", "", "distilled"},
	}, false, intp(300)},
	{"analyst_sentiment_crypto", "加密情绪分析师", "assistant", "基于永续合约资金费率与持仓量判断杠杆水平与多空拥挤度。", []promptVar{
		{"Instrument", "标的（symbol/asset_class/market）", "BTC-USDT / crypto / crypto", "exploration"},
		{"AsOf", "基准时刻（加密 7×24 无收盘）", "2026-10-26T14:30:00Z", "runtime"},
		{"Derivatives", "衍生品数据 JSON（funding_rate/open_interest/unavailable 等）", `{"funding_rate":"0.00012","open_interest":"18234567890"}`, "runtime"},
		{"News", "新闻摘要（get_news 未实现，当前恒空）", "", "runtime"},
		{"SnapshotIDs", "证据快照 id 列表", `["51"]`, "runtime"},
		{"DataGaps", "本次数据缺口清单", `["funding_rate: SOURCE_UNAVAILABLE"]`, "runtime"},
		{"PastContext", "历史反思（W5-1 未实现，当前恒空）", "", "distilled"},
	}, false, intp(300)},
	{"debate_bull", "多头研究员", "assistant", "从分析师事实中构造最有力的看多论证，并声明其证伪条件。", []promptVar{
		{"Instrument", "标的", "SPY / SPDR S&P 500 / etf / us", "exploration"},
		{"AnalystFacts", "分析师事实列表（已格式化）", "- RSI=76.4 超买（analyst_market）", "distilled"},
		{"Round", "当前辩论轮次（从 1 开始）", "1", "runtime"},
		{"MaxRounds", "总轮次", "2", "runtime"},
		{"AsOf", "基准时刻", "2026-10-26", "runtime"},
		{"SkippedAnalysts", "按资产类别裁掉的分析师（本就不存在该维度）", `["analyst_fundamental"]`, "runtime"},
		{"PriorRoundSummary", "上一轮辩论摘要（第 1 轮为空）", "", "distilled"},
		{"AnalystFactsJSON", "分析师事实全量（JSON 数组，放末尾完整证据节）", `[{"role":"analyst_market","claim":"RSI=76.4 超买"}]`, "distilled"},
	}, false, intp(300)},
	{"debate_bear", "空头研究员", "assistant", "针对多头论证找逻辑漏洞与证据缺口，构造看空论证或证伪。", []promptVar{
		{"Instrument", "标的", "SPY / SPDR S&P 500 / etf / us", "exploration"},
		{"AnalystFacts", "分析师事实列表（已格式化）", "- RSI=76.4 超买（analyst_market）", "distilled"},
		{"Round", "当前辩论轮次（从 1 开始）", "1", "runtime"},
		{"MaxRounds", "总轮次", "2", "runtime"},
		{"AsOf", "基准时刻", "2026-10-26", "runtime"},
		{"SkippedAnalysts", "按资产类别裁掉的分析师（本就不存在该维度）", `["analyst_fundamental"]`, "runtime"},
		{"PriorRoundSummary", "上一轮辩论摘要（第 1 轮为空）", "", "distilled"},
		{"AnalystFactsJSON", "分析师事实全量（JSON 数组，放末尾完整证据节）", `[{"role":"analyst_market","claim":"RSI=76.4 超买"}]`, "distilled"},
	}, false, intp(300)},
	{"research_manager", "研究经理", "assistant", "裁决多空论证强度，输出结论与置信度。", []promptVar{
		{"AnalystFacts", "分析师事实列表", "- RSI=76.4 超买（analyst_market）", "distilled"},
		{"DebateTranscript", "多空辩论完整记录", "多头：趋势向上... 空头：但RSI超买...", "distilled"},
		{"SkippedAnalysts", "按资产类别裁掉的分析师", `["analyst_fundamental"]`, "runtime"},
		{"DataGaps", "本次数据缺口清单", "[]", "runtime"},
		{"AnalystFactsJSON", "分析师事实全量（JSON 数组，放末尾完整证据节）", `[{"role":"analyst_market","claim":"RSI=76.4 超买"}]`, "distilled"},
		{"DebateTranscriptJSON", "多空辩论全量（JSON 数组，放末尾完整证据节）", `[{"role":"debate_bull","round":1,"thesis":"趋势向上"}]`, "distilled"},
	}, false, intp(300)},
	{"trader", "交易员", "assistant", "基于研究裁决产出交易信号（方向/周期/置信度）。", []promptVar{
		{"Instrument", "标的", "SPY / SPDR S&P 500 / etf / us", "exploration"},
		{"ResearchConclusion", "研究裁决（bull_wins/bear_wins/draw/inconclusive）", "bull_wins：趋势与量能一致支持", "distilled"},
		{"LastPrice", "最新价", "580.12", "runtime"},
		{"AsOf", "基准时刻", "2026-10-26", "runtime"},
		{"TradingSession", "交易时段（open/close/is_open/next_open/timezone）", "is_open=false, next_open=2026-10-27T13:30:00Z", "runtime"},
		{"AccountMode", "账户模式（只影响措辞）", "paper", "runtime"},
		{"ResearchConclusionJSON", "研究裁决全量（JSON，放末尾，让 trader 读到完整推理链）", `{"conclusion":"bull_wins","confidence":0.72,"reasoning":"..."}`, "distilled"},
		{"SessionError", "交易时段无法确定时的错误（非空表示「不知道是否开市」，与 IsOpen=false 含义不同）", "calendar: 2026-10-26 not found", "runtime"},
	}, false, intp(300)},
	{"risk_aggressive", "激进风控", "assistant", "从机会成本角度审视提案：不做的代价是什么。", []promptVar{
		{"Instrument", "标的", "SPY / etf / us", "exploration"},
		{"TraderProposal", "交易提案", "signal=entry direction=long horizon=medium", "distilled"},
		{"ResearchConclusion", "研究裁决", "bull_wins", "distilled"},
		{"Round", "风控轮次", "1", "runtime"},
		{"SkippedAnalysts", "按资产类别裁掉的分析师", "[]", "runtime"},
		{"TraderProposalJSON", "交易提案全量（JSON，放末尾完整证据节）", `{"signal":"entry","direction":"long","horizon":"medium"}`, "distilled"},
	}, false, intp(300)},
	{"risk_neutral", "中性风控", "assistant", "从内部一致性角度审视提案：推理链是否自洽。", []promptVar{
		{"Instrument", "标的", "SPY / etf / us", "exploration"},
		{"TraderProposal", "交易提案", "signal=entry direction=long horizon=medium", "distilled"},
		{"ResearchConclusion", "研究裁决", "bull_wins", "distilled"},
		{"Round", "风控轮次", "1", "runtime"},
		{"SkippedAnalysts", "按资产类别裁掉的分析师", "[]", "runtime"},
		{"TraderProposalJSON", "交易提案全量（JSON，放末尾完整证据节）", `{"signal":"entry","direction":"long","horizon":"medium"}`, "distilled"},
	}, false, intp(300)},
	{"risk_conservative", "保守风控", "assistant", "从不可逆损失角度审视提案：最坏情况能承受吗。", []promptVar{
		{"Instrument", "标的", "SPY / etf / us", "exploration"},
		{"TraderProposal", "交易提案", "signal=entry direction=long horizon=medium", "distilled"},
		{"ResearchConclusion", "研究裁决", "bull_wins", "distilled"},
		{"Round", "风控轮次", "1", "runtime"},
		{"SkippedAnalysts", "按资产类别裁掉的分析师", "[]", "runtime"},
		{"TraderProposalJSON", "交易提案全量（JSON，放末尾完整证据节）", `{"signal":"entry","direction":"long","horizon":"medium"}`, "distilled"},
	}, false, intp(300)},
	{"portfolio_manager", "组合经理", "assistant", "综合全部输入产出六值评级，是分析链的决策终点。", []promptVar{
		{"Instrument", "标的", "SPY / etf / us", "exploration"},
		{"AnalystFacts", "分析师事实列表", "- RSI=76.4 超买", "distilled"},
		{"TraderProposal", "交易提案", "signal=entry direction=long", "distilled"},
		{"RiskDebateSummary", "三方风控意见摘要", "激进：accept；中性：建议下调confidence；保守：reject", "distilled"},
		{"SkippedAnalysts", "按资产类别裁掉的分析师", "[]", "runtime"},
		{"DataGaps", "本次数据缺口清单", "[]", "runtime"},
		{"AnalystFactsJSON", "分析师事实全量（JSON 数组，放末尾完整数据节）", `[{"role":"analyst_market","claim":"RSI=76.4 超买"}]`, "distilled"},
		{"TraderProposalJSON", "交易提案全量（JSON，放末尾完整数据节）", `{"signal":"entry","direction":"long"}`, "distilled"},
		{"RiskDebateSummaryJSON", "三方风控意见全量（JSON 数组，放末尾完整数据节）", `[{"role":"risk_aggressive","verdict":"accept"}]`, "distilled"},
	}, false, intp(300)},
}

// seedBuiltins inserts the fixed built-in agents and their variable catalog (idempotent).
//
// ON CONFLICT 顺带同步 role：role 只有代码在写（UpdateAgentMeta 只改 name/description），
// 所以它属于「代码定义的属性」。旧库里金融角色是手工按 role='worker' 写进去的，
// 不在这里同步就会永久漂移 —— 而 role 决定 DTO 展示与 agent 分类，
// 留着旧值等于同一个角色在两种库里有两副身份。
// run_seconds / interactive_shell 仍不覆盖，那是用户在 UI 里能改的东西。
func seedBuiltins(d execer) error {
	all := make([]builtinAgent, 0, len(builtinAgents)+len(builtinFinancialAgents))
	all = append(all, builtinAgents...)
	all = append(all, builtinFinancialAgents...)
	for _, a := range all {
		var agentID int64
		err := d.QueryRow(`
INSERT INTO agents(key, name, description, role, builtin, enabled, interactive_shell, run_seconds)
VALUES ($1, $2, NULLIF($3,''), $4, true, true, $5, COALESCE($6, 1200))
ON CONFLICT (key) DO UPDATE SET name = EXCLUDED.name, description = EXCLUDED.description,
  role = EXCLUDED.role
RETURNING id`, a.key, a.name, a.desc, a.role, a.interactiveShell, a.runSeconds).Scan(&agentID)
		if err != nil {
			return fmt.Errorf("agent %s: %w", a.key, err)
		}
		for _, v := range a.vars {
			if _, err := d.Exec(`
INSERT INTO agent_prompt_vars(agent_id, var_name, description, example, source)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (agent_id, var_name) DO UPDATE
  SET description = EXCLUDED.description, example = EXCLUDED.example, source = EXCLUDED.source`,
				agentID, v.name, v.desc, v.example, v.source); err != nil {
				return fmt.Errorf("agent %s var %s: %w", a.key, v.name, err)
			}
		}
	}
	// Drop catalog entries for variables that were renamed, so the white-list no
	// longer advertises a name templates can't resolve (EngagementTitle→Description).
	// 'Now' 从各 agent 目录提升为全局 runtime 变量后,旧库里 goals 仍残留一条 'Now'
	// 会与全局项撞名(前端变量列表 key 重复);一并清掉。
	if _, err := d.Exec(`DELETE FROM agent_prompt_vars WHERE var_name IN ('EngagementTitle', 'CoverageGaps', 'Now')`); err != nil {
		return fmt.Errorf("cleanup renamed vars: %w", err)
	}
	// Default-on interactive_shell for the runtime agents (planner/worker/mainagent/auto)
	// ONCE — respects a later user toggle-off (guarded by a settings flag). goals(one-shot
	// decomposer) stays off. Runs after the column exists (schema applied before seed).
	if v, _, _ := getSettingE(d, "interactive_shell_default_v1"); v != "true" {
		if _, err := d.Exec(`UPDATE agents SET interactive_shell=true WHERE key IN ('planner','worker','mainagent','auto')`); err != nil {
			return fmt.Errorf("seed interactive_shell defaults: %w", err)
		}
		_ = setSettingE(d, "interactive_shell_default_v1", "true")
	}
	// Seed the built-in browser (Playwright) MCP once — DISABLED by default (用户
	// 需要时自行启用), no proxy by default. The traffic-capture toggle injects/strips
	// the recording proxy + CA at runtime (server.Manager.syncBrowserMCPProxy).
	// Insert only if absent so we never clobber user edits (args/env/enabled/
	// visibility) on restart.
	if _, err := d.Exec(`
INSERT INTO mcp_servers(name, transport, command, args, env, enabled)
VALUES ('browser', 'stdio', 'npx', $1, '{}', false)
ON CONFLICT (name) DO NOTHING`,
		`["@playwright/mcp","--headless"]`); err != nil {
		return fmt.Errorf("seed browser mcp: %w", err)
	}
	// NOTE: the placeholder ScopeSentry data-source MCP (empty URL + empty X-API-Key,
	// disabled) is seeded directly in schema.sql §F so a raw `psql < schema.sql` init
	// also gets it. schema.sql is Exec'd on every startup, so it stays idempotent.
	if err := seedBuiltinSkillVisibility(d); err != nil {
		return fmt.Errorf("seed skill visibility: %w", err)
	}
	if err := seedDefaultInterceptRules(d); err != nil {
		return fmt.Errorf("seed intercept rules: %w", err)
	}
	if err := seedDefaultInterceptRulesV2(d); err != nil {
		return fmt.Errorf("seed intercept rules v2: %w", err)
	}
	if err := seedDefaultInterceptRulesV3(d); err != nil {
		return fmt.Errorf("seed intercept rules v3: %w", err)
	}
	if err := seedDefaultAssetInterceptRules(d); err != nil {
		return fmt.Errorf("seed asset intercept rules: %w", err)
	}
	// 下线渗透语义的 agent（一次性，各自 settings flag 守卫）。放最后：
	// 必须在既有 seed 之后，否则同一事务里重开旧库会先建再删。
	if err := seedAgentRemovals(d); err != nil {
		return err
	}
	return nil
}

// seedAgentRemovals 删除已下线的内置 agent 及其残留绑定（每个一次，flag 守卫）。
//
// # 为什么需要它们
//
// 本产品从「渗透测试平台」转为金融分析平台（Terix），三个 agent 属于旧语义：
//   - pentest：独立渗透 agent。Terix 的任务是金融分析，没有渗透链。
//   - reporter：漏洞报告撰写。它绑的 update_finding_report / list_task_findings
//     在代码里根本没有实现，永远写不出报告 —— 死功能。
//   - retester：漏洞复测。生产调用点与 db.finding_retests 表一并删除。
//
// 光在代码里删 builtinAgents 项对**已部署的库**无效：那几行是之前 seed 进去的，
// 重启不会自动消失（seed 只 insert/update，从不 delete）。所以这里写显式 DELETE。
// 用 flag 守卫而不是每次启动都跑：用户若刻意手工重建同名自定义 agent，
// 后续启动不该再把它删掉。
//
// 清理面必须覆盖「引用这张 agent key 的三处」：
//   - agents 行（agent_prompts / agent_prompt_vars / 可见性行靠 FK ON DELETE CASCADE）
//   - agent_triggers 行（无外键，必须手删，否则触发器会继续按 key 唤起不存在的 agent）
//   - tools.agents JSONB 里的 key（无外键，留着会让 UI 显示一个不存在的绑定）
func seedAgentRemovals(d execer) error {
	// 通用清理：删触发器 → 从工具绑定里摘掉 → 删 agent 行。
	// conversations 由调用方按需处理（pentest 没有历史会话，reporter 同理；
	// retester 有 300+ 条只服务于已删复测表的会话，见下）。
	remove := func(agentKey string) error {
		if _, err := d.Exec(`DELETE FROM agent_triggers WHERE agent_key=$1`, agentKey); err != nil {
			return fmt.Errorf("删 %s 触发器: %w", agentKey, err)
		}
		if _, err := d.Exec(`UPDATE tools SET agents = (SELECT COALESCE(json_group_array(value),'[]')
			FROM json_each(tools.agents) WHERE value <> $1)
			WHERE EXISTS (SELECT 1 FROM json_each(tools.agents) WHERE value = $1)`, agentKey); err != nil {
			return fmt.Errorf("从工具绑定摘除 %s: %w", agentKey, err)
		}
		if _, err := d.Exec(`DELETE FROM agents WHERE key=$1`, agentKey); err != nil {
			return fmt.Errorf("删 agent %s: %w", agentKey, err)
		}
		return nil
	}

	if v, _, _ := getSettingE(d, "pentest_agent_removed_v1"); v != "true" {
		if err := remove("pentest"); err != nil {
			return err
		}
		if err := setSettingE(d, "pentest_agent_removed_v1", "true"); err != nil {
			return fmt.Errorf("记录 pentest_agent_removed_v1: %w", err)
		}
	}

	if v, _, _ := getSettingE(d, "reporter_agent_removed_v1"); v != "true" {
		if err := remove("reporter"); err != nil {
			return err
		}
		if err := setSettingE(d, "reporter_agent_removed_v1", "true"); err != nil {
			return fmt.Errorf("记录 reporter_agent_removed_v1: %w", err)
		}
	}

	if v, _, _ := getSettingE(d, "retester_agent_removed_v1"); v != "true" {
		// retester 的历史会话只为 finding_retests 服务（一个会话对应一条复测记录，
		// 记录本身随表一起删）。留着会话没有可用的 agent 打开，点了必然报错，
		// 所以一并删；它们的对话内容属于复测产物，随功能一起下线。
		if _, err := d.Exec(`DELETE FROM conversations WHERE agent_key='retester'`); err != nil {
			return fmt.Errorf("删 retester 会话: %w", err)
		}
		if err := remove("retester"); err != nil {
			return err
		}
		if err := setSettingE(d, "retester_agent_removed_v1", "true"); err != nil {
			return fmt.Errorf("记录 retester_agent_removed_v1: %w", err)
		}
	}

	if v, _, _ := getSettingE(d, "finding_retests_dropped_v1"); v != "true" {
		// SQLite 空库起步：finding_retests 表在新 schema 里根本不存在，
		// 所以不需要（也不能）执行 PG 那套 DROP TRIGGER / DROP FUNCTION /
		// DROP TABLE CASCADE（SQLite 的 DROP TRIGGER 不接受 ON 子句，
		// 也没有 DROP FUNCTION / CASCADE）。
		// 保留这个 flag 是为了让"曾经做过清理"这件事在 settings 里有据可查，
		// 且与新库/旧库两条路径的行为一致。
		if err := setSettingE(d, "finding_retests_dropped_v1", "true"); err != nil {
			return fmt.Errorf("记录 finding_retests_dropped_v1: %w", err)
		}
	}

	// 清掉指向「不存在的 agent」的工具绑定（每次启动都跑，幂等且自愈）。
	//
	// # 为什么不能只靠上面的一次性 flag
	//
	// 时序问题：本函数在 db.Open 里跑，而 tools 表的行是 server 启动后由
	// wireTools / seedOrchestrationTools 才 seed 的。全新库首次启动时这些行还不存在，
	// 一次性 flag 已经写死为 true，于是随后 seed 出来的 tools.agents 里带着
	// pentest/reporter/retester，再也不会被清 —— UI 上永远显示一个不存在的绑定。
	//
	// 判据是「工具绑定里的 key 在 agents 表里没有对应行」：这种绑定无歧义是脏数据
	// （点开必然报错），且因为条件是 NOT EXISTS，用户若真的手工重建同名 agent，
	// 绑定会被保留，不会误删。
	for _, key := range []string{"pentest", "reporter", "retester"} {
		if _, err := d.Exec(`
UPDATE tools SET agents = (SELECT COALESCE(json_group_array(value),'[]')
	FROM json_each(tools.agents) WHERE value <> $1)
WHERE EXISTS (SELECT 1 FROM json_each(tools.agents) WHERE value = $1)
  AND NOT EXISTS (SELECT 1 FROM agents WHERE key = $1)`, key); err != nil {
			return fmt.Errorf("清理 %s 的悬空工具绑定: %w", key, err)
		}
	}
	return nil
}

// seedDefaultAssetInterceptRules inserts the built-in asset blocklist (fuzzy
// domain matches for government / education sites) once on first startup. Gated
// by a settings flag so a user's later disable/delete is never resurrected on
// restart — same policy as the intercept-rule seed.
func seedDefaultAssetInterceptRules(d execer) error {
	if v, _, _ := getSettingE(d, "asset_intercept_default_rules_v1"); v == "done" {
		return nil
	}
	rules := []struct {
		kind    string
		pattern string
		note    string
	}{
		{"fuzzy_domain", ".gov", "[内置] 政府网站 (.gov)"},
		{"fuzzy_domain", ".gov.cn", "[内置] 政府网站 (.gov.cn)"},
		{"fuzzy_domain", ".edu", "[内置] 教育网站 (.edu)"},
		{"fuzzy_domain", ".edu.cn", "[内置] 教育网站 (.edu.cn)"},
	}
	for _, r := range rules {
		if _, err := d.Exec(`
INSERT INTO asset_intercept_rules(enabled, kind, pattern, note, builtin)
SELECT true, $1, $2, $3, true
WHERE NOT EXISTS (SELECT 1 FROM asset_intercept_rules WHERE kind=$1 AND pattern=$2)`,
			r.kind, r.pattern, r.note); err != nil {
			return fmt.Errorf("asset rule %q: %w", r.pattern, err)
		}
	}
	return setSettingE(d, "asset_intercept_default_rules_v1", "done")
}

// builtinSkillVisibility maps a shipped skill's directory name → the built-in
// agent keys that should see it by default. The skill FILES themselves live on the
// filesystem (SkillDir, loaded by norma at runtime); DB only carries this visibility
// binding. Skills omitted here ship invisible by default — the user turns them on
// per-agent when needed.
//
// # 当前为空（Terix）
//
// 原先这里只有一项 "api-recon" → {auto, pentest, worker}，那是渗透侦察技能。
// 它连同 skills/api-recon/ 目录一起删除了（Terix 是金融分析平台）。
//
// 保留这个空 map 而不是删掉整个机制：将来金融 skill（如财报取数、行情指标）
// 需要默认可见时，在这里加一项即可 —— seedBuiltinSkillVisibility() 的
// insert-if-absent 语义对新条目同样适用。
var builtinSkillVisibility = map[string][]string{}

// seedBuiltinSkillVisibility binds the shipped built-in skills to their default
// agents. Insert-if-absent (ON CONFLICT DO NOTHING) so a user's later toggle-off is
// never resurrected on restart — matches the browser-MCP / intercept-rule seed policy.
func seedBuiltinSkillVisibility(d execer) error {
	for skillName, agentKeys := range builtinSkillVisibility {
		for _, key := range agentKeys {
			if _, err := d.Exec(`
INSERT INTO agent_skill_visibility(agent_id, skill_name, enabled)
SELECT id, $2, true FROM agents WHERE key=$1
ON CONFLICT (agent_id, skill_name) DO NOTHING`, key, skillName); err != nil {
				return fmt.Errorf("skill %s → agent %s: %w", skillName, key, err)
			}
		}
	}
	return nil
}

// seedDefaultInterceptRules inserts built-in safety intercept rules once on
// first startup. The seed is gated by a settings flag so user edits (disable,
// delete, re-order) are never overwritten on subsequent restarts.
func seedDefaultInterceptRules(d execer) error {
	if v, _, _ := getSettingE(d, "intercept_default_rules_v1"); v == "done" {
		return nil
	}
	type rule struct {
		name     string
		target   string // tool_name | tool_input
		typ      string // string | regex
		pattern  string
		action   string
		message  string
		priority int
	}
	rules := []rule{
		// ── 系统破坏性命令 (priority 100) ──────────────────────────────────
		{
			name:     "[内置] 递归强制删除 rm -rf",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\brm\b.{0,80}(?:-[a-z]*r[a-z]*f[a-z]*|-[a-z]*f[a-z]*r[a-z]*|--recursive|--no-preserve-root)`,
			action:   "deny",
			message:  "禁止执行递归强制删除（rm -rf / rm --recursive），可能永久损坏系统或靶机环境",
			priority: 100,
		},
		{
			name:     "[内置] 删除系统关键目录",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\brm\b[^"'\n]{0,60}["'\s](/|/etc|/bin|/usr|/boot|/var|/lib|/sys|/proc|/dev|/sbin|/root)`,
			action:   "deny",
			message:  "禁止删除系统关键路径",
			priority: 100,
		},
		{
			name:     "[内置] 磁盘格式化 mkfs",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\bmkfs\b`,
			action:   "deny",
			message:  "禁止格式化磁盘（mkfs）",
			priority: 100,
		},
		{
			name:     "[内置] 覆写磁盘设备 dd",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\bdd\b[^|\n]{0,100}\bof=\s*/dev/[a-zA-Z]`,
			action:   "deny",
			message:  "禁止使用 dd 覆写磁盘设备",
			priority: 100,
		},
		{
			name:     "[内置] Fork 炸弹",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `:\(\)\s*\{[^}]*:\|:`,
			action:   "deny",
			message:  "禁止执行 Fork 炸弹",
			priority: 100,
		},
		{
			name:     "[内置] 关机 / 重启",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\b(?:shutdown|reboot|halt|poweroff|init\s+[06])\b`,
			action:   "deny",
			message:  "禁止执行关机或重启命令",
			priority: 100,
		},
		{
			name:     "[内置] 杀死全部进程",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\bkill\s+-9\s+-1\b|\bkillall\s+-9\b`,
			action:   "deny",
			message:  "禁止 kill -9 -1 或 killall -9（杀死所有进程）",
			priority: 100,
		},
		{
			name:     "[内置] 磁盘擦除 shred / wipe",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\b(?:shred|wipe)\b[^|\n]{0,80}/dev/[a-zA-Z]`,
			action:   "deny",
			message:  "禁止对磁盘设备执行 shred/wipe 擦除",
			priority: 100,
		},
		{
			name:     "[内置] 清空防火墙规则",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\biptables\s+(?:-F|--flush)\b|\bnft\s+flush\s+ruleset\b`,
			action:   "deny",
			message:  "禁止清空防火墙规则（iptables -F / nft flush）",
			priority: 100,
		},
		// ── 数据库破坏性操作 (priority 90) ─────────────────────────────────
		{
			name:     "[内置] SQL DROP DATABASE / TABLE / SCHEMA",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\bDROP\s+(?:DATABASE|TABLE|SCHEMA|INDEX|VIEW|TABLESPACE|USER|ROLE)\b`,
			action:   "deny",
			message:  "禁止执行 DROP 操作，可能不可逆地销毁数据库对象",
			priority: 90,
		},
		{
			name:     "[内置] SQL TRUNCATE",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\bTRUNCATE\s+(?:TABLE\s+)?\w`,
			action:   "deny",
			message:  "禁止执行 TRUNCATE，可能清空数据表所有数据",
			priority: 90,
		},
		{
			name:     "[内置] MongoDB drop / dropDatabase",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\.(?:dropDatabase|dropCollection|drop)\s*\(`,
			action:   "deny",
			message:  "禁止执行 MongoDB drop 操作",
			priority: 90,
		},
		{
			name:     "[内置] Redis FLUSHALL / FLUSHDB",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\b(?:FLUSHALL|FLUSHDB)\b`,
			action:   "deny",
			message:  "禁止执行 Redis FLUSHALL / FLUSHDB，可能清空全部缓存数据",
			priority: 90,
		},
		// ── HTTP 破坏性请求 (priority 80) ──────────────────────────────────
		// Agent 发送 DELETE 请求的三种常见方式：
		//   1. curl -X DELETE / --request DELETE（Bash 工具直接执行或写入脚本）
		//   2. Python HTTP 客户端 .delete() 方法
		//   3. JS/通用脚本里的 method: 'DELETE' / method="DELETE"
		{
			name:     "[内置] curl / wget 发送 DELETE 请求",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\bcurl\b[^|\n&;"]{0,300}(?:-X\s*DELETE|--request\s+DELETE|-XDELETE)|\bwget\b[^|\n&;"]{0,300}--method[=\s]+DELETE`,
			action:   "deny",
			message:  "禁止通过 curl/wget 发送 HTTP DELETE 请求，可能删除目标系统数据",
			priority: 80,
		},
		{
			name:     "[内置] Python HTTP 客户端 DELETE（requests/httpx/aiohttp）",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\b(?:requests|httpx|aiohttp|urllib\.request)\.delete\s*\(|session\.delete\s*\(|client\.delete\s*\(`,
			action:   "deny",
			message:  "禁止使用 Python HTTP 客户端发送 DELETE 请求",
			priority: 80,
		},
		{
			name:     "[内置] 脚本中声明 HTTP DELETE 方法（JS/通用）",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)axios\.delete\s*\(|method\s*[:=]\s*['"]DELETE['"]`,
			action:   "deny",
			message:  "禁止在脚本中声明并发送 HTTP DELETE 请求",
			priority: 80,
		},
		{
			name:     "[内置] 批量清空 / 清除接口路径",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)/(?:clear|wipe|flush|purge|truncate|drop|destroy|factory[-_]reset|reset[-_]all)(?:[/?#"'\s]|$)`,
			action:   "deny",
			message:  "禁止调用批量清空或销毁类接口（/clear /wipe /flush /purge 等）",
			priority: 80,
		},
	}
	for _, r := range rules {
		if _, err := d.Exec(`
INSERT INTO intercept_rules(name, enabled, priority, match_target, match_type, pattern, action, message, timeout_enabled, timeout_seconds, timeout_action)
SELECT $1, true, $2, $3, $4, $5, $6, $7, false, 60, 'deny'
WHERE NOT EXISTS (SELECT 1 FROM intercept_rules WHERE name=$1)`,
			r.name, r.priority, r.target, r.typ, r.pattern, r.action, r.message,
		); err != nil {
			return fmt.Errorf("rule %q (v1): %w", r.name, err)
		}
	}
	return setSettingE(d, "intercept_default_rules_v1", "done")
}

// seedDefaultInterceptRulesV2 migrates the two safety patterns that used to be
// hard-coded in guard.go (destructive shell + data-exfil pipe) into ordinary
// intercept rules. Gated by its own flag so it also lands on DBs that already ran
// v1. Unlike the old guard.go floor, these are plain [内置] rules — the user can
// disable or delete them. The exfil rule ships DISABLED by default (its
// curl/wget/nc pipe pattern mis-fires on legitimate CTF/pentest reverse-shell and
// data-transfer pipes); enable it manually when exfil gating is actually wanted.
func seedDefaultInterceptRulesV2(d execer) error {
	if v, _, _ := getSettingE(d, "intercept_default_rules_v2"); v == "done" {
		return nil
	}
	rules := []struct {
		name     string
		pattern  string
		action   string
		message  string
		enabled  bool
		priority int
	}{
		{
			name:     "[内置] 破坏性系统命令",
			pattern:  `(?i)\b(rm\s+-rf\s+/|mkfs|dd\s+if=|:\(\)\s*\{|shutdown|reboot|>\s*/dev/sd)`,
			action:   "deny",
			message:  "破坏性命令被拒绝（rm -rf / / mkfs / dd / fork bomb / 关机重启 / 覆写磁盘设备）",
			enabled:  true,
			priority: 100,
		},
		{
			name:     "[内置] 数据外泄管道",
			pattern:  `(?i)(curl|wget|nc|ncat)\b[^|]*\b(\|\s*(curl|wget|nc))`,
			action:   "deny",
			message:  "疑似数据外泄管道被拒绝（命令输出经 curl/wget/nc 外传）",
			enabled:  false,
			priority: 80,
		},
	}
	for _, r := range rules {
		if _, err := d.Exec(`
INSERT INTO intercept_rules(name, enabled, priority, match_target, match_type, pattern, action, message, timeout_enabled, timeout_seconds, timeout_action)
SELECT $1, $2, $3, 'tool_input', 'regex', $4, $5, $6, false, 60, 'deny'
WHERE NOT EXISTS (SELECT 1 FROM intercept_rules WHERE name=$1)`,
			r.name, r.enabled, r.priority, r.pattern, r.action, r.message,
		); err != nil {
			return fmt.Errorf("rule %q (v1): %w", r.name, err)
		}
	}
	return setSettingE(d, "intercept_default_rules_v2", "done")
}

// seedDefaultInterceptRulesV3 adds the delete-endpoint path rule. The v1 HTTP rules
// only catch the DELETE *method* (curl -X DELETE, requests.delete(, method:'DELETE'),
// and v1's path rule covers only /clear /wipe /flush /purge /truncate /drop /destroy
// /factory-reset /reset-all — so a plain `curl 'http://t/api/user/delete?id=1'` (a
// delete endpoint reached with GET/POST, which is how most web apps expose deletion)
// slipped through every built-in rule. Own flag so it also lands on DBs that already
// ran v1/v2, where editing the v1 seed would have no effect.
//
// The pattern deliberately requires a separator after the verb so /delivery,
// /details, /delta and /delegate do not match, while /deleteAll, /delete_user and
// /delete-user do. destroy is re-covered here because v1's rule does not allow a
// suffix (/destroyAll was missed).
//
// Exported as a package const only so the seeded regex is unit-testable without a DB.
const deleteEndpointPathPattern = `(?i)/(?:(?:delete|remove|unlink|erase|destroy)[-\w]*|del)(?:[/?#"'\s]|$)`

func seedDefaultInterceptRulesV3(d execer) error {
	if v, _, _ := getSettingE(d, "intercept_default_rules_v3"); v == "done" {
		return nil
	}
	const name = "[内置] 删除类接口路径"
	if _, err := d.Exec(`
INSERT INTO intercept_rules(name, enabled, priority, match_target, match_type, pattern, action, message, timeout_enabled, timeout_seconds, timeout_action)
SELECT $1, true, 80, 'tool_input', 'regex', $2, 'deny', $3, false, 60, 'deny'
WHERE NOT EXISTS (SELECT 1 FROM intercept_rules WHERE name = $1)`,
		name,
		deleteEndpointPathPattern,
		"禁止调用删除类接口（/delete /remove /unlink /erase 等），不论使用哪种 HTTP 方法——多数应用的删除接口用 GET/POST 就能触发，同样会真实删除目标数据",
	); err != nil {
		return fmt.Errorf("rule %q: %w", name, err)
	}
	return setSettingE(d, "intercept_default_rules_v3", "done")
}
