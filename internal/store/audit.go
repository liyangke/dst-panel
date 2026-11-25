package store

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// AuditEntry 是一条平台操作记录。
//
// 和 MOD 桥的 result.json 分工不同：那边回答"这条游戏内命令执行结果如何"，
// 这边回答"谁在什么时候对哪个对象动了什么"。踢人、关服、改世界这种都能从
// 平台发起，出了纠纷必须能指着一条记录说清楚。
type AuditEntry struct {
	ID     int64
	At     time.Time
	Actor  string
	Action string
	Target string
	Detail string
	OK     bool
	Err    string
}

const auditSelect = `SELECT id, at, actor, action, target, detail, ok, err FROM audit_log`

func (s *Store) AppendAudit(e AuditEntry) error {
	if e.Actor == "" {
		return errors.New("审计记录必须有操作者，系统触发的写 system")
	}
	if e.Action == "" {
		return errors.New("审计记录必须有动作名")
	}
	at := e.At
	if at.IsZero() {
		at = time.Now()
	}
	ok := 0
	if e.OK {
		ok = 1
	}
	_, err := s.db.Exec(
		`INSERT INTO audit_log (at, actor, action, target, detail, ok, err) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		at.UnixMilli(), e.Actor, e.Action, e.Target, e.Detail, ok, e.Err)
	if err != nil {
		return fmt.Errorf("写审计记录 %s: %w", e.Action, err)
	}
	return nil
}

// ListAudit 按时间倒序翻页。actionPrefix 用 "mod." 这类前缀筛，
// 索引在 at 上，前缀筛是在已截断的结果集里做，量级完全够。
func (s *Store) ListAudit(limit int, offset int, actionPrefix string) ([]AuditEntry, error) {
	return s.ListAuditFiltered(limit, offset, AuditFilter{ActionPrefix: actionPrefix})
}

// AuditFilter 是操作记录页的筛选条件，零值表示"这一条不筛"。
//
// Since/Until 用 time.Time 而不是字符串：调用方（界面）负责把"用户选的本地那一天"
// 换算成一个确定的时刻再传进来。库里存的是 UTC 毫秒，后端要是自己按日期字符串比，
// 东八区的"11 月 2 日"会被当成 UTC 的 11 月 2 日，界面上看就是少了 8 小时的那一段。
type AuditFilter struct {
	ActionPrefix string
	Since        time.Time // 含这一刻
	Until        time.Time // 不含这一刻
	Actor        string    // 谁：等值匹配，界面给的是从库里已有操作人里挑的下拉
	Target       string    // 对象：子串匹配（"Cluster_1" 要能同时捞到 Master 和 Caves 那两条）
}

// ListAuditFiltered 同 ListAudit，额外支持时间区间、操作人和对象。
func (s *Store) ListAuditFiltered(limit int, offset int, f AuditFilter) ([]AuditEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	query := auditSelect
	args := []any{}
	var where []string
	if f.ActionPrefix != "" {
		where = append(where, `substr(action, 1, ?) = ?`)
		args = append(args, len(f.ActionPrefix), f.ActionPrefix)
	}
	if !f.Since.IsZero() {
		where = append(where, `at >= ?`)
		args = append(args, f.Since.UnixMilli())
	}
	if !f.Until.IsZero() {
		where = append(where, `at < ?`)
		args = append(args, f.Until.UnixMilli())
	}
	if actor := strings.TrimSpace(f.Actor); actor != "" {
		where = append(where, `actor = ?`)
		args = append(args, actor)
	}
	if p := likePattern(f.Target); p != "" {
		where = append(where, `target LIKE ? ESCAPE '\'`)
		args = append(args, p)
	}
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, ` AND `)
	}
	query += ` ORDER BY id DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("查询审计记录: %w", err)
	}
	defer rows.Close()

	var out []AuditEntry
	for rows.Next() {
		var (
			e  AuditEntry
			at int64
			ok int
		)
		if err := rows.Scan(&e.ID, &at, &e.Actor, &e.Action, &e.Target, &e.Detail, &ok, &e.Err); err != nil {
			return nil, fmt.Errorf("解码审计记录: %w", err)
		}
		e.At = time.UnixMilli(at).UTC()
		e.OK = ok == 1
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListAuditActors 给出库里出现过的操作人，给筛选下拉用。
//
// 取值来自这张表本身，所以列出来的每一个人都一定筛得出东西——下拉里摆一个
// "选了永远空"的项，比少一个选项糟糕得多。空的操作人（历史行里可能有）跳过。
func (s *Store) ListAuditActors() ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT actor FROM audit_log WHERE actor <> '' ORDER BY actor`)
	if err != nil {
		return nil, fmt.Errorf("查操作人名单: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("解码操作人名单: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// PurgeAudit 删掉 before 之前的记录，返回删了几条。审计日志默认无限增长，
// 一年就是一堆没人看的东西；保留多久是后台可配项（见 settings）。
func (s *Store) PurgeAudit(before time.Time) (int, error) {
	res, err := s.db.Exec(`DELETE FROM audit_log WHERE at < ?`, before.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("清理审计记录: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("清理审计记录后取行数: %w", err)
	}
	return int(n), nil
}
