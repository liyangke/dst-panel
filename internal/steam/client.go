// Package steam 是 Steam 官方 Web API 的只读客户端，面板用它查工坊模组元数据。
//
// 三条约束决定了这个包只有这么点表面：
//
//   - 只读。订阅、上传、评论都要登录 steamcommunity，面板一个接口都不提供，
//     所以也就不需要存任何用户凭据，只要一个只读 key。
//   - 参数是拼出来的，不是转义出来的。appid、language、返回字段全部写死，
//     用户只能决定搜索词和页码；搜索词进 query 参数（url.Values.Encode），
//     永远不会变成 URL 路径的一部分，也就没有 SSRF 的形态。
//   - key 绝不出现在任何对外可见的文本里。Go 的 http 错误会把完整 URL 带进 error
//     （"Get \"https://...?key=SECRET\": dial tcp ..."），直接返回给用户就等于把凭据
//     发到界面上。所以这里所有 Cause 都先过 scrubKey，再进 Error.Conclusion。
//
// 响应结构是按 2026-10-30 用真 key 打真接口抓下来的样本写的（见 .tools/steam_probe.py）：
// QueryFiles 的 response 里只有 publishedfiledetails 和 total，**没有 success 字段**，
// 所以成败只能看 HTTP 状态码；publishedfileid 有时是字符串有时是数字，用 flexID 兜住。
package steam

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"dstpanel/internal/mods"
)

// WorkshopAppID 是饥荒联机版本体在 Steam 上的 appid，工坊条目都挂在它下面。
// 注意专用服务端（工具）是 343050（dst.ServerAppID），两者不是一回事：
// 模组是给游戏本体的，用 343050 去搜会一条都没有。
const WorkshopAppID = "322330"

// languageSimplifiedChinese 是 Steam 的 ELanguage 枚举里简体中文的值。
// 实测带上它之后 title 会直接返回中文名（"更多物品堆叠"），面板就不用自己再翻一遍。
const languageSimplifiedChinese = "6"

const defaultBaseURL = "https://api.steampowered.com"

// ErrBadKey 表示 Steam 认定这个 key 不可用（HTTP 403）。
// 接口层要单独认它：这时候"重试"没有意义，该做的是让用户去换 key。
var ErrBadKey = errors.New("WebAPIKey 无效")

// Error 把"给用户看的一句话"和"给日志看的原始原因"分开。
//
// Error() 只回 Conclusion（中文、可直接上界面），Cause 留着写运行日志。
// 之前踩过一次：把外部原因原样拼进响应，界面就成了 Go 英文串的展厅。
type Error struct {
	// Status 是 HTTP 状态码；网络层失败时为 0。
	Status int
	// Conclusion 是能直接显示给用户的中文结论。
	Conclusion string
	Cause      error
}

func (e *Error) Error() string { return e.Conclusion }

// Unwrap 让 errors.Is(err, ErrBadKey) 在 403 时成立。
func (e *Error) Unwrap() error { return e.Cause }

// Item 是一个工坊模组条目。
type Item struct {
	ID             string   `json:"workshopId"`
	Name           string   `json:"name"`
	Desc           string   `json:"desc"`
	Tags           []string `json:"tags,omitempty"`
	Version        string   `json:"version"`
	ClientRequired bool     `json:"clientRequired"`
	ServerOnly     bool     `json:"serverOnly"`
	Subscriptions  int64    `json:"subscriptions"`
	// Favorited 是收藏数（工坊星星对应的那个）。0 表示不知道，不表示没人收藏——
	// 从磁盘扫进来的模组没有这两个数。
	Favorited int64 `json:"favorited"`
	// VoteScore 是工坊好评率，0~1；VotesTotal 是参与评分的人数。两个一起才有意义：
	// 只有 98% 没有分母，用户分不清是 2 万人评的还是 20 人评的。
	VoteScore  float64 `json:"voteScore"`
	VotesTotal int64   `json:"votesTotal"`
	// ModUpdatedAt 是模组自己在工坊的最后更新时间（Unix 秒），0 表示接口没给。
	ModUpdatedAt int64  `json:"modUpdatedAt"`
	PreviewURL   string `json:"previewUrl"`
	FileSize     int64  `json:"fileSize"`
	// HasChildren 表示它带了子模组（依赖）。登记时要一起把子模组写进清单，
	// 否则服务端启动会因为缺文件而报错。
	HasChildren bool `json:"hasChildren"`
}

