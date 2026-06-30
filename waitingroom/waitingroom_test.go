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

func TestWaitingRoom_Stress(t *testing.T) {
	cfg := &config.Config{
		MaxConcurrent: 10,
		QueueSize:     10000,
		QueueTimeout:  5 * time.Second,
	}

	wr := New(cfg)

	// Mock backend that just increments a counter and returns immediately
	var handled int64
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&handled, 1)
		time.Sleep(1 * time.Millisecond) // simulate a tiny bit of work
	})

	const numRequests = 5000
	var wg sync.WaitGroup
	wg.Add(numRequests)

	for i := 0; i < numRequests; i++ {
		go func() {
			defer wg.Done()
			req := httptest.NewRequest("GET", "/", nil)
			w := httptest.NewRecorder()
			wr.Handle(w, req, backend)
		}()
	}

	wg.Wait()

	// Wait for queue and active requests to fully drain
	time.Sleep(100 * time.Millisecond)

	stats := wr.GetStats()
	if stats.ActiveRequests != 0 {
		t.Errorf("expected 0 active requests, got %d", stats.ActiveRequests)
	}
	if stats.QueueDepth != 0 {
		t.Errorf("expected 0 queue depth, got %d", stats.QueueDepth)
	}

	// All requests should be handled because QueueSize is large enough (10000 > 5000)
	if handled != numRequests {
		t.Errorf("expected %d handled requests, got %d", numRequests, handled)
	}
}

func TestWaitingRoom_QueueFull(t *testing.T) {
	cfg := &config.Config{
		MaxConcurrent: 1,
		QueueSize:     1,
		QueueTimeout:  1 * time.Second,
	}

	wr := New(cfg)

	var block = make(chan struct{})
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	})

	// Request 1: Takes the only backend slot, blocks
	go func() {
		req := httptest.NewRequest("GET", "/", nil)
		wr.Handle(httptest.NewRecorder(), req, backend)
	}()

	time.Sleep(10 * time.Millisecond)

	// Request 2: Enters the single queue slot
	go func() {
		req := httptest.NewRequest("GET", "/", nil)
		wr.Handle(httptest.NewRecorder(), req, backend)
	}()

	time.Sleep(10 * time.Millisecond)

	// Request 3: Queue is full, should be rejected immediately with 503
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	wr.Handle(w, req, backend)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 Service Unavailable, got %d", w.Code)
	}

	// Unblock backend to clean up
	close(block)
}

func TestWaitingRoom_Timeout(t *testing.T) {
	cfg := &config.Config{
		MaxConcurrent: 1,
		QueueSize:     5,
		QueueTimeout:  50 * time.Millisecond,
	}

	wr := New(cfg)

	var block = make(chan struct{})
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block // Block forever
	})

	// Request 1: takes backend slot
	go func() {
		req := httptest.NewRequest("GET", "/", nil)
		wr.Handle(httptest.NewRecorder(), req, backend)
	}()

	time.Sleep(10 * time.Millisecond)

	// Request 2: enters queue, will timeout
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	wr.Handle(w, req, backend)

	// wait for it to return
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 due to queue timeout, got %d", w.Code)
	}

	stats := wr.GetStats()
	if stats.TotalTimeouts != 1 {
		t.Errorf("expected 1 timeout in stats, got %d", stats.TotalTimeouts)
	}
}

func TestWaitingRoom_ContextCanceled(t *testing.T) {
	cfg := &config.Config{
		MaxConcurrent: 1,
		QueueSize:     5,
		QueueTimeout:  5 * time.Second, // Long timeout
	}

	wr := New(cfg)

	var block = make(chan struct{})
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block // Block forever
	})

	// Request 1: takes backend slot
	go func() {
		req := httptest.NewRequest("GET", "/", nil)
		wr.Handle(httptest.NewRecorder(), req, backend)
	}()

	time.Sleep(10 * time.Millisecond)

	// Request 2: enters queue, then context is canceled
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", "/", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	
	// Cancel the context in the background before the queue timeout
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	wr.Handle(w, req, backend)
	
	// Since client disconnected, nothing is written to the recorder, it just returns.
	
	time.Sleep(50 * time.Millisecond) // Give time for drainTicket to run

	stats := wr.GetStats()
	// QueueDepth should go back to 0 because the request left the queue
	if stats.QueueDepth != 0 {
		t.Errorf("expected queue depth 0 after disconnect, got %d", stats.QueueDepth)
	}
}
