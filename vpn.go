package main

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type TrafficStats struct {
	RxBytes uint64
	TxBytes uint64
	RxRate  float64
	TxRate  float64
}

type Tunnel struct {
	// Persisted fields (via PersistedTunnel)
	Index         int
	Name          string
	ConfigFile    string
	TunDev        string
	AutoReconnect bool
	ConnectedAt   *time.Time
	Cfg           TunnelCfg
	Perf          TunnelPerf

	// Runtime only
	Status   TunnelStatus
	cmd      *exec.Cmd
	stats    TrafficStats
	prevRx   uint64
	prevTx   uint64
	prevTime time.Time
	stopCh   chan struct{}
	mu       sync.Mutex
}

type TunnelInfo struct {
	Index         int          `json:"index"`
	Name          string       `json:"name"`
	ConfigFile    string       `json:"config_file"`
	TunDev        string       `json:"tun_dev"`
	Status        TunnelStatus `json:"status"`
	AutoReconnect bool         `json:"auto_reconnect"`
	ConnectedAt   *time.Time   `json:"connected_at,omitempty"`
	Cfg           TunnelCfg    `json:"cfg"`
	Perf          TunnelPerf   `json:"perf"`
	RxRate        float64      `json:"rx_rate"`
	TxRate        float64      `json:"tx_rate"`
	RxBytes       uint64       `json:"rx_bytes"`
	TxBytes       uint64       `json:"tx_bytes"`
}

type VPNManager struct {
	mu       sync.RWMutex
	dataDir  string
	ovpnBin  string
	libDir   string
	tunnels  []*Tunnel
	state    *AppState
}

func NewVPNManager(dataDir, ovpnBin, libDir string, state *AppState) *VPNManager {
	return &VPNManager{
		dataDir: dataDir,
		ovpnBin: ovpnBin,
		libDir:  libDir,
		state:   state,
	}
}

// ── Persistence ───────────────────────────────────────────────────────────────

func (m *VPNManager) LoadTunnels() {
	snap := m.state.Snap()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, pt := range snap.Tunnels {
		cfgPath := filepath.Join(m.dataDir, "configs", pt.ConfigFile)
		if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
			continue
		}
		t := &Tunnel{
			Index:         pt.Index,
			Name:          pt.Name,
			ConfigFile:    pt.ConfigFile,
			TunDev:        fmt.Sprintf("tun%d", pt.Index),
			AutoReconnect: pt.AutoReconnect,
			ConnectedAt:   pt.ConnectedAt,
			Cfg:           pt.Cfg,
			Perf:          pt.Perf,
			Status:        StatusDisconnected,
			stopCh:        make(chan struct{}),
		}
		if t.Cfg.Protocol == "" {
			t.Cfg.Protocol = snap.Settings.Defaults.Protocol
		}
		if t.Cfg.Protocol == "" {
			t.Cfg.Protocol = "udp"
		}
		m.tunnels = append(m.tunnels, t)
	}
}

func (m *VPNManager) saveTunnels() {
	m.mu.RLock()
	pts := make([]PersistedTunnel, 0, len(m.tunnels))
	for _, t := range m.tunnels {
		t.mu.Lock()
		pts = append(pts, PersistedTunnel{
			Index:         t.Index,
			Name:          t.Name,
			ConfigFile:    t.ConfigFile,
			AutoReconnect: t.AutoReconnect,
			ConnectedAt:   t.ConnectedAt,
			Cfg:           t.Cfg,
			Perf:          t.Perf,
		})
		t.mu.Unlock()
	}
	m.mu.RUnlock()
	m.state.mu.Lock()
	m.state.sf.Tunnels = pts
	m.state.mu.Unlock()
	m.state.Save()
}

// ── Config management ─────────────────────────────────────────────────────────

