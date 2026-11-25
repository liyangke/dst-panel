// 这一层管"同一个大区多久只问一次 Klei"，以及问不到的时候该给界面什么。
//
// 为什么要有它：一份列表解压后有十几 MB，而界面会几秒刷一次；不做缓存的话
// 一个开着的标签页就足以把 CDN 的连接和这台小机的带宽一起喂进去。
// 有了缓存又必须处理失败：这是面板唯一的外部网络依赖，云机出不去公网是常态，
// 那时候界面上该摆"上次取到的那一版（几点）+ 这次为什么没刷动"，
// 而不是把一屏已经看得见的房间换成一条错误。
package lobby

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Cache 按地区存最近一次成功的快照。零值不可用，用 NewCache 建。
type Cache struct {
	Fetch *Fetcher
	// TTL 是一份列表在内存里能当"新"用多久。
	TTL time.Duration
	// RetryAfter 是刷新失败之后隔多久才再撞一次网络。
	// 没有它的话，每次界面轮询都会先卡满一次超时（默认 15 秒），
	// 而"这台机器出不去"这种状态通常不是重启面板能修好的。
	RetryAfter time.Duration

	enabled bool
	mu      sync.Mutex
	entries map[string]*entry
}

type entry struct {
	// mu 把"看有没有在刷"和"刷"合成一步：同地区两个并发请求各拉十几 MB 是白挨一次，
	// 而界面刷新天然就是并发的。
	mu      sync.Mutex
	snap    *Snapshot
	lastErr string
	nextTry time.Time
}

func NewCache(fetch *Fetcher, ttl, retry time.Duration, enabled bool) *Cache {
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	if retry <= 0 {
		retry = 15 * time.Second
	}
	return &Cache{Fetch: fetch, TTL: ttl, RetryAfter: retry, enabled: enabled,
		entries: map[string]*entry{}}
}

func (c *Cache) Enabled() bool { return c != nil && c.enabled }

func (c *Cache) ttl() time.Duration {
	if c != nil && c.TTL > 0 {
		return c.TTL
	}
	return 60 * time.Second
}

func (c *Cache) retryAfter() time.Duration {
	if c != nil && c.RetryAfter > 0 {
		return c.RetryAfter
	}
	return 15 * time.Second
}

// Result 是"要么给你一份能用的列表，要么告诉你为什么没有"。
type Result struct {
	Snap *Snapshot
	// Stale 为真表示这一份是上次成功留下的，这一次没刷动。
	Stale bool
	// WhyStale 是这一次失败的中文原因，只在 Stale 时有内容。
	// 界面必须把它说出来：用户看到"3 分钟前的列表"时第一个问题就是"为什么没更新"。
	WhyStale string
}

// Get 取一个大区的列表。
//
// 返回的 error 只在"一次都没成功过、这次又刷失败"时非空，调用方该回 503
// 并把这句中文原样给界面。有旧数据时不返回 error，而是 Result.Stale。
//
// 地区先过白名单再进缓存：这条顺序保证了 entries 的键只可能是那三个，
// 否则一个拿随机字符串打过来的脚本就能把这张表撑到内存见底。
func (c *Cache) Get(ctx context.Context, regionID string) (Result, error) {
	return c.get(ctx, regionID, false)
}

// GetForce 是「重新取一份」那个按钮走的路：跳过 TTL，也跳过失败冷却。
//
// 为什么要有它：没有它，界面上那颗「刷新」在 TTL 内按下去只会拿到同一份数据，
// 等于给用户一个按下去什么都不发生的按钮——而这一页最容易问的就是"现在还有没有人"。
// 代价是一次十几 MB 的下载，所以它只绑在真人的一次点击上，自动轮询一律走 Get。
func (c *Cache) GetForce(ctx context.Context, regionID string) (Result, error) {
	return c.get(ctx, regionID, true)
}

func (c *Cache) get(ctx context.Context, regionID string, force bool) (Result, error) {
	if !c.Enabled() {
		return Result{}, errors.New("公网房间列表没在配置里打开（lobby.enabled），面板不会去连 Klei 的服务器")
	}
	reg, ok := LookupRegion(regionID)
	if !ok {
		return Result{}, errors.New("没有这个大区：" + regionID + "。面板只读这几个：" + regionIDs())
	}
	e := c.slot(reg.ID)
	e.mu.Lock()
	defer e.mu.Unlock()

	if !force {
		if e.snap != nil && e.snap.Age() < c.ttl() {
			return Result{Snap: e.snap}, nil
		}
		if !e.nextTry.IsZero() && time.Now().Before(e.nextTry) {
			if e.snap != nil {
				return Result{Snap: e.snap, Stale: true, WhyStale: e.lastErr}, nil
			}
			return Result{}, errors.New(e.lastErr)
		}
	}

	snap, err := c.Fetch.Fetch(ctx, reg.ID)
	if err != nil {
		e.nextTry = time.Now().Add(c.retryAfter())
		e.lastErr = err.Error()
		if e.snap != nil {
			return Result{Snap: e.snap, Stale: true, WhyStale: e.lastErr}, nil
		}
		return Result{}, err
	}
	e.snap = snap
	e.lastErr = ""
	e.nextTry = time.Time{}
	return Result{Snap: snap}, nil
}

func (c *Cache) slot(regionID string) *entry {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]*entry{}
	}
	if e, ok := c.entries[regionID]; ok {
		return e
	}
	e := &entry{}
	c.entries[regionID] = e
	return e
}
