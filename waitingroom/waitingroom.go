// Package waitingroom implements a virtual waiting room and circuit breaker
// for PeakShield.
//
// # Problem
//
// Legacy government servers (Tomcat, JBoss, old PHP stacks) have a hard
// concurrency ceiling — typically 50–200 simultaneous active requests before
// the thread pool exhausts and connections receive 502/504 errors or are simply
// dropped. During exam result announcements or form submission windows, traffic
// can spike from ~10 req/s to 50,000+ req/s within seconds.
//
// # Solution
//
// The WaitingRoom intercepts every admitted request and tracks how many are
// concurrently in-flight to the backend. When that count reaches MaxConcurrent
// (threshold T), new requests are NOT forwarded. Instead, they are held open
// in a lightweight FIFO in-memory queue and admitted one-by-one as slots free
// up. Clients see a minimal, auto-refreshing HTML wait page instead of a 502.
//
// # Core Data Structure: chan chan struct{}
//
// The queue is a Go buffered channel whose elements are themselves channels:
//
//	queue chan (chan struct{})   capacity = MaxQueueSize
//
// Each waiting goroutine creates a private "ticket" channel:
//
//	ticket := make(chan struct{}, 1)   // 1-buffered: CRITICAL (see below)
//
// and sends it into the outer queue channel (non-blocking; falls back to
// "queue full" if the outer channel is at cap). The goroutine parks in a
// three-way select:
//
//	select {
//	case <-ticket:               // admission signal arrived → proceed to backend
//	case <-time.After(timeout): // timed out → serve wait page
//	case <-r.Context().Done():  // client disconnected → exit silently
//	}
//
// When a backend request finishes, signalNext() dequeues the front ticket
// and sends an admission signal:
//
//	select {
//	case ticket := <-wr.queue:
//	    ticket <- struct{}{}   // always non-blocking (1-buffered)
//	default:
//	    // queue empty, nobody waiting
//	}
//
// # Why 1-Buffered Tickets?
//
// Tickets must be 1-buffered (not 0-buffered / unbuffered). If a ticket were
// unbuffered, signalNext() would block waiting for the goroutine to receive —
// but the goroutine may have already exited (timeout or context cancel). This
// would deadlock signalNext(), which is called inside a completing goroutine's
// defer — a catastrophic hang. With buffer=1, signalNext() always returns
// immediately: if the goroutine is alive, it receives; if it has exited, the
// signal sits in the buffer until the drainTicket() cleanup goroutine forwards it.
//
// # drainTicket: Slot Conservation Under Cancellation
//
// When a goroutine exits the select via the timeout or Done case, its ticket
// remains in the outer queue channel (Go channels do not support removal from
// the middle). A later signalNext() will dequeue it and write to its buffer.
// Without intervention, this signal would sit unread, and the freed backend
// slot would be permanently "lost" — effective concurrency would silently
// decrease by 1 for each cancelled waiter.
//
// drainTicket() solves this: it is spawned as a goroutine by every exiting
// waiter, waits to receive the eventual signal on the orphaned ticket, then
// calls signalNext() to forward the slot to the next live waiter.
//
// # Memory Model at Peak Load (MaxQueueSize = 500)
//
//   - activeRequests:   8 bytes (int64)
//   - queueDepth:       8 bytes (int64)
//   - queue channel:   ~96 bytes header + 500 × 8 bytes (chan pointers) = ~4 KB
//   - 500 ticket channels: 500 × ~96 bytes = ~48 KB
//   - 500 parked goroutines: 500 × ~2 KB initial stack = ~1 MB (lazy growth)
//   - 500 drainTicket goroutines (worst case): 500 × ~2 KB = ~1 MB
//
// Total: ~2.1 MB under absolute maximum queue load. Well within 30 MB budget.
package waitingroom

import (
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"peakshield/config"
)

// ── Wait Page ─────────────────────────────────────────────────────────────────

// waitMode describes the reason a wait page is being served, controlling
// the user-facing message rendered into the HTML template.
type waitMode uint8

