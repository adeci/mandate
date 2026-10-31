package approvals

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeCert(t *testing.T, certFile, keyFile, cn string, mod time.Time) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	for path, block := range map[string]*pem.Block{
		certFile: {Type: "CERTIFICATE", Bytes: der},
		keyFile:  {Type: "PRIVATE KEY", Bytes: keyDER},
	} {
		if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCertReload(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	t0 := time.Now().Add(-time.Hour)
	writeCert(t, certFile, keyFile, "old", t0)

	r, err := newCertReloader(certFile, keyFile, discard)
	if err != nil {
		t.Fatal(err)
	}
	cn := func() string {
		t.Helper()
		c, err := r.get(nil)
		if err != nil {
			t.Fatal(err)
		}
		leaf, err := x509.ParseCertificate(c.Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		return leaf.Subject.CommonName
	}
	if got := cn(); got != "old" {
		t.Fatalf("cert = %s", got)
	}

	writeCert(t, certFile, keyFile, "new", t0.Add(time.Minute))
	if got := cn(); got != "new" {
		t.Fatalf("after renewal cert = %s, want new", got)
	}

	// Halfway through a renewal the key doesn't match; keep the last good pair.
	if err := os.WriteFile(keyFile, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := cn(); got != "new" {
		t.Fatalf("with a broken key cert = %s, want new", got)
	}

	if _, err := newCertReloader(filepath.Join(dir, "missing.pem"), keyFile, discard); err == nil {
		t.Fatal("missing certificate accepted at startup")
	}
}
