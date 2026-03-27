package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type WebServer struct {
	vpn  *VPNManager
	cron *CronManager
	mon  *Monitor
	ks   *KillSwitch
	state *AppState
	dataDir string
}

func NewWebServer(vpn *VPNManager, cron *CronManager, mon *Monitor, ks *KillSwitch, state *AppState, dataDir string) *WebServer {
	return &WebServer{vpn: vpn, cron: cron, mon: mon, ks: ks, state: state, dataDir: dataDir}
}

func (s *WebServer) Router() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/", s.handleIndex)

	// Tunnels
	mux.HandleFunc("/api/tunnels", s.handleTunnels)
	mux.HandleFunc("/api/tunnels/upload", s.handleUpload)
	mux.HandleFunc("/api/tunnels/import", s.handleImport)
	mux.HandleFunc("/api/tunnels/start", s.handleStart)
	mux.HandleFunc("/api/tunnels/stop", s.handleStop)
	mux.HandleFunc("/api/tunnels/restart", s.handleRestart)
	mux.HandleFunc("/api/tunnels/delete", s.handleDelete)
	mux.HandleFunc("/api/tunnels/autoreconnect", s.handleAutoReconnect)
	mux.HandleFunc("/api/tunnels/settings", s.handleTunnelSettings)
	mux.HandleFunc("/api/tunnels/config", s.handleTunnelConfig)
	mux.HandleFunc("/api/tunnels/speedtest", s.handleSpeedTest)
	mux.HandleFunc("/api/tunnels/speedtest/all", s.handleSpeedTestAll)
	mux.HandleFunc("/api/tunnels/mtu", s.handleMTU)
	mux.HandleFunc("/api/tunnels/mtu/all", s.handleMTUAll)

	// Settings
	mux.HandleFunc("/api/settings", s.handleSettings)

	// Cron
	mux.HandleFunc("/api/cron", s.handleCron)
	mux.HandleFunc("/api/cron/add", s.handleCronAdd)
	mux.HandleFunc("/api/cron/delete", s.handleCronDelete)

	// Logs
	mux.HandleFunc("/api/logs", s.handleLogs)
	mux.HandleFunc("/api/logs/clear", s.handleLogsClear)

	// Kill switch
	mux.HandleFunc("/api/killswitch/arm", s.handleKSArm)
	mux.HandleFunc("/api/killswitch/disarm", s.handleKSDisarm)

	// IP check
	mux.HandleFunc("/api/ipcheck", s.handleIPCheck)

	// Webhook test
	mux.HandleFunc("/api/webhook/test", s.handleWebhookTest)

	// Backup/Restore
	mux.HandleFunc("/api/backup", s.handleBackup)
	mux.HandleFunc("/api/restore", s.handleRestore)

	// Groups
	mux.HandleFunc("/api/groups", s.handleGroups)

	// Full stats (poll endpoint)
	mux.HandleFunc("/api/stats", s.handleStats)

	return mux
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func wj(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func werr(w http.ResponseWriter, code int, msg string) {
	wj(w, code, map[string]string{"error": msg})
}

func qint(r *http.Request, key string) (int, error) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return 0, fmt.Errorf("missing %s", key)
	}
	return strconv.Atoi(v)
}

// ── UI ────────────────────────────────────────────────────────────────────────

func (s *WebServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexHTML)
}

// ── Tunnels ───────────────────────────────────────────────────────────────────

func (s *WebServer) handleTunnels(w http.ResponseWriter, r *http.Request) {
	wj(w, 200, s.vpn.GetTunnels())
}

func (s *WebServer) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		werr(w, 405, "POST required"); return
	}
	r.ParseMultipartForm(20 << 20)
	files := r.MultipartForm.File["config"]
	var results []TunnelInfo
	for _, fh := range files {
		f, err := fh.Open()
		if err != nil {
			continue
		}
		data, _ := io.ReadAll(f)
		f.Close()
		t, err := s.vpn.AddConfig(fh.Filename, data)
		if err != nil {
			werr(w, 400, err.Error()); return
		}
		results = append(results, *t)
	}
	wj(w, 200, results)
}