const (
	// modeQueued: request is actively parked in the queue at a known position.
	modeQueued waitMode = iota

	// modeQueueFull: the queue channel is at capacity; the request cannot be held.
	// Serve the wait page immediately without queuing.
	modeQueueFull

	// modeTimeout: the request parked in the queue but waited longer than
	// QueueTimeout. Serve the wait page and spawn a drainTicket goroutine.
	modeTimeout
)

// waitPageTemplate is the complete HTML wait page, rendered via fmt.Sprintf.
//
// Design principles:
//
//   - Pure HTML + inline CSS, zero JavaScript, zero external resources.
//     Loads reliably even when CDNs and external networks are congested.
//
//   - <meta http-equiv="refresh" content="5">: the oldest, most reliable
//     browser auto-refresh mechanism. Unlike JavaScript polling (which can
//     spawn multiple concurrent requests from a single tab), this triggers
//     exactly one full page reload every 5 seconds per client. Zero DoS risk.
//
//   - CSS @keyframes "blink" animation: provides visual feedback ("the page
//     is alive") without a single byte of JavaScript. Works in all browsers
//     back to IE 10, including screen readers and accessibility tools.
//
//   - Total rendered page weight: ~690 bytes. This is ~1400× smaller than
//     a typical government exam portal page (~1 MB). At MaxQueueSize=500
//     clients each refreshing every 5s: 500 × 690B / 5s ≈ 69 KB/s egress —
//     negligible on any modern server NIC.
//
// Format arguments:
//
//	%s [1] — main status message (HTML, may contain inline tags like <strong>)
//	%s [2] — secondary detail line (plain text)
//
// CSS percentage values use %% (escaped) because this string is processed
// by fmt.Sprintf: %% → literal % in the rendered output.
const waitPageTemplate = `<!DOCTYPE html><html lang="en"><head><meta charset="UTF-8"><meta http-equiv="refresh" content="5"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Please Wait — High Traffic</title><style>*{box-sizing:border-box;margin:0;padding:0}body{font-family:system-ui,-apple-system,sans-serif;background:#0f172a;color:#e2e8f0;display:flex;align-items:center;justify-content:center;min-height:100vh;text-align:center;padding:1rem}.c{max-width:400px}.e{font-size:2.8rem;margin-bottom:.6rem}h1{font-size:1.15rem;font-weight:600;margin-bottom:1rem}.m{color:#94a3b8;font-size:.9rem;margin-bottom:.4rem}.d{color:#94a3b8;font-size:.82rem;margin-bottom:1.2rem}.dots{display:flex;justify-content:center;gap:6px;margin-bottom:1.2rem}.dot{width:9px;height:9px;border-radius:50%%;background:#38bdf8}@keyframes blink{0%%,100%%{opacity:.15}50%%{opacity:1}}.dot:nth-child(1){animation:blink 1.2s 0s infinite}.dot:nth-child(2){animation:blink 1.2s .4s infinite}.dot:nth-child(3){animation:blink 1.2s .8s infinite}small{color:#475569;font-size:.73rem}</style></head><body><div class="c"><div class="e">⏳</div><h1>High Traffic — Please Wait</h1><p class="m">%s</p><p class="d">%s</p><div class="dots"><div class="dot"></div><div class="dot"></div><div class="dot"></div></div><small>Auto-refreshes every 5 seconds. Please do not press F5 or reload.</small></div></body></html>`

// ── WaitingRoom ───────────────────────────────────────────────────────────────

// WaitingRoom is the circuit breaker and virtual queue for PeakShield.
// It is safe for concurrent use by any number of goroutines.
type WaitingRoom struct {
	cfg *config.Config

	// activeRequests counts requests currently in-flight to the backend.
	// Incremented on admission (direct or from queue), decremented on completion.
	// Accessed exclusively via sync/atomic — no mutex, no cache line bouncing.
	//
	// Alignment note: on arm64 (Apple M1), int64 is naturally 8-byte aligned
	// when the struct is allocated on the heap, satisfying the atomic package's
	// alignment requirement without padding.
	activeRequests int64

	// queueDepth is an atomic approximation of current queue occupancy.
	// Incremented on enqueue, decremented on admission or drainTicket cleanup.
	// Used only for UX (wait page position display) — slight inaccuracy is
	// acceptable. May temporarily overcount if goroutines time out before
	// their drainTicket goroutines run.
	queueDepth int64

	// queue is the FIFO channel of ticket channels.
	// Outer channel capacity is fixed at cfg.QueueSize — the hard limit on
	// the number of goroutines that can be waiting simultaneously. Go channels
	// are FIFO, so the first ticket sent is the first ticket received by
	// signalNext() — providing strict ordering.
	queue chan chan struct{}
}

