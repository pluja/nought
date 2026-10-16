// SPDX-License-Identifier: AGPL-3.0-or-later

// Command nou is the nought mail client.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/pluja/nought/internal/cli"
)

// version is set at release time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// After the first signal, restore default handling so a second one
	// terminates immediately if cancellation is slow.
	context.AfterFunc(ctx, stop)
	return cli.Run(ctx, cli.Options{
		Args:    os.Args[1:],
		Stdin:   os.Stdin,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Getenv:  os.Getenv,
		Version: version,
	})
}
