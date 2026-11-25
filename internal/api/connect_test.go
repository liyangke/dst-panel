package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dstpanel/internal/dst"
	"dstpanel/internal/procrun"
)

// 加入命令这一组的契约就一条：界面上按下去必须拿到一句真能连上的东西，
// 拿不到时得有一句话说明为什么拿不到。所以下面每条断言都同时看 command 和 note：
// 空 command + 空 note 是最糟的组合（按钮按着没反应，用户只会反复点）。

type connectReply struct {
	OK   bool        `json:"ok"`
	Data connectView `json:"data"`
	Err  string      `json:"error"`
}

// getConnect 自己发请求而不是复用 do()：这一组的地址是从请求 Host 来的，
// Host 就是被测变量。
func getConnect(t *testing.T, f *apiFixture, host, cluster string) (connectView, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/clusters/"+cluster+"/connect", nil)
	req.Host = host
	req.SetBasicAuth("admin", "secret")
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	text := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("connect(%s, Host=%s) = %d，body %s", cluster, host, rec.Code, text)
	}
	var out connectReply
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v，原文 %q", err, text)
	}
	// 走 {ok,data} 壳子这件事必须钉住：发裸对象的话界面拿到 null，
	// 复制按钮上永远写着"还没拿到"（/api/v1/host 就踩过）。
	if !out.OK {
		t.Fatalf("响应没带 ok:true：%s", text)
	}
	return out.Data, text
}

func (f *apiFixture) setRoom(t *testing.T, name string) {
	t.Helper()
	f.setSettings(t, dst.GameSettings{ClusterName: name, MaxPlayers: 6})
	f.setShard(t, "Master", dst.ShardSettings{ServerPort: 10999, IsMaster: true, Name: "地表", ID: 1})
	f.setShard(t, "Caves", dst.ShardSettings{ServerPort: 10998, IsMaster: false, Name: "洞穴", ID: 2,
		MasterIP: "127.0.0.1", MasterPort: 10999})
}

func TestConnectUsesMasterShardPortNotCaves(t *testing.T) {
	f := newFixture(t, "  20314.dst-Cluster_1-Master\t(Detached)\n")
	f.setRoom(t, "本地实测房")

	v, text := getConnect(t, f, "119.29.239.62", f.cluster)
	if v.Cluster != f.cluster || v.RoomName != "本地实测房" {
		t.Errorf("房间名/目录名 = %q/%q", v.Cluster, v.RoomName)
	}
	// 洞穴那个端口只认地表进程，从外面连它连不上人：
	// 这一条钉的是"取 is_master 那一个"，不是"取目录顺序第一个"。
	if v.Shard != "Master" || v.Port != 10999 {
		t.Errorf("指向的世界/端口 = %q/%d，期望 Master/10999（实际 %s）", v.Shard, v.Port, text)
	}
	if v.Address != "119.29.239.62" {
		t.Errorf("地址 = %q，期望取自请求 Host", v.Address)
	}
	want := `c_connect("119.29.239.62", 10999)`
	if v.Command != want {
		t.Errorf("命令 = %q，期望 %q", v.Command, want)
	}
	if v.Source == "" {
		t.Error("没交代地址是哪来的，反代场景下用户无从核对")
	}
	if v.Note != "" {
		t.Errorf("公网地址 + 进程在跑时不该带任何说明：%q", v.Note)
	}
}

func TestConnectSingleWorldRoomFallsBackToTheOnlyShard(t *testing.T) {
	f := newFixture(t, "")
	f.setSettings(t, dst.GameSettings{ClusterName: "只有一个世界"})
	f.setShard(t, "Caves", dst.ShardSettings{ServerPort: 11001, IsMaster: false, Name: "洞穴", ID: 2})
	if err := os.RemoveAll(filepath.Join(f.paths.KleiRoot, f.cluster, "Master")); err != nil {
		t.Fatalf("删地表目录: %v", err)
	}

	v, _ := getConnect(t, f, "10.0.0.5", f.cluster)
	if v.Shard != "Caves" || v.Port != 11001 {
		t.Errorf("单世界房间 = %q/%d，期望退回唯一存在的那个世界", v.Shard, v.Port)
	}
	if v.Command != `c_connect("10.0.0.5", 11001)` {
		t.Errorf("命令 = %q", v.Command)
	}
}

