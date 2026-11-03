package record

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

type vector struct {
	title, kind              string
	record                   string
	nonce                    []byte
	issuedAt, signer         string
	authData, challenge, sig []byte
}

type testKey struct {
	typ, id      string
	d, pub, cred []byte
}

type spec struct {
	hInput, h []byte
	keys      map[string]*testKey
	vectors   []vector
	envJSON   string // the one JSON envelope
}

// readSpec takes the vectors from records.md itself, so the spec and the
// package cannot drift apart.
func readSpec(t *testing.T) spec {
	t.Helper()
	b, err := os.ReadFile("../docs/spec/records.md")
	if err != nil {
		t.Fatal(err)
	}
	unhex := func(s string) []byte {
		b, err := hex.DecodeString(s)
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		return b
	}
	s := spec{keys: map[string]*testKey{}}
	var (
		inBlock bool
		lang    string
		title   string
		key     *testKey
		v       vector
	)
	for line := range strings.Lines(string(b)) {
		line = strings.TrimSuffix(line, "\n")
		if strings.HasPrefix(line, "```") {
			inBlock, lang = !inBlock, line[3:]
			continue
		}
		if !inBlock {
			if strings.HasPrefix(line, "**") {
				title = strings.Trim(line, "*")
			}
			continue
		}
		if lang == "json" {
			if title != "" { // the examples before the vectors are elided
				s.envJSON += line + "\n"
			}
			continue
		}
		f := strings.Fields(line)
		switch {
		case len(f) == 0, len(f) > 1 && f[1] == "=": // blank, or the challenge formula
		case strings.HasPrefix(line, `H("a", "bc") input`):
			s.hInput = unhex(f[len(f)-1])
		case strings.HasPrefix(line, `H("a", "bc")`):
			s.h = unhex(f[len(f)-1])
		case len(f) == 4 && f[2] == "d":
			key = &testKey{typ: f[1], d: unhex(f[3])}
			s.keys[f[0]] = key
		case f[0] == "pub":
			key.pub = unhex(f[1])
		case f[0] == "id":
			key.id = f[1]
		case f[0] == "credential_id":
			if err := (*Bytes)(&key.cred).UnmarshalText([]byte(f[1])); err != nil {
				t.Fatal(err)
			}
		case line[0] == '{':
			kind, _, _ := strings.Cut(title, ",")
			v = vector{title: title, kind: kind, record: line}
		case f[0] == "nonce":
			v.nonce, v.issuedAt, v.signer = unhex(f[1]), f[3], f[5]
		case f[0] == "authenticator_data":
			v.authData = unhex(f[1])
		case f[0] == "challenge":
			v.challenge = unhex(f[1])
		case f[0] == "sig":
			v.sig = unhex(f[1])
			s.vectors = append(s.vectors, v)
		default:
			t.Fatalf("unparsed vector line %q", line)
		}
	}
	if len(s.vectors) != 8 || len(s.keys) != 3 || s.envJSON == "" {
		t.Fatalf("parsed %d vectors and %d keys from records.md", len(s.vectors), len(s.keys))
	}
	return s
}

func (s spec) priv(t *testing.T, name string) *ecdsa.PrivateKey {
	t.Helper()
	priv, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), s.keys[name].d)
	if err != nil {
		t.Fatal(err)
	}
	return priv
}

func (s spec) envelope(v vector) Envelope {
	e := Envelope{
		Kind:              v.kind,
		Record:            []byte(v.record),
		Nonce:             v.nonce,
		Key:               s.keys[v.signer].id,
		AuthenticatorData: v.authData,
		Sig:               v.sig,
	}
	if err := e.IssuedAt.UnmarshalJSON([]byte(`"` + v.issuedAt + `"`)); err != nil {
		panic(err)
	}
	return e
}

