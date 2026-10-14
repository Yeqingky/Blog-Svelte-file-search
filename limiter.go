package main

import (
	"sync"
	"time"
)

type rateBucket struct {
	tokens   float64
	last     time.Time
	lastSeen time.Time
}

type rateLimiter struct {
	mu       sync.Mutex
	rate     float64
	burst    float64
	buckets  map[string]rateBucket
	requests uint64
}

func newRateLimiter(ratePerSecond, burst float64) *rateLimiter {
	return &rateLimiter{
		rate:    ratePerSecond,
		burst:   burst,
		buckets: make(map[string]rateBucket),
	}
}

func (limiter *rateLimiter) allow(key string, now time.Time) bool {
	if key == "" {
		key = "unknown"
	}
	limiter.mu.Lock()
	defer limiter.mu.Unlock()

	bucket, exists := limiter.buckets[key]
	if !exists {
		bucket = rateBucket{tokens: limiter.burst, last: now, lastSeen: now}
	} else {
		elapsed := now.Sub(bucket.last).Seconds()
		if elapsed > 0 {
			bucket.tokens += elapsed * limiter.rate
			if bucket.tokens > limiter.burst {
				bucket.tokens = limiter.burst
			}
		}
		bucket.last = now
		bucket.lastSeen = now
	}

	allowed := bucket.tokens >= 1
	if allowed {
		bucket.tokens--
	}
	limiter.buckets[key] = bucket
	limiter.requests++
	if limiter.requests%128 == 0 {
		for address, candidate := range limiter.buckets {
			if now.Sub(candidate.lastSeen) > 2*time.Minute {
				delete(limiter.buckets, address)
			}
		}
	}
	return allowed
}
