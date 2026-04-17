// Package circuit implements a simple thread-safe circuit breaker used to
// protect the Go controller from cascading failures when the RL agent service
// is unavailable or returning repeated errors.
//
// The circuit breaker operates in two states:
//
//	Closed  — normal operation; requests are allowed through.
//	Open    — the failure threshold has been exceeded; requests are rejected
//	          immediately for the configured open duration, giving the
//	          downstream service time to recover without being flooded.
//
// There is no half-open probe state; the breaker returns to Closed
// automatically once the open duration has elapsed.
package circuit

import (
	"sync"
	"time"
)

// Breaker is a thread-safe circuit breaker. The zero value is not usable;
// create instances via New.
type Breaker struct {
	mu               sync.Mutex
	failures         int
	openUntil        time.Time
	failureThreshold int
	openDuration     time.Duration
}

// New constructs a Breaker that opens after threshold consecutive failures
// and remains open for openDuration before allowing requests again.
//
// Recommended defaults (matching config defaults):
//
//	threshold    = 5
//	openDuration = 30 * time.Second
func New(threshold int, openDuration time.Duration) *Breaker {
	return &Breaker{
		failureThreshold: threshold,
		openDuration:     openDuration,
	}
}

// Allow reports whether a request should be forwarded to the downstream
// service. It returns false when the circuit is open (i.e. the failure
// threshold was recently exceeded and the recovery window has not yet elapsed).
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return time.Now().After(b.openUntil)
}

// Success records a successful response from the downstream service.
// It resets the failure counter and closes the circuit unconditionally.
// Call this after every request that receives a valid 2xx response.
func (b *Breaker) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
	b.openUntil = time.Time{}
}

// Fail records a failed request. If the cumulative failure count reaches
// the threshold, the circuit is opened for the configured open duration.
// Call this on every network error, timeout, or non-2xx HTTP response.
func (b *Breaker) Fail() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures++
	if b.failures >= b.failureThreshold {
		b.openUntil = time.Now().Add(b.openDuration)
	}
}

// IsOpen returns true if the circuit is currently open. Useful for health
// reporting endpoints that want to surface downstream availability state.
func (b *Breaker) IsOpen() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return time.Now().Before(b.openUntil)
}
