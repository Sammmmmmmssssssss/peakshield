package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"peakshield/config"
)



func TestReverseProxy_HopByHopHeaders(t *testing.T) {
	cfg := &config.Config{
		TargetURL:          "", // Set dynamically in test
		BackendTimeout:     1 * time.Second,
		InsecureSkipVerify: true,
	}

	// Backend server to echo received headers
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check that hop-by-hop headers were removed
		if r.Header.Get("Connection") != "" {
			t.Errorf("Connection header was not stripped")
		}
		if r.Header.Get("Keep-Alive") != "" {
			t.Errorf("Keep-Alive header was not stripped")
		}
		if r.Header.Get("Proxy-Authenticate") != "" {
			t.Errorf("Proxy-Authenticate header was not stripped")
		}
		// Check X-Forwarded-For handling
		w.Header().Set("X-Received-XFF", r.Header.Get("X-Forwarded-For"))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	cfg.TargetURL = backend.URL
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("Failed to create proxy: %v", err)
	}

	req := httptest.NewRequest("GET", "/", nil)
	// Add hop-by-hop headers
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Keep-Alive", "timeout=5, max=100")
	req.Header.Set("Proxy-Authenticate", "Basic")
	// Set an existing X-Forwarded-For
	req.Header.Set("X-Forwarded-For", "203.0.113.1")
	req.RemoteAddr = "198.51.100.1:12345"

	w := httptest.NewRecorder()
	
	// Execute proxy (bypassing middleware chain just to test proxy core)
	p.ServeHTTP(w, req)

	res := w.Result()
	if res.StatusCode != http.StatusOK {
		t.Errorf("Expected 200 OK, got %d", res.StatusCode)
	}

	// Validate X-Forwarded-For was appended to, not overwritten
	xff := res.Header.Get("X-Received-XFF")
	if xff != "203.0.113.1, 198.51.100.1" {
		t.Errorf("Expected X-Forwarded-For to be '203.0.113.1, 198.51.100.1', got %q", xff)
	}
}

func TestReverseProxy_BackendError(t *testing.T) {
	cfg := &config.Config{
		TargetURL:      "http://localhost:1", // guaranteed to fail connection
		BackendTimeout: 1 * time.Second,
	}

	p, err := New(cfg)
	if err != nil {
		t.Fatalf("Failed to create proxy: %v", err)
	}

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()

	p.ServeHTTP(w, req)

	res := w.Result()
	if res.StatusCode != http.StatusBadGateway {
		t.Errorf("Expected 502 Bad Gateway for unreachable backend, got %d", res.StatusCode)
	}

	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("Failed to read body: %v", err)
	}
	if !strings.Contains(string(body), "backend_error") {
		t.Errorf("Expected body to contain 'backend_error', got %s", body)
	}
}

func TestReverseProxy_Timeout(t *testing.T) {
	cfg := &config.Config{
		TargetURL:      "", // Set dynamically
		BackendTimeout: 10 * time.Millisecond,
	}

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond) // deliberately slow to trigger timeout
	}))
	defer backend.Close()

	cfg.TargetURL = backend.URL
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("Failed to create proxy: %v", err)
	}

	req := httptest.NewRequest("GET", "/", nil)
	// Create context with a short timeout to simulate client disconnect, or test transport timeout.
	// We'll test the transport timeout.
	w := httptest.NewRecorder()
	
	p.ServeHTTP(w, req)

	res := w.Result()
	if res.StatusCode != http.StatusGatewayTimeout {
		t.Errorf("Expected 504 Gateway Timeout, got %d", res.StatusCode)
	}
}

func TestReverseProxy_ClientDisconnect(t *testing.T) {
	cfg := &config.Config{
		TargetURL:      "", // Set dynamically
		BackendTimeout: 1 * time.Second,
	}

	block := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block // wait
	}))
	defer backend.Close()
	defer close(block)

	cfg.TargetURL = backend.URL
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("Failed to create proxy: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", "/", nil).WithContext(ctx)
	w := httptest.NewRecorder()

	// Cancel context immediately to simulate client disconnect
	cancel()

	p.ServeHTTP(w, req)

	res := w.Result()
	// Should not have written anything (status code defaults to 200 in httptest.Recorder, but body should be empty)
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("Failed to read body: %v", err)
	}
	if len(body) > 0 {
		t.Errorf("Expected empty response on client disconnect, got %s", body)
	}
}
