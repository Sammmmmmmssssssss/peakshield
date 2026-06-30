# PeakShield Deployment Guide

PeakShield compiles down to a single binary with zero dependencies, making it extremely versatile to deploy.

## Option 1: systemd (Bare Metal / VM)
If you are running on a standard Linux server, `systemd` is the recommended way to keep PeakShield running in the background, restart it on crashes, and capture logs.

1. Create a service file at `/etc/systemd/system/peakshield.service`:
```ini
[Unit]
Description=PeakShield Reverse Proxy
After=network.target

[Service]
Type=simple
User=peakshield
Group=peakshield
Restart=on-failure
RestartSec=5s

# Environment Configuration
Environment="PEAKSHIELD_LISTEN=:8080"
Environment="PEAKSHIELD_METRICS_LISTEN=127.0.0.1:9090"
Environment="PEAKSHIELD_TARGET=http://localhost:9090"
Environment="PEAKSHIELD_RATE_LIMIT=1000"
Environment="PEAKSHIELD_BURST_SIZE=200"
Environment="PEAKSHIELD_MAX_CONCURRENT=5000"

ExecStart=/usr/local/bin/peakshield
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
```

2. Reload daemon and start:
```bash
sudo systemctl daemon-reload
sudo systemctl enable peakshield
sudo systemctl start peakshield
```

3. Check logs:
```bash
sudo journalctl -u peakshield -f
```

## Option 2: Docker Swarm

For multi-node cluster deployments without the complexity of Kubernetes, Docker Swarm is a great fit.

1. Create a `docker-stack.yml`:
```yaml
version: '3.8'

services:
  peakshield:
    image: ghcr.io/yourusername/peakshield:latest
    ports:
      - "80:8080"     # Public Proxy
    environment:
      - PEAKSHIELD_LISTEN=:8080
      - PEAKSHIELD_METRICS_LISTEN=0.0.0.0:9090
      - PEAKSHIELD_TARGET=http://legacy-backend:9090
      - PEAKSHIELD_RATE_LIMIT=1000
    deploy:
      replicas: 3
      update_config:
        parallelism: 1
        delay: 10s
      restart_policy:
        condition: on-failure

  prometheus:
    image: prom/prometheus
    ports:
      - "9090:9090"
    volumes:
      - ./prometheus.yml:/etc/prometheus/prometheus.yml
```

2. Deploy the stack:
```bash
docker stack deploy -c docker-stack.yml peakshield_stack
```

## Option 3: Kubernetes (Helm)

We provide a production-ready Helm chart located in `charts/peakshield/`.

1. Install via local chart:
```bash
helm upgrade --install peakshield ./charts/peakshield \
  --set config.targetURL="http://legacy-backend:9090" \
  --set replicaCount=3
```

## Health Checks
- Readiness/Liveness Probe: `GET /__peakshield/stats` (Port 9090)
- Prometheus Scraping: `GET /metrics` (Port 9090)
