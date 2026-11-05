// Command testdevice plays the owner's app in the NixOS VM test: it pairs
// from a setup link or joins as another device with the code an exchange
// showed, calls the owner API with its device token, and signs challenges
// with its P-256 key.
//
//	testdevice [--state FILE] [--fido2] pair LINK [NAME]
//	testdevice [--state FILE] [--fido2] join URL SPKI CODE [NAME]
//	testdevice [--state FILE] call METHOD PATH [BODY]
//	testdevice [--state FILE] sign PATH [BODY]
//	testdevice [--state FILE] rotate [NAME]
//
// call prints {"status", "body"}; sign posts BODY to the challenge route
// PATH, signs the envelope it answers and posts it to /owner/v1/signed,
// printing the answer with the record and the envelope it signed. pair
// keeps the key of an earlier pairing, as an app would after mandate reset.
// --fido2 makes the new device's key a fido2 one, signed in software with
// the flags a security key sets (UP and UV).
// rotate swaps the device's key as the app's "How you approve" does: the
// current key adds a new one, which then retires it.
package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"time"

	"github.com/adeci/mandate/internal/exchange"
	"github.com/adeci/mandate/record"
)

var fido2 bool

// device is what the app keeps.
type device struct {
	URL   string `json:"url"`
	SPKI  string `json:"spki"`
	Token string `json:"token"`
	Key   []byte `json:"key"` // PKCS #8
	// CredentialID is set for a fido2 key.
	CredentialID []byte `json:"credential_id,omitempty"`
}

func main() {
	state := flag.String("state", "device.json", "the device's state file")
	flag.BoolVar(&fido2, "fido2", false, "make a fido2 key")
	flag.Parse()
	if err := run(context.Background(), *state, flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, "testdevice:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, state string, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: testdevice [--state FILE] pair|join|call|sign|rotate ARGS")
	}
	name := func(i int) string {
		if len(args) > i {
			return args[i]
		}
		return "test device"
	}
	switch {
	case args[0] == "pair" && len(args) <= 3 && len(args) >= 2:
		return pair(ctx, state, args[1], name(2))
	case args[0] == "join" && len(args) <= 5 && len(args) >= 4:
		return join(ctx, state, args[1], args[2], args[3], name(4))
	case args[0] == "call" && (len(args) == 3 || len(args) == 4):
		d, err := load(state)
		if err != nil {
			return err
		}
		status, body, err := d.call(ctx, args[1], args[2], arg(args, 3))
		if err != nil {
			return err
		}
		return emit(map[string]any{"status": status, "body": body})
	case args[0] == "sign" && (len(args) == 2 || len(args) == 3):
		d, err := load(state)
		if err != nil {
			return err
		}
		return d.sign(ctx, args[1], arg(args, 2))
	case args[0] == "rotate" && len(args) <= 2:
		d, err := load(state)
		if err != nil {
			return err
		}
		return d.rotate(ctx, state, name(1))
	}
	return fmt.Errorf("unknown command %q", args)
}

func arg(args []string, i int) string {
	if len(args) > i {
		return args[i]
	}
	return ""
}

func emit(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Printf("%s\n", b)
	return err
}

func load(path string) (*device, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var d device
	return &d, json.Unmarshal(b, &d)
}

func (d *device) save(path string) error {
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

func (d *device) key() (*ecdsa.PrivateKey, error) {
	k, err := x509.ParsePKCS8PrivateKey(d.Key)
	if err != nil {
		return nil, err
	}
	priv, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("not an ECDSA key")
	}
	return priv, nil
}

// newDevice makes the token, and the key unless der holds one, of a device
// that will talk to url.
func newDevice(u, spki, name string, der []byte) (*device, *ecdsa.PrivateKey, record.Key, error) {
	d := &device{URL: u, SPKI: spki, Token: exchange.NewToken(), Key: der}
	if der == nil {
		priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, nil, record.Key{}, err
		}
		if d.Key, err = x509.MarshalPKCS8PrivateKey(priv); err != nil {
			return nil, nil, record.Key{}, err
		}
	}
	if fido2 {
		d.CredentialID = make([]byte, 16)
		rand.Read(d.CredentialID)
	}
	priv, err := d.key()
	if err != nil {
		return nil, nil, record.Key{}, err
	}
	k, err := d.recordKey(priv, name)
	if err != nil {
		return nil, nil, record.Key{}, err
	}
	return d, priv, k, nil
}

