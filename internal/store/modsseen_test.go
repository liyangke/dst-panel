package store

import (
	"path/filepath"
	"strings"
	"testing"
)

func newSeenStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "dstpanel.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestRecordSeenKeepsWhatTheNewSourceDoesntKnow 是这张表的核心风险：
// 磁盘扫描知道名字，接口知道订阅数，任何一边把另一边的字段覆盖成零，
// 界面上就会出现"搜到过一次之后就再也搜不到"。
func TestRecordSeenKeepsWhatTheNewSourceDoesntKnow(t *testing.T) {
	s := newSeenStore(t)
	if err := s.RecordSeen(SeenMod{
		WorkshopID: "350811795", Name: "更多物品堆叠", Version: "1.6",
		Description: "每格 99 个", Tags: []string{"server_only_mod", "version:1.6"},
		Subscriptions: 20134, ModUpdatedAt: 1700000000, FileSize: 81234,
		PreviewURL: "https://images.steamusercontent.com/a.jpg", Origin: SeenFromWorkshop,
	}); err != nil {
		t.Fatalf("RecordSeen: %v", err)
	}
	// 第二次来自磁盘扫描：名字有，订阅数/预览图/更新时间这些接口才有的字段全是零值。
	if err := s.RecordSeen(SeenMod{
		WorkshopID: "350811795", Name: "More Items Less Screens",
		Version: "1.7", Description: "磁盘 modinfo 里的说明", Origin: SeenFromDisk,
	}); err != nil {
		t.Fatalf("RecordSeen 第二次: %v", err)
	}

	got, ok, err := s.GetSeen("350811795")
	if err != nil {
		t.Fatalf("GetSeen: %v", err)
	}
	if !ok {
		t.Fatal("GetSeen 说没这条，写过两次的条目不该消失")
	}
	// 文本以最后一次非空为准：modinfo 的名字是本地实际生效的那个。
	if got.Name != "More Items Less Screens" || got.Version != "1.7" {
		t.Errorf("文本字段没按非空覆盖：%q / %q", got.Name, got.Version)
	}
	if got.Description != "磁盘 modinfo 里的说明" {
		t.Errorf("description = %q", got.Description)
	}
	// 数字只增不减。
	if got.Subscriptions != 20134 {
		t.Errorf("subscriptions 被零值抹掉了 = %d，期望 20134", got.Subscriptions)
	}
	if got.FileSize != 81234 || got.ModUpdatedAt != 1700000000 {
		t.Errorf("file_size/时间被抹掉 = %d / %d", got.FileSize, got.ModUpdatedAt)
	}
	if got.PreviewURL == "" {
		t.Error("preview_url 被空串抹掉了")
	}
	// 标签来自接口，这次没给就不该清空。
	if strings.Join(got.Tags, ",") != "server_only_mod,version:1.6" {
		t.Errorf("tags = %v", got.Tags)
	}
	// 只有 workshop 会把来源拉回来；本地扫描不该让条目"降级"。
	if got.Origin != SeenFromWorkshop {
		t.Errorf("origin = %q，期望保持 %q", got.Origin, SeenFromWorkshop)
	}
	if got.FirstSeenAt.After(got.LastSeenAt) {
		t.Errorf("first_seen_at %v 晚于 last_seen_at %v", got.FirstSeenAt, got.LastSeenAt)
	}
}

