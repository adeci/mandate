// Package approvals is the unit between agents' proposals and the owner's
// passkey. It stores proposals the gateway submits, pushes a notification,
// serves the approval page over HTTPS, and hands approved records to
// connectors, which verify them again before executing. It also runs passkey
// enrollment, whose output becomes trusted only once the owner adds it to the
// approvers file.
package approvals

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
	"strconv"
	"strings"
	"time"

	"github.com/adeci/mandate/internal/approval"
	"github.com/adeci/mandate/internal/connector"
)

const defaultAdminSocket = "/run/mandate-approvals/admin.sock"

const usage = "usage: mandate approvals --hostname HOST --tls-cert FILE --tls-key FILE --state DIR " +
	"--socket PATH [--approvers FILE] [--connector NAME=SOCKET]... [--https-listen ADDR] " +
	"[--origin-port N] [--admin-socket PATH] [--ntfy-url URL --ntfy-topic TOPIC [--ntfy-token-file FILE]]"

func newFlagSet(name string) *flag.FlagSet {
	return flag.NewFlagSet(name, flag.ContinueOnError)
}

// connectorFlags collects repeated --connector name=socket.
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

// Command is `mandate approvals`.
func Command(ctx context.Context, args []string) error {
	flags := newFlagSet("approvals")
	listen := flags.String("https-listen", ":8781", "address for the approval pages")
	hostname := flags.String("hostname", "", "the pages' hostname, also the WebAuthn RP ID (required)")
	originPort := flags.Int("origin-port", 0, "port browsers use to reach the pages, if a proxy changes it (default: the --https-listen port)")
	certFile := flags.String("tls-cert", "", "TLS certificate (required)")
	keyFile := flags.String("tls-key", "", "TLS key (required)")
	stateDir := flags.String("state", "", "directory for the database and enrolled.json (required)")
	approversFile := flags.String("approvers", "", "JSON array of trusted passkeys; missing or empty allows enrollment only")
	socket := flags.String("socket", "", "Unix socket for the gateway's proposal API (required)")
	adminSocket := flags.String("admin-socket", defaultAdminSocket, "root-only Unix socket for mandate approver enroll")
	ntfyURL := flags.String("ntfy-url", "", "ntfy server URL")
	ntfyTopic := flags.String("ntfy-topic", "", "ntfy topic")
	ntfyTokenFile := flags.String("ntfy-token-file", "", "file holding an ntfy access token")
	conns := connectorFlags{}
	flags.Var(conns, "connector", "NAME=SOCKET of a connector with write ops; repeatable")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *hostname == "" || *certFile == "" || *keyFile == "" || *stateDir == "" || *socket == "" {
		return errors.New(usage)
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected arguments %q; %s", flags.Args(), usage)
	}
	if (*ntfyURL == "") != (*ntfyTopic == "") {
		return errors.New("--ntfy-url and --ntfy-topic go together")
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil)).With("unit", "approvals")

	_, listenPort, err := net.SplitHostPort(*listen)
	if err != nil {
		return fmt.Errorf("--https-listen: %w", err)
	}
	port, err := strconv.Atoi(listenPort)
	if err != nil {
		return fmt.Errorf("--https-listen: %w", err)
	}
	if *originPort != 0 {
		port = *originPort
	}

	approvers, err := loadApprovers(*approversFile)
	if err != nil {
		return err
	}
	if len(approvers) == 0 {
		log.Warn("no approvers configured; enrollment only, nothing can be approved")
	}

	var ntfy *Ntfy
	if *ntfyURL != "" {
		ntfy = &Ntfy{URL: *ntfyURL, Topic: *ntfyTopic, Client: &http.Client{Timeout: 10 * time.Second}}
		if *ntfyTokenFile != "" {
			b, err := os.ReadFile(*ntfyTokenFile)
			if err != nil {
				return fmt.Errorf("read ntfy token: %w", err)
			}
			ntfy.Token = strings.TrimSpace(string(b))
		}
	}

	cert, err := tls.LoadX509KeyPair(*certFile, *keyFile)
	if err != nil {
		return fmt.Errorf("load TLS certificate: %w", err)
	}
	srv, err := New(ctx, Config{
		RPID:       *hostname,
		Origin:     originOf(*hostname, port),
		Approvers:  approvers,
		Connectors: conns,
		StateDir:   *stateDir,
		Ntfy:       ntfy,
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
	go func() {
		errc <- wrap("https", serveTLS(ctx, *listen, srv.PageHandler(), cert))
	}()
	log.Info("approvals listening", "origin", originOf(*hostname, port), "socket", *socket,
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

// originOf is how browsers spell the page's origin, which omits the default
// port.
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
	ln, err := connector.ListenUnix(path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return err
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	return serve(ctx, srv, func() error { return srv.Serve(ln) })
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
	}
	srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}
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
