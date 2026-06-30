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
