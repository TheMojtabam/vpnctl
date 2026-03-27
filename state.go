package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ── Tunnel types ──────────────────────────────────────────────────────────────

type TunnelStatus string

const (
	StatusConnected    TunnelStatus = "connected"
	StatusConnecting   TunnelStatus = "connecting"
	StatusDisconnected TunnelStatus = "disconnected"
	StatusReconnecting TunnelStatus = "reconnecting"
)

type TunnelCfg struct {
	Protocol    string `json:"protocol"`     // "udp" | "tcp"
	Compression string `json:"compression"`  // "none" | "lz4-v2" | "stub"
	BandwidthMB int    `json:"bandwidth_mb"` // 0 = unlimited
	Group       string `json:"group"`
	Note        string `json:"note"`
}

type TunnelPerf struct {
	SpeedMbps    float64    `json:"speed_mbps"`
	SpeedAt      *time.Time `json:"speed_at,omitempty"`
	PingMs       float64    `json:"ping_ms"`
	PacketLoss   float64    `json:"packet_loss"`
	MTU          int        `json:"mtu"`
	ExternalIP   string     `json:"external_ip"`
	RestartHour  int        `json:"restart_hour"`
	RestartCount int        `json:"restart_count"`
	UDPFails     int        `json:"udp_fails"`
}

type PersistedTunnel struct {
	Index         int         `json:"index"`
	Name          string      `json:"name"`
	ConfigFile    string      `json:"config_file"`
	AutoReconnect bool        `json:"auto_reconnect"`
	ConnectedAt   *time.Time  `json:"connected_at,omitempty"`
	Cfg           TunnelCfg   `json:"cfg"`
	Perf          TunnelPerf  `json:"perf"`
}

// ── Settings ──────────────────────────────────────────────────────────────────

type SpeedTestCfg struct {
	Enabled  bool   `json:"enabled"`
	URL      string `json:"url"`
	SizeMB   int    `json:"size_mb"`
	Interval int    `json:"interval"` // minutes, 0=manual
}

type AutoRestartCfg struct {
	Enabled       bool `json:"enabled"`
	ThresholdMbps int  `json:"threshold_mbps"`
	MaxPerHour    int  `json:"max_per_hour"`
	AutoSwitchTCP bool `json:"auto_switch_tcp"`
}

type MTUCfg struct {
	Enabled         bool   `json:"enabled"`
	PingTarget      string `json:"ping_target"`
	DetectOnConnect bool   `json:"detect_on_connect"`
	Apply           bool   `json:"apply"`
}

type PingCfg struct {
	Enabled  bool `json:"enabled"`
	Interval int  `json:"interval"` // seconds
	Count    int  `json:"count"`
}

type DNSCfg struct {
	Enabled        bool   `json:"enabled"`
	IPCheckURL     string `json:"ip_check_url"`
	CheckOnConnect bool   `json:"check_on_connect"`
}

type KillSwitchCfg struct {
	Enabled  bool `json:"enabled"`
	AllowLAN bool `json:"allow_lan"`
}

type WebhookCfg struct {
	Enabled    bool   `json:"enabled"`
	URL        string `json:"url"`
	AlertAfter int    `json:"alert_after"` // minutes
}

type TunnelDefaults struct {
	Protocol    string `json:"protocol"`
	Compression string `json:"compression"`
	BandwidthMB int    `json:"bandwidth_mb"`
}

type Settings struct {
	SpeedTest   SpeedTestCfg   `json:"speed_test"`
	AutoRestart AutoRestartCfg `json:"auto_restart"`
	MTU         MTUCfg         `json:"mtu"`
	Ping        PingCfg        `json:"ping"`
	DNS         DNSCfg         `json:"dns"`
	KillSwitch  KillSwitchCfg  `json:"kill_switch"`
	Webhook     WebhookCfg     `json:"webhook"`
	Defaults    TunnelDefaults `json:"defaults"`
}

