# -*- coding: utf-8 -*-
"""从 PG schema 模型生成 SQLite DDL。

设计取舍（对应 ARCH-SQLite-Migration.md）：
- 主键 BIGSERIAL -> INTEGER PRIMARY KEY AUTOINCREMENT（id 进归档/边表，禁止复用）
- TIMESTAMPTZ -> TEXT（ISO8601 UTC 毫秒，strftime 产）
- JSONB / TEXT[] / BIGINT[] / JSONB[] -> TEXT（JSON 文本）
- CIDR/INET -> TEXT（包含判断走 Go UDF）
- NUMERIC -> TEXT（Go 侧 string 承载）
- 表级 CHECK 里的 ANY(ARRAY[...]) -> IN (...)
- 约束名保留：命名 CHECK 会出现在报错 message 里，便于精确判定
"""
import io
import re
import sys

sys.path.insert(0, r'C:/Users/Administrator/WorkBuddy/亏钱/ARTEX/tools_schema')
import gen_sqlite_schema as g

OUT = r'C:/Users/Administrator/WorkBuddy/亏钱/ARTEX/db/schema_sqlite.sql'

SQL_NOW = "strftime('%Y-%m-%dT%H:%M:%fZ','now')"

# 只在 PG 里是「无意义自增」的表：这些表的 id 不出现在其他表的列、不进归档/API DTO，
# 因此用 INTEGER PRIMARY KEY（不带 AUTOINCREMENT）省一张 sqlite_sequence 的开销。
NO_AUTOINCREMENT = {
    'activity', 'server_logs', 'skill_usage', 'tool_usage',
    'snapshots', 'conversation_activities', 'exec_audit', 'mcp_tools_cache',
}


# DECIMAL_COMPARE_COLS 列出「CHECK 里做数值比较、但列类型是 TEXT」的列。
#
# # 为什么需要显式白名单而不是自动推断
#
# 生成器在建 CHECK 时只看得到当前列的声明，看不到同表其它列的类型；
# 而 `filled_qty <= qty` 这种跨列比较必须知道**两边都是 decimal**。
# 自动推断要把整个表的列类型都传进来，改动面大、易错。
# schema 里这类 CHECK 只有 orders_execution 的 3 条，白名单更清晰、更好审计。
#
# 新增 decimal 列的比较型 CHECK 时，把列名加到这里。
DECIMAL_COMPARE_COLS = {
    'orders_execution': ('qty', 'filled_qty', 'avg_price'),
}

