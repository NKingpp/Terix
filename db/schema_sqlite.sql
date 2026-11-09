-- ARTEX SQLite schema（由 tools_schema/gen_sqlite_schema.py 生成，勿手改）
-- 生成源：db/schema.sql（PostgreSQL 版）。改结构请改 schema.sql 后重跑生成器。
--
-- 关键约定（详见 docs/quantex/ARCH-SQLite-Migration.md）：
--   * 时间列 TEXT，格式 strftime('%Y-%m-%dT%H:%M:%fZ','now')（UTC 毫秒，字典序=时间序）
--   * 数组/JSONB 列 TEXT，内容是 JSON（数组是 []，对象是 {}）
--   * 金额 NUMERIC 列 TEXT，Go 侧 big.Rat 精确运算
--   * CIDR/INET 列 TEXT，包含判断走 Go UDF（cidr_contains）
--   * 禁止跳号的 $N 占位符；禁止 ?, 一律 $N

PRAGMA foreign_keys = ON;

-- ============================================================
-- 阶段 1：建表
-- ============================================================

CREATE TABLE IF NOT EXISTS companies (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    name TEXT NOT NULL,
    nkey TEXT NOT NULL UNIQUE,
    logo TEXT,
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE TABLE IF NOT EXISTS assets (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    type TEXT NOT NULL CHECK (type IN ( 'root_domain','ip','subdomain','app','service','endpoint' )),
    company_id INTEGER REFERENCES companies(id) ON DELETE SET NULL,
    company_source TEXT NOT NULL DEFAULT 'explicit' CHECK (company_source IN ('explicit','scope')),
    task_ids TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(task_ids)),
    domain TEXT,
    root_domain TEXT,
    ip TEXT,
    c_segment TEXT,
    port INTEGER CHECK (port BETWEEN 1 AND 65535),
    icp TEXT,
    bound_domains TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(bound_domains)),
    open_ports TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(open_ports)),
    record_type TEXT,
    record_value TEXT CHECK (json_valid(record_value)),
    bundle_id TEXT,
    app_name TEXT,
    category TEXT,
    app_description TEXT,
    app_icp TEXT,
    url TEXT,
    service_type TEXT CHECK (service_type IN ('http','other')),
    service_name TEXT,
    favicon_mmh3 TEXT,
    status_code INTEGER,
    content_length INTEGER,
    page_title TEXT,
    technologies TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(technologies)),
    auth TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(auth)),
    method TEXT,
    params TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(params)),
    extra TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(extra)),
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    last_seen TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    CONSTRAINT assets_company_source_check CHECK (company_source IN ('explicit','scope'))
);

