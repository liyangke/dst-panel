package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"dstpanel/internal/config"
)

func TestGenConfigRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "panel.yaml")
	if err := writeSampleConfig(path, io.Discard); err != nil {
		t.Fatalf("第一次生成: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回示例配置: %v", err)
	}
	if !strings.Contains(string(raw), "adminPassword") || !strings.Contains(string(raw), "kleiRoot") {
		t.Errorf("示例配置缺关键项：%s", raw)
	}
	// kleiRoot 那一段必须自己解释"-savedir 被忽略"：这是抄样例配置的人最容易跳过的一行，
	// 而跳过的代价是开服后引擎用的是另一个世界，面板却一路报成功。
	if !strings.Contains(string(raw), "-savedir") {
		t.Errorf("示例配置没解释 kleiRoot 为什么不能随便指：%s", raw)
	}
	// 已有文件必须拒绝，而不是"顺手备份一下再覆盖"。
	err = writeSampleConfig(path, io.Discard)
	if err == nil {
		t.Fatal("重复生成没报错，用户的配置会被静默换掉")
	}
	if !strings.Contains(err.Error(), "已经存在") {
		t.Errorf("提示是 %v，期望说明没覆盖的原因", err)
	}
	// 拒绝时原文件内容不能被动过。
	again, _ := os.ReadFile(path)
	if string(again) != string(raw) {
		t.Error("拒绝覆盖的同时还是改了文件")
	}
}

// TestSampleConfigRoundTrips 钉的是"示例配置和结构体一起长大"：
// 样例文件是写死的文本，字段名一改它就悄悄失效，用户照着填会得到默认值。
func TestSampleConfigRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "panel.yaml")
	if err := writeSampleConfig(path, io.Discard); err != nil {
		t.Fatalf("生成示例配置: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("示例配置读不回来: %v", err)
	}
	if cfg.Server.Port != 8921 || cfg.Server.Bind != "127.0.0.1" {
		t.Errorf("server 段没读对：%+v", cfg.Server)
	}
	if cfg.Paths.KleiRoot == "" || cfg.Paths.DstInstall == "" || cfg.Paths.WorkDir == "" {
		t.Errorf("paths 段没读对：%+v", cfg.Paths)
	}
	// paths 段必须由 config.Default() 渲染，不能再抄一份字面量：
	// 抄的那份会和部署脚本、实盘目录各走各的，症状是"面板写的配置引擎不认"。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回示例配置: %v", err)
	}
	if strings.Contains(string(raw), "{{") {
		t.Errorf("示例配置里有没替换掉的占位符")
	}
	def := config.Default().Paths
	if cfg.Paths != def {
		t.Errorf("示例配置的 paths 和默认值不一致：样例 %+v，默认 %+v", cfg.Paths, def)
	}
	if cfg.Helper.PollMs != 1000 || cfg.Helper.StaleSeconds != 15 {
		t.Errorf("helper 段没读对：%+v", cfg.Helper)
	}
	if cfg.Network.ShardPortBase != 10998 || cfg.Backup.Keep != 20 {
		t.Errorf("network/backup 没读对：%+v %+v", cfg.Network, cfg.Backup)
	}
}

// TestKleiRootWarning 钉的是实测出来的引擎行为：-savedir 被忽略，存档根目录只认
// 运行用户自己那一份。指错时接口层全都返回成功，所以这句提示是唯一的露出点。
func TestKleiRootWarning(t *testing.T) {
	for _, p := range []string{
		"/home/ubuntu/.klei/DoNotStarveTogether",
		"/home/ubuntu/.klei/DoNotStarveTogether/",
		`C:\Users\ubuntu\Documents\Klei\DoNotStarveTogether`,
		"", // 没配：config 的默认值已经填成引擎那一份，不该喊
	} {
		if w := kleiRootWarning(p); w != "" {
			t.Errorf("%q 是引擎会读的目录，却提示了：%s", p, w)
		}
	}
	for _, p := range []string{
		"/home/ubuntu/dst/klei",
		"/home/ubuntu/.klei",
		"/home/ubuntu/DoNotStarveTogether",
	} {
		w := kleiRootWarning(p)
		if w == "" {
			t.Errorf("%q 指错了目录却没提示", p)
			continue
		}
		// 提示要说清"为什么"和"后果"，光说路径不对用户不知道下一步改哪。
		if !strings.Contains(w, "-savedir") || !strings.Contains(w, "DoNotStarveTogether") {
			t.Errorf("%q 的提示没落到能执行的地步：%s", p, w)
		}
	}
}

