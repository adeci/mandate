// Command mandate is the agent CLI, the gateway, and the console. Connectors
// are separate programs; see package connector.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/adeci/mandate/internal/admin"
	"github.com/adeci/mandate/internal/client"
	"github.com/adeci/mandate/internal/console"
	"github.com/adeci/mandate/internal/exitcode"
	"github.com/adeci/mandate/internal/gateway"
)

const hostUsage = `
on the host:
  gateway                 serve the gateway
  console                 serve the owner's console and the proposal API
  list [--senders] DB     list chat and sender IDs for exposure.toml
`

func main() {
	os.Exit(run())
}

// run returns the status instead of exiting so deferred cleanup runs.
func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(os.Args) < 2 {
		usage(ctx, os.Stderr)
		return 2
	}

	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "status":
		err = client.Status(ctx, args)
	case "gateway":
		err = gateway.Command(ctx, args)
	case "console":
		err = console.Command(ctx, args)
	case "list":
		err = admin.List(ctx, args)
	case "-h", "--help", "help":
		usage(ctx, os.Stdout)
		return 0
	default:
		err = client.Connector(ctx, cmd, args)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "mandate %s: %v\n", cmd, err)
	}
	return exitcode.For(err)
}

func usage(ctx context.Context, w io.Writer) {
	client.Help(ctx, w)
	fmt.Fprint(w, hostUsage)
}
