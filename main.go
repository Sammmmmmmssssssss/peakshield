package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sync"
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
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	slog.Info("Initializing PeakShield Reverse Proxy...")

	// 1. Load Configuration
	cfg, err := config.Load()
	if err != nil {
		slog.Error("Failed to load configuration", "err", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 2. Initialize Core Modules
	rl := ratelimiter.New(ctx, cfg)
	wr := waitingroom.New(cfg)
	st := stripper.New(cfg, wr.ActiveRequests)
	
	prx, err := proxy.New(cfg)
	if err != nil {
		slog.Error("Failed to initialize proxy", "err", err)
		os.Exit(1)
	}

	// 3. Setup Internal Stats Endpoint (Internal Port)
	metricsMux := http.NewServeMux()
	
	metricsMux.HandleFunc("/__peakshield/stats", func(w http.ResponseWriter, r *http.Request) {
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

	// Add Prometheus Metrics Endpoint
	metricsMux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		stats := wr.GetStats()

		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprintf(w, "# HELP peakshield_active_requests Number of requests currently in-flight to backend\n")
		fmt.Fprintf(w, "# TYPE peakshield_active_requests gauge\n")
		fmt.Fprintf(w, "peakshield_active_requests %d\n", stats.ActiveRequests)

		fmt.Fprintf(w, "# HELP peakshield_queue_depth Number of requests waiting in queue\n")
		fmt.Fprintf(w, "# TYPE peakshield_queue_depth gauge\n")
		fmt.Fprintf(w, "peakshield_queue_depth %d\n", stats.QueueDepth)

		fmt.Fprintf(w, "# HELP peakshield_goroutines Number of active goroutines\n")
		fmt.Fprintf(w, "# TYPE peakshield_goroutines gauge\n")
		fmt.Fprintf(w, "peakshield_goroutines %d\n", runtime.NumGoroutine())

		fmt.Fprintf(w, "# HELP peakshield_alloc_bytes Bytes allocated and still in use\n")
		fmt.Fprintf(w, "# TYPE peakshield_alloc_bytes gauge\n")
		fmt.Fprintf(w, "peakshield_alloc_bytes %d\n", mem.Alloc)
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

	// 4. Build Middleware Chain for the Proxy (Public Port)
	mux := http.NewServeMux()
	proxyHandler := middleware.Chain(prx,
		rlMiddleware,
		wr.Middleware,
		st.Middleware,
	)

	// Forward all traffic to the proxy chain
	mux.Handle("/", proxyHandler)

	// 5. Configure Global HTTP Servers
	proxyServer := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: mux,
		// Explicit timeouts to prevent Slowloris attacks and connection leaks
		ReadTimeout:  10 * time.Second,
		WriteTimeout: cfg.QueueTimeout + cfg.BackendTimeout + 15*time.Second,
		IdleTimeout:  120 * time.Second,
	}

	metricsServer := &http.Server{
		Addr:    cfg.MetricsListenAddr,
		Handler: metricsMux,
		// Short timeouts for internal telemetry server
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// 6. Graceful Shutdown Setup
	go func() {
		slog.Info("PeakShield active and listening", "listenAddr", cfg.ListenAddr, "targetURL", cfg.TargetURL)
		if err := proxyServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("Proxy server error", "err", err)
			os.Exit(1)
		}
	}()

	go func() {
		slog.Info("PeakShield metrics listening", "metricsAddr", cfg.MetricsListenAddr)
		if err := metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("Metrics server error", "err", err)
			os.Exit(1)
		}
	}()

	// Wait for OS termination signal (SIGINT/SIGTERM)
	stopChan := make(chan os.Signal, 1)
	signal.Notify(stopChan, os.Interrupt, syscall.SIGTERM)
	<-stopChan

	slog.Info("Received termination signal. Shutting down servers gracefully...")

	// 15-second grace period for in-flight requests and waiting room to drain
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		if err := proxyServer.Shutdown(shutdownCtx); err != nil {
			slog.Error("Proxy graceful shutdown failed", "err", err)
		}
	}()

	go func() {
		defer wg.Done()
		if err := metricsServer.Shutdown(shutdownCtx); err != nil {
			slog.Error("Metrics graceful shutdown failed", "err", err)
		}
	}()

	wg.Wait()

	slog.Info("PeakShield exited safely.")
}
