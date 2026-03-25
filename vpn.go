package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type TunnelStatus string

const (
	StatusConnected    TunnelStatus = "connected"
	StatusConnecting   TunnelStatus = "connecting"
	StatusDisconnected TunnelStatus = "disconnected"
	StatusReconnecting TunnelStatus = "reconnecting"
)

type TrafficStats struct {
	RxBytes uint64
	TxBytes uint64
	RxRate  float64 // bytes/sec
	TxRate  float64 // bytes/sec
}

type Tunnel struct {
	Index      int          `json:"index"`
	Name       string       `json:"name"`
	ConfigFile string       `json:"config_file"`
	TunDev     string       `json:"tun_dev"`
	Status     TunnelStatus `json:"status"`
	ConnectedAt *time.Time  `json:"connected_at,omitempty"`
	AutoReconnect bool      `json:"auto_reconnect"`

	// runtime only
	cmd        *exec.Cmd
	stats      TrafficStats
	prevRx     uint64
	prevTx     uint64
	prevTime   time.Time
	stopCh     chan struct{}
	mu         sync.Mutex
}

type StateFile struct {
	Tunnels []TunnelState `json:"tunnels"`
}

type TunnelState struct {
	Index         int    `json:"index"`
	Name          string `json:"name"`
	ConfigFile    string `json:"config_file"`
	AutoReconnect bool   `json:"auto_reconnect"`
}

type LogEntry struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level"` // ok | warn | err
	Message string    `json:"message"`
}

type VPNManager struct {
	dataDir   string
	mu        sync.RWMutex
	tunnels   []*Tunnel
	logs      []LogEntry
	maxLogs   int
}

func NewVPNManager(dataDir string) *VPNManager {
	return &VPNManager{
		dataDir: dataDir,
		maxLogs: 200,
	}
}

// ── Persistence ──────────────────────────────────────────────────────────────

func (m *VPNManager) stateFile() string {
	return filepath.Join(m.dataDir, "state.json")
}

func (m *VPNManager) SaveState() {
	m.mu.RLock()
	defer m.mu.RUnlock()

	sf := StateFile{}
	for _, t := range m.tunnels {
		sf.Tunnels = append(sf.Tunnels, TunnelState{
			Index:         t.Index,
			Name:          t.Name,
			ConfigFile:    t.ConfigFile,
			AutoReconnect: t.AutoReconnect,
		})
	}

	data, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		log.Printf("SaveState marshal: %v", err)
		return
	}
	if err := os.WriteFile(m.stateFile(), data, 0644); err != nil {
		log.Printf("SaveState write: %v", err)
	}
}

func (m *VPNManager) LoadState() {
	data, err := os.ReadFile(m.stateFile())
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("LoadState: %v", err)
		}
		return
	}

	var sf StateFile
	if err := json.Unmarshal(data, &sf); err != nil {
		log.Printf("LoadState unmarshal: %v", err)
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, ts := range sf.Tunnels {
		cfgPath := filepath.Join(m.dataDir, "configs", ts.ConfigFile)
		if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
			log.Printf("Config file missing, skipping: %s", cfgPath)
			continue
		}
		t := &Tunnel{
			Index:         ts.Index,
			Name:          ts.Name,
			ConfigFile:    ts.ConfigFile,
			TunDev:        fmt.Sprintf("tun%d", ts.Index),
			Status:        StatusDisconnected,
			AutoReconnect: ts.AutoReconnect,
			stopCh:        make(chan struct{}),
		}
		m.tunnels = append(m.tunnels, t)
	}

	log.Printf("Loaded %d tunnel configs from state", len(m.tunnels))
}

func (m *VPNManager) RestoreConnections() {
	m.mu.RLock()
	tunnels := make([]*Tunnel, len(m.tunnels))
	copy(tunnels, m.tunnels)
	m.mu.RUnlock()

	for _, t := range tunnels {
		if t.AutoReconnect {
			m.addLog("warn", fmt.Sprintf("[%s] restoring connection after restart", t.TunDev))
			go m.startTunnel(t)
		}
	}
}

// ── Logging ───────────────────────────────────────────────────────────────────

func (m *VPNManager) addLog(level, msg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.logs = append([]LogEntry{{Time: time.Now(), Level: level, Message: msg}}, m.logs...)
	if len(m.logs) > m.maxLogs {
		m.logs = m.logs[:m.maxLogs]
	}
}

func (m *VPNManager) GetLogs() []LogEntry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	cp := make([]LogEntry, len(m.logs))
	copy(cp, m.logs)
	return cp
}

// ── Config upload ─────────────────────────────────────────────────────────────

