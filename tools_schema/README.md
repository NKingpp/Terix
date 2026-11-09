# tools_schema —— PostgreSQL schema → SQLite schema 生成器

**用途**：`db/schema.sql`（PostgreSQL 版，源头）→ `db/schema_sqlite.sql`（SQLite 版，生成产物）。

## 为什么用生成器而不是手写

两者必须保持一致，而手写 2052 行必然漂移。生成器保证：

- 67 张表的**列集合与列顺序**来自同一份定义
- 144 条 `ALTER TABLE`（ADD COLUMN / ADD\DROP CONSTRAINT）与 7 个 `DO $$` 块里的
  加列都并进了 `CREATE TABLE`（SQLite 空库起步，不需要存量迁移逻辑）
- 索引/触发器的名字与 PG 版一一对应，便于交叉核查

## 怎么用

```bash
# 改了 db/schema.sql 之后，重新生成
python tools_schema/gen_ddl.py

# 验证：生成的 SQL 能真的建出库
go run ./cmd/dbinit          # 或跑 scripts/test-fresh-db.sh
```

`gen_ddl.py` 会打印被丢弃的 PG 专用索引（GIN/GIST，共 9 个）。

## 三个文件

| 文件 | 职责 |
|---|---|
| `gen_sqlite_schema.py` | 解析 PG schema 成结构模型（表/列/约束/ALTER/DO 块）。**这是唯一的解析实现** |
| `gen_ddl.py` | 从模型生成 SQLite DDL：类型映射、CHECK 改写、索引、调用触发器生成 |
| `gen_triggers.py` | 生成 25 个触发器（21 个 `updated_at` + 4 个业务触发器） |

## 关键映射（细节见 docs/quantex/ARCH-SQLite-Migration.md）

| PG | SQLite | 说明 |
|---|---|---|
| `BIGSERIAL` | `INTEGER PRIMARY KEY AUTOINCREMENT` | id 会进归档/边表/DTO，禁止复用。少数纯日志表用不带 AUTOINCREMENT 的版本 |
| `TIMESTAMPTZ` | `TEXT` + `strftime('%Y-%m-%dT%H:%M:%fZ','now')` | ISO8601 UTC 毫秒；字典序＝时间序 |
| `JSONB` / `TEXT[]` / `JSONB[]` | `TEXT`（JSON 文本） | 数组是 `[]`，对象是 `{}` |
| `NUMERIC` | `TEXT` | Go 侧 `big.Rat` 精确运算（见 ARCH §2.11） |
| `CIDR` / `INET` | `TEXT` | 包含判断走 UDF `cidr_contains` |
| `x = ANY(ARRAY[...])` | `x IN (...)` | CHECK 内改写 |
| `x <@ ARRAY[...]` | `x LIKE '%"v"%' OR ...` | **CHECK 里禁止子查询**，只能退化成 LIKE |
| `key ~ 'regex'` | `key GLOB ...` | 单处；UDF 在 CHECK 里不可用（连接级注册） |
| `x ? 'k'`（jsonb 键存在） | `json_type(x,'$.k') IS NOT NULL` | `?` 是 SQLite 占位符 |
| `date_trunc('second', x)` | `substr(x,1,19)` | 索引表达式 |
| `USING GIN` / `USING GIST` | **删除** | 无等价物，靠 `json_each` + UDF |

## 触发器为什么要拆

`AFTER INSERT OR UPDATE OF col` 在 SQLite 里是**语法错误**（`near "OR"`），
所以 `trg_expnodes_owner` 与 `trg_assets_task_links` 各拆成两条（INSERT / UPDATE OF）。
23 个 PG 触发器 → 25 个 SQLite 触发器。

`BEFORE` 触发器不能改 `NEW`，所以 21 个 `set_updated_at` 全部改成
`AFTER UPDATE` + 自写一行 + `WHEN NEW.updated_at IS OLD.updated_at` 守卫。

## 注意

- 生成文件头写着"勿手改"。要改结构请改 `db/schema.sql` 然后重跑生成器。
- 生成器里所有"看起来奇怪"的写法都带注释说明实测依据，改动前先读注释。
