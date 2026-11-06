package cryptostore

import (
	"errors"
	"testing"
)

func TestEnvelopeScopeAndReservation(t *testing.T) {
	c, e := New(make([]byte, 32))
	if e != nil {
		t.Fatal(e)
	}
	aad := AAD("org", "id", "secret", 1)
	n := 0
	b, e := c.Seal([]byte("fictional"), aad, func([32]byte) error { n++; return nil })
	if e != nil || n != 1 {
		t.Fatal(e, n)
	}
	if _, e = c.Open(b, AAD("other", "id", "secret", 1)); e == nil {
		t.Fatal("cross-org authentication accepted")
	}
	if _, e = c.Open(b, aad); e != nil {
		t.Fatal(e)
	}
	if _, e = c.Seal(nil, aad, func([32]byte) error { return errors.New("quota") }); e == nil {
		t.Fatal("ignored quota")
	}
}

func TestEnvelopeRejectsTamperingAndMissingReservation(t *testing.T) {
	for _, size := range []int{0, 16, 24, 31, 33} {
		if _, err := New(make([]byte, size)); err == nil {
			t.Errorf("accepted %d-byte key", size)
		}
	}
	c, err := New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	aad := AAD("org", "record", "purpose", 1)
	if _, err := c.Seal(nil, aad, nil); err == nil {
		t.Fatal("missing reservation accepted")
	}
	envelope, err := c.Seal([]byte("fixture"), aad, func([32]byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, offset := range []int{0, 1, len(envelope) - 1} {
		changed := append([]byte(nil), envelope...)
		changed[offset] ^= 1
		if _, err := c.Open(changed, aad); err == nil {
			t.Errorf("tampering at %d accepted", offset)
		}
	}
	for _, length := range []int{0, 32, 33} {
		if _, err := c.Open(envelope[:length], aad); err == nil {
			t.Error("truncated envelope accepted")
		}
	}
	for _, wrong := range [][]byte{AAD("org", "other", "purpose", 1), AAD("org", "record", "other", 1), AAD("org", "record", "purpose", 2)} {
		if _, err := c.Open(envelope, wrong); err == nil {
			t.Error("scope substitution accepted")
		}
	}
	if string(AAD("ab", "c", "purpose", 1)) == string(AAD("a", "bc", "purpose", 1)) {
		t.Fatal("ambiguous scope encoding")
	}
}
