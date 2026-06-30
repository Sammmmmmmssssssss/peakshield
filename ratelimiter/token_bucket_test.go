package ratelimiter

import (
	"context"
	"testing"
	"time"

	"peakshield/config"
)

func TestTokenBucketRateLimiter(t *testing.T) {
	cfg := &config.Config{
		RateLimit: 10.0,
		BurstSize: 5,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rl := New(ctx, cfg)

	// Test allowable burst
	for i := 0; i < 5; i++ {
		if !rl.Allow("127.0.0.1") {
			t.Errorf("Expected request %d to be allowed", i)
		}
	}

	// Next request should fail
	if rl.Allow("127.0.0.1") {
		t.Errorf("Expected 6th request to be rate limited")
	}

	// Wait for refill (1 token takes 100ms)
	time.Sleep(150 * time.Millisecond)
	if !rl.Allow("127.0.0.1") {
		t.Errorf("Expected request to be allowed after refill")
	}
}

func BenchmarkTokenBucket_Parallel(b *testing.B) {
	cfg := &config.Config{
		RateLimit: 10000.0,
		BurstSize: 1000,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rl := New(ctx, cfg)

	ip := "192.168.1.1"

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			rl.Allow(ip)
		}
	})
}

// TestLimiter_GCStress runs a concurrent stress test that exercises the GC
// alongside Allow() to verify that the atomic.Int64 fix for lastSeenNano
// prevents any data races.
func TestLimiter_GCStress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := &config.Config{
		PeakShieldLimit: 100,
	}
	l := New(ctx, cfg)

	var wg sync.WaitGroup
	ipCount := 50
	goroutinesPerIP := 10

	// 1. Start concurrent Allow() requests.
	for i := 0; i < ipCount; i++ {
		ip := fmt.Sprintf("192.168.0.%d", i)
		for j := 0; j < goroutinesPerIP; j++ {
			wg.Add(1)
			go func(ip string) {
				defer wg.Done()
				for k := 0; k < 100; k++ {
					l.Allow(ip)
					// Tiny sleep to interleave with GC
					time.Sleep(time.Microsecond)
				}
			}(ip)
		}
	}

	// 2. Concurrently run the GC sweep repeatedly.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			// runGC scans all buckets and deletes idle ones
			l.runGC()
			time.Sleep(10 * time.Microsecond)
		}
	}()

	wg.Wait()
}
