package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

var ErrInvalid = errors.New("invalid credentials")
var ErrBusy = errors.New("authentication busy; try again shortly")
var usernamePattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{2,63}$`)

func Username(s string) (string, error) {
	s = strings.ToLower(s)
	if !usernamePattern.MatchString(s) {
		return "", errors.New("username must be 3–64 ASCII letters, numbers, dots, underscores or hyphens, starting with a letter")
	}
	return s, nil
}

func ValidatePassword(password string) error {
	n := utf8.RuneCountInString(password)
	if !utf8.ValidString(password) || n < 15 || n > 128 || len(password) > 1024 {
		return errors.New("password must contain 15–128 characters and at most 1024 UTF-8 bytes")
	}
	return nil
}

func Token() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}
func Digest(s string) []byte { d := sha256.Sum256([]byte(s)); return d[:] }
func ValidToken(s string) bool {
	b, e := base64.RawURLEncoding.DecodeString(s)
	return e == nil && len(b) == 32 && len(s) == 43
}

// CSRF is reproducible only by the bearer of the random session cookie. The DB
// stores its verifier, so bootstrap can support multiple tabs without rotation.
func CSRF(session string) string {
	return base64.RawURLEncoding.EncodeToString(Digest("cosmoformer/csrf/v1\x00" + session))
}
func Equal(a, b []byte) bool { return subtle.ConstantTimeCompare(a, b) == 1 }

type Hasher struct {
	active, admitted chan struct{}
	dummy            string
}

func NewHasher() *Hasher {
	h := &Hasher{active: make(chan struct{}, 2), admitted: make(chan struct{}, 10)}
	h.dummy = derive(Token())
	return h
}
func (h *Hasher) run(ctx context.Context, f func() (string, error)) (string, error) {
	select {
	case h.admitted <- struct{}{}:
	default:
		return "", ErrBusy
	}
	defer func() { <-h.admitted }()
	select {
	case h.active <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-h.active }()
	return f()
}
func derive(password string) string {
	var salt [16]byte
	if _, err := rand.Read(salt[:]); err != nil {
		panic(err)
	}
	key := argon2.IDKey([]byte(password), salt[:], 3, 64*1024, 1, 32)
	return fmt.Sprintf("$argon2id$v=19$m=65536,t=3,p=1$%s$%s", base64.RawStdEncoding.EncodeToString(salt[:]), base64.RawStdEncoding.EncodeToString(key))
}
func (h *Hasher) Hash(ctx context.Context, password string) (string, error) {
	if err := ValidatePassword(password); err != nil {
		return "", err
	}
	return h.run(ctx, func() (string, error) { return derive(password), nil })
}
func (h *Hasher) Verify(ctx context.Context, password, phc string) error {
	_, err := h.run(ctx, func() (string, error) {
		known := phc != ""
		if !known {
			phc = h.dummy
		}
		parts := strings.Split(phc, "$")
		// Accept exactly the configured work factor. Never allocate from stored input.
		if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" || parts[3] != "m=65536,t=3,p=1" {
			return "", ErrInvalid
		}
		salt, e := base64.RawStdEncoding.DecodeString(parts[4])
		if e != nil || len(salt) != 16 {
			return "", ErrInvalid
		}
		want, e := base64.RawStdEncoding.DecodeString(parts[5])
		if e != nil || len(want) != 32 {
			return "", ErrInvalid
		}
		got := argon2.IDKey([]byte(password), salt, 3, 64*1024, 1, 32)
		if !Equal(got, want) || !known {
			return "", ErrInvalid
		}
		return "", nil
	})
	return err
}
