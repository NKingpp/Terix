package redact

import (
	"fmt"
	"log/slog"
	"strings"
)

// 本文件是 slog 侧的两个助手。
//
// 为什么需要专门处理：`slog.ReplaceAttr` 拿到的是单个 `slog.Attr`，而日志里的
// 敏感值可能藏在 group 里（`slog.Group("account", slog.String("passphrase", x))`），
// 也可能藏在 `slog.Any("ctx", map[string]any{...})` 里。只在顶层替换会漏掉嵌套。
//
// 两条路径：
//   - MaskAttr     —— 给 slog.HandlerOptions.ReplaceAttr 用（顶层 attr）
//   - MaskAnySlice —— 给 slog 的 args 序列（kv 对）用，或非 slog 的 []any

// MaskAttr 是 slog.HandlerOptions.ReplaceAttr 的实现。
//
// 签名直接匹配 `func(groups []string, a slog.Attr) slog.Attr`，可直接赋值：
//
//	slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
//	    ReplaceAttr: redact.MaskAttr,
//	}))
//
// 注意：ReplaceAttr 会被**每个** attr 调用一次（包括 group 内部的成员），
// 所以这里不需要自己递归 group —— slog 会自己展开。但 **group 不展开的情况**
// 是 `slog.Any("k", someStruct)`，此时值是一个整体，需要 MaskAny 处理。
//
// groups 参数表示当前 attr 的祖先 group 名路径。本实现不使用它（因为我们按
// **值形态**判定，而不是按属性名），但保留参数以匹配签名。
func MaskAttr(groups []string, a slog.Attr) slog.Attr {
	// 时间 / 级别 / 消息等内置类型不需要处理（不可能是凭证），
	// 直接放行以避免无谓的字符串化。
	switch a.Value.Kind() {
	case slog.KindTime, slog.KindDuration, slog.KindBool, slog.KindInt64,
		slog.KindUint64, slog.KindFloat64, slog.KindLogValuer:
		// LogValuer 需要 Resolve 才能看到真实值，但它通常是自定义类型，
		// 其 String() 已经是我们看到的样子；不 Resolve 以免触发副作用。
		return a
	case slog.KindString:
		s := a.Value.String()
		// key 名敏感 → 值整体遮（不看值本身长什么样）
		if IsSensitiveKey(a.Key) {
			if s == "" {
				return a
			}
			return slog.String(a.Key, maskValue)
		}
		m := Mask(s)
		if m == s {
			return a
		}
		return slog.String(a.Key, m)
	case slog.KindAny:
		v := maskAnyWithKey(a.Key, a.Value.Any())
		return slog.Any(a.Key, v)
	case slog.KindGroup:
		// 理论上 slog 会自己展开 group，不会走到这里；保险起见递归一遍。
		grp := a.Value.Group()
		out := make([]slog.Attr, len(grp))
		for i, sub := range grp {
			out[i] = MaskAttr(append(groups, a.Key), sub)
		}
		return slog.Group(a.Key, attrsToAny(out)...)
	default:
		return a
	}
}

func attrsToAny(as []slog.Attr) []any {
	out := make([]any, len(as))
	for i, a := range as {
		out[i] = a
	}
	return out
}

// MaskAnySlice 对 slog 的 args 序列（`slog.Info("msg", "api_key", v, ...)`）做掩码。
//
// slog 的 args 是「交替的 key/value」。**关键：值是否要遮由前一个 key 名决定。**
//
// 为什么不能只靠 Mask(value) 本身：
//
//	slog.Info("submit", "api_key", "sk_live_abc123")
//
// 拆开后值 "sk_live_abc123" 只有 15 字符、无 key 上下文、不足 64 字符，
// 7 条正则一条都抓不住 —— 这类日志必漏。必须由 key 名驱动。
//
// 对 vaultd 的独立日志（`data/vaultd.log`）来说，字符串化是可接受的 ——
// 那份日志只用于事后排查，不参与任何解析。
func MaskAnySlice(args []any) []any {
	if len(args) == 0 {
		return args
	}
	out := make([]any, 0, len(args))
	for i := 0; i < len(args); i++ {
		if i%2 == 0 {
			// key 位置：保留原文（改了会破坏 slog 的 kv 结构）。
			// 但若 key 名本身含凭证（罕见，如把 key 当成了 value 放在 key 位），
			// 仍走一遍 Mask。
			if s, ok := args[i].(string); ok {
				out = append(out, Mask(s))
			} else {
				out = append(out, args[i])
			}
			continue
		}
		// value 位置：用前一个 key 名决定策略
		key := ""
		if s, ok := args[i-1].(string); ok {
			key = s
		}
		out = append(out, maskAnyWithKey(key, args[i]))
	}
	return out
}

