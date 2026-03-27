package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type Monitor struct {
	vpn   *VPNManager
	state *AppState
}

func NewMonitor(vpn *VPNManager, state *AppState) *Monitor {
	return &Monitor{vpn: vpn, state: state}
}

func (m *Monitor) Start() {
	go m.pingLoop()
	go m.autoRestartLoop()
	go m.trafficLoop()
	go m.speedIntervalLoop()
}

// trafficLoop updates rx/tx rates every 2 seconds
func (m *Monitor) trafficLoop() {
	ticker := time.NewTicker(2 * time.Second)
	for range ticker.C {
		m.vpn.UpdateTraffic()
	}
}

// pingLoop pings all connected tunnels periodically
func (m *Monitor) pingLoop() {
	for {
		settings := m.state.GetSettings()
		interval := settings.Ping.Interval
		if interval <= 0 {
			interval = 30
		}
		time.Sleep(time.Duration(interval) * time.Second)

		if !settings.Ping.Enabled {
			continue
		}
		for _, t := range m.vpn.GetTunnels() {
			if t.Status == StatusConnected {
				go m.vpn.RunPingCheck(t.Index)
			}
		}
	}
}

// autoRestartLoop checks speed results and restarts slow tunnels
func (m *Monitor) autoRestartLoop() {
	ticker := time.NewTicker(60 * time.Second)
	for range ticker.C {
		settings := m.state.GetSettings()
		if !settings.AutoRestart.Enabled {
			continue
		}
		for _, t := range m.vpn.GetTunnels() {
			if t.Status != StatusConnected {
				continue
			}
			if t.Perf.SpeedAt == nil {
				continue
			}
			// Only act on recent speed tests (< 10 minutes old)
			if time.Since(*t.Perf.SpeedAt) > 10*time.Minute {
				continue
			}
			if t.Perf.SpeedMbps > 0 && t.Perf.SpeedMbps < float64(settings.AutoRestart.ThresholdMbps) {
				m.handleSlowTunnel(t, settings.AutoRestart)
			}
		}
	}
}

func (m *Monitor) handleSlowTunnel(t TunnelInfo, cfg AutoRestartCfg) {
	now := time.Now()
	hour := now.Hour()

	// Check restart limit via state
	allowRestart := false
	m.state.mu.Lock()
	for i := range m.state.sf.Tunnels {
		if m.state.sf.Tunnels[i].Index != t.Index {
			continue
		}
		pt := &m.state.sf.Tunnels[i]
		if pt.Perf.RestartHour != hour {
			pt.Perf.RestartHour = hour
			pt.Perf.RestartCount = 0
		}
		if cfg.MaxPerHour == 0 || pt.Perf.RestartCount < cfg.MaxPerHour {
			pt.Perf.RestartCount++
			allowRestart = true
		}
		break
	}
	m.state.mu.Unlock()
	if allowRestart {
		go m.state.Save()
	}

	if !allowRestart {
		return
	}

	m.state.AddLog("warn", fmt.Sprintf("[%s] speed %.1f Mbps < %d Mbps threshold — auto-restarting (%d/%d)",
		t.TunDev, t.Perf.SpeedMbps, cfg.ThresholdMbps, t.Perf.RestartCount, cfg.MaxPerHour))

	m.vpn.RestartTunnel(t.Index)
	m.sendWebhook("slow_tunnel", t.TunDev, t.Name,
		fmt.Sprintf("%s speed %.1f Mbps is below threshold %d Mbps — restarted",
			t.TunDev, t.Perf.SpeedMbps, cfg.ThresholdMbps))
}

// speedIntervalLoop runs speed tests at the configured interval
func (m *Monitor) speedIntervalLoop() {
	for {
		settings := m.state.GetSettings()
		interval := settings.SpeedTest.Interval
		if !settings.SpeedTest.Enabled || interval <= 0 {
			time.Sleep(60 * time.Second)
			continue
		}
		time.Sleep(time.Duration(interval) * time.Minute)
		for _, t := range m.vpn.GetTunnels() {
			if t.Status == StatusConnected {
				go m.vpn.RunSpeedTest(t.Index)
			}
		}
	}
}

// RunSpeedTestAll runs speed tests on all connected tunnels
func (m *Monitor) RunSpeedTestAll() {
	for _, t := range m.vpn.GetTunnels() {
		if t.Status == StatusConnected {
			go m.vpn.RunSpeedTest(t.Index)
		}
	}
}

// RunMTUAll runs MTU detection on all connected tunnels
func (m *Monitor) RunMTUAll() {
	for _, t := range m.vpn.GetTunnels() {
		if t.Status == StatusConnected {
			go m.vpn.detectMTU(t.Index)
		}
	}
}

// ── Webhook ───────────────────────────────────────────────────────────────────

type WebhookPayload struct {
	Event     string `json:"event"`
	TunDev    string `json:"tun_dev"`
	TunName   string `json:"tun_name"`
	Message   string `json:"message"`
	Timestamp string `json:"timestamp"`
}

func (m *Monitor) sendWebhook(event, tunDev, tunName, message string) {
	settings := m.state.GetSettings()
	if !settings.Webhook.Enabled || settings.Webhook.URL == "" {
		return
	}
	payload := WebhookPayload{
		Event:     event,
		TunDev:    tunDev,
		TunName:   tunName,
		Message:   message,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	data, _ := json.Marshal(payload)
	go func() {
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Post(settings.Webhook.URL, "application/json", bytes.NewReader(data))
		if err != nil {
			m.state.AddLog("warn", fmt.Sprintf("[webhook] POST failed: %v", err))
			return
		}
		resp.Body.Close()
		m.state.AddLog("info", fmt.Sprintf("[webhook] POST %d", resp.StatusCode))
	}()
}

func (m *Monitor) TestWebhook() {
	m.sendWebhook("test", "—", "—", "vpnctl webhook test")
}
