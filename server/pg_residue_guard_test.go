package server

// 全局 PG 语法残留守卫（防回归）。
//
// # 为什么需要它
//
// 本次迁移里最危险的不是「改不对」，而是「改得看起来对」：
//
//	`payload || '{}'::jsonb`  →  删掉 `::jsonb`  →  `payload || '{}'`
//
// 编译通过、运行时也**不报错**，但语义从「深合并」变成「字符串拼接」，
// 得到非法 JSON。这类错误没有任何单元测试会主动发现 —— 只有在真实业务
// 流程跑到那行 SQL 时才炸，而且报错信息往往面目全非。
//
// 同理 `payload->>'k'`（PG 操作符）删 `::` 时极易漏掉，运行时报
// `unrecognized token: ">"`。
//
// 所以做一次静态扫描：把非注释期里出现 PG 专有语法的地方揪出来。它比
// 「靠人 review」可靠 —— review 会看漏，grep 不会。
//
// # 扫描范围与白名单
//
// 只扫 `db/ server/ agent/ exec/ evidence/ cmd/` 下的**非测试** Go 文件。
// 注释里的说明文字（例如解释「为什么不能用 `::jsonb`」）是**有价值的**，
// 所以先剥注释再匹配。
//
// 测试文件不扫：它们只用 `sqlite_master` 之类的 SQLite 原生语法，
// 且已由各包自己的测试覆盖。

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// pgResiduePattern 是「SQLite 不认」的 PG 专有语法。
//
// # 每条都说明为什么必须禁
//
//	::jsonb / ::text / ::int / ::bigint / ::numeric   类型转换语法，SQLite 是 unrecognized token ":"
//	->>                                              jsonb 文本操作符，SQLite 报 unrecognized token ">"
//	jsonb_*                                          jsonb 函数族，SQLite 只有 json_*
//	row_to_json                                      无等价物（要用 json_object 显式列字段）
//	xmax                                             PG 的 MVCC 系统列
//	FOR UPDATE / SKIP LOCKED                          行锁，SQLite 无
//	pg_advisory / try_inet / masklen                  PG 内置函数
//	DISTINCT ON                                       PG 语法
//
// 注意不列 `unnest` / `array_to_json`：SQLite 3.53 没有它们，但它们在
// 注释里出现的频率很高，且没有「看起来等价」的误用形态。
var pgResiduePattern = regexp.MustCompile(
	`::(jsonb|text|int|integer|bigint|numeric|timestamptz|date|float8|cidr|inet|regclass)\b` +
		`|->>` +
		`|\bjsonb_[a-z_]+` +
		`|\brow_to_json\b` +
		`|\bxmax\b` +
		`|\bFOR\s+UPDATE\b` +
		`|\bSKIP\s+LOCKED\b` +
		`|\bpg_advisory` +
		`|\btry_inet\b` +
		`|\bmasklen\b` +
		`|\bDISTINCT\s+ON\b`,
)

// pgResidueScanRoots 是被扫的目录（相对仓库根）。
var pgResidueScanRoots = []string{"db", "server", "agent", "exec", "evidence", "cmd"}

// pgResidueAllowed 是按文件名豁免的清单。
//
// 每个条目必须写明理由 —— 一旦有人加条目而不写理由，这条守卫就开始漏。
var pgResidueAllowed = map[string]string{
	// sqlite_err.go 的注释里引用 PG 错误码做对照（"取代 pgconn.PgError"），
	// 那是**说明历史**的文字，不是活动 SQL。
	"db/sqlite_err.go": "注释里对照 PG 的错误码，无活动 SQL",
}

