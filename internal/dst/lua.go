package dst

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// Lua 表编解码器，服务 worldgenoverride.lua / modoverrides.lua / cluster_config.lua / modinfo.lua。
//
// 只做数据解析，不做 Lua 求值——真求值会带来代码执行风险。
// 两种文件形状要分开读：
//   - ParseLuaTable：`return { ... }`，纯数据。这是面板自己写的、以及引擎重写的
//     worldgenoverride.lua 和 modoverrides.lua 的形状。读到看不懂的东西就报错，
//     因为那种文件里出现函数说明文件真的坏了。
//   - ParseLuaAssignments：一串顶层裸赋值（`name = "..."`）。modinfo.lua 是这种，
//     里面出现函数、函数调用都是正常写法，跳过不收，不算错误；变量引用只在
//     "同一个文件里上面已经读到过那张表"时接住（local 名单复用），见 tryRefValue。

type ValueKind int

// KindInvalid 必须占零值：缺键时 Get 返回零值 LuaValue，若零值是 KindTable
// 就会伪装成"一张空表"，让 Nested/AsBool 把 nil 当合法值解引用。
const (
	KindInvalid ValueKind = iota
	KindTable
	KindString
	KindNumber
	KindBool
)

type LuaValue struct {
	Kind ValueKind
	S    string
	N    float64
	B    bool
	T    *LuaTable
}

// LuaTable 保序。DST 会按 key 读，顺序不影响正确性，但面板要做无损往返，
// 顺序变了会让用户 git diff 出现整片噪音。
type LuaTable struct {
	entries []luaEntry
}

type luaEntry struct {
	// key 为 nil 表示数组式写法（leveldataoverride 里少量字段是数组）。
	key *string
	val LuaValue
}

func NewTable() *LuaTable { return &LuaTable{} }

func (t *LuaTable) Len() int { return len(t.entries) }

func (t *LuaTable) IsArray() bool {
	if len(t.entries) == 0 {
		return false
	}
	for _, e := range t.entries {
		if e.key != nil {
			return false
		}
	}
	return true
}

func strp(s string) *string { return &s }

// Get 返回键值；键不存在时返回零值 LuaValue（Kind=KindTable、T=nil），
// 因此调用方必须用 AsString/AsBool 这类带默认值的取值器，不能直接解引用。
func (t *LuaTable) Get(key string) LuaValue {
	for _, e := range t.entries {
		if e.key != nil && *e.key == key {
			return e.val
		}
	}
	return LuaValue{}
}

func (t *LuaTable) Has(key string) bool {
	for _, e := range t.entries {
		if e.key != nil && *e.key == key {
			return true
		}
	}
	return false
}

// Set 更新已有键（保持原位置），否则追加到末尾。
func (t *LuaTable) Set(key string, v LuaValue) {
	for i, e := range t.entries {
		if e.key != nil && *e.key == key {
			t.entries[i].val = v
			return
		}
	}
	t.entries = append(t.entries, luaEntry{key: strp(key), val: v})
}

func (t *LuaTable) Delete(key string) {
	out := t.entries[:0]
	for _, e := range t.entries {
		if e.key == nil || *e.key != key {
			out = append(out, e)
		}
	}
	t.entries = out
}

func (t *LuaTable) Keys() []string {
	out := make([]string, 0, len(t.entries))
	for _, e := range t.entries {
		if e.key != nil {
			out = append(out, *e.key)
		}
	}
	return out
}

// SortedKeys 给前端渲染表单用，避免同一份配置每次刷新顺序抖动。
func (t *LuaTable) SortedKeys() []string {
	k := t.Keys()
	sort.Strings(k)
	return k
}

// Values 按书写顺序返回数组式写法（`{ "a", "b" }`）的每一项。
// Keys() 只给有名字的键，所以 modinfo.lua 里 mod_dependencies 这种列表必须走这里读。
func (t *LuaTable) Values() []LuaValue {
	out := make([]LuaValue, 0, len(t.entries))
	for _, e := range t.entries {
		if e.key == nil {
			out = append(out, e.val)
		}
	}
	return out
}

func (t *LuaTable) Append(v LuaValue) { t.entries = append(t.entries, luaEntry{val: v}) }

// 构造辅助
func String(v string) LuaValue   { return LuaValue{Kind: KindString, S: v} }
func Number(v float64) LuaValue  { return LuaValue{Kind: KindNumber, N: v} }
func Bool(v bool) LuaValue       { return LuaValue{Kind: KindBool, B: v} }
func Table(v *LuaTable) LuaValue { return LuaValue{Kind: KindTable, T: v} }

func (v LuaValue) AsBool(def bool) bool {
	if v.Kind == KindBool {
		return v.B
	}
	return def
}

func (v LuaValue) AsString(def string) string {
	if v.Kind == KindString {
		return v.S
	}
	return def
}

// AsNumber 取数值。Klei 自己写出的文件里数字就是数字，但第三方模组生成的覆盖偶尔写成
// "1" 这种带引号的形式，这里一并收掉，免得面板把一个能用的配置判成坏配置。
func (v LuaValue) AsNumber(def float64) float64 {
	switch v.Kind {
	case KindNumber:
		return v.N
	case KindString:
		if f, err := strconv.ParseFloat(strings.TrimSpace(v.S), 64); err == nil {
			return f
		}
	}
	return def
}

// Nested 沿路径取子表，任一段不存在或不是表则返回 false。
func (t *LuaTable) Nested(path ...string) (*LuaTable, bool) {
	cur := t
	for _, p := range path {
		v := cur.Get(p)
		if v.Kind != KindTable || v.T == nil {
			return nil, false
		}
		cur = v.T
	}
	return cur, true
}

