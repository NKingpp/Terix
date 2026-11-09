package db

import (
	"database/sql"
	"errors"
	"sync"
	"testing"
)

// TestApplySchemaRetriesOnlyBusy 验证 schema 应用只对 SQLITE_BUSY 重试。
//
// 从 PG 的 40P01 迁移到 SQLite 后：
//   - 可重试的是 SQLITE_BUSY（写锁竞争，另一个实例正在初始化）
//   - 其它错误（语法错、权限、schema 冲突）必须立刻失败，不能无限重试
//
// 用注入的 isBusyFn 模拟 BUSY（*sqlite.Error 字段未导出，无法构造）。
func TestBusyApplySchemaRetriesOnlyBusy(t *testing.T) {
	orig := isBusyFn
	t.Cleanup(func() { isBusyFn = orig })

	// 前两次报 BUSY，第三次成功
	attempts := 0
	isBusyFn = func(error) bool { return true }
	err := applySchemaWithRetry(t.Context(), fakeExecer(func() error {
		attempts++
		if attempts < 3 {
			return errFakeBusy
		}
		return nil
	}))
	if err != nil {
		t.Fatalf("期望重试后成功: %v", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts=%d, want 3", attempts)
	}

	// 非 BUSY 错误必须只尝试一次
	isBusyFn = func(error) bool { return false }
	nonRetryable := errors.New("语法错误")
	attempts = 0
	err = applySchemaWithRetry(t.Context(), fakeExecer(func() error {
		attempts++
		return nonRetryable
	}))
	if !errors.Is(err, nonRetryable) || attempts != 1 {
		t.Fatalf("非 BUSY 错误应只尝试一次: attempts=%d err=%v", attempts, err)
	}
}

// errFakeBusy 只在测试里用作"看起来像 BUSY"的错误标记。
var errFakeBusy = errors.New("database is locked (5) (SQLITE_BUSY)")

// fakeExecer 是只有 Exec 的最小 execer 实现。
type fakeExecer func() error

func (f fakeExecer) Exec(string, ...any) (sql.Result, error) { return nil, f() }
func (f fakeExecer) Query(string, ...any) (*sql.Rows, error) { return nil, nil }
func (f fakeExecer) QueryRow(string, ...any) *sql.Row        { return nil }

// testOpen 返回包级共享的测试库（由 TestMain 打开，见 testmain_test.go）。
//
// SQLite 下每个进程一个临时文件天然隔离，不再需要 PG 时代的 advisory lock。
// 仍然共享实例：schema 有 67 张表 + 125 索引 + 25 触发器，每个测试都跑一遍太慢。
var (
	sharedTestDBOnce sync.Once
	sharedTestDBInst *DB
	sharedTestDBErr  error
)

func testOpen(t *testing.T) *DB {
	t.Helper()
	sharedTestDBOnce.Do(func() {
		sharedTestDBInst, sharedTestDBErr = Open(testDSN(t))
	})
	if sharedTestDBErr != nil {
		t.Fatalf("打开测试库失败: %v", sharedTestDBErr)
	}
	return sharedTestDBInst
}

// TestOpenSeed opens the live dev PG, applies schema, seeds, and verifies the
// builtin agents + their variable catalog exist. Skips if PG is unreachable.
func TestOpenSeed(t *testing.T) {
	d := testOpen(t)

	// # 为什么断言「这些 key 都在」而不是「总数等于 N」
	//
	// builtinAgents + builtinFinancialAgents 是代码里的内置角色清单，但 DB 里
	// builtin=true 的行可能更多 —— 用户/运行期脚本也可能写入 builtin 行。
	//
	// 断言「总数 == N」会因为这些行而失败，而那不代表 seed 坏了。
	// 断言「代码清单里每个 key 都存在」才对应「seedBuiltins 跑成功了」这个事实。
	for _, a := range builtinAgents {
		var n int
		if err := d.QueryRow(`SELECT count(*) FROM agents WHERE builtin AND key=$1`, a.key).Scan(&n); err != nil {
			t.Fatalf("查 agent %s 失败: %v", a.key, err)
		}
		if n != 1 {
			t.Errorf("内置 agent %q 应有且仅有一行，实际 %d 行", a.key, n)
		}
	}

	// planner should have at least its seeded catalog vars
	wantPlannerVars := 0
	for _, a := range builtinAgents {
		if a.key == "planner" {
			wantPlannerVars = len(a.vars)
			break
		}
	}
	var plannerVars int
	if err := d.QueryRow(`
SELECT count(*) FROM agent_prompt_vars v
JOIN agents a ON a.id = v.agent_id
WHERE a.key = 'planner'`).Scan(&plannerVars); err != nil {
		t.Fatal(err)
	}
	if plannerVars < wantPlannerVars {
		t.Fatalf("want at least %d planner vars, got %d", wantPlannerVars, plannerVars)
	}

	// re-open must be idempotent (no duplicate agents)
	d2, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	// 幂等性：重复 seed 不能让内置 agent 变成多行。
	// 逐个断言「恰好一行」而不是「总数不变」—— 后者会被运行期写入的
	// 金融 agent 干扰（见上面的注释）。
	for _, a := range builtinAgents {
		var n int
		if err := d2.QueryRow(`SELECT count(*) FROM agents WHERE builtin AND key=$1`, a.key).Scan(&n); err != nil {
			t.Fatalf("re-open 后查 agent %s 失败: %v", a.key, err)
		}
		if n != 1 {
			t.Errorf("re-open 后内置 agent %q 应有且仅有一行（seed 不幂等），实际 %d 行", a.key, n)
		}
	}
}
