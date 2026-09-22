// Package crypto wraps AES-256-GCM so provider API keys are never stored in plaintext.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
)

// KeySize is the required length of the raw AES key.
const KeySize = 32

// ErrInvalidCiphertext is returned when a stored blob cannot be authenticated.
var ErrInvalidCiphertext = errors.New("crypto: ciphertext is invalid or was encrypted with another key")

// Cipher encrypts and decrypts small secrets with a process-wide key.
type Cipher struct {
	aead cipher.AEAD
}

// ParseKey accepts a 32-byte key encoded as hex or base64 (standard or raw URL).
func ParseKey(encoded string) ([]byte, error) {
	if encoded == "" {
		return nil, errors.New("crypto: key is empty")
	}
	decoders := []func(string) ([]byte, error){
		hex.DecodeString,
		base64.StdEncoding.DecodeString,
		base64.RawStdEncoding.DecodeString,
		base64.URLEncoding.DecodeString,
		base64.RawURLEncoding.DecodeString,
	}
	for _, decode := range decoders {
		if key, err := decode(encoded); err == nil && len(key) == KeySize {
			return key, nil
		}
	}
	return nil, fmt.Errorf("crypto: key must decode to %d bytes from hex or base64", KeySize)
}

// New builds a Cipher from an encoded key.
func New(encodedKey string) (*Cipher, error) {
	key, err := ParseKey(encodedKey)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: new cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: new gcm: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

// Encrypt returns nonce||ciphertext for the given plaintext.
func (c *Cipher) Encrypt(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("crypto: read nonce: %w", err)
	}
	return c.aead.Seal(nonce, nonce, plaintext, nil), nil
}

// EncryptString is Encrypt for string secrets.
func (c *Cipher) EncryptString(plaintext string) ([]byte, error) {
	return c.Encrypt([]byte(plaintext))
}

// Decrypt reverses Encrypt. It returns ErrInvalidCiphertext for tampered or foreign data.
func (c *Cipher) Decrypt(blob []byte) ([]byte, error) {
	nonceSize := c.aead.NonceSize()
	if len(blob) < nonceSize+1 {
		return nil, ErrInvalidCiphertext
	}
	plaintext, err := c.aead.Open(nil, blob[:nonceSize], blob[nonceSize:], nil)
	if err != nil {
		return nil, ErrInvalidCiphertext
	}
	return plaintext, nil
}

// DecryptString is Decrypt for string secrets.
func (c *Cipher) DecryptString(blob []byte) (string, error) {
	plaintext, err := c.Decrypt(blob)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

// GenerateKeyHex produces a fresh hex-encoded key, used by `make secret`.
func GenerateKeyHex() (string, error) {
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return "", err
	}
	return hex.EncodeToString(key), nil
}
