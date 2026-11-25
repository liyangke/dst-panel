package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dstpanel/internal/modbridge"
)

// putModDir 直接在 mods/ 造一个模组目录，比走上传接口快，也更接近
// "工坊下载完落地"之后的状态。
func putModDir(t *testing.T, f *apiFixture, dir, modinfo string) {
	t.Helper()
	full := filepath.Join(f.paths.ModsRoot(), dir)
	if err := os.MkdirAll(full, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(full, "modinfo.lua"), []byte(modinfo), 0o644); err != nil {
		t.Fatal(err)
	}
}

func enableMod(t *testing.T, h http.Handler, cluster, shard, dir string, opts map[string]any) {
	t.Helper()
	on := true
	rec := doJSON(t, h, http.MethodPut,
		"/api/v1/clusters/"+cluster+"/shards/"+shard+"/mods/"+dir,
		overridePatchRequest{Enabled: &on, Options: opts})
	if rec.Code != http.StatusOK {
		t.Fatalf("启用 %s/%s 失败: %d %s", shard, dir, rec.Code, rec.Body.String())
	}
}

func getConsistency(t *testing.T, h http.Handler, cluster string) modbridge.ConsistencyReport {
	t.Helper()
	rec := do(t, h, http.MethodGet, "/api/v1/clusters/"+cluster+"/mods/consistency", "admin", "secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d: %s", rec.Code, rec.Body.String())
	}
	var rpt modbridge.ConsistencyReport
	decodeData(t, rec, &rpt)
	return rpt
}

func TestConsistencyEndpoint(t *testing.T) {
	f := newFixture(t, "")
	ensureModsDir(t, f)
	h := f.srv.Handler()
	putModDir(t, f, "ModA", `return { name = "A 模组" }`)
	putModDir(t, f, "ModB", `return { name = "B 模组" }`)

	// 只开地表：引擎不会拒绝这种配置，两个世界就这么开出不同的规则。
	enableMod(t, h, "Cluster_1", "Master", "ModA", map[string]any{"radius": 3})
	rpt := getConsistency(t, h, "Cluster_1")
	if len(rpt.Shards) != 2 {
		t.Fatalf("两个分片都要出现在报告里: %+v", rpt.Shards)
	}
	if len(rpt.Issues) != 1 || rpt.Issues[0].Kind != "启用状态不一致" || rpt.Issues[0].Dir != "ModA" {
		t.Fatalf("该报地表开了洞穴没开: %+v", rpt.Issues)
	}
	if rpt.Issues[0].Fix == "" {
		t.Error("这种问题面板给得出动作")
	}
	// ModB 谁都没开，不该出现在问题里。
	if len(rpt.Issues) == 2 && rpt.Issues[1].Dir == "ModB" {
		t.Error("都没开的模组不算不一致")
	}

	// 两边都开但配置值不同：换成配置项不一致。
	enableMod(t, h, "Cluster_1", "Caves", "ModA", map[string]any{"radius": 9})
	rpt = getConsistency(t, h, "Cluster_1")
	if len(rpt.Issues) != 1 || rpt.Issues[0].Kind != "配置项不一致" {
		t.Fatalf("该报配置项不同: %+v", rpt.Issues)
	}
	if !strings.Contains(rpt.Issues[0].Detail, "Caves=9") ||
		!strings.Contains(rpt.Issues[0].Detail, "Master=3") {
		t.Errorf("要把两边取值摊开: %q", rpt.Issues[0].Detail)
	}

	// 完全一致时不是返回空对象，要给用户一句"查过了，没问题"。
	enableMod(t, h, "Cluster_1", "Caves", "ModA", map[string]any{"radius": 3})
	rpt = getConsistency(t, h, "Cluster_1")
	if len(rpt.Issues) != 0 {
		t.Fatalf("一致了不该报问题: %+v", rpt.Issues)
	}
	if !strings.Contains(rpt.Note, "一致") {
		t.Errorf("没问题也要说明白: %q", rpt.Note)
	}

	if rec := do(t, h, http.MethodGet, "/api/v1/clusters/Nope/mods/consistency", "admin", "secret"); rec.Code != http.StatusNotFound {
		t.Errorf("不存在的集群该 404，实际 %d", rec.Code)
	}
}

func TestShardModsViewReportsUnmetDependency(t *testing.T) {
	f := newFixture(t, "")
	ensureModsDir(t, f)
	h := f.srv.Handler()
	putModDir(t, f, "ModNeed", `return {
		name = "要库的模组",
		all_clients_require_mod = true,
		mod_dependencies = { { workshop = "workshop-999" } },
	}`)

	var mv modsView
	decodeData(t, do(t, h, http.MethodGet, "/api/v1/mods", "admin", "secret"), &mv)
	if len(mv.Mods) != 1 {
		t.Fatalf("列表应该只有这一个模组: %+v", mv.Mods)
	}
	// 这个标记决定玩家没订阅就会被断线（networking.lua:351），界面必须能提前说。
	if !mv.Mods[0].AllClientsRequire {
		t.Errorf("all_clients_require_mod 没读出来: %+v", mv.Mods[0])
	}
	// 磁盘级：依赖还没在这台机器上。
	if len(mv.UnmetDependencies) != 1 || mv.UnmetDependencies[0].Dir != "ModNeed" {
		t.Fatalf("磁盘级依赖预警没报: %+v", mv.UnmetDependencies)
	}
	if !strings.Contains(mv.Hint, "静默禁用") {
		t.Errorf("总提示要说清后果: %q", mv.Hint)
	}

	enableMod(t, h, "Cluster_1", "Master", "ModNeed", nil)
	var sv shardModsView
	decodeData(t, do(t, h, http.MethodGet, "/api/v1/clusters/Cluster_1/shards/Master/mods", "admin", "secret"), &sv)
	if len(sv.DependencyProblems) != 1 {
		t.Fatalf("分片级依赖问题没报出来: %+v", sv.DependencyProblems)
	}
	p := sv.DependencyProblems[0]
	if p.Dir != "ModNeed" || strings.Join(p.Candidates, ",") != "workshop-999" {
		t.Errorf("要说清是谁缺哪一组: %+v", p)
	}
	if !strings.Contains(sv.Hint, "静默禁用") {
		t.Errorf("分片页的提示要说清引擎的做法: %q", sv.Hint)
	}
	// 没开的那个分片不该被捎带报问题。
	var caves shardModsView
	decodeData(t, do(t, h, http.MethodGet, "/api/v1/clusters/Cluster_1/shards/Caves/mods", "admin", "secret"), &caves)
	if len(caves.DependencyProblems) != 0 {
		t.Errorf("Caves 没开这个模组，不该报它的依赖: %+v", caves.DependencyProblems)
	}
}
