package front

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"github.com/adeci/mandate/internal/config"
)

// tlsConfig returns the GetCertificate and NextProtos for cfg's TLS mode.
func tlsConfig(cfg *config.Config, state string) (*tls.Config, error) {
	protos := []string{"h2", "http/1.1"}
	switch cfg.TLS.Mode {
	case "own":
		cert, err := ownCert(state)
		if err != nil {
			return nil, err
		}
		return &tls.Config{
			NextProtos:     protos,
			GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return cert, nil },
		}, nil
	case "acme":
		m := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			Cache:      autocert.DirCache(filepath.Join(state, "acme")),
			HostPolicy: autocert.HostWhitelist(cfg.TLS.Domain),
		}
		return &tls.Config{
			NextProtos:     append(protos, "acme-tls/1"),
			GetCertificate: m.GetCertificate,
		}, nil
	case "cert":
		f := &certFiles{cert: cfg.TLS.Cert, key: cfg.TLS.Key}
		if _, err := f.get(nil); err != nil {
			return nil, err
		}
		return &tls.Config{NextProtos: protos, GetCertificate: f.get}, nil
	}
	return nil, fmt.Errorf("unknown TLS mode %q", cfg.TLS.Mode)
}

// ownCert loads the key and self-signed certificate front made on its
// first start, making them if this is it. Clients pin the key, so it never
// changes.
func ownCert(state string) (*tls.Certificate, error) {
	file := filepath.Join(state, "own.pem")
	b, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		b, err = makeOwnCert(file)
	}
	if err != nil {
		return nil, fmt.Errorf("own certificate: %w", err)
	}
	cert, err := tls.X509KeyPair(b, b)
	if err != nil {
		return nil, fmt.Errorf("own certificate %s: %w", file, err)
	}
	return &cert, nil
}

func makeOwnCert(file string) ([]byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "mandate"},
		NotBefore:    time.Now().Add(-time.Hour),
		// RFC 5280's "no well-defined expiration date": the key is pinned.
		NotAfter:    time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	b := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	b = append(b, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})...)

	dir := filepath.Dir(file)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(dir, ".own.pem.*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(f.Name(), file); err != nil {
		return nil, err
	}
	return b, nil
}

// certFiles serves the owner's certificate and loads it again once the
// files change.
type certFiles struct {
	cert, key string

	mu      sync.Mutex
	stamp   [2]stamp
	current *tls.Certificate
}

type stamp struct {
	mod  int64
	size int64
}

func (f *certFiles) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var now [2]stamp
	for i, name := range []string{f.cert, f.key} {
		fi, err := os.Stat(name)
		if err != nil {
			if f.current != nil {
				return f.current, nil
			}
			return nil, fmt.Errorf("certificate: %w", err)
		}
		now[i] = stamp{fi.ModTime().UnixNano(), fi.Size()}
	}
	if f.current != nil && now == f.stamp {
		return f.current, nil
	}
	// Remember the stamp even when loading fails: an update may have
	// replaced one file but not yet the other, and the next change retries.
	f.stamp = now
	cert, err := tls.LoadX509KeyPair(f.cert, f.key)
	if err != nil {
		if f.current != nil {
			log.Printf("keep the previous certificate: %v", err)
			return f.current, nil
		}
		return nil, fmt.Errorf("certificate: %w", err)
	}
	f.current = &cert
	return f.current, nil
}
