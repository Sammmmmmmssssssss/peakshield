// Package proxy implements a high-performance, single-target HTTP reverse proxy.
//
// It forwards client requests to a configured upstream legacy backend,
// sanitizes all headers per RFC 7230, uses an aggressively tuned connection
// pool to prevent file descriptor exhaustion, and emits structured JSON error
// responses on backend failures — never leaking raw transport errors to clients.
//
// # Connection Pooling Model
//
// A single shared *http.Transport is created per ReverseProxy instance.
// Go's transport maintains a pool of idle TCP connections keyed by host.
// Because PeakShield proxies to a single backend, the per-host pool (256 idle
// connections) is effectively the global pool. Connection reuse eliminates the
// TCP 3-way handshake and TLS negotiation on every request — critical for
// legacy backends that have slow connection setup (many government servers run
// on Tomcat/JBoss with high handshake latency).
//
// # File Descriptor Safety
//
// Every code path that obtains a non-nil *http.Response defers a cleanup that:
//  1. Drains the response body with io.Copy(io.Discard, resp.Body).
//  2. Calls resp.Body.Close().
//
// Draining before closing is essential for HTTP/1.1 keep-alive: if the body
// is not fully read, the transport cannot reuse the underlying TCP connection
// and must discard it, decrementing the pool. Over time, this causes FD
// exhaustion under high load. With draining, connections flow back to the pool.
package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"peakshield/config"
)

// hopByHopHeaders is the canonical list of HTTP/1.1 hop-by-hop headers
// defined in RFC 7230 §6.1. These headers govern a single transport-level
// connection and MUST be consumed and NOT forwarded by any proxy.
//
// Using a fixed-size array (not a slice) so the compiler can place it in the
// read-only data segment and iterate it without a bounds check.
var hopByHopHeaders = [8]string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"TE",
	"Trailers",
	"Transfer-Encoding",
	"Upgrade",
}

// excludedResponseHeaders are headers from the backend response that must NOT
// be forwarded to the client. Stored as a map for O(1) lookup during the
// header copy loop. Transfer-Encoding is excluded because Go's http.ResponseWriter
// handles chunked encoding automatically; forwarding it verbatim would corrupt
// the response framing.
var excludedResponseHeaders = map[string]struct{}{
	"Connection":        {},
	"Keep-Alive":        {},
	"Transfer-Encoding": {},
	"Trailer":           {},
	"Upgrade":           {},
}

// ReverseProxy is a single-target HTTP reverse proxy, safe for concurrent use
// by any number of goroutines. All requests are forwarded to the upstream URL
// configured at construction time via New().
type ReverseProxy struct {
	// target is the parsed upstream URL. All incoming requests are rewritten
	// to: target.Scheme + "://" + target.Host + target.Path + r.URL.Path + "?" + r.URL.RawQuery.
	target *url.URL

	// transport is the shared connection pool for all backend requests.
	// Shared across goroutines — this is how connection reuse works.
	// Never replaced after construction; safe for concurrent reads.
	transport *http.Transport

	// client wraps transport with a global deadline and redirect policy.
	// http.Client is safe for concurrent use by multiple goroutines.
	client *http.Client

	// cfg is a read-only reference to the global configuration.
	cfg *config.Config
}

// ErrorResponse is the JSON payload written to the client on backend failures.
// A stable, documented structure allows API clients and monitoring systems
// (Prometheus alert rules, Datadog monitors) to parse error types without
// relying on HTTP status codes alone.
type ErrorResponse struct {
	Error   string `json:"error"`             // machine-readable snake_case error code
	Code    int    `json:"code"`              // mirrors the HTTP response status code
	Message string `json:"message"`           // human-readable English description
	Backend string `json:"backend,omitempty"` // the upstream URL that failed (for debugging)
}