// Page 是一次关键词检索的结果。
type Page struct {
	Items []Item
	// Total 是工坊里匹配的总条数（用来算还有几页），接口没给时为 0。
	Total int
}

// Client 是 Web API 客户端。零值不可用，用 New 建；nil 也可以安全调用 Enabled。
type Client struct {
	Key     string
	BaseURL string
	HTTP    *http.Client
}

// New 造一个客户端。proxy 为空时跟随环境变量（HTTPS_PROXY），
// 非空时所有请求都走它——国内机房直连 api.steampowered.com 经常不通，这是唯一的出路。
func New(key, proxy string) *Client {
	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConns:        4,
	}
	if p := strings.TrimSpace(proxy); p != "" {
		if u, err := url.Parse(p); err == nil && u.Scheme != "" {
			tr.Proxy = func(*http.Request) (*url.URL, error) { return u, nil }
		}
	}
	return &Client{
		Key:     strings.TrimSpace(key),
		BaseURL: defaultBaseURL,
		HTTP:    &http.Client{Timeout: 15 * time.Second, Transport: tr},
	}
}

func (c *Client) Enabled() bool { return c != nil && c.Key != "" }

func (c *Client) base() string {
	if c == nil || c.BaseURL == "" {
		return defaultBaseURL
	}
	return strings.TrimRight(c.BaseURL, "/")
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// Search 向工坊要"这一页的榜单"：给了关键词就是搜，什么关键词都不给就是全量榜。
// page 从 1 开始，size 上限 20（接口能更多，但界面一屏画不了）。
//
// "空关键词也能翻"是 2026-11-03 在云机打真接口钉下来的（.tools/workshop_browse_probe.py）：
// 不带 search_text 时 HTTP 200、total=27313、page=1/2/3 三页 ID 零重叠，
// 而 offset / start / sort_by 三种参数全被忽略（同一页换个参数还是那 10 条）。
// 所以翻页只能用 page，而且顺序是工坊自己那一个榜，我们既改不了也不该假装能改。
func (c *Client) Search(ctx context.Context, text string, page, size int) (Page, error) {
	if !c.Enabled() {
		return Page{}, fmt.Errorf("没有配置 steamApi.webApiKey，面板不去猜一个 key 用；填好之后重启面板，或者直接用工坊 ID 登记")
	}
	if page < 1 {
		page = 1
	}
	if size < 1 || size > 20 {
		size = 10
	}
	q := url.Values{
		"appid":            {WorkshopAppID},
		"page":             {strconv.Itoa(page)},
		"numperpage":       {strconv.Itoa(size)},
		"language":         {languageSimplifiedChinese},
		"return_tags":      {"true"},
		"return_children":  {"true"},
		"return_vote_data": {"true"},
		"include_children": {"true"},
	}
	// 空关键词时干脆不发这个参数：探针就是这么发着验出"能翻"的（见上），
	// 发一个空值是一条没验过的路，面板不走在没验过的路上。
	if t := strings.TrimSpace(text); t != "" {
		q.Set("search_text", t)
	}
	raw, err := c.get(ctx, "/IPublishedFileService/QueryFiles/v1/", q)
	if err != nil {
		return Page{}, err
	}
	var resp envelope
	if err := json.Unmarshal(raw, &resp); err != nil {
		return Page{}, &Error{Status: http.StatusOK,
			Conclusion: "Steam 返回的内容不是预期的 JSON（可能被网络出口改写了），这一页结果没有采信",
			Cause:      err}
	}
	out := Page{Total: resp.Response.Total}
	for _, it := range resp.Response.Items {
		if usable(it) {
			out.Items = append(out.Items, it.toItem())
		}
	}
	return out, nil
}

// Browse 一次取回一批（最多 100 条）工坊结果，给"按分类筛"那条路用。
//
// 为什么不按分类直接问 Steam：实测 QueryFiles 的 matched_tag / exclude_tag / required_tags
// 全被忽略——同一批 ID、同一个 total，带不带标签参数一模一样（见 .tools/tmp/tag_filter_probe4.py）。
// 那就只剩两种做法：把当前这一屏的 10 条在前端藏掉几条（"共 N 条"立刻变成假的），
// 或者一次多取一些、在我们这一头筛完再切页。这里选后者，
// 并且筛的范围（这一批多少条）必须原样回给界面写进那句话里，不能含糊成"工坊里共 N 条"。
func (c *Client) Browse(ctx context.Context, text string, limit int) ([]Item, error) {
	if !c.Enabled() {
		return nil, fmt.Errorf("没有配置 steamApi.webApiKey，面板不去猜一个 key 用；填好之后重启面板，或者直接用工坊 ID 登记")
	}
	if limit < 1 {
		limit = 1
	}
	q := url.Values{
		"appid":            {WorkshopAppID},
		"page":             {"1"},
		"numperpage":       {strconv.Itoa(min(limit, 100))},
		"language":         {languageSimplifiedChinese},
		"return_tags":      {"true"},
		"return_children":  {"true"},
		"return_vote_data": {"true"},
		"include_children": {"true"},
	}
	if t := strings.TrimSpace(text); t != "" {
		q.Set("search_text", t)
	}
	raw, err := c.get(ctx, "/IPublishedFileService/QueryFiles/v1/", q)
	if err != nil {
		return nil, err
	}
	var resp envelope
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, &Error{Status: http.StatusOK,
			Conclusion: "Steam 返回的内容不是预期的 JSON（可能被网络出口改写了），这一页结果没有采信",
			Cause:      err}
	}
	out := make([]Item, 0, len(resp.Response.Items))
	for _, it := range resp.Response.Items {
		if usable(it) {
			out = append(out, it.toItem())
		}
	}
	return out, nil
}

