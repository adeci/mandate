// Package front terminates TLS on mandate's one port and forwards requests
// to the gateway and owner sockets.
package front

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/adeci/mandate/internal/config"
)

const maxBody = 1 << 20

// Serve listens on cfg.Listen until ctx ends.
func Serve(ctx context.Context, cfg *config.Config) error {
	tc, err := tlsConfig(cfg, config.StateDir("front"))
	if err != nil {
		return err
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	return serve(ctx, ln, tc)
}

func serve(ctx context.Context, ln net.Listener, tc *tls.Config) error {
	l := &listener{Listener: ln, config: tc}
	srv := &http.Server{
		Handler:     handler(config.Socket("gateway"), config.Socket("owner")),
		ConnContext: l.connContext,
		// No read or write timeout: server-sent events stay open and long
		// polls last up to 900 seconds. A request body is capped instead.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(l) }()
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// Streams and long polls never go idle; close what is left.
		if err := srv.Shutdown(shutdown); err != nil {
			srv.Close()
		}
		return nil
	case err := <-errc:
		return fmt.Errorf("serve: %w", err)
	}
}

func handler(gatewaySock, ownerSock string) http.Handler {
	gateway, owner := proxy(gatewaySock), proxy(ownerSock)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		var h http.Handler
		switch {
		// Refuse what a backend could read as another path, such as
		// "/agent/%2e%2e/internal/".
		case p != path.Clean(p) && p != path.Clean(p)+"/":
		case strings.HasPrefix(p, "/agent/"):
			h = gateway
		case strings.HasPrefix(p, "/owner/"), p == "/setup":
			h = owner
		}
		if h == nil {
			http.NotFound(w, r)
			return
		}
		if r.ContentLength > maxBody {
			http.Error(w, http.StatusText(http.StatusRequestEntityTooLarge), http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		h.ServeHTTP(w, r)
	})
}

func proxy(sock string) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = "mandate"
			// Rewrite starts without the client's X-Forwarded-* headers.
			pr.SetXForwarded()
			spki, _ := pr.In.Context().Value(spkiKey{}).(*string)
			if spki != nil && *spki != "" {
				pr.Out.Header.Set("Mandate-Spki-Sha256", *spki)
			} else {
				pr.Out.Header.Del("Mandate-Spki-Sha256")
			}
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
			MaxIdleConnsPerHost: 32,
			IdleConnTimeout:     90 * time.Second,
			DisableCompression:  true,
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
				http.Error(w, http.StatusText(http.StatusRequestEntityTooLarge), http.StatusRequestEntityTooLarge)
				return
			}
			if r.Context().Err() == nil {
				log.Printf("%s %s: %v", r.Method, r.URL.Path, err)
			}
			w.WriteHeader(http.StatusBadGateway)
		},
	}
}

type spkiKey struct{}

// listener does the TLS handshake with a per-connection config to learn
// which certificate this connection was served; the standard library does
// not expose it.
type listener struct {
	net.Listener
	config *tls.Config // GetCertificate and NextProtos
	spki   sync.Map    // *tls.Conn → *string, until connContext takes it
}

func (l *listener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	spki := new(string)
	tc := tls.Server(c, &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: l.config.NextProtos,
		// A resumed session skips GetCertificate and would leave spki empty.
		SessionTicketsDisabled: true,
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			cert, err := l.config.GetCertificate(hello)
			if err != nil {
				return nil, err
			}
			leaf := cert.Leaf
			if leaf == nil {
				if leaf, err = x509.ParseCertificate(cert.Certificate[0]); err != nil {
					return nil, fmt.Errorf("parse certificate: %w", err)
				}
			}
			*spki = spkiSHA256(leaf)
			return cert, nil
		},
	})
	l.spki.Store(tc, spki)
	return tc, nil
}

func (l *listener) connContext(ctx context.Context, c net.Conn) context.Context {
	spki, _ := l.spki.LoadAndDelete(c)
	return context.WithValue(ctx, spkiKey{}, spki)
}

func spkiSHA256(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:])
}

// SPKI returns the hex SHA-256 of the SPKI of the certificate front serves,
// for `mandate setup` to print. It asks the running front.
func SPKI(ctx context.Context, cfg *config.Config) (string, error) {
	host, port, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		return "", fmt.Errorf("listen: %w", err)
	}
	if ip := net.ParseIP(host); host == "" || ip != nil && ip.IsUnspecified() {
		host = "localhost"
	}
	d := tls.Dialer{Config: &tls.Config{
		ServerName: cfg.TLS.Domain,
		// Only the key is read; nothing is sent over this connection.
		InsecureSkipVerify: true,
	}}
	c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return "", fmt.Errorf("connect to front: %w", err)
	}
	defer c.Close()
	return spkiSHA256(c.(*tls.Conn).ConnectionState().PeerCertificates[0]), nil
}
