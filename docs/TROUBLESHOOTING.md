# Troubleshooting & Operational Runbook

This guide helps on-call engineers diagnose and resolve issues with PeakShield and the downstream backend.

## Diagnosing via Metrics (Port 9090)

Start your investigation by hitting `curl http://localhost:9090/__peakshield/stats`.

### Scenario 1: High `queue_depth`
**Symptom:** Users are seeing the HTML waiting room screen. `queue_depth` is growing rapidly.
**Diagnosis:** The upstream backend cannot process requests fast enough. PeakShield is protecting it by holding excess requests in the queue.
**Action:**
1. Check backend health. Is the database locked? Is CPU maxed?
2. DO NOT increase `PEAKSHIELD_MAX_CONCURRENT` unless you have scaled the backend horizontally, otherwise, you will crash the backend.

### Scenario 2: High `goroutines`, Low `queue_depth`
**Symptom:** PeakShield memory and goroutines are spiking, but nobody is in the queue.
**Diagnosis:** The backend is holding connections open for too long (Slowloris, or extremely slow DB queries).
**Action:** 
1. Decrease `PEAKSHIELD_BACKEND_TIMEOUT` to forcibly drop slow backend responses.

## Common Log Messages

PeakShield uses structured JSON logging (`slog`).

### `backend_timeout`
- **Meaning:** PeakShield waited for the backend to respond, but it exceeded `PEAKSHIELD_BACKEND_TIMEOUT`.
- **Result:** PeakShield returned a `504 Gateway Timeout` to the client.
- **Resolution:** Investigate why the backend is taking so long.

### `client_disconnect_before_response`
- **Meaning:** The user closed their browser or lost connection while in the waiting room or waiting for the backend.
- **Result:** PeakShield immediately frees up their slot. This is normal and expected behavior.

### `connection_reset` or `broken_pipe`
- **Meaning:** TCP connections are dropping at the OS level.
- **Resolution:** You are likely hitting OS limits. Review `docs/PERFORMANCE.md` and increase `net.core.somaxconn` and `fs.file-max`.

## Testing Backend Reachability
If PeakShield is returning `502 Bad Gateway` for everything:
1. SSH into the PeakShield server.
2. Run `curl -v <PEAKSHIELD_TARGET>`.
3. If it fails, the network link between PeakShield and the backend is broken (firewall, routing, or the backend is offline). PeakShield is functioning correctly by returning 502.