// log returns the spec's key log: phone, laptop and YubiKey.
func (s spec) log(t *testing.T) *Log {
	t.Helper()
	var entries []Envelope
	for _, v := range s.vectors {
		if v.kind == "key" {
			entries = append(entries, s.envelope(v))
		}
	}
	l, err := NewLog(entries)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func (s spec) vector(t *testing.T, title string) Envelope {
	t.Helper()
	for _, v := range s.vectors {
		if v.title == title {
			return s.envelope(v)
		}
	}
	t.Fatalf("no vector %q", title)
	return Envelope{}
}

func sign(t *testing.T, r Record, at time.Time, priv *ecdsa.PrivateKey) Envelope {
	t.Helper()
	e, err := New(r, at)
	if err != nil {
		t.Fatal(err)
	}
	if err := Sign(&e, priv); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestVectors(t *testing.T) {
	s := readSpec(t)
	if got := h([]byte("a"), []byte("bc")); !bytes.Equal(got, s.h) {
		t.Errorf("H(a, bc) = %x", got)
	}
	if got := sha256.Sum256(s.hInput); !bytes.Equal(got[:], s.h) {
		t.Errorf("H input hashes to %x", got)
	}
	for name, k := range s.keys {
		pub, err := s.priv(t, name).PublicKey.Bytes()
		if err != nil || !bytes.Equal(pub, k.pub) || hashHex(pub) != k.id {
			t.Errorf("key %s: public key %x, id %s", name, pub, hashHex(pub))
		}
	}

	var l Log
	for _, v := range s.vectors {
		e := s.envelope(v)
		if got := e.Challenge(); !bytes.Equal(got, v.challenge) {
			t.Errorf("%s: challenge %x", v.title, got)
		}
		var (
			r   Record
			err error
		)
		if v.kind == "key" {
			if err = l.Append(e); err == nil {
				r, _ = decode(e.Kind, e.Record)
			}
		} else {
			r, err = l.Verify(e, e.IssuedAt.Add(time.Minute))
		}
		if err != nil {
			t.Fatalf("%s: %v", v.title, err)
		}
		if b, err := Encode(r); err != nil || string(b) != v.record {
			t.Errorf("%s: Encode = %s, %v", v.title, b, err)
		}
	}

	var e0 Envelope
	if err := json.Unmarshal([]byte(s.envJSON), &e0); err != nil {
		t.Fatal(err)
	}
	if b, _ := json.MarshalIndent(e0, "", "  "); string(b)+"\n" != s.envJSON {
		t.Errorf("seq 0 envelope marshals as\n%s", b)
	}
	if _, err := NewLog([]Envelope{e0}); err != nil {
		t.Errorf("seq 0 envelope: %v", err)
	}

	// Must fail.
	g := s.vector(t, "grant, one-off")
	g.Kind = "identity"
	if got := hex.EncodeToString(g.Challenge()); got != "fa68aefd9d143f4937810198130e07ad1816c529dd278258c0c776bd4f552ef8" {
		t.Errorf("grant as identity: challenge %s", got)
	}
	phone := l.trusted[0]
	if err := verifySig(phone.Key, phone.pub, g); !errors.Is(err, ErrBadSignature) {
		t.Errorf("grant as identity: signature check gave %v", err)
	}
	if _, err := l.Verify(g, g.IssuedAt.Time); err == nil {
		t.Error("grant as identity verified")
	}
	g = s.vector(t, "grant, one-off")
	g.Record = bytes.Replace(g.Record, []byte(`,"horizon"`), []byte(`, "horizon"`), 1)
	if _, err := l.Verify(g, g.IssuedAt.Time); !errors.Is(err, errNotCanonical) {
		t.Errorf("grant with a space: %v", err)
	}
}

func TestDecodeStrict(t *testing.T) {
	ok := `{"args":{"text":"a<b & c` + "\u2028" + `"},"connector":"signal","created_at":"2026-11-03T09:30:00Z","display":[{"label":"Text","value":"\u001f"}],"expires_at":"2026-11-04T09:30:00Z","id":"Z2K7Q4M3P6X5W2N7D4R3J6YB5C","op":"send"}`
	if _, err := decode("write", []byte(ok)); err != nil {
		t.Fatalf("canonical write refused: %v", err)
	}
	for name, edit := range map[string][2]string{
		"html escape":       {`a<b`, `a\u003cb`},
		"U+2028 escape":     {"\u2028", `\u2028`},
		"uppercase escape":  {`\u001f`, `\u001F`},
		"unknown field":     {`"op":"send"`, `"op":"send","x":"y"`},
		"missing field":     {`"created_at":"2026-11-03T09:30:00Z",`, ``},
		"empty optional":    {`"op":"send"`, `"note":"","op":"send"`},
		"null":              {`"display":[{"label":"Text","value":"\u001f"}]`, `"display":null`},
		"unsorted keys":     {`{"label":"Text","value":"\u001f"}`, `{"value":"\u001f","label":"Text"}`},
		"fractional second": {`09:30:00Z","display"`, `09:30:00.0Z","display"`},
		"offset":            {`09:30:00Z","display"`, `09:30:00+00:00","display"`},
		"trailing data":     {`"op":"send"}`, `"op":"send"} `},
	} {
		bad := strings.Replace(ok, edit[0], edit[1], 1)
		if bad == ok {
			t.Fatalf("%s: edit does not apply", name)
		}
		if _, err := decode("write", []byte(bad)); err == nil {
			t.Errorf("%s: accepted %s", name, bad)
		}
	}
}

func TestLog(t *testing.T) {
	s := readSpec(t)
	at := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	phone, laptop := s.priv(t, "phone"), s.priv(t, "laptop")
	full := s.log(t)
	keys := full.Trusted()
	entries := full.Entries()
	n, head := full.Head()
	if n != 3 || head != hashHex(entries[2].Record) {
		t.Fatalf("Head() = %d, %s", n, head)
	}
	entry := func(l *Log, op string, k Key) KeyEntry {
		n, head := l.Head()
		return KeyEntry{Seq: n, Op: op, Key: k, Prev: head, At: Time{at}}
	}

	phoneOnly, err := NewLog(entries[:1])
	if err != nil {
		t.Fatal(err)
	}
	refused := map[string]struct {
		l *Log
		e Envelope
	}{
		"seq 0 signed by another key": {&Log{}, sign(t, entry(&Log{}, "add", keys[0]), at, laptop)},
		"seq 0 removing":              {&Log{}, sign(t, entry(&Log{}, "remove", keys[0]), at, phone)},
		"adding a trusted key":        {full, sign(t, entry(full, "add", keys[1]), at, phone)},
		"removing the last key":       {phoneOnly, sign(t, entry(phoneOnly, "remove", keys[0]), at, phone)},
		"removing a renamed key": {full, func() Envelope {
			k := keys[1]
			k.Name = "Old MacBook"
			return sign(t, entry(full, "remove", k), at, phone)
		}()},
		"wrong prev": {full, func() Envelope {
			e := entry(full, "remove", keys[1])
			e.Prev = hashHex(entries[0].Record)
			return sign(t, e, at, phone)
		}()},
		"wrong seq": {full, func() Envelope {
			e := entry(full, "remove", keys[1])
			e.Seq++
			return sign(t, e, at, phone)
		}()},
	}
	for name, c := range refused {
		l, err := NewLog(c.l.Entries())
		if err != nil {
			t.Fatal(err)
		}
		if err := l.Append(c.e); err == nil {
			t.Errorf("%s: appended", name)
		}
	}

	// A removed key's grants stop verifying.
	routine := s.vector(t, "grant, routine")
	l, err := NewLog(full.Entries())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Verify(routine, at); err != nil {
		t.Fatal(err)
	}
	if err := l.Append(sign(t, entry(l, "remove", keys[1]), at, laptop)); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Verify(routine, at); !errors.Is(err, ErrUntrusted) {
		t.Errorf("grant by a removed key: %v", err)
	}
	if got := l.Trusted(); len(got) != 2 || got[0].ID != keys[0].ID || got[1].ID != keys[2].ID {
		t.Errorf("Trusted() = %v", got)
	}

	held, err := NewLog(entries[:2])
	if err != nil {
		t.Fatal(err)
	}
	rewritten := []Envelope{entries[0], sign(t, entry(phoneOnly, "add", keys[2]), at, phone)}
	for name, c := range map[string][]Envelope{"shorter": entries[:1], "rewritten": rewritten} {
		if err := held.Extend(c); err == nil {
			t.Errorf("Extend took a %s log", name)
		}
	}
	if err := held.Extend(l.Entries()); err != nil {
		t.Fatal(err)
	}
	if n, h := held.Head(); n != 4 || slices.ContainsFunc(held.Trusted(), func(k Key) bool { return k.ID == keys[1].ID }) {
		t.Errorf("after Extend: Head() = %d, %s; Trusted() = %v", n, h, held.Trusted())
	}
}

func TestFIDO2(t *testing.T) {
	s := readSpec(t)
	l := s.log(t)
	yubikey := s.priv(t, "yubikey")
	signed := s.vector(t, "write, approved with the YubiKey")
	now := signed.IssuedAt.Add(time.Minute)
	if _, err := l.Verify(signed, now); err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(ad []byte){
		"no user verification": func(ad []byte) { ad[32] = 0x01 },
		"another rp ID":        func(ad []byte) { ad[0] ^= 1 },
	} {
		e := signed
		e.AuthenticatorData = slices.Clone(e.AuthenticatorData)
		edit(e.AuthenticatorData)
		d := sha256.Sum256(append(slices.Clone(e.AuthenticatorData), e.Challenge()...))
		r, sv, err := ecdsa.Sign(rand.Reader, yubikey, d[:])
		if err != nil {
			t.Fatal(err)
		}
		e.Sig = append(r.FillBytes(make([]byte, 32)), sv.FillBytes(make([]byte, 32))...)
		if _, err := l.Verify(e, now); err == nil || errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestWriteFreshness(t *testing.T) {
	s := readSpec(t)
	l := s.log(t)
	e := s.vector(t, "write")
	issued := e.IssuedAt.Time
	for _, c := range []struct {
		now time.Duration
		ok  bool
	}{
		{-time.Minute, true},
		{-time.Minute - time.Second, false},
		{5 * time.Minute, true},
		{5*time.Minute + time.Second, false},
	} {
		if _, err := l.Verify(e, issued.Add(c.now)); (err == nil) != c.ok {
			t.Errorf("now = issued_at%+v: %v", c.now, err)
		}
	}

	w := Write{
		ID: NewID(), Connector: "signal", Op: "send", Args: map[string]string{}, Display: []Field{},
		CreatedAt: Time{issued}, ExpiresAt: Time{issued.Add(2 * time.Minute)},
	}
	e = sign(t, w, issued, s.priv(t, "phone"))
	if _, err := l.Verify(e, issued.Add(2*time.Minute)); !errors.Is(err, ErrExpired) {
		t.Errorf("write at expires_at: %v", err)
	}
}

func TestGrantRules(t *testing.T) {
	from := Time{time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)}
	sched := &Schedule{Start: "07:00", Length: 1200, Zone: "Europe/Berlin"}
	for name, edit := range map[string]func(g *Grant){
		"window and schedule": func(g *Grant) { g.Schedule = sched },
		"from equals until":   func(g *Grant) { g.Until = from },
		"no window":           func(g *Grant) { g.From, g.Until = Time{}, Time{} },
		"days descending": func(g *Grant) {
			g.From, g.Until = Time{}, Time{}
			g.Schedule = &Schedule{Days: []int{2, 1}, Start: "07:00", Length: 1200, Zone: "Europe/Berlin"}
		},
		"start 7:00": func(g *Grant) {
			g.From, g.Until = Time{}, Time{}
			g.Schedule = &Schedule{Start: "7:00", Length: 1200, Zone: "Europe/Berlin"}
		},
		"local zone": func(g *Grant) {
			g.From, g.Until = Time{}, Time{}
			g.Schedule = &Schedule{Start: "07:00", Length: 1200, Zone: "Local"}
		},
	} {
		g := Grant{
			ID: NewID(), Identity: NewID(), Connector: "signal", Horizon: 3600,
			From: from, Until: Time{from.Add(time.Hour)}, CreatedAt: from,
		}
		if _, err := Encode(g); err != nil {
			t.Fatalf("valid grant: %v", err)
		}
		edit(&g)
		if b, err := Encode(g); err == nil {
			t.Errorf("%s: encoded %s", name, b)
		}
	}
}