CREATE TABLE IF NOT EXISTS company_scope (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    company_id INTEGER NOT NULL REFERENCES companies(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (kind IN ('domain','ip','cidr','icp','keyword')),
    domain TEXT,
    net TEXT,
    "value" TEXT,
    raw TEXT NOT NULL,
    reason TEXT,
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    CONSTRAINT uq_sv2_domain UNIQUE (company_id, domain),
    CONSTRAINT uq_sv2_net UNIQUE (company_id, net),
    CONSTRAINT company_scope_kind_check CHECK (kind IN ('domain','ip','cidr','icp','keyword')),
    CONSTRAINT ck_company_scope_payload CHECK ( (kind = 'domain' AND domain IS NOT NULL AND net IS NULL AND value IS NULL) OR (kind IN ('ip','cidr') AND domain IS NULL AND net IS NOT NULL AND value IS NULL) OR (kind IN ('icp','keyword') AND domain IS NULL AND net IS NULL AND value IS NOT NULL) )
);

CREATE TABLE IF NOT EXISTS explorations (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    description TEXT,
    goal TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','achieved','failed')),
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    round_no INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS exploration_nodes (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    exploration_id INTEGER NOT NULL REFERENCES explorations(id) ON DELETE CASCADE,
    kind TEXT NOT NULL,
    payload TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(payload)),
    priority INTEGER NOT NULL DEFAULT 0,
    state TEXT NOT NULL DEFAULT 'open',
    origin TEXT,
    owner TEXT,
    blocked_reason TEXT,
    delete_reason TEXT,
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    completed_at TIMESTAMP,
    content_version INTEGER NOT NULL DEFAULT 0,
    cold_since_round INTEGER,
    CONSTRAINT ck_node_kind CHECK (kind IN ('begin','goal','intent','fact','finding','hint','digest','join','thesis','signal','conclusion')),
    CONSTRAINT ck_node_state CHECK ( (kind='begin' AND state IN ('open')) OR (kind='intent' AND state IN ('open','running','paused','done','blocked','exhausted','stopped','deleted')) OR (kind='goal' AND state IN ('open','met','abandoned')) OR (kind='fact' AND state IN ('confirmed','dismissed','origin')) OR (kind='finding' AND state IN ('confirmed','dismissed')) OR (kind='hint' AND state IN ('active','consumed')) OR (kind='digest' AND state IN ('active','superseded')) OR (kind='join' AND state IN ('open','done','blocked')) OR (kind='thesis' AND state IN ('open','standing','refuted','superseded')) OR (kind='signal' AND state IN ('open','acted','expired','voided')) OR (kind='conclusion' AND state IN ('final','superseded')) )
);

CREATE TABLE IF NOT EXISTS exploration_edges (
    exploration_id INTEGER NOT NULL REFERENCES explorations(id) ON DELETE CASCADE,
    src_id INTEGER NOT NULL REFERENCES exploration_nodes(id) ON DELETE CASCADE,
    dst_id INTEGER NOT NULL REFERENCES exploration_nodes(id) ON DELETE CASCADE,
    rel TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    PRIMARY KEY (exploration_id, src_id, rel, dst_id),
    CONSTRAINT ck_edge_noself CHECK (src_id <> dst_id),
    CONSTRAINT ck_edge_rel CHECK (rel IN ('spawns','derived_from','yields','proves','covers','gates'))
);

CREATE TABLE IF NOT EXISTS exploration_anchors (
    node_id INTEGER NOT NULL REFERENCES exploration_nodes(id) ON DELETE CASCADE,
    asset_id INTEGER NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    PRIMARY KEY (node_id, asset_id)
);

CREATE TABLE IF NOT EXISTS task_constraints (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    exploration_id INTEGER NOT NULL REFERENCES explorations(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (kind IN ('allow','deny')),
    text TEXT NOT NULL,
    origin TEXT,
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE TABLE IF NOT EXISTS activity (
    id INTEGER PRIMARY KEY NOT NULL,
    exploration_id INTEGER NOT NULL REFERENCES explorations(id) ON DELETE CASCADE,
    node_id INTEGER REFERENCES exploration_nodes(id) ON DELETE SET NULL,
    worker TEXT,
    kind TEXT,
    tool TEXT,
    tool_use_id TEXT,
    is_error INTEGER NOT NULL DEFAULT 0,
    summary TEXT,
    detail TEXT,
    metadata TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(metadata)),
    input_tokens INTEGER,
    output_tokens INTEGER,
    cache_read_tokens INTEGER,
    cache_write_tokens INTEGER,
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    main_seg INTEGER
);

CREATE TABLE IF NOT EXISTS main_sessions (
    exploration_id INTEGER NOT NULL REFERENCES explorations(id) ON DELETE CASCADE,
    seq INTEGER NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    PRIMARY KEY (exploration_id, seq)
);

CREATE TABLE IF NOT EXISTS settings (
    "key" TEXT PRIMARY KEY,
    "value" TEXT NOT NULL,
    updated_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE TABLE IF NOT EXISTS llm_profiles (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    name TEXT NOT NULL UNIQUE,
    format TEXT NOT NULL CHECK (format IN ('openai','anthropic','openai-responses')),
    base_url TEXT,
    proxy TEXT,
    model TEXT NOT NULL,
    api_key TEXT,
    api_key_hint TEXT,
    rate_per_second REAL NOT NULL DEFAULT 0,
    rate_per_minute REAL NOT NULL DEFAULT 0,
    context_window_k INTEGER NOT NULL DEFAULT 0,
    reasoning_effort TEXT NOT NULL DEFAULT '',
    thinking_type TEXT NOT NULL DEFAULT '',
    is_default INTEGER NOT NULL DEFAULT 0,
    priority INTEGER NOT NULL DEFAULT 0,
    pool_exclude INTEGER NOT NULL DEFAULT 0,
    streaming INTEGER NOT NULL DEFAULT 1,
    max_tokens INTEGER NOT NULL DEFAULT 0,
    max_tokens_field TEXT NOT NULL DEFAULT '',
    session_header_key TEXT NOT NULL DEFAULT '',
    retry_connect_attempts INTEGER NOT NULL DEFAULT 0,
    retry_connect_interval_ms INTEGER NOT NULL DEFAULT 0,
    retry_empty_attempts INTEGER NOT NULL DEFAULT 0,
    retry_empty_interval_ms INTEGER NOT NULL DEFAULT 0,
    retry_stream_attempts INTEGER NOT NULL DEFAULT 0,
    retry_stream_interval_ms INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE TABLE IF NOT EXISTS llm_profile_health (
    profile_id INTEGER REFERENCES llm_profiles(id) ON DELETE CASCADE PRIMARY KEY,
    fails INTEGER NOT NULL DEFAULT 0,
    trips INTEGER NOT NULL DEFAULT 0,
    open_until TIMESTAMP,
    last_error TEXT NOT NULL DEFAULT '',
    last_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE TABLE IF NOT EXISTS task_categories (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    name TEXT NOT NULL,
    nkey TEXT NOT NULL UNIQUE,
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE TABLE IF NOT EXISTS tasks (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    name TEXT NOT NULL DEFAULT '',
    category_id INTEGER REFERENCES task_categories(id) ON DELETE SET NULL,
    description TEXT NOT NULL,
    goal TEXT NOT NULL,
    exploration_id INTEGER NOT NULL REFERENCES explorations(id) ON DELETE RESTRICT UNIQUE,
    status TEXT NOT NULL DEFAULT 'created' CHECK (status IN ('created','running','paused','done','failed','timeout')),
    paused INTEGER NOT NULL DEFAULT 0,
    queued INTEGER NOT NULL DEFAULT 0,
    queued_at TIMESTAMP,
    queue_mode TEXT NOT NULL DEFAULT '',
    llm_profile_id INTEGER REFERENCES llm_profiles(id) ON DELETE SET NULL,
    active_llm_profile_id INTEGER REFERENCES llm_profiles(id) ON DELETE SET NULL,
    llm_chain_revision INTEGER NOT NULL DEFAULT 0,
    company_id INTEGER REFERENCES companies(id) ON DELETE SET NULL,
    parent_ref TEXT,
    timeout_seconds INTEGER NOT NULL DEFAULT 0,
    plan_heartbeat_seconds INTEGER NOT NULL DEFAULT 300,
    coverage_enabled INTEGER NOT NULL DEFAULT 1,
    pinned_at TIMESTAMP,
    first_run_at TIMESTAMP,
    deadline_at TIMESTAMP,
    archived_at TIMESTAMP,
    deleted_at TIMESTAMP,
    completed_at TIMESTAMP,
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    instrument_id INTEGER REFERENCES instruments(id) ON DELETE SET NULL
);

CREATE TABLE IF NOT EXISTS task_archives (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE UNIQUE,
    state TEXT NOT NULL DEFAULT 'archive_queued' CHECK (state IN ( 'archive_queued','archiving','archive_failed','ready', 'restore_queued','restoring','restore_failed', 'delete_queued','deleting','delete_failed' )),
    phase TEXT NOT NULL DEFAULT 'queued',
    progress INTEGER NOT NULL DEFAULT 0 CHECK (progress BETWEEN 0 AND 100),
    error TEXT NOT NULL DEFAULT '',
    warnings TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(warnings)),
    format_version INTEGER NOT NULL DEFAULT 2,
    archive_path TEXT NOT NULL DEFAULT '',
    sha256 TEXT NOT NULL DEFAULT '',
    original_size INTEGER NOT NULL DEFAULT 0,
    compressed_size INTEGER NOT NULL DEFAULT 0,
    task_name TEXT NOT NULL DEFAULT '',
    task_description TEXT NOT NULL DEFAULT '',
    task_goal TEXT NOT NULL DEFAULT '',
    original_status TEXT NOT NULL DEFAULT '',
    category_id_snapshot INTEGER,
    category_name_snapshot TEXT NOT NULL DEFAULT '',
    source_task_ids TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(source_task_ids)),
    remaining_timeout_seconds INTEGER NOT NULL DEFAULT 0,
    data_counts TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(data_counts)),
    aggregate_stats TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(aggregate_stats)),
    archived_at TIMESTAMP,
    requested_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE TABLE IF NOT EXISTS task_templates (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    name TEXT NOT NULL,
    nkey TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL,
    goal TEXT NOT NULL,
    category_id INTEGER REFERENCES task_categories(id) ON DELETE SET NULL,
    intercept_rules TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(intercept_rules)),
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE TABLE IF NOT EXISTS task_relations (
    task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    source_task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    PRIMARY KEY (task_id, source_task_id),
    CONSTRAINT ck_task_relation_not_self CHECK (task_id <> source_task_id)
);

CREATE TABLE IF NOT EXISTS task_asset_links (
    task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    asset_id INTEGER NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    source TEXT NOT NULL DEFAULT 'system',
    source_summary TEXT NOT NULL DEFAULT '',
    source_node_id INTEGER REFERENCES exploration_nodes(id) ON DELETE SET NULL,
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    PRIMARY KEY (task_id, asset_id)
);

CREATE TABLE IF NOT EXISTS task_llm_profiles (
    task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    profile_id INTEGER NOT NULL REFERENCES llm_profiles(id) ON DELETE CASCADE,
    position INTEGER NOT NULL CHECK (position >= 0),
    status TEXT NOT NULL DEFAULT 'ready' CHECK (status IN ('ready','quota_exhausted')),
    last_error TEXT,
    exhausted_at TIMESTAMP,
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    PRIMARY KEY (task_id, profile_id),
    UNIQUE (task_id, position)
);

CREATE TABLE IF NOT EXISTS task_scope (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (kind IN ('company','root_domain','subdomain','ip','cidr','icp','keyword')),
    company_id INTEGER REFERENCES companies(id) ON DELETE CASCADE,
    domain TEXT,
    net TEXT,
    "value" TEXT,
    source TEXT NOT NULL DEFAULT 'auto' CHECK (source IN ('auto','agent','manual')),
    reason TEXT,
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    CONSTRAINT task_scope_kind_check CHECK (kind IN ('company','root_domain','subdomain','ip','cidr','icp','keyword'))
);

CREATE TABLE IF NOT EXISTS agents (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    "key" TEXT NOT NULL UNIQUE CHECK (key GLOB '[a-z]*' AND key NOT GLOB '*[^a-z0-9_]*'),
    name TEXT NOT NULL,
    description TEXT,
    role TEXT NOT NULL,
    builtin INTEGER NOT NULL DEFAULT 1,
    enabled INTEGER NOT NULL DEFAULT 1,
    llm_profile_id INTEGER REFERENCES llm_profiles(id) ON DELETE SET NULL,
    current_prompt_id INTEGER,
    max_turns INTEGER NOT NULL DEFAULT 0,
    run_seconds INTEGER NOT NULL DEFAULT 1200,
    web_search INTEGER NOT NULL DEFAULT 0,
    interactive_shell INTEGER NOT NULL DEFAULT 0,
    wrapup_prompt TEXT NOT NULL DEFAULT '',
    wrapup_max_turns INTEGER NOT NULL DEFAULT 0,
    task_timeout_wrapup_prompt TEXT NOT NULL DEFAULT '',
    task_timeout_wrapup_max_turns INTEGER NOT NULL DEFAULT 0,
    trigger_run_mode TEXT NOT NULL DEFAULT 'serial' CHECK (trigger_run_mode IN ('serial','parallel')),
    trigger_merge_mode TEXT NOT NULL DEFAULT 'all' CHECK (trigger_merge_mode IN ('by_task','all','none')),
    trigger_max_parallel INTEGER NOT NULL DEFAULT 5,
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    CONSTRAINT agents_role_ck CHECK (role IN ('goals','main','planner','worker','assistant')),
    CONSTRAINT fk_agents_curprompt FOREIGN KEY (current_prompt_id) REFERENCES agent_prompts(id) ON DELETE SET NULL
);

CREATE TABLE IF NOT EXISTS agent_prompts (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    agent_id INTEGER NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    version INTEGER NOT NULL,
    template_text TEXT NOT NULL,
    note TEXT,
    updated_by TEXT,
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    UNIQUE (agent_id, version)
);

CREATE TABLE IF NOT EXISTS agent_prompt_vars (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    agent_id INTEGER NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    var_name TEXT NOT NULL,
    description TEXT,
    example TEXT,
    source TEXT NOT NULL CHECK (source IN ('exploration','runtime','distilled')),
    UNIQUE (agent_id, var_name)
);

CREATE TABLE IF NOT EXISTS mcp_servers (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    name TEXT NOT NULL UNIQUE,
    transport TEXT NOT NULL CHECK (transport IN ('stdio','http','sse')),
    command TEXT,
    args TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(args)),
    env TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(env)),
    url TEXT,
    enabled INTEGER NOT NULL DEFAULT 1,
    insecure INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    CONSTRAINT mcp_servers_transport_check CHECK (transport IN ('stdio','http','sse'))
);

CREATE TABLE IF NOT EXISTS mcp_tools_cache (
    id INTEGER PRIMARY KEY NOT NULL,
    server_id INTEGER NOT NULL REFERENCES mcp_servers(id) ON DELETE CASCADE,
    tool_name TEXT NOT NULL,
    description TEXT,
    "schema" TEXT CHECK (json_valid("schema")),
    discovered_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    UNIQUE (server_id, tool_name)
);

CREATE TABLE IF NOT EXISTS agent_visibility (
    agent_id INTEGER NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    resource_kind TEXT NOT NULL CHECK (resource_kind IN ('mcp')),
    resource_id INTEGER NOT NULL,
    mcp_tool_name TEXT NOT NULL DEFAULT '',
    enabled INTEGER NOT NULL DEFAULT 1,
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    PRIMARY KEY (agent_id, resource_kind, resource_id, mcp_tool_name)
);

CREATE TABLE IF NOT EXISTS agent_skill_visibility (
    agent_id INTEGER NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    skill_name TEXT NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 1,
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    PRIMARY KEY (agent_id, skill_name)
);

CREATE TABLE IF NOT EXISTS skill_usage (
    id INTEGER PRIMARY KEY NOT NULL,
    ts TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    skill TEXT NOT NULL,
    agent_key TEXT,
    task_id INTEGER,
    exploration_id INTEGER,
    intent_id INTEGER,
    session_id TEXT,
    args_len INTEGER NOT NULL DEFAULT 0,
    found INTEGER NOT NULL DEFAULT 1
);

CREATE TABLE IF NOT EXISTS tool_usage (
    id INTEGER PRIMARY KEY NOT NULL,
    ts TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    tool_key TEXT NOT NULL,
    agent_key TEXT,
    task_id INTEGER,
    exploration_id INTEGER,
    intent_id INTEGER,
    session_id TEXT
);

CREATE TABLE IF NOT EXISTS tools (
    "key" TEXT PRIMARY KEY,
    system INTEGER NOT NULL DEFAULT 1,
    description TEXT NOT NULL DEFAULT '',
    "schema" TEXT NOT NULL DEFAULT '{}' CHECK (json_valid("schema")),
    agents TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(agents)),
    enabled INTEGER NOT NULL DEFAULT 1,
    kind TEXT NOT NULL DEFAULT 'builtin',
    exec TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(exec)),
    deferred INTEGER NOT NULL DEFAULT 0,
    updated_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE TABLE IF NOT EXISTS conversations (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    agent_key TEXT NOT NULL,
    title TEXT NOT NULL DEFAULT '',
    llm_profile_id INTEGER REFERENCES llm_profiles(id) ON DELETE SET NULL,
    pinned_at TIMESTAMP,
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE TABLE IF NOT EXISTS conversation_activities (
    id INTEGER PRIMARY KEY NOT NULL,
    conversation_id INTEGER NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    worker TEXT,
    kind TEXT,
    tool TEXT,
    tool_use_id TEXT,
    is_error INTEGER NOT NULL DEFAULT 0,
    summary TEXT,
    detail TEXT,
    input_tokens INTEGER,
    output_tokens INTEGER,
    cache_read_tokens INTEGER,
    cache_write_tokens INTEGER,
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE TABLE IF NOT EXISTS agent_triggers (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    agent_key TEXT NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 1,
    interval_sec INTEGER NOT NULL DEFAULT 0,
    on_finding INTEGER NOT NULL DEFAULT 0,
    on_goal_met INTEGER NOT NULL DEFAULT 0,
    on_task_timeout INTEGER NOT NULL DEFAULT 0,
    on_tool_call INTEGER NOT NULL DEFAULT 0,
    on_task_create INTEGER NOT NULL DEFAULT 0,
    interval_message TEXT NOT NULL DEFAULT '',
    finding_message TEXT NOT NULL DEFAULT '',
    goal_message TEXT NOT NULL DEFAULT '',
    task_timeout_message TEXT NOT NULL DEFAULT '',
    tool_call_message TEXT NOT NULL DEFAULT '',
    task_create_message TEXT NOT NULL DEFAULT '',
    tool_names TEXT NOT NULL DEFAULT '',
    last_fire TIMESTAMP,
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE TABLE IF NOT EXISTS scheduler_state (
    "key" TEXT PRIMARY KEY,
    "value" TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS intercept_rules (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    name TEXT NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 1,
    priority INTEGER NOT NULL DEFAULT 0,
    match_target TEXT NOT NULL CHECK (match_target IN ('tool_name', 'tool_input', 'draft_field')),
    match_type TEXT NOT NULL CHECK (match_type IN ( 'string', 'regex', 'numeric_lte', 'list', 'time_window', 'daily_count')),
    pattern TEXT NOT NULL,
    pattern_params TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(pattern_params)),
    action TEXT NOT NULL CHECK (action IN ('allow', 'deny', 'ask')),
    message TEXT NOT NULL DEFAULT '',
    timeout_enabled INTEGER NOT NULL DEFAULT 1,
    timeout_seconds INTEGER NOT NULL DEFAULT 60,
    timeout_action TEXT NOT NULL DEFAULT 'deny',
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    CONSTRAINT intercept_rules_match_target_check CHECK (match_target IN ('tool_name', 'tool_input', 'draft_field')),
    CONSTRAINT intercept_rules_match_type_check CHECK (match_type IN ('string', 'regex', 'numeric_lte', 'list', 'time_window', 'daily_count'))
);

CREATE TABLE IF NOT EXISTS intercept_pending (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    rule_id INTEGER REFERENCES intercept_rules(id) ON DELETE SET NULL,
    conversation_id INTEGER REFERENCES conversations(id) ON DELETE CASCADE,
    task_id TEXT,
    agent_name TEXT NOT NULL DEFAULT '',
    tool_name TEXT NOT NULL,
    tool_input TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(tool_input)),
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'allowed', 'denied', 'timeout')),
    reason TEXT NOT NULL DEFAULT '',
    decided_at TIMESTAMP,
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    audit TEXT CHECK (json_valid(audit)),
    decision_source TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS findings (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    task_id INTEGER REFERENCES tasks(id) ON DELETE SET NULL,
    node_id INTEGER REFERENCES exploration_nodes(id) ON DELETE SET NULL,
    vulnclass TEXT NOT NULL DEFAULT '',
    name TEXT NOT NULL DEFAULT '',
    severity TEXT NOT NULL DEFAULT '',
    summary TEXT NOT NULL DEFAULT '',
    evidence TEXT NOT NULL DEFAULT '',
    worker TEXT NOT NULL DEFAULT '',
    asset_ids TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(asset_ids)),
    status TEXT NOT NULL DEFAULT 'pending',
    report TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    evidence_version INTEGER NOT NULL DEFAULT 0,
    report_evidence_version INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS traffic_evidence_snapshots (
    id TEXT PRIMARY KEY,
    source_traffic_id TEXT NOT NULL,
    captured_at INTEGER NOT NULL,
    url TEXT NOT NULL,
    method TEXT NOT NULL,
    status INTEGER NOT NULL,
    content_type TEXT NOT NULL DEFAULT '',
    req_head TEXT NOT NULL,
    resp_head TEXT NOT NULL,
    req_hash TEXT NOT NULL,
    resp_hash TEXT NOT NULL,
    req_len INTEGER NOT NULL,
    resp_len INTEGER NOT NULL,
    unreferenced_at TIMESTAMP,
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE TABLE IF NOT EXISTS finding_traffic_bindings (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    finding_id INTEGER NOT NULL REFERENCES findings(id) ON DELETE CASCADE,
    snapshot_id TEXT NOT NULL REFERENCES traffic_evidence_snapshots(id),
    role TEXT NOT NULL DEFAULT 'supporting',
    note TEXT NOT NULL DEFAULT '',
    position INTEGER NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    UNIQUE(finding_id, snapshot_id)
);

CREATE TABLE IF NOT EXISTS server_logs (
    id INTEGER PRIMARY KEY NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    level TEXT NOT NULL DEFAULT 'info',
    tag TEXT NOT NULL DEFAULT '',
    text TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS side_question_sessions (
    session_key TEXT PRIMARY KEY,
    conversation_id INTEGER REFERENCES conversations(id) ON DELETE CASCADE,
    task_id INTEGER REFERENCES tasks(id) ON DELETE CASCADE,
    exploration_id INTEGER REFERENCES explorations(id) ON DELETE CASCADE,
    intent_id INTEGER REFERENCES exploration_nodes(id) ON DELETE CASCADE,
    run_id INTEGER NOT NULL,
    version INTEGER NOT NULL,
    snapshot TEXT NOT NULL CHECK (json_valid(snapshot)),
    generation INTEGER NOT NULL DEFAULT 0,
    memory TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(memory)),
    CHECK ((conversation_id IS NOT NULL AND task_id IS NULL AND exploration_id IS NULL AND intent_id IS NULL) OR (conversation_id IS NULL AND task_id IS NOT NULL AND exploration_id IS NOT NULL))
);

CREATE TABLE IF NOT EXISTS side_question_requests (
    id TEXT PRIMARY KEY,
    ordinal INTEGER NOT NULL,
    session_key TEXT NOT NULL REFERENCES side_question_sessions(session_key) ON DELETE CASCADE,
    generation INTEGER NOT NULL,
    client_id TEXT NOT NULL,
    question TEXT NOT NULL,
    answer TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL CHECK (status IN ('running','completed','failed','cancelled','interrupted')),
    error TEXT NOT NULL DEFAULT '',
    model TEXT NOT NULL CHECK (json_valid(model)),
    snapshot_at TIMESTAMP NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    sequence INTEGER NOT NULL DEFAULT 0,
    usage TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(usage)),
    context_info TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(context_info)),
    UNIQUE(session_key,generation,client_id)
);