// Details 按工坊 ID 取详情。用户只知道自己抄了一串数字时走这条，
// 它同时是"登记前先看一眼这是什么"的那一步。
func (c *Client) Details(ctx context.Context, ids ...string) ([]Item, error) {
	if !c.Enabled() {
		return nil, fmt.Errorf("没有配置 steamApi.webApiKey，面板查不到这串 ID 对应的名字；登记本身不受影响（工坊 ID 就是凭据）")
	}
	q := url.Values{
		"appid":            {WorkshopAppID},
		"language":         {languageSimplifiedChinese},
		"include_children": {"true"},
		"return_tags":      {"true"},
	}
	kept := make([]string, 0, len(ids))
	for _, id := range ids {
		// 规则和"写进 Lua 清单"用的是同一个 mods.ValidWorkshopID：
		// 这里放宽一寸，界面上就会有一处"搜到了却登记不了"。
		if mods.ValidWorkshopID(strings.TrimSpace(id)) == nil {
			kept = append(kept, strings.TrimSpace(id))
		}
	}
	if len(kept) == 0 {
		return nil, fmt.Errorf("这些工坊 ID 一个都不合法，面板不会把没核过的字符串拼进请求")
	}
	for i, id := range kept {
		q.Set(fmt.Sprintf("publishedfileids[%d]", i), id)
	}
	raw, err := c.get(ctx, "/IPublishedFileService/GetDetails/v1/", q)
	if err != nil {
		return nil, err
	}
	var resp envelope
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, &Error{Conclusion: "Steam 返回的内容不是预期的 JSON，这一页结果没有采信", Cause: err}
	}
	out := make([]Item, 0, len(resp.Response.Items))
	for _, it := range resp.Response.Items {
		// GetDetails 对不存在/已被封的条目会回 result!=1，这种条目没有 title，必须滤掉。
		if usable(it) {
			out = append(out, it.toItem())
		}
	}
	return out, nil
}

