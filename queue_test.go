package main

import (
	"errors"
	"sync"
	"testing"
)

// TestRunQueue_FIFOOrder pins the queue's core contract: requests come out in
// submission order (every trigger gets its own pass, strictly in order).
func TestRunQueue_FIFOOrder(t *testing.T) {
	t.Parallel()
	q := newRunQueue(4)
	a := newRequest("external")
	b := newRequest("external")
	c := newRequest("interval")
	for _, r := range []*request{a, b, c} {
		if err := q.submit(r); err != nil {
			t.Fatalf("submit() = %v, want nil", err)
		}
	}
	q.close()
	var got []*request
	for r := range q.requests {
		got = append(got, r)
	}
	want := []*request{a, b, c}
	if len(got) != len(want) {
		t.Fatalf("drained %d requests, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("request %d out of order", i)
		}
	}
}

// TestRunQueue_FullRejectsImmediately pins the bounded-backpressure contract:
// a full queue rejects with errQueueFull instead of blocking the trigger.
func TestRunQueue_FullRejectsImmediately(t *testing.T) {
	t.Parallel()
	q := newRunQueue(1)
	if err := q.submit(newRequest("external")); err != nil {
		t.Fatalf("first submit() = %v, want nil", err)
	}
	if err := q.submit(newRequest("external")); !errors.Is(err, errQueueFull) {
		t.Errorf("submit() on a full queue = %v, want errQueueFull", err)
	}
}

// TestRunQueue_ClosedRejectsSubmissions pins the shutdown-admission contract.
func TestRunQueue_ClosedRejectsSubmissions(t *testing.T) {
	t.Parallel()
	q := newRunQueue(4)
	q.close()
	if err := q.submit(newRequest("external")); !errors.Is(err, errQueueClosed) {
		t.Errorf("submit() after close = %v, want errQueueClosed", err)
	}
	q.close() // idempotent: a second close must not panic
}

// TestRunQueue_ConcurrentSubmitAndCloseIsSafe hammers submit against close
// under the race detector: the mutex serializes them, so no send can hit a
// closed channel (the panic the naive design allowed).
func TestRunQueue_ConcurrentSubmitAndCloseIsSafe(t *testing.T) {
	t.Parallel()
	for range 50 {
		q := newRunQueue(2)
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				_ = q.submit(newRequest("external"))
			})
		}
		q.close()
		wg.Wait()
		for range q.requests { // drain only
		}
	}
}
