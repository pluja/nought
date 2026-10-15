// SPDX-License-Identifier: AGPL-3.0-or-later

// Command nou is the nought mail client.
package main

import (
	"context"
	"os"
	"os/signal"

	"nought/internal/cli"
)

// version is set at release time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return cli.Run(ctx, cli.Options{
		Args:    os.Args[1:],
		Stdin:   os.Stdin,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Getenv:  os.Getenv,
		Version: version,
	})
}
