package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/coltonkirsten/a12s/internal/controller"
)

// runController is `a12s controller`: the Controller process, run inside
// the container a12s-<fleet>-controller. It is configured by the
// environment: A12S_FLEET, A12S_API_TOKEN, A12S_STATE_DIR (default
// /var/lib/a12s), A12S_SUBNET_POOL (default 10.213.0.0/16) and optionally
// A12S_API_ADDR.
func runController(args []string) error {
	fs := flag.NewFlagSet("controller", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: a12s controller   (configured by A12S_* environment variables; started by a12s up)")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return fmt.Errorf("controller takes no arguments")
	}
	cfg, err := controller.ConfigFromEnv()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	return controller.Run(ctx, cfg)
}