// TestWorkshopVersionIsNotClobberedByDiskScan 盯的是"工坊更新了没"这条提示的立足点。
//
// 两个版本以前挤在同一列：打开一次模组列表就是一次扫盘，扫盘把磁盘那份写进 version，
// 于是"工坊那边写的版本"这个事实在界面上任何一次刷新之后都不存在了，
// 徽标和「删文件重下」按钮永远不亮——一个永远为空的提示比没有提示更糟。
func TestWorkshopVersionIsNotClobberedByDiskScan(t *testing.T) {
	s := newSeenStore(t)
	if err := s.RecordSeen(SeenMod{WorkshopID: "378160973", Name: "Epic Health Bar",
		Version: "9.9", Origin: SeenFromWorkshop}); err != nil {
		t.Fatalf("RecordSeen 在线那趟: %v", err)
	}
	// 扫盘这一趟：磁盘上这份是 1.7。它该改掉 version，不该碰到 workshop_version。
	if err := s.RecordSeen(SeenMod{WorkshopID: "378160973", Name: "Epic Health Bar",
		Version: "1.7", Origin: SeenFromDisk}); err != nil {
		t.Fatalf("RecordSeen 扫盘那趟: %v", err)
	}
	got, ok, err := s.GetSeen("378160973")
	if err != nil || !ok {
		t.Fatalf("GetSeen: ok=%v err=%v", ok, err)
	}
	if got.Version != "1.7" {
		t.Errorf("version = %q，磁盘读到的那份该覆盖缓存里的旧值", got.Version)
	}
	if got.WorkshopVersion != "9.9" {
		t.Errorf("workshop_version = %q，被扫盘刷掉了（两个版本又变成同一个数，提示永远不亮）", got.WorkshopVersion)
	}

	// 在线那趟没带版本（接口只回 ID 不回 version 是常态）时，上一个工坊版本要留着。
	if err := s.RecordSeen(SeenMod{WorkshopID: "378160973", Name: "Epic Health Bar",
		Subscriptions: 3720000, Origin: SeenFromWorkshop}); err != nil {
		t.Fatalf("RecordSeen 无版本那趟: %v", err)
	}
	got, _, err = s.GetSeen("378160973")
	if err != nil {
		t.Fatalf("GetSeen: %v", err)
	}
	if got.WorkshopVersion != "9.9" {
		t.Errorf("workshop_version = %q，在线那趟没带版本时该保留已知的 9.9", got.WorkshopVersion)
	}

	// 只被扫盘见过、从没在线检索过的条目没有工坊版本可比，必须是空串：
	// 界面拿到空串就不显示"版本对不上"，而不是拿磁盘那个数自己和自己比。
	if err := s.RecordSeen(SeenMod{WorkshopID: "900000001", Name: "只在磁盘上",
		Version: "1.0", Origin: SeenFromDisk}); err != nil {
		t.Fatalf("RecordSeen 只有磁盘: %v", err)
	}
	got, _, err = s.GetSeen("900000001")
	if err != nil {
		t.Fatalf("GetSeen: %v", err)
	}
	if got.WorkshopVersion != "" {
		t.Errorf("workshop_version = %q，磁盘来源不该往这一列里写东西", got.WorkshopVersion)
	}
	if got.Version != "1.0" {
		t.Errorf("version = %q，扫盘读到的版本该照常记下来", got.Version)
	}
}

// TestRecordSeenFlagsAreMonotonic 盯的是提醒位：一旦知道"客户端也要订阅"，
// 后续任何一次信息更少的写入都不该把这个提醒悄悄取消掉。
func TestRecordSeenFlagsAreMonotonic(t *testing.T) {
	s := newSeenStore(t)
	if err := s.RecordSeen(SeenMod{WorkshopID: "123", Name: "A",
		ClientRequired: true, HasChildren: true, Origin: SeenFromWorkshop}); err != nil {
		t.Fatalf("RecordSeen: %v", err)
	}
	if err := s.RecordSeen(SeenMod{WorkshopID: "123", Name: "A", Origin: SeenFromDisk}); err != nil {
		t.Fatalf("RecordSeen 第二次: %v", err)
	}
	got, ok, err := s.GetSeen("123")
	if err != nil || !ok {
		t.Fatalf("GetSeen: ok=%v err=%v", ok, err)
	}
	if !got.ClientRequired || !got.HasChildren {
		t.Errorf("提醒位被抹掉了：client_required=%v has_children=%v", got.ClientRequired, got.HasChildren)
	}
}

func TestRecordSeenRejectsMissingID(t *testing.T) {
	s := newSeenStore(t)
	if err := s.RecordSeen(SeenMod{WorkshopID: "  ", Name: "没有 ID"}); err == nil {
		t.Fatal("没有工坊 ID 的条目写进了缓存，这种行检索永远命中不了，只会占地方")
	}
}

