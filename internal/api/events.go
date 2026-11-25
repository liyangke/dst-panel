// 玩家事件表接口：一个世界这一轮里，谁在什么时候做了什么，一页一页给。
//
// 它和 /log 的分工写在这里，因为两者看着像同一件事：
//   - /log 是"这一份文件最近的行"，一次一个世界、一份文件，给人逐行看、跟着流走；
//   - /events 是"两份文件合起来的玩家行为"，一次一个世界、两份文件，筛选和分页都在后端做。
//
// 为什么筛选分页不在前端做：界面底部那句"第 1~20 条 / 共 312 条"必须是对
// 真的发出去的东西说的；前端自己切页就只能报"这一页有 20 条"，总数得另编。
// 而"同一条公告在两份文件里各有一份"这件事只有写标签的那层说得准（见 gamelog/events.go）。
package api

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"dstpanel/internal/gamelog"
)

// parseEventQuery 解析事件表的参数。任何一项写错都直接 400 并返回 false：
// 参数打错了但页面看起来正常，用户会以为筛选生效了。
func (s *Server) parseEventQuery(w http.ResponseWriter, r *http.Request) (gamelog.EventQuery, bool) {
	raw := r.URL.Query()
	q := gamelog.EventQuery{Lines: gamelog.EventDefaultLines, Page: 1, Size: gamelog.EventDefaultSize}
	if v := strings.TrimSpace(raw.Get("lines")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > gamelog.MaxLines {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("lines 得是 1~%d 之间的整数", gamelog.MaxLines))
			return q, false
		}
		q.Lines = n
	}
	if v := strings.TrimSpace(raw.Get("size")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > gamelog.EventMaxSize {
			writeErr(w, http.StatusBadRequest,
				fmt.Sprintf("size 得是 1~%d 之间的整数（每页行数）", gamelog.EventMaxSize))
			return q, false
		}
		q.Size = n
	}
	if v := strings.TrimSpace(raw.Get("page")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeErr(w, http.StatusBadRequest, "page 得是从 1 开始的整数")
			return q, false
		}
		q.Page = n
	}
	q.Event = strings.TrimSpace(raw.Get("event"))
	// 事件码要校验：认不得的码筛出来是空表，而空表在界面上和
	// "这一轮确实没发生过这件事"长得一模一样，必须当场拒绝。
	if q.Event != "" {
		ok := false
		for _, ev := range gamelog.BehaviourCatalog() {
			if ev.Code == q.Event {
				ok = true
				break
			}
		}
		if !ok {
			writeErr(w, http.StatusBadRequest, "没有这个行为码，可调 /api/v1/log-events 看可选值")
			return q, false
		}
	}
	q.Player = strings.ToLower(strings.TrimSpace(raw.Get("player")))
	q.Text = strings.ToLower(strings.TrimSpace(raw.Get("q")))
	q.Chat = truthy(raw.Get("chat"))
	return q, true
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	cluster, shard, ok := s.shardScope(w, r)
	if !ok {
		return
	}
	q, ok := s.parseEventQuery(w, r)
	if !ok {
		return
	}
	page, err := gamelog.NewReader(s.Paths).Events(cluster, shard, q)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// 503 而不是 404（同 /log）：两份文件都没有是分片没启动过的常态，
			// 报 404 会让前端把它当"地址写错了"，也不去重试。
			writeErr(w, http.StatusServiceUnavailable,
				"这个世界还没有日志文件：世界启动并写出第一行之后，这里才有行为可列")
			return
		}
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	writeOK(w, page)
}
