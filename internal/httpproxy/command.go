package httpproxy

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/connector"
)

const usage = "usage: mandate http-proxy --config FILE --listen SOCKET [--credentials-dir DIR] [--approvers FILE --rp-id ID --origin URL --seen FILE]"

// Command runs `mandate http-proxy`.
func Command(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("http-proxy", flag.ContinueOnError)
	configPath := flags.String("config", "", "JSON service config (required)")
	credDir := flags.String("credentials-dir", os.Getenv("CREDENTIALS_DIRECTORY"), "directory holding the token files (default $CREDENTIALS_DIRECTORY)")
	listen := flags.String("listen", "", "Unix socket to serve on (required)")
	approversPath := flags.String("approvers", "", "JSON file of passkeys that may approve writes; without it writes are refused")
	rpID := flags.String("rp-id", "", "WebAuthn relying party ID of the console")
	origin := flags.String("origin", "", "origin of the console, e.g. https://mandate.example.com")
	seenPath := flags.String("seen", "", "file recording spent approval nonces")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected arguments %q; %s", flags.Args(), usage)
	}
	if *configPath == "" || *listen == "" || *credDir == "" {
		return errors.New("--config, --listen, and --credentials-dir (or $CREDENTIALS_DIRECTORY) are required; " + usage)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil)).With("source", "http-proxy")

	services, err := LoadConfig(*configPath)
	if err != nil {
		return err
	}
	p, err := New(services, *credDir, log)
	if err != nil {
		return err
	}
	srv := &connector.Server{Connector: p, Log: log}
	if *approversPath == "" {
		log.Warn("no --approvers; writes are refused")
	} else {
		if *rpID == "" || *origin == "" || *seenPath == "" {
			return errors.New("--approvers needs --rp-id, --origin, and --seen")
		}
		approvers, err := approval.LoadApprovers(*approversPath)
		if err != nil {
			return err
		}
		seen, err := approval.OpenSeen(*seenPath)
		if err != nil {
			return fmt.Errorf("open seen: %w", err)
		}
		srv.Verifier = &approval.Verifier{RPID: *rpID, Origin: *origin, Approvers: approvers}
		srv.Seen = seen
	}
	log.Info("serving", "socket", *listen, "services", p.names)
	return connector.ServeUnix(ctx, *listen, p.Handler(srv))
}
