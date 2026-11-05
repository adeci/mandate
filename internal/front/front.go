// Package front terminates TLS on mandate's port and forwards requests to
// the gateway and owner sockets.
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
	"github.com/adeci/mandate/internal/peer"
)

const maxBody = 1 << 20

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
	var agentLn net.Listener
	if cfg.AgentListen != "" {
		if agentLn, err = lc.Listen(ctx, "tcp", cfg.AgentListen); err != nil {
			ln.Close()
			return fmt.Errorf("agent listen: %w", err)
		}
	}
	return serve(ctx, tc, ln, agentLn)
}

// serve answers everything on ln and only the agent API on agentLn, if any.
func serve(ctx context.Context, tc *tls.Config, ln, agentLn net.Listener) error {
	gateway, owner := proxy(config.Socket("gateway")), proxy(config.Socket("owner"))
	// Streams and long polls never go idle; they end once shutdown begins.
	stopping, stop := context.WithCancel(context.Background())
	defer stop()
	lns := []net.Listener{ln}
	handlers := []http.Handler{handler(stopping, gateway, owner)}
	if agentLn != nil {
		lns = append(lns, agentLn)
		handlers = append(handlers, handler(stopping, gateway, nil))
	}
	srvs := make([]*http.Server, len(lns))
	errc := make(chan error, len(lns))
	for i, ln := range lns {
		l := &listener{Listener: ln, config: tc}
		srvs[i] = &http.Server{
			Handler:     handlers[i],
			ConnContext: l.connContext,
			// No read or write timeout: server-sent events stay open and long
			// polls last up to 900 seconds. A request body is capped instead.
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       2 * time.Minute,
			MaxHeaderBytes:    64 << 10,
		}
		srvs[i].RegisterOnShutdown(stop)
		go func() { errc <- srvs[i].Serve(l) }()
	}
	var err error
	select {
	case <-ctx.Done():
	case err = <-errc:
		err = fmt.Errorf("serve: %w", err)
	}
	// Streams and long polls end on stopping; the rest gets a few seconds.
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, srv := range srvs {
		if srv.Shutdown(shutdown) != nil {
			srv.Close()
		}
	}
	return err
}

// handler routes to owner too unless it is nil. Streams and long polls
// end once stopping is done.
func handler(stopping context.Context, gateway, owner http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		var h http.Handler
		switch {
		// Refuse what a backend could read as another path, such as
		// "/agent/%2e%2e/internal/".
		case p != path.Clean(p) && p != path.Clean(p)+"/":
		case strings.HasPrefix(p, "/agent/"):
			h = gateway
		case p == "/", strings.HasPrefix(p, "/owner/"), p == "/setup", strings.HasPrefix(p, "/setup/"):
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
		if p == "/owner/v1/stream" || r.URL.Query().Has("wait") {
			ctx, cancel := context.WithCancel(r.Context())
			defer cancel()
			defer context.AfterFunc(stopping, cancel)()
			r = r.WithContext(ctx)
		}
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
				log.Printf("%s %s: %v", r.Method, peer.LogPath(r.URL.Path), err)
			}
			w.WriteHeader(http.StatusBadGateway)
		},
	}
}

type spkiKey struct{}

// listener records the certificate each connection was served, which
// tls.ConnectionState does not tell.
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

// SPKI asks the running front for the hash of the certificate it serves.
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
