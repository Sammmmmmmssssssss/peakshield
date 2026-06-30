# PeakShield Performance & Tuning Guide

Out of the box, PeakShield is highly performant. However, pushing past 10,000+ concurrent connections requires tuning both the host operating system and the PeakShield configuration.

## OS Level Tuning (Linux)

When dealing with massive connection concurrency, Linux defaults will throttle you with "Too many open files" or TCP port exhaustion.

Edit `/etc/sysctl.conf` and apply these values (`sysctl -p`):

```ini
# Increase max connections backlog
net.core.somaxconn = 65535

# Increase ephemeral port range for upstream connections
net.ipv4.ip_local_port_range = 1024 65535

# Reuse TCP sockets in TIME_WAIT state (crucial for reverse proxies)
net.ipv4.tcp_tw_reuse = 1

# Max number of open file descriptors
fs.file-max = 2097152
```

In your `systemd` unit file, ensure you have set:
```ini
LimitNOFILE=1048576
```

## Go Runtime Tuning

PeakShield is written in Go. The standard garbage collector works well, but you can tweak it:

- `GOMAXPROCS`: By default, Go uses all available CPU cores. If you are running PeakShield alongside other heavy processes (like the legacy backend itself) on the same machine, you might want to restrict it: `export GOMAXPROCS=4`.
- `GOGC`: To reduce CPU usage spent on garbage collection (at the cost of using slightly more RAM), you can increase the GC target percentage: `export GOGC=200` (Default is 100). Given PeakShield uses <30MB of RAM, bumping this to 200 or 500 is highly recommended.

## PeakShield Configuration Strategy

How you configure PeakShield depends entirely on your upstream backend's capacity.

### The Equation
`MaxConcurrent` should be STRICTLY LESS THAN or EQUAL TO what your backend can handle before crashing.

**Scenario A: Fragile Backend (Can only handle 50 concurrent requests)**
- `PEAKSHIELD_MAX_CONCURRENT=50`
- `PEAKSHIELD_WAITING_ROOM_SIZE=10000` (Hold everyone else in the HTML waiting room queue)
- `PEAKSHIELD_RATE_LIMIT=10` (Strict limits per IP)

**Scenario B: Robust Backend (Can handle 5,000 concurrent requests)**
- `PEAKSHIELD_MAX_CONCURRENT=5000`
- `PEAKSHIELD_WAITING_ROOM_SIZE=20000` 
- `PEAKSHIELD_RATE_LIMIT=100` 

## Benchmarking Against Our Claims

To verify the "50,000 requests per second with <30MB RAM" claim on your own hardware:

1. Start PeakShield pointing to a fast backend (e.g., a simple Nginx serving static text).
2. Run the included load test script:
   ```bash
   ./scripts/loadtest.sh
   ```
3. Monitor metrics at `http://localhost:9090/metrics` specifically looking at `peakshield_alloc_bytes`.