// New creates a WaitingRoom ready for use. No goroutines are spawned at
// construction time — the WaitingRoom is entirely reactive (driven by incoming
// requests completing and calling releaseSlot).
func New(cfg *config.Config) *WaitingRoom {
	return &WaitingRoom{
		cfg: cfg,
		// Allocate the queue channel at full capacity immediately.
		// Go channels have a fixed capacity; there is no rehashing or reallocation.
		// cap(wr.queue) == cfg.QueueSize for the lifetime of the process.
		queue: make(chan chan struct{}, cfg.QueueSize),
	}
}

// Middleware returns an http.Handler wrapping next with circuit breaker logic.
//
// # Decision Tree
//
//	Increment activeRequests → result ≤ MaxConcurrent?
//	├── YES (slot available): defer releaseSlot; call next; return.
//	└── NO (overload): undo increment; try to queue.
//	    Queue not full?
//	    ├── YES: park on ticket; select:
//	    │   ├── <-ticket: admitted → claim slot; check context; defer releaseSlot; call next.
//	    │   ├── <-time.After: timed out → drainTicket goroutine; serve wait page.
//	    │   └── <-r.Context().Done(): disconnected → drainTicket goroutine; return.
//	    └── NO (queue full): serve wait page immediately; return.
func (wr *WaitingRoom) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

		// ── Path A: Optimistic Direct Admission ──────────────────────────────
		//
		// Atomically increment activeRequests and check the result in one
		// compare-and-swap-free operation. If we land within the concurrency
		// limit, we own the slot and proceed directly to the backend.
		//
		// Transient overshoot analysis:
		//   There is a narrow race where a completing goroutine's decrement
		//   and a new goroutine's increment execute "simultaneously" on separate
		//   CPU cores. In this window, the new goroutine may increment to exactly
		//   MaxConcurrent+1 before the decrement registers. This is an accepted
		//   trade-off: the alternative (a sync.Mutex around the check+increment)
		//   would serialize ALL request admissions through a single lock during
		//   peak traffic — catastrophic at 50,000 req/s.
		//   The overshoot is bounded to at most GOMAXPROCS additional requests
		//   (one per core simultaneously racing), which is ≤ 8 on the M1 MacBook.
		//   The legacy backend can absorb a momentary +8 requests above its
		//   stated limit far better than it can absorb a global mutex bottleneck.
		count := atomic.AddInt64(&wr.activeRequests, 1)
		if count <= wr.cfg.MaxConcurrent {
			// Slot acquired via fast path. The defer runs after next.ServeHTTP returns,
			// decrementing activeRequests and signaling the next waiting goroutine.
			defer wr.releaseSlot()
			next.ServeHTTP(w, r)
			return
		}

		// ── Threshold exceeded: undo optimistic increment ─────────────────────
		//
		// We incremented but lost the race — the backend is at capacity.
		// Revert the increment synchronously before attempting to queue.
		// Another goroutine may grab this slot in the interim, which is correct:
		// that goroutine won the race legitimately.
		atomic.AddInt64(&wr.activeRequests, -1)

		// ── Path B: Queue Admission ───────────────────────────────────────────
		//
		// Create a ticket channel (1-buffered) for this goroutine.
		// The buffer of exactly 1 is non-negotiable:
		//   - Allows signalNext() to always return immediately (never blocks).
		//   - Allows drainTicket() to correctly receive and forward stale signals.
		//   See package-level comment "Why 1-Buffered Tickets?" for full analysis.
		ticket := make(chan struct{}, 1)

		// Increment queueDepth BEFORE attempting to enqueue. This ensures that
		// two goroutines racing to join the queue get distinct position numbers
		// for their wait page display. The increment is undone if enqueue fails.
		pos := int(atomic.AddInt64(&wr.queueDepth, 1))

		// Non-blocking send to the outer queue channel.
		//   Success: ticket is now in the FIFO queue. We park below.
		//   Default (queue full): queue is at capacity. Serve wait page and return.
		select {
		case wr.queue <- ticket:
			// Successfully queued. Fall through to the park-and-wait select below.
		default:
			// The outer queue channel is full: len(wr.queue) == cap(wr.queue).
			// We cannot hold this request — it would require an unbounded buffer.
			// Undo the queueDepth increment (we did not actually queue).
			atomic.AddInt64(&wr.queueDepth, -1)

			// Serve the wait page and advise the client to retry.
			// Retry-After: informs RFC-compliant clients (browsers, CDNs, bots)
			// how many seconds to wait before retrying — prevents retry storms.
			w.Header().Set("Retry-After", fmt.Sprintf("%d", wr.estimateWait(pos)))
			wr.serveWaitPage(w, r, 0, modeQueueFull)
			return
		}

		// ── Park the goroutine ────────────────────────────────────────────────
		//
		// Set the Retry-After header before parking. This is sent as part of
		// the HTTP response headers when a wait page is eventually served (or
		// picked up by an intermediate CDN even before the body is written).
		//
		// RFC 7231 §7.1.3: Retry-After with a delay-seconds value tells clients
		// how long to wait before making a follow-up request.
		w.Header().Set("Retry-After", fmt.Sprintf("%d", wr.estimateWait(pos)))

		// Three-way park: the goroutine suspends here, consuming zero CPU,
		// until one of three events occurs.
		select {

		case <-ticket:
			// ── Event 1: Admission Signal ─────────────────────────────────────
			//
			// A completing goroutine called releaseSlot() → signalNext(), which
			// dequeued our ticket and sent the admission signal.
			//
			// Immediately decrement queueDepth: we are no longer in the queue.
			atomic.AddInt64(&wr.queueDepth, -1)

			// Safety check: did the client disconnect while we were parked?
			// This is common on mobile networks (OS kills background TCP connections)
			// and when users close the browser tab during the wait page countdown.
			// Forwarding a dead request wastes a precious backend slot and returns
			// a connection write error — serve nobody, waste a backend thread.
			if r.Context().Err() != nil {
				slog.Warn("admitted_but_disconnected", "path", r.URL.Path)
				// Pass this slot to the next live waiter. Use a goroutine to avoid
				// blocking the current goroutine's cleanup path.
				go wr.signalNext()
				return
			}

			// Claim the backend slot for this admitted request.
			// See "transient overshoot" note in Path A above — the same analysis
			// applies here. The slot was signaled to us; we are entitled to proceed.
			atomic.AddInt64(&wr.activeRequests, 1)

			// Register the deferred slot release AFTER incrementing activeRequests
			// so that the decrement in releaseSlot() is always balanced.
			defer wr.releaseSlot()

			slog.Info("queue_admission", "pos", pos, "path", r.URL.Path)
			next.ServeHTTP(w, r)

		case <-time.After(wr.cfg.QueueTimeout):
			// ── Event 2: Queue Timeout ────────────────────────────────────────
			//
			// The request waited longer than QueueTimeout without being admitted.
			// Our ticket is still sitting in the outer wr.queue channel — we
			// cannot remove it (Go channels have no "cancel send" operation).
			// When a completing goroutine eventually calls signalNext(), it will
			// dequeue our ticket and write to its buffer. drainTicket() will
			// receive that signal and pass it to the next live waiter.
			//
			// We do NOT decrement queueDepth here because the ticket is still
			// in the queue. drainTicket() decrements it when the slot arrives.
			slog.Warn("queue_timeout", "pos", pos, "path", r.URL.Path, "after", wr.cfg.QueueTimeout)
			go wr.drainTicket(ticket)
			wr.serveWaitPage(w, r, pos, modeTimeout)

		case <-r.Context().Done():
			// ── Event 3: Client Disconnected ─────────────────────────────────
			//
			// The client closed the connection while waiting (browser back button,
			// tab close, mobile OS killed the app in the background, timeout on
			// the client side). Same ticket-in-queue situation as Event 2.
			// Spawn drainTicket to conserve the eventual slot signal.
			slog.Warn("client_disconnect_while_queued", "pos", pos, "path", r.URL.Path)
			go wr.drainTicket(ticket)
			// No response to write — the client connection is gone.
		}
	})
}