// EnsureNested 沿路径建表，缺失或类型不符的中间层替换成空表。
func (t *LuaTable) EnsureNested(path ...string) *LuaTable {
	cur := t
	for _, p := range path {
		v := cur.Get(p)
		if v.Kind != KindTable || v.T == nil {
			sub := NewTable()
			cur.Set(p, Table(sub))
			cur = sub
			continue
		}
		cur = v.T
	}
	return cur
}

// ---------- 解析 ----------

// kleiHeaderRE 是引擎 TheSim:SetPersistentString 给持久化文件加的头。
//
// 2026-10-31 云机实测两处：助手 MOD 写的 status/result/processed 三个文件，以及
// 引擎每次存世界时重写的那份 worldgenoverride.lua（mainfunctions.lua 的世界保存回调
// 里 UpdateWorldGenOverride 走的就是 SetPersistentString），开头都是 11 字节
// "KLEI     1 "（KLEI + 五个空格 + 版本号 + 一个空格），正文从第 12 字节才开始。
// MOD 与引擎自己读时 GetPersistentString 会认这个头，面板用的是普通 os.ReadFile，
// 不剥掉就每一句都是"不是合法 JSON / Lua 表解析失败"——界面全线飘红，
// 而游戏其实跑得好好的。版本号那段按数字通配，别引擎改成 v2 就变成解析失败。
var kleiHeaderRE = regexp.MustCompile(`^KLEI[ \t]+\d+[ \t]+`)

// StripKleiHeader 剥掉头；没有头就原样返回（面板自己写的文件没有头，两种都要能读）。
func StripKleiHeader(raw []byte) []byte {
	if i := kleiHeaderRE.FindIndex(raw); i != nil {
		return raw[i[1]:]
	}
	return raw
}

func stripKleiHeaderString(text string) string {
	if i := kleiHeaderRE.FindStringIndex(text); i != nil {
		return text[i[1]:]
	}
	return text
}

type luaParser struct {
	src   string
	pos   int
	depth int
	// lenient 是给 modinfo.lua 用的：那里的值可以是函数、函数调用或变量引用，
	// 面板不求值，就整段跳到下一个条目。modoverrides / worldgenoverride 是纯数据，
	// 保持严格——那种文件里出现看不懂的东西，说明文件真的坏了，必须报出来。
	lenient bool
	// refs 记下 lenient 模式下已经读到的"名字 = 字面量"。只用来接住
	// `values = POLL_CHOICES` 这种把表先存进 local 再引用的写法，见 tryRefValue。
	// 存的是指针（表就是引用语义），modinfo 这一侧只读不写。
	refs map[string]LuaValue
}

const luaMaxDepth = 32

func ParseLuaTable(text string) (*LuaTable, error) {
	// 引擎会把读进去的 Lua 存档文件重写一遍并带上它的头，
	// 所以每一位调用方都该能读带头的文件；在这里统一剥，省得每一处再记一次。
	text = stripKleiHeaderString(text)
	p := &luaParser{src: text}
	p.skipSpace()
	// 允许带或不带前导 return
	if p.hasPrefixWord("return") {
		p.pos += len("return")
		p.skipSpace()
	}
	tbl, err := p.parseTable()
	if err != nil {
		return nil, err
	}
	p.skipSpace()
	if p.pos < len(p.src) {
		// return 之后的尾巴（如多余语句）不用报错，Klei 生成的文件偶有尾随内容。
		rest := strings.TrimSpace(p.src[p.pos:])
		if rest != "" && rest != ";" {
			return tbl, fmt.Errorf("Lua 表结束后仍有内容: %.60s", rest)
		}
	}
	return tbl, nil
}

func (p *luaParser) parseTable() (*LuaTable, error) {
	p.skipSpace()
	if p.pos >= len(p.src) || p.src[p.pos] != '{' {
		return nil, fmt.Errorf("第 %d 字节处期望 '{'，实际是 %s", p.pos, p.context())
	}
	p.pos++
	if p.depth >= luaMaxDepth {
		return nil, fmt.Errorf("Lua 表嵌套超过 %d 层，疑似异常文件", luaMaxDepth)
	}
	p.depth++
	defer func() { p.depth-- }()

	t := NewTable()
	for {
		p.skipSpace()
		if p.pos >= len(p.src) {
			return nil, fmt.Errorf("表未闭合，起始于第 %d 字节", p.pos)
		}
		if p.src[p.pos] == '}' {
			p.pos++
			return t, nil
		}
		key, isKV, err := p.parseKey()
		if err != nil {
			return nil, err
		}
		// 宽松模式先试"值是前面定义过的名字"（values = POLL_CHOICES 这种）。
		// 接不住再走原来的判断：不是数据（函数、调用、没见过的名字）就整段跳掉，这一条不收。
		var val LuaValue
		resolved := false
		if p.lenient {
			if v, ok := p.tryRefValue(false); ok {
				val, resolved = v, true
			}
		}
		if !resolved {
			if p.lenient && p.startsNonDataValue() {
				p.skipNonDataValue(false)
				continue
			}
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			// 宽松模式下，值后面紧跟的不是条目分隔符，说明这是一个只解出一半的表达式
			// （`n = 1 + 2`、`s = "a" .. b`）。半截值比空值更容易误导人，整条丢掉。
			if p.lenient && !p.atEntryEnd() {
				p.scanTo(false)
				continue
			}
			val = v
		}
		if isKV {
			t.Set(key, val)
		} else {
			t.Append(val)
		}
		p.skipSpace()
		if p.pos < len(p.src) && (p.src[p.pos] == ',' || p.src[p.pos] == ';') {
			p.pos++
		}
	}
}