func TestConnectExplainsEveryWayTheCommandCanFail(t *testing.T) {
	cases := []struct {
		name       string
		host       string
		port       int
		screenOut  string
		wantCmd    string
		wantNoteIn string
	}{
		{
			name: "回环地址", host: "127.0.0.1:8963", port: 10999,
			screenOut: "  9.dst-Cluster_1-Master\t(Detached)\n",
			wantCmd:   `c_connect("127.0.0.1", 10999)`, wantNoteIn: "只有这台机器上的游戏客户端",
		},
		{
			name: "localhost", host: "localhost:8963", port: 10999,
			screenOut: "  9.dst-Cluster_1-Master\t(Detached)\n",
			wantCmd:   `c_connect("localhost", 10999)`, wantNoteIn: "局域网或公网地址",
		},
		{
			name: "没写端口", host: "10.0.0.5", port: 0,
			wantNoteIn: "没写游戏端口",
		},
		{
			name: "进程没在跑", host: "10.0.0.5", port: 10999,
			wantCmd: `c_connect("10.0.0.5", 10999)`, wantNoteIn: "得先开服",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, c.screenOut)
			f.setSettings(t, dst.GameSettings{ClusterName: "本地实测房"})
			if c.port > 0 {
				f.setShard(t, "Master", dst.ShardSettings{ServerPort: c.port, IsMaster: true, Name: "地表", ID: 1})
			}
			v, _ := getConnect(t, f, c.host, f.cluster)
			if v.Command != c.wantCmd {
				t.Errorf("命令 = %q，期望 %q", v.Command, c.wantCmd)
			}
			if c.wantNoteIn == "" {
				if v.Note != "" {
					t.Errorf("不该有说明：%q", v.Note)
				}
				return
			}
			// 每条失败路径都要有话说，而且说的话得能指向下一步动作。
			if !strings.Contains(v.Note, c.wantNoteIn) {
				t.Errorf("说明 = %q，期望含 %q", v.Note, c.wantNoteIn)
			}
		})
	}
}

// TestConnectKeepsWorkingWhenScreenIsUnreachable 钉的是"别把未知说成没开"：
// screen 问不到时命令照拼，只是不硬下"进程没在跑"那句结论。
func TestConnectKeepsWorkingWhenScreenIsUnreachable(t *testing.T) {
	f := newFixture(t, "")
	f.setRoom(t, "本地实测房")
	f.srv.Procs = procrun.New(f.paths, screenRunner{
		err: errors.New(`找不到命令 screen: exec: "screen": executable file not found in %PATH%`),
	})

	v, _ := getConnect(t, f, "10.0.0.5", f.cluster)
	if v.Command != `c_connect("10.0.0.5", 10999)` {
		t.Errorf("命令 = %q，进程状态问不到也该拼出来", v.Command)
	}
	if strings.Contains(v.Note, "没在跑") {
		t.Errorf("把\"问不到\"说成了\"没在跑\"：%q", v.Note)
	}
}

func TestConnectRejectsUnknownClusterAndNoAuth(t *testing.T) {
	f := newFixture(t, "")

	rec := do(t, f.srv.Handler(), http.MethodGet, "/api/v1/clusters/Cluster_9/connect", "admin", "secret")
	if rec.Code != http.StatusNotFound {
		t.Errorf("不存在的集群 = %d，期望 404，body %s", rec.Code, rec.Body.String())
	}
	if r := decodeReply(t, rec); !strings.Contains(r.Err, "Cluster_1") {
		t.Errorf("404 没列出现有集群：%q", r.Err)
	}

	if rec := do(t, f.srv.Handler(), http.MethodGet, "/api/v1/clusters/Cluster_1/connect", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("无凭据 = %d，期望 401", rec.Code)
	}
}