func defaultSettings() Settings {
	return Settings{
		SpeedTest: SpeedTestCfg{
			Enabled: true,
			URL:     "https://speed.cloudflare.com/__down",
			SizeMB:  5,
		},
		AutoRestart: AutoRestartCfg{
			Enabled:       true,
			ThresholdMbps: 10,
			MaxPerHour:    3,
			AutoSwitchTCP: true,
		},
		MTU: MTUCfg{
			Enabled:         true,
			PingTarget:      "8.8.8.8",
			DetectOnConnect: true,
			Apply:           true,
		},
		Ping: PingCfg{
			Enabled:  true,
			Interval: 30,
			Count:    4,
		},
		DNS: DNSCfg{
			Enabled:        true,
			IPCheckURL:     "https://api.ipify.org",
			CheckOnConnect: true,
		},
		KillSwitch: KillSwitchCfg{
			Enabled:  false,
			AllowLAN: true,
		},
		Webhook: WebhookCfg{
			Enabled:    false,
			AlertAfter: 5,
		},
		Defaults: TunnelDefaults{
			Protocol:    "udp",
			Compression: "none",
		},
	}
}

// ── Traffic history ────────────────────────────────────────────────────────────

type TrafficDay struct {
	Date    string `json:"date"`     // "2006-01-02"
	TunDev  string `json:"tun_dev"`
	RxBytes uint64 `json:"rx_bytes"`
	TxBytes uint64 `json:"tx_bytes"`
}

// ── Cron ──────────────────────────────────────────────────────────────────────

type CronJob struct {
	ID         int        `json:"id"`
	Expression string     `json:"expression"`
	Target     string     `json:"target"`
	Action     string     `json:"action"`
	Enabled    bool       `json:"enabled"`
	NextRun    time.Time  `json:"next_run"`
	LastRun    *time.Time `json:"last_run,omitempty"`
}

type PersistedCron struct {
	Jobs   []CronJob `json:"jobs"`
	NextID int       `json:"next_id"`
}

// ── Log ──────────────────────────────────────────────────────────────────────

type LogEntry struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level"` // ok | warn | err | info
	Message string    `json:"message"`
}

// ── Root state file ────────────────────────────────────────────────────────────

type StateFile struct {
	StartedAt      time.Time         `json:"started_at"`
	Tunnels        []PersistedTunnel `json:"tunnels"`
	Settings       Settings          `json:"settings"`
	Cron           PersistedCron     `json:"cron"`
	TrafficHistory []TrafficDay      `json:"traffic_history"`
	ExternalIP     string            `json:"external_ip"`
	Logs           []LogEntry        `json:"logs"`
	Groups         []string          `json:"groups"`
}

// ── AppState — central store ──────────────────────────────────────────────────

type AppState struct {
	mu      sync.RWMutex
	path    string
	sf      StateFile
}

func NewAppState(dataDir string) *AppState {
	return &AppState{
		path: filepath.Join(dataDir, "state.json"),
		sf: StateFile{
			StartedAt: time.Now(),
			Settings:  defaultSettings(),
			Groups:    []string{"EU", "US", "Asia"},
		},
	}
}

func (a *AppState) Load() {
	data, err := os.ReadFile(a.path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("state load: %v", err)
		}
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := json.Unmarshal(data, &a.sf); err != nil {
		log.Printf("state unmarshal: %v", err)
		return
	}
	if a.sf.StartedAt.IsZero() {
		a.sf.StartedAt = time.Now()
	}
	// merge missing settings with defaults
	def := defaultSettings()
	if a.sf.Settings.SpeedTest.URL == "" {
		a.sf.Settings.SpeedTest.URL = def.SpeedTest.URL
	}
	if a.sf.Settings.MTU.PingTarget == "" {
		a.sf.Settings.MTU.PingTarget = def.MTU.PingTarget
	}
	if a.sf.Settings.DNS.IPCheckURL == "" {
		a.sf.Settings.DNS.IPCheckURL = def.DNS.IPCheckURL
	}
	if a.sf.Settings.Defaults.Protocol == "" {
		a.sf.Settings.Defaults.Protocol = def.Defaults.Protocol
	}
}

func (a *AppState) Save() {
	a.mu.RLock()
	data, err := json.MarshalIndent(a.sf, "", "  ")
	a.mu.RUnlock()
	if err != nil {
		log.Printf("state marshal: %v", err)
		return
	}
	if err := os.WriteFile(a.path, data, 0644); err != nil {
		log.Printf("state write: %v", err)
	}
}