// parseKey 返回键名与是否为键值对；数组元素 isKV=false。
func (p *luaParser) parseKey() (string, bool, error) {
	p.skipSpace()
	if p.pos >= len(p.src) {
		return "", false, fmt.Errorf("已到文件尾，期望键")
	}
	// ["workshop-1158324974"] = ...
	if p.src[p.pos] == '[' {
		p.pos++
		p.skipSpace()
		q, err := p.parseQuoted()
		if err != nil {
			return "", false, fmt.Errorf("方括号键必须是字符串: %w", err)
		}
		p.skipSpace()
		if p.pos >= len(p.src) || p.src[p.pos] != ']' {
			return "", false, fmt.Errorf("第 %d 字节处方括号键缺少 ']'", p.pos)
		}
		p.pos++
		p.skipSpace()
		if p.pos >= len(p.src) || p.src[p.pos] != '=' {
			return "", false, fmt.Errorf("第 %d 字节处方括号键后缺少 '='", p.pos)
		}
		p.pos++
		return q, true, nil
	}
	// 数字键 [1] = 形式在 DST 配置里不出现，此处不支持，避免误吞数组元素。
	if isLuaIdentStart(rune(p.src[p.pos])) {
		start := p.pos
		for p.pos < len(p.src) && isLuaIdentRune(rune(p.src[p.pos])) {
			p.pos++
		}
		word := p.src[start:p.pos]
		save := p.pos
		p.skipSpace()
		if p.pos < len(p.src) && p.src[p.pos] == '=' && (p.pos+1 >= len(p.src) || p.src[p.pos+1] != '=') {
			p.pos++
			return word, true, nil
		}
		p.pos = save
	}
	return "", false, nil
}

func (p *luaParser) parseValue() (LuaValue, error) {
	p.skipSpace()
	if p.pos >= len(p.src) {
		return LuaValue{}, fmt.Errorf("已到文件尾，期望值")
	}
	switch {
	case p.src[p.pos] == '{':
		tbl, err := p.parseTable()
		if err != nil {
			return LuaValue{}, err
		}
		return Table(tbl), nil
	case p.longStringLevel() >= 0:
		s, err := p.parseLongString()
		if err != nil {
			return LuaValue{}, err
		}
		return String(s), nil
	case p.src[p.pos] == '"' || p.src[p.pos] == '\'':
		s, err := p.parseQuoted()
		if err != nil {
			return LuaValue{}, err
		}
		return String(s), nil
	case isLuaIdentStart(rune(p.src[p.pos])):
		start := p.pos
		for p.pos < len(p.src) && isLuaIdentRune(rune(p.src[p.pos])) {
			p.pos++
		}
		switch p.src[start:p.pos] {
		case "true":
			return Bool(true), nil
		case "false":
			return Bool(false), nil
		}
		return LuaValue{}, fmt.Errorf("第 %d 字节处出现裸标识符 %q，DST 配置表不允许引用变量", start, p.src[start:p.pos])
	case p.src[p.pos] == '-' || p.src[p.pos] == '.' || isDigitByte(p.src[p.pos]):
		return p.parseNumber()
	}
	return LuaValue{}, fmt.Errorf("第 %d 字节处无法解析，附近内容: %s", p.pos, p.context())
}

func (p *luaParser) parseQuoted() (string, error) {
	q := p.src[p.pos]
	p.pos++
	var b strings.Builder
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == '\n' || c == '\r' {
			// Lua 5.1 里引号字符串不能跨行，跨了就说明引号少了一半。
			// 不报出来的话，下一行会被当成这个字符串的内容吞掉——
			// 一个被截断的文件就会伪装成"读成功了，只是字段少了几个"。
			return "", fmt.Errorf("第 %d 字节处的字符串引号未闭合（引号字符串不能跨行）", p.pos)
		}
		if c == q {
			p.pos++
			return b.String(), nil
		}
		if c == '\\' && p.pos+1 < len(p.src) {
			n := p.src[p.pos+1]
			switch n {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case 'a':
				b.WriteByte('\a')
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'v':
				b.WriteByte('\v')
			case '\\':
				b.WriteByte('\\')
			case '"':
				b.WriteByte('"')
			case '\'':
				b.WriteByte('\'')
			default:
				if isDigitByte(n) {
					// \ddd 是最多三位的十进制字节值，Lua 5.1 支持，
					// 面板自己写出的控制字符就走这个形式，得能读回来。
					digits := 0
					for p.pos+1+digits < len(p.src) && digits < 3 && isDigitByte(p.src[p.pos+1+digits]) {
						digits++
					}
					v, err := strconv.Atoi(p.src[p.pos+1 : p.pos+1+digits])
					if err != nil || v > 255 {
						return "", fmt.Errorf("数字转义不合法，附近 %s", p.context())
					}
					b.WriteByte(byte(v))
					p.pos += 1 + digits
					continue
				}
				// Lua 的 \" 之外少见转义，原样保留以免破坏数据
				b.WriteByte('\\')
				b.WriteByte(n)
			}
			p.pos += 2
			continue
		}
		b.WriteByte(c)
		p.pos++
	}
	return "", fmt.Errorf("字符串引号未闭合")
}

