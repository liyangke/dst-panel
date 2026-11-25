// Package store 存面板自己的状态：任务队列、审计日志、在线趋势、后台可调参数。
//
// 为什么用 SQLite 而不是再写一堆 JSON 文件：这三样都有同一个形状——
// "按时间倒序翻、按状态筛、只取前 N 条"。JSON 文件做这些要么全量读进内存，
// 要么自己造二级索引，而这两条路最后都会写成一个更难测的私有格式。
//
// 为什么是 modernc.org/sqlite：纯 Go 无 cgo，GOOS=linux GOARCH=amd64 一条命令交叉编译，
// 不需要在开发机上装 C 工具链，也不需要把 .so 一起发到云服务器。
package store

import (
	"database/sql"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"time"

	// 只注册驱动，不引入任何 cgo 依赖。
	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// 任务状态。字符串直接存进库，前端拿到的是同一个值，标签由 LabelTaskStatus 出中文。
const (
	TaskQueued    = "queued"
	TaskRunning   = "running"
	TaskSucceeded = "succeeded"
	TaskFailed    = "failed"
	TaskCanceled  = "canceled"
)

var taskStatusLabels = map[string]string{
	TaskQueued:    "排队中",
	TaskRunning:   "执行中",
	TaskSucceeded: "已完成",
	TaskFailed:    "失败",
	TaskCanceled:  "已取消",
}

// LabelTaskStatus 给状态码出中文标签；未知状态原样返回，便于排查脏数据。
func LabelTaskStatus(status string) string {
	if l, ok := taskStatusLabels[status]; ok {
		return l
	}
	return status
}

// IsTaskTerminal 表示这条任务不会再自己变化，只有终态可以被重试。
func IsTaskTerminal(status string) bool {
	return status == TaskSucceeded || status == TaskFailed || status == TaskCanceled
}

type Store struct {
	db *sql.DB
}

// Open 打开（或创建）库并跑一遍建表。
//
// 连接池固定成 1 是有意的：SQLite 的写锁是全库的，多个连接同时写只会换来 SQLITE_BUSY，
// 而面板的写入量本来就小（一秒几条顶天）。单连接把"并发写"这个问题从根上消掉，
// 比调 busy_timeout 或者引入队列层划算。
// 读同理——真要出现读被写堵住的体感，再考虑 WAL 下的只读连接池。
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建数据目录 %s: %w", dir, err)
		}
	}
	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)" +
		"&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库 %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(time.Hour)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("连接数据库 %s: %w", path, err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) DB() *sql.DB { return s.db }

// migrate 目前是"建缺失的表 + 补缺失的列"。schema.sql 里全部语句都带 IF NOT EXISTS，
// 所以重复执行是安全的；ALTER 不在其中（SQLite 没有 IF NOT EXISTS 的加列），
// 靠 addColumnIfMissing 先看 PRAGMA 再决定动不动。
func (s *Store) migrate() error {
	if _, err := s.db.Exec(schemaSQL); err != nil {
		return fmt.Errorf("初始化数据库结构失败: %w", err)
	}
	// 旧库升级：detail 是后加的列，存进度那几个可核对的数。
	if err := s.addColumnIfMissing("tasks", "detail", `ALTER TABLE tasks ADD COLUMN detail TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	// 旧库升级：卡片上那两颗星星（好评率与投票数）和收藏数是后加的。
	// 为什么连这三列也要存：界面在无网时退回本地缓存，如果只有在线那批有星星，
	// 同一个模组昨天有星、今天没星，用户会以为模组掉了分。
	// workshop_version 同批加：它把"工坊写的版本"和"这台机器上那份的版本"分开存，
	// 分开之前两者抢同一列，磁盘扫描每刷一次就把工坊那个数抹掉一次，
	// 于是"工坊更新了"这条提示在任何旧库上都不可能亮。
	for _, col := range []struct{ name, ddl string }{
		{"favorited", `ALTER TABLE mods_seen ADD COLUMN favorited INTEGER NOT NULL DEFAULT 0`},
		{"vote_score", `ALTER TABLE mods_seen ADD COLUMN vote_score REAL NOT NULL DEFAULT 0`},
		{"votes_total", `ALTER TABLE mods_seen ADD COLUMN votes_total INTEGER NOT NULL DEFAULT 0`},
		{"workshop_version", `ALTER TABLE mods_seen ADD COLUMN workshop_version TEXT NOT NULL DEFAULT ''`},
	} {
		if err := s.addColumnIfMissing("mods_seen", col.name, col.ddl); err != nil {
			return err
		}
	}
	return nil
}

// addColumnIfMissing 只在列不存在时执行那一条 ALTER。
// 为什么不直接每次跑 ALTER：SQLite 对重复加列直接报错，而 migrate 每次启动都要跑。
func (s *Store) addColumnIfMissing(table, column, ddl string) error {
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return fmt.Errorf("读 %s 的列清单失败: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("读 %s.%s 列名失败: %w", table, column, err)
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("遍历 %s 的列清单失败: %w", table, err)
	}
	if _, err := s.db.Exec(ddl); err != nil {
		return fmt.Errorf("给 %s 加列 %s 失败: %w", table, column, err)
	}
	return nil
}

func nowMilli() int64 { return time.Now().UnixMilli() }
