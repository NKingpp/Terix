# -*- coding: utf-8 -*-
"""触发器生成：PG 的 BEFORE ... EXECUTE FUNCTION 转 SQLite 的 AFTER + 自写一行。

SQLite 的关键限制（均已实测，见 ARCH-SQLite-Migration.md §8）：
  1. BEFORE 触发器不能修改 NEW（没有 SET NEW.x 语法）
     -> 统一改 AFTER UPDATE + `UPDATE t SET updated_at=... WHERE rowid=NEW.rowid`
  2. `AFTER INSERT OR UPDATE OF col` 语法非法（near "OR"）
     -> 业务触发器必须拆成两条
  3. 必须有 WHEN NEW.x IS OLD.x 守卫，否则 recursive_triggers=1 时会
     撞 "too many levels of trigger recursion"
"""

SQL_NOW = "strftime('%Y-%m-%dT%H:%M:%fZ','now')"

# 21 个 set_updated_at 型触发器（表名 -> 触发器名）。顺序与 PG schema 一致。
UPDATED_AT_TRIGGERS = [
    ('companies', 'trg_companies_upd'),
    ('assets', 'trg_av2_upd'),
    ('explorations', 'trg_exp_upd'),
    ('exploration_nodes', 'trg_expnodes_upd'),
    ('llm_profiles', 'trg_llm_upd'),
    ('task_categories', 'trg_task_categories_upd'),
    ('tasks', 'trg_tasks_upd'),
    ('task_archives', 'trg_task_archives_upd'),
    ('task_templates', 'trg_task_templates_upd'),
    ('task_asset_links', 'trg_task_asset_links_upd'),
    ('task_llm_profiles', 'trg_task_llm_profiles_upd'),
    ('agents', 'trg_agents_upd'),
    ('mcp_servers', 'trg_mcp_upd'),
    ('tools', 'trg_tools_upd'),
    ('conversations', 'trg_conversations_upd'),
    ('agent_triggers', 'trg_agent_triggers_upd'),
    ('intercept_rules', 'trg_intercept_rules_upd'),
    ('asset_intercept_rules', 'trg_asset_intercept_rules_upd'),
    ('task_intercept_rules', 'trg_task_intercept_rules_upd'),
    ('notification_channels', 'trg_notification_channels_upd'),
    ('market_credentials', 'trg_market_credentials_upd'),
]


