package stripper

import (
	"bytes"
	"net/http"
	"strings"
	"sync"

	"peakshield/config"
)

// Stripper is a high-performance, low-allocation HTML token scanner middleware.
// It intercepts downstream text/html responses and dynamically strips heavy
// assets (scripts, large styles, images) when the backend is under heavy load.
type Stripper struct {
	cfg        *config.Config
	activeReqs func() int64
	pool       sync.Pool
}

// New creates a new Stripper middleware.
func New(cfg *config.Config, activeReqs func() int64) *Stripper {
	return &Stripper{
		cfg:        cfg,
		activeReqs: activeReqs,
		pool: sync.Pool{
			New: func() interface{} {
				// 64KB initial buffer minimizes allocations for average legacy pages
				return bytes.NewBuffer(make([]byte, 0, 64*1024))
			},
		},
	}
}

// stripperWriter wraps http.ResponseWriter to buffer and intercept HTML responses.
type stripperWriter struct {
	http.ResponseWriter
	buf           *bytes.Buffer
	status        int
	headerWritten bool
	isHTML        bool
	active        bool
}

// WriteHeader intercepts the status code and detects the Content-Type.
func (w *stripperWriter) WriteHeader(statusCode int) {
	if w.headerWritten {
		return
	}
	w.status = statusCode
	w.headerWritten = true

	if w.active {
		contentType := w.ResponseWriter.Header().Get("Content-Type")
		if strings.Contains(strings.ToLower(contentType), "text/html") {
			w.isHTML = true
			// Strip Content-Length; the stripped body will have a different length.
			// Go's net/http will automatically recalculate it or use Transfer-Encoding: chunked.
			w.ResponseWriter.Header().Del("Content-Length")
			return
		}
	}
	// Pass through headers immediately if we aren't intercepting HTML
	w.ResponseWriter.WriteHeader(statusCode)
}

// Write intercepts the body chunks.
func (w *stripperWriter) Write(b []byte) (int, error) {
	if !w.headerWritten {
		// http.ResponseWriter automatically detects Content-Type on first Write
		// if WriteHeader wasn't called. Trigger our detection logic.
		if w.ResponseWriter.Header().Get("Content-Type") == "" {
			w.ResponseWriter.Header().Set("Content-Type", http.DetectContentType(b))
		}
		w.WriteHeader(http.StatusOK)
	}

	if w.isHTML {
		return w.buf.Write(b)
	}
	// Direct passthrough for non-HTML assets (CSS, JS, images)
	return w.ResponseWriter.Write(b)
}

// Middleware returns the HTTP handler that conditionally strips HTML.
func (s *Stripper) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Fast path: if traffic is below the threshold, bypass the stripper entirely.
		if s.activeReqs() <= s.cfg.StripThreshold {
			next.ServeHTTP(w, r)
			return
		}

		// Prevent the backend from sending compressed responses (gzip/br) so we can scan the raw HTML.
		// Stripping the header here is vastly more memory-efficient than allocating decompression readers.
		r.Header.Del("Accept-Encoding")

		// Activate stripper: acquire a buffer from the pool
		buf := s.pool.Get().(*bytes.Buffer)
		buf.Reset()
		defer s.pool.Put(buf)

		sw := &stripperWriter{
			ResponseWriter: w,
			buf:            buf,
			active:         true,
		}

		// Execute downstream handlers (Proxy)
		next.ServeHTTP(sw, r)

		if sw.isHTML {
			// Acquire an output buffer
			outBuf := s.pool.Get().(*bytes.Buffer)
			outBuf.Reset()
			defer s.pool.Put(outBuf)

			// Perform zero-regex token scanning and stripping
			stripHTML(sw.buf.Bytes(), outBuf)

			// Write headers and the modified body downstream
			w.WriteHeader(sw.status)
			w.Write(outBuf.Bytes())
		}
	})
}

