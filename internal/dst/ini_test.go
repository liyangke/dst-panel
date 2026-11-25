package dst

import (
	"strings"
	"testing"
)

const sampleClusterIni = `# 集群公共配置
[GAMEPLAY]
#游戏模式，可选 survival，endless，wilderness
game_mode = survival
#最大玩家人数 （上限16）
max_players = 8
#是否开启玩家对战
pvp = false
pause_when_empty = true

[NETWORK]
cluster_name = 我的饥荒服务器
cluster_password = "a#b"
tick_rate = 15

[MISC]
#是否开启控制台
console_enabled = true

[SHARD]
shard_enabled = true
bind_ip = 127.0.0.1
master_port = 10888
cluster_key = defaultPass

[STEAM]
steam_group_only = false           # 只允许某 Steam 组的成员加入
steam_group_id = 0                 # 指定某个 Steam 组，填写组 ID
`

func TestIniRoundTripKeepsComments(t *testing.T) {
	f := ParseIni(sampleClusterIni)
	out := f.String()
	for _, want := range []string{
		"# 集群公共配置",
		"#游戏模式，可选 survival，endless，wilderness",
		"#最大玩家人数 （上限16）",
		"#是否开启控制台",
		"# 只允许某 Steam 组的成员加入",
		"steam_group_id = 0                 # 指定某个 Steam 组，填写组 ID",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("注释丢失: %q\n--- 实际输出 ---\n%s", want, out)
		}
	}
	// 幂等：再解析一次输出必须完全一致，否则用户文件会被面板逐次改写。
	if again := ParseIni(out).String(); again != out {
		t.Errorf("二次往返不幂等\n--- 第一次 ---\n%s\n--- 第二次 ---\n%s", out, again)
	}
}

func TestIniTypedAccessors(t *testing.T) {
	f := ParseIni(sampleClusterIni)

	if got := f.GetInt("GAMEPLAY", "max_players", 0); got != 8 {
		t.Errorf("max_players = %d, 期望 8", got)
	}
	if !f.GetBool("MISC", "console_enabled", false) {
		t.Error("console_enabled 应为 true")
	}
	if f.GetBool("GAMEPLAY", "pvp", true) {
		t.Error("pvp 应为 false")
	}
	// 未知键要回落默认值而不是崩。
	if !f.GetBool("GAMEPLAY", "不存在的键", true) {
		t.Error("未知键应返回默认值")
	}
	// 引号内 # 不是注释起点。
	if got := f.Unquote("NETWORK", "cluster_password"); got != "a#b" {
		t.Errorf("cluster_password = %q, 期望 a#b（行内注释切分把值截断了）", got)
	}
	if got := f.Get("SHARD", "master_port"); got != "10888" {
		t.Errorf("master_port = %q", got)
	}
}

func TestIniSetPreservesInlineComment(t *testing.T) {
	f := ParseIni(sampleClusterIni)
	f.SetBool("STEAM", "steam_group_only", true)
	f.Set("NETWORK", "tick_rate", "30")
	// 新键落到对应段末尾。
	f.SetInt("GAMEPLAY", "autocommunity_warning", 5)

	out := f.String()
	if !strings.Contains(out, "steam_group_only = true           # 只允许某 Steam 组的成员加入") {
		t.Errorf("改值后行内注释丢失或被压掉空白: %s", lineWith(out, "steam_group_only"))
	}
	if !strings.Contains(out, "tick_rate = 30") {
		t.Errorf("tick_rate 未更新: %s", lineWith(out, "tick_rate"))
	}
	if !strings.Contains(out, "autocommunity_warning = 5") {
		t.Error("新增键未写入")
	}
	// 没被碰过的段落应当逐字不变。
	if !strings.Contains(out, "#是否开启控制台\nconsole_enabled = true") {
		t.Error("MISC 段被意外改写")
	}
}

func TestIniNewSection(t *testing.T) {
	f := ParseIni("[GAMEPLAY]\npvp = false\n")
	f.SetString("NETWORK", "cluster_name", "测试服")
	out := f.String()
	if !strings.Contains(out, "[NETWORK]") || !strings.Contains(out, "cluster_name = 测试服") {
		t.Errorf("新段或值写入异常:\n%s", out)
	}
	if got := f.Unquote("NETWORK", "cluster_name"); got != "测试服" {
		t.Errorf("Unquote = %q", got)
	}
}

