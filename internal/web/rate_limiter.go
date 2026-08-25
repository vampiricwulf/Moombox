package web

import (
	"container/list"
	"context"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// maxRateLimiterEntries caps per-IP entries to prevent DoS (match TS: 10,000).
const maxRateLimiterEntries = 10000

// rateLimiterLogger is the minimal interface RateLimiter needs to report
// background-goroutine panics. Using an interface keeps the package
// independent of any concrete logger type.
type rateLimiterLogger interface {
	Error(msg string, args ...any)
}

// rateLimiterEntry is the value stored in the per-IP map plus the
// linked-list element so cap-eviction picks the least-recently used IP
// rather than an arbitrary map entry. Audit reports/web.md Q-18.
type rateLimiterEntry struct {
	ip    string
	times []time.Time
	elem  *list.Element // points back to the LRU list element holding `ip`
}

// RateLimiter provides in-memory per-IP sliding window rate limiting.
//
// Cap eviction (when entries >= maxRateLimiterEntries) drops the IP at the
// front of `lruOrder` — the least recently used. The previous map-iteration
// "drop first key found" was direction-blind: an attacker churning through
// 10,000 IPs could push the legitimate user out by happening to land on
// their key. LRU keeps the active user in the map regardless of attacker
// volume. Audit reports/web.md Q-18.
type RateLimiter struct {
	mu        sync.Mutex
	requests  map[string]*rateLimiterEntry
	lruOrder  *list.List // values are string (the IP); front = LRU, back = MRU
	limit     int
	window    time.Duration
	cleanup   *time.Ticker
	done      chan struct{}
	closeOnce sync.Once         // guards close(done) so double-Close doesn't panic
	logger    rateLimiterLogger // optional; may be nil

	// ClientIP resolves the effective client IP used as the bucket key
	// (trusted_proxies / X-Forwarded-For aware). Nil falls back to the raw
	// peer address. Without it, a reverse proxy collapses every remote
	// client into one bucket — one attacker could exhaust the login budget
	// for everyone behind the proxy.
	ClientIP func(*http.Request) string
}

// NewRateLimiter creates a new rate limiter and starts its background
// cleanup loop. Caller must invoke Close() when done to stop the
// goroutine.
//
// For ctx-aware lifecycle (cleanup goroutine exits when ctx cancels),
// use NewRateLimiterCtx instead. Audit reports/web.md Q-19.
func NewRateLimiter(limit int, window time.Duration) *RateLimiter {
	rl := newRateLimiterStruct(limit, window)
	go rl.cleanupLoop()
	return rl
}

// NewRateLimiterCtx creates a rate limiter whose cleanup goroutine is
// tied to the parent ctx — when ctx cancels, the cleanup loop exits
// without needing an explicit Close() call. Useful for tests and for
// services that already own a long-lived ctx.
func NewRateLimiterCtx(ctx context.Context, limit int, window time.Duration) *RateLimiter {
	rl := newRateLimiterStruct(limit, window)
	go rl.cleanupLoop()
	if ctx != nil {
		go func() {
			defer func() {
				if r := recover(); r != nil && rl.logger != nil {
					rl.logger.Error("rate limiter ctx watcher panic", "panic", r)
				}
			}()
			select {
			case <-ctx.Done():
				rl.Close()
			case <-rl.done:
				// Already closed via explicit Close()
			}
		}()
	}
	return rl
}

// newRateLimiterStruct constructs the struct without starting any
// goroutines. Internal helper shared by the public constructors.
func newRateLimiterStruct(limit int, window time.Duration) *RateLimiter {
	return &RateLimiter{
		requests: make(map[string]*rateLimiterEntry),
		lruOrder: list.New(),
		limit:    limit,
		window:   window,
		cleanup:  time.NewTicker(time.Minute), // Match TS: 60 second cleanup
		done:     make(chan struct{}),
	}
}

// SetLogger attaches a logger used to report panics from the cleanup
// goroutine. Safe to call once after construction. Nil-safe.
func (rl *RateLimiter) SetLogger(logger rateLimiterLogger) {
	rl.logger = logger
}

// AllowWithRetry checks if a request from the given IP is allowed.
// Returns (allowed, retryAfterSeconds).
func (rl *RateLimiter) AllowWithRetry(ip string) (bool, int) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-rl.window)

	// Cap eviction: drop the least-recently-used IP when at the
	// per-process limit. Doing this before the lookup means a brand-new
	// IP under attack churn doesn't squeeze out the active user — the
	// active user's MRU position keeps them in the map. Audit
	// reports/web.md Q-18.
	if len(rl.requests) >= maxRateLimiterEntries {
		if oldest := rl.lruOrder.Front(); oldest != nil {
			oldestIP := oldest.Value.(string)
			rl.lruOrder.Remove(oldest)
			delete(rl.requests, oldestIP)
		}
	}

	entry, ok := rl.requests[ip]
	if !ok {
		entry = &rateLimiterEntry{ip: ip}
		entry.elem = rl.lruOrder.PushBack(ip)
		rl.requests[ip] = entry
	} else {
		// Touch — move to MRU (back of list).
		rl.lruOrder.MoveToBack(entry.elem)
	}

	// Filter expired entries
	valid := make([]time.Time, 0, len(entry.times))
	for _, t := range entry.times {
		if t.After(cutoff) {
			valid = append(valid, t)
		}
	}

	if len(valid) >= rl.limit {
		entry.times = valid
		// Calculate remaining time until oldest request expires (matching TS: resetTime - now)
		retryAfter := max(int(valid[0].Add(rl.window).Sub(now).Seconds())+1, 1)
		return false, retryAfter
	}

	entry.times = append(valid, now)
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
		if rl.ClientIP != nil {
			ip = rl.ClientIP(r)
		}
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
			if rl.logger != nil {
				rl.logger.Error("rate limiter cleanup panic", "panic", r)
			}
		}
	}()
	for {
		select {
		case <-rl.done:
			return
		case <-rl.cleanup.C:
			rl.mu.Lock()
			cutoff := time.Now().Add(-rl.window)
			for ip, entry := range rl.requests {
				var valid []time.Time
				for _, t := range entry.times {
					if t.After(cutoff) {
						valid = append(valid, t)
					}
				}
				if len(valid) == 0 {
					rl.lruOrder.Remove(entry.elem)
					delete(rl.requests, ip)
				} else {
					entry.times = valid
				}
			}
			rl.mu.Unlock()
		}
	}
}

// Close stops the cleanup goroutine. Idempotent: subsequent calls are
// no-ops thanks to the internal sync.Once guarding close(rl.done).
// Production wiring already routes through cmd/moombox/services.go's
// own sync.Once so today there's no double-Close path, but the guard
// makes the contract safe by construction for future callers.
func (rl *RateLimiter) Close() {
	rl.closeOnce.Do(func() {
		rl.cleanup.Stop()
		close(rl.done)
	})
}
