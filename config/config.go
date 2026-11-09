// Package config loads runtime configuration from a JSON file, with environment
// variables taking precedence. Currently it carries the SQLite database path.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Database 是库文件配置。
//
// SQLite 的"连接"就是一个文件路径，没有 host/port/用户/密码。
// 相对路径会按 BaseDir() 解析（见 ResolveDBPath 的说明）。
type Database struct {
	Path string `json:"path"`
}

// Config is the on-disk config file shape.
type Config struct {
	Database Database `json:"database"`
	SkillDir string   `json:"skill_dir"`
}

// BaseDir is the directory that anchors all runtime artifacts (config.json and
// the data/ store). It is the directory the running binary lives in, so a
// distributed executable keeps its files next to itself on any OS (Windows,
// Linux, …) regardless of the working directory it is launched from.
//
// When launched via `go run`, the binary is throwaway: it sits either in a temp
// build dir (cache miss → fresh link) OR straight inside the Go build cache
// (cache hit → run from $GOCACHE/.../...-d). We detect both and fall back to the
// current working directory so dev artifacts (data/, transcripts) and config
// resolve against the project dir, not the throwaway binary's location.
func BaseDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	dir := filepath.Dir(exe)
	if isGoRunDir(dir) {
		return "." // throwaway `go run` binary → use CWD
	}
	return dir
}

// isGoRunDir reports whether dir is where `go run` parked its executable: under
// the system temp dir (cache miss), or anywhere inside a Go build cache
// (…/go-build/…, the cache-hit case — NOT under os.TempDir(), which is why the
// old temp-only check failed intermittently). In both cases the binary is
// throwaway, so config/data must resolve against the CWD.
func isGoRunDir(dir string) bool {
	if tmp := os.TempDir(); tmp != "" {
		if rel, err := filepath.Rel(tmp, dir); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	for _, seg := range strings.Split(filepath.ToSlash(dir), "/") {
		if seg == "go-build" {
			return true
		}
	}
	return false
}

// Path returns the config file path. Resolution order:
//  1. env ARTEX_CONFIG (explicit override)
//  2. ./config.json in the current working directory (running from the project
//     dir — robust no matter where `go run` placed the temp/cached binary)
//  3. config.json next to the executable (a distributed binary keeps it beside)
//
// The first existing file wins. If none exist, the CWD path is returned so the
// "not found" message points at the project dir the user most likely expected.
func Path() string {
	if v := strings.TrimSpace(os.Getenv("ARTEX_CONFIG")); v != "" {
		return v
	}
	var candidates []string
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(cwd, "config.json"))
	}
	candidates = append(candidates, filepath.Join(BaseDir(), "config.json"))
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return candidates[0]
}

// Load reads and parses the config file. A missing/unreadable file yields a zero
// Config (so callers fall back to defaults) rather than an error.
func Load() Config {
	var c Config
	b, err := os.ReadFile(Path())
	if err != nil {
		return c
	}
	_ = json.Unmarshal(b, &c)
	return c
}

// SkillDir returns the skill root directory with precedence:
//
//	env ARTEX_SKILL_DIR  >  config file (skill_dir)  >  BaseDir()/skills
//
// The directory is created if it does not exist.
func SkillDir() string {
	var d string
	if v := strings.TrimSpace(os.Getenv("ARTEX_SKILL_DIR")); v != "" {
		d = v
	} else if v := strings.TrimSpace(Load().SkillDir); v != "" {
		d = v
	} else {
		d = filepath.Join(BaseDir(), "skills")
	}
	_ = os.MkdirAll(d, 0o755)
	return d
}

// DefaultDBFileName 是 config 未指定路径时的库文件名（相对 BaseDir()/data）。
const DefaultDBFileName = "quantex.db"

// DBPath 解析 SQLite 库文件路径，优先级：
//
//	env ARTEX_DB_PATH > config file (database.path) > BaseDir()/data/quantex.db
//
// 与 PG 版的关键差异：这里**有**合理默认值。SQLite 空库起步是正常路径，
// 没有"必须配置才能启动"的理由；缺配置时用默认位置并照常启动。
//
// 相对路径按 BaseDir() 解析而不是 CWD：否则从不同目录启动会各自建一个库，
// 表现为"数据莫名消失"。
//
// source 说明路径从哪来，供启动日志使用。
func DBPath() (path, source string, err error) {
	if v := strings.TrimSpace(os.Getenv("ARTEX_DB_PATH")); v != "" {
		return resolveDBPath(v), "环境变量 ARTEX_DB_PATH", nil
	}
	if p := strings.TrimSpace(Load().Database.Path); p != "" {
		return resolveDBPath(p), "配置文件 " + Path() + " (database.path)", nil
	}
	return resolveDBPath(filepath.Join("data", DefaultDBFileName)), "默认位置（BaseDir/data）", nil
}

// resolveDBPath 把相对路径锚定到 BaseDir()，并保证返回绝对路径。
//
// 为什么最后还要 filepath.Abs：BaseDir() 在 `go run` 下故意返回 "."（二进制
// 是临时的，config/data 要相对项目目录解析）。若直接 Join(".", p)，拿到的
// 仍是相对路径 —— 一旦进程 CWD 变了，就会指向另一个库文件，表现为"数据消失"。
// Abs 把 "." 展开成真实 CWD，相对路径就此固定下来。
func resolveDBPath(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	joined := filepath.Join(BaseDir(), p)
	if abs, err := filepath.Abs(joined); err == nil {
		return abs
	}
	return joined
}

// Redact 返回用于日志的路径视图。
//
// 保留这个函数名是为了不动 db.Open 的错误文案。SQLite 路径没有密码可脱敏，
// 所以直接返回原值（函数存在只为语义清晰与未来扩展）。
func Redact(path string) string { return path }
