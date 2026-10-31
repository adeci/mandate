// Command mandate is a multi-call binary: the read-only gateway and one ingester
// per messaging source.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/adeci/mandate/internal/admin"
	"github.com/adeci/mandate/internal/client"
	"github.com/adeci/mandate/internal/discord"
	"github.com/adeci/mandate/internal/exitcode"
	"github.com/adeci/mandate/internal/gateway"
	"github.com/adeci/mandate/internal/gmessages"
	"github.com/adeci/mandate/internal/httpproxy"
	"github.com/adeci/mandate/internal/whatsapp"
)

const usage = `usage: mandate <command> [args]

for agents (set MANDATE_URL and MANDATE_TOKEN):
  query SQL [PARAM...]    run a read-only query
  schema                  list the views you can query
  discord sync|search|fetch
                          read Discord on demand

on the host:
  gateway                 serve the gateway
  whatsapp run|pair       WhatsApp linked device
  messages run|pair       Google Messages (SMS/RCS)
  discord serve           Discord reader behind the gateway
  http-proxy              token-holding proxy for HTTP APIs
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
	case "query":
		err = client.Query(ctx, args)
	case "schema":
		err = client.Schema(ctx, args)
	case "gateway":
		err = gateway.Command(ctx, args)
	case "whatsapp":
		err = whatsapp.Command(ctx, args)
	case "messages":
		err = gmessages.Command(ctx, args)
	case "discord":
		if len(args) > 0 && args[0] == "serve" {
			err = discord.Command(ctx, args)
		} else {
			err = client.Discord(ctx, args)
		}
	case "http-proxy":
		err = httpproxy.Command(ctx, args)
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