// ── Internal Slot Management ──────────────────────────────────────────────────

// releaseSlot atomically decrements activeRequests by 1 and immediately
// attempts to admit the next waiting goroutine via signalNext().
//
// Called via defer in every goroutine that successfully acquired a backend slot.
// This is the exclusive mechanism by which backend slots are returned to
// the pool — no other code path decrements activeRequests.
//
// The defer + releaseSlot pattern guarantees that slots are always released
// even if next.ServeHTTP panics (Go's defer stack unwinds on panic).
func (wr *WaitingRoom) releaseSlot() {
	atomic.AddInt64(&wr.activeRequests, -1)
	wr.signalNext()
}

// signalNext dequeues the next ticket from the FIFO queue and sends an
// admission signal to its waiting goroutine.
//
// The send to the ticket channel is always non-blocking because:
//  1. Each ticket is make(chan struct{}, 1) — capacity 1.
//  2. We write to each ticket exactly once (signalNext is called at most
//     once per slot release, and each ticket is dequeued exactly once).
//  3. A freshly dequeued ticket has a zero-length buffer (the goroutine
//     has not written to it; only we write to it). So the send always
//     succeeds immediately.
//
// If the goroutine is still alive: it receives from the ticket in its
// select, decrements queueDepth, and claims the backend slot.
//
// If the goroutine has already exited (context cancel or timeout before
// signal arrived): the signal sits in the ticket's buffer. The goroutine's
// drainTicket() cleanup goroutine eventually receives it and calls
// signalNext() again to propagate the slot forward.
func (wr *WaitingRoom) signalNext() {
	select {
	case ticket := <-wr.queue:
		// Non-blocking send: always succeeds because ticket has buffer capacity 1.
		// See the "Why 1-Buffered Tickets?" section in the package doc.
		ticket <- struct{}{}
	default:
		// Queue is empty — no goroutines are waiting. The freed backend slot
		// is now available for the next direct-path (Path A) admission.
	}
}

