// Package config provides typed configuration for PeakShield.
// All tunable parameters are read from environment variables at process startup,
// making PeakShield deployable in any environment (bare metal, Docker, systemd)
// without recompilation. Sensible production-safe defaults are baked in so the
// binary can run with zero configuration for local testing.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config is the single source of truth for all runtime parameters.
// It is constructed once in main() and then passed as a read-only pointer
// to every module — no global mutable state.
type Config struct {
	// ── Network ─────────────────────────────────────────────────────────────

	// ListenAddr is the TCP address PeakShield binds to for incoming clients.
	// Format: "[host]:port". Default ":8080" listens on all interfaces.
	ListenAddr string

	// TargetURL is the full URL prefix of the upstream legacy backend.
	// Every incoming request path is appended to this base URL.
	// Example: "http://legacy.gov.in:9090"
	TargetURL string

	// ── Server-side Timeouts ─────────────────────────────────────────────────
	// These govern the net/http.Server's built-in timeout machinery.
	// They are distinct from the backend (transport) timeouts below.

	// ReadTimeout caps the total time allowed to read the full client request
	// including body. Prevents slow-loris attacks from occupying goroutines.
	ReadTimeout time.Duration

	// WriteTimeout caps the total time to write the full response to the client,
	// measured from the end of the request header read. IMPORTANT: this must
	// be strictly greater than QueueTimeout so that clients held in the waiting
	// room do not get disconnected before their queued request is served.
	WriteTimeout time.Duration

	// IdleTimeout is the maximum duration an idle keep-alive connection is
	// kept open waiting for the next request. Frees FDs from idle browsers.
	IdleTimeout time.Duration

	// ── Backend (Transport) Timeouts ─────────────────────────────────────────

	// BackendTimeout is the end-to-end deadline for a single round-trip to the
	// legacy backend: TCP dial + TLS handshake + request write + response header.
	// Does NOT include response body streaming time (which can be long for large
	// pages). Set conservatively to detect hung backend connections early.
	BackendTimeout time.Duration

	// ── Rate Limiter (Module 2) ───────────────────────────────────────────────

	// RateLimit is the sustained token refill rate in tokens per second per IP.
	// Each HTTP request consumes exactly 1 token. An IP making requests faster
	// than this rate will eventually exhaust its burst capacity and be throttled.
	// Example: 100.0 → 100 requests per second per unique client IP.
	RateLimit float64

	// BurstSize is the maximum token bucket depth (capacity) per IP.
	// A fresh IP starts with a full bucket and can immediately fire BurstSize
	// requests before the rate limit kicks in. This allows bursty-but-legitimate
	// browser behavior (page + sub-resources) while still throttling flood bots.
	// Example: 200 → an IP can fire 200 requests instantly before being throttled.
	BurstSize int64

	// ── Circuit Breaker / Waiting Room (Module 3) ────────────────────────────

	// MaxConcurrent is the maximum number of requests simultaneously in-flight
	// to the backend (threshold T). When this limit is reached, new arrivals
	// are diverted into the in-memory FIFO waiting room instead of being
	// forwarded — protecting the legacy server from overload.
	MaxConcurrent int64

	// QueueSize is the maximum number of requests that can wait in the virtual
	// waiting room at any instant. If the queue is full, new arrivals immediately
	// receive the lightweight "Please Wait" HTML page (a hard cap prevents the
	// queue itself from consuming unbounded memory during pathological spikes).
	// Memory cost: each slot is a chan struct{} ≈ 96 bytes.
	// 500 slots × 96 bytes = ~47 KB — negligible.
	QueueSize int

	// QueueTimeout is the maximum duration a request will wait in the queue
	// before being evicted and receiving the "still busy" HTML wait page.
	// Prevents goroutine accumulation if the backend is down for a long time.
	// Must be strictly less than WriteTimeout to avoid the server closing the
	// connection before the timeout handler can write the wait-page response.
	QueueTimeout time.Duration

	// ── HTML Payload Stripper (Module 4) ─────────────────────────────────────

	// StripThreshold is the active-request count above which the stripper
	// middleware activates. When activeRequests > StripThreshold, HTML responses
	// from the backend are filtered to remove heavy assets (images, scripts, CSS)
	// before forwarding to the client, dramatically reducing egress bandwidth.
	// Must be strictly less than MaxConcurrent (stripper activates before the
	// circuit breaker trips, giving the server a chance to recover under load).
	StripThreshold int64
}

