package intercept

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// mustLoc 载入时区;失败即 Fatal(测试环境 tzdata 已内嵌)。
func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	l, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", name, err)
	}
	return l
}

func strptr(s string) *string { return &s }
func boolptr(b bool) *bool    { return &b }

// =====================================================================
// numeric_lte
// =====================================================================

func TestNumericLTE(t *testing.T) {
	cases := []struct {
		name      string
		max       string
		draft     *DraftSubject
		wantMatch bool
	}{
		{
			name: "恰好等于阈值算放行(<=不是<)",
			max:  "10000",
			draft: &DraftSubject{Qty: "10", RefPrice: "1000"},
			// 10*1000 = 10000 = max → 不超限 → 不命中
			wantMatch: false,
		},
		{
			name:      "超一分",
			max:       "9999",
			draft:     &DraftSubject{Qty: "10", RefPrice: "1000"},
			wantMatch: true,
		},
		{
			name: "小数精确 0.1×3=0.3 不超 0.3",
			max:  "0.3",
			// float64 会得 0.30000000000000004 > 0.3 → 误拦。big.Rat 必须精确。
			draft:     &DraftSubject{Qty: "0.1", RefPrice: "3"},
			wantMatch: false,
		},
		{
			name:      "ref_price 缺失(市价单)必须拦",
			max:       "10000",
			draft:     &DraftSubject{Qty: "10", RefPrice: ""},
			wantMatch: true,
		},
		{
			name:      "qty 非法必须拦",
			max:       "10000",
			draft:     &DraftSubject{Qty: "abc", RefPrice: "100"},
			wantMatch: true,
		},
		{
			name:      "调用方算好的 Notional 优先",
			max:       "100",
			draft:     &DraftSubject{Qty: "999", RefPrice: "999", Notional: "50"},
			wantMatch: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := MatcherParams{Field: "notional", Max: strptr(c.max)}
			res, err := matchNumericLTE(p, &Subject{Draft: c.draft})
			if err != nil {
				t.Fatalf("意外错误: %v", err)
			}
			if res.Matched != c.wantMatch {
				t.Errorf("Matched = %v, want %v (reason=%s)", res.Matched, c.wantMatch, res.Reason)
			}
			if res.Matched && res.Reason == "" {
				t.Error("命中时必须给出 reason")
			}
		})
	}

	// 配置错误 → error (fail-closed),不是 Matched=false。
	t.Run("max 缺失必须报错", func(t *testing.T) {
		if _, err := matchNumericLTE(MatcherParams{}, &Subject{Draft: &DraftSubject{Qty: "1"}}); err == nil {
			t.Error("max 缺失必须报错")
		}
	})
	t.Run("max 非法必须报错", func(t *testing.T) {
		p := MatcherParams{Max: strptr("abc")}
		if _, err := matchNumericLTE(p, &Subject{Draft: &DraftSubject{Qty: "1"}}); err == nil {
			t.Error("max 非法必须报错")
		}
	})
	t.Run("field 不支持必须报错", func(t *testing.T) {
		p := MatcherParams{Field: "volume", Max: strptr("1")}
		if _, err := matchNumericLTE(p, &Subject{Draft: &DraftSubject{Qty: "1"}}); err == nil {
			t.Error("field=volume 必须报错")
		}
	})
}

