package db

// JSON 列的 Scan 辅助（SQLite 版）。
//
// # 为什么必须有它
//
// `json.RawMessage` 的底层类型是 `[]byte`，但 database/sql 的默认转换规则里
// **没有** `string → *[]byte` 这一条（它只认 `[]byte` 与 `nil`）。SQLite 的
// JSON 列存的是 TEXT，驱动给出的是 `string`，于是：
//
//	sql: Scan error on column index 3, name "config":
//	unsupported Scan, storing driver.Value type string into type *json.RawMessage
//
// PG 版不会踩到：pgx 对 JSONB 列直接返回 `[]byte`。
//
// 调用点写法：`&c.Config` → `jsonScan{&c.Config}`。

import (
	"encoding/json"
	"fmt"
)

// jsonScan 把 TEXT 列扫进 *json.RawMessage。NULL 视为空（nil），
// 调用方原有的 `if len(x) == 0 { x = ... }` 兜底逻辑继续有效。
type jsonScan struct{ dst *json.RawMessage }

func (s jsonScan) Scan(v any) error {
	raw, ok, err := parseScanJSON(v)
	if err != nil {
		return err
	}
	if !ok {
		*s.dst = nil
		return nil
	}
	*s.dst = raw
	return nil
}

// jsonNullScan 是给 `**json.RawMessage` 用的版本（NULL 与空串都映射为 nil）。
type jsonNullScan struct{ dst **json.RawMessage }

func (s jsonNullScan) Scan(v any) error {
	raw, ok, err := parseScanJSON(v)
	if err != nil {
		return err
	}
	if !ok {
		*s.dst = nil
		return nil
	}
	*s.dst = &raw
	return nil
}

// parseScanJSON 把驱动给的值转成 json.RawMessage。
//
// 不校验 JSON 合法性：列里可能存着历史脏数据（PG 的 jsonb 保证合法，但手工
// UPDATE 过的行未必），校验会让读取失败。与 PG 行为对齐 —— 后者的 JSON 语法
// 错误在写入时就该被拦下，读侧不再重复判定。
func parseScanJSON(v any) (json.RawMessage, bool, error) {
	switch x := v.(type) {
	case nil:
		return nil, false, nil
	case string:
		if x == "" {
			return nil, false, nil
		}
		return json.RawMessage(x), true, nil
	case []byte:
		if len(x) == 0 {
			return nil, false, nil
		}
		// 必须拷贝：驱动会复用底层缓冲，直接持有会让后续 Next() 覆盖已读值。
		out := make(json.RawMessage, len(x))
		copy(out, x)
		return out, true, nil
	default:
		return nil, false, fmt.Errorf("无法把 %T 当作 JSON 文本读取", v)
	}
}