// 引擎不会剥引号，这条只能按字节钉住。云机首启那天写出的就是
// game_mode = "survival"，服务端读成 '"survival"' 然后报
// "Game mode '\"survival\"' not found in GAME_MODES"。
func TestIniStringValuesAreWrittenUnquoted(t *testing.T) {
	f := ParseIni("[GAMEPLAY]\n")
	f.SetString("[GAMEPLAY]", "game_mode", "survival")
	f.SetString("[NETWORK]", "cluster_intention", "cooperative")
	f.SetString("[SHARD]", "cluster_key", "gva9pwa8fuyo")
	out := f.String()
	for _, want := range []string{"game_mode = survival", "cluster_intention = cooperative", "cluster_key = gva9pwa8fuyo"} {
		if !strings.Contains(out, want) {
			t.Errorf("少了 %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, `= "`) {
		t.Errorf("字符串值被加了引号，引擎会连引号一起读进配置:\n%s", out)
	}
	// 别人（或旧版本面板）写的带引号文件仍要读得干净。
	if got := ParseIni("[NETWORK]\ncluster_name = \"旧名\"\n").Unquote("NETWORK", "cluster_name"); got != "旧名" {
		t.Errorf("历史带引号文件读回 %q", got)
	}
}

func TestIniStrayKeyWithoutSection(t *testing.T) {
	// Klei 模板偶有散键，不能丢。
	f := ParseIni("orphan = 1\n[GAMEPLAY]\npvp = false\n")
	if !strings.Contains(f.String(), "orphan = 1") {
		t.Errorf("无段散键被丢弃:\n%s", f.String())
	}
}

// 段名的两种写法必须是同一段。cluster.go 那层按文件里看到的样子传 "[NETWORK]"，
// ini 解析出来的是 "NETWORK"；不归一就会多建一段，渲染时再补一层括号，
// 写出引擎根本不读的 [[NETWORK]]——面板显示已保存，服务端一直用模板默认值。
func TestIniSectionNameWithBracketsIsSameSection(t *testing.T) {
	f := ParseIni("[NETWORK]\ncluster_name = \"旧名\"\n")
	f.SetString("[NETWORK]", "cluster_name", "新名")
	out := f.String()
	if strings.Contains(out, "[[") {
		t.Errorf("写出了一个引擎不认的段头:\n%s", out)
	}
	if n := strings.Count(out, "[NETWORK]"); n != 1 {
		t.Errorf("[NETWORK] 出现 %d 次，同一段被拆成两份:\n%s", n, out)
	}
	// 值要落在引擎会读的那一行上，而不是另一份段里。
	if !strings.Contains(out, "cluster_name = 新名") || strings.Contains(out, "旧名") {
		t.Errorf("改值没生效:\n%s", out)
	}
	if got := f.Unquote("NETWORK", "cluster_name"); got != "新名" {
		t.Errorf("用裸段名读回来是 %q", got)
	}
}

// 老文件（或手改过的文件）里同一个键出现两行时，Set 之后只能留一行，
// 且留下的那行就是 Get 读到的那行——否则面板自己都会读到旧值。
func TestIniSetDedupesRepeatedKey(t *testing.T) {
	f := ParseIni("[GAMEPLAY]\nmax_players = 6\n#注释\nmax_players = 12\n")
	f.SetInt("[GAMEPLAY]", "max_players", 8)
	out := f.String()
	if n := strings.Count(out, "max_players"); n != 1 {
		t.Errorf("重复的键没被合掉，剩下 %d 行:\n%s", n, out)
	}
	if f.GetInt("[GAMEPLAY]", "max_players", 0) != 8 {
		t.Errorf("Get 读回来的还是旧值: %s", out)
	}
	if !strings.Contains(out, "#注释") {
		t.Errorf("去重不该吃掉注释:\n%s", out)
	}
}

func TestClusterOrdinal(t *testing.T) {
	if clusterOrdinal("Cluster_10") <= clusterOrdinal("Cluster_2") {
		t.Error("Cluster_10 应排在 Cluster_2 之后")
	}
	if clusterOrdinal("MyWorld_7") != 7 {
		t.Errorf("MyWorld_7 => %d", clusterOrdinal("MyWorld_7"))
	}
}

func lineWith(text, key string) string {
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), key+" ") {
			return l
		}
	}
	return "(未找到)"
}
