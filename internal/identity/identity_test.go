package identity

import (
	"context"
	"strings"
	"testing"
)

func TestCredentials(t *testing.T) {
	h := NewHasher()
	ctx := context.Background()
	password := "a sufficiently long 🐘 password"
	encoded, err := h.Hash(ctx, password)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.Verify(ctx, password, encoded); err != nil {
		t.Fatal(err)
	}
	for _, phc := range []string{"", strings.Replace(encoded, "m=65536", "m=4294967295", 1), encoded} {
		if err = h.Verify(ctx, "wrong password", phc); err == nil {
			t.Fatal("accepted invalid password/verifier")
		}
	}
	if ValidatePassword(strings.Repeat("界", 15)) != nil || ValidatePassword("short") == nil {
		t.Fatal("Unicode bounds")
	}
	if u, e := Username("Engineer.One"); e != nil || u != "engineer.one" {
		t.Fatal(u, e)
	}
	if _, e := Username(" admin "); e == nil {
		t.Fatal("unexpected normalization")
	}
	token := Token()
	if !ValidToken(token) || !ValidToken(CSRF(token)) || Equal(Digest(token), Digest(CSRF(token))) {
		t.Fatal("credential domains")
	}
}

func TestHashAdmissionBound(t *testing.T) {
	h := NewHasher()
	for range 10 {
		h.admitted <- struct{}{}
	}
	if _, e := h.Hash(context.Background(), "an intentionally long password"); e != ErrBusy {
		t.Fatal("hash admission overflow did not fail closed", e)
	}
}

func TestMalformedVerifiersAndPasswordBounds(t *testing.T) {
	h := NewHasher()
	prefix := "$argon2id$v=19$m=65536,t=3,p=1$"
	salt := "AAAAAAAAAAAAAAAAAAAAAA"
	for _, phc := range []string{"garbage", prefix + "!$!", prefix + salt + "$!", prefix + salt + "$AA", prefix + "AA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"} {
		if err := h.Verify(context.Background(), "a valid length password", phc); err != ErrInvalid {
			t.Errorf("malformed verifier: %v", err)
		}
	}
	for _, password := range []string{"short", strings.Repeat("a", 129), string([]byte{0xff})} {
		if _, err := h.Hash(context.Background(), password); err == nil {
			t.Error("invalid password accepted")
		}
	}
	for _, n := range []int{15, 128} {
		if err := ValidatePassword(strings.Repeat("a", n)); err != nil {
			t.Error(err)
		}
	}
	for _, token := range []string{"", "!" + strings.Repeat("a", 42), strings.Repeat("a", 42), strings.Repeat("a", 44)} {
		if ValidToken(token) {
			t.Error("malformed token accepted")
		}
	}
}

func TestCanceledHashWaitReleasesAdmission(t *testing.T) {
	h := &Hasher{active: make(chan struct{}, 2), admitted: make(chan struct{}, 10)}
	h.active <- struct{}{}
	h.active <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.Hash(ctx, "a valid length password"); err != context.Canceled {
		t.Fatal(err)
	}
	if len(h.admitted) != 0 {
		t.Fatal("canceled wait leaked admission")
	}
}
