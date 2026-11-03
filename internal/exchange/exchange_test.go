package exchange

import (
	"bytes"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/gtank/ristretto255"
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.Join(strings.Fields(s), ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func scalar(t *testing.T, s string) *ristretto255.Scalar {
	t.Helper()
	y, err := ristretto255.NewScalar().SetCanonicalBytes(unhex(t, s))
	if err != nil {
		t.Fatal(err)
	}
	return y
}

// draft-irtf-cfrg-cpace-21, appendix B.3.
func TestCPaceVectors(t *testing.T) {
	sid := unhex(t, "7e4b4791d6a8ef019b936c79fb7f2c57")
	ci := unhex(t, "0b415f696e69746961746f720b425f726573706f6e646572")
	g := generator([]byte("Password"), ci, sid)
	if got, want := g.Bytes(), unhex(t, "222b6b195fe84b1652badb6f6a3ae3d24341e7306967f0b8115b40d5698c7e56"); !bytes.Equal(got, want) {
		t.Fatalf("g = %x, want %x", got, want)
	}

	ya := scalar(t, "da3d23700a9e5699258aef94dc060dfda5ebb61f02a5ea77fad53f4ff0976d08")
	yb := scalar(t, "d2316b454718c35362d83d69df6320f38578ed5984651435e2949762d900b80d")
	wantYa := unhex(t, "d6bac480f2c386c394efc7c47adb9925dcd2630b64f240c50f8d0eec482b9157")
	wantYb := unhex(t, "3ea7e0b19560d7c0b0f5734f63b955286dfa8232b5ebe63324e2d9e7433f7258")
	if got := share(ya, g); !bytes.Equal(got, wantYa) {
		t.Fatalf("Ya = %x", got)
	}
	if got := share(yb, g); !bytes.Equal(got, wantYb) {
		t.Fatalf("Yb = %x", got)
	}

	wantK := unhex(t, "80b69a8a76457ab6a4d7f887a4bf6b55a2f80ac19c333f917a05fc9887c8b40f")
	for _, c := range []struct {
		y    *ristretto255.Scalar
		peer []byte
	}{{ya, wantYb}, {yb, wantYa}} {
		k, err := secret(c.y, c.peer)
		if err != nil || !bytes.Equal(k, wantK) {
			t.Fatalf("K = %x, %v", k, err)
		}
	}

	isk := deriveISK(sid, wantK, wantYa, []byte("ADa"), wantYb, []byte("ADb"))
	if want := unhex(t, `b69effbf61b51d56401c0f65601abe428de8206feaaf0e32198896dc
		ae7b35cd2b38950a39dfd5d4a79164614c2984f7daa460b588c1e80c3fa2068af7900447`); !bytes.Equal(isk, want) {
		t.Fatalf("ISK = %x", isk)
	}

	s := scalar(t, "7cd0e075fa7955ba52c02759a6c90dbbfc10e6d40aea8d283e407d88cf538a05")
	k, err := secret(s, unhex(t, "2c3c6b8c4f3800e7aef6864025b4ed79bd599117e427c41bd47d93d654b4a51c"))
	if want := unhex(t, "7c13645fe790a468f62c39beb7388e541d8405d1ade69d1778c5fe3e7f6b600e"); err != nil || !bytes.Equal(k, want) {
		t.Fatalf("scalar_mult_vfy = %x, %v", k, err)
	}
	for _, bad := range []string{
		"2b3c6b8c4f3800e7aef6864025b4ed79bd599117e427c41bd47d93d654b4a51c",
		"0000000000000000000000000000000000000000000000000000000000000000",
	} {
		if _, err := secret(s, unhex(t, bad)); err == nil {
			t.Errorf("scalar_mult_vfy accepted %s", bad)
		}
	}
}

// The CPace vector in agent-api.md, Derivations.
func TestCPaceInputsVector(t *testing.T) {
	id := unhex(t, "000102030405060708090a0b0c0d0e0f")
	ci, err := channel(Agent, id, "123456")
	if err != nil {
		t.Fatal(err)
	}
	g := generator([]byte("123456"), ci, id)
	if got, want := g.Bytes(), unhex(t, "fa7c52e5f362bd1c187338e289b2ef30b82498a7ac8a1cda8db96fdb9a7c4d33"); !bytes.Equal(got, want) {
		t.Fatalf("g = %x, want %x", got, want)
	}
	ya := share(scalar(t, "da3d23700a9e5699258aef94dc060dfda5ebb61f02a5ea77fad53f4ff0976d08"), g)
	yb := scalar(t, "d2316b454718c35362d83d69df6320f38578ed5984651435e2949762d900b80d")
	if want := unhex(t, "267e392ad5b4d7b690c144d778385d2a9c1f7b1dea9377303d070f4585009d4a"); !bytes.Equal(ya, want) {
		t.Fatalf("Ya = %x", ya)
	}
	k, err := secret(yb, ya)
	if want := unhex(t, "a2ba6d19aa4bb696a266bb772c894b825ac87a5082b06c7c4e97b808abbc9601"); err != nil || !bytes.Equal(k, want) {
		t.Fatalf("K = %x, %v", k, err)
	}
	isk := deriveISK(id, k, ya, nil, share(yb, g), nil)
	if want := unhex(t, `9de94546d597ea438dfb0589a520bc03223a1bb92856b89bb1a0f9c948ddad59
		480f1132191e282e00f70b992ff27541ed1440cc826d857517e5b4b438840596`); !bytes.Equal(isk, want) {
		t.Fatalf("ISK = %x", isk)
	}
}

// The derivation vector in agent-api.md, Derivations.
func TestDerivationVector(t *testing.T) {
	isk := unhex(t, `000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f
		202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f`)
	id := unhex(t, "000102030405060708090a0b0c0d0e0f")
	spki := unhex(t, "f910152c80be10ee2280a04cb011ce9131b993408adb19cf47f1495c331bcdbc")

	payload, err := Payload{TokenSHA256: TokenSHA256(strings.Repeat("1", 64))}.encode(Agent)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"token_sha256":"3138bb9bc78df27c473ecfd1410f7bd45ebac1f59cf3ff9cfe4db77aab7aedd3"}`; string(payload) != want {
		t.Fatalf("payload = %s", payload)
	}
	for _, c := range []struct {
		name      string
		got, want []byte
	}{
		{"party_confirm", partyConfirm(isk), unhex(t, "fb60f9fc9ad3d93847f5d4a12457d11afa66dc6d37cd258035a0c8660d847325")},
		{"server_confirm", serverConfirm(isk, spki), unhex(t, "732f5b9afa026f4715bddcae71c864e6bb6473d00c70b5bc4f80b676df94f00c")},
		{"seal_key", mac(isk, "mandate exchange seal", nil), unhex(t, "2d66336f1cff1e116b70ac5df2ae840353d9650752ff1f583ae6de5e96e7c905")},
		{"sealed", seal(isk, id, payload), unhex(t, `3b3b1c233fad60e0e148b7ac79f6a6845841af004b042d709ed21d95a840babe
			2767ef6877114b374635f0bfe7bfc67102d13a9d47515a4b15f6174911ccc39e
			7e88321efea491d5516290aa3f82f4df7e0707d788af33678c8bbf295e7977cf
			77b217`)},
	} {
		if !bytes.Equal(c.got, c.want) {
			t.Errorf("%s = %x, want %x", c.name, c.got, c.want)
		}
	}
}

func TestExchange(t *testing.T) {
	id := unhex(t, "000102030405060708090a0b0c0d0e0f")
	spki := unhex(t, "f910152c80be10ee2280a04cb011ce9131b993408adb19cf47f1495c331bcdbc")
	key := `{"id":"b9035233be144c52017fbb52541aab66faa7f6c93eef6e76968ba564c477d31e","name":"Pixel 9","public_key":"BN4CBo-cv2UYsv7S22CmwboOxAVnQh_vAIceeHGJ_wTKxefX7CfLCPv9U9pGtDjRzs6ZPn39ROrW67Fcq2ilkcE","type":"p256"}`
	payloads := map[string]Payload{
		Agent:  {TokenSHA256: TokenSHA256(NewToken())},
		Device: {Key: []byte(key), TokenSHA256: TokenSHA256(NewToken())},
	}
	for kind, p := range payloads {
		t.Run(kind, func(t *testing.T) {
			code := NewCode()
			s, err := Start(kind, id, code)
			if err != nil {
				t.Fatal(err)
			}
			r, err := Respond(kind, id, code, s.Msg(), p)
			if err != nil {
				t.Fatal(err)
			}
			got, confirm, err := s.Finish(r.Msg, r.Confirm, r.Sealed, spki)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, p) {
				t.Errorf("payload = %+v, want %+v", got, p)
			}
			if err := r.Check(confirm, spki); err != nil {
				t.Errorf("right spki: %v", err)
			}
			wrong := bytes.Clone(spki)
			wrong[0] ^= 1
			if err := r.Check(confirm, wrong); !errors.Is(err, ErrConfirm) {
				t.Errorf("wrong spki: %v, want ErrConfirm", err)
			}
		})
	}

	s, err := Start(Agent, id, "123456")
	if err != nil {
		t.Fatal(err)
	}
	r, err := Respond(Agent, id, "123457", s.Msg(), payloads[Agent])
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Finish(r.Msg, r.Confirm, r.Sealed, spki); !errors.Is(err, ErrConfirm) {
		t.Errorf("wrong code: %v, want ErrConfirm", err)
	}
}