func (m *VPNManager) AddConfig(filename string, data []byte) (*Tunnel, error) {
	if !strings.HasSuffix(filename, ".ovpn") {
		return nil, fmt.Errorf("file must have .ovpn extension")
	}

	safe := filepath.Base(filename)
	cfgPath := filepath.Join(m.dataDir, "configs", safe)

	if err := os.WriteFile(cfgPath, data, 0600); err != nil {
		return nil, fmt.Errorf("write config: %w", err)
	}

	m.mu.Lock()
	idx := len(m.tunnels)
	name := strings.TrimSuffix(safe, ".ovpn")
	t := &Tunnel{
		Index:         idx,
		Name:          name,
		ConfigFile:    safe,
		TunDev:        fmt.Sprintf("tun%d", idx),
		Status:        StatusDisconnected,
		AutoReconnect: true,
		stopCh:        make(chan struct{}),
	}
	m.tunnels = append(m.tunnels, t)
	m.mu.Unlock()

	m.SaveState()
	m.addLog("ok", fmt.Sprintf("[%s] config uploaded: %s", t.TunDev, safe))
	return t, nil
}

func (m *VPNManager) DeleteConfig(index int) error {
	m.mu.Lock()
	if index < 0 || index >= len(m.tunnels) {
		m.mu.Unlock()
		return fmt.Errorf("invalid index")
	}
	t := m.tunnels[index]
	m.mu.Unlock()

	m.StopTunnel(index)

	cfgPath := filepath.Join(m.dataDir, "configs", t.ConfigFile)
	os.Remove(cfgPath)

	m.mu.Lock()
	m.tunnels = append(m.tunnels[:index], m.tunnels[index+1:]...)
	// re-index
	for i, tt := range m.tunnels {
		tt.Index = i
		tt.TunDev = fmt.Sprintf("tun%d", i)
	}
	m.mu.Unlock()

	m.SaveState()
	m.addLog("warn", fmt.Sprintf("config removed: %s", t.ConfigFile))
	return nil
}

// ── Tunnel control ────────────────────────────────────────────────────────────

func (m *VPNManager) StartTunnel(index int) error {
	m.mu.RLock()
	if index < 0 || index >= len(m.tunnels) {
		m.mu.RUnlock()
		return fmt.Errorf("invalid index")
	}
	t := m.tunnels[index]
	m.mu.RUnlock()

	go m.startTunnel(t)
	return nil
}

