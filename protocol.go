package main

// --- Trigger protocol (client <-> daemon, newline-delimited JSON) ---
//
// The `sync` subcommand is a thin client: it forwards its request over the
// daemon's unix socket and waits for the pass result. Client and daemon ship
// in the same binary inside the same image, so there is no version skew to
// negotiate and the wire format carries no version field.

// wireRequest is the single request line a client sends after connecting. A
// sync pass takes no arguments — the job set comes from the daemon's mounted
// YAML config — so the request carries no fields; the empty JSON object is
// the frame that says "run one pass". Fields added here later must stay
// optional so an older client's `{}` keeps decoding.
type wireRequest struct{}

// wireEvent is one status line the daemon streams back. The client receives
// eventQueued on acceptance, eventStarted when the executor picks the request
// up (the gap between the two is queue wait behind an in-flight pass), and
// exactly one eventDone as the final line.
type wireEvent struct {
	Event string `json:"event"`
	// Reason explains a not-OK outcome that isn't a plain job failure
	// (queue full, cancelled by shutdown, config reload error).
	Reason string `json:"reason,omitempty"`
	// DurationMs is the pass's execution time on eventDone (0 when the
	// request never ran, e.g. cancelled or rejected).
	DurationMs int64 `json:"duration_ms,omitempty"`
	// OK is meaningful only on eventDone: the pass outcome (never omitted,
	// so a failed pass is explicit on the wire).
	OK bool `json:"ok"`
}

const (
	eventQueued  = "queued"
	eventStarted = "started"
	eventDone    = "done"
)
