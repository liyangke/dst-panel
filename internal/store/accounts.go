package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 面板账号角色。字符串直接存库，前端只渲染 LabelRole 的中文。
//
// 三种角色不是"权限从大到小排一排"，而是各自管一件事：
//   - admin 管这台机器（建房间、改面板设置、管账号、指派归属）；
//   - member 只管派给自己的那几个房间；没指派给谁的房间它看得见但改不动，
//     派给别人的房间连列表里都不出现；
//   - viewer 什么都不改，只读全部。
//
// member 夹在中间是为了"几个人一起守几间房"这种真实分工：
// 给 admin 会顺手动到别人的房间和机器设置，给 viewer 又什么都做不了，
// 最后大家只能共用一个管理员账号——那等于没有审计。
const (
	RoleAdmin  = "admin"
	RoleMember = "member"
	RoleViewer = "viewer"
)

var roleLabels = map[string]string{
	RoleAdmin:  "管理员",
	RoleMember: "房间成员",
	RoleViewer: "只读",
}

// roleHints 是界面下拉框里那句"这个角色能做什么"。
// 放在 store 而不是前端：账号页和归属弹窗都要用同一段话，
// 两边各写一份迟早会把权限说得不一样。
var roleHints = map[string]string{
	RoleAdmin:  "能改这台机器上的一切：建房间、面板设置、账号与归属指派",
	RoleMember: "只能操作指派给自己名下的房间；没指派的房间看得见但改不动，归别人的房间不在列表里",
	RoleViewer: "只看，任何修改都会被拒（改自己的密码除外）",
}

// RoleOrder 是角色下拉的顺序，也是报错那句里列举角色的顺序。
// 只有一份：接口校验失败时说的"只能是…"和界面下拉里的项必须一模一样。
var RoleOrder = []string{RoleAdmin, RoleMember, RoleViewer}

// RoleLabels 按 RoleOrder 给出每个角色的中文说法。
func RoleLabels() []string {
	out := make([]string, 0, len(RoleOrder))
	for _, role := range RoleOrder {
		out = append(out, LabelRole(role))
	}
	return out
}

func LabelRole(role string) string {
	if l, ok := roleLabels[role]; ok {
		return l
	}
	return role
}

func HintRole(role string) string {
	return roleHints[role]
}

func ValidRole(role string) bool {
	return role == RoleAdmin || role == RoleMember || role == RoleViewer
}

var (
	ErrAccountNotFound = errors.New("账号不存在")
	ErrUsernameTaken   = errors.New("这个用户名已经被用了")
)

type Account struct {
	ID          int64
	Username    string
	Role        string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	LastLoginAt time.Time
}

const accountSelect = `SELECT id, username, role, created_at, updated_at, last_login_at FROM accounts`

func scanAccount(row interface{ Scan(...any) error }) (Account, error) {
	var (
		a    Account
		ca   int64
		ua   int64
		last sql.NullInt64
	)
	if err := row.Scan(&a.ID, &a.Username, &a.Role, &ca, &ua, &last); err != nil {
		return Account{}, err
	}
	a.CreatedAt = time.UnixMilli(ca).UTC()
	a.UpdatedAt = time.UnixMilli(ua).UTC()
	if last.Valid {
		a.LastLoginAt = time.UnixMilli(last.Int64).UTC()
	}
	return a, nil
}

func (s *Store) CountAccounts() (int, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM accounts`).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计账号: %w", err)
	}
	return n, nil
}

func (s *Store) CreateAccount(username, passwordHash, role string) (Account, error) {
	if !ValidRole(role) {
		return Account{}, fmt.Errorf("未知角色 %q", role)
	}
	now := nowMilli()
	res, err := s.db.Exec(
		`INSERT INTO accounts (username, password_hash, role, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		username, passwordHash, role, now, now)
	if err != nil {
		// modernc 的 SQLite 在这里报的是 "UNIQUE constraint failed: accounts.username"。
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return Account{}, ErrUsernameTaken
		}
		return Account{}, fmt.Errorf("新建账号: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Account{}, fmt.Errorf("新建账号后取 id: %w", err)
	}
	return Account{
		ID: id, Username: username, Role: role,
		CreatedAt: time.UnixMilli(now).UTC(), UpdatedAt: time.UnixMilli(now).UTC(),
	}, nil
}