// TestNoPostgresSyntaxResidue 扫描全部非测试 Go 文件的**非注释**内容。
//
// # 为什么按 Go 语法切字符串而不是全文正则
//
// 全文正则会把注释、日志文案、错误信息里的 `::text` 也当成 SQL —— 误报会让
// 这条守卫被无视（`// 见 PG 版 ::text 的说明` 这种注释到处都是）。
// 用 `go/parser` 拿到 AST 后跳过 Comment 节点，只保留字面量与原始 token 流，
// 误报率降到几乎为零。
func TestNoPostgresSyntaxResidue(t *testing.T) {
	root := repoRoot(t)
	var offenders []string
	for _, rel := range pgResidueScanRoots {
		dir := filepath.Join(root, rel)
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			continue // 目录可能还不存在（如某些分支没有 evidence/）
		}
		if err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				// vendor / node_modules / 生成目录不扫。
				name := entry.Name()
				if name == "vendor" || name == "node_modules" || strings.HasPrefix(name, ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				return nil
			}
			relPath, err := filepath.Rel(root, path)
			if err != nil {
				relPath = path
			}
			relPath = filepath.ToSlash(relPath)
			if reason, ok := pgResidueAllowed[relPath]; ok {
				_ = reason // 豁免：理由已写在 pgResidueAllowed 的条目上
				return nil
			}
			hits := scanFileForPGResidue(t, path)
			for _, h := range hits {
				offenders = append(offenders,
					relPath+":"+strconv.Itoa(h.line)+": "+h.snippet)
			}
			return nil
		}); err != nil {
			t.Fatalf("扫描 %s 失败: %v", rel, err)
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("发现 %d 处 PostgreSQL 专有语法（SQLite 会报 unrecognized token / no such column）。\n"+
			"改法见 docs/quantex/ARCH-SQLite-Migration.md §2.1；若是注释里的说明文字，"+
			"改写成不触发本正则的措辞（例如 `PG 的 jsonb 合并` 而不是 `::jsonb`）：\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}

// residueHit 是一处命中。
type residueHit struct {
	line    int
	snippet string
}

// scanFileForPGResidue 解析一个 Go 文件，只检查**非注释**区域。
//
// 实现取巧但可靠：先用 parser 收集所有注释区间，然后把源文本按行扫，
// 跳过落在注释区间内的部分。这比遍历 AST 每个节点更简单，也不会漏掉
// 裸的 SQL 字符串常量（那种不在任何表达式节点上）。
func scanFileForPGResidue(t *testing.T, path string) []residueHit {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		// 解析失败不是本测试的职责（编译错误会被 go build 拦住）。
		return nil
	}
	// 收集注释区间（字节偏移）。
	type span struct{ start, end int }
	commentSpans := make([]span, 0, len(f.Comments))
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			commentSpans = append(commentSpans, span{start: fset.Position(c.Pos()).Offset, end: fset.Position(c.End()).Offset})
		}
	}
	// 逐行扫；行内若整段落在注释里则跳过，否则去掉注释后缀再匹配。
	var hits []residueHit
	offset := 0
	for lineNo, line := range strings.Split(string(src), "\n") {
		lineStart, lineEnd := offset, offset+len(line)
		offset = lineEnd + 1 // +1 是换行符
		// 把这一行里落在注释区间内的字符替换成空格。
		cleaned := []byte(line)
		for _, s := range commentSpans {
			if s.end <= lineStart || s.start >= lineEnd {
				continue
			}
			from := maxInt(s.start-lineStart, 0)
			to := minInt(s.end-lineStart, len(cleaned))
			for i := from; i < to; i++ {
				cleaned[i] = ' '
			}
		}
		if pgResiduePattern.Match(cleaned) {
			hits = append(hits, residueHit{line: lineNo + 1, snippet: strings.TrimSpace(line)})
		}
	}
	return hits
}

// TestPGResidueGuardCatchesOffenders 是这条守卫的**自检**。
//
// # 为什么守卫自己也要测
//
// 一条永远返回「通过」的守卫是有害的：它给人一种「已经防住了」的错觉，
// 而实际上把 `::jsonb` 加回去也不会有任何东西报警。所以这里造三个
// 合成文件（活动代码 / 注释 / 字符串字面量），断言前两类行为符合预期。
func TestPGResidueGuardCatchesOffenders(t *testing.T) {
	dir := t.TempDir()

	// ① 活动代码里的 PG 语法 —— 必须被抓到。
	active := filepath.Join(dir, "active.go")
	if err := os.WriteFile(active, []byte(`package x

func f() string { return `+"`"+`SELECT a::jsonb FROM t`+"`"+` }
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if hits := scanFileForPGResidue(t, active); len(hits) != 1 {
		t.Fatalf("活动代码里的 ::jsonb 未被抓到（hits=%d）—— 守卫失效", len(hits))
	}

	// ② 纯注释里的同一串 —— 不该被抓（那是说明文字）。
	commented := filepath.Join(dir, "commented.go")
	if err := os.WriteFile(commented, []byte(`package x

// 旧写法是 a::jsonb，现在改成 json_patch。
func f() string { return "ok" }
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if hits := scanFileForPGResidue(t, commented); len(hits) != 0 {
		t.Fatalf("注释里的 ::jsonb 被误报（hits=%d）—— 误报会让守卫被无视", len(hits))
	}

	// ③ 行尾注释 + 同行活动 SQL —— 必须只报 SQL 那一部分。
	mixed := filepath.Join(dir, "mixed.go")
	if err := os.WriteFile(mixed, []byte(`package x

func f() string { return `+"`"+`SELECT a::text`+"`"+` } // 说明：以前是 ::jsonb
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if hits := scanFileForPGResidue(t, mixed); len(hits) != 1 {
		t.Fatalf("同行 SQL 里的 ::text 未被抓到（hits=%d）", len(hits))
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// repoRoot 从当前工作目录向上找 go.mod（测试的工作目录是包目录）。
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("找不到 go.mod（无法定位仓库根）")
		}
		dir = parent
	}
}