func (s *WebServer) handleImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		werr(w, 405, "POST required"); return
	}
	var body struct{ URL string `json:"url"` }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		werr(w, 400, "invalid JSON"); return
	}
	t, err := s.vpn.ImportFromURL(body.URL)
	if err != nil {
		werr(w, 400, err.Error()); return
	}
	wj(w, 200, t)
}

func (s *WebServer) handleStart(w http.ResponseWriter, r *http.Request) {
	idx, err := qint(r, "index")
	if err != nil { werr(w, 400, err.Error()); return }
	if err := s.vpn.StartTunnel(idx); err != nil { werr(w, 400, err.Error()); return }
	wj(w, 200, map[string]string{"status": "starting"})
}

func (s *WebServer) handleStop(w http.ResponseWriter, r *http.Request) {
	idx, err := qint(r, "index")
	if err != nil { werr(w, 400, err.Error()); return }
	if err := s.vpn.StopTunnel(idx); err != nil { werr(w, 400, err.Error()); return }
	wj(w, 200, map[string]string{"status": "stopped"})
}

func (s *WebServer) handleRestart(w http.ResponseWriter, r *http.Request) {
	idx, err := qint(r, "index")
	if err != nil { werr(w, 400, err.Error()); return }
	go s.vpn.RestartTunnel(idx)
	wj(w, 200, map[string]string{"status": "restarting"})
}

func (s *WebServer) handleDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		werr(w, 405, "POST or DELETE required"); return
	}
	idx, err := qint(r, "index")
	if err != nil { werr(w, 400, err.Error()); return }
	if err := s.vpn.DeleteConfig(idx); err != nil { werr(w, 400, err.Error()); return }
	wj(w, 200, map[string]string{"status": "deleted"})
}

func (s *WebServer) handleAutoReconnect(w http.ResponseWriter, r *http.Request) {
	idx, err := qint(r, "index")
	if err != nil { werr(w, 400, err.Error()); return }
	enabled := strings.ToLower(r.URL.Query().Get("enabled")) == "true"
	if err := s.vpn.SetAutoReconnect(idx, enabled); err != nil { werr(w, 400, err.Error()); return }
	wj(w, 200, map[string]bool{"auto_reconnect": enabled})
}

func (s *WebServer) handleTunnelSettings(w http.ResponseWriter, r *http.Request) {
	idx, err := qint(r, "index")
	if err != nil { werr(w, 400, err.Error()); return }
	if r.Method == http.MethodGet {
		pt, ok := s.state.GetPersistedTunnel(idx)
		if !ok { werr(w, 404, "not found"); return }
		wj(w, 200, pt.Cfg)
		return
	}
	var cfg TunnelCfg
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		werr(w, 400, "invalid JSON"); return
	}
	if err := s.vpn.UpdateTunnelSettings(idx, cfg); err != nil {
		werr(w, 400, err.Error()); return
	}
	wj(w, 200, cfg)
}

func (s *WebServer) handleTunnelConfig(w http.ResponseWriter, r *http.Request) {
	idx, err := qint(r, "index")
	if err != nil { werr(w, 400, err.Error()); return }
	if r.Method == http.MethodGet {
		content, err := s.vpn.ReadConfig(idx)
		if err != nil { werr(w, 500, err.Error()); return }
		wj(w, 200, map[string]string{"content": content})
		return
	}
	var body struct {
		Content string `json:"content"`
		Note    string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		werr(w, 400, "invalid JSON"); return
	}
	if err := s.vpn.WriteConfig(idx, body.Content); err != nil {
		werr(w, 500, err.Error()); return
	}
	if body.Note != "" {
		pt, ok := s.state.GetPersistedTunnel(idx)
		if ok {
			cfg := pt.Cfg
			cfg.Note = body.Note
			s.vpn.UpdateTunnelSettings(idx, cfg)
		}
	}
	wj(w, 200, map[string]string{"status": "saved"})
}

