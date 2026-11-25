package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// 房间归属：面板账号 ↔ 磁盘上的那个房间目录。
//
// 为什么不写进 cluster.ini：那是引擎的文件，面板塞进去的自定义键会在下一次
// 引擎整份改写时丢掉；而且用户把房间目录拷给别人时，它跟着走，等于把这台机器
// 上的账号名泄露出去。
//
// 为什么"没有这一行"是合法状态：房间是磁盘事实。装面板之前就存在的房间、
// 在服务器上手工建的房间本来就没有归属。把它们算成"属于管理员"是编数据，
// 算成"未指派"才是界面能说清的话。

type ClusterOwner struct {
	Cluster string
	// AccountName 是归属账号的用户名。带出来是因为房间列表每一行都要显示
	// "这间归谁"，让上层按 id 再查一次账号就是每行一次查询。
	// 用 LEFT JOIN 取：归属行指向一个已被手工删掉的账号时，这一列是空串，
	// 界面据此说"归属账号已不在，请重新指派"，而不是把这条房间变没。
	AccountName string
	AccountID   int64
	AssignedAt  time.Time
	AssignedBy  string
}

// SetClusterOwner 指派房间归属；accountID 传 0 就是取消指派。
// 账号不存在时不写：界面上"指派给一个不存在的账号"只可能是拼出来的请求。
func (s *Store) SetClusterOwner(cluster string, accountID int64, assignedBy string) error {
	if cluster == "" {
		return errors.New("指派归属时没给房间名")
	}
	if accountID == 0 {
		_, err := s.db.Exec(`DELETE FROM cluster_owners WHERE cluster = ?`, cluster)
		if err != nil {
			return fmt.Errorf("取消房间 %s 的归属: %w", cluster, err)
		}
		return nil
	}
	var one int
	if err := s.db.QueryRow(`SELECT 1 FROM accounts WHERE id = ?`, accountID).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrAccountNotFound
		}
		return fmt.Errorf("查账号 %d: %w", accountID, err)
	}
	now := nowMilli()
	_, err := s.db.Exec(`INSERT INTO cluster_owners (cluster, account_id, assigned_at, assigned_by)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(cluster) DO UPDATE SET account_id = excluded.account_id,
			assigned_at = excluded.assigned_at, assigned_by = excluded.assigned_by`,
		cluster, accountID, now, assignedBy)
	if err != nil {
		return fmt.Errorf("指派房间 %s 的归属: %w", cluster, err)
	}
	return nil
}

// ClusterOwners 一次给全量：房间列表每个请求都要标归属，逐间查就是 N+1。
func (s *Store) ClusterOwners() (map[string]ClusterOwner, error) {
	rows, err := s.db.Query(`SELECT o.cluster, o.account_id, COALESCE(a.username, ''),
			o.assigned_at, o.assigned_by
		FROM cluster_owners AS o LEFT JOIN accounts AS a ON a.id = o.account_id`)
	if err != nil {
		return nil, fmt.Errorf("列房间归属: %w", err)
	}
	defer rows.Close()
	out := map[string]ClusterOwner{}
	for rows.Next() {
		var (
			o  ClusterOwner
			at int64
		)
		if err := rows.Scan(&o.Cluster, &o.AccountID, &o.AccountName, &at, &o.AssignedBy); err != nil {
			return nil, fmt.Errorf("解码房间归属: %w", err)
		}
		o.AssignedAt = time.UnixMilli(at).UTC()
		out[o.Cluster] = o
	}
	return out, rows.Err()
}

// ClustersOfAccount 是这个账号名下的房间名单（按房间名排，界面要稳定顺序）。
// 归属页给单个账号看"你管这几间"时用；账号列表那一页是反过来的形态，
// 由 api 层从 ClusterOwners() 自己聚合，不在这个方法上再套一层循环查询。
func (s *Store) ClustersOfAccount(accountID int64) ([]string, error) {
	rows, err := s.db.Query(`SELECT cluster FROM cluster_owners WHERE account_id = ? ORDER BY cluster`, accountID)
	if err != nil {
		return nil, fmt.Errorf("查账号 %d 名下的房间: %w", accountID, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("读房间名: %w", err)
		}
		out = append(out, name)
	}
	return out, rows.Err()
}
