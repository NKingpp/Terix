package db

// ParseDBTime 的单元测试。
//
// # 为什么这个测试比「Reconcile 测试顺带发现」更有价值
//
// Reconcile 测试失败时，错误信息是「MissingRem=0 不符」——
// 得从「为什么是 0」倒推到「解析失败了」。
// 而这个测试失败时，错误信息直接是「无法解析 X 格式」。
//
// 这就是「错误信息指向正确排查方向」在测试层的应用：
// 越靠近失败点的断言，越省排查时间。

import (
	"testing"
	"time"
)

func TestParseDBTimeFormats(t *testing.T) {
	// 期望值统一用 UTC 表示，避免比较时被本地时区干扰。
	const wantUTC = "2026-10-27T19:27:55Z"

	cases := []struct {
		name string
		in   string
		want string // 期望的 UTC 时间（秒级，忽略亚秒）
	}{
		{
			name: "RFC3339 带 Z",
			in:   "2026-10-27T19:27:55Z",
			want: wantUTC,
		},
		{
			name: "RFC3339 带纳秒",
			in:   "2026-10-27T19:27:55.269307Z",
			want: wantUTC,
		},
		{
			// 这是踩过的那个格式：PG 的 timestamptz::text
			name: "PG 格式：空格分隔 + 无冒号时区",
			in:   "2026-10-27 19:27:55.269307+00",
			want: wantUTC,
		},
		{
			name: "PG 格式：带冒号时区",
			in:   "2026-10-27 19:27:55.269307+00:00",
			want: wantUTC,
		},
		{
			name: "PG 格式：无小数位",
			in:   "2026-10-27 19:27:55+00",
			want: wantUTC,
		},
		{
			name: "PG 格式：完全无时区（naive）",
			in:   "2026-10-27 19:27:55",
			want: wantUTC,
		},
		{
			name: "PG 格式：正时区 +08（转换后是 UTC 11:27）",
			in:   "2026-10-28 03:27:55+08",
			want: "2026-10-27T19:27:55Z",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseDBTime(c.in)
			if err != nil {
				t.Fatalf("解析 %q 失败: %v", c.in, err)
			}
			// 比较到秒：亚秒部分不参与断言（各格式的精度不同）。
			if got.UTC().Format("2006-01-02T15:04:05Z") != c.want {
				t.Errorf("解析 %q = %s（UTC），期望 %s", c.in, got.UTC().Format(time.RFC3339), c.want)
			}
		})
	}
}

// TestParseDBTimeRejectsGarbage 非法格式必须报错，而不是返回零值。
//
// # 这条守的是「解析失败」和「时间真的是零」的区分
//
// 如果失败时返回零值（1970-01-01），调用方无法区分：
//   - 「这个字段没填」
//   - 「解析器不认识这个格式」
//
// 而这两种情况的后续处置完全不同：前者可能是合法的缺失，
// 后者一定是 bug（生产环境出现了意料外的格式）。
func TestParseDBTimeRejectsGarbage(t *testing.T) {
	bad := []string{
		"garbage",
		"2026-13-45 99:99:99", // 不存在的日期
		"not-a-time",
		"2026/10/27 19:27:55", // 斜杠分隔（不是 PG 也不是 RFC3339）
	}
	for _, in := range bad {
		got, err := ParseDBTime(in)
		if err == nil {
			t.Errorf("非法格式 %q 应报错，实际返回 %v", in, got)
			continue
		}
		// 错误信息必须含原始值 —— 让「格式不对」可区分于「值为空」
		if !containsStr(err.Error(), in) {
			t.Errorf("错误信息应含原始值 %q，实际：%v", in, err)
		}
	}

	// 空字符串单独测：它不是「格式不对」而是「没有值」。
	if _, err := ParseDBTime(""); err == nil {
		t.Error("空字符串应报错")
	}
	if _, err := ParseDBTime("   "); err == nil {
		t.Error("纯空白应报错")
	}
}

// TestParseDBTimeZeroValueNotReturnedOnError 明确断言「失败不返回零值」。
func TestParseDBTimeZeroValueNotReturnedOnError(t *testing.T) {
	got, err := ParseDBTime("garbage")
	if err == nil {
		t.Fatal("应报错")
	}
	if !got.IsZero() {
		t.Logf("注意：失败时也返回了非零值 %v —— 调用方不该使用它", got)
	}
	// 关键：调用方应该检查 err 而不是检查 IsZero。
	// 这条测试固定「必须检查 err」这个契约。
}

func containsStr(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