func (m *VPNManager) AddConfig(filename string, data []byte) (*TunnelInfo, error) {
	if !strings.HasSuffix(filename, ".ovpn") {
		return nil, fmt.Errorf("file must have .ovpn extension")
	}
	safe := filepath.Base(filename)
	cfgPath := filepath.Join(m.dataDir, "configs", safe)
	if err := os.WriteFile(cfgPath, data, 0600); err != nil {
		return nil, fmt.Errorf("write config: %w", err)
	}

	snap := m.state.GetSettings()

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
		Cfg: TunnelCfg{
			Protocol:    snap.Defaults.Protocol,
			Compression: snap.Defaults.Compression,
			BandwidthMB: snap.Defaults.BandwidthMB,
		},
	}
	if t.Cfg.Protocol == "" {
		t.Cfg.Protocol = "udp"
	}
	m.tunnels = append(m.tunnels, t)
	m.mu.Unlock()

	m.saveTunnels()
	m.state.AddLog("ok", fmt.Sprintf("[%s] config uploaded: %s", t.TunDev, safe))
	info := m.tunnelInfo(t)
	return &info, nil
}

func (m *VPNManager) ImportFromURL(rawURL string) (*TunnelInfo, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(rawURL)
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	// Derive filename from URL
	parts := strings.Split(strings.TrimRight(rawURL, "/"), "/")
	fname := parts[len(parts)-1]
	if !strings.HasSuffix(fname, ".ovpn") {
		fname += ".ovpn"
	}
	return m.AddConfig(fname, data)
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
	os.Remove(filepath.Join(m.dataDir, "configs", t.ConfigFile))

	m.mu.Lock()
	m.tunnels = append(m.tunnels[:index], m.tunnels[index+1:]...)
	for i, tt := range m.tunnels {
		tt.Index = i
		tt.TunDev = fmt.Sprintf("tun%d", i)
	}
	m.mu.Unlock()

	m.saveTunnels()
	m.state.AddLog("warn", fmt.Sprintf("[%s] config deleted", t.TunDev))
	return nil
}

