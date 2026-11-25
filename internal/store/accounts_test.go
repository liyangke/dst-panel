package store

import (
	"errors"
	"testing"
	"time"
)

func TestAccountCreateAndLookup(t *testing.T) {
	s := newStore(t)

	n, err := s.CountAccounts()
	if err != nil || n != 0 {
		t.Fatalf("初始账号数 = %d/%v，期望 0", n, err)
	}
	a, err := s.CreateAccount("PanelAdmin", "hash-1", RoleAdmin)
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	if a.ID == 0 || a.Role != RoleAdmin {
		t.Fatalf("新建账号 = %+v", a)
	}
	if _, err := s.CreateAccount("paneladmin", "hash-2", RoleViewer); !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("大小写不同但同名应被拒绝，得到 %v", err)
	}

	// 用户名大小写不敏感：登录时用户不会记得自己注册时怎么写的。
	got, hash, err := s.AccountByUsername("paneladmin")
	if err != nil {
		t.Fatalf("AccountByUsername: %v", err)
	}
	if got.ID != a.ID || hash != "hash-1" {
		t.Fatalf("查回来 = %+v / %q", got, hash)
	}
	if _, _, err := s.AccountByUsername("nobody"); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("不存在的账号 = %v", err)
	}
}

func TestAccountMutationsAndAdminCount(t *testing.T) {
	s := newStore(t)
	_, _ = s.CreateAccount("boss", "h1", RoleAdmin)
	viewer, _ := s.CreateAccount("guest", "h2", RoleViewer)

	if n, _ := s.CountAdmins(); n != 1 {
		t.Fatalf("管理员数 = %d，期望 1", n)
	}
	if err := s.UpdateAccountRole(viewer.ID, RoleAdmin); err != nil {
		t.Fatalf("升级角色: %v", err)
	}
	if n, _ := s.CountAdmins(); n != 2 {
		t.Fatalf("升级后管理员数 = %d，期望 2", n)
	}
	if err := s.UpdateAccountPassword(viewer.ID, "h3"); err != nil {
		t.Fatalf("改密码: %v", err)
	}
	_, hash, _ := s.AccountByUsername("guest")
	if hash != "h3" {
		t.Fatalf("密码哈希 = %q，期望 h3", hash)
	}

	at := time.Unix(1700000000, 0)
	if err := s.TouchAccountLogin(viewer.ID, at); err != nil {
		t.Fatalf("记登录时间: %v", err)
	}
	got, _ := s.AccountByID(viewer.ID)
	if !got.LastLoginAt.Equal(at) {
		t.Fatalf("最后登录 = %v，期望 %v", got.LastLoginAt, at)
	}

	if err := s.DeleteAccount(viewer.ID); err != nil {
		t.Fatalf("删账号: %v", err)
	}
	if err := s.DeleteAccount(viewer.ID); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("重复删 = %v，期望不存在", err)
	}
	list, _ := s.ListAccounts()
	if len(list) != 1 || list[0].Username != "boss" {
		t.Fatalf("删后列表 = %+v", list)
	}
}

func TestAccountRejectsUnknownRole(t *testing.T) {
	s := newStore(t)
	if _, err := s.CreateAccount("x", "h", "root"); err == nil {
		t.Fatal("未知角色应被拒绝")
	}
}
