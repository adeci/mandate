package exchange

import (
	"bytes"
	"testing"
)

// The server opens what a new party sealed, whatever the new party chose to
// seal: a payload opens to the bytes sealed, and one that decodes encodes back
// to them.
func FuzzOpenPayload(f *testing.F) {
	isk := unhex(f, `000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f
		202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f`)
	id := unhex(f, "000102030405060708090a0b0c0d0e0f")
	hash := `"token_sha256":"3138bb9bc78df27c473ecfd1410f7bd45ebac1f59cf3ff9cfe4db77aab7aedd3"}`
	f.Add(Agent, []byte(`{`+hash))
	f.Add(Device, []byte(`{"key":`+phoneKey+`,`+hash))
	f.Fuzz(func(t *testing.T, kind string, payload []byte) {
		if _, err := open(isk, id, payload); err == nil {
			t.Fatalf("opened unsealed %q", payload)
		}
		b, err := open(isk, id, seal(isk, id, payload))
		if err != nil || !bytes.Equal(b, payload) {
			t.Fatalf("sealed %q opens as %q, %v", payload, b, err)
		}
		p, err := decodePayload(kind, b)
		if err != nil {
			return
		}
		if enc, err := p.encode(kind); err != nil || !bytes.Equal(enc, b) {
			t.Fatalf("accepted %q encodes as %q, %v", b, enc, err)
		}
	})
}