// ReadConfig returns the raw .ovpn file content
func (m *VPNManager) ReadConfig(index int) (string, error) {
	m.mu.RLock()
	if index < 0 || index >= len(m.tunnels) {
		m.mu.RUnlock()
		return "", fmt.Errorf("invalid index")
	}
	cfgFile := m.tunnels[index].ConfigFile
	m.mu.RUnlock()
	data, err := os.ReadFile(filepath.Join(m.dataDir, "configs", cfgFile))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// WriteConfig saves edited .ovpn content
func (m *VPNManager) WriteConfig(index int, content string) error {
	m.mu.RLock()
	if index < 0 || index >= len(m.tunnels) {
		m.mu.RUnlock()
		return fmt.Errorf("invalid index")
	}
	cfgFile := m.tunnels[index].ConfigFile
	tunDev := m.tunnels[index].TunDev
	m.mu.RUnlock()
	if err := os.WriteFile(filepath.Join(m.dataDir, "configs", cfgFile), []byte(content), 0600); err != nil {
		return err
	}
	m.state.AddLog("ok", fmt.Sprintf("[%s] config edited and saved", tunDev))
	return nil
}

// UpdateTunnelSettings updates per-tunnel settings (protocol, compression, group, note, bandwidth)
func (m *VPNManager) UpdateTunnelSettings(index int, cfg TunnelCfg) error {
	m.mu.Lock()
	if index < 0 || index >= len(m.tunnels) {
		m.mu.Unlock()
		return fmt.Errorf("invalid index")
	}
	t := m.tunnels[index]
	t.mu.Lock()
	oldProto := t.Cfg.Protocol
	t.Cfg = cfg
	t.mu.Unlock()
	m.mu.Unlock()

	if oldProto != cfg.Protocol {
		m.state.AddLog("ok", fmt.Sprintf("[%s] protocol changed %s→%s", t.TunDev, oldProto, cfg.Protocol))
	}
	m.saveTunnels()
	return nil
}

func (m *VPNManager) SetAutoReconnect(index int, enabled bool) error {
	m.mu.Lock()
	if index < 0 || index >= len(m.tunnels) {
		m.mu.Unlock()
		return fmt.Errorf("invalid index")
	}
	m.tunnels[index].AutoReconnect = enabled
	m.mu.Unlock()
	m.saveTunnels()
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
	go m.runTunnel(t)
	return nil
}

func (m *VPNManager) StopTunnel(index int) error {
	m.mu.RLock()
	if index < 0 || index >= len(m.tunnels) {
		m.mu.RUnlock()
		return fmt.Errorf("invalid index")
	}
	t := m.tunnels[index]
	m.mu.RUnlock()

	t.mu.Lock()
	select {
	case <-t.stopCh:
	default:
		close(t.stopCh)
	}
	t.mu.Unlock()

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

	m.state.AddLog("warn", fmt.Sprintf("[%s] stopped by user", t.TunDev))
	m.saveTunnels()
	return nil
}

func (m *VPNManager) RestartTunnel(index int) error {
	m.StopTunnel(index)
	time.Sleep(1 * time.Second)
	return m.StartTunnel(index)
}

func (m *VPNManager) RestartAll() {
	m.mu.RLock()
	idxs := make([]int, 0, len(m.tunnels))
	for _, t := range m.tunnels {
		idxs = append(idxs, t.Index)
	}
	m.mu.RUnlock()
	for _, idx := range idxs {
		m.RestartTunnel(idx)
		time.Sleep(200 * time.Millisecond)
	}
}

func (m *VPNManager) RestoreConnections() {
	m.mu.RLock()
	tuns := make([]*Tunnel, len(m.tunnels))
	copy(tuns, m.tunnels)
	m.mu.RUnlock()
	for _, t := range tuns {
		if t.AutoReconnect {
			m.state.AddLog("warn", fmt.Sprintf("[%s] restoring after restart", t.TunDev))
			go m.runTunnel(t)
		}
	}
}

func (m *VPNManager) runTunnel(t *Tunnel) {
	t.mu.Lock()
	if t.Status == StatusConnected || t.Status == StatusConnecting {
		t.mu.Unlock()
		return
	}
	t.Status = StatusConnecting
	t.stopCh = make(chan struct{})
	t.mu.Unlock()

	m.state.AddLog("warn", fmt.Sprintf("[%s] connecting — %s", t.TunDev, t.ConfigFile))

	for {
		t.mu.Lock()
		proto := t.Cfg.Protocol
		comp := t.Cfg.Compression
		udpFails := t.Perf.UDPFails
		t.mu.Unlock()

		// Auto-switch UDP→TCP after 3 failures
		settings := m.state.GetSettings()
		if settings.AutoRestart.AutoSwitchTCP && proto == "udp" && udpFails >= 3 {
			proto = "tcp"
			m.state.AddLog("warn", fmt.Sprintf("[%s] UDP failed %dx — switching to TCP", t.TunDev, udpFails))
		}

		cfgPath := filepath.Join(m.dataDir, "configs", t.ConfigFile)
		args := m.buildArgs(t, cfgPath, proto, comp)
		cmd := exec.Command(m.ovpnBin, args...)
		cmd.Env = append(os.Environ(),
			"LD_LIBRARY_PATH="+m.libDir+":"+os.Getenv("LD_LIBRARY_PATH"),
		)

		t.mu.Lock()
		t.cmd = cmd
		t.mu.Unlock()

		err := cmd.Run()
		if err != nil {
			select {
			case <-t.stopCh:
				t.mu.Lock()
				t.Status = StatusDisconnected
				t.mu.Unlock()
				m.state.AddLog("warn", fmt.Sprintf("[%s] stopped", t.TunDev))
				return
			default:
			}
			// Count UDP failures
			t.mu.Lock()
			if proto == "udp" {
				t.Perf.UDPFails++
			}
			t.mu.Unlock()
			m.state.AddLog("err", fmt.Sprintf("[%s] openvpn exited: %v — retry in 5s", t.TunDev, err))
		} else {
			// Daemon launched
			now := time.Now()
			t.mu.Lock()
			t.Status = StatusConnected
			t.ConnectedAt = &now
			if proto == "tcp" {
				t.Perf.UDPFails = 0
			}
			mtu := t.Perf.MTU
			t.mu.Unlock()

			m.state.AddLog("ok", fmt.Sprintf("[%s] connected — %s (%s)", t.TunDev, t.Name, strings.ToUpper(proto)))
			m.saveTunnels()

			// Post-connect: MTU detection + IP check
			if settings.MTU.Enabled && settings.MTU.DetectOnConnect {
				go m.detectMTU(t.Index)
			}
			if settings.DNS.Enabled && settings.DNS.CheckOnConnect {
				go m.checkExternalIP()
			}
			// Apply MTU if detected
			if settings.MTU.Apply && mtu > 0 {
				exec.Command("ip", "link", "set", t.TunDev, "mtu", strconv.Itoa(mtu)).Run()
			}
			// Apply bandwidth limit
			t.mu.Lock()
			bw := t.Cfg.BandwidthMB
			t.mu.Unlock()
			if bw > 0 {
				rate := fmt.Sprintf("%dmbit", bw*8)
				exec.Command("tc", "qdisc", "add", "dev", t.TunDev,
					"root", "tbf", "rate", rate, "burst", "32kbit", "latency", "400ms").Run()
			}

			m.monitorPid(t)

			select {
			case <-t.stopCh:
				t.mu.Lock()
				t.Status = StatusDisconnected
				t.ConnectedAt = nil
				t.mu.Unlock()
				m.state.AddLog("warn", fmt.Sprintf("[%s] stopped", t.TunDev))
				m.saveTunnels()
				return
			default:
			}

			t.mu.Lock()
			t.Status = StatusReconnecting
			t.ConnectedAt = nil
			t.mu.Unlock()
			m.state.AddLog("err", fmt.Sprintf("[%s] connection lost — reconnecting in 5s", t.TunDev))
			m.saveTunnels()
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

func (m *VPNManager) buildArgs(t *Tunnel, cfgPath, proto, comp string) []string {
	args := []string{
		"--config", cfgPath,
		"--dev", t.TunDev,
		"--dev-type", "tun",
		"--proto", proto,
		"--daemon",
		"--log", filepath.Join(m.dataDir, t.TunDev+".log"),
		"--writepid", filepath.Join(m.dataDir, t.TunDev+".pid"),
		"--script-security", "2",
	}
	switch comp {
	case "lz4-v2":
		args = append(args, "--compress", "lz4-v2")
	case "stub":
		args = append(args, "--compress", "stub")
	}
	return args
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
				return
			}
			pid := strings.TrimSpace(string(data))
			if pid == "" {
				return
			}
			if _, err := os.Stat("/proc/" + pid); os.IsNotExist(err) {
				return
			}
		}
	}
}

// ── Traffic ───────────────────────────────────────────────────────────────────

func (m *VPNManager) UpdateTraffic() {
	m.mu.RLock()
	tuns := make([]*Tunnel, len(m.tunnels))
	copy(tuns, m.tunnels)
	m.mu.RUnlock()

	for _, t := range tuns {
		rx, tx := readNetStat(t.TunDev)
		now := time.Now()
		t.mu.Lock()
		if !t.prevTime.IsZero() {
			elapsed := now.Sub(t.prevTime).Seconds()
			if elapsed > 0 {
				t.stats.RxRate = float64(rx-t.prevRx) / elapsed
				t.stats.TxRate = float64(tx-t.prevTx) / elapsed
				// Record daily traffic delta
				deltaRx := rx - t.prevRx
				deltaTx := tx - t.prevTx
				if deltaRx > 0 || deltaTx > 0 {
					go m.state.AddTraffic(t.TunDev, deltaRx, deltaTx)
				}
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

func readNetStat(iface string) (rx, tx uint64) {
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

// ── Getters ───────────────────────────────────────────────────────────────────

func (m *VPNManager) GetTunnels() []TunnelInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]TunnelInfo, len(m.tunnels))
	for i, t := range m.tunnels {
		out[i] = m.tunnelInfo(t)
	}
	return out
}

func (m *VPNManager) tunnelInfo(t *Tunnel) TunnelInfo {
	t.mu.Lock()
	defer t.mu.Unlock()
	return TunnelInfo{
		Index:         t.Index,
		Name:          t.Name,
		ConfigFile:    t.ConfigFile,
		TunDev:        t.TunDev,
		Status:        t.Status,
		AutoReconnect: t.AutoReconnect,
		ConnectedAt:   t.ConnectedAt,
		Cfg:           t.Cfg,
		Perf:          t.Perf,
		RxRate:        t.stats.RxRate,
		TxRate:        t.stats.TxRate,
		RxBytes:       t.stats.RxBytes,
		TxBytes:       t.stats.TxBytes,
	}
}

func (m *VPNManager) CountActive() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n := 0
	for _, t := range m.tunnels {
		t.mu.Lock()
		if t.Status == StatusConnected {
			n++
		}
		t.mu.Unlock()
	}
	return n
}

func (m *VPNManager) addLog(level, msg string) {
	m.state.AddLog(level, msg)
}

// detectMTU runs a binary search for the optimal MTU on the given tunnel
func (m *VPNManager) detectMTU(index int) {
	m.mu.RLock()
	if index < 0 || index >= len(m.tunnels) {
		m.mu.RUnlock()
		return
	}
	t := m.tunnels[index]
	m.mu.RUnlock()

	settings := m.state.GetSettings()
	if !settings.MTU.Enabled {
		return
	}
	target := settings.MTU.PingTarget
	if target == "" {
		target = "8.8.8.8"
	}

	m.state.AddLog("info", fmt.Sprintf("[%s] detecting MTU…", t.TunDev))
	mtu := binarySearchMTU(t.TunDev, target)
	if mtu <= 0 {
		m.state.AddLog("warn", fmt.Sprintf("[%s] MTU detection failed", t.TunDev))
		return
	}

	t.mu.Lock()
	t.Perf.MTU = mtu
	t.mu.Unlock()
	m.saveTunnels()
	m.state.AddLog("ok", fmt.Sprintf("[%s] MTU detected: %d", t.TunDev, mtu))

	if settings.MTU.Apply {
		exec.Command("ip", "link", "set", t.TunDev, "mtu", strconv.Itoa(mtu)).Run()
		m.state.AddLog("ok", fmt.Sprintf("[%s] MTU %d applied", t.TunDev, mtu))
	}
}

func binarySearchMTU(iface, target string) int {
	lo, hi := 576, 1500
	best := 0
	for lo <= hi {
		mid := (lo + hi) / 2
		// ping with DF bit set, payload size = mid - IP(20) - ICMP(8)
		payloadSize := mid - 28
		if payloadSize < 1 {
			payloadSize = 1
		}
		args := []string{"-c", "1", "-W", "2", "-M", "do", "-s", strconv.Itoa(payloadSize)}
		if iface != "" {
			args = append(args, "-I", iface)
		}
		args = append(args, target)
		err := exec.Command("ping", args...).Run()
		if err == nil {
			best = mid
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	return best
}

// RunSpeedTest tests download speed through a tunnel
func (m *VPNManager) RunSpeedTest(index int) (float64, error) {
	m.mu.RLock()
	if index < 0 || index >= len(m.tunnels) {
		m.mu.RUnlock()
		return 0, fmt.Errorf("invalid index")
	}
	t := m.tunnels[index]
	m.mu.RUnlock()

	t.mu.Lock()
	status := t.Status
	tunDev := t.TunDev
	t.mu.Unlock()

	if status != StatusConnected {
		return 0, fmt.Errorf("tunnel not connected")
	}

	settings := m.state.GetSettings()
	url := settings.SpeedTest.URL
	if url == "" {
		url = "https://speed.cloudflare.com/__down"
	}
	sizeMB := settings.SpeedTest.SizeMB
	if sizeMB <= 0 {
		sizeMB = 5
	}

	m.state.AddLog("info", fmt.Sprintf("[%s] speed test started…", tunDev))

	args := []string{
		"--interface", tunDev,
		"-s", "-o", "/dev/null",
		"--max-time", "60",
		"-w", "%{speed_download}",
		fmt.Sprintf("%s?bytes=%d", url, sizeMB*1024*1024),
	}
	out, err := exec.Command("curl", args...).Output()
	if err != nil {
		// Fallback: try without interface binding
		args2 := []string{"-s", "-o", "/dev/null", "--max-time", "30",
			"-w", "%{speed_download}",
			fmt.Sprintf("%s?bytes=%d", url, sizeMB*1024*1024)}
		out, err = exec.Command("curl", args2...).Output()
		if err != nil {
			m.state.AddLog("warn", fmt.Sprintf("[%s] speed test failed: %v", tunDev, err))
			return 0, err
		}
	}

	speedBytesPerSec, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	speedMbps := (speedBytesPerSec * 8) / 1e6

	now := time.Now()
	t.mu.Lock()
	t.Perf.SpeedMbps = speedMbps
	t.Perf.SpeedAt = &now
	t.mu.Unlock()
	m.saveTunnels()

	m.state.AddLog("ok", fmt.Sprintf("[%s] speed test: %.1f Mbps", tunDev, speedMbps))
	return speedMbps, nil
}

// RunPingCheck measures ping and packet loss through the tunnel
func (m *VPNManager) RunPingCheck(index int) {
	m.mu.RLock()
	if index < 0 || index >= len(m.tunnels) {
		m.mu.RUnlock()
		return
	}
	t := m.tunnels[index]
	m.mu.RUnlock()

	t.mu.Lock()
	status := t.Status
	tunDev := t.TunDev
	count := 4
	t.mu.Unlock()

	if status != StatusConnected {
		return
	}

	settings := m.state.GetSettings()
	if settings.Ping.Count > 0 {
		count = settings.Ping.Count
	}

	out, err := exec.Command("ping",
		"-I", tunDev,
		"-c", strconv.Itoa(count),
		"-W", "3",
		"-q",
		"8.8.8.8",
	).CombinedOutput()

	if err != nil {
		// Try without interface
		out, err = exec.Command("ping",
			"-c", strconv.Itoa(count),
			"-W", "3", "-q",
			"8.8.8.8",
		).CombinedOutput()
	}

	result := string(out)
	pingMs, loss := parsePingOutput(result)

	t.mu.Lock()
	t.Perf.PingMs = pingMs
	t.Perf.PacketLoss = loss
	t.mu.Unlock()
	m.saveTunnels()
}

func parsePingOutput(output string) (pingMs, loss float64) {
	for _, line := range strings.Split(output, "\n") {
		// packet loss: "4 packets transmitted, 4 received, 0% packet loss"
		if strings.Contains(line, "packet loss") {
			parts := strings.Fields(line)
			for i, p := range parts {
				if p == "loss," || p == "loss" {
					if i > 0 {
						ls := strings.TrimSuffix(parts[i-1], "%")
						loss, _ = strconv.ParseFloat(ls, 64)
					}
				}
			}
		}
		// rtt: "rtt min/avg/max/mdev = 12.3/14.5/16.7/1.2 ms"
		if strings.Contains(line, "rtt min/avg") {
			parts := strings.Split(line, "=")
			if len(parts) == 2 {
				vals := strings.Split(strings.TrimSpace(parts[1]), "/")
				if len(vals) >= 2 {
					pingMs, _ = strconv.ParseFloat(vals[1], 64) // avg
				}
			}
		}
	}
	return
}

func (m *VPNManager) checkExternalIP() {
	settings := m.state.GetSettings()
	url := settings.DNS.IPCheckURL
	if url == "" {
		url = "https://api.ipify.org"
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
	ip := strings.TrimSpace(string(data))
	if ip != "" {
		m.state.SetExternalIP(ip)
		m.state.AddLog("info", fmt.Sprintf("[system] external IP: %s", ip))
	}
}
