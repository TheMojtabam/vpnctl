package main

import (
	"embed"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
)

//go:embed static/index.html
var staticFiles embed.FS

func main() {
	port := flag.Int("port", 8080, "Web server port")
	dataDir := flag.String("data", "/etc/vpnctl", "Directory for configs and state")
	flag.Parse()

	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "Warning: vpnctl should run as root to manage OpenVPN tunnels")
	}

	for _, d := range []string{*dataDir, *dataDir + "/configs"} {
		if err := os.MkdirAll(d, 0755); err != nil {
			log.Fatalf("Cannot create directory %s: %v", d, err)
		}
	}

	manager := NewVPNManager(*dataDir)
	manager.LoadState()
	manager.RestoreConnections()

	cronMgr := NewCronManager(manager)
	cronMgr.LoadState()
	cronMgr.Start()

	srv := NewWebServer(manager, cronMgr)

	addr := fmt.Sprintf(":%d", *port)
	log.Printf("vpnctl listening on http://0.0.0.0%s  (data: %s)", addr, *dataDir)

	if err := http.ListenAndServe(addr, srv.Router()); err != nil {
		log.Fatalf("Server: %v", err)
	}
}
