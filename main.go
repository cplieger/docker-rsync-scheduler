package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/cplieger/health"
)

// --- Main ---

// main dispatches on the first argument: `health` runs the Docker probe,
// `sync` triggers one pass via the daemon's socket and exits with that
// pass's result (the external-trigger entry point), and `daemon` runs the
// long-lived daemon that owns all passes. Anything else, including no
// argument, exits 2.
func main() {
	// CLI health probe for the Docker healthcheck.
	if len(os.Args) > 1 && os.Args[1] == "health" {
		// The probe is its own short-lived process and stays silent: it
		// runs before any logger is configured, so an env-parse warning
		// from probeOptions would land in Docker's health log in Go's
		// stock format. RunProbe reports the verdict through its exit code
		// and its own stderr line, neither of which goes through slog.
		slog.SetDefault(slog.New(slog.DiscardHandler))
		health.RunProbe(healthMarkerPath, probeOptions()...)
	}
	os.Exit(dispatch())
}

// dispatch selects the subcommand and returns the process exit code.
// Returning the code (rather than calling os.Exit here) keeps the routing
// testable and lets deferred cleanup in the daemon run before exit.
func dispatch() int {
	var cmd string
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	setupLogger()
	// One signal root for every subcommand. The daemon drains on it; the
	// `sync` client's wait is unbounded by contract, so binding it to the
	// terminal lets an operator interrupting the `docker exec` unwind here --
	// closing the connection, which the daemon observes -- instead of leaving
	// the socket half-open until the kernel reaps this process.
	// trigger.Submit documents signal.NotifyContext as the caller's bound.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "daemon":
		if err := runDaemon(ctx, socketPath, defaultCommandRunner); err != nil {
			return 1
		}
		return 0
	case "sync":
		return runClient(ctx, socketPath)
	default:
		slog.Error("unknown subcommand", "command", cmd, "valid", "daemon, sync, health")
		return 2
	}
}
