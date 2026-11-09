package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// jsonQuote 把路径安全地嵌进 JSON（Windows 反斜杠必须转义）。
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestDBPathPrecedence 验证库路径的三级优先级：
//
//	env ARTEX_DB_PATH > config.json 的 database.path > 默认 BaseDir()/data/quantex.db
//
// 与 PG 版的关键差异：**有**默认值。SQLite 空库起步是正常路径，
// 缺配置不该导致启动失败。
func TestDBPathPrecedence(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	t.Setenv("ARTEX_CONFIG", cfgPath)

	t.Run("配置文件里的绝对路径", func(t *testing.T) {
		abs := filepath.Join(dir, "from-config.db")
		os.WriteFile(cfgPath, []byte(`{"database":{"path":`+jsonQuote(abs)+`}}`), 0o644)
		t.Setenv("ARTEX_DB_PATH", "")
		got, src, err := DBPath()
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got != abs {
			t.Fatalf("got %q want %q", got, abs)
		}
		if src == "" {
			t.Fatal("source 不应为空")
		}
	})

	t.Run("环境变量覆盖配置文件", func(t *testing.T) {
		os.WriteFile(cfgPath, []byte(`{"database":{"path":"/from/config.db"}}`), 0o644)
		envAbs := filepath.Join(dir, "from-env.db")
		t.Setenv("ARTEX_DB_PATH", envAbs)
		got, src, err := DBPath()
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got != envAbs {
			t.Fatalf("env 应优先: got %q want %q", got, envAbs)
		}
		if src != "环境变量 ARTEX_DB_PATH" {
			t.Fatalf("source 不符: %q", src)
		}
	})

	t.Run("两者都没有时用默认路径", func(t *testing.T) {
		t.Setenv("ARTEX_DB_PATH", "")
		t.Setenv("ARTEX_CONFIG", filepath.Join(dir, "nope.json"))
		got, src, err := DBPath()
		if err != nil {
			t.Fatalf("缺配置不应报错（SQLite 有默认值）: %v", err)
		}
		if !filepath.IsAbs(got) {
			t.Fatalf("默认路径应是绝对路径: %q", got)
		}
		if filepath.Base(got) != DefaultDBFileName {
			t.Fatalf("默认文件名不符: %q", filepath.Base(got))
		}
		if src == "" {
			t.Fatal("source 不应为空")
		}
	})

	t.Run("相对路径锚定到 BaseDir", func(t *testing.T) {
		os.WriteFile(cfgPath, []byte(`{"database":{"path":"data/rel.db"}}`), 0o644)
		t.Setenv("ARTEX_CONFIG", cfgPath)
		t.Setenv("ARTEX_DB_PATH", "")
		got, _, err := DBPath()
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if !filepath.IsAbs(got) {
			t.Fatalf("相对路径应被解析成绝对路径（否则换目录启动会换库）: %q", got)
		}
		if filepath.Base(got) != "rel.db" {
			t.Fatalf("文件名不符: %q", got)
		}
	})
}

// TestRedactReturnsPath 记录 Redact 的语义变化：
// SQLite 路径没有密码可脱敏，原样返回。
func TestRedactReturnsPath(t *testing.T) {
	p := "/data/quantex.db"
	if got := Redact(p); got != p {
		t.Fatalf("Redact 应原样返回路径: got %q", got)
	}
}
