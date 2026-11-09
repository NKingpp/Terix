package db

// settings 是全局 key-value 配置（UI 可在运行时切换）。
//
// # 为什么有两套入口
//
// 生产路径用 *DB（走连接池、带 BUSY 重试）。但 seed 在 Open 的 schema 事务里跑，
// 必须用同一个 tx —— 否则会撞 schema 事务持有的写锁。所以同时提供 execer 版本
// （getSettingE / setSettingE），*DB 版本内部委托，逻辑只有一份。
//
// 缺 key 时 GetSetting 返回 ok=false，由调用方给默认值。

import "database/sql"

// GetSetting 返回存储的值；key 未设置时 ok=false。
func (d *DB) GetSetting(key string) (value string, ok bool, err error) {
	return getSettingE(d, key)
}

// SetSetting upsert 一个设置项。
func (d *DB) SetSetting(key, value string) error {
	return setSettingE(d, key, value)
}

// getSettingE 是 GetSetting 的 execer 版本（供 seed 在事务内调用）。
func getSettingE(d execer, key string) (value string, ok bool, err error) {
	err = d.QueryRow(`SELECT value FROM settings WHERE key=$1`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

// setSettingE 是 SetSetting 的 execer 版本。
func setSettingE(d execer, key, value string) error {
	_, err := d.Exec(`
INSERT INTO settings(key, value) VALUES ($1, $2)
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = `+SQLNow, key, value)
	return err
}

// InsertSettingIfAbsent 只在 key 尚未存在时写入，已存在则原样保留并返回 inserted=false。
//
// 给 auth.password_hash 这类"只允许首次设置"的键用：判定落在数据库的主键约束上，
// 调用方先 GetSetting 再写的那种检查就只是快速失败路径——读出错或并发撞车时，
// 已有的值也不会被 ON CONFLICT DO UPDATE 顺手覆盖掉。
func (d *DB) InsertSettingIfAbsent(key, value string) (inserted bool, err error) {
	res, err := d.Exec(`
INSERT INTO settings(key, value) VALUES ($1, $2)
ON CONFLICT (key) DO NOTHING`, key, value)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// GetBool returns the boolean setting, or def when unset/unparseable.
func (d *DB) GetBool(key string, def bool) bool {
	v, ok, err := d.GetSetting(key)
	if err != nil || !ok {
		return def
	}
	return v == "true" || v == "1"
}

// SetBool stores a boolean setting as "true"/"false".
func (d *DB) SetBool(key string, val bool) error {
	if val {
		return d.SetSetting(key, "true")
	}
	return d.SetSetting(key, "false")
}
