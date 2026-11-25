package dst

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// 游戏内三种身份名单。adminlist / whitelist / blocklist 是 Klei 引擎自己读的
// 纯文本文件，一行一个 userid，不支持注释。面板只做"把这一行加进去/删掉"，
// 不把内容改写成别的格式——引擎认不出来就等于玩家莫名其妙进不了服。
const (
	PlayerListAdmin     = "admin"
	PlayerListWhitelist = "whitelist"
	PlayerListBlocklist = "blocklist"
)

var playerListLabels = map[string]string{
	PlayerListAdmin:     "管理员",
	PlayerListWhitelist: "白名单",
	PlayerListBlocklist: "黑名单",
}

// playerListFiles 把名单种类映射到文件名。用 map 而不是拼字符串：
// 拼错一个字符就会写出一份引擎不读的文件，界面上却显示"已保存"。
var playerListFiles = map[string]string{
	PlayerListAdmin:     "adminlist.txt",
	PlayerListWhitelist: "whitelist.txt",
	PlayerListBlocklist: "blocklist.txt",
}

func ValidPlayerList(kind string) bool {
	_, ok := playerListFiles[kind]
	return ok
}

func LabelPlayerList(kind string) string {
	if l, ok := playerListLabels[kind]; ok {
		return l
	}
	return kind
}

// PlayerListFile 返回名单文件的绝对路径。kind 必须先用 ValidPlayerList 校验：
// 这个函数会被 HTTP 路径里的 {kind} 直接调用，不校验就等于让请求方拼路径。
func (p Paths) PlayerListFile(cluster, kind string) (string, bool) {
	name, ok := playerListFiles[kind]
	if !ok {
		return "", false
	}
	return filepath.Join(p.ClusterDir(cluster), name), true
}

// ReadPlayerList 读一份名单。
//
// 容错有边界：空行、首尾空白、CRLF、UTF-8 BOM 都不算内容；除此之外的行原样保留。
// 不按 KU_ 前缀过滤，是因为 Steam 直连的 userid 是纯数字，Klei 账号才是 KU_ 开头，
// 两种混在同一份文件里是正常现象。
func ReadPlayerList(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("读取 %s: %w", filepath.Base(path), err)
	}
	text := strings.TrimPrefix(string(raw), "\ufeff")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	var out []string
	seen := make(map[string]bool)
	for _, line := range strings.Split(text, "\n") {
		id := strings.TrimSpace(line)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out, nil
}

// WritePlayerList 整份重写名单，原子替换并留 .bak。
// 末尾保留一个换行：Klei 自己的文件就是这么结尾的，也方便用户在服务器上直接 cat。
func WritePlayerList(path string, ids []string) error {
	var b strings.Builder
	for _, id := range ids {
		b.WriteString(id)
		b.WriteByte('\n')
	}
	return WriteFileAtomic(path, []byte(b.String()), 0o644)
}

// NormalizePlayerID 校验并整理用户输入的 userid。
//
// 不强制 KU_ 前缀：Steam 平台账号的 userid 是 17 位纯数字，Klei 账号才是 KU_ 开头。
// 这里只挡住"会被写进一行却把一行拆成两行"的字符，以及明显不是 ID 的超长串。
func NormalizePlayerID(raw string) (string, error) {
	id := strings.TrimSpace(raw)
	if id == "" {
		return "", fmt.Errorf("userid 不能为空")
	}
	if len(id) > 64 {
		return "", fmt.Errorf("userid 太长了（最多 64 个字符）")
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '_', r == '-', r == '.':
		default:
			return "", fmt.Errorf("userid 里不能有空格、引号或其它符号：%q", id)
		}
	}
	return id, nil
}