func (p *luaParser) parseNumber() (LuaValue, error) {
	start := p.pos
	// Lua 的负号是一元运算符，和字面量之间可以有空白：实测有人写 `priority = - 0xf423f`。
	neg := false
	if p.pos < len(p.src) && p.src[p.pos] == '-' {
		neg = true
		p.pos++
		p.skipInlineSpace()
	}
	// 十六进制整数：Lua 5.1 认，第三方 modinfo 真有人这么写 api_version
	// （实测 1207269058 整份是一行，`api_version = 0x6`）。不认的话这里只能读到
	// "0" 就停，那条语句会被当成半截表达式整条丢掉——明明文件是好的。
	if p.pos+1 < len(p.src) && p.src[p.pos] == '0' && (p.src[p.pos+1] == 'x' || p.src[p.pos+1] == 'X') &&
		p.pos+2 < len(p.src) && isHexByte(p.src[p.pos+2]) {
		p.pos += 2
		hexStart := p.pos
		for p.pos < len(p.src) && isHexByte(p.src[p.pos]) {
			p.pos++
		}
		v, err := strconv.ParseUint(p.src[hexStart:p.pos], 16, 64)
		if err != nil {
			return LuaValue{}, fmt.Errorf("第 %d 字节处十六进制数字非法: %q", start, p.src[start:p.pos])
		}
		if neg {
			return Number(-float64(v)), nil
		}
		return Number(float64(v)), nil
	}
	for p.pos < len(p.src) && (isDigitByte(p.src[p.pos]) || p.src[p.pos] == '.' ||
		p.src[p.pos] == 'e' || p.src[p.pos] == 'E' ||
		((p.src[p.pos] == '+' || p.src[p.pos] == '-') && p.pos > start && (p.src[p.pos-1] == 'e' || p.src[p.pos-1] == 'E'))) {
		p.pos++
	}
	text := p.src[start:p.pos]
	f, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return LuaValue{}, fmt.Errorf("第 %d 字节处数字非法: %q", start, text)
	}
	return Number(f), nil
}

func (p *luaParser) hasPrefixWord(word string) bool {
	if !strings.HasPrefix(p.src[p.pos:], word) {
		return false
	}
	rest := p.src[p.pos+len(word):]
	if rest == "" {
		return true
	}
	return !isLuaIdentRune(rune(rest[0]))
}

// skipInlineSpace 只跳空格和制表符，不跨行。顶层赋值要求"名字和等号在同一行"：
// 跨行去找等号会把 `local x`（没有赋值的声明）和下一条语句连成一条，把后一条吃掉。
func (p *luaParser) skipInlineSpace() {
	for p.pos < len(p.src) && (p.src[p.pos] == ' ' || p.src[p.pos] == '\t') {
		p.pos++
	}
}

func (p *luaParser) skipSpace() {
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			p.pos++
			continue
		}
		// 注释：-- 到行尾，或 --[[ ]] / --[=[ ]=] 块注释
		if c == '-' && p.pos+1 < len(p.src) && p.src[p.pos+1] == '-' {
			p.scanComment()
			continue
		}
		return
	}
}

func (p *luaParser) context() string {
	lo := p.pos - 12
	if lo < 0 {
		lo = 0
	}
	hi := p.pos + 24
	if hi > len(p.src) {
		hi = len(p.src)
	}
	return strconv.Quote(p.src[lo:hi])
}

func isDigitByte(c byte) bool { return c >= '0' && c <= '9' }

