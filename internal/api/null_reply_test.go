package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// 界面读列表字段普遍是直接 xxx.length（免构建 Vue 没有类型挡这一刀），
// 而 Go 的 nil 切片进 JSON 是 null。云机上玩家管理页整页崩在渲染里就是这么来的：
// 两个世界都暂停 → onlineByUserID 返回 nil → runningShards 是 null → 读 .length 抛。
// 所以"200 的 JSON 响应里不出现 null"当成一条接口级不变式来守，而不是逐个视图小心初始化。
//
// 夹具刻意用最空的那种状态：screen 一个会话都没有、助手快照一份都没写。
// 空状态才是 null 的温床，有数据的路径这些字段本来就是 []。
func TestNoNullInAnyGetReply(t *testing.T) {
	f := newFixture(t, "")
	h := f.srv.Handler()

	paths := []string{
		"/api/v1/session",
		"/api/v1/accounts",
		"/api/v1/clusters",
		"/api/v1/rooms",
		"/api/v1/world-schema",
		"/api/v1/ports/plan",
		"/api/v1/admin-commands",
		"/api/v1/commands",
		"/api/v1/tasks",
		"/api/v1/task-kinds",
		"/api/v1/mods",
		"/api/v1/stats",
		// 玩家在线时间线：people/spans/marks 三层数组，空房间最容易把某一层漏成 null。
		"/api/v1/player-timeline?cluster=Cluster_1",
		"/api/v1/host",
		"/api/v1/audit",
		"/api/v1/audit-actions",
		"/api/v1/backups",
		"/api/v1/settings",
		// 公网房间列表：列表字段最多、又是唯一一个会外部的，null 风险最高。
		"/api/v1/lobby/regions",
		"/api/v1/lobby/rooms",
		"/api/v1/clusters/Cluster_1/connect",
		"/api/v1/clusters/Cluster_1/settings",
		"/api/v1/clusters/Cluster_1/ports",
		"/api/v1/clusters/Cluster_1/players",
		"/api/v1/clusters/Cluster_1/mods/consistency",
		"/api/v1/clusters/Cluster_1/shards/Master/helper",
		"/api/v1/clusters/Cluster_1/shards/Master/status",
		"/api/v1/clusters/Cluster_1/shards/Master/events",
		"/api/v1/clusters/Cluster_1/shards/Master/mods",
		"/api/v1/clusters/Cluster_1/shards/Master/world",
		"/api/v1/clusters/Cluster_1/shards/Master/map",
		"/api/v1/clusters/Cluster_1/shards/Master/log",
	}

	for _, p := range paths {
		rec := do(t, h, http.MethodGet, p, "admin", "secret")
		if rec.Code != http.StatusOK {
			// 这一条只守"答得出来的响应不许带 null"。
			// 4xx/5xx 走的是另一套约定（中文说明 + publicError），有自己的测试。
			continue
		}
		body := rec.Body.String()
		var generic any
		if err := json.Unmarshal([]byte(body), &generic); err != nil {
			t.Errorf("%s 的响应不是合法 JSON: %v", p, err)
			continue
		}
		m, ok := generic.(map[string]any)
		if !ok {
			t.Errorf("%s 的响应顶层不是对象：%s", p, body)
			continue
		}
		if answered, _ := m["ok"].(bool); !answered {
			t.Errorf("%s 返回 200 但 ok 不是 true：%s", p, body)
			continue
		}
		var nulls []string
		walkNull(generic, "$", &nulls)
		if len(nulls) > 0 {
			t.Errorf("%s 的响应里有 %d 处 null，前端读 .length 就会崩：%s\n原文：%s",
				p, len(nulls), strings.Join(nulls, "、"), body)
		}
	}
}

// walkNull 递归找出值为 null 的字段路径。
func walkNull(v any, path string, out *[]string) {
	switch node := v.(type) {
	case map[string]any:
		for k, child := range node {
			p := path + "." + k
			if child == nil {
				*out = append(*out, p)
				continue
			}
			walkNull(child, p, out)
		}
	case []any:
		for i, child := range node {
			walkNull(child, path+"["+strconv.Itoa(i)+"]", out)
		}
	}
}

// 崩溃的那一条要单独钉住：即使上面的全量扫描将来被放宽，这里也还留着现场。
func TestPlayersReplyHasNoNullWhenNoShardAnswers(t *testing.T) {
	f := newFixture(t, "")
	rec := do(t, f.srv.Handler(), http.MethodGet, "/api/v1/clusters/Cluster_1/players", "admin", "secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，body %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`"runningShards":[]`, `"online":[]`, `"roster":[]`, `"entries":[]`} {
		if !strings.Contains(body, want) {
			t.Errorf("没有世界回话时玩家管理页要能直接渲染，响应里该有 %s，实际 %s", want, body)
		}
	}
	if strings.Contains(body, "null") {
		t.Errorf("响应里出现了 null，前端读 .length 会整页崩：%s", body)
	}
}

// 世界规则页在问不到 screen 时也不能给出 null。
func TestClusterSettingsReplyHasNoNullWhenScreenUnreachable(t *testing.T) {
	f := newFixture(t, "")
	f.srv.Procs.Runner = screenRunner{err: errors.New("这台机器上没有 screen")}
	rec := do(t, f.srv.Handler(), http.MethodGet, "/api/v1/clusters/Cluster_1/settings", "admin", "secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，body %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"runningShards":[]`) {
		t.Errorf("问不到进程状态时该给空数组而不是 null，实际 %s", body)
	}
	if !strings.Contains(body, "问不到世界进程状态") {
		t.Errorf("同时要说明为什么读不到，实际 %s", body)
	}
}
