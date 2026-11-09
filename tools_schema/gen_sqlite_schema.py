# -*- coding: utf-8 -*-
"""解析 PG 版 db/schema.sql 成结构模型，供生成 SQLite DDL 使用。

只做解析，不做输出。目的是让"最终列集合"成为可校验的 oracle：
生成出的 SQLite schema 实际建表后，与这里的模型逐表比对。
"""
import io
import json
import re
import sys

SRC = r'C:/Users/Administrator/WorkBuddy/亏钱/ARTEX/db/schema.sql'


def strip_comments(text):
    """去掉整行注释与行尾注释。

    行尾注释必须处理：schema.sql 里有 `state TEXT, -- 说明` 和
    `-- 说明\\n  trips INTEGER`（注释独占一行但下一行是同一条语句的续行）两种。
    危险的是单引号字符串里可能出现 `--`，所以按引号状态机扫描。
    """
    out = []
    for line in text.split('\n'):
        buf, i, in_str = [], 0, False
        while i < len(line):
            ch = line[i]
            if ch == "'":
                # PG 里 '' 是转义的单引号
                if in_str and i + 1 < len(line) and line[i + 1] == "'":
                    buf.append("''")
                    i += 2
                    continue
                in_str = not in_str
                buf.append(ch)
                i += 1
                continue
            if not in_str and ch == '-' and i + 1 < len(line) and line[i + 1] == '-':
                break
            buf.append(ch)
            i += 1
        cleaned = ''.join(buf).rstrip()
        if cleaned.strip():
            out.append(cleaned)
    return '\n'.join(out)


def split_statements(text):
    """按顶层分号切语句。DO $$ ... $$; 与 CREATE FUNCTION ... $$; 视为一个语句。"""
    stmts, buf = [], []
    depth_dollar = False
    for line in text.split('\n'):
        if '$$' in line:
            # 统计本行 $$ 出现次数
            n = line.count('$$')
            if n % 2 == 1:
                depth_dollar = not depth_dollar
        buf.append(line)
        if not depth_dollar and line.rstrip().endswith(';'):
            stmts.append('\n'.join(buf))
            buf = []
    if buf:
        rest = '\n'.join(buf).strip()
        if rest:
            stmts.append(rest)
    return stmts


def split_top_level(body):
    """按顶层逗号切分表体（忽略括号内的逗号）。"""
    parts, depth, cur = [], 0, []
    for ch in body:
        if ch == '(':
            depth += 1
        elif ch == ')':
            depth -= 1
        if ch == ',' and depth == 0:
            parts.append(''.join(cur))
            cur = []
        else:
            cur.append(ch)
    if ''.join(cur).strip():
        parts.append(''.join(cur))
    return [p.strip() for p in parts if p.strip()]


CREATE_RE = re.compile(
    r'CREATE TABLE IF NOT EXISTS (\w+)\s*\((.*)\)\s*;?\s*$', re.S)


class Column(object):
    def __init__(self, name, raw):
        self.name = name
        self.raw = raw            # 原始类型+约束文本
        self.type = ''
        self.not_null = False
        self.default = None
        self.check = None
        self.references = None
        self.primary_key = False
        self.unique = False
        self._parse()

    def _parse(self):
        raw = self.raw.strip()
        # 抓类型（取第一个"看起来像类型"的片段）
        m = re.match(
            r'^(BIGSERIAL|BIGINT|INTEGER|INT|SMALLINT|TEXT|VARCHAR\(\d+\)|BOOLEAN|'
            r'JSONB|TIMESTAMPTZ|timestamp with time zone|DATE|NUMERIC\(\d+,\d+\)|'
            r'CIDR|INET|REAL|DOUBLE PRECISION)'
            r'((?:\[\])?)',
            raw, re.I)
        if not m:
            raise SystemExit('cannot parse type: %r' % raw)
        self.type = (m.group(1) + (m.group(2) or '')).lower()
        rest = raw[m.end():]
        self.not_null = bool(re.search(r'\bNOT\s+NULL\b', rest, re.I))
        self.primary_key = bool(re.search(r'\bPRIMARY\s+KEY\b', rest, re.I))
        self.unique = bool(re.search(r'(?<!ON CONFLICT )\bUNIQUE\b', rest, re.I))
        dm = re.search(r'\bDEFAULT\s+(.+?)(?=\s+(?:NOT\s+NULL|CHECK|REFERENCES|UNIQUE|PRIMARY)|$)',
                       rest, re.I | re.S)
        if dm:
            self.default = dm.group(1).strip().rstrip(',')
        cm = re.search(r'\bCHECK\s*(\(.*)', rest, re.I | re.S)
        if cm:
            self.check = cm.group(1).strip().rstrip(',')
        rm = re.search(r'\bREFERENCES\s+(\w+)\s*\((\w+)\)(.*)$', rest, re.I | re.S)
        if rm:
            tail = rm.group(3).strip().rstrip(',')
            self.references = (rm.group(1), rm.group(2), tail)

    def __repr__(self):
        return 'Column(%s %s%s%s)' % (
            self.name, self.type,
            ' NOT NULL' if self.not_null else '',
            ' DEFAULT %s' % self.default if self.default else '')


