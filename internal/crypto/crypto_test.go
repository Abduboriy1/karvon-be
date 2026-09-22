package crypto

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func testKeyHex() string { return strings.Repeat("ab", KeySize) }

func TestParseKeyAcceptsHexAndBase64(t *testing.T) {
	raw := make([]byte, KeySize)
	for i := range raw {
		raw[i] = byte(i)
	}

	encodings := map[string]string{
		"hex":            hex.EncodeToString(raw),
		"base64":         base64.StdEncoding.EncodeToString(raw),
		"base64 raw url": base64.RawURLEncoding.EncodeToString(raw),
	}

	for name, encoded := range encodings {
		t.Run(name, func(t *testing.T) {
			key, err := ParseKey(encoded)
			if err != nil {
				t.Fatalf("ParseKey: %v", err)
			}
			if string(key) != string(raw) {
				t.Fatal("decoded key does not match the original bytes")
			}
		})
	}
}

func TestParseKeyRejectsWrongLength(t *testing.T) {
	for _, encoded := range []string{"", "abcd", hex.EncodeToString(make([]byte, 16))} {
		if _, err := ParseKey(encoded); err == nil {
			t.Errorf("ParseKey(%q) should have failed", encoded)
		}
	}
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	cipher, err := New(testKeyHex())
	if err != nil {
		t.Fatal(err)
	}

	const secret = "apify_api_1234567890"
	blob, err := cipher.EncryptString(secret)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), secret) {
		t.Fatal("ciphertext contains the plaintext")
	}

	got, err := cipher.DecryptString(blob)
	if err != nil {
		t.Fatal(err)
	}
	if got != secret {
		t.Fatalf("decrypted %q, want %q", got, secret)
	}
}

func TestEncryptUsesAFreshNonceEachTime(t *testing.T) {
	cipher, err := New(testKeyHex())
	if err != nil {
		t.Fatal(err)
	}

	first, err := cipher.EncryptString("same-value")
	if err != nil {
		t.Fatal(err)
	}
	second, err := cipher.EncryptString("same-value")
	if err != nil {
		t.Fatal(err)
	}
	if string(first) == string(second) {
		t.Fatal("encrypting the same value twice produced identical ciphertext")
	}
}

func TestDecryptRejectsForeignKeyAndTampering(t *testing.T) {
	cipher, err := New(testKeyHex())
	if err != nil {
		t.Fatal(err)
	}
	blob, err := cipher.EncryptString("secret")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("another key", func(t *testing.T) {
		other, err := New(strings.Repeat("cd", KeySize))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := other.Decrypt(blob); !errors.Is(err, ErrInvalidCiphertext) {
			t.Fatalf("err = %v, want ErrInvalidCiphertext", err)
		}
	})

	t.Run("tampered ciphertext", func(t *testing.T) {
		tampered := append([]byte(nil), blob...)
		tampered[len(tampered)-1] ^= 0xFF
		if _, err := cipher.Decrypt(tampered); !errors.Is(err, ErrInvalidCiphertext) {
			t.Fatalf("err = %v, want ErrInvalidCiphertext", err)
		}
	})

	t.Run("too short", func(t *testing.T) {
		if _, err := cipher.Decrypt([]byte{1, 2, 3}); !errors.Is(err, ErrInvalidCiphertext) {
			t.Fatalf("err = %v, want ErrInvalidCiphertext", err)
		}
	})
}

func TestGenerateKeyHexProducesAUsableKey(t *testing.T) {
	encoded, err := GenerateKeyHex()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(encoded); err != nil {
		t.Fatalf("generated key is not usable: %v", err)
	}
}