func (s *WebServer) handleSpeedTest(w http.ResponseWriter, r *http.Request) {
	idx, err := qint(r, "index")
	if err != nil { werr(w, 400, err.Error()); return }
	go s.vpn.RunSpeedTest(idx)
	wj(w, 200, map[string]string{"status": "started"})
}

func (s *WebServer) handleSpeedTestAll(w http.ResponseWriter, r *http.Request) {
	go s.mon.RunSpeedTestAll()
	wj(w, 200, map[string]string{"status": "started"})
}

func (s *WebServer) handleMTU(w http.ResponseWriter, r *http.Request) {
	idx, err := qint(r, "index")
	if err != nil { werr(w, 400, err.Error()); return }
	go s.vpn.detectMTU(idx)
	wj(w, 200, map[string]string{"status": "started"})
}

func (s *WebServer) handleMTUAll(w http.ResponseWriter, r *http.Request) {
	go s.mon.RunMTUAll()
	wj(w, 200, map[string]string{"status": "started"})
}

// ── Settings ──────────────────────────────────────────────────────────────────

func (s *WebServer) handleSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		wj(w, 200, s.state.GetSettings()); return
	}
	var cfg Settings
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		werr(w, 400, "invalid JSON"); return
	}
	s.state.SetSettings(cfg)

	// Apply kill switch change
	if cfg.KillSwitch.Enabled && !s.ks.IsArmed() {
		s.ks.Arm(cfg.KillSwitch.AllowLAN)
	} else if !cfg.KillSwitch.Enabled && s.ks.IsArmed() {
		s.ks.Disarm()
	}

	wj(w, 200, cfg)
}

// ── Cron ──────────────────────────────────────────────────────────────────────

func (s *WebServer) handleCron(w http.ResponseWriter, r *http.Request) {
	wj(w, 200, s.cron.GetJobs())
}

func (s *WebServer) handleCronAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { werr(w, 405, "POST required"); return }
	var body struct {
		Expression string `json:"expression"`
		Target     string `json:"target"`
		Action     string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		werr(w, 400, "invalid JSON"); return
	}
	job, err := s.cron.AddJob(body.Expression, body.Target, body.Action)
	if err != nil { werr(w, 400, err.Error()); return }
	wj(w, 200, job)
}

func (s *WebServer) handleCronDelete(w http.ResponseWriter, r *http.Request) {
	id, err := qint(r, "id")
	if err != nil { werr(w, 400, err.Error()); return }
	if err := s.cron.DeleteJob(id); err != nil { werr(w, 404, err.Error()); return }
	wj(w, 200, map[string]string{"status": "deleted"})
}

// ── Logs ──────────────────────────────────────────────────────────────────────

func (s *WebServer) handleLogs(w http.ResponseWriter, r *http.Request) {
	wj(w, 200, s.state.GetLogs())
}

func (s *WebServer) handleLogsClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { werr(w, 405, "POST required"); return }
	s.state.ClearLogs()
	wj(w, 200, map[string]string{"status": "cleared"})
}

// ── Kill Switch ───────────────────────────────────────────────────────────────

func (s *WebServer) handleKSArm(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { werr(w, 405, "POST required"); return }
	settings := s.state.GetSettings()
	if err := s.ks.Arm(settings.KillSwitch.AllowLAN); err != nil {
		werr(w, 500, err.Error()); return
	}
	wj(w, 200, map[string]bool{"armed": true})
}

func (s *WebServer) handleKSDisarm(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { werr(w, 405, "POST required"); return }
	if err := s.ks.Disarm(); err != nil {
		werr(w, 500, err.Error()); return
	}
	wj(w, 200, map[string]bool{"armed": false})
}

// ── IP Check ──────────────────────────────────────────────────────────────────

func (s *WebServer) handleIPCheck(w http.ResponseWriter, r *http.Request) {
	go s.vpn.checkExternalIP()
	wj(w, 200, map[string]string{
		"status": "checking",
		"current_ip": s.state.GetExternalIP(),
	})
}

