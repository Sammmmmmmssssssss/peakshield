package main

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"peakshield/config"
	"peakshield/middleware"
	"peakshield/proxy"
	"peakshield/ratelimiter"
	"peakshield/stripper"
	"peakshield/waitingroom"
)

func main() {
	// Configure global standard logger
	log.SetOutput(os.Stdout)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Println("Initializing PeakShield Reverse Proxy...")

	// 1. Load Configuration
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 2. Initialize Core Modules
	rl := ratelimiter.New(ctx, cfg)
	wr := waitingroom.New(cfg)
	st := stripper.New(cfg, wr.ActiveRequests)
	
	prx, err := proxy.New(cfg)
	if err != nil {
		log.Fatalf("Failed to initialize proxy: %v", err)
	}

	// 3. Setup Internal Stats Endpoint
	mux := http.NewServeMux()
	
	mux.HandleFunc("/__peakshield/stats", func(w http.ResponseWriter, r *http.Request) {
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)

		stats := struct {
			WaitingRoom    waitingroom.Stats `json:"waiting_room"`
			Goroutines     int               `json:"goroutines"`
			AllocatedMB    float64           `json:"allocated_mb"`
			SysMB          float64           `json:"sys_mb"`
			NumGC          uint32            `json:"num_gc"`
		}{
			WaitingRoom: wr.GetStats(),
			Goroutines:  runtime.NumGoroutine(),
			AllocatedMB: float64(mem.Alloc) / 1024 / 1024,
			SysMB:       float64(mem.Sys) / 1024 / 1024,
			NumGC:       mem.NumGC,
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(stats)
	})

	// Rate Limiter wrapper
	rlMiddleware := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				ip = r.RemoteAddr
			}
			if !rl.Allow(ip) {
				http.Error(w, "429 Too Many Requests", http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}

	// 4. Build Middleware Chain for the Proxy
	proxyHandler := middleware.Chain(prx,
		rlMiddleware,
		wr.Middleware,
		st.Middleware,
	)

	// Forward all other traffic to the proxy chain
	mux.Handle("/", proxyHandler)

	// 5. Configure Global HTTP Server
	server := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: mux,
		// Explicit timeouts to prevent Slowloris attacks and connection leaks
		ReadTimeout:  10 * time.Second,
		WriteTimeout: cfg.QueueTimeout + cfg.BackendTimeout + 15*time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// 6. Graceful Shutdown Setup
	stopChan := make(chan os.Signal, 1)
	signal.Notify(stopChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		log.Printf("PeakShield active and listening on %s (Target Backend: %s)", cfg.ListenAddr, cfg.TargetURL)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	// Wait for OS termination signal (SIGINT/SIGTERM)
	<-stopChan
	log.Println("\nReceived termination signal. Shutting down PeakShield gracefully...")

	// 15-second grace period for in-flight requests and waiting room to drain
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("Graceful shutdown failed: %v", err)
	}

	log.Println("PeakShield exited safely.")
}
