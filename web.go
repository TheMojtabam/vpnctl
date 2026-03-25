package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type WebServer struct {
	vpn  *VPNManager
	cron *CronManager
}

func NewWebServer(vpn *VPNManager, cron *CronManager) *WebServer {
	return &WebServer{vpn: vpn, cron: cron}
}

func (s *WebServer) Router() http.Handler {
	mux := http.NewServeMux()

	// UI
	mux.HandleFunc("/", s.handleIndex)

	// Tunnel API
	mux.HandleFunc("/api/tunnels", s.handleTunnels)
	mux.HandleFunc("/api/tunnels/upload", s.handleUpload)
	mux.HandleFunc("/api/tunnels/start", s.handleStart)
	mux.HandleFunc("/api/tunnels/stop", s.handleStop)
	mux.HandleFunc("/api/tunnels/restart", s.handleRestart)
	mux.HandleFunc("/api/tunnels/delete", s.handleDelete)
	mux.HandleFunc("/api/tunnels/autoreconnect", s.handleAutoReconnect)

	// Cron API
	mux.HandleFunc("/api/cron", s.handleCron)
	mux.HandleFunc("/api/cron/add", s.handleCronAdd)
	mux.HandleFunc("/api/cron/delete", s.handleCronDelete)

	// Logs
	mux.HandleFunc("/api/logs", s.handleLogs)

	// Traffic poll (SSE-style long poll)
	mux.HandleFunc("/api/stats", s.handleStats)

	// start background traffic updater
	go s.trafficLoop()

	return mux
}

func (s *WebServer) trafficLoop() {
	t := time.NewTicker(2 * time.Second)
	for range t.C {
		s.vpn.UpdateTraffic()
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func queryInt(r *http.Request, key string) (int, error) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return 0, fmt.Errorf("missing %s", key)
	}
	return strconv.Atoi(v)
}

// ── Handlers ──────────────────────────────────────────────────────────────────

func (s *WebServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	data, err := staticFiles.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "UI not found", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

func (s *WebServer) handleTunnels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.vpn.GetTunnels())
}

func (s *WebServer) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "POST required")
		return
	}
	r.ParseMultipartForm(10 << 20)
	file, header, err := r.FormFile("config")
	if err != nil {
		writeErr(w, 400, "no file in request")
		return
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		writeErr(w, 500, "read error")
		return
	}

	t, err := s.vpn.AddConfig(header.Filename, data)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, t)
}

func (s *WebServer) handleStart(w http.ResponseWriter, r *http.Request) {
	idx, err := queryInt(r, "index")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := s.vpn.StartTunnel(idx); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "starting"})
}

func (s *WebServer) handleStop(w http.ResponseWriter, r *http.Request) {
	idx, err := queryInt(r, "index")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := s.vpn.StopTunnel(idx); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "stopped"})
}

func (s *WebServer) handleRestart(w http.ResponseWriter, r *http.Request) {
	idx, err := queryInt(r, "index")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := s.vpn.RestartTunnel(idx); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "restarting"})
}

func (s *WebServer) handleDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		writeErr(w, 405, "POST or DELETE required")
		return
	}
	idx, err := queryInt(r, "index")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := s.vpn.DeleteConfig(idx); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

func (s *WebServer) handleAutoReconnect(w http.ResponseWriter, r *http.Request) {
	idx, err := queryInt(r, "index")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	enabled := strings.ToLower(r.URL.Query().Get("enabled")) == "true"
	if err := s.vpn.SetAutoReconnect(idx, enabled); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"auto_reconnect": enabled})
}

func (s *WebServer) handleCron(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.cron.GetJobs())
}

func (s *WebServer) handleCronAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "POST required")
		return
	}
	var body struct {
		Expression string `json:"expression"`
		Target     string `json:"target"`
		Action     string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	job, err := s.cron.AddJob(body.Expression, body.Target, body.Action)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, job)
}

func (s *WebServer) handleCronDelete(w http.ResponseWriter, r *http.Request) {
	id, err := queryInt(r, "id")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := s.cron.DeleteJob(id); err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

func (s *WebServer) handleLogs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.vpn.GetLogs())
}

func (s *WebServer) handleStats(w http.ResponseWriter, r *http.Request) {
	tunnels := s.vpn.GetTunnels()

	type StatsResp struct {
		Tunnels []TunnelInfo `json:"tunnels"`
		Logs    []LogEntry   `json:"logs"`
	}

	writeJSON(w, 200, StatsResp{
		Tunnels: tunnels,
		Logs:    s.vpn.GetLogs(),
	})
}
