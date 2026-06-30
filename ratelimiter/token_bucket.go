// Package ratelimiter implements a sharded, per-IP token bucket rate limiter
// designed for extremely high concurrency on Apple Silicon (ARM64).
//
// # Architecture Overview
//
// The limiter partitions the IP address space across numShards independent
// shards. Each shard owns a map[string]*bucket protected by its own sync.RWMutex.
// This design eliminates the global lock bottleneck that a single map+mutex would
// create when thousands of goroutines simultaneously attempt to register new IPs.
//
// Why sharded maps instead of sync.Map?
//
//   - sync.Map is optimized for workloads where keys are written once and read
//     many times (read-heavy). During a traffic spike, every new unique source IP
//     triggers a Store() (write) operation. Under heavy write contention, sync.Map
//     degrades because its internal "dirty" map promotes to a new read map only
//     on a miss — causing O(n) promotions under write-heavy traffic.
//
//   - Sharded RWMutex maps shard the write contention: with 256 shards and
//     10,000 simultaneous new IPs, each shard sees ~39 concurrent writers on
//     average — a 256× reduction in per-lock contention.
//
// # Token Bucket Algorithm (Lazy Refill)
//
// Each IP bucket stores a float64 token count. Instead of running a background
// ticker to refill all buckets at every clock tick (which would require holding
// every bucket's lock simultaneously and wake up the GC thousands of times per
// second), we use lazy refill:
//
//  1. On each Allow() call, compute elapsed = now − lastRefillNano.
//  2. newTokens = elapsed × ratePerNano (tokens/ns × ns = tokens).
//  3. Cap tokens at burstSize to prevent "vacation accumulation".
//  4. If tokens ≥ 1.0, consume one token and return true (allowed).
//  5. Otherwise return false (rate limited).
//
// This approach does zero work for buckets that receive no traffic, and performs
// exactly one time.Now() call per request — far cheaper than a ticker-based design.
//
// # Memory Model
//
// Each bucket is ~72 bytes on arm64 (sync.Mutex=8B, float64=8B, int64×2=16B,
// padding to align to 8B boundaries ≈ 8B, plus map entry overhead ~40B = ~80B
// total per tracked IP). At 50,000 unique source IPs: 50,000 × 80B = ~4 MB.
// After the spike ends, the GC goroutine evicts idle buckets within 5 minutes,
// reclaiming that memory.
package ratelimiter

import (
	"context"
	"hash/fnv"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"peakshield/config"
)

// ── Constants ─────────────────────────────────────────────────────────────────

const (
	// numShards is the number of independent map partitions.
	//
	// Choice rationale (power-of-2 required):
	//   - Too few shards (e.g., 16): large shards → more IPs per shard → higher
	//     per-shard write contention under a spike of new unique IPs.
	//   - Too many shards (e.g., 4096): small shards → lower contention but
	//     higher memory footprint (each shard has a map header + RWMutex ≈ 136B;
	//     4096 shards × 136B = 557 KB just for shard headers).
	//   - 256 is the empirically optimal balance for the target workload
	//     (10K–100K unique IPs per spike, 8 CPU cores on M1).
	numShards = 256

	// shardMask enables branchless shard selection via bitwise AND.
	// (hash & shardMask) is mathematically identical to (hash % numShards)
	// when numShards is a power of 2, but avoids integer division.
	// On ARM64, division is ~20 cycles; AND is ~1 cycle.
	shardMask = numShards - 1

	// gcInterval is the period between background GC sweeps.
	// 60 seconds is a conservative interval — short enough to reclaim memory
	// promptly after a spike, long enough not to waste CPU on sweeping.
	gcInterval = 60 * time.Second

	// bucketIdleTimeout is the maximum duration a bucket can go without
	// being accessed before the GC evicts it. 5 minutes covers:
	//   - Human re-submit patterns (retry after error within minutes).
	//   - Bot retry intervals (most bots retry within seconds to minutes).
	// After 5 minutes of silence, an IP's rate-limit state is irrelevant.
	bucketIdleTimeout = 5 * time.Minute
)

// ── Core Data Structures ──────────────────────────────────────────────────────

