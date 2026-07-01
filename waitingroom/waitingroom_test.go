package waitingroom

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"peakshield/config"
)

// handle is a test helper that wires the WaitingRoom Middleware around a
// backend handler and calls ServeHTTP — mirroring what middleware.Chain does
// in production.
func handle(wr *WaitingRoom, w http.ResponseWriter, r *http.Request, backend http.Handler) {
	wr.Middleware(backend).ServeHTTP(w, r)
}

// TestWaitingRoom_Stress fires 5,000 concurrent goroutines through a waiting
// room with MaxConcurrent=10. After all goroutines finish we assert that every
// slot has been returned (no leak) and every request was handled.
func TestWaitingRoom_Stress(t *testing.T) {
	cfg := &config.Config{
		MaxConcurrent: 10,
		QueueSize:     10000,
		QueueTimeout:  5 * time.Second,
	}
	wr := New(cfg)

	var handled int64
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&handled, 1)
		time.Sleep(1 * time.Millisecond)
	})

	const numRequests = 5000
	var wg sync.WaitGroup
	wg.Add(numRequests)
	for i := 0; i < numRequests; i++ {
		go func() {
			defer wg.Done()
			req := httptest.NewRequest("GET", "/", nil)
			handle(wr, httptest.NewRecorder(), req, backend)
		}()
	}
	wg.Wait()
	// Allow any drainTicket goroutines to finish.
	time.Sleep(200 * time.Millisecond)

	stats := wr.GetStats()
	if stats.ActiveRequests != 0 {
		t.Errorf("slot leak: expected 0 active requests after drain, got %d", stats.ActiveRequests)
	}
	if stats.QueueDepth != 0 {
		t.Errorf("slot leak: expected 0 queue depth after drain, got %d", stats.QueueDepth)
	}
	if handled != numRequests {
		t.Errorf("expected %d handled requests, got %d", numRequests, handled)
	}
}

// TestWaitingRoom_QueueFull verifies that a third request is immediately
// rejected with 503 when both the backend slot and the single queue slot are
// already taken.
func TestWaitingRoom_QueueFull(t *testing.T) {
	cfg := &config.Config{
		MaxConcurrent: 1,
		QueueSize:     1,
		QueueTimeout:  2 * time.Second,
	}
	wr := New(cfg)
	block := make(chan struct{})

	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	})

	var wg sync.WaitGroup
	wg.Add(2)

	// Request 1: occupies the only backend slot.
	go func() {
		defer wg.Done()
		handle(wr, httptest.NewRecorder(), httptest.NewRequest("GET", "/r1", nil), backend)
	}()
	time.Sleep(20 * time.Millisecond)

	// Request 2: backend full → enters the single queue slot.
	go func() {
		defer wg.Done()
		handle(wr, httptest.NewRecorder(), httptest.NewRequest("GET", "/r2", nil), backend)
	}()
	time.Sleep(20 * time.Millisecond)

	// Request 3: both backend and queue are full → must get 503 immediately.
	w := httptest.NewRecorder()
	handle(wr, w, httptest.NewRequest("GET", "/r3", nil), backend)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 Service Unavailable for queue-full, got %d", w.Code)
	}

	close(block)
	wg.Wait()
}

// TestWaitingRoom_Timeout verifies that a queued request that waits longer
// than QueueTimeout is served a 503 and that the slot is eventually released
// (no slot leak via drainTicket).
func TestWaitingRoom_Timeout(t *testing.T) {
	cfg := &config.Config{
		MaxConcurrent: 1,
		QueueSize:     5,
		QueueTimeout:  60 * time.Millisecond,
	}
	wr := New(cfg)
	block := make(chan struct{})

	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	})

	var wg sync.WaitGroup
	wg.Add(1)
	// Request 1: occupies backend slot, blocks indefinitely.
	go func() {
		defer wg.Done()
		handle(wr, httptest.NewRecorder(), httptest.NewRequest("GET", "/r1", nil), backend)
	}()
	time.Sleep(20 * time.Millisecond)

	// Request 2: queued, must timeout after 60 ms and get 503.
	w := httptest.NewRecorder()
	handle(wr, w, httptest.NewRequest("GET", "/r2", nil), backend)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 after queue timeout, got %d", w.Code)
	}

	// Release the blocked backend request so goroutines can finish.
	close(block)
	wg.Wait()
	// Give drainTicket() time to clean up the orphaned slot.
	time.Sleep(100 * time.Millisecond)

	stats := wr.GetStats()
	if stats.ActiveRequests != 0 {
		t.Errorf("expected 0 active requests after timeout+drain, got %d", stats.ActiveRequests)
	}
	if stats.QueueDepth != 0 {
		t.Errorf("expected 0 queue depth after timeout+drain, got %d", stats.QueueDepth)
	}
}

// TestWaitingRoom_ContextCanceled verifies that a client which disconnects
// while queued (context cancelled) does not permanently hold a backend slot.
func TestWaitingRoom_ContextCanceled(t *testing.T) {
	cfg := &config.Config{
		MaxConcurrent: 1,
		QueueSize:     5,
		QueueTimeout:  5 * time.Second,
	}
	wr := New(cfg)
	block := make(chan struct{})

	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	})

	var wg sync.WaitGroup
	wg.Add(1)
	// Request 1: occupies the only backend slot.
	go func() {
		defer wg.Done()
		handle(wr, httptest.NewRecorder(), httptest.NewRequest("GET", "/r1", nil), backend)
	}()
	time.Sleep(20 * time.Millisecond)

	// Request 2: queued; cancel its context after 50 ms to simulate disconnect.
	ctx, cancel := context.WithCancel(context.Background())
	r2 := httptest.NewRequest("GET", "/r2", nil).WithContext(ctx)
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	handle(wr, httptest.NewRecorder(), r2, backend)

	// Release Request 1 so the slot is freed and drainTicket can run.
	close(block)
	wg.Wait()
	time.Sleep(100 * time.Millisecond)

	stats := wr.GetStats()
	if stats.ActiveRequests != 0 {
		t.Errorf("expected 0 active requests after context cancel + drain, got %d", stats.ActiveRequests)
	}
	if stats.QueueDepth != 0 {
		t.Errorf("expected 0 queue depth after context cancel + drain, got %d", stats.QueueDepth)
	}
}
