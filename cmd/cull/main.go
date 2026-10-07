// Command cull scores DNGs from their embedded previews with a vision model.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/jefflaplante/cull/internal/cli"
	"github.com/jefflaplante/cull/internal/journal"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := cli.NewRootCmd().ExecuteContext(ctx)
	// Commands release their folder locks as they return; this catches any a path
	// skipped (os.Exit below skips deferred calls).
	journal.RemoveHolders()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