func TestRunRejectsBadFlags(t *testing.T) {
	if err := run([]string{"-config", filepath.Join(t.TempDir(), "没这个文件", "bogus.yaml"), "-zzz"}, io.Discard); err == nil {
		t.Error("未知参数竟然通过了")
	}
	// -config 指向不存在的文件是"用默认值"以外的错误吗？config.Load 对缺文件是容忍的，
	// 但显式指了一个带语法错误的文件必须报错。
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(bad, []byte("server: [这行不是合法映射\n"), 0o600); err != nil {
		t.Fatalf("写坏配置: %v", err)
	}
	err := run([]string{"-config", bad}, io.Discard)
	if err == nil {
		t.Fatal("坏配置没报错")
	}
	if !strings.Contains(err.Error(), bad) {
		t.Errorf("报错没指出是哪个文件：%v", err)
	}
}

func TestVersionFlagPrints(t *testing.T) {
	var out strings.Builder
	if err := run([]string{"-version"}, &out); err != nil {
		t.Fatalf("-version: %v", err)
	}
	if !strings.Contains(out.String(), version) {
		t.Errorf("-version 输出 = %q", out.String())
	}
}

// TestBindErrorTranslations 直接喂内核错误码，跨平台也能测到翻译逻辑。
func TestBindErrorTranslations(t *testing.T) {
	cases := []struct {
		in   error
		want string
	}{
		{fmt.Errorf("bind: %w", syscall.EADDRINUSE), "已被占用"},
		{fmt.Errorf("bind: %w", syscall.EACCES), "1024 以下"},
		{errors.New("bind: 别的故障"), "监听 127.0.0.1:8921 失败"},
	}
	for _, c := range cases {
		err := bindError(c.in, "127.0.0.1:8921")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("bindError(%v) = %v，期望含 %q", c.in, err, c.want)
		}
	}
	if got := portOf("127.0.0.1:8921"); got != "8921" {
		t.Errorf("portOf = %q", got)
	}
	if got := portOf("没有端口"); got != "没有端口" {
		t.Errorf("portOf 对畸形地址 = %q，期望原样返回", got)
	}
}

// TestRunReportsOccupiedPort 走真实路径：先占住端口，再让面板启动，
// 断言它给的是"端口被占"这条可操作提示，而不是一句 syscall 报错。
func TestRunReportsOccupiedPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占端口: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "panel.yaml")
	body := fmt.Sprintf(`server:
  bind: 127.0.0.1
  port: %d
  adminUser: admin
  adminPassword: "x"
paths:
  kleiRoot: %q
  dstInstall: %q
  workDir: %q
`, port, filepath.Join(dir, "klei"), filepath.Join(dir, "dst"), filepath.Join(dir, "work"))
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("写配置: %v", err)
	}

	err = run([]string{"-config", cfgPath}, io.Discard)
	if err == nil {
		t.Fatal("端口被占还启动成功")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("%d", port)) {
		t.Errorf("报错没带上地址：%v", err)
	}
	// 日志文件、库文件都得已经建好——启动失败也要留下现场。
	if _, err := os.Stat(filepath.Join(dir, "work", "panel.log")); err != nil {
		t.Errorf("失败路径上没留日志文件：%v", err)
	}
}

func TestCheckBindRefusesPublicWithoutPassword(t *testing.T) {
	cfg := config.Default()
	cfg.Server.AdminPassword = ""
	cfg.Server.Bind = "0.0.0.0"
	if err := checkBind(cfg); err == nil {
		t.Error("公网监听 + 空密码竟然允许启动")
	} else if !strings.Contains(err.Error(), "adminPassword") {
		t.Errorf("提示没指出去哪一项：%v", err)
	}
	// 回环 + 空密码是开发者本机场景，放行，但接口层会 503。
	cfg.Server.Bind = "127.0.0.1"
	if err := checkBind(cfg); err != nil {
		t.Errorf("回环 + 空密码被拒：%v", err)
	}
	cfg.Server.AdminPassword = "有密码"
	cfg.Server.Bind = "0.0.0.0"
	if err := checkBind(cfg); err != nil {
		t.Errorf("公网 + 有密码被拒：%v", err)
	}
	// ::1 也算回环。
	cfg.Server.AdminPassword = ""
	cfg.Server.Bind = "::1"
	if err := checkBind(cfg); err != nil {
		t.Errorf("IPv6 回环被拒：%v", err)
	}
}
