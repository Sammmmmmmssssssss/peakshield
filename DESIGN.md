# PeakShield Architecture & Design

This document details the architectural decisions and internal workings of PeakShield. It serves as a runbook for developers and a reference for understanding how PeakShield achieves its high concurrency and low latency targets.

## Core Philosophy

PeakShield is designed around three core constraints:
1. **Zero External Dependencies**: Ensure deployability anywhere without managing complex sidecars or external stores (like Redis).
2. **Predictable Memory Usage**: Pre-allocate queues and aggressively pool byte buffers to avoid GC pauses during load spikes.
3. **Lock-Free Hot Paths**: Avoid global mutexes that cause contention when 10,000+ users connect simultaneously.

## Pipeline Architecture

Every incoming request flows through a chain of HTTP middlewares before reaching the core reverse proxy:

1. **Rate Limiter (`ratelimiter/token_bucket.go`)**
2. **Waiting Room (`waitingroom/waitingroom.go`)**
3. **HTML Stripper (`stripper/stripper.go`)**
4. **Reverse Proxy Core (`proxy/proxy.go`)**

---

## 1. Rate Limiter (Token Bucket)

To prevent abusive clients (bots) from overwhelming the waiting room queue, we implement a classic Token Bucket algorithm.

### The Problem with standard Rate Limiters
A standard `map[string]*Bucket` protected by a single `sync.RWMutex` becomes a severe bottleneck when 10,000 unique IP addresses attempt to connect simultaneously.

### The Sharded Solution
We shard the map into 256 independent segments. 
- When an IP address connects, we hash the IP using **FNV-1a**.
- The hash dictates which of the 256 shards the IP belongs to.
- Each shard has its own `sync.RWMutex`.

This drastically reduces lock contention. The FNV-1a hash is used because it executes in ~11ns for a standard IPv4 address string, making it essentially free.

---

## 2. The Virtual Waiting Room

The Waiting Room is the heart of PeakShield. It acts as a strict concurrency limiter (circuit breaker) for the fragile legacy backend.

### Mechanism
The waiting room maintains a strict limit of `MaxConcurrent` active requests to the backend. It does this using a simple integer counter tracked via `sync/atomic`.

If `ActiveRequests < MaxConcurrent`, the request goes straight through.
If `ActiveRequests >= MaxConcurrent`, the request is sent to the **Queue**.

### The Queue
Instead of holding requests in a complex struct array, we use a single buffered Go channel: `chan chan struct{}`.
- Size of the queue channel = `QueueSize`.
- When a request is queued, we create a private, unbuffered "ticket" channel: `ticket := make(chan struct{})`.
- We push this `ticket` onto the main queue channel.
- The request goroutine then blocks, waiting to receive on the `ticket` (via a `select` statement that also listens for timeouts).

### Admittance
When a request finishes communicating with the backend, it signals the waiting room to release its slot.
The waiting room pulls the oldest `ticket` from the queue and closes it (`close(ticket)`). This instantly unblocks the waiting goroutine, which proceeds to the backend.

If a client disconnects while in the queue, its ticket remains in the queue. When that ticket is eventually pulled and signaled, the `drainTicket()` mechanism intercepts the signal and immediately grants the slot to the *next* person in line, preventing backend slots from being "leaked" to dead connections.

---

## 3. Zero-Regex HTML Stripper

During extreme traffic, egress bandwidth can bottleneck before CPU or Memory. 

If the backend serves a 5MB HTML page with images and scripts, the HTML Stripper dynamically intercepts the `text/html` response. It uses a custom hand-rolled finite state machine (FSM) to parse the raw byte stream as it arrives, maintaining our strict zero-dependency philosophy (no `golang.org/x/net/html` required).

It skips any `<img>`, `<script>`, or `<style>` tags, dropping them from the stream before they are sent to the client. We intentionally avoid Regex because Regex engines scale poorly and are vulnerable to ReDoS attacks on malformed HTML. The hand-rolled FSM tokenizer is an O(N) scanner with virtually zero allocations per request.

---

## 4. Observability and Monitoring

PeakShield uses standard `log/slog` for structured JSON logging. This allows tools like ELK, Datadog, or Fluentd to parse the logs easily.

It also exposes a `/metrics` endpoint on the main port, serving **Prometheus Text Format**. This is done without importing the official Prometheus SDK to strictly maintain the zero-dependency rule. We manually output the required format via `fmt.Fprintf`.

Metrics include:
- `peakshield_active_requests`: Gauge of current backend connections.
- `peakshield_queue_depth`: Gauge of current waiting clients.
- `peakshield_alloc_bytes`: Memory allocated.

## 5. Security

The `config/config.go` defines a `PEAKSHIELD_INSECURE_SKIP_VERIFY` flag. This allows PeakShield to securely proxy to government backends that use expired or self-signed TLS certificates, while still encrypting the traffic channel (HTTPS upstream).