def conv_check_expr(expr, table):
    """把 PG 的 CHECK 表达式转成 SQLite 能跑的形式。

    主要变换：
      x = ANY (ARRAY['a'::text, 'b'::text])  ->  x IN ('a','b')
      col::text                              ->  col
      '...'::jsonb / ::bigint 等             ->  去掉转型
      lower(x) / trim(x) / length(x)         ->  SQLite 内置，原样
    """
    e = expr
    # ANY(ARRAY[...]) -> IN (...)
    def repl_any(m):
        inner = m.group(1)
        items = [s.strip() for s in split_items(inner)]
        out = []
        for it in items:
            it = re.sub(r'::\w+(\[\])?', '', it).strip()
            out.append(it)
        return ' IN (' + ', '.join(out) + ')'
    e = re.sub(r'\s*=?\s*ANY\s*\(\s*ARRAY\s*\[([^\]]*)\]\s*\)', lambda m: ' IN (' +
               ', '.join(re.sub(r'::\w+(\[\])?', '', x).strip()
                         for x in split_items(m.group(1))) + ')', e)
    # jsonb 数组包含 `x <@ ARRAY['a','b']` —— SQLite 的 CHECK 禁止子查询，
    # 所以不能用 json_each。列里存的是 JSON 数组文本，用 LIKE 逐项判断：
    # 要求每个元素"出现"（不是严格子集，但 schema 里这条是白名单校验，
    # 严格性由应用层写入时保证；此处只挡住明显越界值）。
    # 数组「被包含于」：`col <@ ARRAY['a','b']`（PG：col 是右集合的子集）
    #
    # ⚠️ 语义陷阱（实测，2026-11-08）：这是**子集**判断，不是「至少含一个」。
    #
    #     {}            <@ ARRAY[us,hk]  ->  true   （空集是任何集合的子集）
    #     [us]          <@ ARRAY[us,hk]  ->  true
    #     [us,bogus]    <@ ARRAY[us,hk]  ->  false  （bogus 不在白名单）
    #
    # 早先误写成 `col LIKE '%"us"%' OR col LIKE '%"hk"%'`（= 至少含一个），
    # 后果是 exec_accounts.market_support 的 DEFAULT '{}' 反而违反自身 CHECK，
    # 任何不显式给 market_support 的插入都失败 —— 整张表不可用。
    #
    # # 为什么这里只做「白名单内」而不再追求严格子集
    #
    # SQLite 的 CHECK **禁止子查询**，无法用 json_each 逐元素比对，纯 SQL 里
    # 做不到严格的子集判断（试过 replace 链方案：表达式会膨胀到几百字符、
    # 且容易把行尾的逗号吃掉导致 DDL 语法错误，已被否决）。
    #
    # 现在的做法：把每个合法元素从字符串里删掉，然后检查**剩余的引号值**
    # 是否为空 —— 等价于「所有出现的元素都在白名单里」。
    # 空值 {} / [] 天然通过（删完为空）。
    #
    # 已知残留差异：`[us,us]`（重复元素）PG 的子集判断为 true，这里也为 true；
    # 但 `[]` 在 PG 是 true、这里也是 true，符合预期。
    # 「必须非空」这条业务规则由 db/exec_accounts.go 的应用层校验负责
    # （schema 层只管'不越界'）。
    def repl_subset(m):
        col = m.group(1)
        items = [re.sub(r'::\w+(\[\])?', '', x).strip().strip("'").strip('"')
                 for x in split_items(m.group(2))]
        # 逐项删除 "item"，再用正则式判断剩余是否还有 "..." 形态的残片。
        expr = col
        for it in items:
            expr = expr.replace(col, col)  # no-op，占位保持可读
            expr = "replace(%s,'\"%s\"','')" % (expr, it)
        # 剩余里若还含 `"` 说明有白名单外的元素
        return "%s NOT LIKE '%%\"%%'" % expr
    e = re.sub(r'(\w+)\s*<@\s*ARRAY\s*\[([^\]]*)\]', repl_subset, e)

    # jsonb 键存在操作符 `x ? 'key'` -> json_type(x,'$.key') IS NOT NULL
    # `?` 在 SQLite 里是占位符，必须换掉。
    e = re.sub(
        r"(\w+)\s*\?\s*'([A-Za-z_][A-Za-z0-9_]*)'",
        lambda m: "json_type(%s,'$.%s') IS NOT NULL" % (m.group(1), m.group(2)),
        e)
    # 裸类型转型
    e = re.sub(r'::\w+(\[\])?', '', e)
    # PG 的 POSIX 正则操作符 ~ / ~* / !~ / !~*
    # CHECK 在 SQLite 里不能调用 Go UDF（UDF 是连接级注册，建表期不可用），
    # 所以这里只处理 schema 里唯一的一处 `key ~ '^[a-z][a-z0-9_]*$'`，
    # 用 GLOB 精确等价表达：首字符 a-z，其后仅 a-z0-9_。
    m = re.search(r"\(?\s*(\w+)\s*~\s*'\^\[a-z\]\[a-z0-9_\]\*\$'\s*\)?", e)
    if m:
        col = m.group(1)
        e = ("(%s GLOB '[a-z]*' AND %s NOT GLOB '*[^a-z0-9_]*')" % (col, col))
        return e
    # ---- numeric 列的大小比较必须 CAST ----
    #
    # # 为什么（2026-11-08 实测，P0）
    #
    # PG 的 numeric 列在 SQLite 里是 TEXT（conv_type 返回 ('TEXT','decimal')）。
    # TEXT 的比较是**字典序**，不是数值：
    #
    #     '3' <= '10'   → 0   （字符串比较：'3' > '1'）
    #     CAST('3' AS REAL) <= CAST('10' AS REAL)  → 1
    #
    # 后果实测（orders_execution 的 CHECK）：
    #
    #     qty=10, filled_qty=3   → constraint failed   ← 正常部分成交被拒
    #     qty=100, filled_qty=9  → constraint failed   ← 正常部分成交被拒
    #
    # 这会让实盘执行链路完全不可用。所以凡是 CHECK 里对 decimal 列做
    # < > <= >= 比较，一律包 CAST(... AS REAL)。
    #
    # 用 REAL 而不是 NUMERIC：实测 `CAST('0.5' AS NUMERIC)` 在 SQLite 里
    # 返回整数 0（NUMERIC 亲和性截断小数），REAL 才对。
    for col in DECIMAL_COMPARE_COLS.get(table, ()):
        # 只包裸列名（前后不是标识符字符），生成过的 CAST 不会二次包装。
        # 不能加右括号 lookahead —— `filled_qty <= qty)` 里 qty 紧跟 `)`，会漏匹配。
        pat = r'(?<![A-Za-z0-9_])' + re.escape(col) + r'(?![A-Za-z0-9_])'
        e = re.sub(pat, 'CAST(%s AS REAL)' % col, e)
        e = e.replace('CAST(CAST(%s AS REAL) AS REAL)' % col, 'CAST(%s AS REAL)' % col)

    # PG 的 = ANY($1) 之类不在 schema 里，忽略
    return ' '.join(e.split())