// recordKey is priv's public key as the key log names it.
func (d *device) recordKey(priv *ecdsa.PrivateKey, name string) (record.Key, error) {
	k, err := record.P256Key(&priv.PublicKey, name)
	if err == nil && d.CredentialID != nil {
		k.Type, k.CredentialID = "fido2", d.CredentialID
	}
	return k, err
}

var rpIDHash = sha256.Sum256([]byte("mandate"))

// signEnv signs e with priv, as a security key would for a fido2 device.
func (d *device) signEnv(e *record.Envelope, priv *ecdsa.PrivateKey) error {
	if d.CredentialID == nil {
		return record.Sign(e, priv)
	}
	k, err := d.recordKey(priv, "")
	if err != nil {
		return err
	}
	// rp ID hash, flags UP|UV, sign count 1
	ad := append(append(rpIDHash[:0:0], rpIDHash[:]...), 0x05, 0, 0, 0, 1)
	digest := sha256.Sum256(append(slices.Clone(ad), e.Challenge()...))
	r, s, err := ecdsa.Sign(rand.Reader, priv, digest[:])
	if err != nil {
		return err
	}
	e.Key, e.AuthenticatorData = k.ID, ad
	e.Sig = append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	return nil
}

// client pins the server key the link or the command line named.
func (d *device) client() *http.Client {
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			sum := sha256.Sum256(cs.PeerCertificates[0].RawSubjectPublicKeyInfo)
			if hex.EncodeToString(sum[:]) != d.SPKI {
				return errors.New("the server key is not the one paired with")
			}
			return nil
		},
	}}}
}