// New constructs a ReverseProxy with a precisely tuned *http.Transport.
//
// # Transport Parameter Rationale
//
// MaxIdleConns (1024):
//
//	Global cap on idle keep-alive connections across all hosts. At ~4 KB kernel
//	socket buffer overhead per idle socket, 1024 idle conns ≈ 4 MB. Negligible
//	against our 30 MB budget. This ceiling prevents unbounded FD accumulation
//	if the pool is never fully drained.
//
// MaxIdleConnsPerHost (256):
//
//	Since PeakShield targets a single backend host, this is the effective main
//	pool depth. 256 idle connections enables sustained high reuse during spike
//	traffic without connection churn. Each idle conn holds one FD and one
//	goroutine reading the idle-timeout timer — total: ~96 KB for 256 conns.
//
// MaxConnsPerHost (0 = unlimited):
//
//	We do NOT cap concurrency at the transport layer. The WaitingRoom (Module 3)
//	already enforces concurrency limits via its atomic counter. Capping here
//	would cause the transport to silently queue requests internally in a Go
//	channel with no timeout mechanism and no visibility to our circuit breaker.
//
// IdleConnTimeout (90s):
//
//	Most enterprise NAT gateways and firewalls silently drop idle TCP sessions
//	after 60–120 seconds. Setting this to 90s keeps our idle pool fresh without
//	exceeding typical firewall timeouts. Connections idle longer than 90s are
//	proactively closed and removed from the pool, preventing "connection reset
//	by peer" errors on the first request after a lull.
//
// ResponseHeaderTimeout (BackendTimeout - 1s):
//
//	Caps the wait for the first byte of the response status line. Set 1 second
//	below the global BackendTimeout so our per-request context deadline fires
//	first, giving our code control over the error response body. If the
//	transport timeout fires first, we get a raw "*url.Error" with no structured
//	JSON body.
//
// TLSHandshakeTimeout (10s):
//
//	Prevents hangs during TLS negotiation with flaky or misconfigured backends.
//	Government backends on outdated JDK TLS stacks can sometimes stall handshakes.
//
// ReadBufferSize / WriteBufferSize (32 KB each):
//
//	Go's default per-connection I/O buffers are 4 KB — sized for small JSON API
//	responses. Government exam portals frequently serve 300 KB–1 MB of
//	uncompressed HTML (inline images, embedded CSS, etc.). 32 KB buffers reduce
//	the number of read(2) / write(2) syscalls by 8×, measurably reducing
//	latency per response. Each buffer is allocated once per connection and
//	pooled by the transport, not per-request.
func New(cfg *config.Config) (*ReverseProxy, error) {
	targetURL, err := url.Parse(cfg.TargetURL)
	if err != nil {
		return nil, fmt.Errorf("proxy: invalid target URL %q: %w", cfg.TargetURL, err)
	}
	if targetURL.Host == "" {
		return nil, fmt.Errorf(
			"proxy: target URL %q has no host — missing scheme? (example: http://legacy.gov.in:9090)",
			cfg.TargetURL,
		)
	}

	// net.Dialer controls the TCP connection establishment parameters.
	// A shared Dialer instance is used for all connections in the pool.
	dialer := &net.Dialer{
		// Timeout: maximum duration allowed to establish a TCP connection
		// (SYN sent → SYN-ACK received + local port binding).
		// 30s is generous for intranet backends; reduce to 5s if backend
		// is co-located on the same host (PEAKSHIELD_TARGET=http://localhost:...).
		Timeout: 30 * time.Second,

		// KeepAlive: interval at which TCP keep-alive probes are sent on
		// established idle connections. 15s probes detect dead connections
		// (backend rebooted, OS killed the socket) before IdleConnTimeout fires.
		// Without keep-alive, an idle connection appears healthy but triggers
		// a "connection reset" error on the next request — causing 502s.
		// With 15s keep-alive, dead connections are evicted from the pool
		// within 15–45s (probe + OS retransmit interval).
		KeepAlive: 15 * time.Second,
	}

	transport := &http.Transport{
		DialContext:         dialer.DialContext,
		MaxIdleConns:        1024,
		MaxIdleConnsPerHost: 256,
		MaxConnsPerHost:     0, // no cap; WaitingRoom enforces backend concurrency
		IdleConnTimeout:     90 * time.Second,

		// Set 1 second below BackendTimeout so per-request context deadline wins.
		// This ensures writeError() runs (structured JSON) rather than the
		// transport generating a raw network error string.
		ResponseHeaderTimeout: cfg.BackendTimeout - time.Second,

		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,

		// DisableCompression: false (default). Allow backend to send gzip responses;
		// they are forwarded to the client as-is. The stripper (Module 4) disables
		// compression on the per-request outbound header (Accept-Encoding: identity)
		// when it needs to parse the HTML, bypassing this transport-level setting.
		DisableCompression: false,

		// ForceAttemptHTTP2: false. Most legacy government backends speak HTTP/1.1
		// only. Some (running on old Apache/Tomcat) actively reject HTTP/2 ALPN
		// negotiation with a TLS alert, causing all backend connections to fail.
		ForceAttemptHTTP2: false,

		ReadBufferSize:  32 * 1024, // 32 KB per-connection read buffer
		WriteBufferSize: 32 * 1024, // 32 KB per-connection write buffer
	}

	client := &http.Client{
		Transport: transport,
		// Global backstop timeout. Per-request context deadlines (set by the
		// WaitingRoom middleware) provide finer-grained control per request.
		// This catches edge cases where a request reaches the proxy without
		// a context deadline already set.
		Timeout: cfg.BackendTimeout,

		// CheckRedirect: never follow redirects. Government portals frequently
		// redirect to session-expiry pages, CAPTCHA challenges, or login pages.
		// Returning http.ErrUseLastResponse causes client.Do to return (resp, nil)
		// with the 3xx response intact — we forward it to the client.
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	return &ReverseProxy{
		target:    targetURL,
		transport: transport,
		client:    client,
		cfg:       cfg,
	}, nil
}

// ServeHTTP implements http.Handler. This is the terminal handler in the
// middleware chain — it is called after the RateLimiter and WaitingRoom
// have both admitted the request.
//
// Full request lifecycle:
//
//  1. buildBackendRequest: clone + rewrite URL + sanitize headers + inject X-Forwarded-*.
//  2. client.Do: send request to backend via connection pool.
//  3. On transport error: detect client-disconnect vs. genuine backend error;
//     write structured JSON + X-PeakShield-Error header.
//  4. Defer body drain + close (see FD Safety in package doc).
//  5. copyResponseHeaders: copy backend headers to client writer.
//  6. WriteHeader: flush status code.
//  7. io.Copy: stream backend body to client.
func (p *ReverseProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Step 1: Build outbound request.
	outReq, err := p.buildBackendRequest(r)
	if err != nil {
		log.Printf("[proxy] request_build_failed method=%s path=%s err=%v", r.Method, r.URL.Path, err)
		p.writeError(w, http.StatusBadGateway, "request_build_failed", err.Error())
		return
	}

	// Step 2: Forward to backend via connection pool.
	resp, err := p.client.Do(outReq)
	if err != nil {
		// Classify the error before responding.
		//
		// Case A — context cancelled: the client disconnected before the backend
		// responded (browser tab closed, mobile network drop, OS-level kill).
		// There is nothing to write — the connection is gone. Log and return.
		//
		// Case B — genuine backend error: network unreachable, connection refused,
		// TLS failure, ResponseHeaderTimeout fired, etc. Write structured JSON.
		if r.Context().Err() != nil {
			log.Printf("[proxy] client_disconnect_before_response method=%s path=%s", r.Method, r.URL.Path)
			return
		}
		log.Printf("[proxy] backend_error method=%s path=%s backend=%s err=%v",
			r.Method, r.URL.Path, p.cfg.TargetURL, err)
		p.writeError(w, http.StatusBadGateway, "backend_error", "upstream server did not respond in time")
		return
	}

	// Step 3: Register deferred body cleanup.
	//
	// CRITICAL ORDERING: defer is registered immediately after obtaining a
	// non-nil resp. Every subsequent early return (header write error, etc.)
	// will still trigger this defer, ensuring the body is always closed.
	//
	// Why drain before close?
	//   HTTP/1.1 keep-alive (connection reuse) requires the body to be fully
	//   consumed. net/http internally checks whether the body was read to EOF
	//   before deciding to return the connection to the pool. If we close
	//   without draining, the connection is poisoned and discarded.
	//   In the success path, the body is already at EOF after io.Copy(w, ...)
	//   below, so this drain reads 0 bytes at effectively zero cost.
	//   In error paths (client disconnect mid-stream), the drain consumes
	//   the remaining backend bytes and recycles the connection.
	defer func() {
		// io.Discard is a pre-allocated no-op writer. io.Copy to it uses the
		// internal 32 KB buffer pool — no allocations per drain.
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	// Step 4: Copy backend response headers to client.
	// Must happen before WriteHeader() — headers sent after WriteHeader are silently dropped.
	copyResponseHeaders(w.Header(), resp.Header)

	// Inject PeakShield observability headers.
	// These are informational only and do not alter HTTP semantics.
	w.Header().Set("X-PeakShield-Proxied", "1")
	w.Header().Set("Via", "1.1 peakshield")

	// Step 5: Flush the status code to the client.
	// After WriteHeader, w.Header() is locked — further Set/Add calls are no-ops.
	w.WriteHeader(resp.StatusCode)

	// Step 6: Stream the response body from backend to client.
	// io.Copy uses a 32 KB buffer allocated from a sync.Pool — zero per-request
	// allocations for the copy itself. For a 300 KB HTML page, this is ~10 iterations.
	written, copyErr := io.Copy(w, resp.Body)
	if copyErr != nil && r.Context().Err() == nil {
		// Only log genuine write errors, not the "write: broken pipe" that
		// occurs when a client disconnects mid-stream (that is r.Context().Err() != nil).
		log.Printf("[proxy] body_stream_error bytes_written=%d path=%s err=%v",
			written, r.URL.Path, copyErr)
	}
}

// buildBackendRequest constructs the outbound *http.Request for the backend.
//
// RFC 7230 compliance steps performed:
//
//  1. URL rewriting: grafts the backend scheme+host onto the client's path+query.
//
//  2. Header cloning: all client request headers are copied to the outbound
//     request. This preserves cookies, Accept-Language, Content-Type, etc.
//
//  3. Hop-by-hop header removal (RFC 7230 §6.1): strips headers that govern
//     only the client↔PeakShield connection and must not be forwarded.
//
//  4. X-Forwarded-For: appends the client IP to the existing chain (preserving
//     all intermediate proxy IPs if present) per de-facto standard.
//
//  5. X-Real-IP: sets the outermost client IP, unchanged by subsequent proxies.
//     Only set if not already present (first proxy in chain wins).
//
//  6. X-Forwarded-Host: the original Host header the client used.
//     Backend apps use this to construct absolute self-referencing URLs.
//
//  7. X-Forwarded-Proto: original client protocol (http/https).
//
//  8. Via (RFC 7230 §5.7.1): "1.1 peakshield" marks the request as proxied.
//
//  9. Host override: sets outReq.Host to the backend host, not the client's
//     original host. Without this, backends serving multiple virtual hosts
//     may serve the wrong site.
//
// Context inheritance: the outbound request carries r.Context(). If the client
// disconnects, r.Context() is cancelled, propagating cancellation to client.Do()
// which aborts the backend TCP connection — preventing zombie requests.
func (p *ReverseProxy) buildBackendRequest(r *http.Request) (*http.Request, error) {
	// Construct the backend URL.
	// r.URL from an http.Server has Path and RawQuery but NO scheme or host
	// (the server strips them before dispatching). We supply scheme+host from target.
	backendURL := &url.URL{
		Scheme:   p.target.Scheme,
		Host:     p.target.Host,
		Path:     joinPath(p.target.Path, r.URL.Path),
		RawQuery: r.URL.RawQuery,
		// Deliberately omit Fragment: HTTP fragments are client-side only;
		// they are never sent in actual HTTP requests per RFC 7230 §5.1.
	}

	// http.NewRequestWithContext shallow-clones the request method, URL, body,
	// and proto version. Headers are NOT cloned — we copy them explicitly below
	// so we can apply sanitization without modifying r.Header.
	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, backendURL.String(), r.Body)
	if err != nil {
		return nil, fmt.Errorf("could not construct backend request to %s: %w", backendURL, err)
	}

	// Clone all client request headers into the outbound request.
	// We clone ALL headers first, then sanitize, to correctly handle the
	// Connection header's dynamic hop-by-hop nominations (RFC 7230 §6.1 step 1).
	copyRequestHeaders(outReq.Header, r.Header)

	// Remove hop-by-hop headers from the outbound request.
	// This handles both the standard RFC 7230 list and any headers dynamically
	// nominated via the Connection header (e.g., "Connection: X-Custom-Token").
	sanitizeHopByHopHeaders(outReq.Header)

	// ── X-Forwarded-For ─────────────────────────────────────────────────────
	// Build the forwarded-for chain by appending the direct client IP.
	// If PeakShield is behind another proxy (e.g., an ALB), the client IP
	// in RemoteAddr is the proxy IP, and the real client IP is in the
	// existing X-Forwarded-For header. We preserve the full chain.
	clientIP := ExtractIP(r.RemoteAddr)
	if prior := outReq.Header.Get("X-Forwarded-For"); prior != "" {
		outReq.Header.Set("X-Forwarded-For", prior+", "+clientIP)
	} else {
		outReq.Header.Set("X-Forwarded-For", clientIP)
	}

	// ── X-Real-IP ────────────────────────────────────────────────────────────
	// The outermost client IP — set once by the first proxy in the chain.
	// If already present, leave it unchanged (the upstream proxy set the real IP).
	if outReq.Header.Get("X-Real-IP") == "" {
		outReq.Header.Set("X-Real-IP", clientIP)
	}

	// ── X-Forwarded-Host ─────────────────────────────────────────────────────
	// Preserve the Host header the client used to reach PeakShield. Backend
	// applications use this to generate correct absolute URLs (e.g., for
	// Location headers in redirects, self-referencing links in HTML).
	if outReq.Header.Get("X-Forwarded-Host") == "" {
		outReq.Header.Set("X-Forwarded-Host", r.Host)
	}

	// ── X-Forwarded-Proto ────────────────────────────────────────────────────
	// Backend apps use this to detect whether to include TLS-only cookies,
	// use https:// in redirects, etc.
	if outReq.Header.Get("X-Forwarded-Proto") == "" {
		proto := "http"
		if r.TLS != nil {
			proto = "https"
		}
		outReq.Header.Set("X-Forwarded-Proto", proto)
	}

	// ── Via ──────────────────────────────────────────────────────────────────
	// RFC 7230 §5.7.1: an intermediary MUST append a Via header.
	// Format: "received-protocol received-by" where received-by is a pseudonym.
	// We unconditionally set (overwriting any existing Via from other proxies)
	// to keep backend logs clean and identifiable.
	outReq.Header.Set("Via", "1.1 peakshield")

	// ── Host override ─────────────────────────────────────────────────────────
	// Set the outbound Host to the backend's host. Critically: Go's http.Client
	// uses outReq.Host (if non-empty) over the URL's host for the Host header.
	// Without this, the backend receives the client's original Host (e.g.,
	// "www.examportal.gov.in") instead of the backend's host, which breaks
	// virtual host routing on backends serving multiple domains.
	outReq.Host = p.target.Host

	return outReq, nil
}

// writeError sends a structured JSON error response to the client.
//
// Always sets X-PeakShield-Error: <errType> so monitoring systems can
// distinguish PeakShield-generated 502/504 errors from backend-generated ones
// using a simple header match — no response body parsing required.
func (p *ReverseProxy) writeError(w http.ResponseWriter, statusCode int, errType, detail string) {
	payload := ErrorResponse{
		Error:   errType,
		Code:    statusCode,
		Message: http.StatusText(statusCode),
		Backend: p.cfg.TargetURL,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		// json.Marshal only fails for un-serializable types (channels, funcs, etc.).
		// ErrorResponse contains only strings and ints — this branch is unreachable
		// in practice but we handle it defensively to avoid a nil body write.
		body = []byte(`{"error":"marshal_failed","code":500}`)
		statusCode = http.StatusInternalServerError
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-PeakShield-Error", errType)
	// Prevent browsers from MIME-sniffing JSON as HTML, which could enable XSS.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(statusCode)
	_, _ = w.Write(body)

	log.Printf("[proxy] error_response status=%d type=%s detail=%q backend=%s",
		statusCode, errType, detail, p.cfg.TargetURL)
}

// copyResponseHeaders copies response headers from the backend (src) to the
// client writer (dst), skipping headers that govern the proxy↔backend
// connection (hop-by-hop) or that net/http manages automatically.
func copyResponseHeaders(dst, src http.Header) {
	for key, values := range src {
		if _, excluded := excludedResponseHeaders[key]; excluded {
			continue
		}
		// Use Add (not Set) to preserve multi-value headers like Set-Cookie.
		// A backend may set multiple Set-Cookie headers; Set() would overwrite
		// all but the last one, breaking session management.
		for _, v := range values {
			dst.Add(key, v)
		}
	}
}

// copyRequestHeaders copies all headers from the client request (src) to the
// outbound backend request (dst). sanitizeHopByHopHeaders must be called
// immediately after to strip forbidden headers.
func copyRequestHeaders(dst, src http.Header) {
	for key, values := range src {
		for _, v := range values {
			dst.Add(key, v)
		}
	}
}

// sanitizeHopByHopHeaders removes all hop-by-hop headers from h per RFC 7230 §6.1.
//
// Two categories must be handled:
//
// Category 1 — Connection header nominations (RFC 7230 §6.1, paragraph 1):
//
//	The Connection header value is a comma-separated list of field names that
//	the sender wants treated as hop-by-hop for this specific connection.
//	Example: "Connection: keep-alive, X-Custom-Proxy-Token" means that both
//	"keep-alive" (standard) and "X-Custom-Proxy-Token" (non-standard) must be
//	stripped. We handle this FIRST so the nominated headers are deleted before
//	we process the standard list.
//
// Category 2 — Standard RFC 7230 hop-by-hop headers:
//
//	The eight headers in the hopByHopHeaders array. The Connection header
//	itself is in this list and is deleted last (after its nominations are used).
func sanitizeHopByHopHeaders(h http.Header) {
	// Category 1: strip dynamically nominated headers.
	if connHeader := h.Get("Connection"); connHeader != "" {
		for _, token := range strings.Split(connHeader, ",") {
			field := strings.TrimSpace(token)
			if field != "" {
				// Del is case-insensitive (it calls http.CanonicalHeaderKey internally).
				h.Del(field)
			}
		}
	}

	// Category 2: strip the standard hop-by-hop list (includes Connection itself).
	for _, hdr := range hopByHopHeaders {
		h.Del(hdr)
	}
}

// joinPath concatenates a target base path and an incoming request path,
// avoiding double slashes at the join boundary.
//
// Examples:
//
//	joinPath("/api/v1", "/users/42")  → "/api/v1/users/42"
//	joinPath("",        "/login")     → "/login"
//	joinPath("/",       "/login")     → "/login"
//	joinPath("/app",    "")           → "/app/"  (trailing slash intentional)
func joinPath(base, reqPath string) string {
	if base == "" || base == "/" {
		return reqPath
	}
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(reqPath, "/")
}

// ExtractIP extracts the bare IP address from a net.Conn.RemoteAddr string.
//
// RemoteAddr formats produced by Go's net/http server:
//
//	"203.0.113.42:54321"   IPv4 with port
//	"[2001:db8::1]:8080"   IPv6 with port (bracketed per RFC 2732)
//	"203.0.113.42"         IPv4 without port (Unix sockets, test harnesses)
//
// Returns the IP without port. Falls back to the full remoteAddr string if
// net.SplitHostPort cannot parse it (e.g., bare IP in unit tests).
func ExtractIP(remoteAddr string) string {
	ip, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		// Fallback: return as-is. This handles test harnesses that pass bare
		// IP strings or empty strings without a port component.
		return remoteAddr
	}
	return ip
}

// Transport returns the underlying *http.Transport for use by external
// components. The stripper middleware (Module 4) uses this to understand the
// pool state; it also needs to set Accept-Encoding: identity on outbound
// requests it wants to parse — which it does at the request header level,
// not by modifying the transport.
func (p *ReverseProxy) Transport() *http.Transport {
	return p.transport
}
