package dst

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// 控制台命令白名单。
//
// 面板要能"执行简单的命令"，但直接把输入拼进 Lua 等于开了个远程代码执行入口：
// 一个公告文案里带 `") print(...) ; c_shutdown(); ("` 就能关掉服务器。所以这里定死两件事：
// 命令模板由代码写死，用户只能填参数；参数按类型校验后由 LuaStringLiteral 转成字面量再代入。
//
// 面板只提供这一条路：没有任何接口能把一行任意 Lua 送进会话。
// 原始控制台入口留给运维者自己 screen -r 上去敲，那样审计与责任都在面板之外。

// ErrInvalidArg 表示参数没过校验。
// 这类错误必须回 400 并且带上原值，HTTP 层靠 sentinel 判断，不靠匹配中文文案。
var ErrInvalidArg = errors.New("参数不合法")

type ConsoleArg struct {
	Name    string
	Label   string
	Kind    string // int / string / enum
	Default string // int 用十进制字符串，enum 用取值本身，string 用空串
	Min     int64
	Max     int64
	Choices []ConsoleChoice
	MaxLen  int
}

type ConsoleChoice struct {
	Value string // 传给游戏的形式（保持 Klei 认得的英文标识）
	Label string // 给用户看的中文
}

type ConsoleCommand struct {
	Key   string
	Label string
	Desc  string
	// Danger 为 true 时前端必须二次确认：这类命令会改存档或中断在线玩家。
	Danger bool
	// Pattern 里的 %s 按 Args 顺序代入已校验的参数。% 只允许出现在 %s。
	Pattern string
	Args    []ConsoleArg
}

// consoleCommands 是界面"常用命令"的来源。新增命令时先确认 Klei 真有这条，
// 再确认它在专用服务端（没有本地玩家）上也能生效。
var consoleCommands = map[string]ConsoleCommand{
	"save": {
		Key: "save", Label: "立即保存", Desc: "把当前世界状态写入存档，不影响继续运行。",
		Pattern: "c_save()",
	},
	"rollback": {
		Key: "rollback", Label: "回档", Danger: true,
		Desc:    "把世界退回若干次自动保存之前的状态。在线玩家身上的临时物品可能一并消失。",
		Pattern: "c_rollback(%s)",
		Args: []ConsoleArg{{
			Name: "count", Label: "回退次数", Kind: "int", Default: "1", Min: 1, Max: 10,
		}},
	},
	"reset_rollback": {
		Key: "reset_rollback", Label: "重置回档基线", Danger: true,
		Desc:    "以当前状态为新的起点，清空可回退的历史。开新阶段或修完档后用一次。",
		Pattern: "c_reset()",
	},
	"announce": {
		Key: "announce", Label: "全服公告",
		Desc:    "在所有玩家屏幕上方显示一行文字。",
		Pattern: "c_announce(%s)",
		Args: []ConsoleArg{{
			Name: "message", Label: "公告内容", Kind: "string", MaxLen: 200,
		}},
	},
	"netstats": {
		Key: "netstats", Label: "网络统计",
		Desc:    "在服务端控制台打印各连接的上行下行与延迟统计。",
		Pattern: "c_netstats()",
	},
	"worldstate": {
		Key: "worldstate", Label: "导出世界状态",
		Desc:    "把世界组件状态打印到 server_log.txt，排查怪物不刷新、季节不对这类问题用。",
		Pattern: "c_dumpworldstate()",
	},
	"auto_teleport": {
		Key: "auto_teleport", Label: "切换落地自动汇合",
		Desc:    "开启后新玩家进服会被传到世界出生点，适合朋友局防止分散。",
		Pattern: "c_autoteleportplayers()",
	},
	"skip_days": {
		Key: "skip_days", Label: "快进整天", Danger: true,
		Desc:    "直接把世界时间往前推，跳过这些天的所有事件与掉落结算，属于开发工具，可能让任务与季节状态对不齐。",
		Pattern: "c_skip(%s)",
		Args: []ConsoleArg{{
			Name: "days", Label: "跳过天数", Kind: "int", Default: "1", Min: 1, Max: 20,
		}},
	},
	"force_phase": {
		Key: "force_phase", Label: "强制切换时段", Danger: true,
		Desc:    "立刻进入指定时段，会打断当前昼夜进度。",
		Pattern: "c_simphase(%s)",
		Args: []ConsoleArg{{
			Name: "phase", Label: "目标时段", Kind: "enum", Default: "day",
			Choices: []ConsoleChoice{
				{Value: "day", Label: "白天"},
				{Value: "dusk", Label: "黄昏"},
				{Value: "night", Label: "夜晚"},
			},
		}},
	},
	"regenerate_shard": {
		Key: "regenerate_shard", Label: "重生这个世界", Danger: true,
		Desc:    "删掉当前世界的地图重新生成，玩家存档保留但世界全换。执行前必须先做一次外部备份。",
		Pattern: "c_regenerateshard()",
	},
}

