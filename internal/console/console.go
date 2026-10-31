// Package console is where the owner approves agents' proposals, links
// connectors, and manages the trusted passkeys. It stores the proposals the
// gateway submits, keeps the key log that every connector copies, and hands
// approved records to connectors, which verify them again before executing.
package console

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/adeci/mandate/connector"
)

const usage = "usage: mandate console --hostname HOST --tls-cert FILE --tls-key FILE --state DIR " +
	"--socket PATH --admin-password-hash-file FILE [--listen ADDR] [--connector NAME=SOCKET ...]"

type connectorFlags map[string]string

func (c connectorFlags) String() string { return fmt.Sprint(map[string]string(c)) }

func (c connectorFlags) Set(v string) error {
	name, sock, ok := strings.Cut(v, "=")
	if !ok || name == "" || sock == "" {
		return fmt.Errorf("want NAME=SOCKET, got %q", v)
	}
	if _, dup := c[name]; dup {
		return fmt.Errorf("connector %q given twice", name)
	}
	c[name] = sock
	return nil
}

func Command(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("console", flag.ContinueOnError)
	listen := flags.String("listen", ":443", "HTTPS address of the console")
	hostname := flags.String("hostname", "", "the console's hostname, also the WebAuthn RP ID (required)")
	certFile := flags.String("tls-cert", "", "TLS certificate chain (required)")
	keyFile := flags.String("tls-key", "", "TLS key (required)")
	stateDir := flags.String("state", "", "directory for the database and key log (required)")
	socket := flags.String("socket", "", "Unix socket for the gateway's proposal API (required)")
	hashFile := flags.String("admin-password-hash-file", "", "bcrypt hash of the admin password (required)")
	conns := connectorFlags{}
	flags.Var(conns, "connector", "NAME=SOCKET of a connector; repeatable")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *hostname == "" || *certFile == "" || *keyFile == "" || *stateDir == "" || *socket == "" || *hashFile == "" {
		return errors.New(usage)
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected arguments %q; %s", flags.Args(), usage)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil)).With("unit", "console")

	_, listenPort, err := net.SplitHostPort(*listen)
	if err != nil {
		return fmt.Errorf("--listen: %w", err)
	}
	port, err := strconv.Atoi(listenPort)
	if err != nil {
		return fmt.Errorf("--listen: %w", err)
	}
	hash, err := loadPasswordHash(*hashFile)
	if err != nil {
		return err
	}
	cert, err := tls.LoadX509KeyPair(*certFile, *keyFile)
	if err != nil {
		return fmt.Errorf("load TLS certificate: %w", err)
	}
	origin := originOf(*hostname, port)
	srv, err := New(ctx, Config{
		RPID:         *hostname,
		Origin:       origin,
		PasswordHash: hash,
		Connectors:   conns,
		StateDir:     *stateDir,
		Log:          log,
	})
	if err != nil {
		return err
	}
	defer srv.Close()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go srv.Sweep(ctx, 30*time.Second)

	errc := make(chan error, 2)
	go func() { errc <- wrap("proposal socket", connector.ServeUnix(ctx, *socket, srv.ProposalHandler())) }()
	go func() { errc <- wrap("https", serveTLS(ctx, *listen, srv.Handler(), cert)) }()
	n, _ := srv.keys.Head()
	log.Info("console listening", "origin", origin, "socket", *socket, "connectors", len(conns), "key_log", n)

	// Every server returns nil on shutdown; the first to stop takes the other
	// down with it.
	err = <-errc
	cancel()
	return errors.Join(err, <-errc)
}

func loadPasswordHash(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read admin password hash: %w", err)
	}
	b = bytes.TrimSpace(b)
	_, err = bcrypt.Cost(b)
	if v := string(b[:min(len(b), 4)]); err != nil || v != "$2a$" && v != "$2b$" && v != "$2y$" {
		return nil, fmt.Errorf("%s is not a bcrypt hash", path)
	}
	return b, nil
}

func wrap(what string, err error) error {
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}

// originOf is how browsers spell the console's origin, which omits the
// default port.
func originOf(host string, port int) string {
	if port == 443 {
		return "https://" + host
	}
	return "https://" + net.JoinHostPort(host, strconv.Itoa(port))
}

func serveTLS(ctx context.Context, addr string, h http.Handler, cert tls.Certificate) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		// Approving waits for the connector to execute.
		WriteTimeout: executeTimeout + 30*time.Second,
		IdleTimeout:  2 * time.Minute,
		TLSConfig:    &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}},
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ServeTLS(ln, "", "") }()
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	case err := <-errc:
		return err
	}
}