func isHexByte(c byte) bool {
	return isDigitByte(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func isLuaIdentStart(r rune) bool {
	return r == '_' || unicode.IsLetter(r)
}

func isLuaIdentRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

// ---------- modinfo.lua：顶层一串裸赋值 ----------

// ParseLuaAssignments 读 modinfo.lua 这一类"文件本身就是一串赋值"的 Lua。
//
// 引擎那边是这么读的（modindex.lua 的 ModIndex:InitializeModInfo）：
// kleiloadlua 出 modinfo.lua，再 RunInEnvironment(fn, env)，然后从 env 里取
// name / api_version / configuration_options 这些全局变量。所以真实 modinfo.lua 长这样：
//
//	version = "1.0.0"
//	name = "某某模组"
//	description = [[多行说明]]
//	local CHOICES = { {description = "每 1 秒", data = 1} }
//	settings = { { name = "poll", values = CHOICES, default = 1 } }
//
// 而不是 `return { ... }`。早前面板只用 ParseLuaTable 读它，于是连自家助手模组
// 在界面上都变成"名字空白、只能按目录名识别"——而 modinfo 恰恰是模组名、版本、
// 依赖、客户端是否必须订阅这些信息的唯一来源。
//
// 这里仍然不求值：只有字符串/数字/布尔/表这类数据会收进结果。碰到函数、函数调用
// （ChooseTranslationTable({...})）、变量引用（values = CHOICES）就把整段跳到下一个条目，
// 这条不收值——那是 modinfo.lua 的正常写法，不算文件坏了。
// local 赋值照样收：引擎执行完也把它留在 env 里，多带几个键不影响按名字取。
func ParseLuaAssignments(text string) (*LuaTable, error) {
	text = stripKleiHeaderString(text)
	p := &luaParser{src: text, lenient: true, refs: map[string]LuaValue{}}
	p.skipSpace()
	// 少数模组（以及面板早期的样例文件）写成 return { ... }。引擎碰到那种其实读不到 env，
	// 但面板没必要比引擎更挑：能按表读回来就读，用户看到的是完整信息而不是空白。
	if p.hasPrefixWord("return") {
		p.pos += len("return")
		p.skipSpace()
		if p.pos < len(p.src) && p.src[p.pos] == '{' {
			return p.parseTable()
		}
	}
	out := NewTable()
	for {
		p.skipSpace()
		if p.pos >= len(p.src) {
			return out, nil
		}
		// 语句之间的收尾符，以及上一条没配对上的落单 '}'：吃掉，否则原地打转。
		if c := p.src[p.pos]; c == ';' || c == ',' || c == '}' {
			p.pos++
			continue
		}
		if err := p.assignStatement(out); err != nil {
			return nil, err
		}
	}
}

// assignStatement 处理一条顶层语句：能读成数据的写进 out，读不成的整段跳过。
func (p *luaParser) assignStatement(out *LuaTable) error {
	if !isLuaIdentStart(rune(p.src[p.pos])) {
		// 不是以名字开头（面板不认的运算符、方括号索引语句之类），跳到行尾。
		p.scanTo(true)
		return nil
	}
	word := p.readWord()
	switch word {
	case "if", "while", "repeat":
		p.skipBlock(1)
		return nil
	case "for":
		// for 自己不开一层，跟它配对的 do 才开；从 0 起算正好。
		p.skipBlock(0)
		return nil
	case "function":
		p.skipBlock(1)
		return nil
	case "local":
		p.skipSpace()
		if p.hasPrefixWord("function") {
			p.pos += len("function")
			p.skipBlock(1)
			return nil
		}
		if p.pos >= len(p.src) || !isLuaIdentStart(rune(p.src[p.pos])) {
			p.scanTo(true)
			return nil
		}
		word = p.readWord()
		p.skipInlineSpace()
		// local a, b = 1, 2：名字和值不是一一对应的关系，整条丢掉，不猜。
		// 这里必须跳完整条语句：同一行里后面的 `b = 1` 长得就像一条新赋值，
		// 停过去会把 b 当成一个字段收进结果。
		if p.pos < len(p.src) && p.src[p.pos] == ',' {
			p.scanRestOfStatement()
			return nil
		}
	case "then", "else", "elseif", "end", "until", "do", "in", "return", "true", "false", "nil", "and", "or", "not":
		// 落单的关键词。成块的 if/for 已经被 skipBlock 整块吃掉了，走到这里
		// 说明是挤在一行里的零碎或者本来就不规整的文件，这个词丢掉就行。
		// 注意 "return"：这里不解析它后面的表，顶层 return 一张表在 modinfo 里没有意义
		// （入口函数已经处理过 return { 这种整份形状）。
		return nil
	}
	p.skipInlineSpace()
	if p.pos >= len(p.src) {
		return nil
	}
	// 只收 `名字 = 值`。a.b = ... / a[1] = ... 是往别的表里塞东西，
	// 猜它落在哪个键上不如不猜。
	if p.src[p.pos] != '=' || (p.pos+1 < len(p.src) && p.src[p.pos+1] == '=') {
		p.scanTo(true)
		return nil
	}
	p.pos++
	// 顶层也接变量引用：`configuration_options = settings` 是模组复用同一张表的标准写法。
	if v, ok := p.tryRefValue(true); ok {
		out.Set(word, v)
		p.recordRef(word, v)
		return nil
	}
	if p.startsNonDataValue() {
		p.skipNonDataValue(true)
		return nil
	}
	val, err := p.parseValue()
	if err != nil {
		return err
	}
	// 值这一行后面还有代码，通常是 `description = "前缀" .. 变量` 这种拼出来的字符串：
	// 面板只解出了左半边，收下来就是一个假名字，整条丢掉。
	// 例外是"下一条语句就挤在同一行"：实测真有作者的 modinfo 整份零换行
	// （1207269058 Simple Health Bar：`name = "..." version = "2.16" description = "..."`），
	// Lua 的语句本来不需要分隔符，这种写法里左半边是完整值，不是半截。
	if p.hasTrailingCode() && !p.atNewStatement(val.Kind == KindNumber) {
		p.scanTo(true)
		return nil
	}
	out.Set(word, val)
	p.recordRef(word, val)
	return nil
}

// atNewStatement 判断值后面那串代码是不是另一条语句的开头：`名字 =`，
// 或者 local / function / if / for / while / return 这类语句关键字。
// 认不出来就返回 false，由调用方按"半截表达式"丢掉——这里猜不得：
// `name = locale == "zh" and "中文名" or "英文名"` 后面紧跟的是 and/or 的分支，
// 挑哪一支上界面都是编一个名字。
//
// requireGap 是"两个 token 之间必须真有空格"：只有数字需要这一道保险，
// 因为数字是读到不能读为止的，紧贴的尾巴可能是被读断的一半（`0x6` 在没有十六进制
// 支持时会被读成 0）。字符串和表自己就是完整边界，实测有作者一个空格都不留：
// `name="Sweet House"description=[[ ... ]]author="羽中"`。
func (p *luaParser) atNewStatement(requireGap bool) bool {
	save := p.pos
	defer func() { p.pos = save }()
	p.skipInlineSpace()
	if requireGap && p.pos == save {
		return false
	}
	if p.pos >= len(p.src) || !isLuaIdentStart(rune(p.src[p.pos])) {
		return false
	}
	switch p.readWord() {
	case "local", "function", "if", "for", "while", "return":
		return true
	}
	p.skipInlineSpace()
	if p.pos >= len(p.src) || p.src[p.pos] != '=' {
		return false
	}
	return p.pos+1 >= len(p.src) || p.src[p.pos+1] != '='
}

// hasTrailingCode 看当前位置到行尾之间还有没有代码（空白、注释、分号不算）。
// 顶层赋值一行一条，所以这里按行判断；表内条目用 atEntryEnd 按分隔符判断。
func (p *luaParser) hasTrailingCode() bool {
	save := p.pos
	defer func() { p.pos = save }()
	p.skipInlineSpace()
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == '\n' || c == '\r' || c == ';' || c == ',' {
			return false
		}
		if c == '-' && p.pos+1 < len(p.src) && p.src[p.pos+1] == '-' {
			return false
		}
		return true
	}
	return false
}

// atEntryEnd 判断表内条目的值是否正好收尾在分隔符或表尾上。
func (p *luaParser) atEntryEnd() bool {
	save := p.pos
	defer func() { p.pos = save }()
	p.skipSpace()
	if p.pos >= len(p.src) {
		return true
	}
	switch p.src[p.pos] {
	case ',', ';', '}':
		return true
	}
	return false
}

// recordRef 记住一个已经读到的字面量值，供后面的同名引用接住。
func (p *luaParser) recordRef(key string, v LuaValue) {
	if p.refs == nil || v.Kind == KindInvalid {
		return
	}
	p.refs[key] = v
}

// tryRefValue 接住"值是一个前面已经读到的名字"这种写法：
//
//	local POLL_CHOICES = { {description = "每 1 秒", data = 1} }
//	settings = { { name = "poll", values = POLL_CHOICES } }
//
// 这不是求值：名字到那张表的关系就写在同一个文件里、由上往下读到了，
// 而 Lua 赋值本身就是把表的引用放进 env（引擎读 env.settings 拿到的也是同一张表）。
// 所以接住它是把手边已有的数据用起来。认不出来的（函数调用、拼接、名字没在前面定义过、
// 名字后面还跟着别的代码）一律返回 false，由调用方按老规矩整条丢掉，不猜。
func (p *luaParser) tryRefValue(lineBreaks bool) (LuaValue, bool) {
	if p.refs == nil {
		return LuaValue{}, false
	}
	save := p.pos
	v, ok := p.refValueAt(lineBreaks)
	if !ok {
		// 一个字节都不能往前走：调用方接下来要按原位判断这是不是"不求值的表达式"，
		// 位置偏了会把下一行当成这一行的值读掉。
		p.pos = save
		return LuaValue{}, false
	}
	return v, true
}

func (p *luaParser) refValueAt(lineBreaks bool) (LuaValue, bool) {
	p.skipSpace()
	if p.pos >= len(p.src) || !isLuaIdentStart(rune(p.src[p.pos])) {
		return LuaValue{}, false
	}
	word := p.readWord()
	end := p.pos
	p.skipInlineSpace()
	if p.pos < len(p.src) {
		switch p.src[p.pos] {
		case '\n', '\r', ';', '}':
		case ',':
			if lineBreaks {
				return LuaValue{}, false
			}
		case '-':
			if p.pos+1 >= len(p.src) || p.src[p.pos+1] != '-' {
				return LuaValue{}, false
			}
		default:
			return LuaValue{}, false
		}
	}
	v, ok := p.refs[word]
	if !ok {
		return LuaValue{}, false
	}
	p.pos = end
	return v, true
}

// startsNonDataValue 判断当前位置上的值是不是"面板不求值的表达式"：
// 函数、函数调用、没在前面定义过的变量引用、nil，或者已经到文件尾。
// 引号/花括号没闭合那种结构性损坏不属于这一类——那种要留给 parseValue 报错，
// 否则一个被截断的文件会伪装成"读成功但字段全空"。
func (p *luaParser) startsNonDataValue() bool {
	p.skipSpace()
	if p.pos >= len(p.src) {
		return true
	}
	c := p.src[p.pos]
	if c == '{' || c == '"' || c == '\'' || p.longStringLevel() >= 0 {
		return false
	}
	if c == '-' || c == '.' || isDigitByte(c) {
		// 数字读不通就不算数据：实测有作者把 priority 写成 `-THISGZSLOVE…`（混淆过的 modinfo）。
		// 那种情况报错会让整份好文件被判死，界面上就是一个"modinfo 读不懂"，
		// 而正确的做法是丢掉这一条、后面的字段照读。
		save := p.pos
		_, err := p.parseNumber()
		p.pos = save
		return err != nil
	}
	if !isLuaIdentStart(rune(c)) {
		return true
	}
	save := p.pos
	word := p.readWord()
	p.pos = save
	return word != "true" && word != "false"
}

// skipNonDataValue 消费一个面板不求值的值。函数值按块关键字配对跳（函数体里
// 到处是逗号和换行，按分隔符跳会跳断），其余按条目边界跳。
func (p *luaParser) skipNonDataValue(lineBreaks bool) {
	p.skipSpace()
	if p.pos < len(p.src) && isLuaIdentStart(rune(p.src[p.pos])) {
		save := p.pos
		if p.readWord() == "function" {
			p.skipBlock(1)
			return
		}
		p.pos = save
	}
	p.scanTo(lineBreaks)
}

// scanTo 消费一段不求值的代码，停在下一个条目边界之后。
// lineBreaks=true 按"一行一条语句"切（顶层用），false 按逗号切（表内用）。
// 括号层数没归零时不算边界：多行调用、嵌套表、参数列表里的逗号都得连着一一起吃掉。
func (p *luaParser) scanTo(lineBreaks bool) { p.scan(lineBreaks, true) }

// scanRestOfStatement 消费整条语句，不在同一行的下一个"名字 ="上停。
// 只给 `local a, b = 1, 2` 这种"名字和值不是一一对应"的写法用：
// 那里第二个名字看着就像一条新语句，停过去会把它当成 `b = 1` 收进结果。
func (p *luaParser) scanRestOfStatement() { p.scan(true, false) }

func (p *luaParser) scan(lineBreaks, stopAtStatement bool) {
	depth := 0
	// gapBefore 记"当前字符前面刚跳过空白"。识别新语句开头必须要这个间隔：
	// `a.b = 1`、`table.insert(...)` 里的词紧跟在点号后面，那不是边界，
	// 把它当边界会把一条语句切一半。
	gapBefore := false
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if depth == 0 {
			if c == '\n' && lineBreaks {
				p.pos++
				return
			}
			if c == ';' || (c == ',' && !lineBreaks) {
				p.pos++
				return
			}
			if c == '}' {
				return
			}
			// 顶层跳一条表达式时，"名字 =" 也是边界：实测有作者的 modinfo 整份没有换行，
			// 只按行跳会把后面所有字段一起吞掉（一行里 description 是拼出来的，
			// 它后面还排着 author、api_version、settings 那些）。
			if lineBreaks && stopAtStatement && gapBefore && isLuaIdentStart(rune(c)) {
				save := p.pos
				if p.statementHead() {
					p.pos = save
					return
				}
				p.pos = save
				p.readWord()
				gapBefore = false
				continue
			}
		}
		switch {
		case c == '"' || c == '\'':
			p.scanQuoted()
			// 引号闭合就是字面量的完整结尾：后面紧跟的 `名字 =` 是下一条语句
			// （实测有 modinfo 一个空格都不留），和空格的作用一样。
			gapBefore = true
		case c == '[' && p.longStringLevel() >= 0:
			p.scanLongString()
			gapBefore = true
		case c == '-' && p.pos+1 < len(p.src) && p.src[p.pos+1] == '-':
			p.scanComment()
			gapBefore = false
		case c == '{' || c == '(' || c == '[':
			depth++
			p.pos++
			gapBefore = false
		case c == '}' || c == ')' || c == ']':
			if depth > 0 {
				depth--
			}
			p.pos++
			gapBefore = false
		default:
			gapBefore = c == ' ' || c == '\t'
			p.pos++
		}
	}
}