func (m *VPNManager) startTunnel(t *Tunnel) {
	t.mu.Lock()
	if t.Status == StatusConnected || t.Status == StatusConnecting {
		t.mu.Unlock()
		return
	}
	t.Status = StatusConnecting
	t.stopCh = make(chan struct{})
	t.mu.Unlock()

	m.addLog("warn", fmt.Sprintf("[%s] connecting — %s", t.TunDev, t.ConfigFile))

	for {
		cfgPath := filepath.Join(m.dataDir, "configs", t.ConfigFile)
		cmd := exec.Command("openvpn",
			"--config", cfgPath,
			"--dev", t.TunDev,
			"--dev-type", "tun",
			"--daemon",
			"--log", filepath.Join(m.dataDir, t.TunDev+".log"),
			"--writepid", filepath.Join(m.dataDir, t.TunDev+".pid"),
			"--script-security", "2",
		)

		t.mu.Lock()
		t.cmd = cmd
		t.mu.Unlock()

		if err := cmd.Run(); err != nil {
			select {
			case <-t.stopCh:
				t.mu.Lock()
				t.Status = StatusDisconnected
				t.mu.Unlock()
				m.addLog("warn", fmt.Sprintf("[%s] stopped", t.TunDev))
				return
			default:
			}
			m.addLog("err", fmt.Sprintf("[%s] openvpn exited: %v — waiting 5s", t.TunDev, err))
		} else {
			// daemon launched successfully, monitor the pid
			now := time.Now()
			t.mu.Lock()
			t.Status = StatusConnected
			t.ConnectedAt = &now
			t.mu.Unlock()
			m.addLog("ok", fmt.Sprintf("[%s] connected — %s", t.TunDev, t.Name))

			// wait for pid to disappear (process died) or stop signal
			m.monitorPid(t)

			select {
			case <-t.stopCh:
				t.mu.Lock()
				t.Status = StatusDisconnected
				t.mu.Unlock()
				m.addLog("warn", fmt.Sprintf("[%s] stopped", t.TunDev))
				return
			default:
			}

			t.mu.Lock()
			t.Status = StatusReconnecting
			t.mu.Unlock()
			m.addLog("err", fmt.Sprintf("[%s] connection lost — reconnecting in 5s", t.TunDev))
		}

		if !t.AutoReconnect {
			t.mu.Lock()
			t.Status = StatusDisconnected
			t.mu.Unlock()
			return
		}

		select {
		case <-t.stopCh:
			t.mu.Lock()
			t.Status = StatusDisconnected
			t.mu.Unlock()
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func (m *VPNManager) monitorPid(t *Tunnel) {
	pidFile := filepath.Join(m.dataDir, t.TunDev+".pid")
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-t.stopCh:
			return
		case <-ticker.C:
			data, err := os.ReadFile(pidFile)
			if err != nil {
				return // pid file gone → process died
			}
			pid := strings.TrimSpace(string(data))
			if pid == "" {
				return
			}
			// check /proc/<pid>
			if _, err := os.Stat("/proc/" + pid); os.IsNotExist(err) {
				return
			}
		}
	}
}

func (m *VPNManager) StopTunnel(index int) error {
	m.mu.RLock()
	if index < 0 || index >= len(m.tunnels) {
		m.mu.RUnlock()
		return fmt.Errorf("invalid index")
	}
	t := m.tunnels[index]
	m.mu.RUnlock()

	// signal the goroutine to stop
	t.mu.Lock()
	select {
	case <-t.stopCh:
		// already closed
	default:
		close(t.stopCh)
	}
	t.mu.Unlock()

	// kill the openvpn daemon via pid file
	pidFile := filepath.Join(m.dataDir, t.TunDev+".pid")
	if data, err := os.ReadFile(pidFile); err == nil {
		pid := strings.TrimSpace(string(data))
		if pid != "" {
			exec.Command("kill", pid).Run()
		}
		os.Remove(pidFile)
	}

	t.mu.Lock()
	t.Status = StatusDisconnected
	t.ConnectedAt = nil
	t.mu.Unlock()

	m.addLog("warn", fmt.Sprintf("[%s] stopped by user", t.TunDev))
	return nil
}

func (m *VPNManager) RestartTunnel(index int) error {
	m.StopTunnel(index)
	time.Sleep(1 * time.Second)
	return m.StartTunnel(index)
}

func (m *VPNManager) SetAutoReconnect(index int, enabled bool) error {
	m.mu.Lock()
	if index < 0 || index >= len(m.tunnels) {
		m.mu.Unlock()
		return fmt.Errorf("invalid index")
	}
	m.tunnels[index].AutoReconnect = enabled
	m.mu.Unlock()
	m.SaveState()
	return nil
}

// ── Traffic stats ─────────────────────────────────────────────────────────────

func (m *VPNManager) UpdateTraffic() {
	m.mu.RLock()
	tunnels := make([]*Tunnel, len(m.tunnels))
	copy(tunnels, m.tunnels)
	m.mu.RUnlock()

	for _, t := range tunnels {
		rx, tx := readIfaceStats(t.TunDev)
		now := time.Now()

		t.mu.Lock()
		if !t.prevTime.IsZero() {
			elapsed := now.Sub(t.prevTime).Seconds()
			if elapsed > 0 {
				t.stats.RxRate = float64(rx-t.prevRx) / elapsed
				t.stats.TxRate = float64(tx-t.prevTx) / elapsed
			}
		}
		t.stats.RxBytes = rx
		t.stats.TxBytes = tx
		t.prevRx = rx
		t.prevTx = tx
		t.prevTime = now
		t.mu.Unlock()
	}
}

func readIfaceStats(iface string) (rx, tx uint64) {
	data, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return
	}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, iface+":") {
			continue
		}
		line = strings.TrimPrefix(line, iface+":")
		fields := strings.Fields(line)
		if len(fields) >= 9 {
			rx, _ = strconv.ParseUint(fields[0], 10, 64)
			tx, _ = strconv.ParseUint(fields[8], 10, 64)
		}
		return
	}
	return
}

// ── Public getters ────────────────────────────────────────────────────────────

type TunnelInfo struct {
	Index         int          `json:"index"`
	Name          string       `json:"name"`
	ConfigFile    string       `json:"config_file"`
	TunDev        string       `json:"tun_dev"`
	Status        TunnelStatus `json:"status"`
	ConnectedAt   *time.Time   `json:"connected_at,omitempty"`
	AutoReconnect bool         `json:"auto_reconnect"`
	RxBytes       uint64       `json:"rx_bytes"`
	TxBytes       uint64       `json:"tx_bytes"`
	RxRate        float64      `json:"rx_rate"`
	TxRate        float64      `json:"tx_rate"`
}

func (m *VPNManager) GetTunnels() []TunnelInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]TunnelInfo, len(m.tunnels))
	for i, t := range m.tunnels {
		t.mu.Lock()
		out[i] = TunnelInfo{
			Index:         t.Index,
			Name:          t.Name,
			ConfigFile:    t.ConfigFile,
			TunDev:        t.TunDev,
			Status:        t.Status,
			ConnectedAt:   t.ConnectedAt,
			AutoReconnect: t.AutoReconnect,
			RxBytes:       t.stats.RxBytes,
			TxBytes:       t.stats.TxBytes,
			RxRate:        t.stats.RxRate,
			TxRate:        t.stats.TxRate,
		}
		t.mu.Unlock()
	}
	return out
}
