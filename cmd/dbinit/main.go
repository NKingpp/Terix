// 用 Open() 初始化一个 SQLite 库：执行 schema + seed。
//
// 为什么需要它：
//
// 「全新库能否跑起来」的完整含义包括 seed 成功。只验证"表建出来了"会漏掉
// "表在但空"这类问题 —— 它比"表不存在"隐蔽得多：查询不报错，只是返回空集，
// 闸门全部返回 EXEC_GATE_UNAVAILABLE。
//
// 所以脚本需要跑一次真正的 Open()，而不是直接灌 schema 文件。
//
// 用法：
//
//	ARTEX_DB_PATH="C:/data/quantex.db" go run ./cmd/dbinit
//	go run ./cmd/dbinit                  # 用默认位置
//
// 幂等：Open() 里的每一步都是幂等的，可以重复跑。
package main

import (
	"fmt"
	"os"

	"github.com/Autumn-27/artex/config"
	"github.com/Autumn-27/artex/db"
)

func main() {
	// ARTEX_DB_PATH 可选：不设时走 config.DBPath() 的默认位置（BaseDir/data）。
	if v := os.Getenv("ARTEX_DB_PATH"); v != "" {
		fmt.Printf("使用 ARTEX_DB_PATH=%s\n", v)
	}
	path, source, err := config.DBPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "解析库路径失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("库文件: %s（来源: %s）\n", path, source)

	d, err := db.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Open 失败: %v\n", err)
		os.Exit(1)
	}
	defer d.Close()

	// 报一下 seed 的结果，让调用方能确认「真的做了事」而不是「静默通过」。
	rules, err := d.CountBuiltinDraftRules()
	if err != nil {
		fmt.Fprintf(os.Stderr, "数内置规则失败: %v\n", err)
		os.Exit(1)
	}
	var agents, tables, indexes, triggers int
	if err := d.QueryRow(`SELECT COUNT(*) FROM agents WHERE builtin`).Scan(&agents); err != nil {
		fmt.Fprintf(os.Stderr, "数内置 agent 失败: %v\n", err)
		os.Exit(1)
	}
	// 库结构清点：表/索引/触发器数量是"schema 真的应用了"的直接证据。
	if err := d.QueryRow(`SELECT
		(SELECT COUNT(*) FROM sqlite_master WHERE type='table'    AND name NOT LIKE 'sqlite_%'),
		(SELECT COUNT(*) FROM sqlite_master WHERE type='index'    AND name NOT LIKE 'sqlite_%'),
		(SELECT COUNT(*) FROM sqlite_master WHERE type='trigger')`).Scan(&tables, &indexes, &triggers); err != nil {
		fmt.Fprintf(os.Stderr, "清点库结构失败: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Open 成功：表 %d / 索引 %d / 触发器 %d\n", tables, indexes, triggers)
	fmt.Printf("          内置 agent %d 个 / 内置草案规则 %d 条\n", agents, rules)
}