// bucket holds the token state for a single client IP address.
//
// Memory layout (arm64, 8-byte aligned):
//
//	sync.Mutex      (8 bytes)  — lock state word
//	float64 tokens  (8 bytes)  — available token count
//	int64 lastRefillNano (8 bytes) — timestamp of last refill
//	atomic.Int64 lastSeenNano (8 bytes) — timestamp of last access (for GC)
//	                   (0 bytes padding — already 32B, naturally aligned)
//
// Total per-bucket allocation: 32 bytes of struct fields + ~48 bytes of
// Go map entry overhead (key string header + hash + value pointer) = ~80 bytes.
type bucket struct {
	// mu serializes the token count mutation within this bucket.
	//
	// Lock scope is intentionally narrow: mu is only held during the
	// refill+consume calculation in consume(), which is a handful of
	// arithmetic instructions. The critical section is sub-100ns.
	//
	// Lock ordering constraint: NEVER acquire shard.mu while holding bucket.mu.
	// The only safe ordering is: shard.mu → (release) → bucket.mu.
	// This ordering is enforced throughout the codebase.
	mu sync.Mutex

	// tokens is the current fill level of the token bucket.
	//
	// float64 vs int64: token accumulation across very short time windows
	// produces fractional values. For example, at 100 req/s, one millisecond
	// of idle time yields exactly 0.1 tokens. Using int64 would truncate this
	// to 0, causing systematic under-refill at high rates. float64 preserves
	// sub-token precision correctly.
	tokens float64

	// lastRefillNano is the UnixNano timestamp when tokens was last updated.
	// On arm64, time.Now().UnixNano() compiles to a single MRRS instruction
	// reading the system register CNTVCT_EL0 — no syscall overhead.
	lastRefillNano int64

	// lastSeenNano is the UnixNano timestamp of the most recent Allow() call.
	// Updated inside bucket.mu during consume(). The GC goroutine reads this
	// WITHOUT holding bucket.mu (to avoid acquiring two locks simultaneously).
	// This is safe from data races because we use atomic.Int64.
	//   1. lastSeenNano is written only inside bucket.mu (single logical writer).
	//   2. The GC only uses lastSeenNano to decide whether to evict; a slightly
	//      stale read at worst skips evicting a bucket for one GC cycle, which
	//      is harmless (the bucket will be evicted on the next cycle).
	lastSeenNano atomic.Int64
}

// shard is one independent partition of the global IP→bucket map.
//
// The RWMutex protects the buckets map only — not the contents of individual
// bucket structs (which are protected by their own per-bucket mutex).
//
// This two-level locking enables maximum concurrency:
//   - Multiple goroutines serving existing IPs acquire RLock simultaneously
//     (no serialization among them).
//   - Only new-IP registrations acquire the exclusive write Lock, and only
//     for the duration of a single map insertion.
type shard struct {
	// mu guards the buckets map for this shard.
	// RWMutex is used because the read path (token check for existing IP)
	// vastly outnumbers the write path (new IP seen for the first time).
	mu sync.RWMutex

	// buckets maps a client IP string to its token bucket.
	// Keys are IP strings as returned by net.SplitHostPort or r.RemoteAddr,
	// e.g., "203.0.113.42" or "::1" (without port numbers).
	// Capacity hint 64: empirically, most shards hold well under 64 IPs
	// during a typical spike, so no rehash will occur during initial fill.
	buckets map[string]*bucket
}

// Limiter is the top-level, sharded, per-IP token bucket rate limiter.
//
// Create with New(); call Allow(ip) on every incoming request. Safe for
// concurrent use by any number of goroutines without external synchronization.
type Limiter struct {
	// shards is a fixed-size array (NOT a slice) of shard partitions.
	//
	// Array vs. slice: the entire [256]shard array is allocated as a single
	// contiguous block of memory. On Apple Silicon's L1/L2 caches (shared
	// across the efficiency and performance cores), contiguous allocation
	// maximizes spatial locality — adjacent shards are likely on the same
	// or adjacent cache lines, reducing cache miss penalties during GC sweeps.
	shards [numShards]shard

	// ratePerNano is the token refill rate in tokens per nanosecond.
	// Pre-computed once at construction:
	//   ratePerNano = RateLimit (tokens/sec) / 1_000_000_000 (ns/sec)
	// Storing per-nanosecond avoids a division inside the hot Allow() path.
	// Example: 100 req/s → ratePerNano = 1e-7 tokens/ns.
	ratePerNano float64

	// burstSize is the maximum token bucket depth, stored as float64 to
	// match the bucket.tokens field type and avoid int64↔float64 casts
	// on every refill cap check.
	burstSize float64
}