def split_items(s):
    parts, depth, cur, in_str = [], 0, [], False
    for ch in s:
        if ch == "'":
            in_str = not in_str
        if not in_str:
            if ch == '(':
                depth += 1
            elif ch == ')':
                depth -= 1
            elif ch == ',' and depth == 0:
                parts.append(''.join(cur))
                cur = []
                continue
        cur.append(ch)
    if ''.join(cur).strip():
        parts.append(''.join(cur))
    return parts


def conv_type(col):
    """PG 类型 -> SQLite 类型。返回 (sqlite_type, extra_hint)。"""
    t = col.type
    if t == 'bigserial':
        return 'INTEGER', 'pk'
    if t in ('bigint', 'integer', 'int', 'smallint'):
        return 'INTEGER', None
    if t in ('jsonb', 'jsonb[]', 'text[]', 'bigint[]'):
        return 'TEXT', 'json'
    if t in ('timestamptz', 'timestamp with time zone'):
        # TIMESTAMP 而不是 TEXT —— 这是**必须的**，不是风格选择。
        #
        # modernc.org/sqlite 的 rows.go:189-203 只在
        # ColumnTypeDatabaseTypeName() 精确等于 "DATE"/"DATETIME"/"TIMESTAMP"
        # 时把 TEXT 值交给 parseTime 转成 time.Time。声明成 TEXT 时
        # Scan 到 *time.Time 会直接报：
        #   sql: Scan error ... unsupported Scan, storing driver.Value
        #   type string into type *time.Time
        #
        # 列上声明的类型只影响**亲和性**与 scan 转换，**不影响物理存储**：
        # 我们写入的仍是 ISO8601 串，typeof() 仍是 text，字典序仍是时间序。
        #
        # ⚠️ 亲和性陷阱（实测）：TIMESTAMP/DATETIME/DATE 都是 NUMERIC 亲和性，
        # 往这些列写纯数字（如 1700000000）会被静默转成 INTEGER，
        # 之后 time.Time scan 读出 "type int64" 错误、字典序比较失效。
        # 已核查代码库无此写法（见 docs/quantex/ARCH-SQLite-Migration.md §2.2）。
        # 新增写入路径必须走 NowISO()/FormatISO()，不要绑 epoch 数字。
        return 'TIMESTAMP', 'time'
    if t == 'date':
        return 'TEXT', 'date'
    if t == 'boolean':
        return 'INTEGER', 'bool'
    if t == 'cidr' or t == 'inet':
        return 'TEXT', 'net'
    if t.startswith('numeric'):
        return 'TEXT', 'decimal'
    if t in ('text', 'varchar(255)', 'varchar(64)'):
        return 'TEXT', None
    if t in ('real', 'double precision'):
        return 'REAL', None
    raise SystemExit('unmapped type %r (col %s)' % (t, col.name))


