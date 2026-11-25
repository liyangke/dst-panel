package monitor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"dstpanel/internal/dst"
)

// Settings 是监控告警、自愈与停服等待的可调参数。
//
// 为什么单独一份 JSON 而不是塞进 config.Config：这一轮要改的东西必须能在面板
// 运行时改且立刻生效（自愈开关、阈值、Webhook），而 config 只在启动时读一次 yaml；
// 本轮的文件边界也不允许动 config/config.go。落在 workDir 下和数据库、备份同一棵树，
// 重启后面板自己把它读回来，不需要用户再去改配置文件。
type Settings struct {
	// AutoRecover 是「随面板恢复 / 崩溃自动拉起」总开关，默认关闭。
	// 关闭时面板绝不自动重启任何分片，只产生告警。
	AutoRecover bool `json:"autoRecover"`
	// ShutdownWaitSeconds 是停服后确认进程真正退出的等待上限（问题 3）。
	ShutdownWaitSeconds int `json:"shutdownWaitSeconds"`
	// 三个百分比阈值，0 表示关闭这一项告警。默认 90。
	CPUThreshold  float64 `json:"cpuThreshold"`
	MemThreshold  float64 `json:"memThreshold"`
	DiskThreshold float64 `json:"diskThreshold"`
	// HelperSilenceSeconds 是在跑的分片多久没有新的助手快照算「长时间无快照」。
	HelperSilenceSeconds int `json:"helperSilenceSeconds"`
	// WebhookURL 为空则不发通知；非空时告警会 POST 一份 JSON 过去。
	WebhookURL string `json:"webhookUrl"`
	// WebhookTimeoutSeconds 是单次 Webhook 的超时，失败只记日志、不影响采样。
	WebhookTimeoutSeconds int `json:"webhookTimeoutSeconds"`
	// AlertCooldownMinutes 是同一类告警的冷却时间，避免每 60 秒刷一条。
	AlertCooldownMinutes int `json:"alertCooldownMinutes"`
	// MaxSamples 是 samples.jsonl 最多保留几条，防止无限增长。
	MaxSamples int `json:"maxSamples"`
}

const (
	optionsFile = "monitor_options.json"
)

func defaultSettings() Settings {
	return Settings{
		AutoRecover:           false,
		ShutdownWaitSeconds:   30,
		CPUThreshold:          90,
		MemThreshold:          90,
		DiskThreshold:         90,
		HelperSilenceSeconds:  300,
		WebhookTimeoutSeconds: 5,
		AlertCooldownMinutes:  10,
		MaxSamples:            720, // 60 秒一条 = 12 小时
	}
}

// settingsKeys 是 PUT 接口认得的键。拒绝未知键和设置表一个道理：
// 拼错的键安静躺在文件里，用户会以为自己改成功了。
var settingsKeys = map[string]bool{
	"autoRecover": true, "shutdownWaitSeconds": true,
	"cpuThreshold": true, "memThreshold": true, "diskThreshold": true,
	"helperSilenceSeconds": true, "webhookUrl": true,
	"webhookTimeoutSeconds": true, "alertCooldownMinutes": true,
	"maxSamples": true,
}

// normalizeSettings 把手抖填歪的数兜回安全值，阈值里的 0 是「关掉这一项」的正经选项，
// 不动它。parse 旧文件时缺的字段是零值，也会被这里补成默认。
func normalizeSettings(in Settings) Settings {
	d := defaultSettings()
	if in.ShutdownWaitSeconds <= 0 || in.ShutdownWaitSeconds > 600 {
		in.ShutdownWaitSeconds = d.ShutdownWaitSeconds
	}
	for _, p := range []*float64{&in.CPUThreshold, &in.MemThreshold, &in.DiskThreshold} {
		if *p < 0 || *p > 100 {
			*p = 90
		}
	}
	if in.HelperSilenceSeconds <= 0 {
		in.HelperSilenceSeconds = d.HelperSilenceSeconds
	}
	if in.WebhookTimeoutSeconds <= 0 {
		in.WebhookTimeoutSeconds = d.WebhookTimeoutSeconds
	}
	if in.AlertCooldownMinutes <= 0 {
		in.AlertCooldownMinutes = d.AlertCooldownMinutes
	}
	if in.MaxSamples <= 0 || in.MaxSamples > 10000 {
		in.MaxSamples = d.MaxSamples
	}
	return in
}

// Options 是带锁的配置持有者。所有读都走 Get() 拿一份拷贝，写走 Update()，
// 正在跑的采样循环和 HTTP 接口因此不会读到改到一半的结构。
type Options struct {
	mu   sync.RWMutex
	path string
	cfg  Settings
}

// LoadOptions 从 workDir 读配置；文件不存在或坏了都用默认值，不拦住面板启动。
func LoadOptions(dir string) *Options {
	o := &Options{path: filepath.Join(dir, optionsFile), cfg: defaultSettings()}
	raw, err := os.ReadFile(o.path)
	if err == nil {
		var s Settings
		if json.Unmarshal(raw, &s) == nil {
			o.cfg = normalizeSettings(s)
		}
	}
	return o
}

func (o *Options) Get() Settings {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.cfg
}

// ShutdownWait 给生命周期层用：停服等待进程退出的上限。
func (o *Options) ShutdownWait() time.Duration {
	return time.Duration(o.Get().ShutdownWaitSeconds) * time.Second
}

func (o *Options) Cooldown() time.Duration {
	return time.Duration(o.Get().AlertCooldownMinutes) * time.Minute
}

func (o *Options) SampleLimit() int { return o.Get().MaxSamples }

// Update 按补丁方式改配置：只覆盖请求里出现的键，其余保持原值。
// 先校验键名、合并、归一化，再整份原子写回磁盘。
func (o *Options) Update(raw []byte) error {
	var patch map[string]json.RawMessage
	if err := json.Unmarshal(raw, &patch); err != nil {
		return fmt.Errorf("请求体不是合法 JSON 对象: %w", err)
	}
	for k := range patch {
		if !settingsKeys[k] {
			return fmt.Errorf("不认识的配置项 %q", k)
		}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	base, err := json.Marshal(o.cfg)
	if err != nil {
		return err
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(base, &merged); err != nil {
		return err
	}
	for k, v := range patch {
		merged[k] = v
	}
	body, err := json.Marshal(merged)
	if err != nil {
		return err
	}
	var next Settings
	if err := json.Unmarshal(body, &next); err != nil {
		return fmt.Errorf("配置项取值不合法: %w", err)
	}
	next = normalizeSettings(next)
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	if err := dst.WriteFileAtomicNoBackup(o.path, data, 0o600); err != nil {
		return fmt.Errorf("保存监控配置: %w", err)
	}
	o.cfg = next
	return nil
}
