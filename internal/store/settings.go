package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// 设置键。面板能改的东西一律先在这里登记一个键名，
// 免得前端、后台页、调用方三家各写一个字符串，最后对不上还查不出来。
const (
	// KeyAuditRetentionDays 审计日志保留天数，供 PurgeAudit 用。
	KeyAuditRetentionDays = "audit_retention_days"
	// KeySnapshotRetentionDays 在线快照保留天数，供 PurgeSnapshots 用。
	KeySnapshotRetentionDays = "snapshot_retention_days"
	// KeyPlayerEventRetentionDays 玩家行为记录保留天数，供 PurgePlayerEvents 用。
	// 单独一个键而不是跟在线快照共用：一张是一分钟一行，一张是一天几十到几百行，
	// 想留多久是两件可以分开决定的事，而且真正吃磁盘的是前者。
	KeyPlayerEventRetentionDays = "player_event_retention_days"
	// KeyBackupKeep 是存档快照最多留几份。默认值来自配置文件 backup.keep，
	// 这里登记一个键是为了让人在界面里改完就生效，不用为改一个数字重启面板。
	KeyBackupKeep = "backup_keep"
	// KeyBackupScheduleHours 是定时备份的间隔小时数，0 表示不开。
	// 默认留 0：在一台 4G 内存的小机上，没人明确要求就定期往磁盘写快照，
	// 应该是用户自己按下的决定，而不是面板自带的默认行为。
	KeyBackupScheduleHours = "backup_schedule_hours"
)

// defaults 是键的缺省值。库里没写过这个键时按它算，
// 这样"没配"和"配成 0"是两件能区分的事（0 是非法值，会被校验挡掉）。
var defaults = map[string]int{
	KeyAuditRetentionDays:       90,
	KeySnapshotRetentionDays:    30,
	KeyPlayerEventRetentionDays: 30,
	KeyBackupKeep:               20,
	KeyBackupScheduleHours:      0,
}

// ErrUnknownSetting 表示键没登记过。
// 拒绝未知键是有意的：设置表是唯一"写进去就没人再检查"的地方，
// 拼错的键会安静地躺在库里，而用户以为自己在页面上改成功了。
var ErrUnknownSetting = errors.New("不认识的设置项")

func (s *Store) SetSetting(key, value string) error {
	if _, ok := defaults[key]; !ok {
		return fmt.Errorf("%w：%s", ErrUnknownSetting, key)
	}
	if value == "" {
		return fmt.Errorf("设置项 %s 的值不能为空", key)
	}
	_, err := s.db.Exec(
		`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		   ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("写设置项 %s: %w", key, err)
	}
	return nil
}

// Setting 返回库里的原始字符串值；第二返回值表示这个键有没有被显式设置过。
func (s *Store) Setting(key string) (string, bool, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("读设置项 %s: %w", key, err)
	}
	return v, true, nil
}

// Int 取整型设置，库里没有就用登记值。
// 读到脏数据（手改过库、旧版本留下的非法值）时不 panic 也不静默：
// 回落缺省值并把错误带回给调用方，让接口能显式告诉用户"这项要先重设"。
func (s *Store) Int(key string) (int, error) {
	def, ok := defaults[key]
	if !ok {
		return 0, fmt.Errorf("%w：%s", ErrUnknownSetting, key)
	}
	return s.IntWithDefault(key, def)
}

// IntWithDefault 和 Int 一样，但"库里没写过"时用调用方给的缺省值。
//
// 为什么需要这个：backup.keep 这类项在配置文件里也有一份，而配置那份才是缺省值。
// 用 Int 的话，用户在 yaml 里写 keep: 5、库里从没改过，生效的会是登记表里的 20
// ——等于配置文件被一个看不见的常数悄悄盖掉。登记值只该当文档用。
func (s *Store) IntWithDefault(key string, def int) (int, error) {
	if _, ok := defaults[key]; !ok {
		return 0, fmt.Errorf("%w：%s", ErrUnknownSetting, key)
	}
	raw, found, err := s.Setting(key)
	if err != nil {
		return 0, err
	}
	if !found {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def, fmt.Errorf("设置项 %s 的当前值是 %q，不是整数，已按默认值 %d 执行", key, raw, def)
	}
	return n, nil
}

// Settings 列出全部已显式设置的项，供后台页一次渲染。
// 缺省值不在库里，由调用方用 DefaultInt 补，界面上就能区分"用着默认"和"改过"。
func (s *Store) Settings() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key, value FROM settings ORDER BY key`)
	if err != nil {
		return nil, fmt.Errorf("列举设置项: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("列举设置项时解码一行: %w", err)
		}
		out[k] = v
	}
	return out, rows.Err()
}

func DefaultInt(key string) (int, bool) {
	d, ok := defaults[key]
	return d, ok
}

// RetentionCutoff 算出"保留 N 天"对应的时刻，两个 Purge 共用一个算法，
// 免得天数改了只改一处。天数设成 0 或负数表示不清理，返回零值时间，
// 调用方据此跳过——用户说"全都留着"时不该被当成"全删了"。
func (s *Store) RetentionCutoff(key string, now time.Time) (time.Time, error) {
	days, err := s.Int(key)
	if err != nil {
		return time.Time{}, err
	}
	if days <= 0 {
		return time.Time{}, nil
	}
	return now.AddDate(0, 0, -days), nil
}
