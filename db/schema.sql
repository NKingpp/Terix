-- ARTEX PostgreSQL schema (单一数据源)
-- 幂等：可重复执行（IF NOT EXISTS / OR REPLACE / DROP TRIGGER IF EXISTS）。

-- =====================================================================
-- 0. 通用：updated_at 触发器
-- =====================================================================
CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN NEW.updated_at = now(); RETURN NEW; END;
$$ LANGUAGE plpgsql;

-- 安全的 text→inet 转换：非法值返回 NULL 而不是抛 22P02。assets.ip 是自由文本
-- (Agent / 资产 API 可能写进主机名)，裸转 a.ip::inet 会让单独一行脏数据把整条
-- 企业归属重算语句打挂。调用方用 try_inet(...) IS NULL 找出这些行并告警。
-- 不用 pg_input_is_valid 是因为那要 PG16+，这里要兼容更老的存量库。
CREATE OR REPLACE FUNCTION try_inet(value text) RETURNS inet AS $$
BEGIN
    RETURN value::inet;
EXCEPTION WHEN others THEN
    RETURN NULL;
END;
$$ LANGUAGE plpgsql IMMUTABLE STRICT;

-- =====================================================================
-- A. 资产层：companies / assets / company_scope
-- =====================================================================

CREATE TABLE IF NOT EXISTS companies (
    id         BIGSERIAL PRIMARY KEY,
    name       TEXT NOT NULL,
    nkey       TEXT NOT NULL UNIQUE,
    logo       TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_companies_nkey ON companies(nkey);
DROP TRIGGER IF EXISTS trg_companies_upd ON companies;
CREATE TRIGGER trg_companies_upd BEFORE UPDATE ON companies
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS assets (
    id              BIGSERIAL PRIMARY KEY,
    type            TEXT NOT NULL CHECK (type IN (
                        'root_domain','ip','subdomain','app','service','endpoint'
                    )),
    company_id      BIGINT REFERENCES companies(id) ON DELETE SET NULL,
    -- explicit: caller/user selected the company; scope: derived from company_scope.
    -- Existing installations are conservatively migrated as explicit so a scope
    -- rebuild can never erase a historical manual association.
    company_source  TEXT NOT NULL DEFAULT 'explicit'
                    CHECK (company_source IN ('explicit','scope')),
    task_ids        BIGINT[] NOT NULL DEFAULT '{}',
    domain          TEXT,
    root_domain     TEXT,
    ip              TEXT,
    c_segment       CIDR,
    port            INTEGER CHECK (port BETWEEN 1 AND 65535),
    icp             TEXT,
    bound_domains   TEXT[]  NOT NULL DEFAULT '{}',
    open_ports      JSONB[] NOT NULL DEFAULT '{}',
    record_type     TEXT,
    record_value    TEXT[],
    bundle_id       TEXT,
    app_name        TEXT,
    category        TEXT,
    app_description TEXT,
    app_icp         TEXT,
    url             TEXT,
    service_type    TEXT CHECK (service_type IN ('http','other')),
    service_name    TEXT,
    favicon_mmh3    TEXT,
    status_code     INTEGER,
    content_length  BIGINT,
    page_title      TEXT,
    technologies    TEXT[]  NOT NULL DEFAULT '{}',
    auth            JSONB[] NOT NULL DEFAULT '{}',
    method          TEXT,
    params          JSONB[] NOT NULL DEFAULT '{}',
    extra           JSONB   NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_av2_root_domain  ON assets(domain) WHERE type = 'root_domain';
CREATE UNIQUE INDEX IF NOT EXISTS uq_av2_ip           ON assets(ip)     WHERE type = 'ip';
CREATE UNIQUE INDEX IF NOT EXISTS uq_av2_subdomain    ON assets(domain, COALESCE(record_type,'')) WHERE type = 'subdomain';
CREATE UNIQUE INDEX IF NOT EXISTS uq_av2_app_bundle   ON assets(bundle_id) WHERE type = 'app' AND bundle_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_av2_app_name     ON assets(app_name)  WHERE type = 'app' AND bundle_id IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_av2_service_http ON assets(url) WHERE type = 'service' AND service_type = 'http';
CREATE UNIQUE INDEX IF NOT EXISTS uq_av2_service_other
    ON assets(COALESCE(domain,''), COALESCE(ip,''), port, service_name) WHERE type = 'service' AND service_type = 'other';
CREATE UNIQUE INDEX IF NOT EXISTS uq_av2_endpoint     ON assets(url, method) WHERE type = 'endpoint';
CREATE INDEX IF NOT EXISTS idx_av2_company      ON assets(company_id)       WHERE company_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_av2_company_type ON assets(company_id, type) WHERE company_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_av2_task_ids     ON assets USING GIN(task_ids);
CREATE INDEX IF NOT EXISTS idx_av2_domain       ON assets(domain)      WHERE domain IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_av2_root_domain  ON assets(root_domain) WHERE root_domain IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_av2_ip           ON assets(ip)          WHERE ip IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_av2_c_segment    ON assets USING GIST(c_segment inet_ops) WHERE c_segment IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_av2_technologies ON assets USING GIN(technologies) WHERE type = 'service';
CREATE INDEX IF NOT EXISTS idx_av2_bound_domains ON assets USING GIN(bound_domains) WHERE type = 'ip';
CREATE INDEX IF NOT EXISTS idx_av2_open_ports   ON assets USING GIN(open_ports)    WHERE type = 'ip';
CREATE INDEX IF NOT EXISTS idx_av2_last_seen    ON assets(last_seen DESC);
CREATE INDEX IF NOT EXISTS idx_av2_type_seen    ON assets(type, last_seen DESC);
ALTER TABLE assets ADD COLUMN IF NOT EXISTS company_source TEXT;
UPDATE assets SET company_source = 'explicit' WHERE company_source IS NULL;
ALTER TABLE assets ALTER COLUMN company_source SET DEFAULT 'explicit';
ALTER TABLE assets ALTER COLUMN company_source SET NOT NULL;
ALTER TABLE assets DROP CONSTRAINT IF EXISTS assets_company_source_check;
ALTER TABLE assets ADD CONSTRAINT assets_company_source_check
    CHECK (company_source IN ('explicit','scope'));
DROP TRIGGER IF EXISTS trg_av2_upd ON assets;
CREATE TRIGGER trg_av2_upd BEFORE UPDATE ON assets
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS company_scope (
    id         BIGSERIAL PRIMARY KEY,
    company_id BIGINT NOT NULL REFERENCES companies(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL CHECK (kind IN ('domain','ip','cidr','icp','keyword')),
    domain     TEXT,
    net        CIDR,
    value      TEXT,
    raw        TEXT NOT NULL,
    reason     TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_sv2_domain UNIQUE (company_id, domain),
    CONSTRAINT uq_sv2_net    UNIQUE (company_id, net),
    CONSTRAINT ck_company_scope_payload CHECK (
        (kind = 'domain' AND domain IS NOT NULL AND net IS NULL AND value IS NULL)
        OR (kind IN ('ip','cidr') AND domain IS NULL AND net IS NOT NULL AND value IS NULL)
        OR (kind IN ('icp','keyword') AND domain IS NULL AND net IS NULL AND value IS NOT NULL)
    )
);
-- Existing installations need the new text payload and expanded kind check.
ALTER TABLE company_scope ADD COLUMN IF NOT EXISTS value TEXT;
ALTER TABLE company_scope DROP CONSTRAINT IF EXISTS company_scope_kind_check;
ALTER TABLE company_scope ADD CONSTRAINT company_scope_kind_check
    CHECK (kind IN ('domain','ip','cidr','icp','keyword'));
ALTER TABLE company_scope DROP CONSTRAINT IF EXISTS ck_company_scope_payload;
ALTER TABLE company_scope ADD CONSTRAINT ck_company_scope_payload CHECK (
    (kind = 'domain' AND domain IS NOT NULL AND net IS NULL AND value IS NULL)
    OR (kind IN ('ip','cidr') AND domain IS NULL AND net IS NOT NULL AND value IS NULL)
    OR (kind IN ('icp','keyword') AND domain IS NULL AND net IS NULL AND value IS NOT NULL)
);
CREATE INDEX IF NOT EXISTS idx_sv2_domain  ON company_scope(domain)   WHERE kind = 'domain';
CREATE INDEX IF NOT EXISTS idx_sv2_net     ON company_scope USING GIST(net inet_ops) WHERE kind IN ('ip','cidr');
CREATE UNIQUE INDEX IF NOT EXISTS uq_sv2_value ON company_scope(company_id, kind, value) WHERE kind IN ('icp','keyword');
CREATE INDEX IF NOT EXISTS idx_sv2_icp ON company_scope(value) WHERE kind = 'icp';
CREATE INDEX IF NOT EXISTS idx_sv2_company ON company_scope(company_id);

-- =====================================================================
-- B. 推理探索层
-- =====================================================================
CREATE TABLE IF NOT EXISTS explorations (
    id          BIGSERIAL PRIMARY KEY,
    description TEXT,
    goal        TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'open'
                  CHECK (status IN ('open','achieved','failed')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- cold-digest (§2.3): per-task planner round counter — bumped once each time the
-- planner wakes and processes a round. Drives the ≥R cold-node debounce (measured in
-- this exploration's own rounds, not global node ids or wall-clock).
ALTER TABLE explorations ADD COLUMN IF NOT EXISTS round_no BIGINT NOT NULL DEFAULT 0;
DROP TRIGGER IF EXISTS trg_exp_upd ON explorations;
CREATE TRIGGER trg_exp_upd BEFORE UPDATE ON explorations
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS exploration_nodes (
    id             BIGSERIAL PRIMARY KEY,
    exploration_id BIGINT NOT NULL REFERENCES explorations(id) ON DELETE CASCADE,
    kind           TEXT NOT NULL,
    payload        JSONB NOT NULL DEFAULT '{}',
    priority       INT  NOT NULL DEFAULT 0,
    state          TEXT NOT NULL DEFAULT 'open',
    origin         TEXT,
    owner          TEXT,
    blocked_reason TEXT,
    delete_reason  TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at   TIMESTAMPTZ,
    -- join / thesis / signal / conclusion 是金融改造加的 kind（WE-2/WE-8）。
    -- 少了它们，AddNode("join", ...) 会撞 ck_node_kind 而失败。
    CONSTRAINT ck_node_kind CHECK (kind IN ('begin','goal','intent','fact','finding','hint','digest','join','thesis','signal','conclusion')),
    CONSTRAINT ck_node_state CHECK (
        (kind='begin'   AND state IN ('open')) OR
        (kind='intent'  AND state IN ('open','running','paused','done','blocked','exhausted','stopped','deleted')) OR
        (kind='goal'    AND state IN ('open','met','abandoned')) OR
        (kind='fact'    AND state IN ('confirmed','dismissed','origin')) OR
        (kind='finding' AND state IN ('confirmed','dismissed')) OR
        (kind='hint'    AND state IN ('active','consumed')) OR
        (kind='digest'  AND state IN ('active','superseded')) OR
        -- 以下四种 kind 由金融改造加入（WE-2/WE-8）。缺分支会让 AddNode 撞约束。
        (kind='join'       AND state IN ('open','done','blocked')) OR
        (kind='thesis'     AND state IN ('open','standing','refuted','superseded')) OR
        (kind='signal'     AND state IN ('open','acted','expired','voided')) OR
        (kind='conclusion' AND state IN ('final','superseded'))
    )
);
ALTER TABLE exploration_nodes ADD COLUMN IF NOT EXISTS blocked_reason TEXT;
-- 意图假删除(soft delete):state='deleted' 时,delete_reason 记用户填写的删除原因。
ALTER TABLE exploration_nodes ADD COLUMN IF NOT EXISTS delete_reason TEXT;
-- cold-digest (§2.3/§5.3): content_version bumps on any change that could alter a
-- digest body (summary/state/confidence); cold_since_round stamps the planner round
-- a node most recently went from "has a live downstream branch" to none (NULL = hot).
ALTER TABLE exploration_nodes ADD COLUMN IF NOT EXISTS content_version  INT    NOT NULL DEFAULT 0;
ALTER TABLE exploration_nodes ADD COLUMN IF NOT EXISTS cold_since_round BIGINT;
-- ck_node_kind: 老的库可能停在任意一个历史版本（缺 digest / 缺 join 等）。
--
-- 判据用 '%join%'：join 是当前最新加进来的 kind，
-- 如果约束里有它，说明已经是全集；没有就重建。
-- 这比「逐版本判断」简单，且不会漏。
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid='exploration_nodes'::regclass
          AND conname='ck_node_kind'
          AND pg_get_constraintdef(oid) NOT LIKE '%join%'
    ) THEN
        ALTER TABLE exploration_nodes DROP CONSTRAINT ck_node_kind;
        ALTER TABLE exploration_nodes ADD CONSTRAINT ck_node_kind
            CHECK (kind IN ('begin','goal','intent','fact','finding','hint','digest','join','thesis','signal','conclusion'));
    END IF;
END $$;
-- ck_node_state: recreate when it lacks the 'paused' (older), 'superseded' (digest rev),
-- or 'deleted' (intent soft-delete rev) branches.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid='exploration_nodes'::regclass
          AND conname='ck_node_state'
          AND (pg_get_constraintdef(oid) NOT LIKE '%paused%'
               OR pg_get_constraintdef(oid) NOT LIKE '%superseded%'
               OR pg_get_constraintdef(oid) NOT LIKE '%deleted%'
               -- 缺 join 分支说明还停在不同版本的 kind 集合上
               OR pg_get_constraintdef(oid) NOT LIKE '%join%')
    ) THEN
        ALTER TABLE exploration_nodes DROP CONSTRAINT ck_node_state;
        ALTER TABLE exploration_nodes ADD CONSTRAINT ck_node_state CHECK (
            (kind='begin'   AND state IN ('open')) OR
            (kind='intent'  AND state IN ('open','running','paused','done','blocked','exhausted','stopped','deleted')) OR
            (kind='goal'    AND state IN ('open','met','abandoned')) OR
            (kind='fact'    AND state IN ('confirmed','dismissed','origin')) OR
            (kind='finding' AND state IN ('confirmed','dismissed')) OR
            (kind='hint'    AND state IN ('active','consumed')) OR
            (kind='digest'  AND state IN ('active','superseded')) OR
            (kind='join'       AND state IN ('open','done','blocked')) OR
            (kind='thesis'     AND state IN ('open','standing','refuted','superseded')) OR
            (kind='signal'     AND state IN ('open','acted','expired','voided')) OR
            (kind='conclusion' AND state IN ('final','superseded'))
        );
    END IF;
END $$;
CREATE INDEX IF NOT EXISTS idx_expnodes_part     ON exploration_nodes(exploration_id, kind);
CREATE INDEX IF NOT EXISTS idx_expnodes_frontier ON exploration_nodes(exploration_id, priority DESC)
    WHERE kind='intent' AND state='open';
DROP TRIGGER IF EXISTS trg_expnodes_upd ON exploration_nodes;
CREATE TRIGGER trg_expnodes_upd BEFORE UPDATE ON exploration_nodes
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- trg_expnodes_owner：node 的 owner 列从 payload.role 派生。
--
-- # 为什么用触发器而不是在每个 INSERT 里写
--
-- 全仓有 5 处 INSERT INTO exploration_nodes，只有 AddNode 一处写了 owner：
--
--   db/exploration.go:252      AddNode              → 有 owner
--   agent/materialize.go:127   MaterializeGraph     → 无
--   db/digest.go:131           digest 节点          → 无
--   db/finding_traffic.go:366  fact 节点            → 无
--   db/findings.go:429         fact 节点            → 无
--   db/tasks.go:216            origin fact          → 无
--
-- 而 owner 是 sibling 读取（LatestSiblings 按 owner 过滤）与
-- 「按 role 判定」的唯一依据 —— 任何一处漏写，那批节点的「同角色历史」
-- 就永远读不到，且**不报错**（静默失效，见 L16 的失败方式）。
--
-- # 为什么不逐处补
--
-- 逐处补的正确性依赖「将来第 6 处新写入路径要记得写 owner」——而「记得」不可靠。
-- 而 DB 是唯一无法绕过的汇聚点：所有写入最终都变成 INSERT。
--
-- 而且这 5 处里 4 处建的是 fact（无 role），改了是空操作 ——
-- 那更凸显「逐处改」的荒谬：改了一堆不需要改的，而真正会漏的是下一处。
--
-- # 为什么不覆盖已有值
--
-- 只在 owner IS NULL 时填 —— 显式传了 owner 的调用方不被推翻
-- （AddNode 仍然显式写，见下条注释）。
--
-- # 为什么 NULLIF 空串
--
-- 「没有 owner」在库里只能有一种表示（NULL），不能是 '' 和 NULL 两种 ——
-- 否则 WHERE owner IS NULL 会漏掉 '' 的行。
--
-- # 为什么含 UPDATE OF payload
--
-- 让「先建节点、后补 role」的写入方式也能生效。
-- 只写 BEFORE INSERT 的话，那种路径会漏。
CREATE OR REPLACE FUNCTION derive_node_owner() RETURNS trigger AS $$
BEGIN
    IF NEW.owner IS NULL THEN
        NEW.owner := NULLIF(TRIM(NEW.payload->>'role'), '');
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_expnodes_owner ON exploration_nodes;
CREATE TRIGGER trg_expnodes_owner BEFORE INSERT OR UPDATE OF payload ON exploration_nodes
    FOR EACH ROW EXECUTE FUNCTION derive_node_owner();

CREATE TABLE IF NOT EXISTS exploration_edges (
    exploration_id BIGINT NOT NULL REFERENCES explorations(id) ON DELETE CASCADE,
    src_id         BIGINT NOT NULL REFERENCES exploration_nodes(id) ON DELETE CASCADE,
    dst_id         BIGINT NOT NULL REFERENCES exploration_nodes(id) ON DELETE CASCADE,
    rel            TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (exploration_id, src_id, rel, dst_id),
    CONSTRAINT ck_edge_noself CHECK (src_id <> dst_id),
    -- gates 是 WE-2/WE-8 的 join 门控边（src --gates--> dst）。缺它 AddGates 会撞约束。
    CONSTRAINT ck_edge_rel CHECK (rel IN ('spawns','derived_from','yields','proves','covers','gates'))
);
CREATE INDEX IF NOT EXISTS idx_expedges_src ON exploration_edges(src_id, rel);
CREATE INDEX IF NOT EXISTS idx_expedges_dst ON exploration_edges(dst_id, rel);
-- cold-digest (§1): the 'covers' relation (digest→member) postdates shipped installs,
-- whose rel CHECK is an inline auto-named constraint. Find and recreate it as ck_edge_rel.
DO $$
DECLARE cname text;
BEGIN
    -- 判据用 '%gates%'：gates 是当前最后加入的 rel，
    -- 有它就说明约束已是全集（比逐版本判断简单且不漏）。
    SELECT conname INTO cname FROM pg_constraint
     WHERE conrelid='exploration_edges'::regclass AND contype='c'
       AND pg_get_constraintdef(oid) LIKE '%rel%'
       AND pg_get_constraintdef(oid) NOT LIKE '%gates%';
    IF cname IS NOT NULL THEN
        EXECUTE 'ALTER TABLE exploration_edges DROP CONSTRAINT '||quote_ident(cname);
        ALTER TABLE exploration_edges ADD CONSTRAINT ck_edge_rel
            CHECK (rel IN ('spawns','derived_from','yields','proves','covers','gates'));
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS exploration_anchors (
    node_id   BIGINT NOT NULL REFERENCES exploration_nodes(id) ON DELETE CASCADE,
    asset_id  BIGINT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    PRIMARY KEY (node_id, asset_id)
);
CREATE INDEX IF NOT EXISTS idx_anchor_asset ON exploration_anchors(asset_id);

-- task_constraints: operator-authored operation constraints (allow/deny) for a task.
-- Extracted by the goals decomposer at round 0 (from goal/description), editable at
-- runtime by the main agent + 总览「约束管理」. Injected into the planner/worker system
-- prompt each round (config-gated) to keep exploration within the operator's boundary.
CREATE TABLE IF NOT EXISTS task_constraints (
    id             BIGSERIAL PRIMARY KEY,
    exploration_id BIGINT NOT NULL REFERENCES explorations(id) ON DELETE CASCADE,
    kind           TEXT NOT NULL CHECK (kind IN ('allow','deny')),
    text           TEXT NOT NULL,
    origin         TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_task_constraints_exp ON task_constraints(exploration_id);

CREATE TABLE IF NOT EXISTS activity (
    id                 BIGSERIAL PRIMARY KEY,
    exploration_id     BIGINT NOT NULL REFERENCES explorations(id) ON DELETE CASCADE,
    node_id            BIGINT REFERENCES exploration_nodes(id) ON DELETE SET NULL,
    worker             TEXT,
    kind               TEXT,
    tool               TEXT,
    tool_use_id        TEXT,
    is_error           BOOLEAN NOT NULL DEFAULT false,
    summary            TEXT,
    detail             TEXT,
    metadata           JSONB NOT NULL DEFAULT '{}',
    input_tokens       INTEGER,
    output_tokens      INTEGER,
    cache_read_tokens  INTEGER,
    cache_write_tokens INTEGER,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE activity ADD COLUMN IF NOT EXISTS metadata JSONB NOT NULL DEFAULT '{}';
-- main_seg segments the main-agent session into resettable conversations: a new
-- main session bumps the segment so its transcript + activity start clean while the
-- task's graph/assets/goal are untouched. NULL == legacy rows == segment 0 (the
-- original session). Only worker='mainagent' rows carry it.
ALTER TABLE activity ADD COLUMN IF NOT EXISTS main_seg INTEGER;
CREATE INDEX IF NOT EXISTS idx_act_node  ON activity(exploration_id, node_id, id);
CREATE INDEX IF NOT EXISTS idx_act_since ON activity(exploration_id, id);
CREATE INDEX IF NOT EXISTS idx_act_tool_call ON activity(exploration_id, tool_use_id, id)
  WHERE kind IN ('tool_use', 'tool_result');
-- Main/Plan history pages filter by worker (both carry NULL node_id, so idx_act_node
-- can't distinguish them); this covers reverse pagination of those sessions.
CREATE INDEX IF NOT EXISTS idx_act_worker ON activity(exploration_id, worker, id);
-- Main-session pages filter by segment on top of worker='mainagent'; this partial
-- index covers reverse pagination within one segment.
CREATE INDEX IF NOT EXISTS idx_act_main_seg ON activity(exploration_id, main_seg, id)
    WHERE worker='mainagent';
-- Task-list polls aggregate result usage and find the latest event repeatedly.
-- Cover the token columns for index-only aggregation and the timestamp order for
-- per-exploration latest-activity lookups.
CREATE INDEX IF NOT EXISTS idx_act_result_usage ON activity(exploration_id)
    INCLUDE (input_tokens, output_tokens, cache_read_tokens, cache_write_tokens)
    WHERE kind='result';
CREATE INDEX IF NOT EXISTS idx_act_latest ON activity(exploration_id, created_at DESC);

-- main_sessions records the resettable main-agent conversation segments of a task.
-- Segment 0 (the original session) is implicit and never stored; this table holds
-- only the extra segments created by "新建会话" (seq >= 1). The current segment is
-- MAX(seq) or 0. Each segment gets its own transcript file + activity slice; the
-- task's exploration graph/assets/goal are shared and never reset.
CREATE TABLE IF NOT EXISTS main_sessions (
    exploration_id BIGINT NOT NULL REFERENCES explorations(id) ON DELETE CASCADE,
    seq            INTEGER NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (exploration_id, seq)
);

-- =====================================================================
-- C. LLM profiles
-- =====================================================================
CREATE TABLE IF NOT EXISTS settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS llm_profiles (
    id               BIGSERIAL PRIMARY KEY,
    name             TEXT NOT NULL UNIQUE,
    format           TEXT NOT NULL CHECK (format IN ('openai','anthropic','openai-responses')),
    base_url         TEXT,
    proxy            TEXT,
    model            TEXT NOT NULL,
    api_key          TEXT,
    api_key_hint     TEXT,
    rate_per_second  DOUBLE PRECISION NOT NULL DEFAULT 0,
    rate_per_minute  DOUBLE PRECISION NOT NULL DEFAULT 0,
    context_window_k INTEGER NOT NULL DEFAULT 0,
    -- 思考参数拆成两个独立字段：thinking_type=思考开关(''/disabled/enabled)，
    -- reasoning_effort=思考强度(''/low/medium/high/xhigh/max)，互不牵连。
    reasoning_effort TEXT NOT NULL DEFAULT '',
    thinking_type    TEXT NOT NULL DEFAULT '',
    is_default       BOOLEAN NOT NULL DEFAULT false,
    -- 轮询(故障转移)参数，见 docs/LLM轮询设计.md：
    --   priority     顺位，越大越先被选中；激活配置(is_default)永远排链首，与本值无关。
    --   pool_exclude true=不作为故障转移目标(仍可被 agent/任务显式绑定使用)。
    priority         INTEGER NOT NULL DEFAULT 0,
    pool_exclude     BOOLEAN NOT NULL DEFAULT false,
    -- streaming=true(默认)走流式 SSE；false 走真·非流式(stream:false，一次性 JSON)。
    streaming        BOOLEAN NOT NULL DEFAULT true,
    -- 单次回复的输出上限(token)。0=不发送该字段，由服务端默认值决定——保持既有行为。
    -- 与 context_window_k(模型总容量，仅本地用于压缩阈值)是两回事：本值会随请求发出。
    max_tokens       INTEGER NOT NULL DEFAULT 0,
    -- 输出上限用哪个请求字段名，仅对 format='openai' 生效：
    --   ''                      = max_tokens(默认，兼容绝大多数网关)
    --   'max_completion_tokens' = 新字段；OpenAI 推理模型(o 系列/GPT-5)只认它，
    --                             发 max_tokens 会被 unsupported_parameter 拒绝。
    -- anthropic(max_tokens 必填)与 openai-responses(max_output_tokens)自带字段名，不受此值影响。
    max_tokens_field TEXT NOT NULL DEFAULT '',
    -- 自定义会话头：非空时每次请求带一个该名字的 HTTP 头，头值=当前运行的 session id
    -- (chat 会话/worker 意图)。用于某些按 session-id 头做提示缓存/粘性路由的网关。''=不发送。
    session_header_key TEXT NOT NULL DEFAULT '',
    -- 重试覆盖：次数 0=用全局默认/-1=关闭/>0=该值；间隔 0=用默认指数退避/>0=固定毫秒。
    -- 三组分别对应建连重试、空响应重试、同 provider 安全窗口重试，详见下方 ALTER 处注释。
    retry_connect_attempts    INTEGER NOT NULL DEFAULT 0,
    retry_connect_interval_ms INTEGER NOT NULL DEFAULT 0,
    retry_empty_attempts      INTEGER NOT NULL DEFAULT 0,
    retry_empty_interval_ms   INTEGER NOT NULL DEFAULT 0,
    retry_stream_attempts     INTEGER NOT NULL DEFAULT 0,
    retry_stream_interval_ms  INTEGER NOT NULL DEFAULT 0,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_llm_one_default ON llm_profiles(is_default) WHERE is_default;
DROP TRIGGER IF EXISTS trg_llm_upd ON llm_profiles;
CREATE TRIGGER trg_llm_upd BEFORE UPDATE ON llm_profiles
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
-- 轮询顺位/排除标记；补旧库。默认 0 / false = 全部配置都参与轮询。
ALTER TABLE llm_profiles ADD COLUMN IF NOT EXISTS priority     INTEGER NOT NULL DEFAULT 0;
ALTER TABLE llm_profiles ADD COLUMN IF NOT EXISTS pool_exclude BOOLEAN NOT NULL DEFAULT false;
-- 流式开关；补旧库。默认 true = 保持既有的流式行为，旧配置无感升级。
ALTER TABLE llm_profiles ADD COLUMN IF NOT EXISTS streaming    BOOLEAN NOT NULL DEFAULT true;
-- 放开 format 约束以容纳 openai-responses(OpenAI Responses API)；补旧库。
-- 每次启动执行,幂等:先删旧 CHECK 再建含三值的新 CHECK。
ALTER TABLE llm_profiles DROP CONSTRAINT IF EXISTS llm_profiles_format_check;
ALTER TABLE llm_profiles ADD  CONSTRAINT llm_profiles_format_check
    CHECK (format IN ('openai','anthropic','openai-responses'));

-- 输出上限及其字段名；补旧库。默认 0 / '' = 不发送上限、沿用 max_tokens 字段名，
-- 旧配置行为完全不变。
ALTER TABLE llm_profiles ADD COLUMN IF NOT EXISTS max_tokens       INTEGER NOT NULL DEFAULT 0;
ALTER TABLE llm_profiles ADD COLUMN IF NOT EXISTS max_tokens_field TEXT    NOT NULL DEFAULT '';
-- 同 format：先删再建，保证每次启动幂等。
ALTER TABLE llm_profiles DROP CONSTRAINT IF EXISTS llm_profiles_max_tokens_field_check;
ALTER TABLE llm_profiles ADD  CONSTRAINT llm_profiles_max_tokens_field_check
    CHECK (max_tokens_field IN ('','max_completion_tokens'));
ALTER TABLE llm_profiles DROP CONSTRAINT IF EXISTS llm_profiles_max_tokens_check;
ALTER TABLE llm_profiles ADD  CONSTRAINT llm_profiles_max_tokens_check
    CHECK (max_tokens >= 0);
-- 自定义会话头名；补旧库。默认 '' = 不发送，旧配置行为不变。
ALTER TABLE llm_profiles ADD COLUMN IF NOT EXISTS session_header_key TEXT NOT NULL DEFAULT '';

-- 单配置的重试覆盖（见 docs/LLM重试设计.md）。三组各自一对「次数 + 固定间隔」，
-- 语义统一：次数 0=沿用全局默认、-1=关闭该层重试、>0=用该值；间隔 0=沿用该层的
-- 默认指数退避、>0=改用这个固定毫秒数。全部默认 0，所以旧库/旧配置行为不变。
--   connect = 建连重试（SDK doStream：连接重置/超时/429/5xx，流开始前）
--   empty   = 空响应重试（SDK：完成但没有任何 content block，仅 openai 格式）
--   stream  = 同 provider 安全窗口重试（本项目 task_llm：未交付输出前的断流重放）
ALTER TABLE llm_profiles ADD COLUMN IF NOT EXISTS retry_connect_attempts    INTEGER NOT NULL DEFAULT 0;
ALTER TABLE llm_profiles ADD COLUMN IF NOT EXISTS retry_connect_interval_ms INTEGER NOT NULL DEFAULT 0;
ALTER TABLE llm_profiles ADD COLUMN IF NOT EXISTS retry_empty_attempts      INTEGER NOT NULL DEFAULT 0;
ALTER TABLE llm_profiles ADD COLUMN IF NOT EXISTS retry_empty_interval_ms   INTEGER NOT NULL DEFAULT 0;
ALTER TABLE llm_profiles ADD COLUMN IF NOT EXISTS retry_stream_attempts     INTEGER NOT NULL DEFAULT 0;
ALTER TABLE llm_profiles ADD COLUMN IF NOT EXISTS retry_stream_interval_ms  INTEGER NOT NULL DEFAULT 0;
-- 同 format：先删再建，保证每次启动幂等。次数下限 -1(关闭)，间隔不能为负。
ALTER TABLE llm_profiles DROP CONSTRAINT IF EXISTS llm_profiles_retry_check;
ALTER TABLE llm_profiles ADD  CONSTRAINT llm_profiles_retry_check CHECK (
    retry_connect_attempts >= -1 AND retry_empty_attempts >= -1 AND retry_stream_attempts >= -1
    AND retry_connect_interval_ms >= 0 AND retry_empty_interval_ms >= 0 AND retry_stream_interval_ms >= 0);

-- 思考开关字段 thinking_type，从旧的单一 reasoning_effort 语义一次性拆分而来。
-- schema.sql 每次启动都执行，故迁移必须只跑一次：仅当该列尚不存在时才回填，
-- 否则每次启动都会把用户后来手动设的组合覆盖回去。旧 reasoning_effort 语义：
--   'off'                    → 显式关闭  → thinking_type='disabled'，强度清空
--   'low/medium/high/max'    → 开启+强度 → thinking_type='enabled'，强度保留
--   ''                       → 不发送    → 两者皆空(默认)
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'llm_profiles' AND column_name = 'thinking_type'
    ) THEN
        ALTER TABLE llm_profiles ADD COLUMN thinking_type TEXT NOT NULL DEFAULT '';
        UPDATE llm_profiles SET thinking_type = 'enabled'
            WHERE reasoning_effort IN ('low','medium','high','max');
        UPDATE llm_profiles SET thinking_type = 'disabled', reasoning_effort = ''
            WHERE reasoning_effort = 'off';
    END IF;
END $$;

-- LLM 轮询熔断状态：某个配置连续失败(余额不足/key 失效/限流)后进入冷却，冷却期内
-- 轮询直接跳过它。内存态为准，这里落库只为重启后不丢冷却窗口——加载时只取尚未
-- 到期的行(open_until > now)，已到期的自然回到"正常"，等下一次调用半开试探。
CREATE TABLE IF NOT EXISTS llm_profile_health (
    profile_id  BIGINT PRIMARY KEY REFERENCES llm_profiles(id) ON DELETE CASCADE,
    fails       INTEGER NOT NULL DEFAULT 0,  -- 当前连续失败次数(成功即清零)
    trips       INTEGER NOT NULL DEFAULT 0,  -- 累计熔断次数,用于冷却时间指数退避
    open_until  TIMESTAMPTZ,                 -- 冷却截止;NULL/过期 = 未熔断
    last_error  TEXT NOT NULL DEFAULT '',
    last_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- =====================================================================
-- D. 任务层
-- =====================================================================
-- Global task categories are intentionally independent from task templates.
-- Deleting a category only moves its tasks back to the uncategorized bucket.
CREATE TABLE IF NOT EXISTS task_categories (
    id         BIGSERIAL PRIMARY KEY,
    name       TEXT NOT NULL,
    nkey       TEXT NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_task_categories_name ON task_categories(name, id);
DROP TRIGGER IF EXISTS trg_task_categories_upd ON task_categories;
CREATE TRIGGER trg_task_categories_upd BEFORE UPDATE ON task_categories
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS tasks (
    id             BIGSERIAL PRIMARY KEY,
    name           TEXT NOT NULL DEFAULT '',
    category_id    BIGINT REFERENCES task_categories(id) ON DELETE SET NULL,
    description    TEXT NOT NULL,
    goal           TEXT NOT NULL,
    exploration_id BIGINT NOT NULL UNIQUE
                     REFERENCES explorations(id) ON DELETE RESTRICT,
    status         TEXT NOT NULL DEFAULT 'created'
                     CHECK (status IN ('created','running','paused','done','failed','timeout')),
    paused         BOOLEAN NOT NULL DEFAULT false,
    queued         BOOLEAN NOT NULL DEFAULT false,
    queued_at      TIMESTAMPTZ,
    queue_mode     TEXT NOT NULL DEFAULT '',
    llm_profile_id BIGINT REFERENCES llm_profiles(id) ON DELETE SET NULL,
    active_llm_profile_id BIGINT REFERENCES llm_profiles(id) ON DELETE SET NULL,
    llm_chain_revision BIGINT NOT NULL DEFAULT 0,
    company_id     BIGINT REFERENCES companies(id) ON DELETE SET NULL,
    parent_ref     TEXT,
    timeout_seconds INTEGER NOT NULL DEFAULT 0,
    plan_heartbeat_seconds INTEGER NOT NULL DEFAULT 300,
    coverage_enabled BOOLEAN NOT NULL DEFAULT true,
    pinned_at      TIMESTAMPTZ,
    first_run_at   TIMESTAMPTZ,
    deadline_at    TIMESTAMPTZ,
    archived_at    TIMESTAMPTZ,
    deleted_at     TIMESTAMPTZ,
    completed_at   TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_tasks_alive  ON tasks(created_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(status)          WHERE deleted_at IS NULL;
DROP TRIGGER IF EXISTS trg_tasks_upd ON tasks;
CREATE TRIGGER trg_tasks_upd BEFORE UPDATE ON tasks
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
-- planner 心跳触发间隔(秒);补旧库。默认 300s(5min)。见 docs/planner-trigger-impl-plan.md
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS plan_heartbeat_seconds INTEGER NOT NULL DEFAULT 300;
-- 并发上限挂起态;补旧库。true=因并发上限排队、等待空位自动启动。
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS queued BOOLEAN NOT NULL DEFAULT false;
-- 资产覆盖度功能开关;补旧库。true(默认)=计算/展示测试覆盖度、自动累积测试范围、
-- 给 agent 开放 add_task_scope/list_untested_assets;false=全部关闭(见 task_scope.go)。
-- 存量任务默认 true 保持原行为;company 关联(task_scope kind=company)不受此开关影响。
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS coverage_enabled BOOLEAN NOT NULL DEFAULT true;
-- queued_at makes admission FIFO reflect the actual enqueue order rather than the
-- task creation order. queue_mode distinguishes first bootstrap from resuming an
-- exploration that already owns goals/history.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS queued_at TIMESTAMPTZ;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS queue_mode TEXT NOT NULL DEFAULT '';
-- 可选的任务名称;补旧库。空串=未命名,前端展示时回退到描述。
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS name TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS category_id BIGINT REFERENCES task_categories(id) ON DELETE SET NULL;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS pinned_at TIMESTAMPTZ;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS active_llm_profile_id BIGINT REFERENCES llm_profiles(id) ON DELETE SET NULL;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS llm_chain_revision BIGINT NOT NULL DEFAULT 0;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS archived_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_tasks_category ON tasks(category_id, created_at DESC)
    WHERE deleted_at IS NULL AND category_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_tasks_pinned ON tasks(pinned_at DESC)
    WHERE deleted_at IS NULL AND pinned_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_tasks_archived ON tasks(archived_at DESC)
    WHERE archived_at IS NOT NULL;

-- Cold task archives retain only compact metadata in PostgreSQL. The complete
-- task payload lives in a versioned .tar.zst package under data/archives/tasks.
-- task_id stays unique so an operation can be retried safely after a restart.
CREATE TABLE IF NOT EXISTS task_archives (
    id                         BIGSERIAL PRIMARY KEY,
    task_id                    BIGINT NOT NULL UNIQUE REFERENCES tasks(id) ON DELETE CASCADE,
    state                      TEXT NOT NULL DEFAULT 'archive_queued' CHECK (state IN (
                                   'archive_queued','archiving','archive_failed','ready',
                                   'restore_queued','restoring','restore_failed',
                                   'delete_queued','deleting','delete_failed'
                               )),
    phase                      TEXT NOT NULL DEFAULT 'queued',
    progress                   INTEGER NOT NULL DEFAULT 0 CHECK (progress BETWEEN 0 AND 100),
    error                      TEXT NOT NULL DEFAULT '',
    warnings                   JSONB NOT NULL DEFAULT '[]',
    format_version             INTEGER NOT NULL DEFAULT 2,
    archive_path               TEXT NOT NULL DEFAULT '',
    sha256                     TEXT NOT NULL DEFAULT '',
    original_size              BIGINT NOT NULL DEFAULT 0,
    compressed_size            BIGINT NOT NULL DEFAULT 0,
    task_name                  TEXT NOT NULL DEFAULT '',
    task_description           TEXT NOT NULL DEFAULT '',
    task_goal                  TEXT NOT NULL DEFAULT '',
    original_status            TEXT NOT NULL DEFAULT '',
    category_id_snapshot       BIGINT,
    category_name_snapshot     TEXT NOT NULL DEFAULT '',
    source_task_ids            BIGINT[] NOT NULL DEFAULT '{}',
    remaining_timeout_seconds  BIGINT NOT NULL DEFAULT 0,
    data_counts                JSONB NOT NULL DEFAULT '{}',
    aggregate_stats            JSONB NOT NULL DEFAULT '{}',
    archived_at                TIMESTAMPTZ,
    requested_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                 TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE task_archives ALTER COLUMN format_version SET DEFAULT 2;
CREATE INDEX IF NOT EXISTS idx_task_archives_state ON task_archives(state, requested_at, id);
CREATE INDEX IF NOT EXISTS idx_task_archives_archived ON task_archives(archived_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_task_archives_sources ON task_archives USING GIN(source_task_ids);
DROP TRIGGER IF EXISTS trg_task_archives_upd ON task_archives;
CREATE TRIGGER trg_task_archives_upd BEFORE UPDATE ON task_archives
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Reusable task description/goal presets. nkey is the normalized, case-insensitive
-- identity used to reject visually equivalent duplicate names.
CREATE TABLE IF NOT EXISTS task_templates (
    id          BIGSERIAL PRIMARY KEY,
    name        TEXT NOT NULL,
    nkey        TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL,
    goal        TEXT NOT NULL,
    -- 预设的任务分类；分类删除时置空（与 tasks.category_id 一致，不阻断）。
    category_id     BIGINT REFERENCES task_categories(id) ON DELETE SET NULL,
    -- 预设的任务级拦截/允许规则快照(AssetInterceptRuleInput 数组)；应用模板时灌进新任务。
    intercept_rules JSONB NOT NULL DEFAULT '[]',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- 补旧库(已发版,加列带 IF NOT EXISTS)。
ALTER TABLE task_templates ADD COLUMN IF NOT EXISTS category_id BIGINT REFERENCES task_categories(id) ON DELETE SET NULL;
ALTER TABLE task_templates ADD COLUMN IF NOT EXISTS intercept_rules JSONB NOT NULL DEFAULT '[]';
CREATE INDEX IF NOT EXISTS idx_task_templates_updated ON task_templates(updated_at DESC, id DESC);
DROP TRIGGER IF EXISTS trg_task_templates_upd ON task_templates;
CREATE TRIGGER trg_task_templates_upd BEFORE UPDATE ON task_templates
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Direct, read-only task context inheritance. Relations are intentionally not
-- recursive: a task sees only the source tasks explicitly chosen at creation.
CREATE TABLE IF NOT EXISTS task_relations (
    task_id        BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    source_task_id BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (task_id, source_task_id),
    CONSTRAINT ck_task_relation_not_self CHECK (task_id <> source_task_id)
);
CREATE INDEX IF NOT EXISTS idx_task_relations_source ON task_relations(source_task_id);

-- Task/asset provenance supplements the legacy assets.task_ids association. The
-- array remains the compatibility source for existing query and cleanup paths;
-- this relation records how each association was obtained for operator review.
CREATE TABLE IF NOT EXISTS task_asset_links (
    task_id        BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    asset_id       BIGINT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    source         TEXT NOT NULL DEFAULT 'system',
    source_summary TEXT NOT NULL DEFAULT '',
    source_node_id BIGINT REFERENCES exploration_nodes(id) ON DELETE SET NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (task_id, asset_id)
);
CREATE INDEX IF NOT EXISTS idx_task_asset_links_asset ON task_asset_links(asset_id, task_id);
CREATE INDEX IF NOT EXISTS idx_task_asset_links_node ON task_asset_links(source_node_id)
    WHERE source_node_id IS NOT NULL;
DROP TRIGGER IF EXISTS trg_task_asset_links_upd ON task_asset_links;
CREATE TRIGGER trg_task_asset_links_upd BEFORE UPDATE ON task_asset_links
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Keep provenance rows synchronized when existing asset upsert paths append or
-- remove task ids. Detailed callers overwrite the generic source after upsert.
CREATE OR REPLACE FUNCTION sync_task_asset_links() RETURNS trigger AS $$
BEGIN
    INSERT INTO task_asset_links(task_id, asset_id, source, source_summary)
    SELECT task.id, NEW.id, 'system', '任务执行期间自动关联'
    FROM unnest(NEW.task_ids) AS requested(task_id)
    JOIN tasks task ON task.id=requested.task_id AND task.deleted_at IS NULL
    ON CONFLICT (task_id, asset_id) DO NOTHING;

    DELETE FROM task_asset_links link
    WHERE link.asset_id=NEW.id
      AND NOT (link.task_id=ANY(NEW.task_ids));
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_assets_task_links ON assets;
CREATE TRIGGER trg_assets_task_links AFTER INSERT OR UPDATE OF task_ids ON assets
    FOR EACH ROW EXECUTE FUNCTION sync_task_asset_links();

-- Existing installations receive an auditable legacy source without rewriting
-- task_ids. Ignore stale array ids that no longer resolve to a live task.
INSERT INTO task_asset_links(task_id, asset_id, source, source_summary)
SELECT task.id, asset.id, 'legacy', '由历史任务资产关联迁移'
FROM assets asset
CROSS JOIN LATERAL unnest(asset.task_ids) AS requested(task_id)
JOIN tasks task ON task.id=requested.task_id AND task.deleted_at IS NULL
ON CONFLICT (task_id, asset_id) DO NOTHING;

-- Ordered task-level LLM failover chain. A quota-exhausted entry is skipped
-- until the user saves/resets the chain, which clears all failure state.
CREATE TABLE IF NOT EXISTS task_llm_profiles (
    task_id          BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    profile_id       BIGINT NOT NULL REFERENCES llm_profiles(id) ON DELETE CASCADE,
    position         INTEGER NOT NULL CHECK (position >= 0),
    status           TEXT NOT NULL DEFAULT 'ready'
                       CHECK (status IN ('ready','quota_exhausted')),
    last_error       TEXT,
    exhausted_at     TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (task_id, profile_id),
    UNIQUE (task_id, position)
);
CREATE INDEX IF NOT EXISTS idx_task_llm_profiles_order ON task_llm_profiles(task_id, position);
CREATE INDEX IF NOT EXISTS idx_task_llm_profiles_profile ON task_llm_profiles(profile_id, task_id);
CREATE INDEX IF NOT EXISTS idx_tasks_llm_profile ON tasks(llm_profile_id) WHERE llm_profile_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_tasks_active_llm_profile ON tasks(active_llm_profile_id) WHERE active_llm_profile_id IS NOT NULL;
DROP TRIGGER IF EXISTS trg_task_llm_profiles_upd ON task_llm_profiles;
CREATE TRIGGER trg_task_llm_profiles_upd BEFORE UPDATE ON task_llm_profiles
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- One-time-compatible backfill: old pinned tasks become one-entry chains. A user
-- can still clear the chain later because the update path also clears the legacy
-- llm_profile_id column, preventing this block from re-adding it on restart.
INSERT INTO task_llm_profiles(task_id, profile_id, position)
SELECT t.id, t.llm_profile_id, 0
FROM tasks t
WHERE t.llm_profile_id IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM task_llm_profiles x WHERE x.task_id=t.id)
ON CONFLICT DO NOTHING;
UPDATE tasks t
SET active_llm_profile_id = t.llm_profile_id
WHERE t.active_llm_profile_id IS NULL
  AND t.llm_profile_id IS NOT NULL
  AND EXISTS (SELECT 1 FROM task_llm_profiles x WHERE x.task_id=t.id AND x.profile_id=t.llm_profile_id);

-- 任务测试范围（资产覆盖度的分母 + 授权边界）。
--   自动填(source='auto')：insertAssets 顶层按 worker 显式插入的资产类型加保守范围
--     （root_domain→root_domain，subdomain/service/endpoint→subdomain(host)，ip→ip）；
--     side-effect 派生的资产不入范围（钩子在 handler 顶层，派生在 db 层内部）。
--   agent 填(source='agent')：add_task_scope 加 company/root_domain/subdomain/ip。
-- 覆盖度 = 匹配 active 行的 assets（分母）中，被 fact 节点锚定过的占比（分子）。
CREATE TABLE IF NOT EXISTS task_scope (
    id          BIGSERIAL PRIMARY KEY,
    task_id     BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    kind        TEXT NOT NULL CHECK (kind IN ('company','root_domain','subdomain','ip','cidr','icp','keyword')),
    company_id  BIGINT REFERENCES companies(id) ON DELETE CASCADE,  -- kind='company'
    domain      TEXT,          -- root_domain / subdomain
    net         CIDR,          -- ip / cidr
    value       TEXT,          -- icp / keyword
    source      TEXT NOT NULL DEFAULT 'auto' CHECK (source IN ('auto','agent','manual')),
    reason      TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- 旧库升级：扩展任务范围，使其与企业范围的单文本框识别能力一致。
ALTER TABLE task_scope ADD COLUMN IF NOT EXISTS value TEXT;
ALTER TABLE task_scope DROP CONSTRAINT IF EXISTS task_scope_kind_check;
ALTER TABLE task_scope ADD CONSTRAINT task_scope_kind_check
    CHECK (kind IN ('company','root_domain','subdomain','ip','cidr','icp','keyword'));
-- 去重：同一 task 的同一条范围只存一次（自动填批量插入靠它幂等）。
DROP INDEX IF EXISTS uq_task_scope;
CREATE UNIQUE INDEX IF NOT EXISTS uq_task_scope_v2 ON task_scope(
    task_id, kind, COALESCE(domain,''), COALESCE(net::text,''), COALESCE(company_id,0), COALESCE(value,''));
CREATE INDEX IF NOT EXISTS idx_ts_domain  ON task_scope(domain) WHERE kind IN ('root_domain','subdomain');
CREATE INDEX IF NOT EXISTS idx_ts_net     ON task_scope USING GIST(net inet_ops) WHERE kind IN ('ip','cidr');
CREATE INDEX IF NOT EXISTS idx_ts_company ON task_scope(company_id) WHERE kind = 'company';

-- =====================================================================
-- E. Agents / 提示词模板 / 变量目录
-- =====================================================================
CREATE TABLE IF NOT EXISTS agents (
    id                BIGSERIAL PRIMARY KEY,
    key               TEXT NOT NULL UNIQUE CHECK (key ~ '^[a-z][a-z0-9_]*$'),
    name              TEXT NOT NULL,
    description       TEXT,
    role              TEXT NOT NULL,
    builtin           BOOLEAN NOT NULL DEFAULT true,
    enabled           BOOLEAN NOT NULL DEFAULT true,
    llm_profile_id    BIGINT REFERENCES llm_profiles(id) ON DELETE SET NULL,
    current_prompt_id BIGINT,
    max_turns         INTEGER NOT NULL DEFAULT 0,
    run_seconds       INTEGER NOT NULL DEFAULT 1200,
    web_search        BOOLEAN NOT NULL DEFAULT false,
    interactive_shell BOOLEAN NOT NULL DEFAULT false,
    wrapup_prompt     TEXT NOT NULL DEFAULT '',
    wrapup_max_turns  INTEGER NOT NULL DEFAULT 0,
    task_timeout_wrapup_prompt    TEXT NOT NULL DEFAULT '',
    task_timeout_wrapup_max_turns INTEGER NOT NULL DEFAULT 0,
    trigger_run_mode     TEXT    NOT NULL DEFAULT 'serial'  CHECK (trigger_run_mode IN ('serial','parallel')),
    trigger_merge_mode   TEXT    NOT NULL DEFAULT 'all' CHECK (trigger_merge_mode IN ('by_task','all','none')),
    trigger_max_parallel INTEGER NOT NULL DEFAULT 5,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT agents_role_ck CHECK (role IN ('goals','main','planner','worker','assistant'))
);
-- 加列迁移(已发版,旧库升级补列;新库 CREATE 已含。迁移不带 CHECK:旧库存量安全 + 后端写入白名单兜底)。
ALTER TABLE agents ADD COLUMN IF NOT EXISTS trigger_run_mode     TEXT    NOT NULL DEFAULT 'serial';
ALTER TABLE agents ADD COLUMN IF NOT EXISTS trigger_merge_mode   TEXT    NOT NULL DEFAULT 'all';
ALTER TABLE agents ADD COLUMN IF NOT EXISTS trigger_max_parallel INTEGER NOT NULL DEFAULT 5;
-- per-agent LLM 绑定(agent 级默认模型):列自初版即在上方 CREATE 中,此 ALTER 仅为极旧库兜底(幂等)。
ALTER TABLE agents ADD COLUMN IF NOT EXISTS llm_profile_id BIGINT REFERENCES llm_profiles(id) ON DELETE SET NULL;
-- run_seconds 单次 run 墙钟默认 600→1200:只改列默认(影响将来新插入的行),不动旧库存量行。
ALTER TABLE agents ALTER COLUMN run_seconds SET DEFAULT 1200;
CREATE INDEX IF NOT EXISTS idx_agents_llm_profile ON agents(llm_profile_id) WHERE llm_profile_id IS NOT NULL;
DROP TRIGGER IF EXISTS trg_agents_upd ON agents;
CREATE TRIGGER trg_agents_upd BEFORE UPDATE ON agents
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS agent_prompts (
    id            BIGSERIAL PRIMARY KEY,
    agent_id      BIGINT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    version       INT NOT NULL,
    template_text TEXT NOT NULL,
    note          TEXT,
    updated_by    TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (agent_id, version)
);
-- 循环外键：agents.current_prompt_id → agent_prompts.id（需在两表创建后加）
DO $$ BEGIN
    ALTER TABLE agents ADD CONSTRAINT fk_agents_curprompt
        FOREIGN KEY (current_prompt_id) REFERENCES agent_prompts(id) ON DELETE SET NULL;
EXCEPTION WHEN duplicate_object THEN NULL; END $$;

CREATE TABLE IF NOT EXISTS agent_prompt_vars (
    id          BIGSERIAL PRIMARY KEY,
    agent_id    BIGINT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    var_name    TEXT NOT NULL,
    description TEXT,
    example     TEXT,
    source      TEXT NOT NULL CHECK (source IN ('exploration','runtime','distilled')),
    UNIQUE (agent_id, var_name)
);

-- =====================================================================
-- F. MCP 服务
-- =====================================================================
CREATE TABLE IF NOT EXISTS mcp_servers (
    id          BIGSERIAL PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    transport   TEXT NOT NULL CHECK (transport IN ('stdio','http','sse')),
    command     TEXT,
    args        JSONB NOT NULL DEFAULT '[]',
    env         JSONB NOT NULL DEFAULT '{}',
    url         TEXT,
    enabled     BOOLEAN NOT NULL DEFAULT true,
    insecure    BOOLEAN NOT NULL DEFAULT false,  -- http: 跳过 TLS 证书校验(自签证书场景, issue #108)
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Allow legacy MCP SSE servers on databases created before SSE support.
ALTER TABLE mcp_servers DROP CONSTRAINT IF EXISTS mcp_servers_transport_check;
ALTER TABLE mcp_servers ADD CONSTRAINT mcp_servers_transport_check
    CHECK (transport IN ('stdio','http','sse'));
-- 旧库补列(schema.sql 每次启动都会 Exec)。
ALTER TABLE mcp_servers ADD COLUMN IF NOT EXISTS insecure BOOLEAN NOT NULL DEFAULT false;
DROP TRIGGER IF EXISTS trg_mcp_upd ON mcp_servers;
CREATE TRIGGER trg_mcp_upd BEFORE UPDATE ON mcp_servers
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS mcp_tools_cache (
    id            BIGSERIAL PRIMARY KEY,
    server_id     BIGINT NOT NULL REFERENCES mcp_servers(id) ON DELETE CASCADE,
    tool_name     TEXT NOT NULL,
    description   TEXT,
    schema        JSONB,
    discovered_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (server_id, tool_name)
);

-- =====================================================================
-- G. 可见性：agent × mcp / skill
-- =====================================================================
CREATE TABLE IF NOT EXISTS agent_visibility (
    agent_id      BIGINT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    resource_kind TEXT   NOT NULL CHECK (resource_kind IN ('mcp')),
    resource_id   BIGINT NOT NULL,
    mcp_tool_name TEXT   NOT NULL DEFAULT '',
    enabled       BOOLEAN NOT NULL DEFAULT true,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (agent_id, resource_kind, resource_id, mcp_tool_name)
);
CREATE INDEX IF NOT EXISTS idx_vis_resource ON agent_visibility(resource_kind, resource_id);

CREATE TABLE IF NOT EXISTS agent_skill_visibility (
    agent_id   BIGINT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    skill_name TEXT   NOT NULL,
    enabled    BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (agent_id, skill_name)
);
CREATE INDEX IF NOT EXISTS idx_askv_skill ON agent_skill_visibility(skill_name);

-- Skill 调用账本（见 db/skill_usage.go）。一次 Skill() 调用一行，只记维度不记正文。
-- 刻意不设外键：任务/会话删除后统计仍要保留（与 llm_usage 同理），skill 本身也只是
-- 文件系统上的目录名，没有对应的表。
CREATE TABLE IF NOT EXISTS skill_usage (
    id             BIGSERIAL PRIMARY KEY,
    ts             TIMESTAMPTZ NOT NULL DEFAULT now(),
    skill          TEXT NOT NULL,
    agent_key      TEXT,
    task_id        BIGINT,
    exploration_id BIGINT,
    intent_id      BIGINT,
    session_id     TEXT,
    args_len       INTEGER NOT NULL DEFAULT 0,
    -- false = 模型点名了一个不存在的 skill(未命中)。这类行同样保留：它反映"想用但没有"
    -- 的缺口，是补 skill 的依据。
    found          BOOLEAN NOT NULL DEFAULT true
);
CREATE INDEX IF NOT EXISTS idx_skill_usage_skill ON skill_usage(skill, ts DESC);
CREATE INDEX IF NOT EXISTS idx_skill_usage_task  ON skill_usage(task_id);

-- 工具调用账本（见 db/tool_usage.go）。一次实际 CoreTool.Call 一行，只记归属维度，
-- 不保存工具参数或返回内容。刻意不设外键，任务、会话或自定义工具删除后仍保留统计。
CREATE TABLE IF NOT EXISTS tool_usage (
    id             BIGSERIAL PRIMARY KEY,
    ts             TIMESTAMPTZ NOT NULL DEFAULT now(),
    tool_key       TEXT NOT NULL,
    agent_key      TEXT,
    task_id        BIGINT,
    exploration_id BIGINT,
    intent_id      BIGINT,
    session_id     TEXT
);
CREATE INDEX IF NOT EXISTS idx_tool_usage_tool ON tool_usage(tool_key, ts DESC);
CREATE INDEX IF NOT EXISTS idx_tool_usage_task ON tool_usage(task_id);

-- =====================================================================
-- H. 内置工具目录
-- =====================================================================
CREATE TABLE IF NOT EXISTS tools (
    key         TEXT PRIMARY KEY,
    system      BOOLEAN NOT NULL DEFAULT true,
    description TEXT    NOT NULL DEFAULT '',
    schema      JSONB   NOT NULL DEFAULT '{}',
    agents      JSONB   NOT NULL DEFAULT '[]',
    enabled     BOOLEAN NOT NULL DEFAULT true,
    kind        TEXT    NOT NULL DEFAULT 'builtin',
    exec        JSONB   NOT NULL DEFAULT '{}',
    deferred    BOOLEAN NOT NULL DEFAULT false,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
DROP TRIGGER IF EXISTS trg_tools_upd ON tools;
CREATE TRIGGER trg_tools_upd BEFORE UPDATE ON tools
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- =====================================================================
-- I. 会话（对话页）
-- =====================================================================
CREATE TABLE IF NOT EXISTS conversations (
    id             BIGSERIAL PRIMARY KEY,
    agent_key      TEXT NOT NULL,
    title          TEXT NOT NULL DEFAULT '',
    llm_profile_id BIGINT REFERENCES llm_profiles(id) ON DELETE SET NULL,
    pinned_at      TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE conversations ADD COLUMN IF NOT EXISTS pinned_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_conversations_llm_profile ON conversations(llm_profile_id) WHERE llm_profile_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_conversations_pinned ON conversations(pinned_at DESC) WHERE pinned_at IS NOT NULL;
DROP TRIGGER IF EXISTS trg_conversations_upd ON conversations;
CREATE TRIGGER trg_conversations_upd BEFORE UPDATE ON conversations
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS conversation_activities (
    id                 BIGSERIAL PRIMARY KEY,
    conversation_id    BIGINT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    worker             TEXT,
    kind               TEXT,
    tool               TEXT,
    tool_use_id        TEXT,
    is_error           BOOLEAN NOT NULL DEFAULT false,
    summary            TEXT,
    detail             TEXT,
    input_tokens       INTEGER,
    output_tokens      INTEGER,
    cache_read_tokens  INTEGER,
    cache_write_tokens INTEGER,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_conv_act ON conversation_activities(conversation_id, id);
CREATE INDEX IF NOT EXISTS idx_conv_act_tool_call ON conversation_activities(conversation_id, tool_use_id, id)
  WHERE kind IN ('tool_use', 'tool_result');

-- =====================================================================
-- J. Agent 触发器
-- =====================================================================
CREATE TABLE IF NOT EXISTS agent_triggers (
    id                          BIGSERIAL PRIMARY KEY,
    agent_key                   TEXT NOT NULL,
    enabled                     BOOLEAN NOT NULL DEFAULT true,
    interval_sec                INTEGER NOT NULL DEFAULT 0,
    on_finding                  BOOLEAN NOT NULL DEFAULT false,
    on_goal_met                 BOOLEAN NOT NULL DEFAULT false,
    on_task_timeout             BOOLEAN NOT NULL DEFAULT false,
    on_tool_call                BOOLEAN NOT NULL DEFAULT false,
    on_task_create              BOOLEAN NOT NULL DEFAULT false,
    interval_message            TEXT NOT NULL DEFAULT '',
    finding_message             TEXT NOT NULL DEFAULT '',
    goal_message                TEXT NOT NULL DEFAULT '',
    task_timeout_message        TEXT NOT NULL DEFAULT '',
    tool_call_message           TEXT NOT NULL DEFAULT '',
    task_create_message         TEXT NOT NULL DEFAULT '',
    tool_names                  TEXT NOT NULL DEFAULT '',
    last_fire                   TIMESTAMPTZ,
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_agent_triggers_agent ON agent_triggers(agent_key);
-- 加列迁移(已发版,旧库升级补列;新库 CREATE 已含这些列,ALTER 为 no-op)。幂等,每次启动可重复执行。
ALTER TABLE agent_triggers ADD COLUMN IF NOT EXISTS on_tool_call        BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE agent_triggers ADD COLUMN IF NOT EXISTS tool_call_message   TEXT    NOT NULL DEFAULT '';
ALTER TABLE agent_triggers ADD COLUMN IF NOT EXISTS tool_names          TEXT    NOT NULL DEFAULT '';
ALTER TABLE agent_triggers ADD COLUMN IF NOT EXISTS on_task_create      BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE agent_triggers ADD COLUMN IF NOT EXISTS task_create_message TEXT    NOT NULL DEFAULT '';
DROP TRIGGER IF EXISTS trg_agent_triggers_upd ON agent_triggers;
CREATE TRIGGER trg_agent_triggers_upd BEFORE UPDATE ON agent_triggers
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS scheduler_state (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL DEFAULT ''
);

-- =====================================================================
-- K. 拦截规则
-- =====================================================================
CREATE TABLE IF NOT EXISTS intercept_rules (
    id              BIGSERIAL PRIMARY KEY,
    name            TEXT NOT NULL,
    enabled         BOOLEAN NOT NULL DEFAULT true,
    priority        INTEGER NOT NULL DEFAULT 0,
    -- draft_field 是草案闸门用的目标类型（WE-7 加入）。
    -- 只写 tool_name / tool_input 会让内置的 5 条草案规则插不进去。
    match_target    TEXT NOT NULL CHECK (match_target IN ('tool_name', 'tool_input', 'draft_field')),
    -- string/regex 是文本类 matcher；后四种是结构化 matcher（WE-7 加入）：
    --   numeric_lte  单笔金额上限      list        标的/字段白名单
    --   time_window  交易时段          daily_count 日内次数/集中度
    match_type      TEXT NOT NULL CHECK (match_type IN (
                        'string', 'regex', 'numeric_lte', 'list', 'time_window', 'daily_count')),
    pattern         TEXT NOT NULL,
    -- pattern_params 是结构化 matcher 的参数载体（JSON 对象）。
    -- string/regex 规则留空对象；shape 见 ARCH-W2-W5 §2.6.2。
    pattern_params  JSONB NOT NULL DEFAULT '{}'::jsonb,
    action          TEXT NOT NULL CHECK (action IN ('allow', 'deny', 'ask')),
    message         TEXT NOT NULL DEFAULT '',
    timeout_enabled BOOLEAN NOT NULL DEFAULT true,
    timeout_seconds INTEGER NOT NULL DEFAULT 60,
    timeout_action  TEXT    NOT NULL DEFAULT 'deny',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
DROP TRIGGER IF EXISTS trg_intercept_rules_upd ON intercept_rules;
CREATE TRIGGER trg_intercept_rules_upd BEFORE UPDATE ON intercept_rules
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- 已有库的迁移（CREATE TABLE IF NOT EXISTS 对已存在的表不生效）。
-- 三处都与上面的 CREATE 保持一致：新增列 + 放宽两个 CHECK 的允许值集合。
--
-- 为什么要放宽 CHECK：WE-7 加了草案闸门，match_target 多出 draft_field，
-- match_type 多出 4 种结构化 matcher。旧约束会让内置的 5 条草案规则插不进去。
ALTER TABLE intercept_rules
    ADD COLUMN IF NOT EXISTS pattern_params JSONB NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE intercept_rules DROP CONSTRAINT IF EXISTS intercept_rules_match_target_check;
ALTER TABLE intercept_rules ADD CONSTRAINT intercept_rules_match_target_check
    CHECK (match_target IN ('tool_name', 'tool_input', 'draft_field'));
ALTER TABLE intercept_rules DROP CONSTRAINT IF EXISTS intercept_rules_match_type_check;
ALTER TABLE intercept_rules ADD CONSTRAINT intercept_rules_match_type_check
    CHECK (match_type IN ('string', 'regex', 'numeric_lte', 'list', 'time_window', 'daily_count'));

CREATE TABLE IF NOT EXISTS intercept_pending (
    id              BIGSERIAL PRIMARY KEY,
    rule_id         BIGINT REFERENCES intercept_rules(id) ON DELETE SET NULL,
    conversation_id BIGINT REFERENCES conversations(id) ON DELETE CASCADE,
    task_id         TEXT,
    agent_name      TEXT NOT NULL DEFAULT '',
    tool_name       TEXT NOT NULL,
    tool_input      JSONB NOT NULL DEFAULT '{}',
    status          TEXT NOT NULL DEFAULT 'pending'
                        CHECK (status IN ('pending', 'allowed', 'denied', 'timeout')),
    -- 判定理由:规则命中时为规则 message;LLM 兜底判定时为模型给的简短理由(前缀 [模型])。
    reason          TEXT NOT NULL DEFAULT '',
    decided_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_intercept_pending_status ON intercept_pending(status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_intercept_pending_task   ON intercept_pending(task_id, created_at DESC);
-- 补旧库:reason 列(已发版,加列要带 IF NOT EXISTS)。
ALTER TABLE intercept_pending ADD COLUMN IF NOT EXISTS reason TEXT NOT NULL DEFAULT '';
-- Detail payloads are lazy-loaded; NULL preserves the meaning of legacy history.
ALTER TABLE intercept_pending ADD COLUMN IF NOT EXISTS audit JSONB;
ALTER TABLE intercept_pending ADD COLUMN IF NOT EXISTS decision_source TEXT NOT NULL DEFAULT '';
UPDATE intercept_pending SET decision_source=CASE WHEN rule_id IS NOT NULL THEN 'rule'
 WHEN reason LIKE '[模型]%' THEN 'model' ELSE 'unknown' END WHERE decision_source='';

-- =====================================================================
-- L. 漏洞发现持久化
-- =====================================================================
CREATE TABLE IF NOT EXISTS findings (
    id          BIGSERIAL PRIMARY KEY,
    task_id     BIGINT REFERENCES tasks(id) ON DELETE SET NULL,
    node_id     BIGINT REFERENCES exploration_nodes(id) ON DELETE SET NULL,
    vulnclass   TEXT NOT NULL DEFAULT '',
    -- 漏洞名称(可读标题)；为空时前端回退展示 vulnclass。severity 取值：
    -- critical 严重 / high 高 / medium 中 / low 低（不加 CHECK，与 status 一致由 server 白名单校验）。
    name        TEXT NOT NULL DEFAULT '',
    severity    TEXT NOT NULL DEFAULT '',
    summary     TEXT NOT NULL DEFAULT '',
    evidence    TEXT NOT NULL DEFAULT '',
    worker      TEXT NOT NULL DEFAULT '',
    asset_ids   JSONB NOT NULL DEFAULT '[]',
    -- 处置状态：pending 待处理 / in_progress 处理中 / confirmed 已确认 / resolved 已处理 / fixed 已修复 /
    -- false_positive 误报 / ignored 忽略 / duplicate 重复 / risk_accepted 风险接受。
    -- 取值不加 CHECK：旧库靠下面的 ALTER 补列,CHECK 无法回填,统一由 server 侧白名单校验。
    status      TEXT NOT NULL DEFAULT 'pending',
    -- 漏洞详细报告(Markdown)；默认空,仅详情页读取/展示,不进列表接口以免 payload 膨胀。
    report      TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE findings ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT 'pending';
ALTER TABLE findings ADD COLUMN IF NOT EXISTS name   TEXT NOT NULL DEFAULT '';
ALTER TABLE findings ADD COLUMN IF NOT EXISTS report TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_findings_task ON findings(task_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_findings_time ON findings(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_findings_status ON findings(status, created_at DESC);
-- 「按资产」视图靠 asset_ids @> '[<id>]' 反查发现,没有这个 GIN 索引就是全表扫。
CREATE INDEX IF NOT EXISTS idx_findings_asset_ids ON findings USING GIN(asset_ids jsonb_path_ops);

-- 漏洞复测（finding_retests）已删除：它属于旧渗透语义，本产品（Terix）不做复测。
-- 已有库的清理在 db.seedAgentRemovals 里做（flag finding_retests_dropped_v1，幂等）。
-- 全新库不建表；旧库靠上面的 DROP 迁移清掉表 + 索引 + 触发器。
DROP TRIGGER IF EXISTS trg_conversation_retest_delete ON conversations;
DROP FUNCTION IF EXISTS stop_deleted_conversation_retest();
DROP TABLE IF EXISTS finding_retests CASCADE;

ALTER TABLE findings ADD COLUMN IF NOT EXISTS evidence_version BIGINT NOT NULL DEFAULT 0;
ALTER TABLE findings ADD COLUMN IF NOT EXISTS report_evidence_version BIGINT NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS traffic_evidence_snapshots (
    id TEXT PRIMARY KEY,
    source_traffic_id TEXT NOT NULL,
    captured_at BIGINT NOT NULL,
    url TEXT NOT NULL,
    method TEXT NOT NULL,
    status INTEGER NOT NULL,
    content_type TEXT NOT NULL DEFAULT '',
    req_head TEXT NOT NULL,
    resp_head TEXT NOT NULL,
    req_hash TEXT NOT NULL,
    resp_hash TEXT NOT NULL,
    req_len BIGINT NOT NULL,
    resp_len BIGINT NOT NULL,
    unreferenced_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS finding_traffic_bindings (
    id BIGSERIAL PRIMARY KEY,
    finding_id BIGINT NOT NULL REFERENCES findings(id) ON DELETE CASCADE,
    snapshot_id TEXT NOT NULL REFERENCES traffic_evidence_snapshots(id),
    role TEXT NOT NULL DEFAULT 'supporting',
    note TEXT NOT NULL DEFAULT '',
    position INTEGER NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(finding_id, snapshot_id)
);
CREATE INDEX IF NOT EXISTS idx_finding_traffic_order ON finding_traffic_bindings(finding_id, position, id);
CREATE INDEX IF NOT EXISTS idx_finding_traffic_snapshot ON finding_traffic_bindings(snapshot_id);

-- =====================================================================
-- M. 后端日志持久化
-- =====================================================================
CREATE TABLE IF NOT EXISTS server_logs (
    id         BIGSERIAL PRIMARY KEY,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    level      TEXT NOT NULL DEFAULT 'info',
    tag        TEXT NOT NULL DEFAULT '',
    text       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_server_logs_id ON server_logs(id DESC);

-- Independent /btw history and the latest provider-ready main checkpoint.
CREATE TABLE IF NOT EXISTS side_question_sessions (
    session_key TEXT PRIMARY KEY,
    conversation_id BIGINT REFERENCES conversations(id) ON DELETE CASCADE,
    task_id BIGINT REFERENCES tasks(id) ON DELETE CASCADE,
    exploration_id BIGINT REFERENCES explorations(id) ON DELETE CASCADE,
    intent_id BIGINT REFERENCES exploration_nodes(id) ON DELETE CASCADE,
    run_id BIGINT NOT NULL,
    version BIGINT NOT NULL,
    snapshot JSONB NOT NULL,
    generation BIGINT NOT NULL DEFAULT 0,
    CHECK ((conversation_id IS NOT NULL AND task_id IS NULL AND exploration_id IS NULL AND intent_id IS NULL)
        OR (conversation_id IS NULL AND task_id IS NOT NULL AND exploration_id IS NOT NULL))
);
CREATE INDEX IF NOT EXISTS idx_side_sessions_conv ON side_question_sessions(conversation_id);
CREATE INDEX IF NOT EXISTS idx_side_sessions_task ON side_question_sessions(task_id);
CREATE INDEX IF NOT EXISTS idx_side_sessions_exp ON side_question_sessions(exploration_id);
CREATE INDEX IF NOT EXISTS idx_side_sessions_intent ON side_question_sessions(intent_id);

CREATE TABLE IF NOT EXISTS side_question_requests (
    id TEXT PRIMARY KEY,
    ordinal BIGSERIAL UNIQUE,
    session_key TEXT NOT NULL REFERENCES side_question_sessions(session_key) ON DELETE CASCADE,
    generation BIGINT NOT NULL,
    client_id TEXT NOT NULL,
    question TEXT NOT NULL,
    answer TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL CHECK(status IN ('running','completed','failed','cancelled','interrupted')),
    error TEXT NOT NULL DEFAULT '',
    model JSONB NOT NULL,
    snapshot_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    sequence BIGINT NOT NULL DEFAULT 0,
    usage JSONB NOT NULL DEFAULT '{}',
    UNIQUE(session_key,generation,client_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_side_request_running ON side_question_requests(session_key) WHERE status='running';
CREATE INDEX IF NOT EXISTS idx_side_requests_history ON side_question_requests(session_key,ordinal DESC);

-- Additive v3 archive fields; old archives restore these as empty objects.
ALTER TABLE side_question_sessions ADD COLUMN IF NOT EXISTS memory JSONB NOT NULL DEFAULT '{}';
ALTER TABLE side_question_requests ADD COLUMN IF NOT EXISTS context_info JSONB NOT NULL DEFAULT '{}';

-- =====================================================================
-- 资产拦截规则（全局黑名单）
-- 独立于 §K 命令拦截(intercept_rules)：intercept_rules 匹配工具名/入参文本，
-- 这张表匹配「目标资产」——全等/模糊的域名·IP·URL 以及 CIDR 网段。
-- 仅存规则；具体的匹配/拦截逻辑在别处实现。
-- kind 七种：
--   exact_domain / exact_ip / exact_url  —— 全等匹配
--   fuzzy_domain / fuzzy_ip / fuzzy_url  —— 模糊匹配
--   cidr                                 —— CIDR 网段
-- =====================================================================
CREATE TABLE IF NOT EXISTS asset_intercept_rules (
    id          BIGSERIAL PRIMARY KEY,
    enabled     BOOLEAN NOT NULL DEFAULT true,
    kind        TEXT NOT NULL CHECK (kind IN (
                    'exact_domain', 'exact_ip', 'exact_url',
                    'fuzzy_domain', 'fuzzy_ip', 'fuzzy_url',
                    'cidr')),
    pattern     TEXT NOT NULL,
    note        TEXT NOT NULL DEFAULT '',
    builtin     BOOLEAN NOT NULL DEFAULT false,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_asset_intercept_enabled ON asset_intercept_rules(enabled);
DROP TRIGGER IF EXISTS trg_asset_intercept_rules_upd ON asset_intercept_rules;
CREATE TRIGGER trg_asset_intercept_rules_upd BEFORE UPDATE ON asset_intercept_rules
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- =====================================================================
-- 任务级资产拦截/允许规则
-- 与全局 asset_intercept_rules 同构（kind/pattern/note/enabled），但按 task_id
-- 关联、随任务级联删除；创建任务时录入、任务详情里可编辑。
-- action: 'block'=拦截(禁止测试)  'allow'=允许(白名单)。
-- 执行判定：先按 拦截规则(全局 ∪ 任务block) 匹配，命中即禁止；未命中且该任务存在
-- 启用的 allow 规则时，须命中某条 allow 才放行，否则「不允许测试」。
-- =====================================================================
CREATE TABLE IF NOT EXISTS task_intercept_rules (
    id          BIGSERIAL PRIMARY KEY,
    task_id     BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    enabled     BOOLEAN NOT NULL DEFAULT true,
    action      TEXT NOT NULL DEFAULT 'block' CHECK (action IN ('block','allow')),
    kind        TEXT NOT NULL CHECK (kind IN (
                    'exact_domain', 'exact_ip', 'exact_url',
                    'fuzzy_domain', 'fuzzy_ip', 'fuzzy_url',
                    'cidr')),
    pattern     TEXT NOT NULL,
    note        TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_task_intercept_task ON task_intercept_rules(task_id);
-- 补旧库(本会话早前建过该表、无 action 列)：加列(带 IF NOT EXISTS)。
ALTER TABLE task_intercept_rules ADD COLUMN IF NOT EXISTS action TEXT NOT NULL DEFAULT 'block';
DROP TRIGGER IF EXISTS trg_task_intercept_rules_upd ON task_intercept_rules;
CREATE TRIGGER trg_task_intercept_rules_upd BEFORE UPDATE ON task_intercept_rules
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- =====================================================================
-- M. 漏洞 IM 推送
--
-- 三张表刻意分开，核心是**爆炸半径**：写漏洞的那个事务(RecordFindingTx，
-- 持任务行锁)只允许做一次盲 INSERT，不读渠道表、不跑用户的过滤规则。否则
-- 一条配错的 webhook 过滤条件就能污染/中止事务，导致漏洞存不进去。
--
--   notification_channels   渠道实例配置(可变、含凭据、UI 管理)
--   notification_events     事件事实(写漏洞事务内盲插，含渲染快照)
--   notification_deliveries 投递任务(事务外 fan-out 产生，承载状态/重试/批次)
-- =====================================================================

-- 渠道实例：同一 kind 可配任意多个(如「应急群」「日常群」各一个钉钉机器人)。
-- kind 取值由 server 侧白名单校验，不加 CHECK：与 findings.status 同理，
-- 后续加渠道不应要求改表结构。
CREATE TABLE IF NOT EXISTS notification_channels (
    id           BIGSERIAL PRIMARY KEY,
    name         TEXT NOT NULL,
    -- dingtalk 钉钉 / feishu 飞书 / wecom 企业微信 / webhook 通用 / telegram / email
    kind         TEXT NOT NULL,
    enabled      BOOLEAN NOT NULL DEFAULT true,
    -- 凭据(明文存储，UI 掩码回显；见 server 侧 maskChannelSecrets)。六种渠道字段差异极大，
    -- 统一 JSONB + Go 侧按 kind 严格校验，避免为每渠道加一堆 NULL 列：
    --   dingtalk {webhook,secret}
    --   feishu   {webhook,secret}
    --   wecom    {webhook}
    --   webhook  {url,method,content_type,headers{},body_template}
    --   telegram {bot_token,chat_id,base_url}
    --   email    {host,port,username,password,from,to[],tls}
    config       JSONB NOT NULL DEFAULT '{}',
    -- 推送时机：realtime 命中即推 / digest 进批次按全局周期汇总成一条。
    mode         TEXT NOT NULL DEFAULT 'realtime',
    -- 过滤条件，字段全部可选(缺省=不过滤)：
    --   min_severity       ''|low|medium|high|critical
    --   task_ids/asset_ids 空数组=不限；非空则须交集非空
    --   vulnclass_include/exclude 关键词数组(大小写不敏感子串)；include 空=全收
    --   on_status_change   bool，仅 realtime 模式有意义
    filter       JSONB NOT NULL DEFAULT '{}',
    -- 每分钟投递上限；0=不限流。默认 20 对齐钉钉/企微官方硬限。
    -- 超限不丢消息，只把投递推迟到下一个 tick。
    rate_per_min INTEGER NOT NULL DEFAULT 20,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
DROP TRIGGER IF EXISTS trg_notification_channels_upd ON notification_channels;
CREATE TRIGGER trg_notification_channels_upd BEFORE UPDATE ON notification_channels
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- 事件事实。由 RecordFindingTx / 状态变更事务**同事务**写入，保证「漏洞落库」
-- 与「推送任务存在」原子一致——不存在提交成功但没入队、消息永久丢失的窗口。
-- snapshot 刻意冗余：漏洞事后会被改名/改级别/改状态，推送内容应反映「事发当时」，
-- 且 fan-out 与渲染不必回查 findings/tasks/assets 多张表。
-- finding 删除后事件不级联删除：与 findings 表「任务删除仍独立留存」的语义一致。
CREATE TABLE IF NOT EXISTS notification_events (
    id         BIGSERIAL PRIMARY KEY,
    -- finding_created | finding_status_changed
    kind       TEXT NOT NULL,
    finding_id BIGINT NOT NULL,
    snapshot   JSONB NOT NULL,
    -- fan-out 幂等标记：dispatcher 按此列取待分派事件，处理完置 true。
    -- 用列而非删行，以便投递历史能回溯到事件。
    fanned_out BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_notification_events_pending
    ON notification_events(id) WHERE NOT fanned_out;

-- 投递任务：一条事件 × 一个启用渠道 = 一行。fan-out 在事务外做，所以渠道
-- 后开不会补历史(与 agent_triggers 的「迟开 trigger 不补历史」语义一致，
-- 避免启用渠道时一次性刷屏历史积压)。
-- channel_id 级联删除：渠道配置都没了，其投递历史无意义。
CREATE TABLE IF NOT EXISTS notification_deliveries (
    id          BIGSERIAL PRIMARY KEY,
    event_id    BIGINT NOT NULL REFERENCES notification_events(id) ON DELETE CASCADE,
    channel_id  BIGINT NOT NULL REFERENCES notification_channels(id) ON DELETE CASCADE,
    -- pending 待发 / sent 已发 / failed 重试耗尽(可手动重发) / skipped 渠道停用或批次取消
    -- pending 待发 / sending 已被某 dispatcher 领取(租约未到期) / sent 已发 /
    -- failed 重试耗尽或永久失败(可手动重发) / skipped 渠道停用。取值不加 CHECK，
    -- 与 findings.status 同理，由 server 侧白名单校验。
    state       TEXT NOT NULL DEFAULT 'pending',
    attempts    INTEGER NOT NULL DEFAULT 0,
    -- 兼作「下次可领取时间」与「租约到期时间」：领取时把它推到未来即构成租约，
    -- 于是「租约未到期」与「未到重试时间」共用同一个条件表达，不需要额外的
    -- lease_until 列。进程崩溃留下的 sending 行会因租约到期被下一轮重新领取。
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error  TEXT NOT NULL DEFAULT '',
    -- digest 模式同批次共享；realtime 恒为 NULL。整批渲染成一条消息后一起置 sent。
    batch_id    BIGINT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    sent_at     TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_notification_deliveries_due
    ON notification_deliveries(next_attempt_at) WHERE state='pending';
CREATE INDEX IF NOT EXISTS idx_notification_deliveries_history
    ON notification_deliveries(id DESC);
CREATE INDEX IF NOT EXISTS idx_notification_deliveries_batch
    ON notification_deliveries(batch_id) WHERE batch_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_notification_deliveries_channel
    ON notification_deliveries(channel_id, id DESC);


-- ===========================================================================
-- 金融 / 交易模块的表（WE-1 ~ WE-10）
--
-- # 为什么这一整段是后来补的
--
-- 这些表在开发期是由手工 psql 建进 DB 的，DDL 从未提交过。
-- 结果：DB 有 69 张表，schema.sql 只有 50 张 —— 新环境建库后
-- 整个金融功能报 relation does not exist，而本地测试全绿
-- （因为本地库的表是历史累积的，CREATE TABLE IF NOT EXISTS 让缺表静默）。
--
-- # 两阶段结构
--
--   阶段 1：所有 CREATE TABLE
--   阶段 2：所有 ALTER TABLE ADD CONSTRAINT / CREATE INDEX / CREATE TRIGGER
--
-- 为什么不用「严格按依赖排序 + 单阶段」：外键依赖形成 DAG，
-- 任何「CREATE 后紧跟该表的 ALTER」都可能引用尚未创建的表。
-- 两阶段把「建表」和「建约束」彻底分开，顺序问题消失。
--
-- # 不在本文件建的表
--
-- llm_records / llm_usage 由 Go 代码在首次使用时建
-- （db/commands.go:157、db/llm_usage.go:30）。
-- 不在 schema.sql 里重复定义，理由：两处定义同一张表 → 改一处忘另一处
-- → 下次 schema 与 DB 不一致。
--
-- 所以「schema.sql 的 CREATE TABLE 数」不等于「DB 的表数」——
-- 全新库需要跑一次会建这两张表的代码路径（test-fresh-db.sh 里显式处理）。
--
-- 如果将来要统一到 schema.sql，必须同时删掉 Go 里的建表语句。
-- ===========================================================================

-- 阶段 1：CREATE TABLE（被引用的表在前，但顺序不敏感 —— 阶段 2 的 ALTER 才建外键）

CREATE TABLE IF NOT EXISTS instruments (
    id BIGSERIAL PRIMARY KEY,
    market text NOT NULL,
    symbol text NOT NULL,
    name text,
    asset_class text NOT NULL,
    currency text,
    exchange text,
    sector text,
    industry text,
    active boolean DEFAULT true NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CHECK ((asset_class = ANY (ARRAY['stock'::text, 'etf'::text, 'index'::text, 'crypto'::text, 'future'::text, 'option'::text]))),
    CHECK ((market = ANY (ARRAY['us'::text, 'hk'::text, 'cn'::text, 'crypto'::text])))
);

-- ck_instruments_symbol_not_test：拦测试残留的 symbol 前缀。
--
-- # 为什么用 DO $$ 而不是写在 CREATE TABLE 里
--
-- CREATE TABLE IF NOT EXISTS 在已有库上不执行 —— 所以写在表定义里的约束
-- 只对「全新库」生效，已部署的库永远拿不到它。
--
-- # 为什么拦 GATE-TEST- / RATING-FALLBACK-
--
-- 两者都含 TEST，不是任何真实证券代码的形态。
--
-- # 为什么不拦 RD- / DR-
--
-- 它们是 2 字母前缀，理论上可能是某交易所的代码体系（ADR/GDR 分类、
-- 德国交易所的 Registered Dealer）。更重要的是「误伤后的可解释性」：
-- 真实标的被拦时报错只有「violates ck_instruments_symbol_not_test」，
-- 用户不知道 RD 为什么被禁，会花几小时排查一个看起来像 bug 的规则。
--
-- 所以 RD- / DR- 靠「测试改用 TINSTR- 前缀 + cleanup 显式删父表」解决。
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid='instruments'::regclass AND conname='ck_instruments_symbol_not_test'
    ) THEN
        ALTER TABLE instruments ADD CONSTRAINT ck_instruments_symbol_not_test
            CHECK (symbol NOT LIKE 'GATE-TEST-%' AND symbol NOT LIKE 'RATING-FALLBACK%');
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS market_credentials (
    id BIGSERIAL PRIMARY KEY,
    provider text NOT NULL,
    account_label text DEFAULT ''::text NOT NULL,
    api_key text DEFAULT ''::text NOT NULL,
    api_secret text DEFAULT ''::text NOT NULL,
    passphrase text DEFAULT ''::text NOT NULL,
    api_key_hint text DEFAULT ''::text NOT NULL,
    base_url text DEFAULT ''::text NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    verified_at timestamp with time zone,
    verify_error text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    extra jsonb DEFAULT '{}'::jsonb NOT NULL
);

CREATE TABLE IF NOT EXISTS watchlists (
    id BIGSERIAL PRIMARY KEY,
    name text NOT NULL,
    note text,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE IF NOT EXISTS bars (
    id BIGSERIAL PRIMARY KEY,
    instrument_id bigint NOT NULL,
    "interval" text DEFAULT '1d'::text NOT NULL,
    ts timestamp with time zone NOT NULL,
    trading_date date NOT NULL,
    open numeric(28,10),
    high numeric(28,10),
    low numeric(28,10),
    close numeric(28,10),
    adj_close numeric(28,10),
    volume numeric(28,10),
    snapshot_id bigint,
    is_partial boolean DEFAULT false NOT NULL,
    source text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CHECK (("interval" = ANY (ARRAY['1d'::text, '1h'::text, '30m'::text, '5m'::text, '1m'::text])))
);

CREATE TABLE IF NOT EXISTS instrument_relations (
    id BIGSERIAL PRIMARY KEY,
    instrument_id bigint NOT NULL,
    related_type text NOT NULL,
    related_ref text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CHECK ((related_type = ANY (ARRAY['industry'::text, 'macro'::text, 'news_source'::text, 'vendor'::text, 'peer'::text])))
);

CREATE TABLE IF NOT EXISTS snapshots (
    id BIGSERIAL PRIMARY KEY,
    source text NOT NULL,
    endpoint text NOT NULL,
    params_hash text NOT NULL,
    collected_at timestamp with time zone NOT NULL,
    body_ref text,
    checksum text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    coverage_start date,
    coverage_end date,
    bar_count integer DEFAULT 0 NOT NULL
);

CREATE TABLE IF NOT EXISTS signals (
    id BIGSERIAL PRIMARY KEY,
    task_id bigint,
    instrument_id bigint,
    node_id bigint,
    signal_type text NOT NULL,
    direction text NOT NULL,
    confidence numeric(5,4),
    horizon text,
    rationale text,
    status text DEFAULT 'open'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    action text,
    CHECK (((action IS NULL) OR (action = ANY (ARRAY['entry'::text, 'exit'::text, 'scale_in'::text, 'scale_out'::text, 'hold'::text, 'review'::text])))),
    CHECK ((direction = ANY (ARRAY['long'::text, 'short'::text, 'neutral'::text])))
);

CREATE TABLE IF NOT EXISTS signal_settlements (
    id BIGSERIAL PRIMARY KEY,
    signal_id bigint NOT NULL,
    settle_date date NOT NULL,
    entry_ref numeric(20,8),
    exit_ref numeric(20,8),
    return_pct numeric(12,6),
    benchmark_return_pct numeric(12,6),
    alpha numeric(12,6),
    hit boolean,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE IF NOT EXISTS ratings (
    id BIGSERIAL PRIMARY KEY,
    task_id bigint,
    instrument_id bigint,
    -- intent_id 是「产出这条评级的 intent 节点」。
    --
    -- # 为什么需要它
    --
    -- 「PM intent 结束时触发评级 → 草案」需要一个精确判据：
    -- 「这个 intent 产出的评级是哪一条」。
    --
    -- 时间窗口（created_at >= state_at）是猜测 —— 同一秒内两条 intent 结束时会取错。
    -- 而把 rating_id 写进 intent 的 payload 会让 intent 承担「记住产出物」的职责。
    --
    -- # 为什么 ON DELETE SET NULL
    --
    -- 评级是审计记录，不随 intent 删除而消失。
    --
    -- # 为什么可为 NULL
    --
    -- 历史数据、以及（将来）人工补录的评级没有 intent 来源。
    intent_id bigint REFERENCES exploration_nodes(id) ON DELETE SET NULL,
    rating text NOT NULL,
    confidence numeric(5,4),
    decided_at timestamp with time zone,
    evidence_node_ids bigint[],
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    confidence_reason text,
    review_reasons jsonb,
    CHECK ((((rating = 'review'::text) AND (review_reasons IS NOT NULL) AND (review_reasons ? 'data_gaps'::text) AND (review_reasons ? 'conflicting_facts'::text) AND (review_reasons ? 'skipped_analysts'::text) AND (review_reasons ? 'next_step'::text)) OR ((rating <> 'review'::text) AND (review_reasons IS NULL)))),
    CHECK ((rating = ANY (ARRAY['buy'::text, 'overweight'::text, 'hold'::text, 'underweight'::text, 'sell'::text, 'review'::text])))
);

CREATE TABLE IF NOT EXISTS watchlist_items (
    id BIGSERIAL PRIMARY KEY,
    watchlist_id bigint NOT NULL,
    instrument_id bigint NOT NULL,
    note text,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE IF NOT EXISTS exec_accounts (
    id BIGSERIAL PRIMARY KEY,
    channel text NOT NULL,
    channel_account_id text NOT NULL,
    label text DEFAULT ''::text NOT NULL,
    market text NOT NULL,
    market_support text[] DEFAULT '{}'::text[] NOT NULL,
    active boolean DEFAULT true NOT NULL,
    blocked_at timestamp with time zone,
    block_reason text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CHECK ((channel = ANY (ARRAY['fosunwealth'::text, 'okx'::text]))),
    CHECK ((market = ANY (ARRAY['us'::text, 'hk'::text, 'cn'::text, 'crypto'::text]))),
    CHECK ((market_support <@ ARRAY['us'::text, 'hk'::text, 'cn'::text, 'crypto'::text]))
);

CREATE TABLE IF NOT EXISTS exec_authorizations (
    id BIGSERIAL PRIMARY KEY,
    channel text NOT NULL,
    account_ref text NOT NULL,
    mode text NOT NULL,
    from_mode text NOT NULL,
    disclaimer_version text NOT NULL,
    confirm_phrase text NOT NULL,
    limits_snapshot jsonb NOT NULL,
    rules_revision bigint NOT NULL,
    granted_at timestamp with time zone DEFAULT now() NOT NULL,
    granted_by text DEFAULT 'local'::text NOT NULL,
    revoked_at timestamp with time zone,
    revoked_reason text DEFAULT ''::text NOT NULL,
    CHECK ((channel = ANY (ARRAY['fosunwealth'::text, 'okx'::text]))),
    -- 2026-11-05 加 live_auto：从实盘自动往 paper 降档也需要留一条授权记录。
    CHECK ((from_mode = ANY (ARRAY['paper'::text, 'live_manual'::text, 'live_whitelist'::text, 'live_auto'::text]))),
    -- 2026-11-05 加 live_auto：进入实盘自动是独立的一次授权（不是 live_manual 的延伸）。
    CHECK ((mode = ANY (ARRAY['live_whitelist'::text, 'live_manual'::text, 'live_auto'::text])))
);

-- # execution_limits 的设计迭代史（重要）
--
-- ARCH-W2-W5.md:525 记录的是**第一版设计**（EAV 模式）：
--   key + value + currency + scope + scope_ref
--   意图：一行一个 key-value，支持 global / account 两级 scope。
--
-- 实际实现是**第二版**（扁平列）：
--   key + max_single_amount + max_daily_amount + max_daily_orders + mode + whitelist_symbols
--   意图：一行一组限额，key 区分不同配置集。
--
-- # 为什么第二版取代了第一版
--
--   `scope` / `scope_ref` 在代码里**零引用** —— 两级 scope 从未实现。
--   `mode` 列后来加、随后标记废弃（见 db/execution_limits.go 的 DEPRECATED 注释）。
--   `whitelist_symbols` 同样加后又废弃（白名单改由 intercept_rules 承担）。
--
-- # 当前有效字段
--
--   max_single_amount / max_daily_amount / max_daily_orders  ← 三个硬上限，NULL = 未配置
--   revision                                                 ← 每次写 +1，用于授权失效判定
--   mode / whitelist_symbols                                 ← 已废弃，勿使用
--
-- **不要照 ARCH 文档的旧 DDL 建这张表** —— 那套列名与代码不匹配：
-- 代码 scan 的是 MaxSingleAmount / MaxDailyAmount / MaxDailyOrders / WhitelistSymbols，
-- 照文档建会编译通过但运行时全部 `column does not exist`。
CREATE TABLE IF NOT EXISTS execution_limits (
    id BIGSERIAL PRIMARY KEY,
    key text NOT NULL,
    mode text DEFAULT 'paper'::text NOT NULL,
    max_single_amount numeric(20,8),
    max_daily_amount numeric(20,8),
    max_daily_orders integer,
    whitelist_symbols text[],
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    revision bigint DEFAULT 1 NOT NULL,
    -- 2026-11-05 加 live_auto：自动交易需要独立的限额档（比手动更严）。
    CHECK ((mode = ANY (ARRAY['paper'::text, 'live_whitelist'::text, 'live_manual'::text, 'live_auto'::text])))
);

CREATE TABLE IF NOT EXISTS orders_draft (
    id BIGSERIAL PRIMARY KEY,
    signal_id bigint,
    -- rating_id 记录「草案由哪条评级生成」。
    --
    -- # 为什么需要它（幂等的唯一判据）
    --
    -- 「评级 → 草案」必须幂等：同一评级调两次只能生成一个草案。
    -- 而 orders_draft 原来的列里没有能关联回评级的字段
    -- （signal_id 是交易信号，不是评级）。
    --
    -- # 为什么用 UNIQUE 而不是应用层查重
    --
    -- 应用层查重是「先 SELECT 再 INSERT」，两个并发调用会同时查到「没有」
    -- 然后都插入 —— 经典 TOCTOU。而 UNIQUE 约束在 DB 层保证唯一，
    -- 并发下只有一个能成功（另一个拿 23505，调用方转成 created=false）。
    --
    -- # 为什么是 ON DELETE SET NULL 而不是 CASCADE
    --
    -- 删评级不该连带删草案 —— 草案可能已经进过闸门、有执行记录。
    -- 那是审计链的一部分，不能因为上游评级被清理就消失。
    rating_id bigint REFERENCES ratings(id) ON DELETE SET NULL,
    instrument_id bigint,
    side text NOT NULL,
    qty numeric(28,10) NOT NULL,
    ref_price numeric(28,10),
    risk_verdict text,
    exported_at timestamp with time zone,
    executed_note text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CHECK ((risk_verdict = ANY (ARRAY['pass'::text, 'deny'::text, 'ask'::text]))),
    CHECK ((side = ANY (ARRAY['buy'::text, 'sell'::text])))
);

CREATE TABLE IF NOT EXISTS draft_risk_verdicts (
    id BIGSERIAL PRIMARY KEY,
    draft_id bigint NOT NULL,
    verdict text NOT NULL,
    matched_rule_ids bigint[] DEFAULT '{}'::bigint[] NOT NULL,
    rule_results jsonb DEFAULT '[]'::jsonb NOT NULL,
    reason text DEFAULT ''::text NOT NULL,
    rules_revision bigint DEFAULT 0 NOT NULL,
    rules_snapshot jsonb DEFAULT '{}'::jsonb NOT NULL,
    evaluated_by text DEFAULT ''::text NOT NULL,
    evaluated_at timestamp with time zone DEFAULT now() NOT NULL,
    CHECK ((verdict = ANY (ARRAY['pass'::text, 'deny'::text, 'ask'::text])))
);

-- 注意：文档 ARCH-W2-W5.md:322 的 DDL 漏了两项，以本文件为准：
--
--   created_at      —— state_at 每次 Transition 都会被 UPDATE 改，
--                      而「这单什么时候建的」在审计/对账时是必答问题。
--   idx_exec_draft  —— 一次草案可能对应多条执行尝试（重试/部分成交），
--                      按 draft_id 查是最常见的查询。
CREATE TABLE IF NOT EXISTS orders_execution (
    id BIGSERIAL PRIMARY KEY,
    draft_id bigint NOT NULL,
    account_id bigint NOT NULL,
    mode text NOT NULL,
    state text NOT NULL,
    risk_verdict_id bigint,
    client_order_id text NOT NULL,
    channel_order_id text,
    qty numeric(28,10) NOT NULL,
    filled_qty numeric(28,10) DEFAULT 0 NOT NULL,
    avg_price numeric(28,10),
    raw_ack jsonb DEFAULT '{}'::jsonb NOT NULL,
    state_at timestamp with time zone DEFAULT now() NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CHECK (((filled_qty <= qty) OR (state = ANY (ARRAY['cancelled'::text, 'settled'::text])))),
    CHECK ((filled_qty >= (0)::numeric)),
    -- 2026-11-05 加 live_auto：自动交易产生的执行单需要能落到这张表。
    CHECK ((mode = ANY (ARRAY['paper'::text, 'live_whitelist'::text, 'live_manual'::text, 'live_auto'::text]))),
    CHECK ((qty > (0)::numeric)),
    CHECK ((state = ANY (ARRAY['draft'::text, 'risk_pending'::text, 'risk_passed'::text, 'risk_denied'::text, 'submitted'::text, 'partial'::text, 'filled'::text, 'rejected'::text, 'cancel_requested'::text, 'cancelled'::text, 'settled'::text])))
);

CREATE TABLE IF NOT EXISTS paper_orders (
    id BIGSERIAL PRIMARY KEY,
    task_id bigint,
    signal_id bigint,
    instrument_id bigint,
    -- account_id 让 paper 单能按账户统计（WE-9）。
    -- 没有它时 GetEquityCurve(task_id) 会把多账户混算，胜率指标失去意义。
    -- 老数据为 NULL（加列前的历史），查询时「不传 account_id 统计全部含 NULL，
    -- 传了只统计匹配的」—— 两者都不能简单包含或排除。
    account_id bigint REFERENCES exec_accounts(id) ON DELETE RESTRICT,
    mode text NOT NULL,
    side text NOT NULL,
    qty numeric(28,10) NOT NULL,
    price numeric(28,10),
    filled_qty numeric(28,10) DEFAULT 0 NOT NULL,
    avg_fill_price numeric(28,10),
    status text DEFAULT 'draft'::text NOT NULL,
    broker_order_id text,
    submitted_at timestamp with time zone,
    filled_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CHECK ((mode = ANY (ARRAY['paper'::text, 'live'::text]))),
    CHECK ((side = ANY (ARRAY['buy'::text, 'sell'::text]))),
    CHECK ((status = ANY (ARRAY['draft'::text, 'risk_passed'::text, 'submitted'::text, 'filled'::text, 'partial'::text, 'rejected'::text, 'cancelled'::text, 'settled'::text])))
);

-- 注意：文档 ARCH-W2-W5.md:386 的 DDL 漏了 idx_recon_acct，以本文件为准。
-- 「某账户最近的对账记录」是高频查询路径（和 paper_orders 的 account 索引同模式）。
CREATE TABLE IF NOT EXISTS exec_reconciliations (
    id BIGSERIAL PRIMARY KEY,
    execution_id bigint,
    account_id bigint NOT NULL,
    kind text NOT NULL,
    local_snapshot jsonb DEFAULT '{}'::jsonb NOT NULL,
    remote_snapshot jsonb DEFAULT '{}'::jsonb NOT NULL,
    diff jsonb DEFAULT '{}'::jsonb NOT NULL,
    resolved_at timestamp with time zone,
    resolved_by text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CHECK ((kind = ANY (ARRAY['state'::text, 'amount'::text, 'missing_local'::text, 'missing_remote'::text, 'query_failed'::text])))
);

-- 阶段 2：约束 / 索引 / 触发器
-- 放在所有 CREATE TABLE 之后：外键引用此时才保证目标表已存在。

ALTER TABLE instruments DROP CONSTRAINT IF EXISTS instruments_market_symbol_key;
ALTER TABLE instruments ADD CONSTRAINT instruments_market_symbol_key UNIQUE (market, symbol);
CREATE INDEX IF NOT EXISTS idx_instruments_market_active ON instruments USING btree (market, active);
CREATE INDEX IF NOT EXISTS idx_instruments_sector ON instruments USING btree (sector) WHERE (sector IS NOT NULL);
-- market_credentials 的唯一键从 (provider, account_label) 升级为 (provider, kind, account_label)。
--
-- # 为什么要加 kind
--
-- 同一个 provider 有两种用途完全不同的凭据：
--   kind='trade' → 下单通道（要 api_key/api_secret/passphrase）
--   kind='data'  → 行情数据源（OKX 公开行情免凭证，填 key 只为提高限频配额）
-- 两者是不同的密钥、不同的权限，混在一起会让「这个 key 能干什么」不明确。
--
-- # 为什么唯一键必须带上 kind
--
-- 用户对同一个 provider 的两个 kind 可能填相同的 account_label（都叫「主账户」）。
-- 不带 kind 的话，存第二条会撞 UNIQUE 约束。
--
-- ⚠️ 表在加列时是 0 条数据（2026-10-29 实测），所以 DEFAULT 'trade' 不碰任何行，
--    也不存在「老数据怎么归 kind」的问题。
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns
                   WHERE table_name='market_credentials' AND column_name='kind') THEN
        ALTER TABLE market_credentials ADD COLUMN kind TEXT NOT NULL DEFAULT 'trade';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname='ck_market_credentials_kind') THEN
        ALTER TABLE market_credentials ADD CONSTRAINT ck_market_credentials_kind
            CHECK (kind IN ('data','trade'));
    END IF;
END $$;
ALTER TABLE market_credentials DROP CONSTRAINT IF EXISTS market_credentials_provider_account_label_key;
ALTER TABLE market_credentials DROP CONSTRAINT IF EXISTS market_credentials_provider_kind_label_key;
ALTER TABLE market_credentials ADD CONSTRAINT market_credentials_provider_kind_label_key UNIQUE (provider, kind, account_label);
CREATE INDEX IF NOT EXISTS idx_market_credentials_provider ON market_credentials USING btree (provider);
DROP TRIGGER IF EXISTS trg_market_credentials_upd ON market_credentials;
CREATE TRIGGER trg_market_credentials_upd BEFORE UPDATE ON market_credentials FOR EACH ROW EXECUTE FUNCTION set_updated_at();
ALTER TABLE watchlists DROP CONSTRAINT IF EXISTS watchlists_name_key;
ALTER TABLE watchlists ADD CONSTRAINT watchlists_name_key UNIQUE (name);
ALTER TABLE bars DROP CONSTRAINT IF EXISTS bars_instrument_id_interval_ts_key;
ALTER TABLE bars ADD CONSTRAINT bars_instrument_id_interval_ts_key UNIQUE (instrument_id, "interval", ts);
ALTER TABLE bars DROP CONSTRAINT IF EXISTS bars_instrument_id_fkey;
ALTER TABLE bars ADD CONSTRAINT bars_instrument_id_fkey FOREIGN KEY (instrument_id) REFERENCES instruments(id) ON DELETE CASCADE;
ALTER TABLE bars DROP CONSTRAINT IF EXISTS bars_snapshot_id_fkey;
ALTER TABLE bars ADD CONSTRAINT bars_snapshot_id_fkey FOREIGN KEY (snapshot_id) REFERENCES snapshots(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_bars_partial ON bars USING btree (instrument_id) WHERE is_partial;
CREATE INDEX IF NOT EXISTS idx_bars_slice ON bars USING btree (instrument_id, "interval", trading_date DESC);
CREATE INDEX IF NOT EXISTS idx_bars_snapshot ON bars USING btree (snapshot_id) WHERE (snapshot_id IS NOT NULL);
CREATE INDEX IF NOT EXISTS idx_bars_ts ON bars USING btree (instrument_id, "interval", ts DESC);
ALTER TABLE instrument_relations DROP CONSTRAINT IF EXISTS instrument_relations_instrument_id_fkey;
ALTER TABLE instrument_relations ADD CONSTRAINT instrument_relations_instrument_id_fkey FOREIGN KEY (instrument_id) REFERENCES instruments(id) ON DELETE CASCADE;
CREATE INDEX IF NOT EXISTS idx_instrument_relations_instrument ON instrument_relations USING btree (instrument_id, related_type);
CREATE UNIQUE INDEX IF NOT EXISTS uq_instrument_relations_edge ON instrument_relations USING btree (instrument_id, related_type, related_ref);
CREATE INDEX IF NOT EXISTS idx_snapshots_collected ON snapshots USING btree (collected_at DESC);
CREATE INDEX IF NOT EXISTS idx_snapshots_coverage ON snapshots USING btree (source, endpoint, coverage_end DESC) WHERE (coverage_end IS NOT NULL);
CREATE UNIQUE INDEX IF NOT EXISTS uq_snapshots_dedup ON snapshots USING btree (source, endpoint, params_hash, date_trunc('second'::text, (collected_at AT TIME ZONE 'UTC'::text)));
ALTER TABLE signals DROP CONSTRAINT IF EXISTS signals_instrument_id_fkey;
ALTER TABLE signals ADD CONSTRAINT signals_instrument_id_fkey FOREIGN KEY (instrument_id) REFERENCES instruments(id);
CREATE INDEX IF NOT EXISTS idx_signals_instrument ON signals USING btree (instrument_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_signals_status ON signals USING btree (status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_signals_task ON signals USING btree (task_id) WHERE (task_id IS NOT NULL);
ALTER TABLE signal_settlements DROP CONSTRAINT IF EXISTS signal_settlements_signal_id_settle_date_key;
ALTER TABLE signal_settlements ADD CONSTRAINT signal_settlements_signal_id_settle_date_key UNIQUE (signal_id, settle_date);
ALTER TABLE signal_settlements DROP CONSTRAINT IF EXISTS signal_settlements_signal_id_fkey;
ALTER TABLE signal_settlements ADD CONSTRAINT signal_settlements_signal_id_fkey FOREIGN KEY (signal_id) REFERENCES signals(id) ON DELETE CASCADE;
CREATE INDEX IF NOT EXISTS idx_signal_settlements_date ON signal_settlements USING btree (settle_date);
ALTER TABLE ratings DROP CONSTRAINT IF EXISTS ratings_instrument_id_fkey;
ALTER TABLE ratings ADD CONSTRAINT ratings_instrument_id_fkey FOREIGN KEY (instrument_id) REFERENCES instruments(id);
CREATE INDEX IF NOT EXISTS idx_ratings_instrument ON ratings USING btree (instrument_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_ratings_task ON ratings USING btree (task_id) WHERE (task_id IS NOT NULL);
-- 按 intent 查「它产出的评级」—— D 方案（PM intent 结束时建草案）的热路径。
CREATE INDEX IF NOT EXISTS idx_ratings_intent ON ratings USING btree (intent_id) WHERE (intent_id IS NOT NULL);
ALTER TABLE watchlist_items DROP CONSTRAINT IF EXISTS watchlist_items_watchlist_id_instrument_id_key;
ALTER TABLE watchlist_items ADD CONSTRAINT watchlist_items_watchlist_id_instrument_id_key UNIQUE (watchlist_id, instrument_id);
ALTER TABLE watchlist_items DROP CONSTRAINT IF EXISTS watchlist_items_instrument_id_fkey;
ALTER TABLE watchlist_items ADD CONSTRAINT watchlist_items_instrument_id_fkey FOREIGN KEY (instrument_id) REFERENCES instruments(id) ON DELETE CASCADE;
ALTER TABLE watchlist_items DROP CONSTRAINT IF EXISTS watchlist_items_watchlist_id_fkey;
ALTER TABLE watchlist_items ADD CONSTRAINT watchlist_items_watchlist_id_fkey FOREIGN KEY (watchlist_id) REFERENCES watchlists(id) ON DELETE CASCADE;
CREATE INDEX IF NOT EXISTS idx_watchlist_items_instrument ON watchlist_items USING btree (instrument_id);
ALTER TABLE exec_accounts DROP CONSTRAINT IF EXISTS exec_accounts_channel_channel_account_id_key;
ALTER TABLE exec_accounts ADD CONSTRAINT exec_accounts_channel_channel_account_id_key UNIQUE (channel, channel_account_id);
CREATE INDEX IF NOT EXISTS idx_exec_accounts_blocked ON exec_accounts USING btree (blocked_at) WHERE (blocked_at IS NOT NULL);
CREATE UNIQUE INDEX IF NOT EXISTS uq_auth_active ON exec_authorizations USING btree (channel, account_ref) WHERE (revoked_at IS NULL);
ALTER TABLE execution_limits DROP CONSTRAINT IF EXISTS execution_limits_key_key;
ALTER TABLE execution_limits ADD CONSTRAINT execution_limits_key_key UNIQUE (key);
ALTER TABLE orders_draft DROP CONSTRAINT IF EXISTS orders_draft_instrument_id_fkey;
ALTER TABLE orders_draft ADD CONSTRAINT orders_draft_instrument_id_fkey FOREIGN KEY (instrument_id) REFERENCES instruments(id);
ALTER TABLE orders_draft DROP CONSTRAINT IF EXISTS orders_draft_signal_id_fkey;
ALTER TABLE orders_draft ADD CONSTRAINT orders_draft_signal_id_fkey FOREIGN KEY (signal_id) REFERENCES signals(id);
CREATE INDEX IF NOT EXISTS idx_orders_draft_pending ON orders_draft USING btree (created_at DESC) WHERE (exported_at IS NULL);
-- 一条评级最多生成一个草案（幂等的唯一判据）。
-- 部分索引：rating_id IS NULL 的历史数据不受约束（它们没有评级来源）。
CREATE UNIQUE INDEX IF NOT EXISTS uq_orders_draft_rating ON orders_draft (rating_id) WHERE rating_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_orders_draft_signal ON orders_draft USING btree (signal_id);
ALTER TABLE draft_risk_verdicts DROP CONSTRAINT IF EXISTS draft_risk_verdicts_draft_id_fkey;
ALTER TABLE draft_risk_verdicts ADD CONSTRAINT draft_risk_verdicts_draft_id_fkey FOREIGN KEY (draft_id) REFERENCES orders_draft(id) ON DELETE CASCADE;
CREATE INDEX IF NOT EXISTS idx_verdicts_draft ON draft_risk_verdicts USING btree (draft_id, evaluated_at DESC);
ALTER TABLE orders_execution DROP CONSTRAINT IF EXISTS orders_execution_client_order_id_key;
ALTER TABLE orders_execution ADD CONSTRAINT orders_execution_client_order_id_key UNIQUE (client_order_id);
ALTER TABLE orders_execution DROP CONSTRAINT IF EXISTS orders_execution_account_id_fkey;
ALTER TABLE orders_execution ADD CONSTRAINT orders_execution_account_id_fkey FOREIGN KEY (account_id) REFERENCES exec_accounts(id) ON DELETE RESTRICT;
ALTER TABLE orders_execution DROP CONSTRAINT IF EXISTS orders_execution_draft_id_fkey;
ALTER TABLE orders_execution ADD CONSTRAINT orders_execution_draft_id_fkey FOREIGN KEY (draft_id) REFERENCES orders_draft(id) ON DELETE RESTRICT;
ALTER TABLE orders_execution DROP CONSTRAINT IF EXISTS orders_execution_risk_verdict_id_fkey;
ALTER TABLE orders_execution ADD CONSTRAINT orders_execution_risk_verdict_id_fkey FOREIGN KEY (risk_verdict_id) REFERENCES draft_risk_verdicts(id);
CREATE INDEX IF NOT EXISTS idx_exec_acct ON orders_execution USING btree (account_id, state_at DESC);
CREATE INDEX IF NOT EXISTS idx_exec_draft ON orders_execution USING btree (draft_id);
CREATE INDEX IF NOT EXISTS idx_exec_state ON orders_execution USING btree (state, state_at);
ALTER TABLE paper_orders DROP CONSTRAINT IF EXISTS paper_orders_instrument_id_fkey;
ALTER TABLE paper_orders ADD CONSTRAINT paper_orders_instrument_id_fkey FOREIGN KEY (instrument_id) REFERENCES instruments(id);
ALTER TABLE paper_orders DROP CONSTRAINT IF EXISTS paper_orders_signal_id_fkey;
ALTER TABLE paper_orders ADD CONSTRAINT paper_orders_signal_id_fkey FOREIGN KEY (signal_id) REFERENCES signals(id);
CREATE INDEX IF NOT EXISTS idx_paper_orders_instrument ON paper_orders USING btree (instrument_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_paper_orders_status ON paper_orders USING btree (status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_paper_orders_task ON paper_orders USING btree (task_id) WHERE (task_id IS NOT NULL);
CREATE UNIQUE INDEX IF NOT EXISTS uq_paper_orders_broker ON paper_orders USING btree (broker_order_id) WHERE (broker_order_id IS NOT NULL);
ALTER TABLE exec_reconciliations DROP CONSTRAINT IF EXISTS exec_reconciliations_account_id_fkey;
ALTER TABLE exec_reconciliations ADD CONSTRAINT exec_reconciliations_account_id_fkey FOREIGN KEY (account_id) REFERENCES exec_accounts(id);
ALTER TABLE exec_reconciliations DROP CONSTRAINT IF EXISTS exec_reconciliations_execution_id_fkey;
ALTER TABLE exec_reconciliations ADD CONSTRAINT exec_reconciliations_execution_id_fkey FOREIGN KEY (execution_id) REFERENCES orders_execution(id) ON DELETE RESTRICT;
CREATE INDEX IF NOT EXISTS idx_recon_acct ON exec_reconciliations USING btree (account_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_recon_open ON exec_reconciliations USING btree (created_at DESC) WHERE (resolved_at IS NULL);

-- paper_orders.account_id：WE-9 加的列（让 paper 单能按账户统计）。
--
-- 必须同时有 CREATE TABLE 里的定义（新库）和这里的 ALTER（老库）：
--   新库靠 CREATE TABLE 建出这一列；
--   老库的表已存在，CREATE TABLE IF NOT EXISTS 是 no-op，只能靠 ALTER。
--
-- 这是「schema 同时承担建库和迁移两个职责」的必然结果 ——
-- 两处都要写，缺一处就有一类环境不对。
ALTER TABLE paper_orders ADD COLUMN IF NOT EXISTS account_id BIGINT REFERENCES exec_accounts(id) ON DELETE RESTRICT;
-- 索引必须在 ALTER 之后：老库在这一句之前还没有 account_id 列。
CREATE INDEX IF NOT EXISTS idx_paper_orders_account ON paper_orders USING btree (account_id, created_at DESC);

-- tasks.instrument_id 必须放在本文件末尾：
-- 它引用 instruments，而 instruments 在「金融模块」段里创建。
-- 放在原 tasks ALTER 段（文件中部）会在全新库上报
-- `relation "instruments" does not exist`。
--
-- 这是「两阶段」之外的一个特例：跨段的表引用只能放在所有 CREATE 之后。
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS instrument_id BIGINT REFERENCES instruments(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_tasks_instrument ON tasks(instrument_id) WHERE instrument_id IS NOT NULL;

-- ===========================================================================
-- 执行层审计（WE-12）
-- ===========================================================================
--
-- # 为什么执行层审计不能复用 activity 表
--
-- 三个原因，任一条都足以否决：
--
--   ① activity.exploration_id 是 NOT NULL —— 执行层事件不属于任何任务。
--      强行塞进「某个任务」会污染那个任务的执行历史
--      （用户查任务详情时会看到「模式切换」这种与任务无关的行）。
--
--   ② activity 有 ON DELETE CASCADE —— 任务删除时活动记录一起删。
--      但执行审计是**合规记录**：「谁在什么时候授权了实盘、谁冻结了账户」
--      必须永久保留，不能因为删任务而消失。
--
--   ③ 语义不同：activity 是「任务执行过程」，exec_audit 是「系统状态变更」。
--      查询模式也不同（前者按 exploration_id，后者按时间 + 事件类型）。
--
-- # 与文档的关系
--
-- ARCH-W2-W5.md 的 §3.3 / §3.5 原写「INSERT activity」——
-- 那是文档写作时的假设，但 activity 的表结构不支持它
-- （NOT NULL + CASCADE 是后加的）。文档已同步更正为 exec_audit。
CREATE TABLE IF NOT EXISTS exec_audit (
    id          BIGSERIAL PRIMARY KEY,
    action      TEXT NOT NULL,
    detail      JSONB NOT NULL DEFAULT '{}'::jsonb,
    actor       TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT exec_audit_action_check CHECK (action IN (
        'exec_mode_change', 'exec_authorize', 'exec_unfreeze',
        'exec_frozen', 'exec_limits_change'
    ))
);

-- 按时间倒序查最近事件（运维面板）
CREATE INDEX IF NOT EXISTS idx_exec_audit_time ON exec_audit(created_at DESC);
-- 按事件类型查（「最近 10 次授权」）
CREATE INDEX IF NOT EXISTS idx_exec_audit_action ON exec_audit(action, created_at DESC);