func (c *Client) get(ctx context.Context, path string, q url.Values) ([]byte, error) {
	q.Set("key", c.Key)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base()+path+"?"+q.Encode(), nil)
	if err != nil {
		return nil, &Error{Conclusion: "构造 Steam 接口请求失败：" + scrubKey(err.Error()), Cause: err}
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		// 这一条最容易泄漏 key：http 的出错文本带完整 URL。
		cause := transportCause(err)
		if errors.Is(err, context.Canceled) {
			// 用户关掉页面或切了搜索词，不是机房网络故障。这里如实返回取消，
			// 让上层连 Note 都不用写——把"你自己取消了"说成"Steam 连不上"是假诊断。
			return nil, &Error{Conclusion: "这次检索已经取消，不需要再报", Cause: cause}
		}
		return nil, &Error{
			Conclusion: "连不上 Steam 的 Web API（api.steampowered.com）。国内机房通常要走代理：" +
				"在配置里填 steamApi.proxy，或者确认这台机器能直连。",
			Cause: cause,
		}
	}
	defer resp.Body.Close()
	// 403 的正文是一段 HTML（实测），既没有信息量又可能很长，读固定字节数就够。
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, &Error{Status: resp.StatusCode,
			Conclusion: "读 Steam 的响应读到一半断了，稍后再试", Cause: err}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, statusError(resp.StatusCode, raw)
	}
	return raw, nil
}

// statusError 把 Steam 的错误状态码翻成能照着做的中文结论。
// 这一段是实测对出来的：403 返回 HTML 而不是 JSON，所以绝不能先解 JSON 再看状态。
func statusError(code int, raw []byte) error {
	cause := errors.New(scrubKey(strings.TrimSpace(string(raw))))
	switch code {
	case http.StatusForbidden:
		return &Error{Status: code, Cause: fmt.Errorf("%w: %v", ErrBadKey, cause),
			Conclusion: "Steam 拒绝了这个 key（HTTP 403）：无效、已被删除，或者不属于当前网络出口。" +
				"到 steamcommunity 的开发者页面重新生成一个再填进配置，重试没有用"}
	case http.StatusTooManyRequests:
		return &Error{Status: code, Cause: cause,
			Conclusion: "Steam 对这个 key 限流了（HTTP 429），等一分钟再搜"}
	case http.StatusUnauthorized:
		return &Error{Status: code, Cause: fmt.Errorf("%w: %v", ErrBadKey, cause),
			Conclusion: "Steam 不认这个 key（HTTP 401），到开发者页面确认它还在"}
	default:
		return &Error{Status: code, Cause: cause,
			Conclusion: fmt.Sprintf("Steam 的接口返回了 HTTP %d，面板没法完成检索；稍后再试，或者改用工坊 ID 直接登记", code)}
	}
}

// transportCause 把 Go 网络错误变成"能写日志、又不泄密、还认得出取消"的形状。
//
// 出错文本形如 Get "https://...?key=SECRET": dial tcp ...：
// 前半段带 URL，必须洗掉再留；只保留引号后面那段原因。
// 但直接 errors.New 会把 context.Canceled / DeadlineExceeded 的链断掉，
// 上层就分不清"用户取消了"和"机房出不去"，所以这两个哨兵错误用 %w 重新接上。
func transportCause(err error) error {
	msg := scrubKey(err.Error())
	if i := strings.LastIndex(msg, `": `); i >= 0 {
		msg = msg[i+3:]
	}
	base := errors.New(msg)
	switch {
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("%w: %w", base, context.Canceled)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%w: %w", base, context.DeadlineExceeded)
	}
	return base
}

// scrubKey 把任何文本里的 key 值替换掉。宁可把日志弄脏一点也不留凭据。
// 替换串里不能再带上 $1（那正好是被洗的那个值）——第一次就栽在这里。
func scrubKey(s string) string {
	if s == "" {
		return s
	}
	return keyInQuery.ReplaceAllString(s, "key=***")
}

var keyInQuery = regexp.MustCompile(`(?i)key=([^&"'\\ ]+)`)

// ValidWorkshopID 的判定只有一份，在 mods 包里：那串数字最终要写进
// dedicated_server_mods_setup.lua，能写进 Lua 的形状和能发给接口的形状必须是同一个约束。

