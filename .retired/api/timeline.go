// 玩家行为时间线接口：一个世界这一轮里，谁进过、谁死过、谁说过话。
//
// 它和 /log 的分工写在这里，因为两者看着像同一件事：
//   - /log 是"这一份文件最近的行"，一次一个世界、一份文件，给人逐行看；
//   - /timeline 是"这两个文件合起来的行为汇总"，一次一个世界、两份文件，给人数次数。
//
// 合成一个接口的话，前端要先各读一份再自己对齐时钟和公告抄本——
// 而"同一条公告在两份文件里各有一份"这件事只有写标签的那层说得准（见 gamelog/timeline.go）。
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

func (s *Server) handleTimeline(w http.ResponseWriter, r *http.Request) {
	cluster, shard, ok := s.shardScope(w, r)
	if !ok {
		return
	}
	lines := gamelog.TimelineDefaultLines
	if v := strings.TrimSpace(r.URL.Query().Get("lines")); v != "" {
		n, err := strconv.Atoi(v)
		// 越界照样 400：时间线的行数是"我数的是哪一段"的唯一依据，
		// 悄悄裁到上限会让界面写着 5000 行、实际数了 2000 行。
		if err != nil || n < 1 || n > gamelog.MaxLines {
			writeErr(w, http.StatusBadRequest,
				fmt.Sprintf("lines 得是 1~%d 之间的整数", gamelog.MaxLines))
			return
		}
		lines = n
	}
	tl, err := gamelog.NewReader(s.Paths).Timeline(cluster, shard, lines)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// 503 而不是 404（同 /log）：文件还没写出来是分片没启动过的常态。
			writeErr(w, http.StatusServiceUnavailable,
				"这个世界还没有日志文件：世界启动并写出第一行之后，这里才有行为可统计")
			return
		}
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	writeOK(w, tl)
}