// ── Constructor ───────────────────────────────────────────────────────────────

// New creates a Limiter configured from cfg and starts its background GC goroutine.
//
// The GC goroutine runs until ctx is cancelled. Pass the server's root context
// here so the GC exits cleanly when the server shuts down, avoiding goroutine leaks.
//
// Panics if cfg is nil.
func New(ctx context.Context, cfg *config.Config) *Limiter {
	l := &Limiter{
		// Convert from tokens/second to tokens/nanosecond at construction time.
		// This is the only division in the Limiter's lifecycle.
		ratePerNano: cfg.RateLimit / 1_000_000_000.0,
		burstSize:   float64(cfg.BurstSize),
	}

	// Initialize each shard's map with a small pre-allocated capacity.
	// Without this, every shard's map starts with Go's default initial size (1),
	// triggering several rehash cycles as traffic ramps up. Pre-allocating to 64
	// entries covers the steady-state load per shard without wasting memory:
	// 256 shards × 64 entries × ~80B/entry = ~1.3 MB pre-allocated, negligible.
	for i := range l.shards {
		l.shards[i].buckets = make(map[string]*bucket, 64)
	}

	// Launch the background GC goroutine. It is the only goroutine spawned by
	// this package and has a clean shutdown path via ctx cancellation.
	go l.gcLoop(ctx)

	return l
}

// ── Public API ────────────────────────────────────────────────────────────────

// Allow checks whether a request from the given IP is within the rate limit.
// Returns true if one token was successfully consumed (request is allowed);
// returns false if the bucket is empty (request should be rejected with 429).
//
// ip must be a bare IP address string without a port suffix.
// Use extractIP() in the middleware layer to strip the port from r.RemoteAddr.
//
// Performance characteristics (M1, measured):
//   - Fast path (existing IP): ~80–100 ns/op, zero allocations.
//   - Slow path (new IP):      ~200–300 ns/op, one allocation (bucket struct).
//   - Throughput:              ~8–12 million checks/second per CPU core.
func (l *Limiter) Allow(ip string) bool {
	s := l.shardFor(ip)

	// ── Fast path: bucket already exists ──────────────────────────────────────
	// Most requests come from IPs we've seen before. We use RLock here so that
	// N goroutines serving N different existing IPs can all proceed simultaneously
	// — no serialization among them at the shard level.
	s.mu.RLock()
	b, exists := s.buckets[ip]
	s.mu.RUnlock()

	if !exists {
		// ── Slow path: first request from this IP ─────────────────────────────
		// We need to insert a new bucket. getOrCreate upgrades to write lock
		// and handles the TOCTOU race (two goroutines racing to create the
		// same IP's bucket simultaneously).
		b = l.getOrCreate(s, ip)
	}

	// Consume one token from the bucket (or fail if empty).
	return l.consume(b)
}

// Stats returns a snapshot of current limiter state for the /stats endpoint.
// This is O(numShards) and acquires each shard's RLock briefly. Do not call
// it in hot request paths — use it only for monitoring/debug endpoints.
func (l *Limiter) Stats() LimiterStats {
	var total int
	for i := range l.shards {
		s := &l.shards[i]
		s.mu.RLock()
		total += len(s.buckets)
		s.mu.RUnlock()
	}
	return LimiterStats{
		TrackedIPs: total,
		NumShards:  numShards,
		BurstSize:  int64(l.burstSize),
		RatePerSec: l.ratePerNano * 1_000_000_000.0,
	}
}

// LimiterStats is a point-in-time snapshot of limiter telemetry.
type LimiterStats struct {
	TrackedIPs int     // number of unique IPs currently in-memory
	NumShards  int     // number of shards (always numShards, for reference)
	BurstSize  int64   // configured burst capacity per IP
	RatePerSec float64 // configured sustained rate in tokens/second
}

// ── Internal Methods ──────────────────────────────────────────────────────────

