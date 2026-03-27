package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
)

func main() {
	port    := flag.Int("port", 8080, "Web server port")
	dataDir := flag.String("data", "/etc/vpnctl", "Data directory for configs and state")
	flag.Parse()

	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "Warning: vpnctl should run as root to manage OpenVPN tunnels and iptables")
	}

	// Create required directories
	for _, d := range []string{*dataDir, *dataDir + "/configs"} {
		if err := os.MkdirAll(d, 0755); err != nil {
			log.Fatalf("mkdir %s: %v", d, err)
		}
	}

	// Extract embedded OpenVPN if needed
	ovpnBin, libDir := EnsureOpenVPN(*dataDir)

	// Core state
	state := NewAppState(*dataDir)
	state.Load()

	// VPN manager
	vpn := NewVPNManager(*dataDir, ovpnBin, libDir, state)
	vpn.LoadTunnels()

	// Monitoring
	mon := NewMonitor(vpn, state)
	mon.Start()

	// Kill switch
	ks := NewKillSwitch(state)
	// Re-arm if it was enabled in settings
	if state.GetSettings().KillSwitch.Enabled {
		if err := ks.Arm(state.GetSettings().KillSwitch.AllowLAN); err != nil {
			log.Printf("Kill switch arm: %v", err)
		}
	}

	// Cron
	cron := NewCronManager(vpn, state)
	cron.Load()
	cron.Start()

	// Restore connections
	vpn.RestoreConnections()

	// Initial IP check
	go vpn.checkExternalIP()

	// Web server
	srv := NewWebServer(vpn, cron, mon, ks, state, *dataDir)

	addr := fmt.Sprintf(":%d", *port)
	log.Printf("vpnctl listening on http://0.0.0.0%s  (data: %s)", addr, *dataDir)
	log.Printf("OpenVPN binary: %s", ovpnBin)

	if err := http.ListenAndServe(addr, srv.Router()); err != nil {
		log.Fatalf("server: %v", err)
	}
}
