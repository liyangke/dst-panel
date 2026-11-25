package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"dstpanel/internal/store"
)

// steamCmdFixture 造一个"steamcmd 真的在磁盘上"的前置条件，
// 免得所有正向用例都只能验证失败分支。
func (f *apiFixture) steamCmdFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "steamcmd.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("写 steamcmd: %v", err)
	}
	f.srv.SteamCmd = path
	return path
}

func TestTaskKindsTellWhetherTheMachineCanDoIt(t *testing.T) {
	f := newFixture(t, "")
	rec := doJSON(t, f.srv.Handler(), http.MethodGet, "/api/v1/task-kinds", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("task-kinds = %d，body %s", rec.Code, rec.Body.String())
	}
	var out []taskKindView
	decodeData(t, rec, &out)
	if len(out) == 0 {
		t.Fatal("一个可提交的任务类型都没有")
	}
	var upd *taskKindView
	for i := range out {
		k := out[i]
		if k.Label == "" || k.Label == k.Kind || k.Desc == "" {
			t.Errorf("任务类型 %q 没配上中文标签或说明：%+v", k.Kind, k)
		}
		if k.Kind == "server.update" {
			upd = &out[i]
		}
	}
	if upd == nil {
		t.Fatal("没有 server.update 这一条")
	}
	if !upd.Danger {
		t.Error("更新服务端本体没标危险，前端就不会二次确认")
	}
	// steamcmd 还没配：界面要提前看到"做不了"和为什么做不了，而不是提交以后才知道。
	if upd.Runnable || !strings.Contains(upd.Note, "steamCmd") {
		t.Errorf("未配置时的可跑性 = %+v", *upd)
	}

	f.steamCmdFixture(t)
	out = nil
	rec = doJSON(t, f.srv.Handler(), http.MethodGet, "/api/v1/task-kinds", nil)
	decodeData(t, rec, &out)
	for _, k := range out {
		switch {
		case k.Kind == "server.update":
			// steamcmd 配好了就能跑。这个 fixture 里没有服务端本体，所以那句 Note
			// 说的应该是"这一次是全新下载"，而不是"做不了"——界面靠这句话决定
			// 给用户摆"下载"还是"更新"。
			if !k.Runnable {
				t.Errorf("steamcmd 配好了却报不可跑：%+v", k)
			}
			if !strings.Contains(k.Note, "还没有服务端本体") {
				t.Errorf("本体缺失时要把这一次说成全新下载：%+v", k)
			}
			// 整包大小随构建变，带宽每台机器不一样：写死一个"约 4.6G"就是在承诺一个
			// 面板不可能知道的事实，慢机器上还会显得"早该下完了"。
			if strings.Contains(k.Note, "4.6") || strings.Contains(k.Desc, "4.6") {
				t.Errorf("文案里写死了整包大小：%+v", k)
			}
		case k.SubmitVia != "":
			// 存档那两类的前置条件是快照模块，这个 fixture 里没接上：
			// 继续报"做不了"+接上的办法，正好是这条接口存在的意义。
			if k.Runnable || k.Note == "" {
				t.Errorf("没接上快照模块却报可跑：%+v", k)
			}
			if !k.NeedsPayload {
				t.Errorf("%s 的参数要面板自己拼，却没标 needsPayload：%+v", k.Kind, k)
			}
			if !strings.Contains(k.SubmitVia, "/api/v1/") {
				t.Errorf("%s 的 submitVia 没指到真实入口：%q", k.Kind, k.SubmitVia)
			}
		}
	}
}

