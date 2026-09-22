package instantly_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/campaign"
	"github.com/bory/karvon-be/internal/campaign/provider"
	"github.com/bory/karvon-be/internal/campaign/provider/instantly"
	"github.com/bory/karvon-be/internal/crypto"
	"github.com/bory/karvon-be/internal/db/dbgen"
)

func testCipher(t *testing.T) *crypto.Cipher {
	t.Helper()
	cipher, err := crypto.New(strings.Repeat("ab", crypto.KeySize))
	if err != nil {
		t.Fatalf("crypto.New: %v", err)
	}
	return cipher
}

// dbgenSource is an enabled Instantly outreach source without a key.
func dbgenSource() dbgen.Source {
	return dbgen.Source{
		ID:      uuid.New(),
		Kind:    campaign.KindInstantly,
		Role:    campaign.RoleOutreach,
		Name:    "Instantly",
		Enabled: true,
	}
}

func TestFactoryBuildsAndCachesAClientPerSourceAndKey(t *testing.T) {
	cipher := testCipher(t)
	factory := instantly.NewFactory(cipher, instantly.FactoryConfig{})
	ctx := context.Background()

	source := dbgenSource()
	var err error
	if source.ApiKeyEnc, err = cipher.EncryptString("key-one"); err != nil {
		t.Fatalf("EncryptString: %v", err)
	}

	first, err := factory.For(ctx, source)
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if _, ok := first.(*instantly.HTTPClient); !ok {
		t.Fatalf("For returned %T, want *HTTPClient", first)
	}
	second, err := factory.For(ctx, source)
	if err != nil {
		t.Fatalf("For (again): %v", err)
	}
	if first != second {
		t.Fatal("the same source and key must reuse the client so breaker state persists")
	}

	// A fresh encryption of the same key still hits the cache: the cache is keyed
	// by the decrypted key's digest, not by the ciphertext.
	if source.ApiKeyEnc, err = cipher.EncryptString("key-one"); err != nil {
		t.Fatalf("EncryptString: %v", err)
	}
	third, err := factory.For(ctx, source)
	if err != nil || third != first {
		t.Fatalf("re-encrypted key: %v, same client = %v", err, third == first)
	}

	// A rotated key yields a new client.
	if source.ApiKeyEnc, err = cipher.EncryptString("key-two"); err != nil {
		t.Fatalf("EncryptString: %v", err)
	}
	rotated, err := factory.For(ctx, source)
	if err != nil {
		t.Fatalf("For (rotated): %v", err)
	}
	if rotated == first {
		t.Fatal("a rotated key must not reuse the old client")
	}
}

func TestFactoryRejectsTheWrongSources(t *testing.T) {
	factory := instantly.NewFactory(testCipher(t), instantly.FactoryConfig{})
	ctx := context.Background()

	assertCode := func(t *testing.T, err error, code apperr.Code) {
		t.Helper()
		var appErr *apperr.Error
		if !errors.As(err, &appErr) {
			t.Fatalf("err = %v, want *apperr.Error", err)
		}
		if appErr.Code != code {
			t.Fatalf("code = %q, want %q (%v)", appErr.Code, code, err)
		}
	}

	wrongRole := dbgenSource()
	wrongRole.Role = campaign.RoleNewsletter
	_, err := factory.For(ctx, wrongRole)
	assertCode(t, err, apperr.CodeValidationFailed)

	wrongKind := dbgenSource()
	wrongKind.Kind = campaign.KindMailchimp
	_, err = factory.For(ctx, wrongKind)
	assertCode(t, err, apperr.CodeValidationFailed)

	disabled := dbgenSource()
	disabled.Enabled = false
	_, err = factory.For(ctx, disabled)
	assertCode(t, err, apperr.CodeConflict)

	_, err = factory.For(ctx, dbgenSource())
	assertCode(t, err, apperr.CodeProviderAuth)
	if !errors.Is(err, provider.ErrNotConfigured) {
		t.Fatalf("a missing key must wrap ErrNotConfigured, got %v", err)
	}

	undecryptable := dbgenSource()
	undecryptable.ApiKeyEnc = []byte("not a ciphertext")
	_, err = factory.For(ctx, undecryptable)
	assertCode(t, err, apperr.CodeProviderAuth)
}
