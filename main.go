package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/cplieger/health"
)

// --- Main ---

// main dispatches on the first argument: `health` runs the Docker probe,
// `sync` triggers one pass via the daemon's socket and exits with that
// pass's result (the external-trigger entry point), and anything else
// (including no argument) runs the long-lived daemon that owns all passes.
func main() {
	// CLI health probe for the Docker healthcheck. Checked before the logger
	// is configured because RunProbe calls os.Exit.
	if len(os.Args) > 1 && os.Args[1] == "health" {
		health.RunProbe(healthMarkerPath, probeOptions()...)
	}
	os.Exit(dispatch())
}

// dispatch selects the subcommand and returns the process exit code.
// Returning the code (rather than calling os.Exit here) keeps the routing
// testable and lets deferred cleanup in the daemon run before exit.
func dispatch() int {
	cmd := "daemon"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	switch cmd {
	case "daemon":
		if err := runDaemon(context.Background(), socketPath, defaultCommandRunner); err != nil {
			return 1
		}
		return 0
	case "sync":
		return runClient(socketPath)
	default:
		setupLogger()
		slog.Error("unknown subcommand", "command", cmd, "valid", "daemon, sync, health")
		return 2
	}
}
