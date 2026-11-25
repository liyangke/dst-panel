package store

import (
	"fmt"
	"time"
)

// Snapshot 是一次在线人数/世界状态采样，用于趋势图。
//
// 助手 MOD 每秒都写 status.json，但这里按采样间隔存：一台跑一个月的服务器，
// 按秒存就是 260 万行，全在画图上根本看不出来，还先把自己压垮了。
type Snapshot struct {
	ID      int64
	At      time.Time
	Cluster string
	Shard   string
	Day     int
	Season  string
	Online  int
	// Detail 是玩家名列表的 JSON，只在"看某一天谁在线"时才解。
	Detail string
}

func (s *Store) RecordSnapshot(sn Snapshot) error {
	if sn.Cluster == "" || sn.Shard == "" {
		return fmt.Errorf("记在线人数必须有集群和世界名")
	}
	at := sn.At
	if at.IsZero() {
		at = time.Now()
	}
	_, err := s.db.Exec(
		`INSERT INTO player_snapshots (at, cluster, shard, day, season, online, detail)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		at.UnixMilli(), sn.Cluster, sn.Shard, sn.Day, sn.Season, sn.Online, sn.Detail)
	if err != nil {
		return fmt.Errorf("写在线人数记录 %s/%s: %w", sn.Cluster, sn.Shard, err)
	}
	return nil
}

// Snapshots 取某个集群一段时间内的采样，按时间升序返回（前端画折线不用自己反）。
// shard 传空串表示所有分片。
func (s *Store) Snapshots(cluster, shard string, since time.Time, limit int) ([]Snapshot, error) {
	if limit <= 0 || limit > 5000 {
		limit = 720
	}
	query := `SELECT id, at, cluster, shard, day, season, online, detail
	            FROM player_snapshots WHERE cluster = ? AND at >= ?`
	args := []any{cluster, since.UnixMilli()}
	if shard != "" {
		query += ` AND shard = ?`
		args = append(args, shard)
	}
	query += ` ORDER BY at ASC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("查询在线人数记录: %w", err)
	}
	defer rows.Close()

	var out []Snapshot
	for rows.Next() {
		var (
			sn Snapshot
			at int64
		)
		if err := rows.Scan(&sn.ID, &at, &sn.Cluster, &sn.Shard, &sn.Day,
			&sn.Season, &sn.Online, &sn.Detail); err != nil {
			return nil, fmt.Errorf("解码在线人数记录: %w", err)
		}
		sn.At = time.UnixMilli(at).UTC()
		out = append(out, sn)
	}
	return out, rows.Err()
}

// Trend 是趋势图的整份答案：归好类的点，加上"这段时间里到底有多少条原始采样、
// 最早最晚各是什么时刻"。后面那三个数是给界面写诚实说明用的，不是装饰。
type Trend struct {
	Points   []TrendPoint
	Samples  int       // 这段时间内的原始采样条数
	DataFrom time.Time // 最早一条采样；Samples 为 0 时是零值
	DataTo   time.Time // 最晚一条采样，趋势图右端停在哪儿由它决定
}

type TrendPoint struct {
	At     time.Time
	Online int
}

// SnapshotTrend 把一段时间内的采样按 bucket 归成一条曲线：每个点是"这段时间里每个世界的平均人数相加"。
//
// 为什么必须在 SQL 里聚合而不是把原始行交给界面去归并：一次轮询每个世界各写一条，
// 双世界跑一天就是两千多条，而任何"取前 N 行"的读法都会悄悄把最近的一段砍掉——
// 云端实测过一次：界面写着"过去 24 小时"，图上最右端其实是 6 小时前的采样。
// 数字看着正常、其实是旧的，这类错比报错难发现得多。
// shard 传空串表示整个房间（各世界相加）。
func (s *Store) SnapshotTrend(cluster, shard string, since time.Time, bucket time.Duration) (Trend, error) {
	b := int64(bucket / time.Millisecond)
	if b < 1000 {
		// 小于 1 秒的格子没有意义，还会让 (at/b)*b 的除法退化成按毫秒分桶。
		b = 60_000
	}
	where := `cluster = ? AND at >= ?`
	countArgs := []any{cluster, since.UnixMilli()}
	if shard != "" {
		where += ` AND shard = ?`
		countArgs = append(countArgs, shard)
	}
	var (
		samples    int
		fromMs     int64
		toMs       int64
		dataFrom   time.Time
		dataTo     time.Time
		samplesErr error
	)
	row := s.db.QueryRow(`SELECT COUNT(*), IFNULL(MIN(at), 0), IFNULL(MAX(at), 0)
	                      FROM player_snapshots WHERE `+where, countArgs...)
	if err := row.Scan(&samples, &fromMs, &toMs); err != nil {
		samplesErr = fmt.Errorf("统计在线人数采样条数: %w", err)
	}
	if samples > 0 {
		dataFrom = time.UnixMilli(fromMs).UTC()
		dataTo = time.UnixMilli(toMs).UTC()
	}

	inner := `SELECT (at / ?) * ? AS bucket, shard, AVG(online) AS avg_online
	          FROM player_snapshots WHERE ` + where + `
	          GROUP BY bucket, shard`
	innerArgs := []any{b, b}
	innerArgs = append(innerArgs, countArgs...)
	query := `SELECT bucket, CAST(ROUND(SUM(avg_online)) AS INTEGER)
	          FROM (` + inner + `)
	          GROUP BY bucket ORDER BY bucket`

	rows, err := s.db.Query(query, innerArgs...)
	if err != nil {
		return Trend{}, fmt.Errorf("按格子统计在线人数: %w", err)
	}
	defer rows.Close()

	var out []TrendPoint
	for rows.Next() {
		var (
			ms  int64
			num int
		)
		if err := rows.Scan(&ms, &num); err != nil {
			return Trend{}, fmt.Errorf("解码在线人数格子: %w", err)
		}
		out = append(out, TrendPoint{At: time.UnixMilli(ms).UTC(), Online: num})
	}
	if err := rows.Err(); err != nil {
		return Trend{}, err
	}
	if samplesErr != nil {
		return Trend{}, samplesErr
	}
	return Trend{Points: out, Samples: samples, DataFrom: dataFrom, DataTo: dataTo}, nil
}

func (s *Store) PurgeSnapshots(before time.Time) (int, error) {
	res, err := s.db.Exec(`DELETE FROM player_snapshots WHERE at < ?`, before.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("清理在线人数记录: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("清理在线人数记录后取行数: %w", err)
	}
	return int(n), nil
}
