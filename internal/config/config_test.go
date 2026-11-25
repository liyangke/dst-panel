package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cfg.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("写配置: %v", err)
	}
	return path
}

// 老配置文件里没有 tasks 这一段是常态（发版前装的机器都没有），
// 这种情况必须落到面板默认的判停窗口上，而不是 0 分钟——0 分钟等于一启动就判死。
func TestLoadKeepsTaskDefaultsWhenSectionMissing(t *testing.T) {
	cfg, err := Load(writeCfg(t, "server:\n  port: 8921\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Tasks.StallMinutes != 5 {
		t.Errorf("stallMinutes = %d，缺省该是 5", cfg.Tasks.StallMinutes)
	}
	if cfg.Tasks.TimeoutMinutes != 0 {
		t.Errorf("timeoutMinutes = %d，缺省该是 0（= 按任务类型取默认）", cfg.Tasks.TimeoutMinutes)
	}
}

func TestLoadClampsNonsenseTaskValues(t *testing.T) {
	cfg, err := Load(writeCfg(t, "tasks:\n  timeoutMinutes: -30\n  stallMinutes: -1\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Tasks.TimeoutMinutes != 0 {
		t.Errorf("timeoutMinutes = %d，负数该写成 0（按类型取默认）", cfg.Tasks.TimeoutMinutes)
	}
	if cfg.Tasks.StallMinutes != 5 {
		t.Errorf("stallMinutes = %d，负数该兜回默认 5", cfg.Tasks.StallMinutes)
	}

	// 填正数就按填的走，这是这一项存在的唯一理由：换台机器带宽就不一样。
	cfg2, err := Load(writeCfg(t, "tasks:\n  timeoutMinutes: 90\n  stallMinutes: 20\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg2.Tasks.TimeoutMinutes != 90 || cfg2.Tasks.StallMinutes != 20 {
		t.Errorf("填的数字被改写了：%+v", cfg2.Tasks)
	}
}

// 公网列表这一段是后加的：发版前装的机器、以及 deploy/start.sh 生成的旧配置里都没有它，
// 缺段必须落到"开、60 秒、20 秒"这组默认上，否则新页面在老机器上永远显示"没打开"。
func TestLoadLobbyDefaultsAndClamps(t *testing.T) {
	cfg, err := Load(writeCfg(t, "server:\n  port: 8921\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Lobby.Enabled || cfg.Lobby.RefreshSeconds != 60 || cfg.Lobby.TimeoutSeconds != 20 {
		t.Errorf("缺 lobby 段时该用默认值：%+v", cfg.Lobby)
	}

	// 关掉是一句正经话，不能被兜回"开"：这一项唯一的语义就是不要碰那个 CDN。
	cfg2, err := Load(writeCfg(t, "lobby:\n  enabled: false\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg2.Lobby.Enabled {
		t.Errorf("写了 enabled: false 却被当成开：%+v", cfg2.Lobby)
	}

	// 0 秒等于每次轮询都重拉十几 MB，9999 秒等于一个卡住的出口把请求挂住两小时多。
	cfg3, err := Load(writeCfg(t, "lobby:\n  refreshSeconds: 0\n  timeoutSeconds: 9999\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg3.Lobby.RefreshSeconds != 60 || cfg3.Lobby.TimeoutSeconds != 120 {
		t.Errorf("非法值没被钳住：%+v", cfg3.Lobby)
	}

	// 落在合理区间内就按用户写的走，这一档存在的意义就是换台机器带宽不一样。
	cfg4, err := Load(writeCfg(t, "lobby:\n  refreshSeconds: 300\n  timeoutSeconds: 45\n  proxy: http://127.0.0.1:7890\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg4.Lobby.RefreshSeconds != 300 || cfg4.Lobby.TimeoutSeconds != 45 || cfg4.Lobby.Proxy != "http://127.0.0.1:7890" {
		t.Errorf("填的值被改写了：%+v", cfg4.Lobby)
	}
}