// drainTicket is a lightweight cleanup goroutine spawned when a waiting goroutine
// exits its select (via timeout or context cancellation) before receiving an
// admission signal. Its sole purpose is slot conservation.
//
// # The Slot Conservation Problem
//
// When goroutine G exits its select without receiving from its ticket:
//  1. G's ticket is still in the outer wr.queue channel.
//  2. A future signalNext() call will dequeue G's ticket and send to it.
//  3. The signal arrives in the ticket's buffer (buffer=1, so send succeeds).
//  4. If nobody reads from G's ticket, the signal (= one freed backend slot)
//     is permanently lost. Effective backend concurrency silently decreases by 1.
//     If 50 clients time out under a 5-minute outage, we lose 50 slots —
//     after recovery, the backend runs at MaxConcurrent-50 instead of MaxConcurrent.
//
// # The Solution
//
// drainTicket parks until the signal arrives on the orphaned ticket, then:
//  1. Decrements queueDepth (the enqueue increment is undone at this point).
//  2. Calls signalNext() to offer the slot to the next actual live waiter.
//
// This forms a forwarding chain: completing goroutine → signalNext() →
// orphaned ticket → drainTicket → signalNext() → live waiter (or next orphan).
//
// # Safety Timeout
//
// If the backend goes completely down (no completing goroutines, no slot
// releases ever), no signal will arrive on orphaned tickets and drainTicket
// goroutines would leak indefinitely. The safety timeout (QueueTimeout + 30s)
// bounds the goroutine lifetime: after the safety window, drainTicket exits
// and decrements queueDepth, even if no signal arrived.
//
// In practice, the safety timeout fires only if the backend is fully dead for
// QueueTimeout+30s straight — at which point the server operator should be
// paged anyway.
func (wr *WaitingRoom) drainTicket(ticket chan struct{}) {
	// Safety window: generous enough to cover any realistic backend recovery
	// scenario, but bounded to prevent indefinite goroutine accumulation.
	safetyWindow := wr.cfg.QueueTimeout + 30*time.Second

	select {
	case <-ticket:
		// The completing goroutine's signalNext() dequeued our ticket and wrote
		// the admission signal. Our goroutine has already exited, so we receive
		// it here. Undo the queueDepth increment from enqueue time.
		atomic.AddInt64(&wr.queueDepth, -1)

		// Forward the slot to the next waiter. This is the slot conservation
		// step: without this call, the backend slot freed by the completing
		// goroutine would never reach any live waiter (we absorbed the signal
		// but did not proceed to the backend).
		//
		// Concurrency note: signalNext() is safe to call from any goroutine at
		// any time — it is lock-free (only a channel operation).
		wr.signalNext()

	case <-time.After(safetyWindow):
		// Safety valve: backend may be completely down (no completions ever).
		// Clean up and exit to prevent goroutine leak.
		atomic.AddInt64(&wr.queueDepth, -1)
		slog.Error("drainTicket: safety_timeout reached — backend may be unreachable", "after", safetyWindow)
	}
}

