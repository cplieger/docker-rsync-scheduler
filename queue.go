package main

import (
	"errors"
	"sync"
	"time"
)

// --- Run queue ---
//
// The daemon is the single owner of pass execution: every trigger — the
// built-in ticker and each socket client — submits a request here, and one
// executor goroutine (daemon.runPasses) serves them strictly in order. FIFO
// with no coalescing: every accepted request gets its own pass and its own
// true result, which is simpler and more honest than the old cross-process
// defer (a trigger that raced an in-flight pass used to exit 0 without
// running; now it queues and reports the result of its own pass). Passes are
// idempotent — rsync converges, so a back-to-back duplicate costs only a
// cheap delta pass.

// queueCapacity bounds pending requests. The realistic trigger set is one
// periodic job (Ofelia) plus a manual exec, so 16 is generous headroom; a
// client hitting a full queue is rejected immediately with a clear reason
// (honest backpressure) rather than queued unboundedly.
const queueCapacity = 16

var (
	errQueueClosed = errors.New("scheduler is shutting down")
	errQueueFull   = errors.New("run queue is full")
)

// request is one queued pass request. (Named request, not job: `job` is the
// YAML sync-job type in config.go.)
type request struct {
	// started is closed by the executor the moment the pass begins.
	started chan struct{}
	// result receives exactly one outcome per accepted request — from the
	// pass itself, or from shutdown cancellation. Buffered so the executor
	// never blocks on a departed waiter.
	result chan runOutcome
	// trigger labels the pass's origin in logs: startup, interval, external.
	trigger string
}

// runOutcome is a request's final result.
type runOutcome struct {
	// reason explains a not-ok outcome that isn't a plain job failure
	// (cancelled by shutdown, config reload error).
	reason   string
	duration time.Duration
	ok       bool
}

// newRequest builds a request for the given trigger.
func newRequest(trigger string) *request {
	return &request{
		trigger: trigger,
		started: make(chan struct{}),
		result:  make(chan runOutcome, 1),
	}
}

// finish delivers the request's single result.
func (r *request) finish(out runOutcome) {
	r.result <- out
}

// runQueue is the bounded FIFO between triggers and the executor. Submission
// is non-blocking: a full or closed queue rejects immediately. The channel is
// the queue; the executor is its only receiver.
type runQueue struct {
	requests chan *request
	mu       sync.Mutex
	closed   bool
}

func newRunQueue(capacity int) *runQueue {
	return &runQueue{requests: make(chan *request, capacity)}
}

// submit enqueues r, failing fast when the queue is full or the daemon is
// shutting down. An accepted request is guaranteed exactly one result. The
// send is non-blocking and happens under the mutex, so it can never race
// close's channel close.
func (q *runQueue) submit(r *request) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return errQueueClosed
	}
	select {
	case q.requests <- r:
		return nil
	default:
		return errQueueFull
	}
}

// close stops admission and closes the channel, letting the executor's range
// loop drain the already-queued requests (it cancels each once shutdown is
// signalled) and terminate. Idempotent; called at shutdown.
func (q *runQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.closed = true
	close(q.requests)
}
