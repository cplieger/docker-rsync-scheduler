package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/cplieger/health"
)

// main dispatches on the first argument: `health` runs the Docker probe,
// `sync` triggers one pass via the daemon's socket and exits with that
// pass's result, and `daemon` runs the long-lived daemon that owns all
// passes. Anything else, including no argument, exits 2.
func main() {
	if len(os.Args) > 1 && os.Args[1] == "health" {
		// Stays silent: runs before any logger is configured, and RunProbe
		// reports through its exit code and its own stderr line.
		slog.SetDefault(slog.New(slog.DiscardHandler))
		health.RunProbe(healthMarkerPath, probeOptions()...)
	}
	os.Exit(dispatch())
}

// dispatch selects the subcommand and returns the process exit code so
// deferred cleanup in the daemon runs before the process exits.
func dispatch() int {
	var cmd string
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	setupLogger()
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
