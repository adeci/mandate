// Command mandate runs mandate's server processes and is the agent CLI.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/adeci/mandate/internal/cli"
	"github.com/adeci/mandate/internal/config"
	"github.com/adeci/mandate/internal/conformance"
	"github.com/adeci/mandate/internal/front"
	"github.com/adeci/mandate/internal/gateway"
	"github.com/adeci/mandate/internal/owner"
)

const usage = `usage:
  mandate serve front
  mandate serve gateway
  mandate serve owner
  sudo mandate setup
  sudo mandate reset
  mandate conformance PROGRAM [--config FILE] [--store FILE]`

var errUsage = errors.New("bad usage")

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:])
	stop()
	switch {
	case errors.Is(err, errUsage):
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	case err != nil:
		fmt.Fprintf(os.Stderr, "mandate: %v\n", err)
		os.Exit(cli.ExitCode(err))
	}
}

func run(ctx context.Context, args []string) error {
	switch {
	case len(args) > 0 && args[0] == "serve":
		if len(args) != 2 {
			return errUsage
		}
		return serve(ctx, args[1])
	case len(args) == 1 && (args[0] == "setup" || args[0] == "reset"):
		cfg, err := config.Load(config.File())
		if err != nil {
			return err
		}
		if args[0] == "setup" {
			return owner.Setup(ctx, cfg, os.Stdout)
		}
		return owner.Reset(ctx, cfg, os.Stdout)
	case len(args) > 0 && args[0] == "conformance":
		return conformance.Run(ctx, args[1:], os.Stdout)
	}
	return cli.Run(ctx, args)
}

func serve(ctx context.Context, process string) error {
	var start func(context.Context, *config.Config) error
	switch process {
	case "front":
		start = front.Serve
	case "gateway":
		start = gateway.Serve
	case "owner":
		start = owner.Serve
	default:
		return errUsage
	}
	cfg, err := config.Load(config.File())
	if err != nil {
		return err
	}
	return start(ctx, cfg)
}
