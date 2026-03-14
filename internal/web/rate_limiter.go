package web

import (
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// maxRateLimiterEntries caps per-IP entries to prevent DoS (match TS: 10,000).
const maxRateLimiterEntries = 10000

// RateLimiter provides in-memory per-IP sliding window rate limiting.
type RateLimiter struct {
	mu       sync.Mutex
	requests map[string][]time.Time
	limit    int
	window   time.Duration
	cleanup  *time.Ticker
	done     chan struct{}
}

// NewRateLimiter creates a new rate limiter.
func NewRateLimiter(limit int, window time.Duration) *RateLimiter {
	rl := &RateLimiter{
		requests: make(map[string][]time.Time),
		limit:    limit,
		window:   window,
		cleanup:  time.NewTicker(time.Minute), // Match TS: 60 second cleanup
		done:     make(chan struct{}),
	}

	go rl.cleanupLoop()
	return rl
}

// AllowWithRetry checks if a request from the given IP is allowed.
// Returns (allowed, retryAfterSeconds).
func (rl *RateLimiter) AllowWithRetry(ip string) (bool, int) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-rl.window)

	// Max entries protection: evict oldest entry when limit exceeded (match TS)
	if len(rl.requests) >= maxRateLimiterEntries {
		// Evict first key found (like TS Map.keys().next())
		for k := range rl.requests {
			delete(rl.requests, k)
			break
		}
	}

	// Filter expired entries
	times := rl.requests[ip]
	valid := make([]time.Time, 0, len(times))
	for _, t := range times {
		if t.After(cutoff) {
			valid = append(valid, t)
		}
	}

	if len(valid) >= rl.limit {
		rl.requests[ip] = valid
		// Calculate remaining time until oldest request expires (matching TS: resetTime - now)
		retryAfter := max(int(valid[0].Add(rl.window).Sub(now).Seconds())+1, 1)
		return false, retryAfter
	}

	rl.requests[ip] = append(valid, now)
	return true, 0
}

// Allow checks if a request from the given IP is allowed.
func (rl *RateLimiter) Allow(ip string) bool {
	allowed, _ := rl.AllowWithRetry(ip)
	return allowed
}

// Middleware returns an HTTP middleware that applies rate limiting.
// Includes Retry-After header to match TypeScript behavior.
func (rl *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := ExtractIP(r)
		if allowed, retryAfter := rl.AllowWithRetry(ip); !allowed {
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprintf(w, `{"error":"Too many requests, please try again later","retryAfter":%d}`, retryAfter)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (rl *RateLimiter) cleanupLoop() {
	defer func() {
		if r := recover(); r != nil {
			// Rate limiter cleanup panic — silently recover
		}
	}()
	for {
		select {
		case <-rl.done:
			return
		case <-rl.cleanup.C:
			rl.mu.Lock()
			cutoff := time.Now().Add(-rl.window)
			for ip, times := range rl.requests {
				var valid []time.Time
				for _, t := range times {
					if t.After(cutoff) {
						valid = append(valid, t)
					}
				}
				if len(valid) == 0 {
					delete(rl.requests, ip)
				} else {
					rl.requests[ip] = valid
				}
			}
			rl.mu.Unlock()
		}
	}
}

// Close stops the cleanup goroutine.
func (rl *RateLimiter) Close() {
	rl.cleanup.Stop()
	close(rl.done)
}