def conv_default(col, hint, table):
    d = col.default
    if d is None:
        return None
    d = d.strip()
    low = d.lower()
    # now()
    if re.fullmatch(r'now\(\)', low):
        return '(%s)' % SQL_NOW
    # 数组 / jsonb 默认
    if d == "'{}'":
        # 数组默认空 -> '[]'；jsonb 默认空对象 -> '{}'
        if col.type.endswith('[]'):
            return "'[]'"
        return "'{}'"
    if d == "'[]'::jsonb" or d == "'[]'":
        return "'[]'"
    if d == "'{}'::jsonb":
        return "'{}'"
    # 去 ::type
    dd = re.sub(r"::\w+(\[\])?", '', d).strip()
    if dd.lower() == 'true':
        return '1'
    if dd.lower() == 'false':
        return '0'
    return dd


# 需要加引号的标识符（SQLite 保留字或与内置名冲突）
QUOTED_IDENTS = {'interval', 'index', 'key', 'value', 'schema', 'order', 'group',
                 'where', 'from', 'table', 'primary', 'check', 'unique', 'default'}


def q(name):
    """按需给标识符加双引号。"""
    return '"%s"' % name if name.lower() in QUOTED_IDENTS else name


def conv_col_line(col, table, has_other_pk=False):
    sqlite_type, hint = conv_type(col)
    name = q(col.name)
    parts = [name, sqlite_type]

    if hint == 'pk':
        # bigserial 默认做自增主键；但若该表另有单列 PRIMARY KEY
        # （如 side_question_requests.id TEXT PRIMARY KEY + ordinal BIGSERIAL），
        # 就不能再当主键 —— SQLite 只允许一个，会报
        # "table X has more than one primary key"。
        if has_other_pk:
            parts.append('NOT NULL')
            if col.not_null or True:
                pass
        else:
            parts.append('PRIMARY KEY')
            if table not in NO_AUTOINCREMENT:
                parts.append('AUTOINCREMENT')
            parts.append('NOT NULL')
    else:
        if col.not_null:
            parts.append('NOT NULL')
        d = conv_default(col, hint, table)
        if d is not None:
            parts.append('DEFAULT ' + d)
        if col.references:
            rt, rc, tail = col.references
            fk = 'REFERENCES %s(%s)' % (rt, q(rc))
            if 'ON DELETE' in tail.upper():
                m = re.search(r'ON\s+DELETE\s+(CASCADE|SET\s+NULL|RESTRICT|NO\s+ACTION)', tail, re.I)
                if m:
                    fk += ' ON DELETE ' + ' '.join(m.group(1).upper().split())
            parts.append(fk)
        # 非 bigserial 的单列主键（如 settings.key）
        if col.primary_key:
            parts.append('PRIMARY KEY')
        if col.unique:
            parts.append('UNIQUE')
        # JSON 列必须有 json_valid 校验 —— 这是 PG JSONB 类型自带的能力，SQLite 的 TEXT 没有。
        # 不加校验 = 静默丢掉一层数据完整性保护：'{' 这种非法 JSON 会被完整写入，
        # 下游 json_extract 读到时返回 NULL 或报错，且很难定位来源。
        # 见 docs/quantex/ARCH-SQLite-Migration.md §2.1。
        #
        # 注意 NULL 语义：json_valid(NULL) 返回 NULL（不是 0），CHECK 对 NULL 结果视为通过，
        # 所以可空 JSON 列留 NULL 仍然合法 —— 与 PG 的 jsonb NULL 行为一致。
        if hint == 'json':
            parts.append('CHECK (json_valid(%s))' % name)
        if col.check:
            parts.append('CHECK ' + conv_check_expr(col.check, table))
    return '    ' + ' '.join(parts)


