// 历史分页与导出：从整份日志里按行筛、按页给，或者流式导成文件。
//
// /log 那个接口只回看末尾 200~2000 行、8MB 窗口，更早的内容一律让用户去服务端
// grep。这一层补的正是"不用上机器也能翻历史"：
//   - History 按 offset/limit 分页，total/hasMore 说的是"整份文件筛完"的口径；
//   - Export 流式写 text/ndjson，不把整份文件读进内存再拼响应。
//
// 两者共用 walkHistory 这一趟扫描，筛选条件也和 /log 的 level/event/q 完全同一套，
// 免得"日志页筛得出、历史页筛不出"这种两边各写一份解析的老毛病。
package gamelog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// maxHistoryBytes 是单次扫描的字节上限。日志页窗口只有 8MB，历史查询放到 64MB：
// 再往上就该去归档里翻了，把几十 MB 一次性读进 4G 小机的内存不是查日志。
// 超限时只从末尾一段开始，并在结果里如实标 Truncated/Note，不假装 total 是全量。
const maxHistoryBytes = 64 * 1024 * 1024

// HistoryFilter 是历史查询的筛选条件，和 /log 的 level/event/q 一一对应。
type HistoryFilter struct {
	Level string // 级别下限，空串不过滤
	Event string // 事件码精确匹配，空串不过滤
	Text  string // 关键词（调用方已转小写），空串不过滤
}

func (f HistoryFilter) keep(e Entry) bool {
	if f.Level != "" && LevelRank(e.Level) < LevelRank(f.Level) {
		return false
	}
	if f.Event != "" && e.Event != f.Event {
		return false
	}
	if f.Text != "" && !strings.Contains(strings.ToLower(e.Raw+" "+e.Player+" "+e.UserID), f.Text) {
		return false
	}
	return true
}

// HistoryPage 是一次历史分页的结果。
type HistoryPage struct {
	Cluster   string `json:"cluster"`
	Shard     string `json:"shard"`
	Kind      string `json:"kind"`
	KindLabel string `json:"kindLabel"`
	Path      string `json:"path"`
	SizeBytes int64  `json:"sizeBytes"`
	// Offset/Limit 是这次实际用的分页参数（越界会被夹紧后回写，前端照它校准状态）。
	Offset int `json:"offset"`
	Limit  int `json:"limit"`
	// Total 是"整份文件筛选之后"的总行数，不是这一页的行数；HasMore/NextOffset 都基于它。
	Total      int  `json:"total"`
	HasMore    bool `json:"hasMore"`
	NextOffset int  `json:"nextOffset"`
	// Scanned 是从文件里读到的完整行数（含被筛掉的），和 Total 分开给：
	// "筛完只剩 2 条"和"这轮只写过 2 行"是两件事。
	Scanned int `json:"scanned"`
	// Truncated 为 true 表示文件超过单次上限，只查了末尾一段，Total 不是全量。
	Truncated bool    `json:"truncated"`
	Lines     []Entry `json:"lines"`
	StartAt   string  `json:"startAt,omitempty"`
	Note      string  `json:"note,omitempty"`
}

// historyMeta 是一趟扫描顺带攒下的文件信息。
type historyMeta struct {
	Path      string
	SizeBytes int64
	Scanned   int
	Truncated bool
	Start     time.Time
}