// TestNumericScientific 科学计数法必须被拒。
//
// big.Rat.SetString 接受 "1e3",风控不该接受 —— 金额写成科学计数法
// 要么是程序 bug,要么是刻意绕过上限比较。
func TestNumericScientific(t *testing.T) {
	bad := []string{"1e3", "1E3", "1.5e10", "-1e2", "1e-3", "0x10", "1_000"}
	for _, s := range bad {
		t.Run("max="+s, func(t *testing.T) {
			p := MatcherParams{Max: strptr(s)}
			_, err := matchNumericLTE(p, &Subject{Draft: &DraftSubject{Qty: "1", RefPrice: "1"}})
			if err == nil {
				t.Errorf("max=%q 必须被拒(科学计数法/非十进制)", s)
			}
		})
	}
	// 合法十进制必须通过,证明上面的拒绝没有误伤。
	good := []string{"1000", "1000.00", "-5", ".5", "0"}
	for _, s := range good {
		t.Run("ok max="+s, func(t *testing.T) {
			p := MatcherParams{Max: strptr(s)}
			if _, err := matchNumericLTE(p, &Subject{Draft: &DraftSubject{Qty: "1", RefPrice: "1"}}); err != nil {
				t.Errorf("max=%q 不应被拒: %v", s, err)
			}
		})
	}
}

// =====================================================================
// list
// =====================================================================

func TestList(t *testing.T) {
	cases := []struct {
		name          string
		values        []string
		mode          string
		caseSensitive *bool
		symbol        string
		wantMatch     bool
	}{
		{"大小写与空格归一化后命中白名单", []string{"spy", " QQQ "}, "include", nil, "SPY", false},
		{"不在白名单", []string{"QQQ"}, "include", nil, "SPY", true},
		{"空白名单必须拦", nil, "include", nil, "SPY", true},
		{"空数组字面量必须拦", []string{}, "include", nil, "SPY", true},
		{"exclude 命中则拦", []string{"SPY"}, "exclude", nil, "SPY", true},
		{"exclude 未命中则放", []string{"QQQ"}, "exclude", nil, "SPY", false},
		{"case_sensitive 显式区分", []string{"spy"}, "include", boolptr(true), "SPY", true},
		{"标为空必须拦", []string{"SPY"}, "include", nil, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := MatcherParams{Field: "symbol", Values: c.values, Mode: c.mode, CaseSensitive: c.caseSensitive}
			res, err := matchList(p, &Subject{Draft: &DraftSubject{Symbol: c.symbol}})
			if err != nil {
				t.Fatalf("意外错误: %v", err)
			}
			if res.Matched != c.wantMatch {
				t.Errorf("Matched = %v, want %v (reason=%s)", res.Matched, c.wantMatch, res.Reason)
			}
		})
	}

	t.Run("mode 未知必须报错", func(t *testing.T) {
		p := MatcherParams{Values: []string{"SPY"}, Mode: "allow"}
		if _, err := matchList(p, &Subject{Draft: &DraftSubject{Symbol: "SPY"}}); err == nil {
			t.Error("mode=allow 必须报错")
		}
	})
}

// =====================================================================
// time_window
// =====================================================================