class Table(object):
    def __init__(self, name):
        self.name = name
        self.columns = []        # list[Column]
        self.constraints = []    # list[str] 原样的表级约束
        self.dropped_columns = set()

    def col(self, name):
        for c in self.columns:
            if c.name == name:
                return c
        return None

    def add_column(self, name, raw):
        if self.col(name):
            return False
        self.columns.append(Column(name, raw))
        return True


ADDCOL_RE = re.compile(
    r'ALTER TABLE (\w+) ADD COLUMN IF NOT EXISTS (\w+)\s+(.*?);\s*$', re.S)
ADDCON_RE = re.compile(r'ALTER TABLE (\w+) ADD CONSTRAINT (\w+)\s+(.*?);\s*$', re.S)
DROPCON_RE = re.compile(r'ALTER TABLE (\w+) DROP CONSTRAINT IF EXISTS (\w+)\s*;', re.S)


def apply_alters(tables, stmts):
    """把 ADD COLUMN / ADD CONSTRAINT / DROP CONSTRAINT 并入模型。

    目标：模型 == 「全新库跑完整个 schema.sql 后的最终形态」。
    DROP 掉的约束直接从列表移除；ADD 的追加。
    返回统计信息，便于人工核对。
    """
    stats = {'addcol': 0, 'addcon': 0, 'dropcon': 0, 'skip': []}
    for st in stmts:
        s = st.strip()
        m = ADDCOL_RE.match(s)
        if m:
            name, col, raw = m.group(1), m.group(2), m.group(3)
            t = tables.get(name)
            if t is None:
                stats['skip'].append('ADD COLUMN on unknown table %s' % name)
                continue
            if t.add_column(col, raw):
                stats['addcol'] += 1
            continue
        m = ADDCON_RE.match(s)
        if m:
            name, cname, rest = m.group(1), m.group(2), m.group(3).strip()
            t = tables.get(name)
            if t is None:
                stats['skip'].append('ADD CONSTRAINT on unknown table %s' % name)
                continue
            t.constraints.append('CONSTRAINT %s %s' % (cname, rest))
            stats['addcon'] += 1
            continue
        m = DROPCON_RE.match(s)
        if m:
            name, cname = m.group(1), m.group(2)
            t = tables.get(name)
            if t is None:
                stats['skip'].append('DROP CONSTRAINT on unknown table %s' % name)
                continue
            before = len(t.constraints)
            t.constraints = [c for c in t.constraints
                             if not re.match(r'CONSTRAINT\s+%s\b' % re.escape(cname), c, re.I)]
            if len(t.constraints) == before:
                stats['skip'].append('DROP %s.%s (not inline)' % (name, cname))
            stats['dropcon'] += 1
            continue
    return stats


DO_BLOCK_COLUMNS = {
    # DO 块里用 information_schema 探测后 ADD COLUMN 的列。
    # SQLite 是空库起步，直接把最终形态并进 CREATE TABLE。
    #   来源：DO #4  -> llm_profiles.thinking_type
    #   来源：DO #7  -> market_credentials.kind
    'llm_profiles': [('thinking_type', "TEXT NOT NULL DEFAULT ''")],
    'market_credentials': [('kind', "TEXT NOT NULL DEFAULT 'trade'")],
}