// Read-only snapshot
func (a *AppState) Snap() StateFile {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.sf
}

func (a *AppState) GetSettings() Settings {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.sf.Settings
}

func (a *AppState) SetSettings(s Settings) {
	a.mu.Lock()
	a.sf.Settings = s
	a.mu.Unlock()
	a.Save()
}

func (a *AppState) GetStartedAt() time.Time {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.sf.StartedAt
}

func (a *AppState) GetExternalIP() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.sf.ExternalIP
}

func (a *AppState) SetExternalIP(ip string) {
	a.mu.Lock()
	a.sf.ExternalIP = ip
	a.mu.Unlock()
	a.Save()
}

func (a *AppState) GetGroups() []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	cp := make([]string, len(a.sf.Groups))
	copy(cp, a.sf.Groups)
	return cp
}

func (a *AppState) SetGroups(g []string) {
	a.mu.Lock()
	a.sf.Groups = g
	a.mu.Unlock()
	a.Save()
}

// AddTraffic records daily traffic for a tunnel
func (a *AppState) AddTraffic(tunDev string, rx, tx uint64) {
	today := time.Now().Format("2006-01-02")
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range a.sf.TrafficHistory {
		if a.sf.TrafficHistory[i].Date == today && a.sf.TrafficHistory[i].TunDev == tunDev {
			a.sf.TrafficHistory[i].RxBytes += rx
			a.sf.TrafficHistory[i].TxBytes += tx
			return
		}
	}
	a.sf.TrafficHistory = append(a.sf.TrafficHistory, TrafficDay{
		Date: today, TunDev: tunDev, RxBytes: rx, TxBytes: tx,
	})
	// Keep last 30 days × tunnels
	if len(a.sf.TrafficHistory) > 30*30 {
		a.sf.TrafficHistory = a.sf.TrafficHistory[len(a.sf.TrafficHistory)-900:]
	}
}

func (a *AppState) GetTrafficHistory() []TrafficDay {
	a.mu.RLock()
	defer a.mu.RUnlock()
	cp := make([]TrafficDay, len(a.sf.TrafficHistory))
	copy(cp, a.sf.TrafficHistory)
	return cp
}

// AddLog prepends a log entry
func (a *AppState) AddLog(level, msg string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sf.Logs = append([]LogEntry{{Time: time.Now(), Level: level, Message: msg}}, a.sf.Logs...)
	if len(a.sf.Logs) > 300 {
		a.sf.Logs = a.sf.Logs[:300]
	}
}

func (a *AppState) GetLogs() []LogEntry {
	a.mu.RLock()
	defer a.mu.RUnlock()
	cp := make([]LogEntry, len(a.sf.Logs))
	copy(cp, a.sf.Logs)
	return cp
}

func (a *AppState) ClearLogs() {
	a.mu.Lock()
	a.sf.Logs = nil
	a.mu.Unlock()
	a.Save()
}

// UpdateTunnelPerf updates performance data for a tunnel by index
func (a *AppState) UpdateTunnelPerf(index int, fn func(*TunnelPerf)) {
	a.mu.Lock()
	for i := range a.sf.Tunnels {
		if a.sf.Tunnels[i].Index == index {
			fn(&a.sf.Tunnels[i].Perf)
			break
		}
	}
	a.mu.Unlock()
	a.Save()
}

// UpdateTunnelCfg updates per-tunnel config
func (a *AppState) UpdateTunnelCfg(index int, fn func(*TunnelCfg)) {
	a.mu.Lock()
	for i := range a.sf.Tunnels {
		if a.sf.Tunnels[i].Index == index {
			fn(&a.sf.Tunnels[i].Cfg)
			break
		}
	}
	a.mu.Unlock()
	a.Save()
}

// GetPersistedTunnel returns a copy of a persisted tunnel
func (a *AppState) GetPersistedTunnel(index int) (PersistedTunnel, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, t := range a.sf.Tunnels {
		if t.Index == index {
			return t, true
		}
	}
	return PersistedTunnel{}, false
}