// TestTimeWindowExplicit 覆盖 window 字符串路径的边界。
func TestTimeWindowExplicit(t *testing.T) {
	ny := mustLoc(t, "America/New_York")

	mkSubject := func(y, mo, d, h, mi, s int) *Subject {
		return &Subject{
			Now: time.Date(y, time.Month(mo), d, h, mi, s, 0, ny),
			Loc: ny,
		}
	}
	p := MatcherParams{Source: "window", Window: "09:30-16:00"}

	cases := []struct {
		name      string
		subj      *Subject
		wantMatch bool
	}{
		{"左边界 09:30:00 开盘可下单", mkSubject(2026, 3, 10, 9, 30, 0), false},
		{"右边界 16:00:00 收盘瞬间必须拦(右开)", mkSubject(2026, 3, 10, 16, 0, 0), true},
		{"15:59:59 仍在窗口内", mkSubject(2026, 3, 10, 15, 59, 59), false},
		{"09:29:59 开盘前", mkSubject(2026, 3, 10, 9, 29, 59), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := matchTimeWindow(p, c.subj)
			if err != nil {
				t.Fatalf("意外错误: %v", err)
			}
			if res.Matched != c.wantMatch {
				t.Errorf("Matched = %v, want %v", res.Matched, c.wantMatch)
			}
		})
	}

	t.Run("跨午夜窗口 23:00 在 22:00-02:00 内", func(t *testing.T) {
		res, err := matchTimeWindow(
			MatcherParams{Source: "window", Window: "22:00-02:00"},
			mkSubject(2026, 3, 10, 23, 0, 0))
		if err != nil || res.Matched {
			t.Errorf("应在窗口内: Matched=%v err=%v", res.Matched, err)
		}
	})
	t.Run("跨午夜窗口 03:00 在外", func(t *testing.T) {
		res, err := matchTimeWindow(
			MatcherParams{Source: "window", Window: "22:00-02:00"},
			mkSubject(2026, 3, 10, 3, 0, 0))
		if err != nil || !res.Matched {
			t.Errorf("应在窗口外: Matched=%v err=%v", res.Matched, err)
		}
	})
	t.Run("两段式窗口 12:00 午休必须拦", func(t *testing.T) {
		sh := mustLoc(t, "Asia/Shanghai")
		subj := &Subject{Now: time.Date(2026, 3, 10, 12, 0, 0, 0, sh), Loc: sh}
		res, err := matchTimeWindow(
			MatcherParams{Source: "window", Window: "09:30-11:30,13:00-15:00"}, subj)
		if err != nil || !res.Matched {
			t.Errorf("午休应拦: Matched=%v err=%v", res.Matched, err)
		}
	})
	t.Run("days 过滤:周六必须拦", func(t *testing.T) {
		// 2026-03-14 是周六。
		res, err := matchTimeWindow(
			MatcherParams{Source: "window", Window: "09:30-16:00", Days: []int{1, 2, 3, 4, 5}},
			mkSubject(2026, 3, 14, 10, 0, 0))
		if err != nil || !res.Matched {
			t.Errorf("周六应拦: Matched=%v err=%v", res.Matched, err)
		}
	})
	t.Run("Loc 为 nil 必须报错", func(t *testing.T) {
		_, err := matchTimeWindow(p, &Subject{Now: time.Now()})
		if err == nil {
			t.Error("Loc=nil 必须报错(禁止默认 UTC)")
		}
	})
	t.Run("window 为空必须报错", func(t *testing.T) {
		_, err := matchTimeWindow(MatcherParams{Source: "window"}, mkSubject(2026, 3, 10, 10, 0, 0))
		if !errors.Is(err, ErrEmptyWin) {
			t.Errorf("expected ErrEmptyWin, got %v", err)
		}
	})
	t.Run("source 未知必须报错", func(t *testing.T) {
		_, err := matchTimeWindow(MatcherParams{Source: "always_open"}, mkSubject(2026, 3, 10, 10, 0, 0))
		if !errors.Is(err, ErrBadSource) {
			t.Errorf("expected ErrBadSource, got %v", err)
		}
	})
}

