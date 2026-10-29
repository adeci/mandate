// Command sieve is a multi-call binary: the read-only gateway and one ingester
// per messaging source.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/adeci/sieve/internal/admin"
	"github.com/adeci/sieve/internal/discord"
	"github.com/adeci/sieve/internal/exitcode"
	"github.com/adeci/sieve/internal/gateway"
	"github.com/adeci/sieve/internal/gmessages"
	"github.com/adeci/sieve/internal/whatsapp"
)

const usage = `usage: sieve <command> [args]

commands:
  gateway                 serve the read-only SQL gateway
  whatsapp run|pair       WhatsApp linked-device ingester
  messages run|pair       Google Messages (SMS/RCS) ingester
  discord serve           Discord on-demand reader (gateway-only socket)
  list <data.db>          list containers and senders (for exposure.toml)
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
	case "gateway":
		err = gateway.Command(ctx, args)
	case "whatsapp":
		err = whatsapp.Command(ctx, args)
	case "messages":
		err = gmessages.Command(ctx, args)
	case "discord":
		err = discord.Command(ctx, args)
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
		fmt.Fprintf(os.Stderr, "sieve %s: %v\n", cmd, err)
	}
	return exitcode.For(err)
}
