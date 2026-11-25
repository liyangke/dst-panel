package dst

import (
	"bufio"
	"fmt"
	"strconv"
	"strings"
)

// IniFile 是保形的 cluster.ini / server.ini 表示。
//
// 之所以不直接用第三方 ini 库：DST 的 ini 带大量中文注释，且 Klei 会忽略未知键，
// 平台必须能在不认识某些键时原样写回，否则用户手加的调优项会被面板吃掉。
type IniFile struct {
	sections []*iniSection
	// preamble 是第一个 section 之前的注释/空行。
	preamble []iniLine
}

type iniSection struct {
	name  string
	lines []iniLine
}

type iniLine struct {
	kind   lineKind
	key    string
	value  string
	suffix string // 行尾注释，含前导空白
	raw    string // 注释行或空行的原文
}

type lineKind int

const (
	kindComment lineKind = iota
	kindKV
)

func ParseIni(text string) *IniFile {
	f := &IniFile{}
	var cur *iniSection
	scanner := bufio.NewScanner(strings.NewReader(text))
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]"):
			name := strings.TrimSpace(trimmed[1 : len(trimmed)-1])
			cur = f.section(name)
		case trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";"):
			l := iniLine{kind: kindComment, raw: line}
			if cur == nil {
				f.preamble = append(f.preamble, l)
			} else {
				cur.lines = append(cur.lines, l)
			}
		default:
			l := parseKV(line)
			if cur == nil {
				// 无 section 头的散落键，归到隐式根段，避免丢数据。
				cur = f.section("")
			}
			cur.lines = append(cur.lines, l)
		}
	}
	return f
}

func parseKV(line string) iniLine {
	eq := strings.Index(line, "=")
	if eq < 0 {
		return iniLine{kind: kindComment, raw: line}
	}
	key := strings.TrimSpace(line[:eq])
	rest := line[eq+1:]
	value, suffix := splitInlineComment(rest)
	return iniLine{kind: kindKV, key: key, value: value, suffix: suffix, raw: line}
}

// splitInlineComment 把 `false           # 只允许某 Steam 组` 拆成值和注释。
// 引号内的 # 不算注释起点，否则 cluster_password = "a#b" 会被截断。
// 注释前的空白必须一并归入 suffix，否则回写会变成 `true# 注释` —— DST 会把
// 整个 `true#` 当值解析。
func splitInlineComment(rest string) (value, suffix string) {
	inQuote := byte(0)
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		switch {
		case inQuote != 0:
			if c == inQuote {
				inQuote = 0
			}
		case c == '"' || c == '\'':
			inQuote = c
		case c == '#' || c == ';':
			value = strings.TrimSpace(rest[:i])
			// 把 i 之前那段连续空白留作 suffix 开头。
			gap := i
			for gap > 0 && (rest[gap-1] == ' ' || rest[gap-1] == '\t') {
				gap--
			}
			return value, rest[gap:]
		}
	}
	return strings.TrimSpace(rest), ""
}

func (f *IniFile) section(name string) *iniSection {
	name = normSection(name)
	for _, s := range f.sections {
		if s.name == name {
			return s
		}
	}
	s := &iniSection{name: name}
	f.sections = append(f.sections, s)
	return s
}

// normSection 统一段名形态：解析时 "[NETWORK]" 会剥掉括号变成 "NETWORK"，
// 而调用方（cluster.go 那一层）习惯按文件里看到的写法传 "[NETWORK]"。
// 两边不归一就会各建一段：渲染时再补一层括号，写出 [[NETWORK]]，
// 引擎只认单括号，于是面板设置的值全落进一个它不读的段里——
// 界面显示"已保存"，服务端却一直在用模板默认值。
func normSection(name string) string {
	name = strings.TrimSpace(name)
	for len(name) >= 2 && name[0] == '[' && name[len(name)-1] == ']' {
		name = strings.TrimSpace(name[1 : len(name)-1])
	}
	return name
}

func (f *IniFile) Get(section, key string) string {
	sec := f.findSection(section)
	if sec == nil {
		return ""
	}
	for _, l := range sec.lines {
		if l.kind == kindKV && l.key == key {
			return l.value
		}
	}
	return ""
}

