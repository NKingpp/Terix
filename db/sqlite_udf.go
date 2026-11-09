package db

// SQLite 自定义函数（UDF）。必须在任何连接被创建之前注册，所以放 init()。
//
// # 为什么需要 now()
//
// schema 里 90 处 DEFAULT now()，Go 代码里 104 处 now()。注册一个同名的
// UDF 之后这些 SQL 一行都不用改 —— 相比之下把 194 处替换成
// strftime(...) 是纯机械改动且容易漏。
//
// 实测：DEFAULT (now()) 在 CREATE TABLE 里也能正常工作。
//
// # 为什么需要 cidr_contains / cidr_masklen
//
// SQLite 没有网络类型，也没有任意前缀的网段包含判断 —— 纯 SQL 无法实现。
// 原 PG 的 `cs.net >>= try_inet(a.ip)` 与 `masklen(cs.net)` 各需要一条 UDF。
// 语义要保住 PG 的"脏数据跳过而不报错"行为：任一侧非法时返回 NULL，
// 让 WHERE 自然过滤掉这一行（PG 侧靠 try_inet 的 EXCEPTION 实现同样效果）。

import (
	"database/sql/driver"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"modernc.org/sqlite"
)

var (
	// regexpCache 避免在 CHECK 约束（逐行求值）里反复编译同一个模式。
	regexpCache sync.Map // map[string]*regexp.Regexp
)

func init() {
	// now() -> ISO8601 UTC 毫秒串，与 SQLNow / NowISO 完全同格式。
	sqlite.MustRegisterScalarFunction("now", 0,
		func(_ *sqlite.FunctionContext, _ []driver.Value) (driver.Value, error) {
			return NowISO(), nil
		})

	// cidr_contains(net, addr) -> 1 / 0 / NULL
	//
	// 等价 PG 的 `net >>= try_inet(addr)`：
	//   - addr 非法（主机名等脏数据）-> NULL（等价 try_inet 返回 NULL）
	//   - net 非法 -> NULL
	//   - 否则 -> net 包含 addr 时 1，否则 0
	sqlite.MustRegisterDeterministicScalarFunction("cidr_contains", 2,
		func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			netStr, ok1 := argString(args, 0)
			addrStr, ok2 := argString(args, 1)
			if !ok1 || !ok2 || netStr == "" || addrStr == "" {
				return nil, nil
			}
			prefix, err := netip.ParsePrefix(netStr)
			if err != nil {
				return nil, nil
			}
			addr, err := netip.ParseAddr(addrStr)
			if err != nil {
				return nil, nil
			}
			if prefix.Contains(addr) {
				return int64(1), nil
			}
			return int64(0), nil
		})

	// cidr_masklen(net) -> 前缀长度 / NULL
	//
	// 等价 PG 的 masklen(net)，用于"最长前缀优先"的排序。
	sqlite.MustRegisterDeterministicScalarFunction("cidr_masklen", 1,
		func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			netStr, ok := argString(args, 0)
			if !ok || netStr == "" {
				return nil, nil
			}
			prefix, err := netip.ParsePrefix(netStr)
			if err != nil {
				return nil, nil
			}
			return int64(prefix.Bits()), nil
		})

	// regexp_match(text, pattern) -> 1 / 0 / NULL
	//
	// PG 的 `col ~ 'regex'` 在 SQLite 里没有等价语法。schema 里只有一处用到
	// （agents.key 的格式校验），但那一处必须真的用正则才能保住语义 ——
	// 所以注册这条 UDF。模式非法时返回 NULL（不让脏模式打断整个查询）。
	sqlite.MustRegisterDeterministicScalarFunction("regexp_match", 2,
		func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			text, ok1 := argString(args, 0)
			pat, ok2 := argString(args, 1)
			if !ok1 || !ok2 {
				return nil, nil
			}
			re, err := compiledRegexp(pat)
			if err != nil {
				return nil, nil
			}
			if re.MatchString(text) {
				return int64(1), nil
			}
			return int64(0), nil
		})
}

// argString 把 driver.Value 读成字符串。
//
// SQLite 的值可能是 TEXT / INTEGER / REAL / NULL。UDF 的入参在 schema 与查询里
// 都是文本列，但 NULL 会以 nil 出现，必须显式拦掉（否则会被当成空串，
// 而空串与 NULL 在 cidr 判断里语义不同）。
func argString(args []driver.Value, i int) (string, bool) {
	if i >= len(args) {
		return "", false
	}
	switch v := args[i].(type) {
	case nil:
		return "", false
	case string:
		return v, true
	case []byte:
		return string(v), true
	default:
		// 数字等：按 fmt 的默认形态转（sqlite 会把 TEXT 列的值当字符串传，
		// 走不到这里；保留分支是为了不 panic）。
		return strings.TrimSpace(sprintValue(v)), true
	}
}

func sprintValue(v driver.Value) string {
	switch x := v.(type) {
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case bool:
		if x {
			return "1"
		}
		return "0"
	default:
		return ""
	}
}

func compiledRegexp(pat string) (*regexp.Regexp, error) {
	if v, ok := regexpCache.Load(pat); ok {
		return v.(*regexp.Regexp), nil
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return nil, err
	}
	regexpCache.Store(pat, re)
	return re, nil
}