// statementHead 判断当前位置是不是一条新语句的开头：语句关键字，或者"一个词紧跟 '='"。
// 前进的量在这里全部回滚，调用方只拿结论——它要么把这里当边界停下，要么整词跳过去。
//
// 词和等号之间必须是空白或注释：`a.b = 1`、`a[1] = 2` 是在往别的东西里塞字段，
// 那个 'a' 不是边界，把它当边界会把这条语句切一半。
func (p *luaParser) statementHead() bool {
	save := p.pos
	word := p.readWord()
	switch word {
	case "local", "function", "if", "for", "while", "repeat", "return":
		p.pos = save
		return true
	}
	p.skipInlineSpace()
	if p.pos+1 < len(p.src) && p.src[p.pos] == '-' && p.src[p.pos+1] == '-' {
		p.scanComment()
		p.skipInlineSpace()
	}
	ok := p.pos < len(p.src) && p.src[p.pos] == '=' &&
		(p.pos+1 >= len(p.src) || p.src[p.pos+1] != '=')
	p.pos = save
	return ok
}

// skipBlock 从块关键字之后前进到与它配对的 end（repeat 配 until）。
// 只按 Lua 的块语法数层数：function / if / do / repeat 各开一层，end / until 各收一层，
// then、else、elseif 不改层数；for 和 while 自己不开层，跟它们配对的 do 才开层。
// 这不是执行代码，只是知道该跳到哪儿——不跳的话
// `if locale == "zh" then name = "中文名" else name = "English" end`
// 会把两个分支都当成顶层赋值收进来，界面上就显示一个凭运气选出来的名字。
func (p *luaParser) skipBlock(depth int) {
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		switch {
		case c == '"' || c == '\'':
			p.scanQuoted()
		case c == '[' && p.longStringLevel() >= 0:
			p.scanLongString()
		case c == '-' && p.pos+1 < len(p.src) && p.src[p.pos+1] == '-':
			p.scanComment()
		case isLuaIdentStart(rune(c)):
			switch p.readWord() {
			case "function", "if", "do", "repeat":
				depth++
			case "end", "until":
				depth--
				if depth <= 0 {
					return
				}
			}
		default:
			p.pos++
		}
	}
}

