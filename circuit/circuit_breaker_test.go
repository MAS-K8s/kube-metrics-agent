package circuit_test

import (
	"testing"
	"time"

	"go-controller-agent/circuit"
)

// TC-UT-01: Circuit starts closed and allows requests
func TestBreakerStartsClosed(t *testing.T) {
	b := circuit.New(3, 10*time.Second)
	if !b.Allow() {
		t.Error("expected circuit to be closed on creation")
	}
}

// TC-UT-02: Circuit opens after reaching failure threshold
func TestBreakerOpensAtThreshold(t *testing.T) {
	b := circuit.New(3, 10*time.Second)
	b.Fail()
	b.Fail()
	if b.IsOpen() {
		t.Error("circuit should still be closed after 2 failures (threshold=3)")
	}
	b.Fail()
	if !b.IsOpen() {
		t.Error("circuit should be open after 3 failures")
	}
	if b.Allow() {
		t.Error("open circuit should reject requests")
	}
}

// TC-UT-03: Success resets failure count and closes circuit
func TestBreakerSuccessResetsFailures(t *testing.T) {
	b := circuit.New(3, 10*time.Second)
	b.Fail()
	b.Fail()
	b.Success()
	b.Fail()
	b.Fail()
	// Only 2 failures after reset — should still be closed
	if b.IsOpen() {
		t.Error("circuit should be closed after Success() reset")
	}
}

// TC-UT-04: Circuit closes automatically after open duration elapses
func TestBreakerClosesAfterDuration(t *testing.T) {
	b := circuit.New(1, 50*time.Millisecond)
	b.Fail()
	if !b.IsOpen() {
		t.Error("expected open after 1 failure")
	}
	time.Sleep(60 * time.Millisecond)
	if !b.Allow() {
		t.Error("circuit should close after open duration elapses")
	}
}

// TC-UT-05: Circuit remains open before duration elapses
func TestBreakerRemainsOpenBeforeDuration(t *testing.T) {
	b := circuit.New(1, 5*time.Second)
	b.Fail()
	if b.Allow() {
		t.Error("circuit should not allow requests before open duration elapses")
	}
}
