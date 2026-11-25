// 一次性探针：拿面板自己的 Lua 解析器读本机所有已订阅模组的 modinfo.lua，
// 把读不出来的字段按"作者写法"归类，看解析器该补哪一类。用完即删，不进仓库。
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"dstpanel/internal/dst"
)

var (
	callRe   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*\s*\(`)
	andRe    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*\s+and\b`)
	ifRe     = regexp.MustCompile(`^if\b`)
	multiRe  = regexp.MustCompile(`["\]]\s+[A-Za-z_][A-Za-z0-9_]*\s*=`)
	concatRe = regexp.MustCompile(`\.\.`)
)

func main() {
	root := os.Args[1]
	names := []string{"name", "version", "description", "author", "api_version"}
	dirs := []string{}
	entries, err := os.ReadDir(root)
	if err != nil {
		fmt.Println("READDIR", err)
		return
	}
	for _, e := range entries {
		if _, err := os.Stat(filepath.Join(root, e.Name(), "modinfo.lua")); err == nil {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Strings(dirs)
	fmt.Println("TOTAL", len(dirs))
	stats := map[string]int{}
	for _, d := range dirs {
		raw, err := os.ReadFile(filepath.Join(root, d, "modinfo.lua"))
		if err != nil {
			fmt.Printf("ERR-READ\t%s\t%v\n", d, err)
			continue
		}
		text := string(raw)
		tbl, err := dst.ParseLuaAssignments(text)
		if err != nil {
			fmt.Printf("PARSE-FAIL\t%s\t%s\n", d, err.Error())
			continue
		}
		for _, n := range names {
			v := tbl.Get(n)
			if v.Kind != dst.KindInvalid && !(v.Kind == dst.KindString && v.S == "") {
				continue
			}
			expr := valueExpr(text, n)
			shape := classify(expr)
			stats[shape]++
			short := string([]rune(expr))
			if len([]rune(short)) > 70 {
				short = string([]rune(short)[:70])
			}
			fmt.Printf("EVIDENCE\t%s\t%s\t%s\t%s\n", d, n, shape, short)
		}
	}
	keys := make([]string, 0, len(stats))
	for k := range stats {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return stats[keys[i]] > stats[keys[j]] })
	fmt.Println("--- shapes ---")
	for _, k := range keys {
		fmt.Printf("%-28s %d\n", k, stats[k])
	}
}

// valueExpr 取 "<key> =" 之后到这一句结束的那段原文（按 \r / \n 两种换行都算结束）。
func valueExpr(text, key string) string {
	re := regexp.MustCompile(`(?m)^[ \t]*` + key + `[ \t]*=[ \t]*`)
	loc := re.FindStringIndex(text)
	if loc == nil {
		return ""
	}
	rest := text[loc[1]:]
	end := len(rest)
	if i := strings.IndexAny(rest, "\r\n"); i >= 0 && i < end {
		end = i
	}
	// 有些作者把等号留在行尾、值写到下一行，所以行内取空时往后多取一段。
	expr := strings.TrimSpace(rest[:end])
	if expr == "" {
	 win := rest
		if len(win) > 120 {
			win = win[:120]
		}
		expr = strings.Join(strings.Fields(win), " ")
		if m := multiRe.FindStringIndex(expr); m != nil && m[0] > 0 {
			expr = strings.TrimSpace(expr[:m[0]])
		}
	}
	return expr
}

func classify(expr string) string {
	switch {
	case expr == "":
		return "ABSENT"
	case ifRe.MatchString(expr):
		return "IF-EXPR"
	case callRe.MatchString(expr):
		return "CALL(...)"
	case andRe.MatchString(expr):
		if concatRe.MatchString(expr) {
			return "X and .. or ..(拼接)"
		}
		return "X and A or B"
	case multiRe.MatchString(expr):
		return "同行还有下一条赋值"
	case concatRe.MatchString(expr):
		return "字符串拼接 .."
	case strings.HasPrefix(expr, "[["):
		return "长字符串(应能读)"
	case strings.HasPrefix(expr, `"`) || strings.HasPrefix(expr, "'"):
		return "引号串(应能读)"
	}
	return "OTHER"
}