// TestTimeWindowMarketSession 覆盖 source=market_session 路径。
func TestTimeWindowMarketSession(t *testing.T) {
	sh := mustLoc(t, "Asia/Shanghai")
	// 假 Session:两段式,模拟 A 股。
	fakeSession := func(market string, d time.Time, loc *time.Location) ([]Session, error) {
		base := d.In(loc)
		y, m, dd := base.Date()
		return []Session{
			{Open: time.Date(y, m, dd, 9, 30, 0, 0, loc), Close: time.Date(y, m, dd, 11, 30, 0, 0, loc)},
			{Open: time.Date(y, m, dd, 13, 0, 0, 0, loc), Close: time.Date(y, m, dd, 15, 0, 0, 0, loc)},
		}, nil
	}
	mkSubj := func(h, mi int) *Subject {
		return &Subject{
			Now:     time.Date(2026, 3, 10, h, mi, 0, 0, sh),
			Loc:     sh,
			Session: fakeSession,
			Draft:   &DraftSubject{Market: "cn"},
		}
	}
	p := MatcherParams{Source: "market_session"}

	t.Run("10:00 上午段内放行", func(t *testing.T) {
		res, err := matchTimeWindow(p, mkSubj(10, 0))
		if err != nil || res.Matched {
			t.Errorf("Matched=%v err=%v", res.Matched, err)
		}
	})
	t.Run("12:00 午休必须拦(单段实现会误放行)", func(t *testing.T) {
		res, err := matchTimeWindow(p, mkSubj(12, 0))
		if err != nil || !res.Matched {
			t.Errorf("午休应拦: Matched=%v err=%v", res.Matched, err)
		}
	})
	t.Run("14:00 下午段内放行", func(t *testing.T) {
		res, err := matchTimeWindow(p, mkSubj(14, 0))
		if err != nil || res.Matched {
			t.Errorf("Matched=%v err=%v", res.Matched, err)
		}
	})
	t.Run("Session 为 nil 必须报错", func(t *testing.T) {
		subj := mkSubj(10, 0)
		subj.Session = nil
		if _, err := matchTimeWindow(p, subj); !errors.Is(err, ErrNoSession) {
			t.Errorf("expected ErrNoSession, got %v", err)
		}
	})
	t.Run("market 为空必须报错", func(t *testing.T) {
		subj := mkSubj(10, 0)
		subj.Draft = &DraftSubject{}
		if _, err := matchTimeWindow(p, subj); err == nil {
			t.Error("market 为空必须报错")
		}
	})
}

// =====================================================================
// daily_count
// =====================================================================

func TestDailyCount(t *testing.T) {
	ny := mustLoc(t, "America/New_York")

	mk := func(n int64, countErr error, y, mo, d int) *Subject {
		return &Subject{
			Now: time.Date(y, time.Month(mo), d, 12, 0, 0, 0, ny),
			Loc: ny,
			Count: func(ctx context.Context, q CountQuery) (int64, error) {
				return n, countErr
			},
			Draft: &DraftSubject{InstrumentID: 1, Symbol: "SPY", Market: "us"},
		}
	}
	max := "3"

	t.Run("count=2 < max=3 放行", func(t *testing.T) {
		res, err := matchDailyCount(context.Background(), MatcherParams{Max: strptr(max)}, mk(2, nil, 2026, 3, 10))
		if err != nil || res.Matched {
			t.Errorf("Matched=%v err=%v", res.Matched, err)
		}
	})
	t.Run("count=3 == max=3 必须拦(>=不是>)", func(t *testing.T) {
		res, err := matchDailyCount(context.Background(), MatcherParams{Max: strptr(max)}, mk(3, nil, 2026, 3, 10))
		if err != nil || !res.Matched {
			t.Errorf("Matched=%v err=%v", res.Matched, err)
		}
	})
	t.Run("count=4 > max 拦", func(t *testing.T) {
		res, err := matchDailyCount(context.Background(), MatcherParams{Max: strptr(max)}, mk(4, nil, 2026, 3, 10))
		if err != nil || !res.Matched {
			t.Errorf("Matched=%v err=%v", res.Matched, err)
		}
	})
	t.Run("Count 为 nil 必须报错", func(t *testing.T) {
		subj := mk(0, nil, 2026, 3, 10)
		subj.Count = nil
		if _, err := matchDailyCount(context.Background(), MatcherParams{Max: strptr(max)}, subj); !errors.Is(err, errNoCount) {
			t.Errorf("expected errNoCount, got %v", err)
		}
	})
	t.Run("计数查询失败必须拦", func(t *testing.T) {
		res, err := matchDailyCount(context.Background(), MatcherParams{Max: strptr(max)},
			mk(0, errors.New("db down"), 2026, 3, 10))
		if err != nil {
			t.Fatalf("计数失败不应是 matcher error: %v", err)
		}
		if !res.Matched {
			t.Error("查不到次数时不能放行")
		}
	})
	t.Run("max 非整数必须报错", func(t *testing.T) {
		if _, err := matchDailyCount(context.Background(), MatcherParams{Max: strptr("3.5")}, mk(0, nil, 2026, 3, 10)); err == nil {
			t.Error("max=3.5 必须报错")
		}
	})
	t.Run("max<=0 必须报错", func(t *testing.T) {
		for _, m := range []string{"0", "-1"} {
			if _, err := matchDailyCount(context.Background(), MatcherParams{Max: strptr(m)}, mk(0, nil, 2026, 3, 10)); err == nil {
				t.Errorf("max=%s 必须报错", m)
			}
		}
	})
}

