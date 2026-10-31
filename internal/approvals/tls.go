package approvals

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// certReloader serves the certificate at certFile/keyFile, reloading it when
// either file changes, e.g. after `tailscale cert` renews it.
type certReloader struct {
	certFile, keyFile string
	log               *slog.Logger

	mu    sync.Mutex
	cert  *tls.Certificate
	stamp [2]fileStamp
}

type fileStamp struct {
	mod  time.Time
	size int64
}

func newCertReloader(certFile, keyFile string, log *slog.Logger) (*certReloader, error) {
	c := &certReloader{certFile: certFile, keyFile: keyFile, log: log}
	if _, err := c.get(nil); err != nil {
		return nil, err
	}
	return c, nil
}

func stampOf(path string) (fileStamp, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return fileStamp{}, err
	}
	return fileStamp{mod: fi.ModTime(), size: fi.Size()}, nil
}

func (c *certReloader) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var stamp [2]fileStamp
	var err error
	for i, p := range []string{c.certFile, c.keyFile} {
		if stamp[i], err = stampOf(p); err != nil {
			break
		}
	}
	if err == nil && c.cert != nil && stamp == c.stamp {
		return c.cert, nil
	}
	if err == nil {
		var cert tls.Certificate
		cert, err = tls.LoadX509KeyPair(c.certFile, c.keyFile)
		if err == nil {
			c.cert, c.stamp = &cert, stamp
			return c.cert, nil
		}
	}
	// A renewal may be caught halfway, with a new cert and the old key;
	// keep serving the previous pair until both files agree.
	if c.cert != nil {
		c.log.Warn("reload TLS certificate", "err", err)
		return c.cert, nil
	}
	return nil, fmt.Errorf("load TLS certificate: %w", err)
}