// TestSearchSeenEscapesLikeWildcards 是搜索框里的经典坑：
// 用户打一个 % 想搜"50% 堆叠"，不转义的话 LIKE '%%%' 会把整张表都端出来，
// 于是"搜不到"变成"搜出所有"，比空结果更让人困惑。
func TestSearchSeenEscapesLikeWildcards(t *testing.T) {
	s := newSeenStore(t)
	if err := s.RecordSeen(
		SeenMod{WorkshopID: "1", Name: "50%堆叠", Subscriptions: 10},
		SeenMod{WorkshopID: "2", Name: "普通模组", Subscriptions: 20},
		SeenMod{WorkshopID: "3", Name: "A_B", Subscriptions: 5},
	); err != nil {
		t.Fatalf("RecordSeen: %v", err)
	}

	hits, err := s.SearchSeen("%", 10)
	if err != nil {
		t.Fatalf("SearchSeen: %v", err)
	}
	if len(hits) != 1 || hits[0].WorkshopID != "1" {
		t.Errorf("搜 %% 命中 %d 条 %v，期望只有名字里带 %% 的那条", len(hits), namesOf(hits))
	}
	hits, err = s.SearchSeen("_", 10)
	if err != nil {
		t.Fatalf("SearchSeen _: %v", err)
	}
	if len(hits) != 1 || hits[0].Name != "A_B" {
		t.Errorf("搜 _ 命中 %v，期望只有名字里带下划线的那条", namesOf(hits))
	}
	hits, err = s.SearchSeen(`\`, 10)
	if err != nil {
		t.Fatalf("SearchSeen 反斜杠: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("搜反斜杠命中了 %v，转义符本身没被当字面量", namesOf(hits))
	}
}

// TestSearchSeenByExactID 让用户可以直接把工坊 ID 粘进搜索框。
func TestSearchSeenByExactID(t *testing.T) {
	s := newSeenStore(t)
	if err := s.RecordSeen(SeenMod{WorkshopID: "351210508", Name: "几何防御", Subscriptions: 1}); err != nil {
		t.Fatalf("RecordSeen: %v", err)
	}
	hits, err := s.SearchSeen("351210508", 10)
	if err != nil {
		t.Fatalf("SearchSeen: %v", err)
	}
	if len(hits) != 1 || hits[0].Name != "几何防御" {
		t.Fatalf("按 ID 搜命中 %v，期望几何防御", namesOf(hits))
	}
}

// TestSearchSeenEmptyQueryOrdersByPopularity：没配 key 时界面靠这条列出"见过的模组"，
// 顺序错了就等于把一个没人用的模组推到最前面。
func TestSearchSeenEmptyQueryOrdersByPopularity(t *testing.T) {
	s := newSeenStore(t)
	if err := s.RecordSeen(
		SeenMod{WorkshopID: "1", Name: "冷门", Subscriptions: 3},
		SeenMod{WorkshopID: "2", Name: "热门", Subscriptions: 900},
		SeenMod{WorkshopID: "3", Name: "中等", Subscriptions: 40},
	); err != nil {
		t.Fatalf("RecordSeen: %v", err)
	}
	hits, err := s.SearchSeen("", 10)
	if err != nil {
		t.Fatalf("SearchSeen: %v", err)
	}
	if names := namesOf(hits); strings.Join(names, ",") != "热门,中等,冷门" {
		t.Errorf("空搜索词的排序 = %v，期望按订阅数降序", names)
	}
	narrow, err := s.SearchSeen("", 2)
	if err != nil {
		t.Fatalf("SearchSeen limit: %v", err)
	}
	if len(narrow) != 2 {
		t.Errorf("limit 没生效，返回 %d 条", len(narrow))
	}
}

// TestSearchSeenPagesCoverTheWholeCache 钉的是分页的地基：界面上那个"下一页"
// 之前点了没反应，就是因为缓存这条路既不吃 offset 也没人给总数。
// 这里逐页扫一遍，要求不漏（并集=全部）、不重（每页 ID 互不相同）、总数对得上。
func TestSearchSeenPagesCoverTheWholeCache(t *testing.T) {
	s := newSeenStore(t)
	// 故意放两条订阅数相同的：并列时若没有兜底排序，同一行可能在两页都出现。
	if err := s.RecordSeen(
		SeenMod{WorkshopID: "1", Name: "堆叠", Subscriptions: 900},
		SeenMod{WorkshopID: "2", Name: "装备栏", Subscriptions: 900},
		SeenMod{WorkshopID: "3", Name: "小地图", Subscriptions: 40},
		SeenMod{WorkshopID: "4", Name: "几何防御", Subscriptions: 3},
		SeenMod{WorkshopID: "5", Name: "服务器面板助手", Subscriptions: 0},
	); err != nil {
		t.Fatalf("RecordSeen: %v", err)
	}
	total, err := s.CountSeen("", "")
	if err != nil {
		t.Fatalf("CountSeen: %v", err)
	}
	if total != 5 {
		t.Fatalf("CountSeen = %d，期望 5", total)
	}

	seen := map[string]int{}
	for page := 1; (page-1)*2 < total; page++ {
		rows, err := s.SearchSeenPage("", "", 2, (page-1)*2)
		if err != nil {
			t.Fatalf("第 %d 页: %v", page, err)
		}
		for _, r := range rows {
			if before, dup := seen[r.WorkshopID]; dup {
				t.Errorf("ID %s 在第 %d 页和第 %d 页各出现一次，翻页会重复", r.WorkshopID, before, page)
			}
			seen[r.WorkshopID] = page
		}
	}
	if len(seen) != 5 {
		t.Errorf("逐页扫下来只见到 %d 条，期望 5 条（有行被翻页跳过了）", len(seen))
	}

	// 带搜索词时总数要跟着搜索词走，否则"共 5 条"会骗人。
	n, err := s.CountSeen("堆叠", "")
	if err != nil {
		t.Fatalf("CountSeen(堆叠): %v", err)
	}
	if n != 1 {
		t.Errorf("CountSeen(堆叠) = %d，期望 1", n)
	}
}

// TestSeenTagFilterMatchesWholeTag 钉的是分类筛选的地基本身：
// tags 列是一段 JSON 文本，按"带引号的整个标签"匹配才不会搜 item 把 item_manager
// 一起捞进来；大小写要跟着 SQLite 的 LIKE（对 ASCII 不敏感），因为工坊那边
// 实测同一个分类有人写 other、有人写 Other（1200 条样本里 294 次 / 52 次），
// 分开算会让"其它"这一列少掉一半。
func TestSeenTagFilterMatchesWholeTag(t *testing.T) {
	s := newSeenStore(t)
	if err := s.RecordSeen(
		SeenMod{WorkshopID: "1", Name: "堆叠类", Tags: []string{"item", "utility"}},
		SeenMod{WorkshopID: "2", Name: "别的东西", Tags: []string{"item_manager"}},
		SeenMod{WorkshopID: "3", Name: "大写分类", Tags: []string{"Other"}},
		SeenMod{WorkshopID: "4", Name: "小写分类", Tags: []string{"other", "version:1.6"}},
	); err != nil {
		t.Fatalf("RecordSeen: %v", err)
	}

	hits, err := s.SearchSeenPage("", "item", 10, 0)
	if err != nil {
		t.Fatalf("按 item 筛: %v", err)
	}
	if len(hits) != 1 || hits[0].WorkshopID != "1" {
		t.Errorf("按 item 筛命中 %v，期望只有 tags 里真有 item 这一条（不能把 item_manager 捞进来）", namesOf(hits))
	}
	hits, err = s.SearchSeenPage("", "other", 10, 0)
	if err != nil {
		t.Fatalf("按 other 筛: %v", err)
	}
	if len(hits) != 2 {
		t.Errorf("按 other 筛命中 %v，期望大小写两种写法都算同一类", namesOf(hits))
	}
	n, err := s.CountSeen("", "other")
	if err != nil {
		t.Fatalf("CountSeen other: %v", err)
	}
	if n != 2 {
		t.Errorf("CountSeen(other) = %d，和上面那页的 %d 条对不上，界面就会出现共 N 条却翻不出内容", n, len(hits))
	}
	// 分类和关键词是叠在一起的：只认其中一个都会让"共几条"和这一页脱节。
	n, err = s.CountSeen("堆叠", "item")
	if err != nil {
		t.Fatalf("CountSeen(堆叠,item): %v", err)
	}
	if n != 1 {
		t.Errorf("CountSeen(堆叠,item) = %d，期望 1", n)
	}
	// 反例要挑一条这一行真没有的分类：上面那条"堆叠类"同时带 item 和 utility，
	// 用 utility 当"分类不对"的例子会真的命中，测的就成了恒真。
	if n, err := s.CountSeen("堆叠", "character"); err != nil || n != 0 {
		t.Errorf("关键词对得上但分类不对时 CountSeen = %d err = %v，期望 0 条", n, err)
	}
}

// TestSeenFacetsCountsPerCategory 是那一排分类标签上的数字。
// 它们必须只跟着关键词走、不跟着"当前选了哪一类"走——否则选中之外的那些列全变 0，
// 用户没法从"人物 12"跳到"环境 5"，只能一层层退回去。
func TestSeenFacetsCountsPerCategory(t *testing.T) {
	s := newSeenStore(t)
	if err := s.RecordSeen(
		SeenMod{WorkshopID: "1", Name: "扩容堆叠", Tags: []string{"item", "utility"}},
		SeenMod{WorkshopID: "2", Name: "新人物", Tags: []string{"character"}},
		SeenMod{WorkshopID: "3", Name: "环境调整", Tags: []string{"environment", "tweak"}},
		SeenMod{WorkshopID: "4", Name: "别的房间的扩容", Tags: []string{"item"}},
	); err != nil {
		t.Fatalf("RecordSeen: %v", err)
	}
	tags := []string{"item", "character", "environment", "tweak", "utility", "pet"}
	got, err := s.SeenFacets("", tags)
	if err != nil {
		t.Fatalf("SeenFacets: %v", err)
	}
	want := map[string]int{"item": 2, "character": 1, "environment": 1, "tweak": 1, "utility": 1, "pet": 0}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("分类 %s 计数 = %d，期望 %d", k, got[k], w)
		}
	}
	// 关键词收窄之后每一列都要跟着收窄，且和"这一类有几条"（CountSeen）同一个口径。
	narrow, err := s.SeenFacets("扩容", tags)
	if err != nil {
		t.Fatalf("SeenFacets(扩容): %v", err)
	}
	if narrow["item"] != 2 || narrow["character"] != 0 {
		t.Errorf("SeenFacets(扩容) = %v，期望 item 2 条、character 0 条", narrow)
	}
	n, err := s.CountSeen("扩容", "item")
	if err != nil {
		t.Fatalf("CountSeen(扩容,item): %v", err)
	}
	if n != narrow["item"] {
		t.Errorf("界面上那一列写 %d、点进去分页说共 %d 条，两个数来自两套条件是错的", narrow["item"], n)
	}
	// 字典里没有的分类（pet 那种一条都没有的）要出现且为 0，不能靠调用方猜键不在 map 里。
	if _, ok := narrow["pet"]; !ok {
		t.Errorf("SeenFacets 漏了没有命中的分类，界面那一列只能显示成空")
	}
}

// TestRecordSeenVoteFields 盯的是星星那几个数：收藏数和投票人数是累计量（只增不减），
// 好评率是一次读数，必须"这次真读到了才换"。
// 为什么不能对好评率用 MAX：模组后来掉分是常事，留历史最高分就是在替作者吹。
func TestRecordSeenVoteFields(t *testing.T) {
	s := newSeenStore(t)
	if err := s.RecordSeen(SeenMod{WorkshopID: "9", Name: "评分模组",
		Favorited: 118000, VoteScore: 0.94, VotesTotal: 3200, Origin: SeenFromWorkshop}); err != nil {
		t.Fatalf("RecordSeen: %v", err)
	}
	// 磁盘扫描没有这些数：不能把星星抹掉。
	if err := s.RecordSeen(SeenMod{WorkshopID: "9", Name: "评分模组", Origin: SeenFromDisk}); err != nil {
		t.Fatalf("RecordSeen 磁盘: %v", err)
	}
	got, ok, err := s.GetSeen("9")
	if err != nil || !ok {
		t.Fatalf("GetSeen: ok=%v err=%v", ok, err)
	}
	if got.Favorited != 118000 || got.VoteScore != 0.94 || got.VotesTotal != 3200 {
		t.Fatalf("星星那三个数被抹掉了：%d / %v / %d", got.Favorited, got.VoteScore, got.VotesTotal)
	}
	// 第二次在线读数：分数掉了要跟着掉，人数只增。
	if err := s.RecordSeen(SeenMod{WorkshopID: "9", Name: "评分模组",
		Favorited: 120000, VoteScore: 0.71, VotesTotal: 3100, Origin: SeenFromWorkshop}); err != nil {
		t.Fatalf("RecordSeen 第二次在线: %v", err)
	}
	got, _, err = s.GetSeen("9")
	if err != nil {
		t.Fatalf("GetSeen: %v", err)
	}
	if got.VoteScore != 0.71 {
		t.Errorf("好评率 = %v，期望跟着最新读数掉到 0.71（取大的那个就是替作者吹）", got.VoteScore)
	}
	if got.Favorited != 120000 {
		t.Errorf("收藏数 = %d，期望 120000", got.Favorited)
	}
	if got.VotesTotal != 3200 {
		t.Errorf("投票人数 = %d，期望 3200（累计量只增不减）", got.VotesTotal)
	}
}

func TestGetSeenMissingIsNotAnError(t *testing.T) {
	s := newSeenStore(t)
	if _, ok, err := s.GetSeen("424242"); ok || err != nil {
		t.Errorf("查不存在的 ID：ok=%v err=%v，期望 ok=false 且不是错误", ok, err)
	}
}

func TestSeenCountReflectsCache(t *testing.T) {
	s := newSeenStore(t)
	if n, err := s.SeenCount(); err != nil || n != 0 {
		t.Fatalf("空缓存 SeenCount = %d err = %v", n, err)
	}
	if err := s.RecordSeen(SeenMod{WorkshopID: "1"}, SeenMod{WorkshopID: "2"}); err != nil {
		t.Fatalf("RecordSeen: %v", err)
	}
	n, err := s.SeenCount()
	if err != nil {
		t.Fatalf("SeenCount: %v", err)
	}
	if n != 2 {
		t.Errorf("SeenCount = %d，期望 2", n)
	}
}

func namesOf(list []SeenMod) []string {
	out := make([]string, 0, len(list))
	for _, m := range list {
		out = append(out, m.Name)
	}
	return out
}