def conv_table_constraint(c, table):
    s = ' '.join(c.split())
    # CHECK (...) 里的表达式要转换
    m = re.match(r'(CONSTRAINT\s+(\w+)\s+)?CHECK\s*(\(.*\))$', s, re.I | re.S)
    if m:
        prefix = ('CONSTRAINT %s ' % m.group(2)) if m.group(2) else ''
        return '    ' + prefix + 'CHECK ' + conv_check_expr(m.group(3), table)
    # UNIQUE (cols) / UNIQUE (cols) 里的 ::text
    s = re.sub(r'::\w+(\[\])?', '', s)
    return '    ' + s


def build_create(t, order_hint):
    cols = list(t.columns)
    # 表里是否已有「非 bigserial 的」单列主键（决定 bigserial 还能不能当主键）
    named_pk = [c.name for c in cols if c.primary_key and c.type != 'bigserial']
    serial_pk = [c.name for c in cols if c.type == 'bigserial']
    # 表级 PRIMARY KEY (...) 也算"另有主键"
    table_pk = any(re.match(r'PRIMARY\s+KEY', c, re.I) for c in t.constraints)
    has_other_pk = bool(named_pk) or table_pk
    # 同一张表里出现多个 bigserial 时，只有第一个做自增主键
    serial_seen = 0

    lines = []
    for c in cols:
        other = has_other_pk
        if c.type == 'bigserial':
            serial_seen += 1
            if serial_seen > 1:
                other = True
        lines.append(conv_col_line(c, t.name, other))
    for c in t.constraints:
        lines.append(conv_table_constraint(c, t.name))
    body = ',\n'.join(lines)
    return 'CREATE TABLE IF NOT EXISTS %s (\n%s\n);' % (t.name, body)


def gen():
    tables, order, stmts, stats = g.parse()

    out = []
    out.append('-- ARTEX SQLite schema（由 tools_schema/gen_sqlite_schema.py 生成，勿手改）')
    out.append('-- 生成源：db/schema.sql（PostgreSQL 版）。改结构请改 schema.sql 后重跑生成器。')
    out.append('--')
    out.append('-- 关键约定（详见 docs/quantex/ARCH-SQLite-Migration.md）：')
    out.append('--   * 时间列 TEXT，格式 %s（UTC 毫秒，字典序=时间序）' % SQL_NOW)
    out.append('--   * 数组/JSONB 列 TEXT，内容是 JSON（数组是 []，对象是 {}）')
    out.append('--   * 金额 NUMERIC 列 TEXT，Go 侧 big.Rat 精确运算')
    out.append('--   * CIDR/INET 列 TEXT，包含判断走 Go UDF（cidr_contains）')
    out.append('--   * 禁止跳号的 $N 占位符；禁止 ?, 一律 $N')
    out.append('')
    out.append('PRAGMA foreign_keys = ON;')
    out.append('')

    # 阶段 1：建表
    out.append('-- ============================================================')
    out.append('-- 阶段 1：建表')
    out.append('-- ============================================================')
    for name in order:
        out.append('')
        out.append(build_create(tables[name], order))
    out.append('')

    # 阶段 2：索引
    out.append('-- ============================================================')
    out.append('-- 阶段 2：索引')
    out.append('--')
    out.append('-- 删掉了 PG 专用的 GIN/GIST 索引（SQLite 无等价物）：')
    out.append('--   idx_av2_task_ids / idx_av2_c_segment / idx_av2_technologies /')
    out.append('--   idx_av2_bound_domains / idx_av2_open_ports / idx_sv2_net /')
    out.append('--   idx_task_archives_sources / idx_ts_net / idx_findings_asset_ids')
    out.append('-- 这些列在 SQLite 里是 JSON 文本 / TEXT，靠 json_each 与 UDF 查询，无法索引。')
    out.append('-- 数据量上去后（> 5 万 assets）再考虑生成列 + 普通索引。')
    out.append('-- ============================================================')
    idx_sql, dropped = build_indexes(stmts)
    out.append('')
    out.extend(idx_sql)
    out.append('')

    # 阶段 3：触发器
    import gen_triggers
    out.extend(gen_triggers.gen_trigger_sql())
    out.append('')
    return '\n'.join(out), tables, order, stmts, stats, dropped


