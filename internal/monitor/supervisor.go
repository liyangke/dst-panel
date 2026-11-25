package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"dstpanel/internal/dst"
	"dstpanel/internal/hoststats"
	"dstpanel/internal/procrun"
	"dstpanel/internal/store"
)

const (
	samplesFile  = "monitor_samples.jsonl"
	expectedFile = "monitor_expected.json"
)

// Sample 是一轮主机采样。它是新增的监控数据，和 store 里那份「在线人数趋势」不是一回事：
// 后者按分片存，这份按整机存（CPU/内存/磁盘/助手新鲜度），用 JSONL 落盘。
type Sample struct {
	At                   string  `json:"at"`
	CPUPercent           float64 `json:"cpuPercent"`
	CPUReady             bool    `json:"cpuReady"`
	MemPercent           float64 `json:"memPercent"`
	DiskPercent          float64 `json:"diskPercent"`
	HelperSilenceSeconds int     `json:"helperSilenceSeconds"`
	ShardsRunning        int     `json:"shardsRunning"`
}

// Alert 是一条已经去过重、写进操作记录表并（可选）推出去的告警。
type Alert struct {
	At      string `json:"at"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// View 是新增监控接口一次性给出的全部数据。三个列表都保证非 nil（界面会直接读 .length）。
type View struct {
	Config    Settings `json:"config"`
	Samples   []Sample `json:"samples"`
	Alerts    []Alert  `json:"alerts"`
	Expected  []string `json:"expected"`
	Recovered int      `json:"recovered"`
}

// Supervisor 是采样持久化、阈值告警、Webhook 与崩溃自愈共用的那份状态。
//
// 由 main.go 建一个实例，同时交给 Sampler（每轮采样/心跳）和 api.Server（读配置、改配置、
// 记录哪些分片是面板自己启动的）。用指针共享而不是各存一份，否则「开关关没关」会在两处漂移。
type Supervisor struct {
	mu      sync.Mutex
	opts    *Options
	store   *store.Store
	paths   dst.Paths
	procs   *procrun.Driver
	logger  *slog.Logger
	dataDir string

	samples     []Sample
	alerts      []Alert
	lastAlert   map[string]time.Time // 告警去重/冷却，key 是 kind
	expected    map[string]bool      // 面板启动过、期望在跑的分片
	prevRunning map[string]bool      // 上一次心跳看到的运行中分片
	recovered   int                  // 本次面板启动自动拉起了几个分片
}

// NewSupervisor 从磁盘加载最近 N 条采样和「期望运行」名单，避免面板重启后数据断档。
func NewSupervisor(dataDir string, paths dst.Paths, st *store.Store, procs *procrun.Driver,
	opts *Options, logger *slog.Logger) *Supervisor {
	if opts == nil {
		opts = LoadOptions(dataDir)
	}
	s := &Supervisor{
		opts: opts, store: st, paths: paths, procs: procs, logger: logger, dataDir: dataDir,
		samples: []Sample{}, alerts: []Alert{},
		lastAlert: map[string]time.Time{},
		expected:  map[string]bool{}, prevRunning: map[string]bool{},
	}
	s.samples = s.loadSamples(opts.SampleLimit())
	s.expected = s.loadExpected()
	return s
}

func (s *Supervisor) log() *slog.Logger {
	if s.logger != nil {
		return s.logger
	}
	return slog.Default()
}

func (s *Supervisor) Options() *Options { return s.opts }

// View 返回一份拷贝给 HTTP 层，列表字段一律非 nil。
func (s *Supervisor) View() View {
	s.mu.Lock()
	samples := append([]Sample(nil), s.samples...)
	alerts := append([]Alert(nil), s.alerts...)
	keys := make([]string, 0, len(s.expected))
	for k := range s.expected {
		keys = append(keys, k)
	}
	recovered := s.recovered
	s.mu.Unlock()
	sort.Strings(keys)
	if samples == nil {
		samples = []Sample{}
	}
	if alerts == nil {
		alerts = []Alert{}
	}
	if keys == nil {
		keys = []string{}
	}
	return View{Config: s.opts.Get(), Samples: samples, Alerts: alerts,
		Expected: keys, Recovered: recovered}
}

// MarkExpected 由生命周期接口在启停成功后调用：只有面板自己起过的分片才纳入
// 「原本应该在跑」的名单，正常停服会把它摘掉，下一轮心跳就不会误报崩溃。
func (s *Supervisor) MarkExpected(cluster, shard string, on bool) {
	key := shardKey(cluster, shard)
	s.mu.Lock()
	if on {
		s.expected[key] = true
	} else {
		delete(s.expected, key)
		delete(s.prevRunning, key)
	}
	snap := make(map[string]bool, len(s.expected))
	for k, v := range s.expected {
		snap[k] = v
	}
	s.mu.Unlock()
	if err := s.writeExpected(snap); err != nil {
		s.log().Warn("保存期望运行名单失败", "err", err.Error())
	}
}

// Collect 由 Sampler 每个采样节拍调用一次：落一条主机采样、判阈值、发告警。
// 所有失败（读进程、写盘、Webhook）都只记日志，绝不让采样循环中断。
func (s *Supervisor) Collect(now time.Time, hs hoststats.Stats) {
	cfg := s.opts.Get()
	running, err := s.currentRunning()
	if err != nil {
		s.log().Warn("监控采样枚举运行中分片失败", "err", err.Error())
		running = map[string]bool{}
	}
	silence, silenceKey := s.helperSilence(now, running)
	sample := Sample{
		At:         now.UTC().Format(time.RFC3339),
		CPUPercent: hs.CPUPercent, CPUReady: hs.CPUReady,
		MemPercent: hs.MemPercent, DiskPercent: hs.DiskPercent,
		HelperSilenceSeconds: int(silence.Seconds()),
		ShardsRunning:        len(running),
	}

	s.mu.Lock()
	s.samples = append(s.samples, sample)
	if len(s.samples) > cfg.MaxSamples {
		s.samples = s.samples[len(s.samples)-cfg.MaxSamples:]
	}
	snap := append([]Sample(nil), s.samples...)
	s.mu.Unlock()
	if err := s.writeSamples(snap); err != nil {
		s.log().Warn("监控采样落盘失败", "err", err.Error())
	}

	if hs.CPUReady && cfg.CPUThreshold > 0 && hs.CPUPercent >= cfg.CPUThreshold {
		s.emit(now, "cpu", fmt.Sprintf("CPU 使用率 %.1f%% 达到阈值 %.1f%%", hs.CPUPercent, cfg.CPUThreshold))
	}
	if cfg.MemThreshold > 0 && hs.MemPercent >= cfg.MemThreshold {
		s.emit(now, "mem", fmt.Sprintf("内存使用率 %.1f%% 达到阈值 %.1f%%", hs.MemPercent, cfg.MemThreshold))
	}
	if cfg.DiskThreshold > 0 && hs.DiskPercent >= cfg.DiskThreshold {
		s.emit(now, "disk", fmt.Sprintf("存档分区磁盘使用率 %.1f%% 达到阈值 %.1f%%", hs.DiskPercent, cfg.DiskThreshold))
	}
	if len(running) > 0 && cfg.HelperSilenceSeconds > 0 &&
		silence >= time.Duration(cfg.HelperSilenceSeconds)*time.Second {
		s.emit(now, "helper_silence", fmt.Sprintf("分片 %s 已 %s 没有新的助手 MOD 快照（阈值 %d 秒）",
			silenceKey, humanSeconds(int(silence.Seconds())), cfg.HelperSilenceSeconds))
	}
}

// CheckShards 是每轮心跳的崩溃检测：上一轮在跑、这一轮消失、且不是面板正常停过的分片，
// 一律产生一条明确告警（不静默）。只有自愈开关打开时才自动拉起。
func (s *Supervisor) CheckShards(ctx context.Context) {
	cfg := s.opts.Get()
	running, err := s.currentRunning()
	if err != nil {
		s.log().Warn("心跳检测读进程状态失败", "err", err.Error())
		return
	}
	now := time.Now()
	s.mu.Lock()
	prev := s.prevRunning
	s.prevRunning = running
	expected := make(map[string]bool, len(s.expected))
	for k, v := range s.expected {
		expected[k] = v
	}
	s.mu.Unlock()

	for key := range prev {
		if running[key] || !expected[key] {
			continue
		}
		cluster, shard := splitKey(key)
		note := "；自愈开关当前关闭，面板不会自动重启它"
		if cfg.AutoRecover {
			note = "；自愈开关已打开，正在自动拉起"
		}
		s.emit(now, "shard_exit:"+key,
			fmt.Sprintf("分片 %s/%s 原本在运行，进程已消失（疑似崩溃）%s", cluster, shard, note))
		if !cfg.AutoRecover {
			continue
		}
		if err := s.procs.Start(ctx, cluster, shard); err != nil {
			s.emit(now, "shard_recover_fail:"+key,
				fmt.Sprintf("自动拉起 %s/%s 失败：%s", cluster, shard, err.Error()))
		}
	}
}

// Recover 在面板启动时跑一次：开关打开就把上次退出前仍在跑、如今不在的分片拉起来。
// 由 main.go 放在后台 goroutine 里调，不拖住面板监听端口。
func (s *Supervisor) Recover(ctx context.Context) {
	cfg := s.opts.Get()
	if !cfg.AutoRecover {
		return
	}
	running, err := s.currentRunning()
	if err != nil {
		s.log().Warn("随面板恢复读进程状态失败", "err", err.Error())
		return
	}
	s.mu.Lock()
	expected := make(map[string]bool, len(s.expected))
	for k, v := range s.expected {
		expected[k] = v
	}
	s.mu.Unlock()
	now := time.Now()
	for key := range expected {
		if running[key] {
			continue
		}
		cluster, shard := splitKey(key)
		if err := s.procs.Start(ctx, cluster, shard); err != nil {
			s.emit(now, "shard_boot_fail:"+key,
				fmt.Sprintf("随面板恢复：自动拉起 %s/%s 失败：%s", cluster, shard, err.Error()))
			continue
		}
		s.mu.Lock()
		s.recovered++
		s.mu.Unlock()
		s.emit(now, "shard_boot:"+key,
			fmt.Sprintf("随面板恢复：已自动拉起上次在运行的分片 %s/%s", cluster, shard))
	}
}

// emit 做去重/冷却，然后写一条操作记录并（可选）POST Webhook。
// 复用 store.AppendAudit 这一条现成的事件机制：actor=system 的告警行在审计页查得到。
func (s *Supervisor) emit(now time.Time, kind, msg string) {
	cfg := s.opts.Get()
	cooldown := time.Duration(cfg.AlertCooldownMinutes) * time.Minute
	s.mu.Lock()
	if last, ok := s.lastAlert[kind]; ok && now.Sub(last) < cooldown {
		s.mu.Unlock()
		return
	}
	s.lastAlert[kind] = now
	a := Alert{At: now.UTC().Format(time.RFC3339), Kind: kind, Message: msg}
	s.alerts = append(s.alerts, a)
	if len(s.alerts) > 50 {
		s.alerts = s.alerts[len(s.alerts)-50:]
	}
	s.mu.Unlock()

	if s.store != nil {
		if err := s.store.AppendAudit(store.AuditEntry{
			Actor: "system", Action: "monitor.alert", Target: kind,
			Detail: msg, OK: false, At: now,
		}); err != nil {
			s.log().Warn("告警写入操作记录失败", "kind", kind, "err", err.Error())
		}
	}
	if cfg.WebhookURL != "" {
		go s.postWebhook(cfg, a)
	}
	s.log().Warn("监控告警", "kind", kind, "message", msg)
}

// postWebhook 带超时，失败只记日志。放 goroutine 里跑，卡住也不拖慢采样。
func (s *Supervisor) postWebhook(cfg Settings, a Alert) {
	body, err := json.Marshal(map[string]any{
		"source": "dstpanel", "kind": a.Kind, "message": a.Message, "at": a.At,
	})
	if err != nil {
		return
	}
	client := &http.Client{Timeout: time.Duration(cfg.WebhookTimeoutSeconds) * time.Second}
	req, err := http.NewRequest(http.MethodPost, cfg.WebhookURL, bytes.NewReader(body))
	if err != nil {
		s.log().Warn("Webhook 地址不合法", "err", err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := client.Do(req)
	if err != nil {
		s.log().Warn("Webhook 通知失败", "kind", a.Kind, "err", err.Error())
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode >= 400 {
		s.log().Warn("Webhook 返回非成功状态", "kind", a.Kind, "code", resp.StatusCode)
	}
}

// currentRunning 一次 screen -ls 问完全部分片的会话，再按本面板管理的集群/分片名对照。
func (s *Supervisor) currentRunning() (map[string]bool, error) {
	sessions, err := s.procs.Sessions(context.Background())
	if err != nil {
		return nil, err
	}
	names, err := s.paths.ClusterNames()
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, cluster := range names {
		for _, shard := range s.paths.ShardNames(cluster) {
			if _, ok := sessions[s.procs.SessionName(cluster, shard)]; ok {
				out[shardKey(cluster, shard)] = true
			}
		}
	}
	return out, nil
}

// helperSilence 返回「在跑的分片里最久没写过助手快照」的时长和它属于谁。
// 文件从没写出来过记 24 小时（一个必然超过阈值的哨兵），不是编一个精确的假时刻。
func (s *Supervisor) helperSilence(now time.Time, running map[string]bool) (time.Duration, string) {
	if len(running) == 0 {
		return 0, ""
	}
	var worst time.Duration
	worstKey := ""
	for key := range running {
		cluster, shard := splitKey(key)
		var silence time.Duration
		st, err := os.Stat(s.paths.HelperStatusFile(cluster, shard))
		switch {
		case err != nil || st.Size() == 0:
			silence = 24 * time.Hour
		default:
			silence = now.Sub(st.ModTime())
			if silence < 0 {
				silence = 0
			}
		}
		if silence > worst {
			worst, worstKey = silence, key
		}
	}
	return worst, worstKey
}

func (s *Supervisor) writeSamples(samples []Sample) error {
	var b strings.Builder
	for _, sm := range samples {
		raw, err := json.Marshal(sm)
		if err != nil {
			continue
		}
		b.Write(raw)
		b.WriteByte('\n')
	}
	return dst.WriteFileAtomicNoBackup(filepath.Join(s.dataDir, samplesFile), []byte(b.String()), 0o644)
}

func (s *Supervisor) loadSamples(limit int) []Sample {
	out := []Sample{}
	raw, err := os.ReadFile(filepath.Join(s.dataDir, samplesFile))
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var sm Sample
		if json.Unmarshal([]byte(line), &sm) == nil {
			out = append(out, sm)
		}
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

func (s *Supervisor) writeExpected(m map[string]bool) error {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	raw, err := json.MarshalIndent(struct {
		Keys []string `json:"keys"`
	}{Keys: keys}, "", "  ")
	if err != nil {
		return err
	}
	return dst.WriteFileAtomicNoBackup(filepath.Join(s.dataDir, expectedFile), raw, 0o644)
}

func (s *Supervisor) loadExpected() map[string]bool {
	out := map[string]bool{}
	raw, err := os.ReadFile(filepath.Join(s.dataDir, expectedFile))
	if err != nil {
		return out
	}
	var f struct {
		Keys []string `json:"keys"`
	}
	if json.Unmarshal(raw, &f) != nil {
		return out
	}
	for _, k := range f.Keys {
		out[k] = true
	}
	return out
}

func shardKey(cluster, shard string) string { return cluster + "/" + shard }

func splitKey(key string) (string, string) {
	i := strings.LastIndexByte(key, '/')
	if i < 0 {
		return key, ""
	}
	return key[:i], key[i+1:]
}

func humanSeconds(sec int) string {
	switch {
	case sec < 60:
		return fmt.Sprintf("%d 秒", sec)
	case sec < 3600:
		return fmt.Sprintf("%d 分钟", sec/60)
	default:
		return fmt.Sprintf("%d 小时 %d 分钟", sec/3600, (sec%3600)/60)
	}
}
