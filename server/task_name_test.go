package server

// deriveTaskName 的单测。
//
// # 为什么这个函数需要测
//
// 它的输出会进 tasks.name（NOT NULL），而任务列表直接展示它。
// 三个易错点：① 空 goal 要回落到「未命名任务」② 中文截断要按 rune（按 byte 会出乱码）
// ③ 多行 goal 只取第一行。

import (
	"strings"
	"testing"
)

func TestDeriveTaskName(t *testing.T) {
	// 41 个汉字 = 123 字节。按 byte 截 40 会切在汉字中间，产生 U+FFFD。
	longCJK := strings.Repeat("判", 41)

	cases := []struct {
		name string
		goal string
		want string
	}{
		{"空 goal", "", "未命名任务"},
		{"纯空白", "   \n\t ", "未命名任务"},
		{"短 goal 原样返回", "判断 SPY 方向", "判断 SPY 方向"},
		{"多行只取第一行", "判断 SPY 方向\n补充说明：关注突破", "判断 SPY 方向"},
		{"首行带前后空白", "  判断 SPY 方向  \n第二行", "判断 SPY 方向"},
		{"恰好 40 个汉字不截断", strings.Repeat("判", 40), strings.Repeat("判", 40)},
		{"41 个汉字截断加省略号", longCJK, strings.Repeat("判", 40) + "…"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := deriveTaskName(c.goal)
			if got != c.want {
				t.Fatalf("deriveTaskName(%q) = %q, want %q", c.goal, got, c.want)
			}
			// 截断不能产生乱码 —— 这是「按 rune 而非 byte」的直接检验。
			if strings.ContainsRune(got, '\uFFFD') {
				t.Fatalf("结果含替换字符（截断切断了多字节字符）: %q", got)
			}
		})
	}
}
