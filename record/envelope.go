package record

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"time"
)

// Envelope carries record bytes and a device's signature over them. Before
// signing, Key and Sig are empty.
type Envelope struct {
	Kind              string `json:"kind"`
	Record            Bytes  `json:"record"`
	Nonce             Bytes  `json:"nonce"`
	IssuedAt          Time   `json:"issued_at"`
	Key               string `json:"key,omitempty"`
	AuthenticatorData Bytes  `json:"authenticator_data,omitempty"`
	Sig               Bytes  `json:"sig,omitempty"`
}

var (
	ErrUntrusted    = errors.New("key is not trusted")
	ErrRetired      = errors.New("key is retired and signs nothing new")
	ErrBadSignature = errors.New("signature does not verify")
	ErrExpired      = errors.New("expired")
)

// New returns r as an unsigned envelope with a fresh nonce.
func New(r Record, now time.Time) (Envelope, error) {
	b, err := Encode(r)
	if err != nil {
		return Envelope{}, err
	}
	nonce := make([]byte, 16)
	rand.Read(nonce)
	return Envelope{
		Kind:     r.Kind(),
		Record:   b,
		Nonce:    nonce,
		IssuedAt: Time{now.UTC().Truncate(time.Second)},
	}, nil
}

// h hashes each part as its 8-byte big-endian length, then the part.
func h(parts ...[]byte) []byte {
	d := sha256.New()
	for _, p := range parts {
		d.Write(binary.BigEndian.AppendUint64(nil, uint64(len(p))))
		d.Write(p)
	}
	return d.Sum(nil)
}

// Challenge is what the device signs: for a fido2 key, the clientDataHash.
func (e Envelope) Challenge() []byte {
	return h([]byte("mandate-record-v1"), []byte(e.Kind), e.Record, e.Nonce, []byte(e.IssuedAt.format()))
}

// P256Key returns pub as a p256 device key.
func P256Key(pub *ecdsa.PublicKey, name string) (Key, error) {
	if pub.Curve != elliptic.P256() {
		return Key{}, errors.New("not a P-256 key")
	}
	b, err := pub.Bytes()
	if err != nil {
		return Key{}, fmt.Errorf("encoding public key: %w", err)
	}
	return Key{Type: "p256", ID: hashHex(b), Name: name, PublicKey: b}, nil
}

// Sign signs e with priv as a p256 key.
func Sign(e *Envelope, priv *ecdsa.PrivateKey) error {
	k, err := P256Key(&priv.PublicKey, "")
	if err != nil {
		return err
	}
	d := sha256.Sum256(e.Challenge())
	r, s, err := ecdsa.Sign(rand.Reader, priv, d[:])
	if err != nil {
		return fmt.Errorf("signing %s record: %w", e.Kind, err)
	}
	e.Key = k.ID
	e.AuthenticatorData = nil
	e.Sig = append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	return nil
}

var rpIDHash = sha256.Sum256([]byte("mandate"))

func verifySig(k Key, pub *ecdsa.PublicKey, e Envelope) error {
	msg := e.Challenge()
	ad := e.AuthenticatorData
	if k.Type == "fido2" {
		const up, uv = 0x01, 0x04
		// rp ID hash, flags, sign count
		if len(ad) < 37 || !bytes.Equal(ad[:32], rpIDHash[:]) {
			return errors.New("authenticator_data is not for rp ID mandate")
		}
		if ad[32]&(up|uv) != up|uv {
			return errors.New("authenticator_data lacks user presence or verification")
		}
		msg = append(slices.Clone(ad), msg...)
	} else if len(ad) > 0 {
		return fmt.Errorf("authenticator_data with %s key", k.Type)
	}
	if len(e.Sig) != 64 {
		return ErrBadSignature
	}
	d := sha256.Sum256(msg)
	r, s := new(big.Int).SetBytes(e.Sig[:32]), new(big.Int).SetBytes(e.Sig[32:])
	if !ecdsa.Verify(pub, d[:], r, s) {
		return ErrBadSignature
	}
	return nil
}