CREATE TABLE IF NOT EXISTS asset_intercept_rules (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 1,
    kind TEXT NOT NULL CHECK (kind IN ( 'exact_domain', 'exact_ip', 'exact_url', 'fuzzy_domain', 'fuzzy_ip', 'fuzzy_url', 'cidr')),
    pattern TEXT NOT NULL,
    note TEXT NOT NULL DEFAULT '',
    builtin INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE TABLE IF NOT EXISTS task_intercept_rules (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    enabled INTEGER NOT NULL DEFAULT 1,
    action TEXT NOT NULL DEFAULT 'block' CHECK (action IN ('block','allow')),
    kind TEXT NOT NULL CHECK (kind IN ( 'exact_domain', 'exact_ip', 'exact_url', 'fuzzy_domain', 'fuzzy_ip', 'fuzzy_url', 'cidr')),
    pattern TEXT NOT NULL,
    note TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE TABLE IF NOT EXISTS notification_channels (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    name TEXT NOT NULL,
    kind TEXT NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 1,
    config TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(config)),
    mode TEXT NOT NULL DEFAULT 'realtime',
    filter TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(filter)),
    rate_per_min INTEGER NOT NULL DEFAULT 20,
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE TABLE IF NOT EXISTS notification_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    kind TEXT NOT NULL,
    finding_id INTEGER NOT NULL,
    snapshot TEXT NOT NULL CHECK (json_valid(snapshot)),
    fanned_out INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE TABLE IF NOT EXISTS notification_deliveries (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    event_id INTEGER NOT NULL REFERENCES notification_events(id) ON DELETE CASCADE,
    channel_id INTEGER NOT NULL REFERENCES notification_channels(id) ON DELETE CASCADE,
    state TEXT NOT NULL DEFAULT 'pending',
    attempts INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    last_error TEXT NOT NULL DEFAULT '',
    batch_id INTEGER,
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    sent_at TIMESTAMP
);

CREATE TABLE IF NOT EXISTS instruments (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    market TEXT NOT NULL,
    symbol TEXT NOT NULL,
    name TEXT,
    asset_class TEXT NOT NULL,
    currency TEXT,
    exchange TEXT,
    sector TEXT,
    industry TEXT,
    active INTEGER NOT NULL DEFAULT 1,
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    CHECK ((asset_class IN ('stock', 'etf', 'index', 'crypto', 'future', 'option'))),
    CHECK ((market IN ('us', 'hk', 'cn', 'crypto'))),
    CONSTRAINT instruments_market_symbol_key UNIQUE (market, symbol),
    CONSTRAINT ck_instruments_symbol_not_test CHECK (symbol NOT LIKE 'GATE-TEST-%' AND symbol NOT LIKE 'RATING-FALLBACK%')
);

CREATE TABLE IF NOT EXISTS market_credentials (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    provider TEXT NOT NULL,
    account_label TEXT NOT NULL DEFAULT '',
    api_key TEXT NOT NULL DEFAULT '',
    api_secret TEXT NOT NULL DEFAULT '',
    passphrase TEXT NOT NULL DEFAULT '',
    api_key_hint TEXT NOT NULL DEFAULT '',
    base_url TEXT NOT NULL DEFAULT '',
    enabled INTEGER NOT NULL DEFAULT 1,
    verified_at TIMESTAMP,
    verify_error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    extra TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(extra)),
    kind TEXT NOT NULL DEFAULT 'trade',
    CONSTRAINT market_credentials_provider_kind_label_key UNIQUE (provider, kind, account_label),
    CONSTRAINT ck_market_credentials_kind CHECK (kind IN ('data','trade'))
);