func (p *luaParser) readWord() string {
	start := p.pos
	for p.pos < len(p.src) && isLuaIdentRune(rune(p.src[p.pos])) {
		p.pos++
	}
	return p.src[start:p.pos]
}

// longStringLevel 若当前位置是长字符串（或长注释）的开头 [[ / [=[ / [==[ ，
// 返回中间 '=' 的个数（可以为 0）；不是则返回 -1。Lua 5.1 就认这个层级写法。
func (p *luaParser) longStringLevel() int {
	if p.pos >= len(p.src) || p.src[p.pos] != '[' {
		return -1
	}
	i := p.pos + 1
	eq := 0
	for i < len(p.src) && p.src[i] == '=' {
		eq++
		i++
	}
	if i >= len(p.src) || p.src[i] != '[' {
		return -1
	}
	return eq
}

func longStringCloser(level int) string {
	return "]" + strings.Repeat("=", level) + "]"
}

// parseLongString 取出长字符串的内容。长字符串内部不做转义（Lua 5.1 规则）；
// 紧跟开括号的第一个换行不计入内容——大家都写 description = [[\n正文\n]]，
// 少裁这一步界面上就多出一个空行。
func (p *luaParser) parseLongString() (string, error) {
	level := p.longStringLevel()
	if level < 0 {
		return "", fmt.Errorf("第 %d 字节处不是长字符串开头，附近: %s", p.pos, p.context())
	}
	closer := longStringCloser(level)
	p.pos += 2 + level
	idx := strings.Index(p.src[p.pos:], closer)
	if idx < 0 {
		return "", fmt.Errorf("第 %d 字节处的 [[ 没有配对的 %s", p.pos, closer)
	}
	body := p.src[p.pos : p.pos+idx]
	p.pos += idx + len(closer)
	body = strings.TrimPrefix(body, "\r\n")
	if strings.HasPrefix(body, "\n") || strings.HasPrefix(body, "\r") {
		body = body[1:]
	}
	return body, nil
}