func TestTaskSubmitNeedsConfirmAndPrerequisites(t *testing.T) {
	f := newFixture(t, "")
	h := f.srv.Handler()

	rec := doJSON(t, h, http.MethodPost, "/api/v1/tasks", map[string]any{"kind": "server.update"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("危险任务没确认 = %d，期望 400，body %s", rec.Code, rec.Body.String())
	}
	if msg := decodeReply(t, rec).Err; !strings.Contains(msg, "确认") || !strings.Contains(msg, "更新服务端本体") {
		t.Errorf("提示没说要确认什么：%q", msg)
	}
	tasks, err := f.srv.Store.ListTasks(10)
	if err != nil || len(tasks) != 0 {
		t.Errorf("被拒的提交还是进了队列：%+v %v", tasks, err)
	}

	// 表外的 kind 一律挡住，并把可选的列出来。
	rec = doJSON(t, h, http.MethodPost, "/api/v1/tasks", map[string]any{"kind": "cluster.delete", "confirm": true})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("表外 kind = %d，期望 400", rec.Code)
	}
	if msg := decodeReply(t, rec).Err; !strings.Contains(msg, "server.update") {
		t.Errorf("没列出可用的类型：%q", msg)
	}

	// 确认了但前置条件不满足：不能收一条注定失败的任务进队列。
	rec = doJSON(t, h, http.MethodPost, "/api/v1/tasks", map[string]any{"kind": "server.update", "confirm": true})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("steamcmd 没配 = %d，期望 400，body %s", rec.Code, rec.Body.String())
	}
	if msg := decodeReply(t, rec).Err; !strings.Contains(msg, "steamCmd") {
		t.Errorf("没说清缺哪个配置：%q", msg)
	}

	f.steamCmdFixture(t)
	rec = doJSON(t, h, http.MethodPost, "/api/v1/tasks", map[string]any{"kind": "server.update", "confirm": true})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("提交 = %d，期望 202，body %s", rec.Code, rec.Body.String())
	}
	var v taskView
	decodeData(t, rec, &v)
	if v.ID == 0 || v.Status != store.TaskQueued || v.StatusLab != "排队中" {
		t.Errorf("回的任务 = %+v，期望带 id 的排队中", v)
	}
	if v.Title != "下载/更新服务端本体" {
		t.Errorf("标题应该是中文标签，实际 %q", v.Title)
	}

	entries, _ := f.srv.Store.ListAudit(20, 0, "")
	var found bool
	for _, e := range entries {
		if e.Action == "task.submit" && e.OK && e.Actor == "admin" {
			found = true
		}
	}
	if !found {
		t.Errorf("提交任务没留审计：%+v", entries)
	}
}

func TestTaskCancelOnlyTouchesQueued(t *testing.T) {
	f := newFixture(t, "")
	f.steamCmdFixture(t)
	h := f.srv.Handler()

	rec := doJSON(t, h, http.MethodPost, "/api/v1/tasks", map[string]any{"kind": "server.update", "confirm": true})
	var v taskView
	decodeData(t, rec, &v)

	rec = doJSON(t, h, http.MethodDelete, "/api/v1/tasks/"+strconv.FormatInt(v.ID, 10), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("取消排队任务 = %d，body %s", rec.Code, rec.Body.String())
	}
	var canceled taskView
	decodeData(t, rec, &canceled)
	if canceled.Status != store.TaskCanceled || canceled.StatusLab != "已取消" {
		t.Errorf("取消后的状态 = %+v", canceled)
	}
	if !strings.Contains(canceled.Message, "面板里取消") {
		t.Errorf("没记下是谁取消的：%q", canceled.Message)
	}

	// 重复取消是无效动作，要把它现在的状态说回来。
	rec = doJSON(t, h, http.MethodDelete, "/api/v1/tasks/"+strconv.FormatInt(v.ID, 10), nil)
	if rec.Code != http.StatusConflict {
		t.Errorf("重复取消 = %d，期望 409，body %s", rec.Code, rec.Body.String())
	}
	if msg := decodeReply(t, rec).Err; !strings.Contains(msg, "已取消") {
		t.Errorf("409 的文案没说明当前状态：%q", msg)
	}

	// 正在执行的不支持半路掐：会留下没写完的文件，那种脏状态比多等几分钟贵。
	if _, err := f.srv.Store.CreateTask("server.update", "更新服务端本体", nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	running, err := f.srv.Store.ClaimTask()
	if err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	rec = doJSON(t, h, http.MethodDelete, "/api/v1/tasks/"+strconv.FormatInt(running.ID, 10), nil)
	if rec.Code != http.StatusConflict {
		t.Errorf("取消执行中的任务 = %d，期望 409，body %s", rec.Code, rec.Body.String())
	}
	if msg := decodeReply(t, rec).Err; !strings.Contains(msg, "正在执行") || !strings.Contains(msg, "半路") {
		t.Errorf("要解释为什么不掐：%q", msg)
	}
	if cur, _ := f.srv.Store.GetTask(running.ID); cur.Status != store.TaskRunning {
		t.Errorf("接口说不掐，却还是把任务改成了 %s", cur.Status)
	}

	rec = doJSON(t, h, http.MethodDelete, "/api/v1/tasks/999999", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("不存在的任务 = %d，期望 404", rec.Code)
	}
	rec = doJSON(t, h, http.MethodDelete, "/api/v1/tasks/abc", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("非数字 id = %d，期望 400", rec.Code)
	}
	if msg := decodeReply(t, rec).Err; !strings.Contains(msg, "abc") {
		t.Errorf("没带上原值：%q", msg)
	}

	entries, _ := f.srv.Store.ListAudit(30, 0, "")
	var found bool
	for _, e := range entries {
		if e.Action == "task.cancel" && e.OK {
			found = true
		}
	}
	if !found {
		t.Error("取消任务没留审计")
	}
}