CREATE TABLE IF NOT EXISTS watchlists (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    name TEXT NOT NULL,
    note TEXT,
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    CONSTRAINT watchlists_name_key UNIQUE (name)
);

CREATE TABLE IF NOT EXISTS bars (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    instrument_id INTEGER NOT NULL,
    "interval" TEXT NOT NULL DEFAULT '1d',
    ts TIMESTAMP NOT NULL,
    trading_date TEXT NOT NULL,
    open TEXT,
    high TEXT,
    low TEXT,
    close TEXT,
    adj_close TEXT,
    volume TEXT,
    snapshot_id INTEGER,
    is_partial INTEGER NOT NULL DEFAULT 0,
    source TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    CHECK (("interval" IN ('1d', '1h', '30m', '5m', '1m'))),
    CONSTRAINT bars_instrument_id_interval_ts_key UNIQUE (instrument_id, "interval", ts),
    CONSTRAINT bars_instrument_id_fkey FOREIGN KEY (instrument_id) REFERENCES instruments(id) ON DELETE CASCADE,
    CONSTRAINT bars_snapshot_id_fkey FOREIGN KEY (snapshot_id) REFERENCES snapshots(id) ON DELETE SET NULL
);

CREATE TABLE IF NOT EXISTS instrument_relations (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    instrument_id INTEGER NOT NULL,
    related_type TEXT NOT NULL,
    related_ref TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    CHECK ((related_type IN ('industry', 'macro', 'news_source', 'vendor', 'peer'))),
    CONSTRAINT instrument_relations_instrument_id_fkey FOREIGN KEY (instrument_id) REFERENCES instruments(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS snapshots (
    id INTEGER PRIMARY KEY NOT NULL,
    source TEXT NOT NULL,
    endpoint TEXT NOT NULL,
    params_hash TEXT NOT NULL,
    collected_at TIMESTAMP NOT NULL,
    body_ref TEXT,
    checksum TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    coverage_start TEXT,
    coverage_end TEXT,
    bar_count INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS signals (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    task_id INTEGER,
    instrument_id INTEGER,
    node_id INTEGER,
    signal_type TEXT NOT NULL,
    direction TEXT NOT NULL,
    confidence TEXT,
    horizon TEXT,
    rationale TEXT,
    status TEXT NOT NULL DEFAULT 'open',
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    action TEXT,
    CHECK (((action IS NULL) OR (action IN ('entry', 'exit', 'scale_in', 'scale_out', 'hold', 'review')))),
    CHECK ((direction IN ('long', 'short', 'neutral'))),
    CONSTRAINT signals_instrument_id_fkey FOREIGN KEY (instrument_id) REFERENCES instruments(id)
);

CREATE TABLE IF NOT EXISTS signal_settlements (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    signal_id INTEGER NOT NULL,
    settle_date TEXT NOT NULL,
    entry_ref TEXT,
    exit_ref TEXT,
    return_pct TEXT,
    benchmark_return_pct TEXT,
    alpha TEXT,
    hit INTEGER,
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    CONSTRAINT signal_settlements_signal_id_settle_date_key UNIQUE (signal_id, settle_date),
    CONSTRAINT signal_settlements_signal_id_fkey FOREIGN KEY (signal_id) REFERENCES signals(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS ratings (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    task_id INTEGER,
    instrument_id INTEGER,
    intent_id INTEGER REFERENCES exploration_nodes(id) ON DELETE SET NULL,
    rating TEXT NOT NULL,
    confidence TEXT,
    decided_at TIMESTAMP,
    evidence_node_ids TEXT CHECK (json_valid(evidence_node_ids)),
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    confidence_reason TEXT,
    review_reasons TEXT CHECK (json_valid(review_reasons)),
    CHECK ((((rating = 'review') AND (review_reasons IS NOT NULL) AND (json_type(review_reasons,'$.data_gaps') IS NOT NULL) AND (json_type(review_reasons,'$.conflicting_facts') IS NOT NULL) AND (json_type(review_reasons,'$.skipped_analysts') IS NOT NULL) AND (json_type(review_reasons,'$.next_step') IS NOT NULL)) OR ((rating <> 'review') AND (review_reasons IS NULL)))),
    CHECK ((rating IN ('buy', 'overweight', 'hold', 'underweight', 'sell', 'review'))),
    CONSTRAINT ratings_instrument_id_fkey FOREIGN KEY (instrument_id) REFERENCES instruments(id)
);

CREATE TABLE IF NOT EXISTS watchlist_items (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    watchlist_id INTEGER NOT NULL,
    instrument_id INTEGER NOT NULL,
    note TEXT,
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    CONSTRAINT watchlist_items_watchlist_id_instrument_id_key UNIQUE (watchlist_id, instrument_id),
    CONSTRAINT watchlist_items_instrument_id_fkey FOREIGN KEY (instrument_id) REFERENCES instruments(id) ON DELETE CASCADE,
    CONSTRAINT watchlist_items_watchlist_id_fkey FOREIGN KEY (watchlist_id) REFERENCES watchlists(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS exec_accounts (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    channel TEXT NOT NULL,
    channel_account_id TEXT NOT NULL,
    label TEXT NOT NULL DEFAULT '',
    market TEXT NOT NULL,
    market_support TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(market_support)),
    active INTEGER NOT NULL DEFAULT 1,
    blocked_at TIMESTAMP,
    block_reason TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    CHECK ((channel IN ('fosunwealth', 'okx'))),
    CHECK ((market IN ('us', 'hk', 'cn', 'crypto'))),
    CHECK ((replace(replace(replace(replace(market_support,'"us"',''),'"hk"',''),'"cn"',''),'"crypto"','') NOT LIKE '%"%')),
    CONSTRAINT exec_accounts_channel_channel_account_id_key UNIQUE (channel, channel_account_id)
);

CREATE TABLE IF NOT EXISTS exec_authorizations (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    channel TEXT NOT NULL,
    account_ref TEXT NOT NULL,
    mode TEXT NOT NULL,
    from_mode TEXT NOT NULL,
    disclaimer_version TEXT NOT NULL,
    confirm_phrase TEXT NOT NULL,
    limits_snapshot TEXT NOT NULL CHECK (json_valid(limits_snapshot)),
    rules_revision INTEGER NOT NULL,
    granted_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    granted_by TEXT NOT NULL DEFAULT 'local',
    revoked_at TIMESTAMP,
    revoked_reason TEXT NOT NULL DEFAULT '',
    CHECK ((channel IN ('fosunwealth', 'okx'))),
    CHECK ((from_mode IN ('paper', 'live_manual', 'live_whitelist', 'live_auto'))),
    CHECK ((mode IN ('live_whitelist', 'live_manual', 'live_auto')))
);

CREATE TABLE IF NOT EXISTS execution_limits (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    "key" TEXT NOT NULL,
    mode TEXT NOT NULL DEFAULT 'paper',
    max_single_amount TEXT,
    max_daily_amount TEXT,
    max_daily_orders INTEGER,
    whitelist_symbols TEXT CHECK (json_valid(whitelist_symbols)),
    updated_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    revision INTEGER NOT NULL DEFAULT 1,
    CHECK ((mode IN ('paper', 'live_whitelist', 'live_manual', 'live_auto'))),
    CONSTRAINT execution_limits_key_key UNIQUE (key)
);

CREATE TABLE IF NOT EXISTS orders_draft (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    signal_id INTEGER,
    rating_id INTEGER REFERENCES ratings(id) ON DELETE SET NULL,
    instrument_id INTEGER,
    side TEXT NOT NULL,
    qty TEXT NOT NULL,
    ref_price TEXT,
    risk_verdict TEXT,
    exported_at TIMESTAMP,
    executed_note TEXT,
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    CHECK ((risk_verdict IN ('pass', 'deny', 'ask'))),
    CHECK ((side IN ('buy', 'sell'))),
    CONSTRAINT orders_draft_instrument_id_fkey FOREIGN KEY (instrument_id) REFERENCES instruments(id),
    CONSTRAINT orders_draft_signal_id_fkey FOREIGN KEY (signal_id) REFERENCES signals(id)
);

CREATE TABLE IF NOT EXISTS draft_risk_verdicts (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    draft_id INTEGER NOT NULL,
    verdict TEXT NOT NULL,
    matched_rule_ids TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(matched_rule_ids)),
    rule_results TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(rule_results)),
    reason TEXT NOT NULL DEFAULT '',
    rules_revision INTEGER NOT NULL DEFAULT 0,
    rules_snapshot TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(rules_snapshot)),
    evaluated_by TEXT NOT NULL DEFAULT '',
    evaluated_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    CHECK ((verdict IN ('pass', 'deny', 'ask'))),
    CONSTRAINT draft_risk_verdicts_draft_id_fkey FOREIGN KEY (draft_id) REFERENCES orders_draft(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS orders_execution (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    draft_id INTEGER NOT NULL,
    account_id INTEGER NOT NULL,
    mode TEXT NOT NULL,
    state TEXT NOT NULL,
    risk_verdict_id INTEGER,
    client_order_id TEXT NOT NULL,
    channel_order_id TEXT,
    qty TEXT NOT NULL,
    filled_qty TEXT NOT NULL DEFAULT 0,
    avg_price TEXT,
    raw_ack TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(raw_ack)),
    state_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    CHECK (((CAST(filled_qty AS REAL) <= CAST(qty AS REAL)) OR (state IN ('cancelled', 'settled')))),
    CHECK ((CAST(filled_qty AS REAL) >= (0))),
    CHECK ((mode IN ('paper', 'live_whitelist', 'live_manual', 'live_auto'))),
    CHECK ((CAST(qty AS REAL) > (0))),
    CHECK ((state IN ('draft', 'risk_pending', 'risk_passed', 'risk_denied', 'submitted', 'partial', 'filled', 'rejected', 'cancel_requested', 'cancelled', 'settled'))),
    CONSTRAINT orders_execution_client_order_id_key UNIQUE (client_order_id),
    CONSTRAINT orders_execution_account_id_fkey FOREIGN KEY (account_id) REFERENCES exec_accounts(id) ON DELETE RESTRICT,
    CONSTRAINT orders_execution_draft_id_fkey FOREIGN KEY (draft_id) REFERENCES orders_draft(id) ON DELETE RESTRICT,
    CONSTRAINT orders_execution_risk_verdict_id_fkey FOREIGN KEY (risk_verdict_id) REFERENCES draft_risk_verdicts(id)
);

CREATE TABLE IF NOT EXISTS paper_orders (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    task_id INTEGER,
    signal_id INTEGER,
    instrument_id INTEGER,
    account_id INTEGER REFERENCES exec_accounts(id) ON DELETE RESTRICT,
    mode TEXT NOT NULL,
    side TEXT NOT NULL,
    qty TEXT NOT NULL,
    price TEXT,
    filled_qty TEXT NOT NULL DEFAULT 0,
    avg_fill_price TEXT,
    status TEXT NOT NULL DEFAULT 'draft',
    broker_order_id TEXT,
    submitted_at TIMESTAMP,
    filled_at TIMESTAMP,
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    CHECK ((mode IN ('paper', 'live'))),
    CHECK ((side IN ('buy', 'sell'))),
    CHECK ((status IN ('draft', 'risk_passed', 'submitted', 'filled', 'partial', 'rejected', 'cancelled', 'settled'))),
    CONSTRAINT paper_orders_instrument_id_fkey FOREIGN KEY (instrument_id) REFERENCES instruments(id),
    CONSTRAINT paper_orders_signal_id_fkey FOREIGN KEY (signal_id) REFERENCES signals(id)
);

CREATE TABLE IF NOT EXISTS exec_reconciliations (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    execution_id INTEGER,
    account_id INTEGER NOT NULL,
    kind TEXT NOT NULL,
    local_snapshot TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(local_snapshot)),
    remote_snapshot TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(remote_snapshot)),
    diff TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(diff)),
    resolved_at TIMESTAMP,
    resolved_by TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    CHECK ((kind IN ('state', 'amount', 'missing_local', 'missing_remote', 'query_failed'))),
    CONSTRAINT exec_reconciliations_account_id_fkey FOREIGN KEY (account_id) REFERENCES exec_accounts(id),
    CONSTRAINT exec_reconciliations_execution_id_fkey FOREIGN KEY (execution_id) REFERENCES orders_execution(id) ON DELETE RESTRICT
);

CREATE TABLE IF NOT EXISTS exec_audit (
    id INTEGER PRIMARY KEY NOT NULL,
    action TEXT NOT NULL,
    detail TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(detail)),
    actor TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    CONSTRAINT exec_audit_action_check CHECK (action IN ( 'exec_mode_change', 'exec_authorize', 'exec_unfreeze', 'exec_frozen', 'exec_limits_change' ))
);