def gen_trigger_sql():
    out = []
    out.append('-- ============================================================')
    out.append('-- 阶段 3：触发器')
    out.append('--')
    out.append('-- PG 的 23 个触发器在这里变成 25 个，原因有二（实测，见 §8）：')
    out.append('--   1. SQLite 的 BEFORE 不能改 NEW -> 全部改 AFTER UPDATE + 自写一行。')
    out.append('--   2. `AFTER INSERT OR UPDATE OF col` 语法非法（near "OR"）')
    out.append('--      -> 两个业务触发器各拆成 2 条。')
    out.append('-- 21 + 2 + 2 = 25。')
    out.append('-- ============================================================')
    out.append('')

    out.append('-- ---------- 21 个 updated_at 自动维护 ----------')
    out.append('--')
    out.append('-- WHEN NEW.updated_at IS OLD.updated_at 是必须的：')
    out.append('-- 自写的那一行也会触发本触发器，守卫让第二次进来时条件不成立。')
    out.append('-- 用 rowid=NEW.rowid 而不是主键列，这样复合主键表（task_llm_profiles）')
    out.append('-- 也能用同一套模板。')
    for table, name in UPDATED_AT_TRIGGERS:
        out.append('')
        out.append('DROP TRIGGER IF EXISTS %s;' % name)
        out.append('CREATE TRIGGER %s AFTER UPDATE ON %s FOR EACH ROW' % (name, table))
        out.append('WHEN NEW.updated_at IS OLD.updated_at')
        out.append('BEGIN')
        out.append("    UPDATE %s SET updated_at = %s WHERE rowid = NEW.rowid;" % (table, SQL_NOW))
        out.append('END;')

    out.append('')
    out.append('-- ---------- 业务触发器 1：exploration_nodes.owner 派生 ----------')
    out.append('--')
    out.append('-- PG 原语义（BEFORE INSERT OR UPDATE OF payload）：')
    out.append('--   IF NEW.owner IS NULL THEN NEW.owner := NULLIF(TRIM(payload->>\'role\'), \'\'); END IF;')
    out.append('-- 四条被测试锁住的语义（db/exploration_test.go）：')
    out.append('--   ① 不写 owner 时从 payload.role 派生')
    out.append('--   ② 无 role -> NULL 而不是 \'\'')
    out.append('--   ③ 空白 role -> NULL')
    out.append('--   ④ 显式传的 owner 不被覆盖')
    out.append('-- 拆成两条（INSERT / UPDATE OF payload）是语法强制，不是选择。')
    out.append('--')
    out.append('-- ⚠️ INSERT ... RETURNING owner 拿不到这里写入的值（AFTER 触发器的局限，')
    out.append('--    见 §2.10 第 3 条）。调用方需要派生值时请 INSERT 后再 SELECT。')
    out.append('')
    out.append('DROP TRIGGER IF EXISTS trg_expnodes_owner_ins;')
    out.append('CREATE TRIGGER trg_expnodes_owner_ins AFTER INSERT ON exploration_nodes FOR EACH ROW')
    out.append('WHEN NEW.owner IS NULL')
    out.append('BEGIN')
    out.append("    UPDATE exploration_nodes")
    out.append("       SET owner = NULLIF(trim(json_extract(NEW.payload,'$.role')), '')")
    out.append('     WHERE id = NEW.id AND owner IS NULL;')
    out.append('END;')
    out.append('')
    out.append('DROP TRIGGER IF EXISTS trg_expnodes_owner_upd;')
    out.append('CREATE TRIGGER trg_expnodes_owner_upd AFTER UPDATE OF payload ON exploration_nodes FOR EACH ROW')
    out.append('BEGIN')
    out.append("    UPDATE exploration_nodes")
    out.append("       SET owner = NULLIF(trim(json_extract(NEW.payload,'$.role')), '')")
    out.append('     WHERE id = NEW.id AND owner IS NULL;')
    out.append('END;')

    out.append('')
    out.append('-- ---------- 业务触发器 2：assets.task_ids -> task_asset_links ----------')
    out.append('--')
    out.append('-- PG 原语义（AFTER INSERT OR UPDATE OF task_ids）：')
    out.append('--   补行：unnest(NEW.task_ids) 里指向存活任务（deleted_at IS NULL）的')
    out.append('--   删行：不再被 NEW.task_ids 引用的')
    out.append('-- SQLite 侧数组是 JSON 文本，用 json_each 展开；')
    out.append('-- INSERT OR IGNORE 依赖 task_asset_links 的 PRIMARY KEY (task_id, asset_id)。')
    out.append('')
    out.append('DROP TRIGGER IF EXISTS trg_assets_task_links_ins;')
    out.append('CREATE TRIGGER trg_assets_task_links_ins AFTER INSERT ON assets FOR EACH ROW')
    out.append('BEGIN')
    out.append('    INSERT OR IGNORE INTO task_asset_links(task_id, asset_id, source, source_summary)')
    out.append("      SELECT CAST(value AS INTEGER), NEW.id, 'system', '任务执行期间自动关联'")
    out.append('        FROM json_each(NEW.task_ids)')
    out.append('       WHERE CAST(value AS INTEGER) IN (SELECT id FROM tasks WHERE deleted_at IS NULL);')
    out.append('END;')
    out.append('')
    out.append('DROP TRIGGER IF EXISTS trg_assets_task_links_upd;')
    out.append('CREATE TRIGGER trg_assets_task_links_upd AFTER UPDATE OF task_ids ON assets FOR EACH ROW')
    out.append('BEGIN')
    out.append('    INSERT OR IGNORE INTO task_asset_links(task_id, asset_id, source, source_summary)')
    out.append("      SELECT CAST(value AS INTEGER), NEW.id, 'system', '任务执行期间自动关联'")
    out.append('        FROM json_each(NEW.task_ids)')
    out.append('       WHERE CAST(value AS INTEGER) IN (SELECT id FROM tasks WHERE deleted_at IS NULL);')
    out.append('    DELETE FROM task_asset_links')
    out.append('     WHERE asset_id = NEW.id')
    out.append('       AND task_id NOT IN (SELECT CAST(value AS INTEGER) FROM json_each(NEW.task_ids));')
    out.append('END;')
    out.append('')
    return out