DO_BLOCK_CONSTRAINTS = {
    # DO 块里加的表级约束（同样并进 CREATE TABLE）。
    'agents': ["CONSTRAINT fk_agents_curprompt FOREIGN KEY (current_prompt_id) "
               "REFERENCES agent_prompts(id) ON DELETE SET NULL"],
    'instruments': ["CONSTRAINT ck_instruments_symbol_not_test "
                    "CHECK (symbol NOT LIKE 'GATE-TEST-%' AND symbol NOT LIKE 'RATING-FALLBACK%')"],
    'market_credentials': ["CONSTRAINT ck_market_credentials_kind CHECK (kind IN ('data','trade'))"],
    'exploration_nodes': [
        "CONSTRAINT ck_node_kind CHECK (kind IN ('begin','goal','intent','fact','finding',"
        "'hint','digest','join','thesis','signal','conclusion'))",
        "CONSTRAINT ck_node_state CHECK ("
        "(kind='begin'   AND state IN ('open')) OR "
        "(kind='intent'  AND state IN ('open','running','paused','done','blocked','exhausted',"
        "'stopped','deleted')) OR "
        "(kind='goal'    AND state IN ('open','met','abandoned')) OR "
        "(kind='fact'    AND state IN ('confirmed','dismissed','origin')) OR "
        "(kind='finding' AND state IN ('confirmed','dismissed')) OR "
        "(kind='hint'    AND state IN ('active','consumed')) OR "
        "(kind='digest'  AND state IN ('active','superseded')) OR "
        "(kind='join'       AND state IN ('open','done','blocked')) OR "
        "(kind='thesis'     AND state IN ('open','standing','refuted','superseded')) OR "
        "(kind='signal'     AND state IN ('open','acted','expired','voided')) OR "
        "(kind='conclusion' AND state IN ('final','superseded')))",
    ],
    'exploration_edges': [
        "CONSTRAINT ck_edge_rel CHECK (rel IN ('spawns','derived_from','yields',"
        "'proves','covers','gates'))",
    ],
}


def apply_do_blocks(tables):
    """把 DO 块里新增的列/约束并入模型（只在 CREATE TABLE 里缺时才补）。"""
    stats = {'do_col': 0, 'do_con': 0}
    for tname, cols in DO_BLOCK_COLUMNS.items():
        t = tables.get(tname)
        if t is None:
            raise SystemExit('DO block targets unknown table %s' % tname)
        for cname, craw in cols:
            if t.add_column(cname, craw):
                stats['do_col'] += 1
    for tname, cons in DO_BLOCK_CONSTRAINTS.items():
        t = tables.get(tname)
        if t is None:
            raise SystemExit('DO block targets unknown table %s' % tname)
        for c in cons:
            cname = re.match(r'CONSTRAINT\s+(\w+)', c).group(1)
            if not any(re.match(r'CONSTRAINT\s+%s\b' % re.escape(cname), x, re.I)
                       for x in t.constraints):
                t.constraints.append(c)
                stats['do_con'] += 1
    return stats


def parse():
    text = strip_comments(io.open(SRC, encoding='utf-8').read())
    stmts = split_statements(text)
    tables, order = {}, []
    for st in stmts:
        s = st.strip()
        m = CREATE_RE.match(s)
        if m:
            name, body = m.group(1), m.group(2)
            t = Table(name)
            for part in split_top_level(body):
                p = ' '.join(part.split())
                if re.match(r'^(CONSTRAINT\s+\w+|PRIMARY\s+KEY|UNIQUE|CHECK|FOREIGN\s+KEY)\b', p, re.I):
                    t.constraints.append(p)
                else:
                    cm = re.match(r'^("?\w+"?)\s+(.+)$', p, re.S)
                    if not cm:
                        raise SystemExit('bad column in %s: %r' % (name, p))
                    colname = cm.group(1).strip('"')
                    t.columns.append(Column(colname, cm.group(2)))
            tables[name] = t
            order.append(name)
    stats = apply_alters(tables, stmts)
    stats.update(apply_do_blocks(tables))
    return tables, order, stmts, stats


if __name__ == '__main__':
    tables, order, stmts = parse()
    print('tables:', len(tables), 'stmts:', len(stmts))
    for n in order[:3]:
        t = tables[n]
        print('\n--', n)
        for c in t.columns:
            print('   ', c)
        for k in t.constraints:
            print('   C:', k[:100])
