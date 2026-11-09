package db

// 数据库时间戳的解析（全项目唯一实现）。
//
// # 现状：时间列声明是 TIMESTAMP，Scan(&time.Time) 本来就可用
//
// schema 里时间列声明为 TIMESTAMP（见 tools_schema/gen_ddl.py 与 ARCH §2.2）。
// 驱动 modernc.org/sqlite 的 rows.go:189-203 只在 decltype 精确等于
// DATE/DATETIME/TIMESTAMP 时才把 TEXT 值交给 parseTime 转成 time.Time，
// 而 TIMESTAMP 正好命中，所以：
//
//	rows.Scan(&t.CreatedAt)                     // ✅ 直接用即可
//	rows.Scan(&nt)   // sql.NullTime            // ✅ 可直接用
//
// **正常代码不需要 timeScan / nullTimeScan。** 它们保留在这里只有两个用途：
//  1. 兼容历史遗留：某些列可能仍是 TEXT（如 traffic_evidence_snapshots.captured_at 是 BIGINT）。
//  2. 兜底：若将来驱动行为变化或某列误改回 TEXT，可作为过渡手段。
//
// 新代码请优先用 `&t.Field` / `sql.NullTime`，不要无谓套一层包装。
//
// 后半个 layout 列表保留 PG 形态（历史归档包里的时间串仍是 PG 格式，
// 见 db/task_archives_restore.go 的 json_populate_record 替代实现）。

import (
	"fmt"
	"strings"
	"time"
)

// pgTimeLayouts 覆盖 PG timestamptz::text 的所有常见形态。
//
// 小数位数不固定（.269307 / .2693 / 无小数），所以用 .999999999 一次覆盖；
// 时区可能是 +00 / +00:00 / 无时区。
var pgTimeLayouts = []string{
	"2006-01-02 15:04:05.999999999-07",
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02 15:04:05-07",
	"2006-01-02 15:04:05-07:00",
	"2006-01-02 15:04:05.999999999", // 无时区（PG 在 session TimeZone 下的 naive 输出）
	"2006-01-02 15:04:05",
}

// ParseDBTime 解析 PG 的时间戳文本（`xxx_at::text` 的输出）。
//
// 先试 RFC3339（有些路径/测试会给出标准格式），再逐个试 PG 的形态。
// 全部失败时返回错误 —— **不返回零值**：
// 零值（1970-01-01）会让「解析失败」和「时间真的是零」无法区分。
func ParseDBTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("空时间字符串")
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	for _, layout := range pgTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	// 带原始值：让「格式不对」在输出上可区分于「值为空」。
	return time.Time{}, fmt.Errorf("无法解析时间 %q（既不是 RFC3339 也不是 PG 格式）", s)
}

// ---------------------------------------------------------------------------
// Scan 辅助：TEXT 时间列 → time.Time / *time.Time
// ---------------------------------------------------------------------------

// timeScan 把 TEXT 时间列扫进 *time.Time。
//
// ⚠️ 当前 schema 的时间列声明是 TIMESTAMP，驱动已自动转换，**一般不需要它**。
// 只在列仍是 TEXT 的历史/异常场景用。正常写 `rows.Scan(&t.CreatedAt)` 即可。
//
// 保留理由与边界见文件头注释。
type timeScan struct{ dst *time.Time }

func (s timeScan) Scan(v any) error {
	t, ok, err := parseScanTime(v)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("timeScan: NULL 无法写入 *time.Time（该列应非空；可空列请用 nullTimeScan）")
	}
	*s.dst = t
	return nil
}

// nullTimeScan 同上，NULL 映射为 nil。
//
// 调用点写法：`&t.PinnedAt` → `nullTimeScan{&t.PinnedAt}`。
type nullTimeScan struct{ dst **time.Time }

func (s nullTimeScan) Scan(v any) error {
	t, ok, err := parseScanTime(v)
	if err != nil {
		return err
	}
	if !ok {
		*s.dst = nil
		return nil
	}
	*s.dst = &t
	return nil
}

// parseScanTime 把驱动给出的值转成 time.Time。
//
// 兼容四种形态：nil（NULL）、string/[]byte（TEXT 列）、time.Time（列声明成
// TIMESTAMP 时的驱动转换结果）、int64（历史 unix 秒）。
func parseScanTime(v any) (time.Time, bool, error) {
	switch x := v.(type) {
	case nil:
		return time.Time{}, false, nil
	case time.Time:
		return x.UTC(), true, nil
	case string:
		t, err := ParseDBTime(x)
		if err != nil {
			return time.Time{}, false, err
		}
		return t.UTC(), true, nil
	case []byte:
		t, err := ParseDBTime(string(x))
		if err != nil {
			return time.Time{}, false, err
		}
		return t.UTC(), true, nil
	case int64:
		return time.Unix(x, 0).UTC(), true, nil
	default:
		return time.Time{}, false, fmt.Errorf("无法把 %T 解析成时间", v)
	}
}