-- ============================================================
-- 阶段 2：索引
--
-- 删掉了 PG 专用的 GIN/GIST 索引（SQLite 无等价物）：
--   idx_av2_task_ids / idx_av2_c_segment / idx_av2_technologies /
--   idx_av2_bound_domains / idx_av2_open_ports / idx_sv2_net /
--   idx_task_archives_sources / idx_ts_net / idx_findings_asset_ids
-- 这些列在 SQLite 里是 JSON 文本 / TEXT，靠 json_each 与 UDF 查询，无法索引。
-- 数据量上去后（> 5 万 assets）再考虑生成列 + 普通索引。
-- ============================================================

CREATE INDEX IF NOT EXISTS idx_companies_nkey ON companies(nkey);
CREATE UNIQUE INDEX IF NOT EXISTS uq_av2_root_domain ON assets(domain) WHERE type = 'root_domain';
CREATE UNIQUE INDEX IF NOT EXISTS uq_av2_ip ON assets(ip) WHERE type = 'ip';
CREATE UNIQUE INDEX IF NOT EXISTS uq_av2_subdomain ON assets(domain, COALESCE(record_type,'')) WHERE type = 'subdomain';
CREATE UNIQUE INDEX IF NOT EXISTS uq_av2_app_bundle ON assets(bundle_id) WHERE type = 'app' AND bundle_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_av2_app_name ON assets(app_name) WHERE type = 'app' AND bundle_id IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_av2_service_http ON assets(url) WHERE type = 'service' AND service_type = 'http';
CREATE UNIQUE INDEX IF NOT EXISTS uq_av2_service_other ON assets(COALESCE(domain,''), COALESCE(ip,''), port, service_name) WHERE type = 'service' AND service_type = 'other';
CREATE UNIQUE INDEX IF NOT EXISTS uq_av2_endpoint ON assets(url, method) WHERE type = 'endpoint';
CREATE INDEX IF NOT EXISTS idx_av2_company ON assets(company_id) WHERE company_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_av2_company_type ON assets(company_id, type) WHERE company_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_av2_domain ON assets(domain) WHERE domain IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_av2_root_domain ON assets(root_domain) WHERE root_domain IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_av2_ip ON assets(ip) WHERE ip IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_av2_last_seen ON assets(last_seen DESC);
CREATE INDEX IF NOT EXISTS idx_av2_type_seen ON assets(type, last_seen DESC);
CREATE INDEX IF NOT EXISTS idx_sv2_domain ON company_scope(domain) WHERE kind = 'domain';
CREATE UNIQUE INDEX IF NOT EXISTS uq_sv2_value ON company_scope(company_id, kind, value) WHERE kind IN ('icp','keyword');
CREATE INDEX IF NOT EXISTS idx_sv2_icp ON company_scope(value) WHERE kind = 'icp';
CREATE INDEX IF NOT EXISTS idx_sv2_company ON company_scope(company_id);
CREATE INDEX IF NOT EXISTS idx_expnodes_part ON exploration_nodes(exploration_id, kind);
CREATE INDEX IF NOT EXISTS idx_expnodes_frontier ON exploration_nodes(exploration_id, priority DESC) WHERE kind='intent' AND state='open';
CREATE INDEX IF NOT EXISTS idx_expedges_src ON exploration_edges(src_id, rel);
CREATE INDEX IF NOT EXISTS idx_expedges_dst ON exploration_edges(dst_id, rel);
CREATE INDEX IF NOT EXISTS idx_anchor_asset ON exploration_anchors(asset_id);
CREATE INDEX IF NOT EXISTS idx_task_constraints_exp ON task_constraints(exploration_id);
CREATE INDEX IF NOT EXISTS idx_act_node ON activity(exploration_id, node_id, id);
CREATE INDEX IF NOT EXISTS idx_act_since ON activity(exploration_id, id);
CREATE INDEX IF NOT EXISTS idx_act_tool_call ON activity(exploration_id, tool_use_id, id) WHERE kind IN ('tool_use', 'tool_result');
CREATE INDEX IF NOT EXISTS idx_act_worker ON activity(exploration_id, worker, id);
CREATE INDEX IF NOT EXISTS idx_act_main_seg ON activity(exploration_id, main_seg, id) WHERE worker='mainagent';
CREATE INDEX IF NOT EXISTS idx_act_result_usage ON activity(exploration_id);
CREATE INDEX IF NOT EXISTS idx_act_latest ON activity(exploration_id, created_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS uq_llm_one_default ON llm_profiles(is_default) WHERE is_default;
CREATE INDEX IF NOT EXISTS idx_task_categories_name ON task_categories(name, id);
CREATE INDEX IF NOT EXISTS idx_tasks_alive ON tasks(created_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(status) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_tasks_category ON tasks(category_id, created_at DESC) WHERE deleted_at IS NULL AND category_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_tasks_pinned ON tasks(pinned_at DESC) WHERE deleted_at IS NULL AND pinned_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_tasks_archived ON tasks(archived_at DESC) WHERE archived_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_task_archives_state ON task_archives(state, requested_at, id);
CREATE INDEX IF NOT EXISTS idx_task_archives_archived ON task_archives(archived_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_task_templates_updated ON task_templates(updated_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_task_relations_source ON task_relations(source_task_id);
CREATE INDEX IF NOT EXISTS idx_task_asset_links_asset ON task_asset_links(asset_id, task_id);
CREATE INDEX IF NOT EXISTS idx_task_asset_links_node ON task_asset_links(source_node_id) WHERE source_node_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_task_llm_profiles_order ON task_llm_profiles(task_id, position);
CREATE INDEX IF NOT EXISTS idx_task_llm_profiles_profile ON task_llm_profiles(profile_id, task_id);
CREATE INDEX IF NOT EXISTS idx_tasks_llm_profile ON tasks(llm_profile_id) WHERE llm_profile_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_tasks_active_llm_profile ON tasks(active_llm_profile_id) WHERE active_llm_profile_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_task_scope_v2 ON task_scope(task_id, kind, COALESCE(domain,''), COALESCE(net,''), COALESCE(company_id,0), COALESCE(value,''));
CREATE INDEX IF NOT EXISTS idx_ts_domain ON task_scope(domain) WHERE kind IN ('root_domain','subdomain');
CREATE INDEX IF NOT EXISTS idx_ts_company ON task_scope(company_id) WHERE kind = 'company';
CREATE INDEX IF NOT EXISTS idx_agents_llm_profile ON agents(llm_profile_id) WHERE llm_profile_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_vis_resource ON agent_visibility(resource_kind, resource_id);
CREATE INDEX IF NOT EXISTS idx_askv_skill ON agent_skill_visibility(skill_name);
CREATE INDEX IF NOT EXISTS idx_skill_usage_skill ON skill_usage(skill, ts DESC);
CREATE INDEX IF NOT EXISTS idx_skill_usage_task ON skill_usage(task_id);
CREATE INDEX IF NOT EXISTS idx_tool_usage_tool ON tool_usage(tool_key, ts DESC);
CREATE INDEX IF NOT EXISTS idx_tool_usage_task ON tool_usage(task_id);
CREATE INDEX IF NOT EXISTS idx_conversations_llm_profile ON conversations(llm_profile_id) WHERE llm_profile_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_conversations_pinned ON conversations(pinned_at DESC) WHERE pinned_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_conv_act ON conversation_activities(conversation_id, id);
CREATE INDEX IF NOT EXISTS idx_conv_act_tool_call ON conversation_activities(conversation_id, tool_use_id, id) WHERE kind IN ('tool_use', 'tool_result');
CREATE INDEX IF NOT EXISTS idx_agent_triggers_agent ON agent_triggers(agent_key);
CREATE INDEX IF NOT EXISTS idx_intercept_pending_status ON intercept_pending(status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_intercept_pending_task ON intercept_pending(task_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_findings_task ON findings(task_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_findings_time ON findings(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_findings_status ON findings(status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_finding_traffic_order ON finding_traffic_bindings(finding_id, position, id);
CREATE INDEX IF NOT EXISTS idx_finding_traffic_snapshot ON finding_traffic_bindings(snapshot_id);
CREATE INDEX IF NOT EXISTS idx_server_logs_id ON server_logs(id DESC);
CREATE INDEX IF NOT EXISTS idx_side_sessions_conv ON side_question_sessions(conversation_id);
CREATE INDEX IF NOT EXISTS idx_side_sessions_task ON side_question_sessions(task_id);
CREATE INDEX IF NOT EXISTS idx_side_sessions_exp ON side_question_sessions(exploration_id);
CREATE INDEX IF NOT EXISTS idx_side_sessions_intent ON side_question_sessions(intent_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_side_request_running ON side_question_requests(session_key) WHERE status='running';
CREATE INDEX IF NOT EXISTS idx_side_requests_history ON side_question_requests(session_key, ordinal DESC);
CREATE INDEX IF NOT EXISTS idx_asset_intercept_enabled ON asset_intercept_rules(enabled);
CREATE INDEX IF NOT EXISTS idx_task_intercept_task ON task_intercept_rules(task_id);
CREATE INDEX IF NOT EXISTS idx_notification_events_pending ON notification_events(id) WHERE NOT fanned_out;
CREATE INDEX IF NOT EXISTS idx_notification_deliveries_due ON notification_deliveries(next_attempt_at) WHERE state='pending';
CREATE INDEX IF NOT EXISTS idx_notification_deliveries_history ON notification_deliveries(id DESC);
CREATE INDEX IF NOT EXISTS idx_notification_deliveries_batch ON notification_deliveries(batch_id) WHERE batch_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_notification_deliveries_channel ON notification_deliveries(channel_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_instruments_market_active ON instruments(market, active);
CREATE INDEX IF NOT EXISTS idx_instruments_sector ON instruments(sector) WHERE (sector IS NOT NULL);
CREATE INDEX IF NOT EXISTS idx_market_credentials_provider ON market_credentials(provider);
CREATE INDEX IF NOT EXISTS idx_bars_partial ON bars(instrument_id) WHERE is_partial;
CREATE INDEX IF NOT EXISTS idx_bars_slice ON bars(instrument_id, "interval", trading_date DESC);
CREATE INDEX IF NOT EXISTS idx_bars_snapshot ON bars(snapshot_id) WHERE (snapshot_id IS NOT NULL);
CREATE INDEX IF NOT EXISTS idx_bars_ts ON bars(instrument_id, "interval", ts DESC);
CREATE INDEX IF NOT EXISTS idx_instrument_relations_instrument ON instrument_relations(instrument_id, related_type);
CREATE UNIQUE INDEX IF NOT EXISTS uq_instrument_relations_edge ON instrument_relations(instrument_id, related_type, related_ref);
CREATE INDEX IF NOT EXISTS idx_snapshots_collected ON snapshots(collected_at DESC);
CREATE INDEX IF NOT EXISTS idx_snapshots_coverage ON snapshots(source, endpoint, coverage_end DESC) WHERE (coverage_end IS NOT NULL);
CREATE UNIQUE INDEX IF NOT EXISTS uq_snapshots_dedup ON snapshots(source, endpoint, params_hash, substr(collected_at,1,19));
CREATE INDEX IF NOT EXISTS idx_signals_instrument ON signals(instrument_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_signals_status ON signals(status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_signals_task ON signals(task_id) WHERE (task_id IS NOT NULL);
CREATE INDEX IF NOT EXISTS idx_signal_settlements_date ON signal_settlements(settle_date);
CREATE INDEX IF NOT EXISTS idx_ratings_instrument ON ratings(instrument_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_ratings_task ON ratings(task_id) WHERE (task_id IS NOT NULL);
CREATE INDEX IF NOT EXISTS idx_ratings_intent ON ratings(intent_id) WHERE (intent_id IS NOT NULL);
CREATE INDEX IF NOT EXISTS idx_watchlist_items_instrument ON watchlist_items(instrument_id);
CREATE INDEX IF NOT EXISTS idx_exec_accounts_blocked ON exec_accounts(blocked_at) WHERE (blocked_at IS NOT NULL);
CREATE UNIQUE INDEX IF NOT EXISTS uq_auth_active ON exec_authorizations(channel, account_ref) WHERE (revoked_at IS NULL);
CREATE INDEX IF NOT EXISTS idx_orders_draft_pending ON orders_draft(created_at DESC) WHERE (exported_at IS NULL);
CREATE UNIQUE INDEX IF NOT EXISTS uq_orders_draft_rating ON orders_draft(rating_id) WHERE rating_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_orders_draft_signal ON orders_draft(signal_id);
CREATE INDEX IF NOT EXISTS idx_verdicts_draft ON draft_risk_verdicts(draft_id, evaluated_at DESC);
CREATE INDEX IF NOT EXISTS idx_exec_acct ON orders_execution(account_id, state_at DESC);
CREATE INDEX IF NOT EXISTS idx_exec_draft ON orders_execution(draft_id);
CREATE INDEX IF NOT EXISTS idx_exec_state ON orders_execution(state, state_at);
CREATE INDEX IF NOT EXISTS idx_paper_orders_instrument ON paper_orders(instrument_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_paper_orders_status ON paper_orders(status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_paper_orders_task ON paper_orders(task_id) WHERE (task_id IS NOT NULL);
CREATE UNIQUE INDEX IF NOT EXISTS uq_paper_orders_broker ON paper_orders(broker_order_id) WHERE (broker_order_id IS NOT NULL);
CREATE INDEX IF NOT EXISTS idx_recon_acct ON exec_reconciliations(account_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_recon_open ON exec_reconciliations(created_at DESC) WHERE (resolved_at IS NULL);
CREATE INDEX IF NOT EXISTS idx_paper_orders_account ON paper_orders(account_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_tasks_instrument ON tasks(instrument_id) WHERE instrument_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_exec_audit_time ON exec_audit(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_exec_audit_action ON exec_audit(action, created_at DESC);

-- ============================================================
-- 阶段 3：触发器
--
-- PG 的 23 个触发器在这里变成 25 个，原因有二（实测，见 §8）：
--   1. SQLite 的 BEFORE 不能改 NEW -> 全部改 AFTER UPDATE + 自写一行。
--   2. `AFTER INSERT OR UPDATE OF col` 语法非法（near "OR"）
--      -> 两个业务触发器各拆成 2 条。
-- 21 + 2 + 2 = 25。
-- ============================================================

-- ---------- 21 个 updated_at 自动维护 ----------
--
-- WHEN NEW.updated_at IS OLD.updated_at 是必须的：
-- 自写的那一行也会触发本触发器，守卫让第二次进来时条件不成立。
-- 用 rowid=NEW.rowid 而不是主键列，这样复合主键表（task_llm_profiles）
-- 也能用同一套模板。

DROP TRIGGER IF EXISTS trg_companies_upd;
CREATE TRIGGER trg_companies_upd AFTER UPDATE ON companies FOR EACH ROW
WHEN NEW.updated_at IS OLD.updated_at
BEGIN
    UPDATE companies SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE rowid = NEW.rowid;
END;

DROP TRIGGER IF EXISTS trg_av2_upd;
CREATE TRIGGER trg_av2_upd AFTER UPDATE ON assets FOR EACH ROW
WHEN NEW.updated_at IS OLD.updated_at
BEGIN
    UPDATE assets SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE rowid = NEW.rowid;
END;

DROP TRIGGER IF EXISTS trg_exp_upd;
CREATE TRIGGER trg_exp_upd AFTER UPDATE ON explorations FOR EACH ROW
WHEN NEW.updated_at IS OLD.updated_at
BEGIN
    UPDATE explorations SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE rowid = NEW.rowid;
END;

DROP TRIGGER IF EXISTS trg_expnodes_upd;
CREATE TRIGGER trg_expnodes_upd AFTER UPDATE ON exploration_nodes FOR EACH ROW
WHEN NEW.updated_at IS OLD.updated_at
BEGIN
    UPDATE exploration_nodes SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE rowid = NEW.rowid;
END;

DROP TRIGGER IF EXISTS trg_llm_upd;
CREATE TRIGGER trg_llm_upd AFTER UPDATE ON llm_profiles FOR EACH ROW
WHEN NEW.updated_at IS OLD.updated_at
BEGIN
    UPDATE llm_profiles SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE rowid = NEW.rowid;
END;

DROP TRIGGER IF EXISTS trg_task_categories_upd;
CREATE TRIGGER trg_task_categories_upd AFTER UPDATE ON task_categories FOR EACH ROW
WHEN NEW.updated_at IS OLD.updated_at
BEGIN
    UPDATE task_categories SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE rowid = NEW.rowid;
END;

DROP TRIGGER IF EXISTS trg_tasks_upd;
CREATE TRIGGER trg_tasks_upd AFTER UPDATE ON tasks FOR EACH ROW
WHEN NEW.updated_at IS OLD.updated_at
BEGIN
    UPDATE tasks SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE rowid = NEW.rowid;
END;

DROP TRIGGER IF EXISTS trg_task_archives_upd;
CREATE TRIGGER trg_task_archives_upd AFTER UPDATE ON task_archives FOR EACH ROW
WHEN NEW.updated_at IS OLD.updated_at
BEGIN
    UPDATE task_archives SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE rowid = NEW.rowid;
END;

DROP TRIGGER IF EXISTS trg_task_templates_upd;
CREATE TRIGGER trg_task_templates_upd AFTER UPDATE ON task_templates FOR EACH ROW
WHEN NEW.updated_at IS OLD.updated_at
BEGIN
    UPDATE task_templates SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE rowid = NEW.rowid;
END;

DROP TRIGGER IF EXISTS trg_task_asset_links_upd;
CREATE TRIGGER trg_task_asset_links_upd AFTER UPDATE ON task_asset_links FOR EACH ROW
WHEN NEW.updated_at IS OLD.updated_at
BEGIN
    UPDATE task_asset_links SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE rowid = NEW.rowid;
END;

DROP TRIGGER IF EXISTS trg_task_llm_profiles_upd;
CREATE TRIGGER trg_task_llm_profiles_upd AFTER UPDATE ON task_llm_profiles FOR EACH ROW
WHEN NEW.updated_at IS OLD.updated_at
BEGIN
    UPDATE task_llm_profiles SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE rowid = NEW.rowid;
END;

DROP TRIGGER IF EXISTS trg_agents_upd;
CREATE TRIGGER trg_agents_upd AFTER UPDATE ON agents FOR EACH ROW
WHEN NEW.updated_at IS OLD.updated_at
BEGIN
    UPDATE agents SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE rowid = NEW.rowid;
END;

DROP TRIGGER IF EXISTS trg_mcp_upd;
CREATE TRIGGER trg_mcp_upd AFTER UPDATE ON mcp_servers FOR EACH ROW
WHEN NEW.updated_at IS OLD.updated_at
BEGIN
    UPDATE mcp_servers SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE rowid = NEW.rowid;
END;

DROP TRIGGER IF EXISTS trg_tools_upd;
CREATE TRIGGER trg_tools_upd AFTER UPDATE ON tools FOR EACH ROW
WHEN NEW.updated_at IS OLD.updated_at
BEGIN
    UPDATE tools SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE rowid = NEW.rowid;
END;

DROP TRIGGER IF EXISTS trg_conversations_upd;
CREATE TRIGGER trg_conversations_upd AFTER UPDATE ON conversations FOR EACH ROW
WHEN NEW.updated_at IS OLD.updated_at
BEGIN
    UPDATE conversations SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE rowid = NEW.rowid;
END;

DROP TRIGGER IF EXISTS trg_agent_triggers_upd;
CREATE TRIGGER trg_agent_triggers_upd AFTER UPDATE ON agent_triggers FOR EACH ROW
WHEN NEW.updated_at IS OLD.updated_at
BEGIN
    UPDATE agent_triggers SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE rowid = NEW.rowid;
END;

DROP TRIGGER IF EXISTS trg_intercept_rules_upd;
CREATE TRIGGER trg_intercept_rules_upd AFTER UPDATE ON intercept_rules FOR EACH ROW
WHEN NEW.updated_at IS OLD.updated_at
BEGIN
    UPDATE intercept_rules SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE rowid = NEW.rowid;
END;

DROP TRIGGER IF EXISTS trg_asset_intercept_rules_upd;
CREATE TRIGGER trg_asset_intercept_rules_upd AFTER UPDATE ON asset_intercept_rules FOR EACH ROW
WHEN NEW.updated_at IS OLD.updated_at
BEGIN
    UPDATE asset_intercept_rules SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE rowid = NEW.rowid;
END;

DROP TRIGGER IF EXISTS trg_task_intercept_rules_upd;
CREATE TRIGGER trg_task_intercept_rules_upd AFTER UPDATE ON task_intercept_rules FOR EACH ROW
WHEN NEW.updated_at IS OLD.updated_at
BEGIN
    UPDATE task_intercept_rules SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE rowid = NEW.rowid;
END;

DROP TRIGGER IF EXISTS trg_notification_channels_upd;
CREATE TRIGGER trg_notification_channels_upd AFTER UPDATE ON notification_channels FOR EACH ROW
WHEN NEW.updated_at IS OLD.updated_at
BEGIN
    UPDATE notification_channels SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE rowid = NEW.rowid;
END;

DROP TRIGGER IF EXISTS trg_market_credentials_upd;
CREATE TRIGGER trg_market_credentials_upd AFTER UPDATE ON market_credentials FOR EACH ROW
WHEN NEW.updated_at IS OLD.updated_at
BEGIN
    UPDATE market_credentials SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE rowid = NEW.rowid;
END;

-- ---------- 业务触发器 1：exploration_nodes.owner 派生 ----------
--
-- PG 原语义（BEFORE INSERT OR UPDATE OF payload）：
--   IF NEW.owner IS NULL THEN NEW.owner := NULLIF(TRIM(payload->>'role'), ''); END IF;
-- 四条被测试锁住的语义（db/exploration_test.go）：
--   ① 不写 owner 时从 payload.role 派生
--   ② 无 role -> NULL 而不是 ''
--   ③ 空白 role -> NULL
--   ④ 显式传的 owner 不被覆盖
-- 拆成两条（INSERT / UPDATE OF payload）是语法强制，不是选择。
--
-- ⚠️ INSERT ... RETURNING owner 拿不到这里写入的值（AFTER 触发器的局限，
--    见 §2.10 第 3 条）。调用方需要派生值时请 INSERT 后再 SELECT。

DROP TRIGGER IF EXISTS trg_expnodes_owner_ins;
CREATE TRIGGER trg_expnodes_owner_ins AFTER INSERT ON exploration_nodes FOR EACH ROW
WHEN NEW.owner IS NULL
BEGIN
    UPDATE exploration_nodes
       SET owner = NULLIF(trim(json_extract(NEW.payload,'$.role')), '')
     WHERE id = NEW.id AND owner IS NULL;
END;

DROP TRIGGER IF EXISTS trg_expnodes_owner_upd;
CREATE TRIGGER trg_expnodes_owner_upd AFTER UPDATE OF payload ON exploration_nodes FOR EACH ROW
BEGIN
    UPDATE exploration_nodes
       SET owner = NULLIF(trim(json_extract(NEW.payload,'$.role')), '')
     WHERE id = NEW.id AND owner IS NULL;
END;

-- ---------- 业务触发器 2：assets.task_ids -> task_asset_links ----------
--
-- PG 原语义（AFTER INSERT OR UPDATE OF task_ids）：
--   补行：unnest(NEW.task_ids) 里指向存活任务（deleted_at IS NULL）的
--   删行：不再被 NEW.task_ids 引用的
-- SQLite 侧数组是 JSON 文本，用 json_each 展开；
-- INSERT OR IGNORE 依赖 task_asset_links 的 PRIMARY KEY (task_id, asset_id)。

DROP TRIGGER IF EXISTS trg_assets_task_links_ins;
CREATE TRIGGER trg_assets_task_links_ins AFTER INSERT ON assets FOR EACH ROW
BEGIN
    INSERT OR IGNORE INTO task_asset_links(task_id, asset_id, source, source_summary)
      SELECT CAST(value AS INTEGER), NEW.id, 'system', '任务执行期间自动关联'
        FROM json_each(NEW.task_ids)
       WHERE CAST(value AS INTEGER) IN (SELECT id FROM tasks WHERE deleted_at IS NULL);
END;

DROP TRIGGER IF EXISTS trg_assets_task_links_upd;
CREATE TRIGGER trg_assets_task_links_upd AFTER UPDATE OF task_ids ON assets FOR EACH ROW
BEGIN
    INSERT OR IGNORE INTO task_asset_links(task_id, asset_id, source, source_summary)
      SELECT CAST(value AS INTEGER), NEW.id, 'system', '任务执行期间自动关联'
        FROM json_each(NEW.task_ids)
       WHERE CAST(value AS INTEGER) IN (SELECT id FROM tasks WHERE deleted_at IS NULL);
    DELETE FROM task_asset_links
     WHERE asset_id = NEW.id
       AND task_id NOT IN (SELECT CAST(value AS INTEGER) FROM json_each(NEW.task_ids));
END;