// ── Webhook test ──────────────────────────────────────────────────────────────

func (s *WebServer) handleWebhookTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { werr(w, 405, "POST required"); return }
	go s.mon.TestWebhook()
	wj(w, 200, map[string]string{"status": "sent"})
}

// ── Groups ────────────────────────────────────────────────────────────────────

func (s *WebServer) handleGroups(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		wj(w, 200, s.state.GetGroups()); return
	}
	var groups []string
	if err := json.NewDecoder(r.Body).Decode(&groups); err != nil {
		werr(w, 400, "invalid JSON"); return
	}
	s.state.SetGroups(groups)
	wj(w, 200, groups)
}

// ── Backup / Restore ──────────────────────────────────────────────────────────

func (s *WebServer) handleBackup(w http.ResponseWriter, r *http.Request) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	// state.json
	stateData, _ := os.ReadFile(filepath.Join(s.dataDir, "state.json"))
	if f, err := zw.Create("state.json"); err == nil {
		f.Write(stateData)
	}

	// configs/*.ovpn
	entries, _ := os.ReadDir(filepath.Join(s.dataDir, "configs"))
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".ovpn") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dataDir, "configs", entry.Name()))
		if err != nil {
			continue
		}
		if f, err := zw.Create("configs/" + entry.Name()); err == nil {
			f.Write(data)
		}
	}
	zw.Close()

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(
		"attachment; filename=\"vpnctl-backup-%s.zip\"",
		time.Now().Format("2006-01-02")))
	w.Write(buf.Bytes())
}

func (s *WebServer) handleRestore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { werr(w, 405, "POST required"); return }
	r.ParseMultipartForm(50 << 20)
	file, _, err := r.FormFile("backup")
	if err != nil { werr(w, 400, "no file"); return }
	defer file.Close()

	data, _ := io.ReadAll(file)
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil { werr(w, 400, "invalid zip"); return }

	for _, f := range zr.File {
		if f.Name == "state.json" {
			rc, _ := f.Open()
			content, _ := io.ReadAll(rc)
			rc.Close()
			os.WriteFile(filepath.Join(s.dataDir, "state.json"), content, 0644)
		}
		if strings.HasPrefix(f.Name, "configs/") && strings.HasSuffix(f.Name, ".ovpn") {
			rc, _ := f.Open()
			content, _ := io.ReadAll(rc)
			rc.Close()
			base := filepath.Base(f.Name)
			os.WriteFile(filepath.Join(s.dataDir, "configs", base), content, 0600)
		}
	}
	s.state.Load()
	wj(w, 200, map[string]string{"status": "restored — restart vpnctl to apply"})
}

// ── Stats (main poll) ─────────────────────────────────────────────────────────

type StatsResponse struct {
	Tunnels        []TunnelInfo `json:"tunnels"`
	Logs           []LogEntry   `json:"logs"`
	Settings       Settings     `json:"settings"`
	CronJobs       []CronJob    `json:"cron_jobs"`
	TrafficHistory []TrafficDay `json:"traffic_history"`
	StartedAt      time.Time    `json:"started_at"`
	ExternalIP     string       `json:"external_ip"`
	KillSwitch     bool         `json:"kill_switch_armed"`
	Groups         []string     `json:"groups"`
}

func (s *WebServer) handleStats(w http.ResponseWriter, r *http.Request) {
	wj(w, 200, StatsResponse{
		Tunnels:        s.vpn.GetTunnels(),
		Logs:           s.state.GetLogs(),
		Settings:       s.state.GetSettings(),
		CronJobs:       s.cron.GetJobs(),
		TrafficHistory: s.state.GetTrafficHistory(),
		StartedAt:      s.state.GetStartedAt(),
		ExternalIP:     s.state.GetExternalIP(),
		KillSwitch:     s.ks.IsArmed(),
		Groups:         s.state.GetGroups(),
	})
}
