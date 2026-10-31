// Command mandate is the agent CLI, the gateway, and the console. Connectors
// are separate programs; see package connector.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/adeci/mandate/internal/admin"
	"github.com/adeci/mandate/internal/client"
	"github.com/adeci/mandate/internal/console"
	"github.com/adeci/mandate/internal/exitcode"
	"github.com/adeci/mandate/internal/gateway"
)

const usage = `usage: mandate <command> [args]

for agents (set MANDATE_URL and MANDATE_TOKEN):
  guide                   print the agent guide (markdown, from the live schema)
  query SQL [PARAM...]    run a read-only query
  schema                  list the views and connector ops
  read CONNECTOR OP [KEY=VALUE...]
                          run a connector read op
  propose CONNECTOR OP [KEY=VALUE...] [--note TEXT] [--wait DURATION]
                          propose a write; prints the URL to send the owner
  status ID [--wait DURATION]
                          show a proposal; --wait blocks until the owner decides
  http METHOD SERVICE PATH [--data BODY] [--note TEXT] [--wait DURATION]
                          GET/HEAD read through the proxy; other methods propose
  exit: 0 done, 75 pending/paced/timed out, 1 rejected/expired/failed/unknown/error, 2 usage

on the host:
  gateway                 serve the gateway
  console                 serve the owner's console and the proposal API
  passkey add --name NAME
                          print a one-time passkey enrollment link (as root)
  list [--senders] DB     list chat and sender IDs for exposure.toml
`

func main() {
	os.Exit(run())
}

// run returns the status instead of exiting so deferred cleanup runs.
func run() int {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "guide":
		err = client.Guide(ctx, args)
	case "query":
		err = client.Query(ctx, args)
	case "schema":
		err = client.Schema(ctx, args)
	case "read":
		err = client.Read(ctx, args)
	case "propose":
		err = client.Propose(ctx, args)
	case "status":
		err = client.Status(ctx, args)
	case "http":
		err = client.HTTP(ctx, args)
	case "gateway":
		err = gateway.Command(ctx, args)
	case "console":
		err = console.Command(ctx, args)
	case "passkey":
		err = console.Passkey(ctx, args)
	case "list":
		err = admin.List(ctx, args)
	case "-h", "--help", "help":
		fmt.Print(usage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		return 2
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "mandate %s: %v\n", cmd, err)
	}
	return exitcode.For(err)
}