// ConsoleCommandKeys 返回按界面顺序排好的命令 key。
// 排序规则：非危险命令在前，同组按 key 字典序，保证 UI 上按钮位置稳定。
func ConsoleCommandKeys() []string {
	keys := make([]string, 0, len(consoleCommands))
	for k := range consoleCommands {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := consoleCommands[keys[i]], consoleCommands[keys[j]]
		if a.Danger != b.Danger {
			return !a.Danger
		}
		return keys[i] < keys[j]
	})
	return keys
}

func ConsoleCommandSpec(key string) (ConsoleCommand, bool) {
	c, ok := consoleCommands[key]
	return c, ok
}

// BuildConsoleLine 按 key 取模板、校验参数、生成单行 Lua 命令。
// 返回值一定不含换行，可以直接交给 Driver.Console。
func BuildConsoleLine(key string, args map[string]string) (string, error) {
	cmd, ok := consoleCommands[key]
	if !ok {
		return "", fmt.Errorf("不支持的命令 %q，请从常用命令列表里选", key)
	}
	parts := make([]any, 0, len(cmd.Args))
	for _, a := range cmd.Args {
		v, err := a.literal(args)
		if err != nil {
			return "", fmt.Errorf("%w: %s: %w", ErrInvalidArg, a.Label, err)
		}
		parts = append(parts, v)
	}
	line := fmt.Sprintf(cmd.Pattern, parts...)
	if err := checkSingleLine(line); err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidArg, err)
	}
	return line, nil
}

// literal 把用户填的参数变成可以直接拼进 Lua 的片段。
// 关键点：字符串和枚举都走 LuaStringLiteral，永远不会把原始输入直接当代码。
func (a ConsoleArg) literal(args map[string]string) (string, error) {
	raw, ok := args[a.Name]
	if !ok || strings.TrimSpace(raw) == "" {
		raw = a.Default
	}
	switch a.Kind {
	case "int":
		n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil {
			return "", fmt.Errorf("要填整数，收到的是 %q", raw)
		}
		if n < a.Min || n > a.Max {
			return "", fmt.Errorf("取值范围是 %d 到 %d，收到 %d", a.Min, a.Max, n)
		}
		return strconv.FormatInt(n, 10), nil
	case "enum":
		for _, c := range a.Choices {
			if c.Value == raw {
				return LuaStringLiteral(c.Value), nil
			}
		}
		var allowed []string
		for _, c := range a.Choices {
			allowed = append(allowed, c.Label)
		}
		return "", fmt.Errorf("只能选 %s", strings.Join(allowed, "、"))
	case "string":
		if raw == "" {
			return "", fmt.Errorf("不能为空")
		}
		if a.MaxLen > 0 && len([]rune(raw)) > a.MaxLen {
			return "", fmt.Errorf("最长 %d 个字，当前 %d 个字", a.MaxLen, len([]rune(raw)))
		}
		if strings.ContainsRune(raw, '\n') || strings.ContainsRune(raw, '\r') {
			return "", fmt.Errorf("不能包含换行")
		}
		return LuaStringLiteral(raw), nil
	default:
		return "", fmt.Errorf("内部错误：未知参数类型 %q", a.Kind)
	}
}

// checkSingleLine 是最后一道闸：模板或参数出了问题，宁可拒绝也不注入多行。
func checkSingleLine(line string) error {
	if strings.ContainsAny(line, "\r\n") {
		return fmt.Errorf("生成的命令含换行，已拒绝")
	}
	if len(line) > 2000 {
		return fmt.Errorf("生成的命令过长（%d 字节），已拒绝", len(line))
	}
	return nil
}