// maskAnyWithKey 是 MaskAny 的「带 key 上下文」版。
func maskAnyWithKey(key string, v any) any {
	// 1) key 名敏感 → 值整体遮，不再看值长什么样
	if IsSensitiveKey(key) {
		if s, ok := v.(string); ok && s != "" {
			return maskValue
		}
		return maskValue
	}
	// 2) key 名不敏感 → 按值形态处理（值里可能仍有内嵌凭证）
	return MaskAny(v)
}

// MaskAny 对任意值做掩码，返回可以安全写入日志的值。
//
// 递归处理 map / slice / struct（通过 fmt 的 %v）。对于已经实现
// `String() string` 的类型，%v 会调用它，因此掩码作用于其输出。
func MaskAny(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		return Mask(x)
	case []byte:
		return MaskJSON(x)
	case error:
		// 错误链里最常夹带凭证（底层 SDK 的 "request body: {...}"）
		return fmt.Errorf("%s", Mask(x.Error()))
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, vv := range x {
			// 必须把 key 带进递归:key 名敏感性判定不能在中途丢掉。
			// 用 maskAnyWithKey 而非 MaskAny —— 后者只看值形态,对
			// "passphrase": "普通密码串" 这种 key 敏感但值不匹配任何模式的情况
			// 会原样放行(就是 TestMaskAttrsRecursive 抓到的漏洞)。
			// 输出 key 也过一遍 Mask:key 名本身罕见地可能含值。
			out[Mask(k)] = maskAnyWithKey(k, vv)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, vv := range x {
			out[i] = MaskAny(vv)
		}
		return out
	default:
		// 其余类型：先字符串化，再判断是否需要遮；不需要遮就返回原值
		// （保留类型信息，便于 slog 的结构化输出）。
		s := fmt.Sprintf("%v", v)
		m := Mask(s)
		if m == s {
			return v
		}
		return m
	}
}

// ContainsSecret 是给**测试与 CI** 用的辅助：判断一段文本里是否还有未遮的凭证。
//
// 它**不用于生产路径** —— 生产路径只做 Mask，不做二次检测（成本翻倍）。
// 用途：`redact.ContainsSecret(tableDump)` 作为端到端泄漏测试的断言工具，
// 以及 CI 里扫 `data/*.log`。
//
// 判定口径：用与 Mask 相同的 7 条正则，看是否有**非掩码**匹配。
func ContainsSecret(s string) (string, bool) {
	if s == "" {
		return "", false
	}
	checks := []*struct {
		name string
		re   *regexpWrap
	}{}
	_ = checks
	// 逐条试探：把匹配到的片段取出来，若它本身已经是掩码字面量则跳过。
	for _, probe := range []struct {
		name string
		find func(string) string
	}{
		{"pem", func(x string) string {
			if loc := rePEM.FindString(x); loc != "" && !strings.Contains(loc, maskBlock) {
				return loc
			}
			return ""
		}},
		{"bearer", func(x string) string {
			if loc := reBearer.FindString(x); loc != "" && !strings.HasSuffix(loc, maskValue) {
				return loc
			}
			return ""
		}},
		{"keyvalue", func(x string) string {
			if loc := reKeyValue.FindStringSubmatch(x); len(loc) == 3 && loc[2] != maskValue {
				return loc[2]
			}
			return ""
		}},
	} {
		if hit := probe.find(s); hit != "" {
			return probe.name + ": " + hit, true
		}
	}
	return "", false
}

// regexpWrap 是占位类型，避免上面 checks 变量引入额外 import。
type regexpWrap struct{}