// call returns the status and the JSON body, or the body as text if it is
// not JSON.
func (d *device) call(ctx context.Context, method, path, body string) (int, any, error) {
	var in io.Reader
	if body != "" {
		in = bytes.NewBufferString(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, d.URL+path, in)
	if err != nil {
		return 0, nil, err
	}
	if d.Token != "" {
		req.Header.Set("Authorization", "Bearer "+d.Token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.client().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	var out any
	if json.Unmarshal(b, &out) != nil {
		out = string(b)
	}
	return resp.StatusCode, out, nil
}

// callJSON calls with in as the body, if not nil, and wants a 2xx answer
// decoding into out.
func (d *device) callJSON(ctx context.Context, method, path string, in, out any) error {
	var body string
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = string(b)
	}
	status, answer, err := d.call(ctx, method, path, body)
	if err != nil {
		return err
	}
	if status/100 != 2 {
		return fmt.Errorf("%s %s: %d %v", method, path, status, answer)
	}
	b, err := json.Marshal(answer)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func pair(ctx context.Context, state, link, name string) error {
	u, err := url.Parse(link)
	if err != nil || u.Scheme != "mandate" || u.Host != "pair" {
		return fmt.Errorf("not a pairing link: %q", link)
	}
	q := u.Query()
	// Pairing again after mandate reset keeps the device's key.
	var der []byte
	if old, err := load(state); err == nil {
		der = old.Key
	}
	d, priv, k, err := newDevice(q.Get("url"), q.Get("spki"), name, der)
	if err != nil {
		return err
	}
	env, err := record.New(record.KeyEntry{Seq: 0, Op: "add", Key: k, At: record.Time{Time: time.Now()}}, time.Now())
	if err != nil {
		return err
	}
	if err := d.signEnv(&env, priv); err != nil {
		return err
	}
	token := d.Token
	d.Token = ""
	var out map[string]string
	if err := d.callJSON(ctx, http.MethodPost, "/owner/v1/pair", map[string]any{
		"secret":       q.Get("secret"),
		"token_sha256": exchange.TokenSHA256(token),
		"entry":        env,
	}, &out); err != nil {
		return err
	}
	d.Token = token
	if err := d.save(state); err != nil {
		return err
	}
	return emit(out)
}

// join is the new device's side of the code exchange: it tries the code,
// then waits for the key add.
func join(ctx context.Context, state, u, spki, code, name string) error {
	d, _, k, err := newDevice(u, spki, name, nil)
	if err != nil {
		return err
	}
	pinned, err := hex.DecodeString(spki)
	if err != nil {
		return err
	}
	token := d.Token
	d.Token = ""
	payload := exchange.Payload{Key: &k, TokenSHA256: exchange.TokenSHA256(token)}

	var started struct{ ID, Msg string }
	if err := d.callJSON(ctx, http.MethodPost, "/owner/v1/join", map[string]string{"kind": exchange.Device, "name": name}, &started); err != nil {
		return err
	}
	id, err1 := hex.DecodeString(started.ID)
	msg, err2 := hex.DecodeString(started.Msg)
	if err := errors.Join(err1, err2); err != nil {
		return err
	}
	r, err := exchange.Respond(exchange.Device, id, code, msg, payload)
	if err != nil {
		return err
	}
	var answer struct{ Confirm string }
	if err := d.callJSON(ctx, http.MethodPost, "/owner/v1/join/"+started.ID, map[string]string{
		"msg": hex.EncodeToString(r.Msg), "confirm": hex.EncodeToString(r.Confirm), "sealed": hex.EncodeToString(r.Sealed),
	}, &answer); err != nil {
		return err
	}
	confirm, err := hex.DecodeString(answer.Confirm)
	if err != nil {
		return err
	}
	if err := r.Check(confirm, pinned); err != nil {
		return err
	}
	for {
		var poll struct{ Status string }
		if err := d.callJSON(ctx, http.MethodGet, "/owner/v1/join/"+started.ID+"?wait=30", nil, &poll); err != nil {
			return err
		}
		switch poll.Status {
		case "approved":
			d.Token = token
			return d.save(state)
		case "ended":
			return errors.New("the exchange ended")
		}
	}
}

func (d *device) sign(ctx context.Context, path, body string) error {
	if body == "" {
		body = "{}"
	}
	status, out, err := d.call(ctx, http.MethodPost, path, body)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return emit(map[string]any{"status": status, "body": out})
	}
	b, err := json.Marshal(out)
	if err != nil {
		return err
	}
	var env record.Envelope
	if err := json.Unmarshal(b, &env); err != nil {
		return err
	}
	priv, err := d.key()
	if err != nil {
		return err
	}
	if err := d.signEnv(&env, priv); err != nil {
		return err
	}
	signed, err := json.Marshal(env)
	if err != nil {
		return err
	}
	status, out, err = d.call(ctx, http.MethodPost, "/owner/v1/signed", string(signed))
	if err != nil {
		return err
	}
	return emit(map[string]any{
		"status": status, "body": out,
		"record": json.RawMessage(env.Record), "envelope": json.RawMessage(signed),
	})
}

// signed signs env with priv and posts it to /owner/v1/signed.
func (d *device) signed(ctx context.Context, env record.Envelope, priv *ecdsa.PrivateKey) error {
	if err := d.signEnv(&env, priv); err != nil {
		return err
	}
	var out map[string]string
	return d.callJSON(ctx, http.MethodPost, "/owner/v1/signed", env, &out)
}

func (d *device) rotate(ctx context.Context, state, name string) error {
	oldPriv, err := d.key()
	if err != nil {
		return err
	}
	oldKey, err := d.recordKey(oldPriv, name)
	if err != nil {
		return err
	}
	fido2 = d.CredentialID != nil // the same kind of key
	next, newPriv, newKey, err := newDevice(d.URL, d.SPKI, name, nil)
	if err != nil {
		return err
	}
	var env record.Envelope
	if err := d.callJSON(ctx, http.MethodPost, "/owner/v1/keys/add/challenge", map[string]any{"key": newKey}, &env); err != nil {
		return err
	}
	if err := d.signed(ctx, env, oldPriv); err != nil {
		return err
	}
	// The token belongs to the new key now.
	d.Key, d.CredentialID = next.Key, next.CredentialID
	if err := d.save(state); err != nil {
		return err
	}
	if err := d.callJSON(ctx, http.MethodPost, "/owner/v1/keys/"+oldKey.ID+"/retire/challenge", struct{}{}, &env); err != nil {
		return err
	}
	if err := d.signed(ctx, env, newPriv); err != nil {
		return err
	}
	return emit(map[string]string{"old": oldKey.ID, "new": newKey.ID})
}