// Load constructs a Config by reading environment variables.
// Each variable has a documented default that is used when the variable is
// absent or empty. Returns a non-nil error if any present variable is
// unparseable or if cross-field validation fails.
//
// Intended call site: once at process startup in main().
//
//	cfg, err := config.Load()
//	if err != nil {
//	    log.Fatalf("config: %v", err)
//	}
func Load() (*Config, error) {
	cfg := &Config{}
	var err error

	// ── Network ──────────────────────────────────────────────────────────────
	cfg.ListenAddr = envString("PEAKSHIELD_LISTEN", ":8080")
	cfg.TargetURL = envString("PEAKSHIELD_TARGET", "http://localhost:9090")

	// ── Server-side Timeouts ──────────────────────────────────────────────────
	if cfg.ReadTimeout, err = envDuration("PEAKSHIELD_READ_TIMEOUT", 10*time.Second); err != nil {
		return nil, fmt.Errorf("PEAKSHIELD_READ_TIMEOUT: %w", err)
	}
	if cfg.WriteTimeout, err = envDuration("PEAKSHIELD_WRITE_TIMEOUT", 60*time.Second); err != nil {
		return nil, fmt.Errorf("PEAKSHIELD_WRITE_TIMEOUT: %w", err)
	}
	if cfg.IdleTimeout, err = envDuration("PEAKSHIELD_IDLE_TIMEOUT", 120*time.Second); err != nil {
		return nil, fmt.Errorf("PEAKSHIELD_IDLE_TIMEOUT: %w", err)
	}

	// ── Backend Timeouts ──────────────────────────────────────────────────────
	if cfg.BackendTimeout, err = envDuration("PEAKSHIELD_BACKEND_TIMEOUT", 15*time.Second); err != nil {
		return nil, fmt.Errorf("PEAKSHIELD_BACKEND_TIMEOUT: %w", err)
	}

	// ── Rate Limiter ──────────────────────────────────────────────────────────
	if cfg.RateLimit, err = envFloat64("PEAKSHIELD_RATE_LIMIT", 100.0); err != nil {
		return nil, fmt.Errorf("PEAKSHIELD_RATE_LIMIT: %w", err)
	}
	if cfg.BurstSize, err = envInt64("PEAKSHIELD_BURST", 200); err != nil {
		return nil, fmt.Errorf("PEAKSHIELD_BURST: %w", err)
	}

	// ── Circuit Breaker ───────────────────────────────────────────────────────
	if cfg.MaxConcurrent, err = envInt64("PEAKSHIELD_MAX_CONCURRENT", 50); err != nil {
		return nil, fmt.Errorf("PEAKSHIELD_MAX_CONCURRENT: %w", err)
	}
	if cfg.QueueSize, err = envInt("PEAKSHIELD_QUEUE_SIZE", 500); err != nil {
		return nil, fmt.Errorf("PEAKSHIELD_QUEUE_SIZE: %w", err)
	}
	if cfg.QueueTimeout, err = envDuration("PEAKSHIELD_QUEUE_TIMEOUT", 30*time.Second); err != nil {
		return nil, fmt.Errorf("PEAKSHIELD_QUEUE_TIMEOUT: %w", err)
	}

	// ── Stripper ──────────────────────────────────────────────────────────────
	if cfg.StripThreshold, err = envInt64("PEAKSHIELD_STRIP_THRESHOLD", 40); err != nil {
		return nil, fmt.Errorf("PEAKSHIELD_STRIP_THRESHOLD: %w", err)
	}

	// Semantic cross-field validation after all values are parsed.
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// validate performs semantic validation across config fields.
// Individual field parsing (type correctness) happens in the Load() callers.
// This function catches logical inconsistencies between fields.
func (c *Config) validate() error {
	if c.TargetURL == "" {
		return fmt.Errorf("PEAKSHIELD_TARGET must not be empty")
	}
	if c.RateLimit <= 0 {
		return fmt.Errorf("PEAKSHIELD_RATE_LIMIT must be > 0, got %g", c.RateLimit)
	}
	if c.BurstSize <= 0 {
		return fmt.Errorf("PEAKSHIELD_BURST must be > 0, got %d", c.BurstSize)
	}
	if c.MaxConcurrent <= 0 {
		return fmt.Errorf("PEAKSHIELD_MAX_CONCURRENT must be > 0, got %d", c.MaxConcurrent)
	}
	if c.QueueSize <= 0 {
		return fmt.Errorf("PEAKSHIELD_QUEUE_SIZE must be > 0, got %d", c.QueueSize)
	}
	// StripThreshold must be below MaxConcurrent so the stripper activates
	// before the circuit breaker trips. If they're equal, the stripper would
	// never fire before connections start queuing.
	if c.StripThreshold >= c.MaxConcurrent {
		return fmt.Errorf(
			"PEAKSHIELD_STRIP_THRESHOLD (%d) must be < PEAKSHIELD_MAX_CONCURRENT (%d): "+
				"stripper must activate before the circuit breaker trips",
			c.StripThreshold, c.MaxConcurrent,
		)
	}
	// WriteTimeout must exceed QueueTimeout. If WriteTimeout ≤ QueueTimeout,
	// the HTTP server will close the connection to the client before the waiting
	// room can write the wait-page HTML, resulting in a broken pipe error.
	if c.WriteTimeout <= c.QueueTimeout {
		return fmt.Errorf(
			"PEAKSHIELD_WRITE_TIMEOUT (%s) must be > PEAKSHIELD_QUEUE_TIMEOUT (%s): "+
				"the server would hang up on queued clients before serving them",
			c.WriteTimeout, c.QueueTimeout,
		)
	}
	return nil
}

// ── Environment Variable Helpers ──────────────────────────────────────────────
// These are unexported helpers used only by Load(). They follow a consistent
// contract: return the default if the env var is absent or empty; return an
// error (wrapping strconv's error) if the var is present but malformed.

// envString reads a string env var, returning def if the variable is unset or empty.
func envString(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envDuration parses a Go duration string (e.g. "10s", "1m30s", "500ms").
// Returns def if the variable is unset; error if present but unparseable.
func envDuration(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q (example: \"10s\", \"1m30s\"): %w", v, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("duration must be positive, got %q", v)
	}
	return d, nil
}

// envFloat64 parses a 64-bit floating point decimal string.
// Returns def if the variable is unset; error if present but unparseable.
func envFloat64(key string, def float64) (float64, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid float %q: %w", v, err)
	}
	return f, nil
}

// envInt64 parses a base-10 64-bit integer string.
// Returns def if the variable is unset; error if present but unparseable.
func envInt64(key string, def int64) (int64, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	i, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid integer %q: %w", v, err)
	}
	return i, nil
}

// envInt parses a base-10 int string (platform-native width).
// Returns def if the variable is unset; error if present but unparseable.
func envInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	i, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("invalid integer %q: %w", v, err)
	}
	return i, nil
}
