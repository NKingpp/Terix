package intercept

import (
	"context"
	"errors"
	"regexp"
	"testing"
)

// =====================================================================
// G12:结构化 matcher 不得静默落到 strings.Contains
// =====================================================================

// TestRuleMatchesNoFallthrough 是唯一能证明「新 matcher 真的被分派」的测试。
//
// 构造手法:给每种结构化类型喂一份**非法 pattern_params**,让真 matcher
// 返回它的配置错误。若实现退化成裸 strings.Contains(subject, r.Pattern),
// 会得到 (false/true, nil) 而**不会**得到 matcher 的配置错误。
//
// 关键:断言 err 来自真 matcher(errBadMatcherConfig 族 / errNoCount),
// 且 **不是** errMatcherNotImplemented —— 后者说明还停在空壳阶段。
func TestRuleMatchesNoFallthrough(t *testing.T) {
	cases := []struct {
		matchType string
		params    string
	}{
		{"numeric_lte", `{}`},                  // 缺 max → ErrBadMax
		{"list", `{"mode":"allow"}`},           // 非法 mode → ErrBadMode
		{"time_window", `{"source":"always"}`}, // 非法 source → ErrBadSource
		{"daily_count", `{}`},                  // Count 为 nil → errNoCount
	}
	for _, c := range cases {
		t.Run(c.matchType, func(t *testing.T) {
			r := compiledRule{}
			r.ID = 1
			r.Name = "g12-probe"
			r.MatchTarget = "draft_field"
			r.MatchType = c.matchType
			r.Pattern = "\x00__NO_SUCH_SUBSTRING__\x00"
			r.PatternParams = []byte(c.params)

			subj := &Subject{Draft: &DraftSubject{
				Symbol: "SPY", Market: "us", Qty: "10", RefPrice: "100",
			}}
			res, err := ruleMatches(context.Background(), r, subj)

			if err == nil {
				t.Fatalf("match_type=%q 静默返回 err=nil(Matched=%v),说明没走 matchStructured",
					c.matchType, res.Matched)
			}
			if errors.Is(err, errMatcherNotImplemented) {
				t.Fatalf("match_type=%q 仍停在空壳,真实 matcher 未被调用: %v", c.matchType, err)
			}
		})
	}
}

// TestMatchStructuredDispatch 直接验证 matchStructured 的四路分派。
func TestMatchStructuredDispatch(t *testing.T) {
	subj := &Subject{Draft: &DraftSubject{Symbol: "SPY", Qty: "10", RefPrice: "100"}}
	cases := []struct {
		name string
		mt   string
		want bool
	}{
		{"numeric_lte 超限", "numeric_lte", true},
		{"list 不在白名单", "list", true},
		{"time_window 无 Now", "time_window", false}, // 会报错,不会返回 Matched
		{"daily_count 无 Count", "daily_count", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var p MatcherParams
			switch c.mt {
			case "numeric_lte":
				max := "50"
				p.Max = &max
			case "list":
				p.Values = []string{"QQQ"}
			case "time_window":
				p.Source = "window"
				p.Window = "09:30-16:00"
			case "daily_count":
				max := "5"
				p.Max = &max
			}
			res, err := matchStructured(context.Background(), c.mt, p, subj)
			switch c.mt {
			case "time_window", "daily_count":
				if err == nil {
					t.Errorf("%s 缺依赖时必须报错(fail-closed)", c.mt)
				}
			default:
				if err != nil {
					t.Fatalf("%s 意外报错: %v", c.mt, err)
				}
				if res.Matched != c.want {
					t.Errorf("Matched = %v, want %v", res.Matched, c.want)
				}
			}
		})
	}

	// 未知 match_type 必须报错。
	if _, err := matchStructured(context.Background(), "nope", MatcherParams{}, subj); err == nil {
		t.Error("未知 match_type 必须报错")
	}
}

