package intercept

import (
	"testing"

	"github.com/Autumn-27/artex/db"
)

// testDSNForIntercept 取与 db 包测试同一套 DSN 解析逻辑。
// 本包不常连库,只有「已关闭 DB」这个用例需要。
func testDSNForIntercept(t *testing.T) string {
	t.Helper()
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skipf("no database config (%v)", err)
	}
	return dsn
}

// =====================================================================
// G11 的对侧:工具路径必须保持 fail-open
// =====================================================================

// TestMatchStillFailOpen 是防止「为了做 fail-closed 顺手把 Match 也改了」的护栏。
//
// 工具路径与草案路径的失败语义**必须相反**,这是场景差异,不是不一致:
//   - 工具路径(渗透):规则读不到 / matcher 报错 → 当作未命中 → 放行。
//     误拦会打断探索,且渗透场景试错成本低。
//   - 草案路径(金融):任一步失败 → deny。放行一个本该拦住的单不可逆。
//
// 这个测试在每次改动 Match / ruleMatches / matchStructured 后都应该跑。
// 它一旦变红,说明有人把两边的语义统一了 —— 无论统一到哪一侧都会破坏另一侧。
func TestMatchStillFailOpen(t *testing.T) {
	t.Run("DB error 时按未命中放行", func(t *testing.T) {
		// 用「已关闭的 DB」制造稳定的 error:db.DB 内嵌 *sql.DB,
		// 关闭后 Query 必然返回 sql.ErrConnDone,不会 panic。
		// 不能用 &Interceptor{db: nil}:loadLocked 会走 d.Query,
		// 对 nil 内嵌指针解引用直接 panic,测不到 fail-open 分支。
		d, err := db.Open(testDSNForIntercept(t))
		if err != nil {
			t.Skipf("postgres unavailable (%v)", err)
		}
		d.Close()

		i := &Interceptor{db: d}
		dec, matched := i.Match("Bash", []byte(`{"command":"rm -rf /"}`))
		if matched {
			t.Error("规则加载失败时必须返回 matched=false(工具路径 fail-open)")
		}
		if dec.Action == "deny" {
			t.Error("规则加载失败时不得 deny(那是草案路径的语义)")
		}
	})

	t.Run("零规则时按未命中放行", func(t *testing.T) {
		i := &Interceptor{}
		i.cached = []compiledRule{} // 显式空规则集,跳过 DB 加载
		dec, matched := i.Match("Bash", []byte(`{"command":"sudo su"}`))
		if matched {
			t.Error("零规则时必须返回 matched=false")
		}
		if dec.Action == "deny" {
			t.Error("零规则时不得 deny")
		}
	})

	t.Run("matcher 返回 error 时跳过该条继续", func(t *testing.T) {
		i := &Interceptor{}
		// 一条结构化规则,其 matcher 当前返回 errMatcherNotImplemented。
		// 工具路径应当跳过它,而不是把它当成 deny。
		r := compiledRule{}
		r.ID = 1
		r.Name = "structured-but-unimplemented"
		r.MatchTarget = "tool_input"
		r.MatchType = "numeric_lte"
		r.Pattern = "never-matches"
		r.Action = "deny"
		r.Enabled = true
		i.cached = []compiledRule{r}

		dec, matched := i.Match("Bash", []byte(`{"qty":99999}`))
		if matched {
			t.Error("matcher 报「无法判定」时,工具路径应跳过而非命中")
		}
		if dec.Action == "deny" {
			t.Error("matcher 报错时不得 deny(工具路径 fail-open)")
		}
	})

	t.Run("未知 match_target 时跳过该条继续", func(t *testing.T) {
		i := &Interceptor{}
		r := compiledRule{}
		r.ID = 2
		r.Name = "unknown-target"
		r.MatchTarget = "mars_rover"
		r.MatchType = "string"
		r.Pattern = "x"
		r.Action = "deny"
		r.Enabled = true
		i.cached = []compiledRule{r}

		dec, matched := i.Match("Bash", []byte("x"))
		if matched {
			t.Error("未知 match_target 在工具路径应跳过,不得命中")
		}
		if dec.Action == "deny" {
			t.Error("未知 match_target 不得让工具路径 deny")
		}
	})
}

// TestMatchStillMatchesNormalRules 证明上面的 fail-open 没有把正常判定也弄坏。
func TestMatchStillMatchesNormalRules(t *testing.T) {
	i := &Interceptor{}
	r := compiledRule{}
	r.ID = 3
	r.Name = "block-rm"
	r.MatchTarget = "tool_input"
	r.MatchType = "string"
	r.Pattern = "rm -rf"
	r.Action = "deny"
	r.Enabled = true
	i.cached = []compiledRule{r}

	dec, matched := i.Match("Bash", []byte(`{"command":"rm -rf /tmp/x"}`))
	if !matched {
		t.Fatal("正常命中的规则必须 matched=true")
	}
	if dec.Action != "deny" {
		t.Errorf("action = %q, want deny", dec.Action)
	}
	if dec.RuleName != "block-rm" {
		t.Errorf("rule name = %q", dec.RuleName)
	}

	// 不匹配时仍应为未命中。
	_, matched = i.Match("Bash", []byte(`{"command":"ls"}`))
	if matched {
		t.Error("不匹配时 matched 必须为 false")
	}
}

// dbOpenForTest 打开测试用 DB。仅供需要「已关闭 DB」的用例使用。
func dbOpenForTest(t *testing.T) (*db.DB, error) {
	t.Helper()
	dsn, _, err := db.DSN()
	if err != nil {
		return nil, err
	}
	return db.Open(dsn)
}
