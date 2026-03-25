# vpnctl

A minimal OpenVPN manager with a web dashboard. Single static binary, no dependencies.

## Features
- Upload and manage multiple `.ovpn` configs (tun0, tun1, tun2, ...)
- Start / Stop / Restart individual tunnels
- Live per-tunnel traffic stats (RX/TX)
- Auto-reconnect on connection drop
- Cron-based scheduled restarts
- Persistent state — configs and settings survive restarts
- Event log

## Build

```
git clone https://github.com/YOU/vpnctl
cd vpnctl
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o vpnctl .
```

Or download the pre-built binary from [GitHub Actions artifacts](../../actions).

## Usage

```bash
# Run (requires root for OpenVPN)
sudo ./vpnctl --port 8080 --data /etc/vpnctl
```

Then open `http://YOUR_SERVER_IP:8080`

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--port` | `8080` | Web UI port |
| `--data` | `/etc/vpnctl` | State + config directory |

### Data directory layout

```
/etc/vpnctl/
├── state.json       # tunnel list, auto-reconnect flags
├── cron.json        # scheduled jobs
├── configs/
│   ├── nl-amsterdam-01.ovpn
│   └── de-frankfurt-03.ovpn
├── tun0.pid         # runtime pid files
├── tun0.log         # OpenVPN logs per tunnel
└── tun1.pid
```

### Run as systemd service

```ini
# /etc/systemd/system/vpnctl.service
[Unit]
Description=vpnctl OpenVPN manager
After=network.target

[Service]
ExecStart=/usr/local/bin/vpnctl --port 8080 --data /etc/vpnctl
Restart=always
User=root

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl enable --now vpnctl
```

## Requirements

- OpenVPN installed (`apt install openvpn`)
- Linux, amd64
- Root privileges