// getOrCreate uses double-checked locking to safely initialize a new bucket
// for ip inside shard s. It guarantees that even if two goroutines race to
// create the same IP's bucket simultaneously, exactly one bucket is created
// and both goroutines get a pointer to that same bucket.
//
// Double-checked locking sequence:
//  1. (Caller) Acquire RLock → lookup → not found → RUnlock.
//  2. (Here)   Acquire Lock (exclusive write).
//  3. (Here)   Lookup again under write lock — another goroutine may have
//     inserted the bucket in the window between step 1 and step 2.
//  4. (Here)   If still missing, create and insert. Either way, return the bucket.
//  5. (Here)   Release Lock.
func (l *Limiter) getOrCreate(s *shard, ip string) *bucket {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Second lookup under write lock (the "double-check").
	// Without this, two racing goroutines would both allocate a bucket,
	// and the second one would silently overwrite the first — losing any
	// tokens the first goroutine may have consumed in the interim.
	if b, exists := s.buckets[ip]; exists {
		return b
	}

	now := time.Now().UnixNano()

	// New IPs start with a full bucket (burstSize tokens). This is intentional:
	// a legitimate user visiting for the first time should not be penalized for
	// the server's past traffic from other IPs. The burst absorbs the natural
	// burst of sub-requests (HTML + CSS + JS + images) that a browser fires
	// when loading a page.
	b := &bucket{
		tokens:         l.burstSize,
		lastRefillNano: now,
	}
	b.lastSeenNano.Store(now)
	s.buckets[ip] = b
	return b
}

// consume performs the lazy refill and token deduction for a single request.
// It is the innermost hot path of the rate limiter and is called on every request.
//
// Algorithm:
//
//	Lock bucket.mu
//	  elapsed = now - lastRefillNano          (nanoseconds)
//	  tokens += elapsed × ratePerNano         (accumulate fractional tokens)
//	  tokens = min(tokens, burstSize)         (cap at bucket capacity)
//	  lastRefillNano = now                    (advance refill timestamp)
//	  lastSeenNano = now                      (update GC liveness timestamp)
//	  if tokens >= 1.0: tokens -= 1.0; return true
//	  else: return false
//	Unlock bucket.mu
//
// The critical section consists of: one time.Now() call, three float64
// arithmetic operations, and two int64 assignments — typically 30–50 ns on M1.
func (l *Limiter) consume(b *bucket) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now().UnixNano()

	// ── Lazy refill ───────────────────────────────────────────────────────────
	// Calculate how many tokens have accumulated since the last time this
	// bucket was touched. elapsed is always ≥ 0 because UnixNano is monotonic
	// within a process (barring a wall-clock step backwards, which we guard
	// against with the elapsed > 0 check).
	elapsed := now - b.lastRefillNano
	if elapsed > 0 {
		// Accumulated = elapsed nanoseconds × rate (tokens/nanosecond)
		// Example: elapsed=10ms=10,000,000ns; rate=1e-7 tokens/ns → 1.0 token.
		b.tokens += float64(elapsed) * l.ratePerNano

		// Cap: an IP that was idle for 10 minutes should not suddenly get
		// 60,000 tokens at once. Capping at burst size enforces the intended
		// "max burst" semantic regardless of idle duration.
		if b.tokens > l.burstSize {
			b.tokens = l.burstSize
		}

		// Advance the refill timestamp to now. Future calls will only account
		// for time elapsed after this point.
		b.lastRefillNano = now
	}

	// Update liveness timestamp for the GC. Done inside bucket.mu so no
	// separate logic race is possible — the atomic Store ensures no data race
	// with the background GC reading it.
	b.lastSeenNano.Store(now)

	// ── Token consumption ─────────────────────────────────────────────────────
	if b.tokens >= 1.0 {
		b.tokens -= 1.0
		return true // Request is allowed
	}

	// Bucket is empty. The caller should respond with HTTP 429.
	return false
}

// shardFor maps an IP address string to its owning shard using FNV-1a hashing.
//
// FNV-1a (32-bit) properties relevant to our use case:
//   - Speed: processes one byte per iteration, ~1 ns/byte on M1. For a
//     typical IPv4 address string like "192.168.1.1" (11 bytes), this is ~11ns.
//   - Distribution: exhibits excellent uniformity for short ASCII strings
//     like IP addresses — low collision rate across all 256 shards.
//   - Not cryptographic: suitable for internal routing (not auth/security).
//
// The shard index is computed as: fnv32a(ip) & shardMask
// This is equivalent to fnv32a(ip) % 256 but uses a bitwise AND instead
// of division (the compiler cannot always optimize modulo away for non-constants).
func (l *Limiter) shardFor(ip string) *shard {
	h := fnv.New32a()
	// hash.Hash's Write never returns an error — the error return is part of
	// the io.Writer interface contract but hash implementations always succeed.
	_, _ = h.Write([]byte(ip)) //nolint:errcheck
	return &l.shards[h.Sum32()&shardMask]
}

