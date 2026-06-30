package integration_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"peakshield/config"
	"peakshield/middleware"
	"peakshield/proxy"
	"peakshield/ratelimiter"
	"peakshield/stripper"
	"peakshield/waitingroom"
)

// setupIntegrationTest creates a full PeakShield pipeline pointing to a mock backend.
func setupIntegrationTest(t *testing.T, cfg *config.Config, backendHandler http.HandlerFunc) (*httptest.Server, *httptest.Server) {
	// 1. Mock Backend
	backend := httptest.NewServer(backendHandler)
	cfg.TargetURL = backend.URL

	// 2. Initialize PeakShield Components
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rl := ratelimiter.New(ctx, cfg)
	wr := waitingroom.New(cfg)
	st := stripper.New(cfg, wr.ActiveRequests)
	prx, err := proxy.New(cfg)
	if err != nil {
		t.Fatalf("Failed to create proxy: %v", err)
	}

	// 3. Rate Limiter Middleware Wrapper (same as main.go)
	rlMiddleware := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !rl.Allow("127.0.0.1") {
				http.Error(w, "429 Too Many Requests", http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}

	// 4. Build Middleware Chain
	proxyHandler := middleware.Chain(prx,
		rlMiddleware,
		wr.Middleware,
		st.Middleware,
	)

	// 5. PeakShield Frontend Server
	frontend := httptest.NewServer(proxyHandler)
	return frontend, backend
}

func TestRateLimiter_NetworkLayer(t *testing.T) {
	cfg := &config.Config{
		RateLimit:     5, // Allow 5 req/sec
		BurstSize:     5,
		MaxConcurrent: 100, // High enough to avoid queueing
	}

	backendHitCount := 0
	var mu sync.Mutex
	frontend, backend := setupIntegrationTest(t, cfg, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		backendHitCount++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	defer frontend.Close()
	defer backend.Close()

	client := frontend.Client()
	
	// Send 10 concurrent requests
	var wg sync.WaitGroup
	statusCounts := make(map[int]int)
	var countMu sync.Mutex

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := client.Get(frontend.URL)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			
			countMu.Lock()
			statusCounts[resp.StatusCode]++
			countMu.Unlock()
		}()
	}
	wg.Wait()

	if statusCounts[http.StatusOK] != 5 {
		t.Errorf("Expected 5 allowed requests, got %d", statusCounts[http.StatusOK])
	}
	if statusCounts[http.StatusTooManyRequests] != 5 {
		t.Errorf("Expected 5 rate-limited (429) requests, got %d", statusCounts[http.StatusTooManyRequests])
	}
}

func TestHTMLStripper_RealResponse(t *testing.T) {
	cfg := &config.Config{
		RateLimit:      100,
		BurstSize:      100,
		MaxConcurrent:  100,
		StripThreshold: -1, // Guarantee stripper activation
	}

	rawHTML := `<html><body><script>alert(1);</script><h1>Hello</h1></body></html>`
	expectedHTML := `<html><body><div id="peakshield-notice" style="background:#f59e0b;color:#fff;text-align:center;padding:10px;font-family:sans-serif;font-weight:bold;z-index:999999;position:relative;border-bottom:2px solid #b45309">⚠️ PeakShield Active: High Traffic Mode. Assets stripped to conserve bandwidth.</div><h1>Hello</h1></body></html>`

	frontend, backend := setupIntegrationTest(t, cfg, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(rawHTML))
	})
	defer frontend.Close()
	defer backend.Close()

	resp, err := http.Get(frontend.URL)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if string(body) != expectedHTML {
		t.Errorf("Stripper failed to strip script.\nExpected: %s\nGot: %s", expectedHTML, string(body))
	}
}

func TestWaitingRoom_Queueing(t *testing.T) {
	cfg := &config.Config{
		RateLimit:       100,
		BurstSize:       100,
		MaxConcurrent:   1, // Only 1 request allowed to backend at a time
		QueueSize:       10,
		QueueTimeout:    1 * time.Second, // Wait in queue before timing out
	}

	backendBlocker := make(chan struct{})
	backendEntered := make(chan struct{})

	frontend, backend := setupIntegrationTest(t, cfg, func(w http.ResponseWriter, r *http.Request) {
		backendEntered <- struct{}{}
		<-backendBlocker // Block until released
		w.WriteHeader(http.StatusOK)
	})
	defer frontend.Close()
	defer backend.Close()

	client := &http.Client{Timeout: 5 * time.Second}

	// 1st request enters backend
	go func() {
		_, _ = client.Get(frontend.URL)
	}()
	<-backendEntered // Ensure it's in the backend

	// 2nd request should queue and get a 503 waiting room response
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", frontend.URL, nil)
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Failed to queue request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("Expected 503 Waiting Room, got %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("Failed to read body: %v", err)
	}
	if !strings.Contains(string(body), "queue position was") && !strings.Contains(string(body), "High Traffic") {
		t.Errorf("Expected waiting room HTML, got %s", string(body))
	}

	close(backendBlocker) // Release backend
}