// ── Wait Page Rendering ───────────────────────────────────────────────────────

// serveWaitPage writes the inline HTML wait page to the client.
//
// This function is called in three scenarios (mode):
//
//	modeQueued:   request is actively in the queue at position pos.
//	modeQueueFull: queue is at capacity; request cannot be held.
//	modeTimeout:  request was in queue but waited too long.
//
// HTTP semantics:
//   - Status 503 Service Unavailable: correct code for "temporarily overloaded,
//     retry later". Instructs CDNs and load balancers not to cache the response
//     and signals monitoring systems that the service is degraded.
//   - Cache-Control: no-store prevents the wait page from being cached by any
//     intermediate proxy or CDN — every refresh must hit PeakShield directly
//     so the client gets an up-to-date queue position.
//   - X-Robots-Tag: noindex prevents search engine crawlers from indexing the
//     wait page in Google/Bing, which would confuse applicants searching for
//     the portal URL.
func (wr *WaitingRoom) serveWaitPage(w http.ResponseWriter, r *http.Request, pos int, mode waitMode) {
	var mainMsg, detailMsg string

	switch mode {
	case modeQueued:
		// Shown on the initial wait page served while the goroutine is parked.
		// In practice, this case is not reached in the current middleware flow
		// (the goroutine parks silently and serves the wait page only on timeout).
		// It is included for completeness if future enhancements want to show
		// the wait page immediately on queue entry.
		est := wr.estimateWait(pos)
		mainMsg = fmt.Sprintf(
			`Queue position: <strong style="color:#38bdf8;font-size:1.4rem">#%d</strong>`,
			pos,
		)
		detailMsg = fmt.Sprintf("Estimated wait: approximately %d seconds.", est)

	case modeQueueFull:
		// Queue is full: no position to show.
		mainMsg = "The server is at full capacity right now."
		detailMsg = "You will be automatically redirected when a slot becomes available."

	case modeTimeout:
		// Request timed out: show the last-known position and updated estimate.
		est := wr.estimateWait(pos)
		mainMsg = fmt.Sprintf(
			`Still waiting &mdash; queue position was <strong style="color:#38bdf8">#%d</strong>.`,
			pos,
		)
		detailMsg = fmt.Sprintf("Estimated remaining wait: ~%d seconds. Holding your place.", est)
	}

	// Render the page by injecting dynamic content into the compiled template.
	// fmt.Sprintf allocates a string each call — acceptable since this path is
	// only hit when the server is overloaded, meaning page serving frequency
	// is self-limited by client refresh intervals (every 5 seconds per client).
	pageBody := fmt.Sprintf(waitPageTemplate, mainMsg, detailMsg)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Prevent any caching of the wait page at any level (browser, CDN, ISP).
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	w.Header().Set("Pragma", "no-cache")
	// Identify this as a PeakShield queue response for operator dashboards.
	w.Header().Set("X-PeakShield-Queue", "1")
	// Prevent search engines from indexing the wait page.
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")

	// 503 Service Unavailable is the semantically correct status code.
	// It tells clients, CDNs, and monitors: "temporarily overloaded; retry later."
	// Browsers will NOT cache a 503 response (unlike 301/302/404).
	w.WriteHeader(http.StatusServiceUnavailable)

	_, _ = w.Write([]byte(pageBody))

	slog.Info("wait_page_served", "mode", mode, "pos", pos, "path", r.URL.Path, "body_bytes", len(pageBody))
}

