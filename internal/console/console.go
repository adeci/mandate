// Package console is where the owner approves agents' proposals and links
// connectors. It stores the proposals the gateway submits, serves a web app
// behind passkey login, and hands approved records to connectors, which
// verify them again before executing. Passkeys it enrolls are trusted only
// once the owner pins them in the NixOS config, so a compromised console
// can't add its own.
package console

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/connector"
)

const defaultAdminSocket = "/run/mandate-console/admin.sock"

const usage = "usage: mandate console --hostname HOST --tls-cert FILE --tls-key FILE --state DIR " +
	"--socket PATH [--listen ADDR] [--approvers FILE] [--connector NAME=SOCKET]... [--admin-socket PATH]"

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
	stateDir := flags.String("state", "", "directory for the database and enrolled.json (required)")
	approversFile := flags.String("approvers", "", "JSON array of pinned passkeys; missing or empty allows enrollment only")
	socket := flags.String("socket", "", "Unix socket for the gateway's proposal API (required)")
	adminSocket := flags.String("admin-socket", defaultAdminSocket, "owner-only Unix socket for mandate passkey add")
	conns := connectorFlags{}
	flags.Var(conns, "connector", "NAME=SOCKET of a connector; repeatable")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *hostname == "" || *certFile == "" || *keyFile == "" || *stateDir == "" || *socket == "" {
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
	approvers, err := loadApprovers(*approversFile)
	if err != nil {
		return err
	}
	if len(approvers) == 0 {
		log.Warn("no passkeys pinned; enrollment only, nothing can log in or approve")
	}
	cert, err := tls.LoadX509KeyPair(*certFile, *keyFile)
	if err != nil {
		return fmt.Errorf("load TLS certificate: %w", err)
	}
	origin := originOf(*hostname, port)
	srv, err := New(ctx, Config{
		RPID:       *hostname,
		Origin:     origin,
		Approvers:  approvers,
		Connectors: conns,
		StateDir:   *stateDir,
		Log:        log,
	})
	if err != nil {
		return err
	}
	defer srv.Close()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go srv.Sweep(ctx, 30*time.Second)

	errc := make(chan error, 3)
	go func() { errc <- wrap("proposal socket", connector.ServeUnix(ctx, *socket, srv.ProposalHandler())) }()
	go func() { errc <- wrap("admin socket", serveAdmin(ctx, *adminSocket, srv.AdminHandler())) }()
	go func() { errc <- wrap("https", serveTLS(ctx, *listen, srv.Handler(), cert)) }()
	log.Info("console listening", "origin", origin, "socket", *socket,
		"connectors", len(conns), "approvers", len(approvers))

	// Every server returns nil on shutdown; the first to stop takes the rest
	// down with it.
	err = <-errc
	cancel()
	for range 2 {
		err = errors.Join(err, <-errc)
	}
	return err
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

func loadApprovers(path string) ([]approval.Approver, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && strings.TrimSpace(string(b)) == "") {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read approvers: %w", err)
	}
	return approval.LoadApprovers(path)
}

func serveAdmin(ctx context.Context, path string, h http.Handler) error {
	ln, err := listenPrivate(path)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	return serve(ctx, srv, func() error { return srv.Serve(ln) })
}

// listenPrivate listens on a 0600 Unix socket at path. It binds inside a
// fresh 0700 directory and renames the socket into place, so no one else can
// connect even between bind and chmod.
func listenPrivate(path string) (net.Listener, error) {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&fs.ModeSocket == 0 {
		return nil, fmt.Errorf("%s exists and is not a socket", path)
	}
	dir, err := os.MkdirTemp(filepath.Dir(path), ".admin-")
	if err != nil {
		return nil, err
	}
	tmp := filepath.Join(dir, "sock")
	defer func() {
		os.Remove(tmp)
		os.Remove(dir)
	}()
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "unix", tmp)
	if err != nil {
		return nil, err
	}
	// The path changes below; closing must not unlink whatever is at tmp.
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := os.Chmod(tmp, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
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
	return serve(ctx, srv, func() error { return srv.ServeTLS(ln, "", "") })
}

func serve(ctx context.Context, srv *http.Server, run func() error) error {
	errc := make(chan error, 1)
	go func() { errc <- run() }()
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	case err := <-errc:
		return err
	}
}
