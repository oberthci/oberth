// Command oberth-source-plugin serves the native source artifact socket.
//
// Spec: docs/plans/c6-source-delivery.md#owned-data-and-public-contract
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/oberthci/oberth/internal/sourceplugin"
	"k8s.io/utils/clock"
)

func main() { os.Exit(run()) }
func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	cfg := sourceplugin.Environment()
	cfg.Now = clock.RealClock{}.Now
	return sourceplugin.Run(ctx, os.Args[1:], cfg, os.Stderr)
}