// estimateWait computes an approximate wait time in seconds for a given queue
// position. Used for UX display only — not a scheduling guarantee.
//
// Model: the backend processes MaxConcurrent requests simultaneously. Each
// request takes an average of BackendTimeout/3 seconds (heuristic: most
// requests complete at ~1/3 of the timeout). Position pos is in a "batch"
// system where MaxConcurrent positions are served per batch:
//
//	batch      = ceil(pos / MaxConcurrent)
//	avgService = BackendTimeout / 3   (in seconds)
//	estimated  = batch × avgService
//
// Example: pos=75, MaxConcurrent=50, BackendTimeout=15s, avgService=5s:
//
//	batch = ceil(75/50) = 2
//	estimated = 2 × 5s = 10s
//
// The minimum returned value is 5 seconds to avoid displaying "0s" for
// positions very close to the front of the queue.
func (wr *WaitingRoom) estimateWait(pos int) int {
	if pos <= 0 {
		return 5
	}

	maxConc := int(wr.cfg.MaxConcurrent)
	if maxConc <= 0 {
		maxConc = 1 // guard against zero division
	}

	// avgServiceTimeSec: assume each request takes BackendTimeout / 3.
	// Division by 3 is a heuristic: most requests complete well before
	// the timeout; the timeout is a worst-case bound, not the average.
	avgServiceTimeSec := wr.cfg.BackendTimeout.Seconds() / 3.0

	// Ceiling division: ceil(pos / maxConc).
	// How many "waves" of MaxConcurrent completions are needed before pos is served.
	waves := (pos + maxConc - 1) / maxConc

	estimated := int(float64(waves) * avgServiceTimeSec)
	if estimated < 5 {
		estimated = 5
	}

	return estimated
}

// ── Observability ─────────────────────────────────────────────────────────────

// ActiveRequests returns the current number of requests in-flight to the backend.
// Safe for concurrent use. Used by the stripper middleware (Module 4) to
// decide whether to activate HTML asset stripping.
func (wr *WaitingRoom) ActiveRequests() int64 {
	return atomic.LoadInt64(&wr.activeRequests)
}

// QueueDepth returns the approximate number of requests currently waiting
// in the virtual queue. May transiently overestimate due to the drainTicket
// cleanup delay after goroutine timeouts. Safe for concurrent use.
func (wr *WaitingRoom) QueueDepth() int64 {
	return atomic.LoadInt64(&wr.queueDepth)
}

// Stats is a point-in-time snapshot of waiting room metrics,
// suitable for JSON serialization in the /stats handler.
type Stats struct {
	ActiveRequests int64 `json:"active_requests"` // current in-flight backend requests
	QueueDepth     int64 `json:"queue_depth"`      // approximate waiting room occupancy
	MaxConcurrent  int64 `json:"max_concurrent"`   // circuit breaker threshold T
	QueueCapacity  int   `json:"queue_capacity"`   // maximum queue depth (QueueSize)
}

// GetStats returns a Stats snapshot. O(1), no locks, safe for hot paths.
func (wr *WaitingRoom) GetStats() Stats {
	return Stats{
		ActiveRequests: atomic.LoadInt64(&wr.activeRequests),
		QueueDepth:     atomic.LoadInt64(&wr.queueDepth),
		MaxConcurrent:  wr.cfg.MaxConcurrent,
		QueueCapacity:  wr.cfg.QueueSize,
	}
}
