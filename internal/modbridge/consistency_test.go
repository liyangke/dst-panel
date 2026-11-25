package modbridge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dstpanel/internal/dst"
)

// writeOverrides 把一份 modoverrides.lua 落到指定分片，内容原样写不做加工——
// 一致性校验的输入必须是"用户手里那份文件"，包括他手写的畸形写法。
func writeOverrides(t *testing.T, b *Bridge, shard, body string) {
	t.Helper()
	path := b.Paths.ModOverrides("Cluster_1", shard)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func bothShards() []string { return []string{dst.ShardMaster, dst.ShardCaves} }

func findIssue(rpt *ConsistencyReport, dir string) *ModIssue {
	for i := range rpt.Issues {
		if rpt.Issues[i].Dir == dir {
			return &rpt.Issues[i]
		}
	}
	return nil
}

func TestCompareShardOverridesIdenticalIsClean(t *testing.T) {
	b, _ := newFixture(t)
	seed := `return {
	["workshop-111"] = {
		enabled = true,
		priority = 0,
		configuration_options = { radius = 3, name = "同样" },
	},
	["workshop-222"] = { enabled = false },
}
`
	for _, sh := range bothShards() {
		writeOverrides(t, b, sh, seed)
	}
	rpt, err := b.CompareShardOverrides("Cluster_1", bothShards())
	if err != nil {
		t.Fatal(err)
	}
	if len(rpt.Issues) != 0 {
		t.Fatalf("一模一样的两边不该报问题: %+v", rpt.Issues)
	}
	if !strings.Contains(rpt.Note, "一致") {
		t.Errorf("没问题时也要给一句人话，界面不能空着: %q", rpt.Note)
	}
	for _, v := range rpt.Shards {
		if strings.Join(v.Enabled, ",") != "workshop-111" {
			t.Errorf("%s 的启用清单不对: %+v", v.Shard, v)
		}
		if !v.Present {
			t.Errorf("文件明明在: %+v", v)
		}
	}
}

// 引擎先 DisableAllMods 再按 enabled 开，所以"没条目"和"enabled = false"结果相同，
// 把它当不一致会让用户去追一个不存在的问题。
func TestCompareShardOverridesTreatsMissingEntryAsDisabled(t *testing.T) {
	b, _ := newFixture(t)
	writeOverrides(t, b, dst.ShardMaster, `return {
	["workshop-111"] = { enabled = false },
}
`)
	writeOverrides(t, b, dst.ShardCaves, `return {
}
`)
	rpt, err := b.CompareShardOverrides("Cluster_1", bothShards())
	if err != nil {
		t.Fatal(err)
	}
	if len(rpt.Issues) != 0 {
		t.Fatalf("两边都不开不该算不一致: %+v", rpt.Issues)
	}
}

func TestCompareShardOverridesReportsEnabledMismatch(t *testing.T) {
	b, _ := newFixture(t)
	writeOverrides(t, b, dst.ShardMaster, `return {
	["workshop-111"] = { enabled = true, configuration_options = { a = 1 } },
}
`)
	writeOverrides(t, b, dst.ShardCaves, `return {
	["workshop-111"] = { enabled = false, configuration_options = { a = 2 } },
}
`)
	rpt, err := b.CompareShardOverrides("Cluster_1", bothShards())
	if err != nil {
		t.Fatal(err)
	}
	issue := findIssue(rpt, "workshop-111")
	if issue == nil {
		t.Fatalf("没报启用状态不一致: %+v", rpt.Issues)
	}
	if issue.Kind != "启用状态不一致" {
		t.Errorf("Kind 要能直接显示，实际 %q", issue.Kind)
	}
	if !strings.Contains(issue.Detail, "Master") || !strings.Contains(issue.Detail, "Caves") {
		t.Errorf("要说清哪边开了哪边没开: %q", issue.Detail)
	}
	if issue.Fix == "" {
		t.Error("这种问题面板能给出动作，不能只报错")
	}
	// 启用状态不一致时不该再报配置项差异——修好开没开之后配置自然要重看，
	// 同时抛两条会让用户以为是两个独立故障。
	if len(rpt.Issues) != 1 {
		t.Errorf("一个模组只该出一条主问题: %+v", rpt.Issues)
	}
}

func TestCompareShardOverridesReportsOptionMismatch(t *testing.T) {
	b, _ := newFixture(t)
	writeOverrides(t, b, dst.ShardMaster, `return {
	["workshop-111"] = { enabled = true, configuration_options = { radius = 3 } },
}
`)
	writeOverrides(t, b, dst.ShardCaves, `return {
	["workshop-111"] = { enabled = true, configuration_options = { radius = 9 } },
}
`)
	rpt, err := b.CompareShardOverrides("Cluster_1", bothShards())
	if err != nil {
		t.Fatal(err)
	}
	issue := findIssue(rpt, "workshop-111")
	if issue == nil || issue.Kind != "配置项不一致" {
		t.Fatalf("配置项不同必须报: %+v", rpt.Issues)
	}
	if !strings.Contains(issue.Detail, "radius") ||
		!strings.Contains(issue.Detail, "Master=3") || !strings.Contains(issue.Detail, "Caves=9") {
		t.Errorf("要把每个分片的取值摊开: %q", issue.Detail)
	}
}

// 一边写了某项、另一边没写：没写的那边用 modinfo 默认值，同样是"两个世界规则不同"。
func TestCompareShardOverridesReportsOptionMissingOnOneSide(t *testing.T) {
	b, _ := newFixture(t)
	writeOverrides(t, b, dst.ShardMaster, `return {
	["workshop-111"] = { enabled = true, configuration_options = { radius = 3 } },
}
`)
	writeOverrides(t, b, dst.ShardCaves, `return {
	["workshop-111"] = { enabled = true },
}
`)
	rpt, err := b.CompareShardOverrides("Cluster_1", bothShards())
	if err != nil {
		t.Fatal(err)
	}
	issue := findIssue(rpt, "workshop-111")
	if issue == nil || issue.Kind != "配置项不一致" {
		t.Fatalf("一边没写配置项也要报: %+v", rpt.Issues)
	}
	if !strings.Contains(issue.Detail, "没写") {
		t.Errorf("缺的那边要说清是没写: %q", issue.Detail)
	}
}

func TestCompareShardOverridesReportsNestedOnlyOnOneSide(t *testing.T) {
	b, _ := newFixture(t)
	writeOverrides(t, b, dst.ShardMaster, `return {
	["workshop-111"] = { enabled = true, configuration_options = { scalar = "ok", groups = { 1, 2 } } },
}
`)
	writeOverrides(t, b, dst.ShardCaves, `return {
	["workshop-111"] = { enabled = true, configuration_options = { scalar = "ok" } },
}
`)
	rpt, err := b.CompareShardOverrides("Cluster_1", bothShards())
	if err != nil {
		t.Fatal(err)
	}
	issue := findIssue(rpt, "workshop-111")
	if issue == nil || issue.Kind != "有面板读不懂的配置项" {
		t.Fatalf("嵌套表要单独说明: %+v", rpt.Issues)
	}
	if issue.Fix != "" {
		t.Errorf("面板比不了嵌套值，不该假装能给修复动作: %q", issue.Fix)
	}
	if !strings.Contains(issue.Detail, "比不了") {
		t.Errorf("要坦白边界: %q", issue.Detail)
	}
}

func TestCompareShardOverridesReportsPriorityWrittenDifferently(t *testing.T) {
	b, _ := newFixture(t)
	writeOverrides(t, b, dst.ShardMaster, `return { ["workshop-111"] = { enabled = true, priority = 0 } }`)
	writeOverrides(t, b, dst.ShardCaves, `return { ["workshop-111"] = { enabled = true, priority = -100 } }`)
	rpt, err := b.CompareShardOverrides("Cluster_1", bothShards())
	if err != nil {
		t.Fatal(err)
	}
	issue := findIssue(rpt, "workshop-111")
	if issue == nil || issue.Kind != "priority 写法不同" {
		t.Fatalf("写法不同要显示出来: %+v", rpt.Issues)
	}
	// 这是实测出来的引擎行为：modoverrides 里的 priority 没有任何代码读它。
	if !strings.Contains(issue.Detail, "modinfo.lua") {
		t.Errorf("必须说明这个键不影响加载顺序，别让用户白折腾: %q", issue.Detail)
	}
}

func TestCompareShardOverridesReportsClientModsDisabledMismatch(t *testing.T) {
	b, _ := newFixture(t)
	writeOverrides(t, b, dst.ShardMaster, `return {
	client_mods_disabled = true,
	["workshop-111"] = { enabled = true },
}
`)
	writeOverrides(t, b, dst.ShardCaves, `return {
	["workshop-111"] = { enabled = true },
}
`)
	rpt, err := b.CompareShardOverrides("Cluster_1", bothShards())
	if err != nil {
		t.Fatal(err)
	}
	if len(rpt.Shards[0].Enabled) != 1 || len(rpt.Shards[1].Enabled) != 1 {
		t.Errorf("顶层键不能被当成模组条目: %+v", rpt.Shards)
	}
	issue := findIssue(rpt, "client_mods_disabled")
	if issue == nil || issue.Kind != "客户端模组总开关不一致" {
		t.Fatalf("总开关不一致必须报: %+v", rpt.Issues)
	}
	if strings.Contains(issue.Detail, "true") || strings.Contains(issue.Detail, "false") {
		t.Errorf("界面文案里不该出现码值: %q", issue.Detail)
	}
}

func TestCompareShardOverridesSingleShardIsNotAnError(t *testing.T) {
	b, _ := newFixture(t)
	writeOverrides(t, b, dst.ShardMaster, `return { ["workshop-111"] = { enabled = true } }`)
	rpt, err := b.CompareShardOverrides("Cluster_1", []string{dst.ShardMaster})
	if err != nil {
		t.Fatal(err)
	}
	if len(rpt.Issues) != 0 {
		t.Errorf("单分片没有一致性可校验: %+v", rpt.Issues)
	}
	if !strings.Contains(rpt.Note, "只有一个世界") {
		t.Errorf("要说清为什么没问题: %q", rpt.Note)
	}
}

// 文件不存在是合法状态（一个模组都没开过），不能报成错误。
func TestCompareShardOverridesMissingFilesAreClean(t *testing.T) {
	b, _ := newFixture(t)
	rpt, err := b.CompareShardOverrides("Cluster_1", bothShards())
	if err != nil {
		t.Fatal(err)
	}
	if len(rpt.Issues) != 0 {
		t.Fatalf("两边都没有文件不该报问题: %+v", rpt.Issues)
	}
	for _, v := range rpt.Shards {
		if v.Present {
			t.Errorf("文件不存在却报存在: %+v", v)
		}
	}
}

func TestCompareShardOverridesUnreadableFileDoesNotPanic(t *testing.T) {
	b, _ := newFixture(t)
	writeOverrides(t, b, dst.ShardMaster, `return { ["workshop-111"] = { enabled = true } }`)
	writeOverrides(t, b, dst.ShardCaves, `this is not lua at all (((`)
	_, err := b.CompareShardOverrides("Cluster_1", bothShards())
	if err == nil {
		t.Fatal("读不进来的文件必须报错，否则用户以为两边一致")
	}
	if !strings.Contains(err.Error(), "modoverrides.lua") {
		t.Errorf("错误要说清是哪个文件: %q", err)
	}
}
