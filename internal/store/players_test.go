package store

import (
	"testing"
	"time"
)

// 一颗快照里的空名字不能盖掉之前的真名：助手偶尔在玩家还没加载完时采样，
// 界面上就变成"一个没有名字的 KU_xxx"，而且再也修不回来。
func TestUpsertSeenPlayersKeepsFirstSeenAndNonEmptyNames(t *testing.T) {
	s := newStore(t)
	first := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	later := first.Add(2 * time.Hour)

	if err := s.UpsertSeenPlayers("Cluster_1", "Master", []SeenPlayer{
		{UserID: "KU_aaa", Name: "阿甲", Prefab: "wilson"},
	}, first); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertSeenPlayers("Cluster_1", "Caves", []SeenPlayer{
		{UserID: "KU_aaa", Name: "", Prefab: ""},
	}, later); err != nil {
		t.Fatal(err)
	}

	rows, err := s.ListSeenPlayers("Cluster_1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("同一个 userid 应只有一行，实际 %d 行", len(rows))
	}
	p := rows[0]
	if p.Name != "阿甲" || p.Prefab != "wilson" {
		t.Errorf("空值不该覆盖已有资料: %+v", p)
	}
	if !p.FirstSeen.Equal(first) {
		t.Errorf("first_seen 被推到了 %v，应保持 %v", p.FirstSeen, first)
	}
	if !p.LastSeen.Equal(later) {
		t.Errorf("last_seen 应更新到 %v，实际 %v", later, p.LastSeen)
	}
	if p.LastShard != "Caves" {
		t.Errorf("最近分片应跟着快照走，实际 %q", p.LastShard)
	}
}

// 花名册按集群隔离：两个集群各有一个同名角色很常见，不能串。
func TestSeenPlayersAreIsolatedPerCluster(t *testing.T) {
	s := newStore(t)
	at := time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)
	if err := s.UpsertSeenPlayers("Cluster_1", "Master",
		[]SeenPlayer{{UserID: "KU_aaa", Name: "阿甲"}}, at); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertSeenPlayers("Cluster_2", "Master",
		[]SeenPlayer{{UserID: "KU_bbb", Name: "阿乙"}}, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	one, err := s.ListSeenPlayers("Cluster_1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 || one[0].UserID != "KU_aaa" {
		t.Errorf("Cluster_1 只该看到自己的玩家，实际 %+v", one)
	}
	none, err := s.ListSeenPlayers("Cluster_9", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Errorf("不存在的集群不该有花名册，实际 %+v", none)
	}
}

func TestListSeenPlayersOrdersByLastSeen(t *testing.T) {
	s := newStore(t)
	base := time.Date(2026, 3, 3, 9, 0, 0, 0, time.UTC)
	if err := s.UpsertSeenPlayers("Cluster_1", "Master", []SeenPlayer{
		{UserID: "KU_old", Name: "先来的"},
	}, base); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertSeenPlayers("Cluster_1", "Master", []SeenPlayer{
		{UserID: "KU_new", Name: "后来的"},
	}, base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListSeenPlayers("Cluster_1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].UserID != "KU_new" {
		t.Fatalf("应按最近在线倒序，实际 %+v", rows)
	}
}

// 没有集群或分片时不能悄悄写进一张没有归属的表。
func TestUpsertSeenPlayersNeedsScope(t *testing.T) {
	s := newStore(t)
	if err := s.UpsertSeenPlayers("", "Master", []SeenPlayer{{UserID: "KU_aaa"}}, time.Now()); err == nil {
		t.Error("缺集群名应该报错")
	}
	if err := s.UpsertSeenPlayers("Cluster_1", "", []SeenPlayer{{UserID: "KU_aaa"}}, time.Now()); err == nil {
		t.Error("缺分片名应该报错")
	}
}