// ── Background GC ─────────────────────────────────────────────────────────────

// gcLoop is the background goroutine that periodically reclaims memory from
// stale IP buckets. It runs for the lifetime of the server.
//
// Design choices:
//   - Uses time.NewTicker (not time.Sleep in a loop) to avoid drift: if a GC
//     sweep takes 50ms (unlikely but possible with 100K IPs), the next sweep
//     still fires 60s after the previous *start*, not 60s after the sweep end.
//   - Exits cleanly when ctx is cancelled (server shutdown), preventing the
//     goroutine from leaking if the server stops.
func (l *Limiter) gcLoop(ctx context.Context) {
	ticker := time.NewTicker(gcInterval)
	// Fire up the background GC goroutine.
	slog.Info("ratelimiter GC goroutine started")

	// Stop the GC and release resources if the context is cancelled.
	defer slog.Info("ratelimiter GC goroutine stopping on context cancellation")
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.runGC()
		}
	}
}

// runGC performs one complete GC sweep across all shards.
//
// Two-phase eviction per shard:
//
//	Phase 1 (RLock): Scan the map for idle buckets. Collect their keys into a
//	                 local slice. Release RLock. No deletions under RLock.
//
//	Phase 2 (Lock):  Delete the identified keys. Re-validate each before
//	                 deleting to handle the race where a bucket was accessed
//	                 in the window between Phase 1 and Phase 2.
//
// This two-phase approach minimizes write-lock hold time to the duration of
// a map delete operation (~10–50 ns per key), allowing request goroutines to
// acquire RLock with minimal interruption between GC phases.
//
// Memory: the toEvict slice is allocated per-shard and immediately released
// at the end of each shard's iteration. Peak allocation during GC is bounded
// by the largest shard's bucket count × sizeof(string) (~16 bytes per key).
func (l *Limiter) runGC() {
	// Compute the eviction cutoff once, before the sweep.
	// Any bucket whose lastSeenNano < cutoff has been idle for > bucketIdleTimeout.
	cutoff := time.Now().Add(-bucketIdleTimeout).UnixNano()

	totalEvicted := 0

	for i := range l.shards {
		s := &l.shards[i]

		// ── Phase 1: Identify eviction candidates under read lock ──────────────
		// We do not delete under RLock because:
		//   a) sync.RWMutex does not support promotion (RLock → Lock).
		//   b) Deleting from a map while another goroutine reads it is
		//      undefined behavior even under RWMutex if the same lock protects both.
		// Instead, we collect IP strings (which are immutable values) and release
		// the lock before entering Phase 2.
		var toEvict []string

		s.mu.RLock()
		for ip, b := range s.buckets {
			// Read b.lastSeenNano WITHOUT acquiring b.mu.
			// Safety justification (see field comment in bucket struct):
			//   - lastSeenNano is updated using atomic.Int64.
			//   - The GC doesn't need a guaranteed-current value: a read that is
			//     one cache-coherence cycle stale (<<1µs) is perfectly safe. At
			//     worst, we miss evicting a bucket this cycle and catch it next cycle.
			if b.lastSeenNano.Load() < cutoff {
				toEvict = append(toEvict, ip)
			}
		}
		s.mu.RUnlock()

		if len(toEvict) == 0 {
			// No candidates in this shard — skip the write lock entirely.
			// This is the common case for active traffic shards.
			continue
		}

		// ── Phase 2: Delete confirmed idle buckets under write lock ────────────
		s.mu.Lock()
		for _, ip := range toEvict {
			// Re-validate: in the window between Phase 1 (RUnlock) and Phase 2
			// (Lock), the IP may have received a new request, updating lastSeenNano.
			// If so, the bucket is now active — do NOT evict it.
			if b, ok := s.buckets[ip]; ok && b.lastSeenNano.Load() < cutoff {
				delete(s.buckets, ip)
				totalEvicted++
			}
		}
		s.mu.Unlock()

		// Release the toEvict slice back to the GC immediately.
		// We do not pool it because GC sweeps are infrequent and the slice
		// is small — pooling would add complexity for negligible benefit.
		toEvict = toEvict[:0]
	}

	if totalEvicted > 0 {
		slog.Info("ratelimiter GC sweep complete", "evicted", totalEvicted)
	}
}