// TestDailyCountDST23h 验证 DST 日「今日」边界用 AddDate 而非 +24h。
//
// 用 CountQuery 回调把 SinceIncl/UntilExcl 抓出来断言跨度:
//   - 2026-03-08 春季拨快 → 23 小时
//   - 2026-11-01 秋季拨慢 → 25 小时
//
// 用 +24h 会算错边界,漏掉或多算 1 小时的下单。
func TestDailyCountDST23h(t *testing.T) {
	ny := mustLoc(t, "America/New_York")
	max := "10"

	cases := []struct {
		name string
		day  time.Time
		want time.Duration
	}{
		{"春季拨快 23h", time.Date(2026, 3, 8, 12, 0, 0, 0, ny), 23 * time.Hour},
		{"秋季拨慢 25h", time.Date(2026, 11, 1, 12, 0, 0, 0, ny), 25 * time.Hour},
		{"平常日 24h", time.Date(2026, 6, 10, 12, 0, 0, 0, ny), 24 * time.Hour},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got CountQuery
			subj := &Subject{
				Now: c.day,
				Loc: ny,
				Count: func(ctx context.Context, q CountQuery) (int64, error) {
					got = q
					return 0, nil
				},
				Draft: &DraftSubject{Symbol: "SPY"},
			}
			if _, err := matchDailyCount(context.Background(), MatcherParams{Max: strptr(max)}, subj); err != nil {
				t.Fatalf("意外错误: %v", err)
			}
			span := got.UntilExcl.Sub(got.SinceIncl)
			if span != c.want {
				t.Errorf("今日跨度 = %v, want %v(用 +24h 就会错)", span, c.want)
			}
			// 边界必须是当地 00:00。
			if got.SinceIncl.Hour() != 0 || got.SinceIncl.Minute() != 0 {
				t.Errorf("SinceIncl 不是当地午夜: %v", got.SinceIncl)
			}
			// 默认排除被取代的草案。
			if !got.ExcludeSuper {
				t.Error("默认应 ExcludeSuper=true")
			}
		})
	}
}

// =====================================================================
// EvaluateDraft fail-closed
// =====================================================================

