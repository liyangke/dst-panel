// Package auth 负责面板账号的密码哈希与登录校验缓存。
//
// 为什么不用明文或简单 SHA：面板可能直接开在公网端口上，库文件一旦被拖走，
// 明文密码等于把复用到别处的口令一起送出去。用标准库实现 PBKDF2-HMAC-SHA256，
// 不引第三方依赖，也能在 4G 小机上跑得动。
//
// 为什么要缓存：HTTP Basic 每个请求都要核对一次，而面板前端每五秒会打一组接口，
// 每次都真算一遍 PBKDF2 会把小机的 CPU 吃满。缓存键里带密码哈希，
// 改密码后哈希变了，旧缓存自然失效，不需要额外的版本号。
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	algo       = "pbkdf2-sha256"
	iterations = 210000
	saltLen    = 16
	keyLen     = 32
)

// Hash 生成可入库的密码串，格式 pbkdf2-sha256$轮数$盐$摘要（base64url 无填充）。
func Hash(password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("生成密码盐: %w", err)
	}
	dk := pbkdf2SHA256([]byte(password), salt, iterations, keyLen)
	return fmt.Sprintf("%s$%d$%s$%s", algo, iterations,
		base64.RawURLEncoding.EncodeToString(salt),
		base64.RawURLEncoding.EncodeToString(dk)), nil
}

// Verify 核对密码。格式不认识或轮数离谱一律当失败，避免脏数据让整次登录 500。
func Verify(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != algo {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 10000 || iter > 2000000 {
		return false
	}
	salt, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(salt) == 0 {
		return false
	}
	want, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil || len(want) == 0 {
		return false
	}
	got := pbkdf2SHA256([]byte(password), salt, iter, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// dummyHash 给"用户名不存在"那次核对用：不存在的用户也要花同样的时间算一遍，
// 否则响应快慢就能被用来枚举哪些用户名是真的。
var dummyHash = func() string {
	h, err := Hash("dstpanel-dummy-password")
	if err != nil {
		return ""
	}
	return h
}()

// VerifyDummy 只消耗与真实验证相当的时间，结果永远不用。
func VerifyDummy(password string) {
	if dummyHash != "" {
		_ = Verify(dummyHash, password)
	}
}

// ValidatePassword 是新建账号和改密码时的唯一规则来源。
// 下限 5 位是为了让内置默认凭据（admin/admin）能通过同一套规则——规则如果比默认值严，
// 部署者想"先照默认值来"就必须绕过校验，那才是真出事的路子。
// 代价是面板不再拦弱口令：界面会对短密码给提示，并一直显示"还在用默认密码"，
// 剩下的强度问题由部署者自己负责（见 README 的安全设计）。
func ValidatePassword(p string) error {
	n := len([]rune(p))
	if n < 5 {
		return errors.New("密码至少 5 个字符")
	}
	if len(p) > 128 {
		return errors.New("密码不能超过 128 字节")
	}
	return nil
}

// IsWeak 只用来给界面出提示，不参与任何放行判断：
// 面板已经不拦弱口令了，但"这个密码短到会被人猜到"这件事得让人看见。
func IsWeak(p string) bool { return len([]rune(p)) < 8 }

func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hLen := prf.Size()
	blocks := (keyLen + hLen - 1) / hLen
	out := make([]byte, 0, blocks*hLen)
	u := make([]byte, hLen)
	t := make([]byte, hLen)
	var idx [4]byte
	for block := 1; block <= blocks; block++ {
		prf.Reset()
		prf.Write(salt)
		binary.BigEndian.PutUint32(idx[:], uint32(block))
		prf.Write(idx[:])
		u = prf.Sum(u[:0])
		copy(t, u)
		for i := 1; i < iter; i++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

// Cache 只缓存"这组用户名+密码+当前哈希能不能过"，不缓存角色。
// 角色每次从库里现读，管理员被降成只读后不需要等缓存过期。
type Cache struct {
	mu  sync.Mutex
	m   map[string]entry
	ttl time.Duration
	now func() time.Time
}

type entry struct {
	ok  bool
	exp time.Time
}

const cacheCap = 512

func NewCache(ttl time.Duration) *Cache {
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}
	return &Cache{m: make(map[string]entry), ttl: ttl, now: time.Now}
}

// Key 把一次凭据核对压成定长键；哈希进键里，改密码后自然不命中。
func Key(username, password, passwordHash string) string {
	sum := sha256.Sum256([]byte(username + "\x00" + password + "\x00" + passwordHash))
	return string(sum[:])
}

func (c *Cache) Get(key string) (bool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok {
		return false, false
	}
	if c.now().After(e.exp) {
		delete(c.m, key)
		return false, false
	}
	return e.ok, true
}

func (c *Cache) Put(key string, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) >= cacheCap {
		// 面板的账号数远小于这个上限；真到了这里说明有人在拿字典刷接口，
		// 整表清空比实现一套 LRU 更简单，也不会让旧的成功缓存挡住新密码。
		c.m = make(map[string]entry)
	}
	c.m[key] = entry{ok: ok, exp: c.now().Add(c.ttl)}
}
