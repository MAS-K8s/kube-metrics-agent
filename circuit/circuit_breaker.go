package circuit

import (
	"sync"
	"time"
)

type Breaker struct {
	mu               sync.Mutex
	failures         int
	openUntil        time.Time
	failureThreshold int
	openDuration     time.Duration
}


func New(threshold int, openDuration time.Duration) *Breaker {
	return &Breaker{
		failureThreshold: threshold,
		openDuration:     openDuration,
	}
}

func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return time.Now().After(b.openUntil)
}


func (b *Breaker) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
	b.openUntil = time.Time{}
}

func (b *Breaker) Fail() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures++
	if b.failures >= b.failureThreshold {
		b.openUntil = time.Now().Add(b.openDuration)
	}
}


func (b *Breaker) IsOpen() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return time.Now().Before(b.openUntil)
}
