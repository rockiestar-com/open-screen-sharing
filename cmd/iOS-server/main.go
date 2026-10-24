//go:build darwin && !ios

package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := mainCommand.ExecuteContext(ctx)
	stop()
	if err != nil {
		fatal(err)
	}
}
