package store

import (
	"fmt"
	"time"
)

// SeenPlayer 是助手快照里出现过的玩家。表见 schema.sql 的 players_seen。
type SeenPlayer struct {
	UserID    string
	Name      string
	Prefab    string
	LastShard string
	FirstSeen time.Time
	LastSeen  time.Time
}

// UpsertSeenPlayers 把一轮快照里的玩家写进花名册。
//
// 名字和角色取最后一次看到的非空值：助手偶尔拿不到 name（玩家还没加载完），
// 用空串覆盖掉上一次的真名，界面上就会变成"一个没有名字的 KU_xxx"。
// 先从 first_seen 保留原值，避免每次采样都把"第一次见到"推到现在。
func (s *Store) UpsertSeenPlayers(cluster, shard string, players []SeenPlayer, at time.Time) error {
	if cluster == "" || shard == "" {
		return fmt.Errorf("记玩家花名册必须有集群和世界名")
	}
	if at.IsZero() {
		at = time.Now()
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("开启玩家花名册事务: %w", err)
	}
	defer tx.Rollback()

	now := at.UnixMilli()
	for _, p := range players {
		if p.UserID == "" {
			continue
		}
		_, err := tx.Exec(
			`INSERT INTO players_seen (cluster, userid, name, prefab, last_shard, first_seen_at, last_seen_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(cluster, userid) DO UPDATE SET
			   name = CASE WHEN excluded.name <> '' THEN excluded.name ELSE players_seen.name END,
			   prefab = CASE WHEN excluded.prefab <> '' THEN excluded.prefab ELSE players_seen.prefab END,
			   last_shard = excluded.last_shard,
			   last_seen_at = excluded.last_seen_at`,
			cluster, p.UserID, p.Name, p.Prefab, shard, now, now)
		if err != nil {
			return fmt.Errorf("写入玩家花名册 %s: %w", p.UserID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交玩家花名册: %w", err)
	}
	return nil
}

// ListSeenPlayers 按最近在线倒序列出这个集群见过的玩家。
func (s *Store) ListSeenPlayers(cluster string, limit int) ([]SeenPlayer, error) {
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	rows, err := s.db.Query(
		`SELECT userid, name, prefab, last_shard, first_seen_at, last_seen_at
		   FROM players_seen WHERE cluster = ?
		  ORDER BY last_seen_at DESC LIMIT ?`, cluster, limit)
	if err != nil {
		return nil, fmt.Errorf("查询玩家花名册: %w", err)
	}
	defer rows.Close()

	var out []SeenPlayer
	for rows.Next() {
		var (
			p     SeenPlayer
			first int64
			last  int64
		)
		if err := rows.Scan(&p.UserID, &p.Name, &p.Prefab, &p.LastShard, &first, &last); err != nil {
			return nil, fmt.Errorf("解码玩家花名册: %w", err)
		}
		p.FirstSeen = time.UnixMilli(first).UTC()
		p.LastSeen = time.UnixMilli(last).UTC()
		out = append(out, p)
	}
	return out, rows.Err()
}
