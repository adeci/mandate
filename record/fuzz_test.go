package record

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// Record bytes come from devices and the server's store: whatever decodes is
// canonical, decodes again to the same record, and encodes to the same bytes
// when the kind's rules hold.
func FuzzDecode(f *testing.F) {
	for _, v := range readSpec(f).vectors {
		f.Add(v.kind, []byte(v.record))
	}
	f.Fuzz(func(t *testing.T, kind string, b []byte) {
		r, err := decode(kind, b)
		if err != nil {
			return
		}
		if r.Kind() != kind {
			t.Fatalf("%s bytes decoded as a %s record", kind, r.Kind())
		}
		c, err := canonical(r)
		if err != nil || !bytes.Equal(c, b) {
			t.Fatalf("accepted %q, which encodes as %q, %v", b, c, err)
		}
		again, err := decode(kind, c)
		if err != nil || !reflect.DeepEqual(again, r) {
			t.Fatalf("re-encoding %q decodes as %+v, %v", b, again, err)
		}
		if r.check() == nil {
			if e, err := Encode(r); err != nil || !bytes.Equal(e, b) {
				t.Fatalf("Encode of accepted %q = %q, %v", b, e, err)
			}
		}
	})
}

// Envelopes arrive as JSON from devices: whatever decodes marshals stably,
// keeps its challenge, and verifies against the spec's log without panicking.
func FuzzEnvelope(f *testing.F) {
	s := readSpec(f)
	l := s.log(f)
	f.Add([]byte(s.envJSON))
	for _, v := range s.vectors {
		b, err := json.Marshal(s.envelope(v))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		var e Envelope
		if json.Unmarshal(b, &e) != nil {
			return
		}
		j, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("decoded %q but cannot marshal it: %v", b, err)
		}
		var e2 Envelope
		if err := json.Unmarshal(j, &e2); err != nil {
			t.Fatalf("%q marshals as %q, which does not decode: %v", b, j, err)
		}
		if j2, _ := json.Marshal(e2); !bytes.Equal(j2, j) {
			t.Fatalf("%q marshals as %q, then as %q", b, j, j2)
		}
		if !bytes.Equal(e.Challenge(), e2.Challenge()) {
			t.Fatalf("%q changes its challenge through JSON", b)
		}
		r, err := l.Verify(e, e.IssuedAt.Add(time.Minute))
		if err != nil {
			return
		}
		if c, err := Encode(r); err != nil || !bytes.Equal(c, e.Record) {
			t.Fatalf("verified record %q encodes as %q, %v", e.Record, c, err)
		}
	})
}
