// Package limiter provides a simple bounded-concurrency gate so a burst of
// large uploads can't starve every other customer's request. It holds no
// per-upload global mutable state — just a counting semaphore sized once
// at startup — so it is safe to share across concurrent requests.
package limiter

import (
	"net/http"
)

// Semaphore bounds how many requests may proceed past Acquire at once.
// The zero value is not usable; use New.
type Semaphore struct {
	slots chan struct{}
}

// New creates a Semaphore allowing up to n concurrent holders. n <= 0 is
// treated as 1 to avoid an unusable (permanently blocking) limiter.
func New(n int) *Semaphore {
	if n <= 0 {
		n = 1
	}
	return &Semaphore{slots: make(chan struct{}, n)}
}

// TryAcquire attempts to take a slot without blocking. It reports whether
// the slot was obtained; if true, the caller must call Release exactly
// once when done.
func (s *Semaphore) TryAcquire() bool {
	select {
	case s.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

// Release returns a slot to the pool.
func (s *Semaphore) Release() {
	<-s.slots
}

// Middleware wraps next so that requests exceeding the configured
// concurrency limit receive an immediate 429 instead of queuing behind a
// slow analysis (a queued request would still tie up a goroutine and a
// half-read request body; failing fast lets the customer retry and lets a
// load balancer redistribute the request instead).
func Middleware(sem *Semaphore, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !sem.TryAcquire() {
			w.Header().Set("Retry-After", "2")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"success":false,"error":{"code":"SERVER_BUSY","message":"The server is handling the maximum number of concurrent analyses. Please retry in a moment."}}`))
			return
		}
		defer sem.Release()
		next.ServeHTTP(w, r)
	})
}