// stripHTML performs a single-pass, zero-regex string tokenization to strip heavy
// assets while maintaining strict memory bounds.
func stripHTML(input []byte, out *bytes.Buffer) {
	pos := 0
	bodyInjected := false
	notice := []byte(`<div id="peakshield-notice" style="background:#f59e0b;color:#fff;text-align:center;padding:10px;font-family:sans-serif;font-weight:bold;z-index:999999;position:relative;border-bottom:2px solid #b45309">⚠️ PeakShield Active: High Traffic Mode. Assets stripped to conserve bandwidth.</div>`)

	for pos < len(input) {
		// Fast-forward to the next tag opening
		start := bytes.IndexByte(input[pos:], '<')
		if start == -1 {
			out.Write(input[pos:])
			break
		}

		out.Write(input[pos : pos+start])
		pos += start
		rem := input[pos:]

		// 1. Strip <script> blocks
		if hasTagPrefix(rem, "<script") {
			end := indexFold(rem, "</script>")
			if end != -1 {
				pos += end + 9 // length of "</script>"
				continue
			}
		}

		// 2. Strip <style> blocks larger than 2KB
		if hasTagPrefix(rem, "<style") {
			end := indexFold(rem, "</style>")
			if end != -1 {
				blockLen := end + 8 // length of "</style>"
				if blockLen > 2048 {
					pos += blockLen // Strip
					continue
				}
				// Keep small styles
				out.Write(rem[:blockLen])
				pos += blockLen
				continue
			}
		}

		// 3. Strip stylesheet <link> tags
		if hasTagPrefix(rem, "<link") {
			end := bytes.IndexByte(rem, '>')
			if end != -1 {
				tag := rem[:end+1]
				if indexFold(tag, "stylesheet") != -1 {
					pos += end + 1 // Strip
					continue
				}
				// Keep non-stylesheet links (e.g., canonical, icon)
				out.Write(tag)
				pos += end + 1
				continue
			}
		}

		// 4. Strip <img> tags (including heavy base64 inline images)
		if hasTagPrefix(rem, "<img") {
			end := bytes.IndexByte(rem, '>')
			if end != -1 {
				out.WriteString("<span>[img]</span>")
				pos += end + 1
				continue
			}
		}

		// 5. Inject PeakShield notice immediately after <body>
		if !bodyInjected && hasTagPrefix(rem, "<body") {
			end := bytes.IndexByte(rem, '>')
			if end != -1 {
				out.Write(rem[:end+1]) // write <body> tag
				out.Write(notice)      // inject banner
				pos += end + 1
				bodyInjected = true
				continue
			}
		}

		// Not a special tag, or malformed tag missing '>'. Emit '<' and advance by 1
		// This makes the parser extremely robust against malformed legacy HTML.
		out.WriteByte('<')
		pos++
	}
}

// hasTagPrefix performs a case-insensitive, zero-allocation prefix match.
// It also enforces word boundaries to ensure `<script` doesn't match `<scriptures`.
func hasTagPrefix(b []byte, prefix string) bool {
	if len(b) < len(prefix) {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		c := b[i]
		if c >= 'A' && c <= 'Z' {
			c += 32 // lowercase
		}
		if c != prefix[i] {
			return false
		}
	}
	// Check word boundary
	if len(b) > len(prefix) {
		next := b[len(prefix)]
		if next != '>' && next != ' ' && next != '\t' && next != '\n' && next != '\r' && next != '/' {
			return false
		}
	}
	return true
}

// indexFold performs a zero-allocation, case-insensitive substring search.
func indexFold(b []byte, target string) int {
	if len(target) == 0 {
		return 0
	}
	n := len(b) - len(target)
	for i := 0; i <= n; i++ {
		match := true
		for j := 0; j < len(target); j++ {
			c := b[i+j]
			if c >= 'A' && c <= 'Z' {
				c += 32
			}
			if c != target[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