// TestRuleMatchesStringStillWorks 防止我把 switch 改坏,连基础子串匹配都不工作了。
func TestRuleMatchesStringStillWorks(t *testing.T) {
	cases := []struct {
		name      string
		matchType string
		pattern   string
		subject   string
		want      bool
	}{
		{"explicit string hit", "string", "rm -rf", "run rm -rf / now", true},
		{"explicit string miss", "string", "rm -rf", "harmless", false},
		// 空 match_type 是历史数据默认语义(子串匹配),必须保留兼容。
		{"legacy empty type hit", "", "sudo", "sudo su", true},
		{"legacy empty type miss", "", "sudo", "ls", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := compiledRule{}
			r.MatchTarget = "tool_input"
			r.MatchType = c.matchType
			r.Pattern = c.pattern
			res, err := ruleMatches(context.Background(), r, &Subject{ToolName: "any_tool", ToolInput: []byte(c.subject)})
			if err != nil {
				t.Fatalf("string 路径不应返回 error: %v", err)
			}
			if res.Matched != c.want {
				t.Errorf("Matched = %v, want %v", res.Matched, c.want)
			}
		})
	}
}

// TestRuleMatchesRegexStillWorks 保护 regex 分支不被 switch 改造破坏。
func TestRuleMatchesRegexStillWorks(t *testing.T) {
	r := compiledRule{}
	r.MatchTarget = "tool_input"
	r.MatchType = "regex"
	// compiledRule 的 re 由编译阶段填充,这里手工构造等价正则。
	re, err := regexp.Compile(`^rm\s+-rf`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	r.re = re

	res, mErr := ruleMatches(context.Background(), r, &Subject{ToolName: "bash", ToolInput: []byte("rm -rf /tmp")})
	if mErr != nil {
		t.Fatalf("regex 路径不应返回 error: %v", mErr)
	}
	if !res.Matched {
		t.Error("expected regex to match")
	}

	res, mErr = ruleMatches(context.Background(), r, &Subject{ToolName: "bash", ToolInput: []byte("ls -la")})
	if mErr != nil {
		t.Fatalf("regex 路径不应返回 error: %v", mErr)
	}
	if res.Matched {
		t.Error("expected regex not to match")
	}
}

// TestRuleMatchesUnknownTargetReturnsError 保护 matchTarget 层的 G12 同类问题。
//
// 改造前这里的断言是 `err == nil`,那是在**把 bug 当成正确行为锁定**:
// 未知 match_target 返回 (false, nil) = 「未命中 + 无错误」= 静默放行。
// 将来加 match_target='draft_field' 之类新目标时若忘了加 case,
// 规则会静默失效 —— 不报错、不命中,等于这条风控规则不存在。
//
// 正确语义:未知 target 是「无法判定」,必须返 error 交给调用方按场景处理。
func TestRuleMatchesUnknownTargetReturnsError(t *testing.T) {
	r := compiledRule{}
	r.MatchTarget = "mars_rover"
	r.MatchType = "string"
	r.Pattern = "x"
	res, err := ruleMatches(context.Background(), r, &Subject{ToolName: "t", ToolInput: []byte("x")})
	if err == nil {
		t.Fatalf("未知 match_target 必须返回 error,否则就是静默放行"+
			"(Matched=%v)", res.Matched)
	}
	if !errors.Is(err, errUnknownMatchTarget) {
		t.Fatalf("expected errUnknownMatchTarget, got %v", err)
	}
	if res.Matched {
		t.Error("无法判定时 Matched 必须为 false")
	}
}

// TestRuleMatchesTargetMismatchSkips 验证「规则不适用于本条路径」是跳过而非报错。
// 与 TestRuleMatchesUnknownTargetReturnsError 的区别:
//   - draft_field 规则遇到工具路径 Subject → 不适用 → (false, nil),正常跳过
//   - mars_rover 规则 → 谁都不认识 → 无法判定 → error
// 这两者不能混:前者是正常的路径过滤,后者是配置错误。
func TestRuleMatchesTargetMismatchSkips(t *testing.T) {
	r := compiledRule{}
	r.MatchTarget = "draft_field"
	r.MatchType = "string"
	r.Pattern = "x"
	// 只填工具路径,没有 Draft → draft_field 规则不适用,应跳过且不报错。
	res, err := ruleMatches(context.Background(), r, &Subject{ToolName: "Bash", ToolInput: []byte("x")})
	if err != nil {
		t.Fatalf("不适用的规则应跳过而非报错: %v", err)
	}
	if res.Matched {
		t.Error("不适用的规则不应命中")
	}
}