// flexID 兜住 publishedfileid 既可能是字符串又可能是数字这一件事。
type flexID string

func (f *flexID) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" {
		return nil
	}
	if len(s) > 1 && s[0] == '"' {
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*f = flexID(v)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("工坊 ID 既不是数字也不是字符串：%s", s)
	}
	*f = flexID(n.String())
	return nil
}

// flexInt64 兜住"数字或数字字符串"。这不只是防御性写法：2026-11-01 真接口回来的
// file_size 就是字符串，按 int64 硬解会让整个响应判为"不是预期的 JSON"，
// 于是一次成功的在线检索在界面上变成"在线没成功，下面是缓存老结果"。
// 同一个对象里的 publishedfileid 早就证明过这件事（见 flexID）。
type flexInt64 int64

func (n *flexInt64) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" || s == `""` {
		*n = 0
		return nil
	}
	if len(s) > 1 && s[0] == '"' {
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		s = strings.TrimSpace(v)
		if s == "" {
			*n = 0
			return nil
		}
	}
	i, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("工坊返回的数字字段读不出：%s", s)
	}
	*n = flexInt64(i)
	return nil
}

type envelope struct {
	Response struct {
		Total int       `json:"total"`
		Items []rawItem `json:"publishedfiledetails"`
	} `json:"response"`
}
type rawItem struct {
	ID              flexID `json:"publishedfileid"`
	Title           string `json:"title"`
	FileDescription string `json:"file_description"`
	Tags            []struct {
		Tag string `json:"tag"`
	} `json:"tags"`
	Subscriptions flexInt64 `json:"subscriptions"`
	// Favorited 是"收藏过这个模组"的人数。它和订阅数是两个数（实测 Epic Healthbar：
	// 订阅 372 万、收藏 11.8 万），工坊卡片上那颗星星对应的就是它，所以两个都要留。
	Favorited flexInt64 `json:"favorited"`
	// VoteData 是工坊的好评率与投票数。QueryFiles 要带 return_vote_data=true 才给
	// （Search 那边已经带了），实测 score 是 0~1 的小数、votes_up/down 是整数。
	VoteData struct {
		Score     float64   `json:"score"`
		VotesUp   flexInt64 `json:"votes_up"`
		VotesDown flexInt64 `json:"votes_down"`
	} `json:"vote_data"`
	TimeUpdated flexInt64 `json:"time_updated"`
	PreviewURL  string    `json:"preview_url"`
	FileSize    flexInt64 `json:"file_size"`
	NumChildren flexInt64 `json:"num_children"`
	Result      flexInt64 `json:"result"`
	Banned      bool      `json:"banned"`
}

// usable 滤掉被 ban 的、以及 GetDetails 里 result 不是 OK(1) 的条目。
// QueryFiles 不带 result 字段（值为 0），所以 0 也算可用。
func usable(it rawItem) bool {
	if it.Banned || strings.TrimSpace(string(it.ID)) == "" {
		return false
	}
	return it.Result == 0 || it.Result == 1
}
func (it rawItem) toItem() Item {
	out := Item{
		ID:            string(it.ID),
		Name:          it.Title,
		Desc:          it.FileDescription,
		Subscriptions: int64(it.Subscriptions),
		Favorited:     int64(it.Favorited),
		VoteScore:     it.VoteData.Score,
		VotesTotal:    int64(it.VoteData.VotesUp) + int64(it.VoteData.VotesDown),
		ModUpdatedAt:  int64(it.TimeUpdated),
		PreviewURL:    it.PreviewURL,
		FileSize:      int64(it.FileSize),
		HasChildren:   it.NumChildren > 0,
	}
	for _, t := range it.Tags {
		tag := strings.TrimSpace(t.Tag)
		if tag == "" {
			continue
		}
		out.Tags = append(out.Tags, tag)
		switch {
		case tag == "all_clients_require_mod":
			out.ClientRequired = true
		case tag == "server_only_mod":
			out.ServerOnly = true
		case strings.HasPrefix(tag, "version:"):
			out.Version = strings.TrimPrefix(tag, "version:")
		}
	}
	return out
}