func (f *IniFile) Set(section, key, value string) {
	sec := f.section(section)
	first := -1
	for i, l := range sec.lines {
		if l.kind == kindKV && l.key == key {
			first = i
			break
		}
	}
	if first < 0 {
		sec.lines = append(sec.lines, iniLine{kind: kindKV, key: key, value: value})
		return
	}
	l := sec.lines[first]
	l.value = value
	// 同一个键出现多行时只留第一行：Get 读的就是第一行，写入若把值放进别的那行，
	// 面板自己都会读到旧值。空行和注释原样保留。
	out := make([]iniLine, 0, len(sec.lines))
	out = append(out, l)
	for i, other := range sec.lines {
		if i == first {
			continue
		}
		if other.kind == kindKV && other.key == key {
			continue
		}
		out = append(out, other)
	}
	sec.lines = out
}

func (f *IniFile) GetBool(section, key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(f.Get(section, key)))
	switch v {
	case "true", "1", "yes":
		return true
	case "false", "0", "no":
		return false
	case "":
		return def
	default:
		return def
	}
}

func (f *IniFile) SetBool(section, key string, value bool) {
	f.Set(section, key, strconv.FormatBool(value))
}

func (f *IniFile) GetInt(section, key string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(f.Get(section, key)))
	if err != nil {
		return def
	}
	return n
}

func (f *IniFile) SetInt(section, key string, value int) {
	f.Set(section, key, strconv.Itoa(value))
}

// SetString 写字符串值：不加引号，和 Klei 自己生成的 cluster.ini 一致。
//
// 这一条是云机首启拿引擎日志纠正的：原先以为"字符串要加引号"，于是写成
// game_mode = "survival"，服务端按字面读，日志里就是
// Game mode '"survival"' not found in GAME_MODES —— 它不会剥引号。
// 同理 cluster_name / cluster_password / cluster_key / cluster_intention
// 加了引号都会变成携带引号的值，后果从房间名显示到好友无法连接不等。
// 读方向仍保留 Unquote：别人手写过带引号的文件要能认。
func (f *IniFile) SetString(section, key, value string) {
	f.Set(section, key, value)
}

func (f *IniFile) Unquote(section, key string) string {
	v := f.Get(section, key)
	if len(v) < 2 || v[0] != '"' || v[len(v)-1] != '"' {
		return v
	}
	inner := v[1 : len(v)-1]
	var b strings.Builder
	for i := 0; i < len(inner); i++ {
		if inner[i] == '\\' && i+1 < len(inner) && (inner[i+1] == '"' || inner[i+1] == '\\') {
			// 只解 SetQuoted 会写出的两种转义；用户手写的其他转义原样保留。
			b.WriteByte(inner[i+1])
			i++
			continue
		}
		b.WriteByte(inner[i])
	}
	return b.String()
}

func (f *IniFile) findSection(name string) *iniSection {
	name = normSection(name)
	for _, s := range f.sections {
		if s.name == name {
			return s
		}
	}
	return nil
}

// Sections 返回所有段名，供前端渲染表单分组。
func (f *IniFile) Sections() []string {
	out := make([]string, 0, len(f.sections))
	for _, s := range f.sections {
		out = append(out, s.name)
	}
	return out
}

func (f *IniFile) Keys(section string) []string {
	sec := f.findSection(section)
	if sec == nil {
		return nil
	}
	var out []string
	for _, l := range sec.lines {
		if l.kind == kindKV {
			out = append(out, l.key)
		}
	}
	return out
}

func (f *IniFile) String() string {
	var out []string
	add := func(s string) { out = append(out, s) }

	for _, l := range f.preamble {
		add(renderLine(l))
	}
	for i, s := range f.sections {
		if s.name == "" {
			// 隐式根段（无 section 头的散键）直接接在前面内容之后。
			for _, l := range s.lines {
				add(renderLine(l))
			}
			continue
		}
		// 段间恰好一个空行：原文件的空行会作为数据保留，若再无条件补一个，
		// 每次往返就会多长出一批空行。
		if i > 0 && !endsWithBlank(out) {
			add("")
		}
		add("[" + s.name + "]")
		for _, l := range s.lines {
			add(renderLine(l))
		}
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, "\n") + "\n"
}

// renderLine 把一条记录还原成单行文本（不含换行符）。
func renderLine(l iniLine) string {
	if l.kind == kindKV {
		return fmt.Sprintf("%s = %s%s", l.key, l.value, l.suffix)
	}
	return l.raw
}

func endsWithBlank(lines []string) bool {
	return len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == ""
}

func (f *IniFile) Save(path string) error {
	return WriteFileAtomic(path, []byte(f.String()), 0o644)
}

func LoadIni(path string) (*IniFile, string, error) {
	text, err := ReadTextFile(path)
	if err != nil {
		return nil, "", err
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return ParseIni(text), text, nil
}