// 下面三个 scan* 只用于"这段我们不收，跳过去"：碰到没闭合的结构就一路跳到文件尾，
// 由外层按整体形状继续判断，不在这里报错。

func (p *luaParser) scanQuoted() {
	q := p.src[p.pos]
	p.pos++
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == '\\' {
			p.pos += 2
			continue
		}
		p.pos++
		if c == q {
			return
		}
	}
}

func (p *luaParser) scanLongString() {
	level := p.longStringLevel()
	if level < 0 {
		p.pos++
		return
	}
	closer := longStringCloser(level)
	p.pos += 2 + level
	idx := strings.Index(p.src[p.pos:], closer)
	if idx < 0 {
		p.pos = len(p.src)
		return
	}
	p.pos += idx + len(closer)
}

func (p *luaParser) scanComment() {
	p.pos += 2
	if p.longStringLevel() >= 0 {
		p.scanLongString()
		return
	}
	for p.pos < len(p.src) && p.src[p.pos] != '\n' {
		p.pos++
	}
}

// ---------- 序列化 ----------

// Render 输出 Klei 生成风格：制表符缩进、键值各一行。
func Render(t *LuaTable) string {
	var b strings.Builder
	b.WriteString("return ")
	writeTable(&b, t, 0)
	b.WriteString("\n")
	return b.String()
}

func writeTable(b *strings.Builder, t *LuaTable, depth int) {
	if t.Len() == 0 {
		b.WriteString("{  }")
		return
	}
	nl := "\n" + strings.Repeat("\t", depth+1)
	b.WriteString("{")
	for i, e := range t.entries {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(nl)
		if e.key != nil {
			b.WriteString(formatKey(*e.key))
			b.WriteString(" = ")
		}
		writeValue(b, e.val, depth+1)
	}
	b.WriteString(",")
	b.WriteString("\n" + strings.Repeat("\t", depth))
	b.WriteString("}")
}

func writeValue(b *strings.Builder, v LuaValue, depth int) {
	switch v.Kind {
	case KindString:
		b.WriteString(LuaStringLiteral(v.S))
	case KindNumber:
		b.WriteString(formatNumber(v.N))
	case KindBool:
		if v.B {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case KindTable:
		if v.T == nil {
			b.WriteString("{  }")
			return
		}
		writeTable(b, v.T, depth)
	case KindInvalid:
		// 解析不会产出它，只有取不到键的零值会走到这里。写 nil 而不是悄悄留空，
		// 否则会渲染出 `{a = }` 这种语法错误的文件。
		b.WriteString("nil")
	}
}

// formatKey 只对非合法标识符加方括号引号，workshop-xxx 这类键必须走方括号形式。
func formatKey(k string) string {
	if k != "" {
		ok := true
		for i, r := range k {
			if i == 0 {
				if !isLuaIdentStart(r) {
					ok = false
					break
				}
				continue
			}
			if !isLuaIdentRune(r) {
				ok = false
				break
			}
		}
		if ok {
			return k
		}
	}
	return `["` + luaStringBody(k) + `"]`
}

// LuaStringLiteral 把 Go 字符串写成 Lua 5.1 字面量（含首尾双引号）。
//
// 不能图省事用 strconv.Quote：它对不可见字节产出 \xNN、\uXXXX，而 DST 内嵌的是 Lua 5.1，
// 只认 \a \b \f \n \r \t \v \" \\ 和 \ddd（十进制字节值）。转义写错，服务端加载
// modoverrides.lua 会报语法错误并整份放弃这个模组的配置，而这种文件是面板生成的，
// 排查成本全落在用户身上。非 ASCII 字节（中文模组名等）原样透传——Lua 按字节处理字符串，UTF-8 安全。
func LuaStringLiteral(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\':
			b.WriteString(`\\`)
		case c == '"':
			b.WriteString(`\"`)
		case c == '\a':
			b.WriteString(`\a`)
		case c == '\b':
			b.WriteString(`\b`)
		case c == '\f':
			b.WriteString(`\f`)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\r':
			b.WriteString(`\r`)
		case c == '\t':
			b.WriteString(`\t`)
		case c == '\v':
			b.WriteString(`\v`)
		case c < 0x20 || c == 0x7f:
			fmt.Fprintf(&b, `\%03d`, c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// luaStringBody 取字面量去掉首尾引号的部分，供方括号键这种自带引号的场景用。
func luaStringBody(s string) string {
	lit := LuaStringLiteral(s)
	return lit[1 : len(lit)-1]
}

func formatNumber(f float64) string {
	if math.Trunc(f) == f && math.Abs(f) < 1e15 {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}