func (s *Store) ListAccounts() ([]Account, error) {
	rows, err := s.db.Query(accountSelect + ` ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("列账号: %w", err)
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, fmt.Errorf("解码账号: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AccountByUsername 同时返回密码哈希，登录校验要用；列表接口不拿它。
func (s *Store) AccountByUsername(username string) (Account, string, error) {
	var hash string
	row := s.db.QueryRow(
		`SELECT id, username, role, created_at, updated_at, last_login_at, password_hash
		 FROM accounts WHERE username = ?`, username)
	var (
		a    Account
		ca   int64
		ua   int64
		last sql.NullInt64
	)
	if err := row.Scan(&a.ID, &a.Username, &a.Role, &ca, &ua, &last, &hash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Account{}, "", ErrAccountNotFound
		}
		return Account{}, "", fmt.Errorf("查账号 %s: %w", username, err)
	}
	a.CreatedAt = time.UnixMilli(ca).UTC()
	a.UpdatedAt = time.UnixMilli(ua).UTC()
	if last.Valid {
		a.LastLoginAt = time.UnixMilli(last.Int64).UTC()
	}
	return a, hash, nil
}

func (s *Store) AccountByID(id int64) (Account, error) {
	row := s.db.QueryRow(accountSelect+` WHERE id = ?`, id)
	a, err := scanAccount(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Account{}, ErrAccountNotFound
		}
		return Account{}, fmt.Errorf("查账号 %d: %w", id, err)
	}
	return a, nil
}

// PasswordHashByID 是哈希唯一的显式取回入口。
// Account 结构体里刻意不带 password_hash：列表接口拿不到哈希，就不存在"某处忘了脱敏把哈希写进响应"。
// 需要拿哈希做判断（比如"还在不用出厂默认密码"）时必须走这里，调用点写得清清楚楚。
func (s *Store) PasswordHashByID(id int64) (string, error) {
	var hash string
	err := s.db.QueryRow(`SELECT password_hash FROM accounts WHERE id = ?`, id).Scan(&hash)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrAccountNotFound
		}
		return "", fmt.Errorf("查账号 %d 的密码哈希: %w", id, err)
	}
	return hash, nil
}

func (s *Store) UpdateAccountPassword(id int64, passwordHash string) error {
	res, err := s.db.Exec(`UPDATE accounts SET password_hash = ?, updated_at = ? WHERE id = ?`,
		passwordHash, nowMilli(), id)
	if err != nil {
		return fmt.Errorf("改账号密码: %w", err)
	}
	return requireOneRow(res, id)
}

func (s *Store) UpdateAccountRole(id int64, role string) error {
	if !ValidRole(role) {
		return fmt.Errorf("未知角色 %q", role)
	}
	res, err := s.db.Exec(`UPDATE accounts SET role = ?, updated_at = ? WHERE id = ?`,
		role, nowMilli(), id)
	if err != nil {
		return fmt.Errorf("改账号角色: %w", err)
	}
	return requireOneRow(res, id)
}

// DeleteAccount 连带清掉这个账号名下的房间归属。
//
// 为什么必须在一个事务里：分两条语句跑的话，中间崩了就留下"归属指向一个
// 已经不存在的账号"，界面上会显示"这房间归 id=7 管"，而 id=7 查无此人——
// 那种状态既指派不掉也取消不掉，只能动数据库文件。
// 房间本身照样在磁盘上，只是回到"未指派"。
func (s *Store) DeleteAccount(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("开事务删账号: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM cluster_owners WHERE account_id = ?`, id); err != nil {
		return fmt.Errorf("清掉账号 %d 名下的房间归属: %w", id, err)
	}
	res, err := tx.Exec(`DELETE FROM accounts WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("删账号: %w", err)
	}
	if err := requireOneRow(res, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) TouchAccountLogin(id int64, at time.Time) error {
	if at.IsZero() {
		at = time.Now()
	}
	_, err := s.db.Exec(`UPDATE accounts SET last_login_at = ? WHERE id = ?`, at.UnixMilli(), id)
	if err != nil {
		return fmt.Errorf("记账号登录时间: %w", err)
	}
	return nil
}

func (s *Store) CountAdmins() (int, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM accounts WHERE role = ?`, RoleAdmin).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计管理员: %w", err)
	}
	return n, nil
}

func requireOneRow(res sql.Result, id int64) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("取影响行数: %w", err)
	}
	if n == 0 {
		return ErrAccountNotFound
	}
	return nil
}