// walkHistory 流式扫一遍日志文件，把通过筛选的每一行交给 visit。
//
// 三个和 Tail/ReadSince 保持一致的地方：路径仍走 r.path（里面再校验一次名字，
// 不让请求方的 ../ 读到别的目录）；起始时间从文件头的 Current time 认；
// 末尾没有换行符的半行不消费（引擎还在写这一行，等下一轮它才完整）。
func (r *Reader) walkHistory(cluster, shard string, kind Kind, filter HistoryFilter,
	visit func(Entry) error) (historyMeta, error) {

	path, err := r.path(cluster, shard, kind)
	if err != nil {
		return historyMeta{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return historyMeta{}, fmt.Errorf("读取日志 %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return historyMeta{}, fmt.Errorf("读取日志 %s: %w", path, err)
	}
	meta := historyMeta{Path: path, SizeBytes: st.Size()}

	// 本轮启动的墙上时间：fillHeader 用 ReadAt，不挪动文件游标，这里借它一用。
	head := &Snapshot{Size: st.Size()}
	head.fillHeader(f)
	meta.Start = head.Start

	start := int64(0)
	if st.Size() > maxHistoryBytes {
		start = st.Size() - maxHistoryBytes
		meta.Truncated = true
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return meta, fmt.Errorf("定位日志 %s: %w", path, err)
	}
	br := bufio.NewReader(f)
	lineStart := start
	if start > 0 {
		// 起点大概率落在某一行中间：丢掉这半行，从下一个行首开始。
		discard, derr := br.ReadString('\n')
		lineStart += int64(len(discard))
		if derr != nil && derr != io.EOF {
			return meta, fmt.Errorf("读取日志 %s: %w", path, derr)
		}
		if derr == io.EOF {
			return meta, nil // 后半段连一个完整行都没有
		}
	}
	for {
		line, rerr := br.ReadString('\n')
		if len(line) > 0 && strings.HasSuffix(line, "\n") {
			meta.Scanned++
			text := strings.TrimSuffix(line, "\n")
			if e, ok := ParseLine(kind, lineStart, text); ok {
				if !meta.Start.IsZero() {
					one := []Entry{e}
					stampAt(one, meta.Start)
					e = one[0]
				}
				if filter.keep(e) {
					if verr := visit(e); verr != nil {
						return meta, verr
					}
				}
			}
			lineStart += int64(len(line))
		}
		if rerr != nil {
			if rerr == io.EOF {
				break // 末尾半行不消费
			}
			return meta, fmt.Errorf("读取日志 %s: %w", path, rerr)
		}
	}
	return meta, nil
}

// History 按 offset/limit 取一页历史。筛选、口径都在后端做，
// 界面底部那句"第 N~M 条 / 共 Total 条"才是对真的发出去的东西说的。
func (r *Reader) History(cluster, shard string, kind Kind, offset, limit int, filter HistoryFilter) (*HistoryPage, error) {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = DefaultLines
	}
	if limit > MaxLines {
		limit = MaxLines
	}
	page := &HistoryPage{
		Cluster: cluster, Shard: shard,
		Kind: string(kind), KindLabel: kind.Label(),
		Offset: offset, Limit: limit, Lines: []Entry{},
	}
	matched := 0
	meta, err := r.walkHistory(cluster, shard, kind, filter, func(e Entry) error {
		if matched >= offset && len(page.Lines) < limit {
			page.Lines = append(page.Lines, e)
		}
		matched++
		return nil
	})
	if err != nil {
		return nil, err
	}
	page.Path = meta.Path
	page.SizeBytes = meta.SizeBytes
	page.Scanned = meta.Scanned
	page.Truncated = meta.Truncated
	page.Total = matched
	page.NextOffset = offset + len(page.Lines)
	page.HasMore = page.NextOffset < page.Total
	if !meta.Start.IsZero() {
		page.StartAt = meta.Start.UTC().Format(time.RFC3339)
	}
	if page.Truncated {
		page.Note = fmt.Sprintf("文件有 %d 字节，超过单次历史查询 %d 字节的上限，只查了末尾一段；total 是这一段的口径",
			meta.SizeBytes, maxHistoryBytes)
	}
	if page.Total == 0 {
		page.Note = moreNote(page.Note, "这些条件在查询范围里一行都没命中；放宽 level/event/q 再试")
	}
	return page, nil
}

// Export 把筛选后的日志流式写给 w。format 只认 "text"（每行原文）与
// "ndjson"（每行一个 JSON Entry），由接口层校验后传进来。
// 原文仍走 ParseLine，脱敏（Klei 令牌、token=...）在出进程之前就做完了。
func (r *Reader) Export(cluster, shard string, kind Kind, filter HistoryFilter,
	format string, w io.Writer) (int64, error) {

	bw := bufio.NewWriter(w)
	var count int64
	_, err := r.walkHistory(cluster, shard, kind, filter, func(e Entry) error {
		var line []byte
		if format == "ndjson" {
			raw, merr := json.Marshal(e)
			if merr != nil {
				return merr
			}
			line = append(raw, '\n')
		} else {
			line = []byte(e.Raw + "\n")
		}
		if _, werr := bw.Write(line); werr != nil {
			return werr
		}
		count++
		return nil
	})
	if err != nil {
		return count, err
	}
	return count, bw.Flush()
}
