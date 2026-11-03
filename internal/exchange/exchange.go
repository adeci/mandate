// Package exchange is the code exchange of docs/spec/agent-api.md: a new
// party and the server run CPace keyed by a 6-digit code, confirm the key,
// and the new party seals its payload under it.
package exchange

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/gtank/ristretto255"
)

// Kinds of new party.
const (
	Agent  = "agent"
	Device = "device"
)

// ErrConfirm means the other side did not derive the same key: a wrong code
// on the server, or a wrong code or TLS key on the new party.
var ErrConfirm = errors.New("exchange: confirmation failed")

// NewCode draws the code the new party shows.
func NewCode() string {
	var b [4]byte
	for {
		rand.Read(b[:])
		// Values from 4294 * 10^6 up would favour the low codes.
		if v := binary.BigEndian.Uint32(b[:]); v < 4294*1000000 {
			return fmt.Sprintf("%06d", v%1000000)
		}
	}
}

// NewToken makes an agent's bearer token.
func NewToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// TokenSHA256 is the hash of a token as the server keeps it.
func TokenSHA256(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// Payload is what the new party seals.
type Payload struct {
	// Key is the device's key object (records.md), devices only. It is
	// carried as given; records decoding checks it.
	Key         json.RawMessage `json:"key,omitempty"`
	TokenSHA256 string          `json:"token_sha256"`
}

// encode writes p in JCS form, given that Key already is.
func (p Payload) encode(kind string) ([]byte, error) {
	if h, err := hex.DecodeString(p.TokenSHA256); err != nil || len(h) != sha256.Size ||
		hex.EncodeToString(h) != p.TokenSHA256 {
		return nil, errors.New("exchange: token_sha256 is not a lowercase hex SHA-256")
	}
	switch {
	case kind == Agent && p.Key != nil:
		return nil, errors.New("exchange: agent payload has a key")
	case kind == Device && (len(p.Key) == 0 || p.Key[0] != '{'):
		return nil, errors.New("exchange: device payload lacks a key object")
	}
	b := []byte("{")
	if p.Key != nil {
		b = append(b, `"key":`...)
		b = append(b, p.Key...)
		b = append(b, ',')
	}
	return append(b, `"token_sha256":"`+p.TokenSHA256+`"}`...), nil
}

func decodePayload(kind string, b []byte) (Payload, error) {
	var p Payload
	if err := json.Unmarshal(b, &p); err != nil {
		return Payload{}, fmt.Errorf("exchange: payload: %w", err)
	}
	// Re-encoding also catches unknown and duplicate fields.
	enc, err := p.encode(kind)
	if err != nil {
		return Payload{}, err
	}
	if !bytes.Equal(enc, b) {
		return Payload{}, errors.New("exchange: payload is not canonical")
	}
	return p, nil
}

func channel(kind string, id []byte, code string) ([]byte, error) {
	if kind != Agent && kind != Device {
		return nil, fmt.Errorf("exchange: unknown kind %q", kind)
	}
	if len(id) != 16 {
		return nil, fmt.Errorf("exchange: id is %d bytes, want 16", len(id))
	}
	if len(code) != 6 || strings.Trim(code, "0123456789") != "" {
		return nil, errors.New("exchange: code is not 6 digits")
	}
	return []byte("mandate join " + kind), nil
}

func mac(isk []byte, label string, extra []byte) []byte {
	h := hmac.New(sha256.New, isk)
	h.Write([]byte(label))
	h.Write(extra)
	return h.Sum(nil)
}

func partyConfirm(isk []byte) []byte { return mac(isk, "mandate exchange party", nil) }

func serverConfirm(isk, spkiSHA256 []byte) []byte {
	return mac(isk, "mandate exchange server", spkiSHA256)
}

// sealer uses a fixed zero nonce: each ISK, and so each key, seals once.
func sealer(isk []byte) cipher.AEAD {
	block, _ := aes.NewCipher(mac(isk, "mandate exchange seal", nil))
	aead, _ := cipher.NewGCM(block)
	return aead
}

func seal(isk, id, payload []byte) []byte {
	return sealer(isk).Seal(nil, make([]byte, 12), payload, id)
}

func open(isk, id, sealed []byte) ([]byte, error) {
	b, err := sealer(isk).Open(nil, make([]byte, 12), sealed, id)
	if err != nil {
		return nil, fmt.Errorf("exchange: open payload: %w", err)
	}
	return b, nil
}

// Server is one attempt of the server, the CPace initiator, at the code the
// owner typed.
type Server struct {
	kind string
	id   []byte
	msg  []byte
	y    atomic.Pointer[ristretto255.Scalar]
}

// Start begins an attempt on the exchange id (16 bytes).
func Start(kind string, id []byte, code string) (*Server, error) {
	ci, err := channel(kind, id, code)
	if err != nil {
		return nil, err
	}
	y := sampleScalar()
	s := &Server{kind: kind, id: id, msg: share(y, generator([]byte(code), ci, id))}
	s.y.Store(y)
	return s, nil
}

// Msg is Ya, for the new party.
func (s *Server) Msg() []byte { return s.msg }

// Finish takes the new party's answer: its message, party_confirm and the
// sealed payload. It returns the payload and server_confirm. The attempt is
// spent whatever the outcome; ErrConfirm counts as a wrong code.
func (s *Server) Finish(msg, confirm, sealed, spkiSHA256 []byte) (Payload, []byte, error) {
	y := s.y.Swap(nil)
	if y == nil {
		return Payload{}, nil, errors.New("exchange: attempt already finished")
	}
	k, err := secret(y, msg)
	if err != nil {
		return Payload{}, nil, fmt.Errorf("%w: %w", ErrConfirm, err)
	}
	isk := deriveISK(s.id, k, s.msg, nil, msg, nil)
	if !hmac.Equal(confirm, partyConfirm(isk)) {
		return Payload{}, nil, ErrConfirm
	}
	b, err := open(isk, s.id, sealed)
	if err != nil {
		return Payload{}, nil, err
	}
	p, err := decodePayload(s.kind, b)
	if err != nil {
		return Payload{}, nil, err
	}
	return p, serverConfirm(isk, spkiSHA256), nil
}

// Reply is the new party's answer to the server's message.
type Reply struct {
	Msg     []byte // Yb
	Confirm []byte // party_confirm
	Sealed  []byte
	isk     []byte
}

// Respond runs CPace as responder on the server's message and seals p.
func Respond(kind string, id []byte, code string, msg []byte, p Payload) (*Reply, error) {
	ci, err := channel(kind, id, code)
	if err != nil {
		return nil, err
	}
	payload, err := p.encode(kind)
	if err != nil {
		return nil, err
	}
	y := sampleScalar()
	k, err := secret(y, msg)
	if err != nil {
		return nil, fmt.Errorf("exchange: server message: %w", err)
	}
	yb := share(y, generator([]byte(code), ci, id))
	isk := deriveISK(id, k, msg, nil, yb, nil)
	return &Reply{Msg: yb, Confirm: partyConfirm(isk), Sealed: seal(isk, id, payload), isk: isk}, nil
}

// Check verifies server_confirm against the SHA-256 of the DER
// SubjectPublicKeyInfo of this connection's TLS key.
func (r *Reply) Check(confirm, spkiSHA256 []byte) error {
	if !hmac.Equal(confirm, serverConfirm(r.isk, spkiSHA256)) {
		return ErrConfirm
	}
	return nil
}
