package auth

import (
	"strings"
	"testing"
	"time"

	"dstpanel/internal/config"
)

func TestHashVerify(t *testing.T) {
	h, err := Hash("test-passwd-1")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if !strings.HasPrefix(h, "pbkdf2-sha256$") {
		t.Fatalf("哈希格式 = %q，期望带算法前缀", h)
	}
	if !Verify(h, "test-passwd-1") {
		t.Error("正确密码没通过")
	}
	if Verify(h, "test-passwd-2") {
		t.Error("错误密码通过了")
	}
	if Verify("脏数据", "test-passwd-1") {
		t.Error("坏哈希不应通过")
	}
	if Verify("pbkdf2-sha256$999$c2FsdA$aGFzaA", "x") {
		t.Error("轮数太低的哈希不应通过")
	}
}

func TestHashUsesFreshSalt(t *testing.T) {
	a, _ := Hash("same-password")
	b, _ := Hash("same-password")
	if a == b {
		t.Fatal("两次哈希相同，盐没有随机")
	}
}

// TestDefaultCredentialPassesRule 钉住一条容易踩的坑：校验规则不能比内置默认凭据更严。
// 规则更严的话，部署者想"先照默认值来"就得绕过校验写库，绕过的那条路才是真出事的地方。
func TestDefaultCredentialPassesRule(t *testing.T) {
	if err := ValidatePassword(config.DefaultAdminPassword); err != nil {
		t.Fatalf("内置默认密码 %q 应通过自己的校验规则，得到 %v", config.DefaultAdminPassword, err)
	}
	if err := ValidatePassword("admin"); err != nil {
		t.Fatalf("5 位密码应通过，得到 %v", err)
	}
}

func TestValidatePassword(t *testing.T) {
	if err := ValidatePassword("abcd"); err == nil {
		t.Error("4 位密码应被拒绝")
	}
	if err := ValidatePassword("abcde"); err != nil {
		t.Errorf("5 位密码应通过，得到 %v", err)
	}
	if err := ValidatePassword(strings.Repeat("a", 129)); err == nil {
		t.Error("超长密码应被拒绝")
	}
}

// TestIsWeakOnlyHints 说明 IsWeak 的边界：8 位以下算弱，但它不参与放行。
func TestIsWeakOnlyHints(t *testing.T) {
	if !IsWeak("admin") {
		t.Error("5 位密码应标记为弱")
	}
	if IsWeak("abcdefgh") {
		t.Error("8 位密码不应标记为弱")
	}
}

func TestCacheHitAndExpiry(t *testing.T) {
	now := time.Unix(1000, 0)
	c := NewCache(time.Minute)
	c.now = func() time.Time { return now }

	key := Key("u", "p", "h")
	if _, hit := c.Get(key); hit {
		t.Fatal("空缓存不该命中")
	}
	c.Put(key, true)
	if ok, hit := c.Get(key); !hit || !ok {
		t.Fatalf("刚写入的缓存 = %v/%v，期望命中且为 true", hit, ok)
	}
	now = now.Add(61 * time.Second)
	if _, hit := c.Get(key); hit {
		t.Fatal("过期后仍然命中")
	}
}

func TestCacheKeyChangesWithHash(t *testing.T) {
	if Key("u", "p", "h1") == Key("u", "p", "h2") {
		t.Fatal("密码哈希变化后缓存键应变化")
	}
}
