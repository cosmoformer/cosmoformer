package cryptostore

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/binary"
	"errors"
)

// Envelope is reserved for managed-secret storage; local account verifiers are
// one-way hashes and never use reversible encryption.
func AAD(org, record, purpose string, revision uint64) []byte {
	b := []byte("cosmoformer/envelope/v1")
	for _, s := range []string{org, record, purpose} {
		b = binary.BigEndian.AppendUint32(b, uint32(len(s)))
		b = append(b, s...)
	}
	return binary.BigEndian.AppendUint64(b, revision)
}

type Cipher struct {
	aead        cipher.AEAD
	fingerprint [32]byte
}

func New(key []byte) (*Cipher, error) {
	if len(key) != 32 {
		return nil, errors.New("encryption key must be 32 bytes")
	}
	b, e := aes.NewCipher(key)
	if e != nil {
		return nil, e
	}
	a, e := cipher.NewGCMWithRandomNonce(b)
	if e != nil {
		return nil, e
	}
	return &Cipher{a, sha256.Sum256(key)}, nil
}

// Reserve must durably reserve usage for this actual key before encryption;
// failed writes still consume their reservation. No in-memory quota fallback.
func (c *Cipher) Seal(plaintext, aad []byte, reserve func([32]byte) error) ([]byte, error) {
	if reserve == nil {
		return nil, errors.New("durable key quota reservation required")
	}
	if e := reserve(c.fingerprint); e != nil {
		return nil, e
	}
	out := append([]byte{1}, c.fingerprint[:]...)
	return c.aead.Seal(out, nil, plaintext, aad), nil
}
func (c *Cipher) Open(envelope, aad []byte) ([]byte, error) {
	if len(envelope) < 33 || envelope[0] != 1 || string(envelope[1:33]) != string(c.fingerprint[:]) {
		return nil, errors.New("invalid envelope")
	}
	out, e := c.aead.Open(nil, nil, envelope[33:], aad)
	if e != nil {
		return nil, errors.New("envelope authentication failed")
	}
	return out, nil
}