INDEX_RE = re.compile(
    r'CREATE (UNIQUE )?INDEX IF NOT EXISTS (\w+)\s+ON\s+(\w+)\s*(.*?)\s*;\s*$', re.S)


def is_pg_only_index(stmt):
    up = stmt.upper()
    return 'USING GIN' in up or 'USING GIST' in up


def conv_index_expr(expr):
    """索引列表达式转换。"""
    e = expr
    # 0) 先去所有 ::type（否则 'second'::text 会挡住后面的 date_trunc 匹配）
    e = re.sub(r'::\w+(\[\])?', '', e)
    # 1) 去 `x AT TIME ZONE 'UTC'`（含可能的外层括号）-> x
    e = re.sub(r"\(\s*([\w.]+)\s+AT\s+TIME\s+ZONE\s+'UTC'\s*\)", r"\1", e, flags=re.I)
    e = re.sub(r"([\w.]+)\s+AT\s+TIME\s+ZONE\s+'UTC'", r"\1", e, flags=re.I)
    # 2) date_trunc('sec|day', X) -> substr(X,1,19|10)
    e = re.sub(r"date_trunc\s*\(\s*'second'\s*,\s*", "date_trunc_second(", e, flags=re.I)
    e = re.sub(r"date_trunc\s*\(\s*'day'\s*,\s*", "date_trunc_day(", e, flags=re.I)
    e = _rewrite_trunc(e, 'date_trunc_second', 19)
    e = _rewrite_trunc(e, 'date_trunc_day', 10)
    e = re.sub(r'^btree\s*', '', e, flags=re.I)
    return ' '.join(e.split())


def _rewrite_trunc(e, fname, n):
    """把 fname(X) 改写成 substr(X,1,n)，正确配对括号。"""
    while True:
        i = e.find(fname + '(')
        if i < 0:
            return e
        j = i + len(fname)
        depth, k = 0, j
        while k < len(e):
            if e[k] == '(':
                depth += 1
            elif e[k] == ')':
                depth -= 1
                if depth == 0:
                    break
            k += 1
        inner = e[j + 1:k]
        e = e[:i] + 'substr(%s,1,%d)' % (inner.strip(), n) + e[k + 1:]


def build_indexes(stmts):
    out, dropped = [], []
    for st in stmts:
        s = st.strip()
        m = INDEX_RE.match(s)
        if not m:
            continue
        if is_pg_only_index(s):
            dropped.append(m.group(2))
            continue
        unique = m.group(1) or ''
        name, table, rest = m.group(2), m.group(3), m.group(4).strip()

        if not rest.startswith('('):
            mm = re.match(r'USING\s+btree\s*(\(.*)$', rest, re.I | re.S)
            if mm:
                rest = mm.group(1)
        if not rest.startswith('('):
            raise SystemExit('unparsed index %s: %r' % (name, rest))

        depth, end = 0, -1
        for i, ch in enumerate(rest):
            if ch == '(':
                depth += 1
            elif ch == ')':
                depth -= 1
                if depth == 0:
                    end = i
                    break
        cols_raw = rest[1:end]
        tail = rest[end + 1:].strip()

        cols = ', '.join(conv_index_expr(c) for c in split_items(cols_raw) if c.strip())
        where = ''
        wm = re.match(r'WHERE\s+(.+)$', tail, re.I | re.S)
        if wm:
            where = ' WHERE ' + conv_index_expr(wm.group(1))
        out.append('CREATE %sINDEX IF NOT EXISTS %s ON %s(%s)%s;' % (
            unique, name, table, cols, where))
    return out, dropped


if __name__ == '__main__':
    text, tables, order, stmts, stats, dropped = gen()
    io.open(OUT, 'w', encoding='utf-8', newline='\n').write(text)
    print('written', OUT)
    print('tables', len(order), 'dropped indexes', len(dropped))
    for d in dropped:
        print('  dropped:', d)
