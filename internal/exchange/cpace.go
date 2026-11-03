package exchange

import (
	"crypto/rand"
	"crypto/sha512"
	"encoding/binary"
	"errors"

	"github.com/gtank/ristretto255"
)

// CPace as in draft-irtf-cfrg-cpace-21, suite CPACE-RISTR255-SHA512, with
// initiator and responder roles.

const dsi = "CPaceRistretto255"

var errAbort = errors.New("cpace: peer message is not a valid point")

// lvCat prepends each part with its LEB128 length.
func lvCat(parts ...[]byte) []byte {
	var b []byte
	for _, p := range parts {
		b = binary.AppendUvarint(b, uint64(len(p)))
		b = append(b, p...)
	}
	return b
}

func generator(prs, ci, sid []byte) *ristretto255.Element {
	// The padding fills the first hash block with DSI and PRS alone, so the
	// work done on the password does not depend on its length.
	zpad := max(0, sha512.BlockSize-1-len(lvCat(prs))-len(lvCat([]byte(dsi))))
	h := sha512.Sum512(lvCat([]byte(dsi), prs, make([]byte, zpad), ci, sid))
	g, _ := ristretto255.NewIdentityElement().SetUniformBytes(h[:])
	return g
}

func sampleScalar() *ristretto255.Scalar {
	b := make([]byte, 64)
	rand.Read(b)
	y, _ := ristretto255.NewScalar().SetUniformBytes(b)
	return y
}

func share(y *ristretto255.Scalar, g *ristretto255.Element) []byte {
	return ristretto255.NewIdentityElement().ScalarMult(y, g).Bytes()
}

// secret is scalar_mult_vfy, aborting where the draft would return G.I.
func secret(y *ristretto255.Scalar, peer []byte) ([]byte, error) {
	p, err := ristretto255.NewIdentityElement().SetCanonicalBytes(peer)
	if err != nil {
		return nil, errAbort
	}
	k := ristretto255.NewIdentityElement().ScalarMult(y, p)
	if k.Equal(ristretto255.NewIdentityElement()) == 1 {
		return nil, errAbort
	}
	return k.Bytes(), nil
}

// deriveISK hashes lv_cat(DSI_ISK, sid, K) || transcript_ir(Ya, ADa, Yb, ADb),
// which is one lv_cat over all seven parts.
func deriveISK(sid, k, ya, ada, yb, adb []byte) []byte {
	h := sha512.Sum512(lvCat([]byte(dsi+"_ISK"), sid, k, ya, ada, yb, adb))
	return h[:]
}
