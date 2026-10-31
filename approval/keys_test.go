package approval_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/approval/approvaltest"
)

func openKeys(t *testing.T) (*approval.Keys, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "keys.json")
	k, err := approval.OpenKeys(path, rpID, origin)
	if err != nil {
		t.Fatal(err)
	}
	return k, path
}

// change encodes op on key as the next entry of k.
func change(t *testing.T, k *approval.Keys, op string, key approval.Key) []byte {
	t.Helper()
	n, head := k.Head()
	b, err := approval.EncodeChange(approval.KeyChange{Seq: n, Op: op, Key: key, Prev: head, At: now})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustApply(t *testing.T, k *approval.Keys, sc approval.SignedChange) {
	t.Helper()
	if err := k.Apply(sc); err != nil {
		t.Fatal(err)
	}
}

// trusting returns a log that trusts first (unsigned) and then each of more,
// signed by first.
func trusting(t *testing.T, first *approvaltest.Authenticator, more ...*approvaltest.Authenticator) (*approval.Keys, string) {
	t.Helper()
	k, path := openKeys(t)
	mustApply(t, k, approval.SignedChange{Change: change(t, k, approval.OpAdd, first.Key("first"))})
	for _, a := range more {
		mustApply(t, k, first.Sign(change(t, k, approval.OpAdd, a.Key("more"))))
	}
	return k, path
}

func TestKeyChangeRules(t *testing.T) {
	t.Parallel()
	a := approvaltest.New(t, rpID, origin)
	b := approvaltest.New(t, rpID, origin)
	c := approvaltest.New(t, rpID, origin)

	t.Run("later add without a signature", func(t *testing.T) {
		k, _ := trusting(t, a)
		if err := k.Apply(approval.SignedChange{Change: change(t, k, approval.OpAdd, b.Key("b"))}); !errors.Is(err, approval.ErrUnsigned) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("signer not yet trusted", func(t *testing.T) {
		k, _ := trusting(t, a)
		if err := k.Apply(b.Sign(change(t, k, approval.OpAdd, b.Key("b")))); !errors.Is(err, approval.ErrUnknown) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("signer is someone else", func(t *testing.T) {
		k, _ := trusting(t, a)
		sc := b.Sign(change(t, k, approval.OpAdd, b.Key("b")))
		sc.Signer = a.Key("").CredentialID
		if err := k.Apply(sc); err == nil {
			t.Fatal("assertion by b accepted as signed by a")
		}
	})
	t.Run("removed key can't sign", func(t *testing.T) {
		k, _ := trusting(t, a, b)
		mustApply(t, k, a.Sign(change(t, k, approval.OpRemove, b.Key("more"))))
		if err := k.Apply(b.Sign(change(t, k, approval.OpAdd, c.Key("c")))); !errors.Is(err, approval.ErrUnknown) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("last key stays", func(t *testing.T) {
		k, _ := trusting(t, a)
		if err := k.Apply(a.Sign(change(t, k, approval.OpRemove, a.Key("first")))); !errors.Is(err, approval.ErrLastKey) {
			t.Fatalf("err = %v", err)
		}
		if _, err := k.Next(approval.OpRemove, a.Key("first"), now); !errors.Is(err, approval.ErrLastKey) {
			t.Fatalf("Next: %v", err)
		}
	})
	t.Run("out of order", func(t *testing.T) {
		k, _ := trusting(t, a)
		stale := change(t, k, approval.OpAdd, b.Key("b"))
		mustApply(t, k, a.Sign(change(t, k, approval.OpAdd, c.Key("c"))))
		if err := k.Apply(a.Sign(stale)); !errors.Is(err, approval.ErrOutOfOrder) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("add twice or remove a stranger", func(t *testing.T) {
		k, _ := trusting(t, a, b)
		if err := k.Apply(a.Sign(change(t, k, approval.OpAdd, b.Key("more")))); err == nil {
			t.Fatal("re-added a trusted key")
		}
		if err := k.Apply(a.Sign(change(t, k, approval.OpRemove, c.Key("c")))); err == nil {
			t.Fatal("removed an untrusted key")
		}
	})
	t.Run("approval signature isn't a key change signature", func(t *testing.T) {
		k, _ := trusting(t, a)
		ch := change(t, k, approval.OpAdd, b.Key("b"))
		sc := a.Sign(ch)
		sc.Assertion = a.Approve(ch, now).Assertion
		if err := k.Apply(sc); !errors.Is(err, approval.ErrSignature) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("key change signature isn't an approval", func(t *testing.T) {
		k, _ := trusting(t, a)
		rec := record(t)
		ap := a.Approve(rec, now)
		ap.Assertion = a.Sign(rec).Assertion
		if _, err := (&approval.Verifier{Keys: k, Now: func() time.Time { return now }}).Verify(ap); !errors.Is(err, approval.ErrSignature) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestKeyLogLoadReverifies(t *testing.T) {
	t.Parallel()
	a := approvaltest.New(t, rpID, origin)
	b := approvaltest.New(t, rpID, origin)
	k, path := trusting(t, a, b)

	reopened, err := approval.OpenKeys(path, rpID, origin)
	if err != nil {
		t.Fatal(err)
	}
	if n, h := reopened.Head(); n != 2 || len(reopened.Trusted()) != 2 {
		t.Fatalf("reopened head %d %s, trusted %v", n, h, reopened.Trusted())
	}
	if _, err := approval.OpenKeys(path, rpID, "https://elsewhere.example"); !errors.Is(err, approval.ErrSignature) {
		t.Fatalf("log loaded for another origin: %v", err)
	}

	log := k.Log()
	tampered := append([]approval.SignedChange{}, log...)
	ch, err := approval.DecodeChange(log[1].Change)
	if err != nil {
		t.Fatal(err)
	}
	ch.Key.Name = "renamed"
	tampered[1].Change, _ = approval.EncodeChange(ch)
	for name, entries := range map[string][]approval.SignedChange{
		"tampered":  tampered,
		"reordered": {log[1], log[0]},
	} {
		b, _ := json.Marshal(entries)
		bad := filepath.Join(t.TempDir(), "keys.json")
		if err := os.WriteFile(bad, b, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := approval.OpenKeys(bad, rpID, origin); err == nil {
			t.Errorf("%s log loaded", name)
		}
	}
}

func TestKeyLogExtend(t *testing.T) {
	t.Parallel()
	a := approvaltest.New(t, rpID, origin)
	b := approvaltest.New(t, rpID, origin)
	c := approvaltest.New(t, rpID, origin)
	src, _ := trusting(t, a, b, c)
	full := src.Log()

	// An empty log trusts the first key it is given.
	empty, _ := openKeys(t)
	if err := empty.Extend(full[:1]); err != nil {
		t.Fatal(err)
	}
	if err := empty.Extend(full); err != nil {
		t.Fatal(err)
	}
	if n, h := empty.Head(); n != 3 || h != headOf(src) {
		t.Fatalf("head %d %s", n, h)
	}
	if err := empty.Extend(full[:2]); !errors.Is(err, approval.ErrDiverged) {
		t.Fatalf("shorter log: %v", err)
	}

	// A log that started elsewhere is refused and leaves the local one alone.
	other := approvaltest.New(t, rpID, origin)
	mine, path := trusting(t, other)
	if err := mine.Extend(full); !errors.Is(err, approval.ErrDiverged) {
		t.Fatalf("divergent log: %v", err)
	}
	reopened, err := approval.OpenKeys(path, rpID, origin)
	if err != nil {
		t.Fatal(err)
	}
	if k := reopened.Trusted(); len(k) != 1 || k[0].CredentialID != other.Key("").CredentialID {
		t.Fatalf("trusted after refused extend = %v", k)
	}

	// A matching prefix with a bad suffix applies nothing.
	prefix, _ := openKeys(t)
	if err := prefix.Extend(full[:1]); err != nil {
		t.Fatal(err)
	}
	forged := append([]approval.SignedChange{}, full...)
	forged[2] = other.Sign(full[2].Change)
	if err := prefix.Extend(forged); err == nil {
		t.Fatal("extended with a change signed by an untrusted key")
	}
	if n, _ := prefix.Head(); n != 1 {
		t.Fatalf("partially applied: length %d", n)
	}
}

func headOf(k *approval.Keys) string {
	_, h := k.Head()
	return h
}