// TestEvaluateDraftFailClosed 是 G11 的核心用例:三种失败路径全部 deny。
func TestEvaluateDraftFailClosed(t *testing.T) {
	t.Run("规则加载失败 → deny", func(t *testing.T) {
		// 已关闭的 DB:Query 稳定返回 sql.ErrConnDone,不 panic。
		d, err := dbOpenForTest(t)
		if err != nil {
			t.Skipf("postgres unavailable (%v)", err)
		}
		d.Close()
		i := &Interceptor{db: d}
		dec, err := i.EvaluateDraft(context.Background(), &DraftSubject{Symbol: "SPY"})
		if dec.Action != "deny" {
			t.Errorf("action = %q, want deny", dec.Action)
		}
		if err == nil {
			t.Error("规则加载失败必须返回 error")
		}
	})

	t.Run("零条草案规则 → deny", func(t *testing.T) {
		i := &Interceptor{}
		i.cached = []compiledRule{} // 空规则集
		dec, err := i.EvaluateDraft(context.Background(), &DraftSubject{Symbol: "SPY"})
		if dec.Action != "deny" {
			t.Errorf("action = %q, want deny", dec.Action)
		}
		if !errors.Is(err, ErrNoDraftRules) {
			t.Errorf("expected ErrNoDraftRules, got %v", err)
		}
	})

	t.Run("只有工具规则、无草案规则 → deny", func(t *testing.T) {
		i := &Interceptor{}
		r := compiledRule{}
		r.MatchTarget = "tool_input"
		r.MatchType = "string"
		r.Pattern = "rm -rf"
		r.Enabled = true
		i.cached = []compiledRule{r}
		dec, err := i.EvaluateDraft(context.Background(), &DraftSubject{Symbol: "SPY"})
		if dec.Action != "deny" {
			t.Errorf("action = %q, want deny", dec.Action)
		}
		if !errors.Is(err, ErrNoDraftRules) {
			t.Errorf("expected ErrNoDraftRules, got %v", err)
		}
	})

	t.Run("matcher 报错 → deny", func(t *testing.T) {
		i := &Interceptor{}
		r := compiledRule{}
		r.ID = 1
		r.Name = "bad-config"
		r.MatchTarget = "draft_field"
		r.MatchType = "numeric_lte"
		r.PatternParams = []byte(`{}`) // 缺 max → matcher 报错
		r.Action = "deny"
		r.Enabled = true
		i.cached = []compiledRule{r}
		dec, err := i.EvaluateDraft(context.Background(), &DraftSubject{Symbol: "SPY", Qty: "1"})
		if dec.Action != "deny" {
			t.Errorf("action = %q, want deny", dec.Action)
		}
		if err == nil {
			t.Error("matcher 报错必须向上冒泡")
		}
	})

	t.Run("全部放行才 pass", func(t *testing.T) {
		i := &Interceptor{}
		r := compiledRule{}
		r.ID = 1
		r.Name = "allow-spy"
		r.MatchTarget = "draft_field"
		r.MatchType = "list"
		r.PatternParams = []byte(`{"values":["SPY"],"mode":"include"}`)
		r.Action = "deny"
		r.Enabled = true
		i.cached = []compiledRule{r}
		dec, err := i.EvaluateDraft(context.Background(), &DraftSubject{Symbol: "SPY"})
		if err != nil {
			t.Fatalf("意外错误: %v", err)
		}
		if dec.Action != "allow" {
			t.Errorf("action = %q, want allow", dec.Action)
		}
	})

	t.Run("deny 短路", func(t *testing.T) {
		i := &Interceptor{}
		r1 := compiledRule{}
		r1.ID, r1.Name = 1, "deny-first"
		r1.MatchTarget = "draft_field"
		r1.MatchType = "list"
		r1.PatternParams = []byte(`{"values":["QQQ"]}`) // SPY 不在 → 命中 → deny
		r1.Action = "deny"
		r1.Enabled = true
		i.cached = []compiledRule{r1}
		dec, _ := i.EvaluateDraft(context.Background(), &DraftSubject{Symbol: "SPY"})
		if dec.Action != "deny" || dec.RuleName != "deny-first" {
			t.Errorf("dec = %+v", dec)
		}
	})

	t.Run("ask 不短路但最终返回 ask", func(t *testing.T) {
		i := &Interceptor{}
		r := compiledRule{}
		r.ID, r.Name = 1, "ask-rule"
		r.MatchTarget = "draft_field"
		r.MatchType = "list"
		r.PatternParams = []byte(`{"values":["QQQ"]}`) // 命中
		r.Action = "ask"
		r.Enabled = true
		i.cached = []compiledRule{r}
		dec, err := i.EvaluateDraft(context.Background(), &DraftSubject{Symbol: "SPY"})
		if err != nil {
			t.Fatalf("意外错误: %v", err)
		}
		if dec.Action != "ask" {
			t.Errorf("action = %q, want ask", dec.Action)
		}
	})
}

// 防止编译期 unused 报错的锚点(truthy 辅助函数在别处也会用到)。
var _ = strings.TrimSpace
